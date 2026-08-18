package dasandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
)

type fakeSandbox struct {
	dabackend.Backend
	id        string
	mu        sync.Mutex
	uploads   []dabackend.Upload
	deletes   []string
	commands  []string
	exitCode  int
	uploadErr string
}

func (sandbox *fakeSandbox) ID() string { return sandbox.id }

func (sandbox *fakeSandbox) Execute(_ context.Context, command string, _ time.Duration) (dabackend.ExecuteResult, error) {
	sandbox.mu.Lock()
	sandbox.commands = append(sandbox.commands, command)
	sandbox.mu.Unlock()
	return dabackend.ExecuteResult{ExitCode: &sandbox.exitCode}, nil
}

func (sandbox *fakeSandbox) Upload(_ context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		upload.Content = append([]byte(nil), upload.Content...)
		sandbox.uploads = append(sandbox.uploads, upload)
		results[index] = dabackend.UploadResult{Path: upload.Path, Error: sandbox.uploadErr}
	}
	return results
}

func (sandbox *fakeSandbox) Delete(_ context.Context, path string) (dabackend.DeleteResult, error) {
	sandbox.mu.Lock()
	sandbox.deletes = append(sandbox.deletes, path)
	sandbox.mu.Unlock()
	return dabackend.DeleteResult{Path: path}, nil
}

type fakeProvider struct {
	sandbox     dabackend.Sandbox
	requests    []ProviderRequest
	deletes     []string
	deleteError error
	panicOpen   bool
	mu          sync.Mutex
}

func (provider *fakeProvider) GetOrCreate(_ context.Context, request ProviderRequest) (dabackend.Sandbox, error) {
	if provider.panicOpen {
		panic("credential-value-must-not-escape")
	}
	provider.mu.Lock()
	request.Params = cloneParams(request.Params)
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	return provider.sandbox, nil
}

func (provider *fakeProvider) Delete(ctx context.Context, sandboxID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.deletes = append(provider.deletes, sandboxID)
	return provider.deleteError
}

func definitionFor(provider Provider, metadata *Metadata, params map[string]string) Definition {
	return Definition{Metadata: metadata, Params: params, Factory: func(context.Context, ProviderConfig) (Provider, error) {
		return provider, nil
	}}
}

func TestBuiltinMetadataAndUnavailableFactories(t *testing.T) {
	registry := NewRegistry(nil, RegistryOptions{})
	want := []string{"agentcore", "daytona", "langsmith", "modal", "runloop", "vercel"}
	if got := registry.Available(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Available() = %#v, want %#v", got, want)
	}
	for name, metadata := range BuiltinMetadata() {
		got, exists := registry.Metadata(name)
		if !exists || got != metadata || !got.BuiltIn {
			t.Fatalf("Metadata(%q) = %#v, %v; want %#v", name, got, exists, metadata)
		}
	}
	if _, err := registry.Open(context.Background(), "runloop", OpenRequest{}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("Open() error = %v, want ErrProviderUnavailable", err)
	}
}

func TestRegistryPrecedenceAndParameterCopies(t *testing.T) {
	sandbox := &fakeSandbox{id: "fresh"}
	provider := &fakeProvider{sandbox: sandbox}
	configuredMetadata := Metadata{WorkingDir: "/configured", SupportsSandboxID: true, SupportsSnapshotName: true}
	registry := NewRegistry(map[string]Definition{
		"runloop": definitionFor(provider, nil, map[string]string{"layer": "builtin", "unchanged": "yes"}),
	}, RegistryOptions{
		Extensions: map[string]Definition{
			"runloop": definitionFor(provider, nil, map[string]string{"layer": "extension"}),
		},
		Configuration: map[string]Definition{
			"runloop": {Metadata: &configuredMetadata, Params: map[string]string{"layer": "configuration"}},
		},
	})
	if source, _ := registry.Source("runloop"); source != "configuration" {
		t.Fatalf("Source() = %q, want configuration", source)
	}
	metadata, _ := registry.Metadata("runloop")
	if metadata.WorkingDir != "/configured" || !metadata.BuiltIn {
		t.Fatalf("Metadata() = %#v", metadata)
	}
	requestParams := map[string]string{"layer": "request"}
	session, err := registry.Open(context.Background(), "runloop", OpenRequest{Params: requestParams})
	if err != nil {
		t.Fatal(err)
	}
	requestParams["layer"] = "mutated"
	if got := provider.requests[0].Params["layer"]; got != "request" {
		t.Fatalf("provider layer = %q, want request", got)
	}
	if _, exists := provider.requests[0].Params["unchanged"]; exists {
		t.Fatal("lower-precedence parameters unexpectedly leaked through an overriding definition")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRequiresExplicitSelectionAndValidCapabilities(t *testing.T) {
	provider := &fakeProvider{sandbox: &fakeSandbox{id: "box"}}
	registry := NewRegistry(map[string]Definition{
		"agentcore": definitionFor(provider, nil, nil),
		"runloop":   definitionFor(provider, nil, nil),
	}, RegistryOptions{})
	for _, test := range []struct {
		name     string
		provider string
		request  OpenRequest
		want     error
	}{
		{name: "no default", want: ErrInvalidRequest},
		{name: "unknown", provider: "missing", want: ErrUnknownProvider},
		{name: "id and snapshot", provider: "runloop", request: OpenRequest{SandboxID: "id", Snapshot: "snap"}, want: ErrInvalidRequest},
		{name: "unsupported id", provider: "agentcore", request: OpenRequest{SandboxID: "id"}, want: ErrUnsupportedAttach},
		{name: "unsupported snapshot", provider: "agentcore", request: OpenRequest{Snapshot: "snap"}, want: ErrUnsupportedSnapshot},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := registry.Open(context.Background(), test.provider, test.request); !errors.Is(err, test.want) {
				t.Fatalf("Open() error = %v, want %v", err, test.want)
			}
		})
	}
	defaulted := NewRegistry(map[string]Definition{"runloop": definitionFor(provider, nil, nil)}, RegistryOptions{Default: "runloop"})
	if _, err := defaulted.Open(context.Background(), "", OpenRequest{}); err != nil {
		t.Fatalf("open configured default: %v", err)
	}
}

func TestSetupIsLiteralBoundedAndFreshFailureCleansUp(t *testing.T) {
	t.Setenv("DASANDBOX_SETUP_SECRET", "must-not-expand")
	sandbox := &fakeSandbox{id: "fresh"}
	provider := &fakeProvider{sandbox: sandbox}
	registry := NewRegistry(map[string]Definition{"runloop": definitionFor(provider, nil, nil)}, RegistryOptions{MaxSetupBytes: 32})
	script := []byte("printf '$DASANDBOX_SETUP_SECRET'")
	session, err := registry.Open(context.Background(), "runloop", OpenRequest{SetupScript: script})
	if err != nil {
		t.Fatal(err)
	}
	if len(sandbox.uploads) != 1 || string(sandbox.uploads[0].Content) != string(script) {
		t.Fatalf("uploads = %#v", sandbox.uploads)
	}
	if len(sandbox.commands) != 1 || !strings.HasPrefix(sandbox.commands[0], "bash /tmp/dago-setup-") || strings.Contains(sandbox.commands[0], "must-not-expand") {
		t.Fatalf("commands = %#v", sandbox.commands)
	}
	if len(sandbox.deletes) != 1 || sandbox.deletes[0] != sandbox.uploads[0].Path {
		t.Fatalf("temporary setup deletes = %#v", sandbox.deletes)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open(context.Background(), "runloop", OpenRequest{SetupScript: make([]byte, 33)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized setup error = %v", err)
	}

	failingSandbox := &fakeSandbox{id: "failed", exitCode: 7}
	failingProvider := &fakeProvider{sandbox: failingSandbox}
	failingRegistry := NewRegistry(map[string]Definition{"runloop": definitionFor(failingProvider, nil, nil)}, RegistryOptions{})
	if _, err := failingRegistry.Open(context.Background(), "runloop", OpenRequest{SetupScript: []byte("exit 7")}); !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("setup failure = %v, want ErrSetupFailed", err)
	}
	if !reflect.DeepEqual(failingProvider.deletes, []string{"failed"}) {
		t.Fatalf("fresh cleanup = %#v", failingProvider.deletes)
	}
}

func TestAttachedSessionsAreRetained(t *testing.T) {
	sandbox := &fakeSandbox{id: "attached", exitCode: 4}
	provider := &fakeProvider{sandbox: sandbox}
	registry := NewRegistry(map[string]Definition{"runloop": definitionFor(provider, nil, nil)}, RegistryOptions{})
	if _, err := registry.Open(context.Background(), "runloop", OpenRequest{SandboxID: "attached", SetupScript: []byte("false")}); !errors.Is(err, ErrSetupFailed) {
		t.Fatalf("Open() = %v, want setup failure", err)
	}
	if len(provider.deletes) != 0 {
		t.Fatalf("attached resource was deleted: %#v", provider.deletes)
	}
	sandbox.exitCode = 0
	session, err := registry.Open(context.Background(), "runloop", OpenRequest{SandboxID: "attached"})
	if err != nil {
		t.Fatal(err)
	}
	if session.Owned() {
		t.Fatal("attached session reported ownership")
	}
	if err := session.Close(context.Background()); err != nil || len(provider.deletes) != 0 {
		t.Fatalf("Close() = %v, deletes %#v", err, provider.deletes)
	}
}

func TestCloseRetriesAndProviderFailuresDoNotLeakDetails(t *testing.T) {
	provider := &fakeProvider{sandbox: &fakeSandbox{id: "fresh"}, deleteError: errors.New("private token text")}
	registry := NewRegistry(map[string]Definition{"runloop": definitionFor(provider, nil, nil)}, RegistryOptions{})
	session, err := registry.Open(context.Background(), "runloop", OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Close() = %v", err)
	}
	if len(provider.deletes) != 0 {
		t.Fatal("canceled close attempted remote deletion")
	}
	if err := session.Close(context.Background()); !errors.Is(err, ErrProviderFailed) || strings.Contains(err.Error(), "private token") {
		t.Fatalf("failed Close() = %v", err)
	}
	provider.deleteError = nil
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() = %v", err)
	}
	if err := session.Close(context.Background()); err != nil || len(provider.deletes) != 2 {
		t.Fatalf("idempotent Close() = %v, deletes %#v", err, provider.deletes)
	}

	panicking := &fakeProvider{sandbox: &fakeSandbox{id: "unused"}, panicOpen: true}
	panicRegistry := NewRegistry(map[string]Definition{"runloop": definitionFor(panicking, nil, nil)}, RegistryOptions{})
	if _, err := panicRegistry.Open(context.Background(), "runloop", OpenRequest{}); !errors.Is(err, ErrProviderFailed) || strings.Contains(err.Error(), "credential-value") {
		t.Fatalf("panic error = %v", err)
	}
}

func TestRuntimeParametersFailAsRequestErrors(t *testing.T) {
	provider := &fakeProvider{sandbox: &fakeSandbox{id: "box"}}
	registry := NewRegistry(map[string]Definition{"runloop": definitionFor(provider, nil, nil)}, RegistryOptions{})
	if _, err := registry.Open(context.Background(), "runloop", OpenRequest{Params: map[string]string{"": "invalid"}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Open() = %v, want ErrInvalidRequest", err)
	}
}
