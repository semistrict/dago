package runloop

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
)

type executeCall struct {
	sandboxID string
	command   string
	timeout   time.Duration
	maxOutput int
}

type uploadCall struct {
	sandboxID string
	path      string
	content   []byte
	maxBytes  int64
}

type downloadCall struct {
	sandboxID string
	path      string
	maxBytes  int64
}

type fakeClient struct {
	executeCalls   []executeCall
	uploadCalls    []uploadCall
	downloadCalls  []downloadCall
	attachCalls    []string
	createCalls    int
	blueprintIDs   []string
	blueprintNames []string
	listCalls      []string
	buildNames     []string
	buildFiles     []string
	shutdownCalls  []string

	executeResult  CommandResult
	executeErr     error
	uploadErr      error
	downloadData   []byte
	downloadErr    error
	attachErr      error
	createdID      string
	blueprintID    string
	blueprintName  string
	pages          []BlueprintPage
	listErr        error
	buildErr       error
	shutdownErr    error
	cancelExecute  func()
	cancelUpload   func()
	cancelDownload func()
	cancelList     func()
}

func (client *fakeClient) Execute(_ context.Context, sandboxID, command string, timeout time.Duration, maxOutput int) (CommandResult, error) {
	client.executeCalls = append(client.executeCalls, executeCall{sandboxID: sandboxID, command: command, timeout: timeout, maxOutput: maxOutput})
	if client.cancelExecute != nil {
		client.cancelExecute()
	}
	return client.executeResult, client.executeErr
}

func (client *fakeClient) Upload(_ context.Context, sandboxID, path string, content []byte, maxBytes int64) error {
	client.uploadCalls = append(client.uploadCalls, uploadCall{sandboxID: sandboxID, path: path, content: append([]byte(nil), content...), maxBytes: maxBytes})
	if client.cancelUpload != nil {
		client.cancelUpload()
	}
	return client.uploadErr
}

func (client *fakeClient) Download(_ context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	client.downloadCalls = append(client.downloadCalls, downloadCall{sandboxID: sandboxID, path: path, maxBytes: maxBytes})
	if client.cancelDownload != nil {
		client.cancelDownload()
	}
	return append([]byte(nil), client.downloadData...), client.downloadErr
}

func (client *fakeClient) Attach(_ context.Context, sandboxID string) error {
	client.attachCalls = append(client.attachCalls, sandboxID)
	return client.attachErr
}

func (client *fakeClient) Create(context.Context) (string, error) {
	client.createCalls++
	return client.createdID, nil
}

func (client *fakeClient) CreateFromBlueprintID(_ context.Context, id string) (string, error) {
	client.blueprintIDs = append(client.blueprintIDs, id)
	return client.blueprintID, nil
}

func (client *fakeClient) CreateFromBlueprintName(_ context.Context, name string) (string, error) {
	client.blueprintNames = append(client.blueprintNames, name)
	return client.blueprintName, nil
}

func (client *fakeClient) ListBlueprints(_ context.Context, name, cursor string, _ int) (BlueprintPage, error) {
	client.listCalls = append(client.listCalls, name+"|"+cursor)
	if client.cancelList != nil {
		client.cancelList()
	}
	if client.listErr != nil {
		return BlueprintPage{}, client.listErr
	}
	if len(client.pages) == 0 {
		return BlueprintPage{}, nil
	}
	page := client.pages[0]
	client.pages = client.pages[1:]
	return page, nil
}

func (client *fakeClient) BuildBlueprint(_ context.Context, name, dockerfile string) error {
	client.buildNames = append(client.buildNames, name)
	client.buildFiles = append(client.buildFiles, dockerfile)
	return client.buildErr
}

func (client *fakeClient) Shutdown(_ context.Context, sandboxID string) error {
	client.shutdownCalls = append(client.shutdownCalls, sandboxID)
	return client.shutdownErr
}

func TestBackendUsesStableIDUsefulDefaultsAndBoundedOutput(t *testing.T) {
	client := &fakeClient{executeResult: CommandResult{
		Stdout:   "1234é",
		Stderr:   "failure",
		ExitCode: 7,
	}}
	backend := New(client, " dev-1 ", Options{MaxOutput: 5})
	if backend.ID() != "dev-1" {
		t.Fatalf("ID = %q", backend.ID())
	}
	result, err := backend.Execute(t.Context(), "false", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "1234" || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("Execute = %#v", result)
	}
	want := []executeCall{{sandboxID: "dev-1", command: "false", timeout: 30 * time.Minute, maxOutput: 5}}
	if !reflect.DeepEqual(client.executeCalls, want) {
		t.Fatalf("execute calls = %#v, want %#v", client.executeCalls, want)
	}
}

func TestBackendPreservesExplicitZeroTimeout(t *testing.T) {
	client := &fakeClient{}
	backend := New(client, "dev-1", Options{})
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{}); err != nil {
		t.Fatal(err)
	}
	zero := time.Duration(0)
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &zero}); err != nil {
		t.Fatal(err)
	}
	if client.executeCalls[0].timeout != defaultTimeout || client.executeCalls[1].timeout != 0 {
		t.Fatalf("timeouts = %#v", client.executeCalls)
	}
	negative := -time.Second
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &negative}); err == nil {
		t.Fatal("negative timeout succeeded")
	}
}

func TestBackendTransferBoundsCancellationAndErrorDetails(t *testing.T) {
	client := &fakeClient{downloadData: []byte("12345")}
	backend := New(client, "dev-1", Options{MaxFileSize: 4, MaxTransferFiles: 2})
	uploads := backend.Upload(t.Context(), []dabackend.Upload{
		{Path: "/ok", Content: []byte("1234")},
		{Path: "/large", Content: []byte("12345")},
		{Path: "/batch", Content: []byte("x")},
	})
	if uploads[0].Error != "" || !strings.Contains(uploads[1].Error, "payload too large") || !strings.Contains(uploads[2].Error, "batch limit") {
		t.Fatalf("Upload = %#v", uploads)
	}
	if len(client.uploadCalls) != 1 || client.uploadCalls[0].maxBytes != 4 {
		t.Fatalf("upload calls = %#v", client.uploadCalls)
	}
	downloads := backend.Download(t.Context(), []string{"/large", "/large-again", "/batch"})
	if !strings.Contains(downloads[0].Error, "payload too large") || len(downloads[0].Content) != 0 || !strings.Contains(downloads[2].Error, "batch limit") {
		t.Fatalf("Download = %#v", downloads)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client.executeErr = errors.New("transport ignored cancellation")
	if _, err := backend.Execute(ctx, "true", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute cancellation = %v", err)
	}
	results := backend.Upload(ctx, []dabackend.Upload{{Path: "/never", Content: []byte("x")}})
	if !strings.Contains(results[0].Error, context.Canceled.Error()) || len(client.uploadCalls) != 1 {
		t.Fatalf("canceled Upload = %#v, calls = %#v", results, client.uploadCalls)
	}
}

func TestBackendPassesOpaqueFilePathsOnlyToNativeTransport(t *testing.T) {
	path := "/workspace/a'; printf attacked\n"
	client := &fakeClient{downloadData: []byte("safe")}
	backend := New(client, "dev-1", Options{})
	if result := backend.Upload(t.Context(), []dabackend.Upload{{Path: path, Content: []byte("content")}}); result[0].Error != "" {
		t.Fatalf("Upload = %#v", result)
	}
	if result := backend.Download(t.Context(), []string{path}); result[0].Error != "" || string(result[0].Content) != "safe" {
		t.Fatalf("Download = %#v", result)
	}
	if client.uploadCalls[0].path != path || client.downloadCalls[0].path != path {
		t.Fatalf("paths changed: upload %q, download %q", client.uploadCalls[0].path, client.downloadCalls[0].path)
	}
}

func TestBackendCancellationIgnoringTransportCannotReturnSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client := &fakeClient{cancelExecute: cancel}
	backend := New(client, "dev-1", Options{})
	if _, err := backend.Execute(ctx, "true", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v", err)
	}

	ctx, cancel = context.WithCancel(t.Context())
	client.cancelExecute = nil
	client.cancelUpload = cancel
	result := backend.Upload(ctx, []dabackend.Upload{{Path: "/one", Content: []byte("x")}, {Path: "/two", Content: []byte("y")}})
	if !strings.Contains(result[0].Error, context.Canceled.Error()) || !strings.Contains(result[1].Error, context.Canceled.Error()) || len(client.uploadCalls) != 1 {
		t.Fatalf("Upload = %#v, calls = %#v", result, client.uploadCalls)
	}
}

func TestBackendBoundsErrorsWithoutLosingClassification(t *testing.T) {
	sentinel := errors.New("transport unavailable")
	client := &fakeClient{executeErr: errors.Join(sentinel, errors.New(strings.Repeat("x", 1000)))}
	backend := New(client, "dev-1", Options{})
	_, err := backend.Execute(t.Context(), "true", 0)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Execute error classification = %v", err)
	}
	if len(err.Error()) > len("runloop backend: execute: ")+303 {
		t.Fatalf("Execute error was not bounded: %d bytes", len(err.Error()))
	}
}

func TestBackendDerivesFileOperationsThroughBaseSandbox(t *testing.T) {
	client := &fakeClient{executeResult: CommandResult{Stdout: `{"entries":[]}`, ExitCode: 0}}
	backend := New(client, "dev-1", Options{})
	listing, err := backend.List(t.Context(), "/workspace")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("List = %#v, %v", listing, err)
	}
	if len(client.executeCalls) != 1 || !strings.HasPrefix(client.executeCalls[0].command, "python3 - ") || !strings.Contains(client.executeCalls[0].command, "__DAGO_SANDBOX_PY__") {
		t.Fatalf("base sandbox command = %#v", client.executeCalls)
	}
}

func TestNewRejectsInvalidStaticConfiguration(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "nil transport", call: func() { New(nil, "dev", Options{}) }},
		{name: "typed nil transport", call: func() {
			var client *fakeClient
			New(client, "dev", Options{})
		}},
		{name: "empty id", call: func() { New(&fakeClient{}, " ", Options{}) }},
		{name: "negative bound", call: func() { New(&fakeClient{}, "dev", Options{MaxOutput: -1}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("constructor did not panic")
				}
			}()
			test.call()
		})
	}
}

func TestProviderCreatesAttachesAndDeletesExplicitly(t *testing.T) {
	client := &fakeClient{createdID: "new-dev"}
	provider := NewProvider(client, ProviderOptions{})
	created, err := provider.GetOrCreate(t.Context(), SandboxOptions{})
	if err != nil || created.ID() != "new-dev" || client.createCalls != 1 {
		t.Fatalf("create = %#v, %v; calls = %d", created, err, client.createCalls)
	}
	attached, err := provider.GetOrCreate(t.Context(), SandboxOptions{SandboxID: "existing"})
	if err != nil || attached.ID() != "existing" || !reflect.DeepEqual(client.attachCalls, []string{"existing"}) {
		t.Fatalf("attach = %#v, %v; calls = %#v", attached, err, client.attachCalls)
	}
	if err := provider.Delete(t.Context(), attached.ID()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.shutdownCalls, []string{"existing"}) {
		t.Fatalf("shutdown calls = %#v", client.shutdownCalls)
	}
}

func TestProviderBlueprintResolutionPrecedenceAndPagination(t *testing.T) {
	env := map[string]string{EnvBlueprintID: "bp-id", EnvBlueprintName: "env-name"}
	client := &fakeClient{blueprintID: "from-id"}
	provider := NewProvider(client, ProviderOptions{ResolveEnv: func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}})
	backend, err := provider.GetOrCreate(t.Context(), SandboxOptions{Snapshot: "explicit-name"})
	if err != nil || backend.ID() != "from-id" || !reflect.DeepEqual(client.blueprintIDs, []string{"bp-id"}) || len(client.listCalls) != 0 {
		t.Fatalf("ID precedence = %#v, %v; client = %#v", backend, err, client)
	}

	delete(env, EnvBlueprintID)
	client.blueprintName = "from-name"
	client.pages = []BlueprintPage{
		{Blueprints: []Blueprint{{ID: "one", Name: "other", Status: "build_complete"}}, HasMore: true},
		{Blueprints: []Blueprint{{ID: "two", Name: "explicit-name", Status: "build_complete"}}},
	}
	backend, err = provider.GetOrCreate(t.Context(), SandboxOptions{Snapshot: "explicit-name"})
	if err != nil || backend.ID() != "from-name" {
		t.Fatalf("name create = %#v, %v", backend, err)
	}
	if !reflect.DeepEqual(client.listCalls, []string{"explicit-name|", "explicit-name|one"}) || !reflect.DeepEqual(client.blueprintNames, []string{"explicit-name"}) || len(client.buildNames) != 0 {
		t.Fatalf("name lifecycle = lists %#v names %#v builds %#v", client.listCalls, client.blueprintNames, client.buildNames)
	}
}

func TestProviderBuildsMissingBlueprintWithUsefulDefault(t *testing.T) {
	client := &fakeClient{blueprintName: "dev-blueprint", pages: []BlueprintPage{{}}}
	provider := NewProvider(client, ProviderOptions{ResolveEnv: func(string) (string, bool) { return "", false }})
	backend, err := provider.GetOrCreate(t.Context(), SandboxOptions{Snapshot: "snapshot"})
	if err != nil || backend.ID() != "dev-blueprint" {
		t.Fatalf("GetOrCreate = %#v, %v", backend, err)
	}
	if !reflect.DeepEqual(client.buildNames, []string{"snapshot"}) || !reflect.DeepEqual(client.buildFiles, []string{defaultBlueprintDockerfile}) {
		t.Fatalf("build = names %#v files %#v", client.buildNames, client.buildFiles)
	}
}

func TestProviderPrefixedEnvironmentWinsAndEmptySuppressesPlainValue(t *testing.T) {
	values := map[string]string{
		upstreamEnvPrefix + EnvBlueprintID: "prefixed",
		EnvBlueprintID:                     "plain",
	}
	client := &fakeClient{blueprintID: "dev"}
	provider := NewProvider(client, ProviderOptions{ResolveEnv: func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}})
	if _, err := provider.GetOrCreate(t.Context(), SandboxOptions{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.blueprintIDs, []string{"prefixed"}) {
		t.Fatalf("blueprint IDs = %#v", client.blueprintIDs)
	}
	values[upstreamEnvPrefix+EnvBlueprintID] = ""
	client.createdID = "fresh"
	client.blueprintIDs = nil
	if backend, err := provider.GetOrCreate(t.Context(), SandboxOptions{}); err != nil || backend.ID() != "fresh" {
		t.Fatalf("empty prefix = %#v, %v", backend, err)
	}
	if len(client.blueprintIDs) != 0 {
		t.Fatalf("empty prefix fell through to plain value: %#v", client.blueprintIDs)
	}
}

func TestProviderRejectsNonReadyAndAdversarialPagination(t *testing.T) {
	client := &fakeClient{pages: []BlueprintPage{{Blueprints: []Blueprint{{ID: "bp", Name: "snapshot", Status: "building"}}}}}
	provider := NewProvider(client, ProviderOptions{ResolveEnv: func(string) (string, bool) { return "", false }})
	_, err := provider.GetOrCreate(t.Context(), SandboxOptions{Snapshot: "snapshot"})
	if !errors.Is(err, ErrBlueprintNotReady) || len(client.buildNames) != 0 {
		t.Fatalf("non-ready error = %v, builds = %#v", err, client.buildNames)
	}

	client.pages = []BlueprintPage{{HasMore: true}}
	_, err = provider.GetOrCreate(t.Context(), SandboxOptions{Snapshot: "snapshot"})
	if err == nil || !strings.Contains(err.Error(), "has_more without entries") {
		t.Fatalf("invalid pagination error = %v", err)
	}
	client.pages = []BlueprintPage{
		{Blueprints: []Blueprint{{ID: "same", Name: "other", Status: "build_complete"}}, HasMore: true},
		{Blueprints: []Blueprint{{ID: "same", Name: "other", Status: "build_complete"}}, HasMore: true},
	}
	_, err = provider.GetOrCreate(t.Context(), SandboxOptions{Snapshot: "snapshot"})
	if err == nil || !strings.Contains(err.Error(), "repeated cursor") {
		t.Fatalf("repeated cursor error = %v", err)
	}
}

func TestProviderPreservesNotFoundAndCancellation(t *testing.T) {
	client := &fakeClient{attachErr: fmtError(ErrSandboxNotFound)}
	provider := NewProvider(client, ProviderOptions{})
	_, err := provider.GetOrCreate(t.Context(), SandboxOptions{SandboxID: "missing"})
	if !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("not found error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.GetOrCreate(ctx, SandboxOptions{})
	if !errors.Is(err, context.Canceled) || client.createCalls != 0 {
		t.Fatalf("canceled create = %v, calls = %d", err, client.createCalls)
	}
}

func TestProviderBlueprintListCancellationWinsOverTransportSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client := &fakeClient{cancelList: cancel, blueprintName: "never"}
	provider := NewProvider(client, ProviderOptions{ResolveEnv: func(string) (string, bool) { return "", false }})
	_, err := provider.GetOrCreate(ctx, SandboxOptions{Snapshot: "snapshot"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetOrCreate = %v", err)
	}
	if len(client.buildNames) != 0 || len(client.blueprintNames) != 0 {
		t.Fatalf("canceled list continued: builds %#v creates %#v", client.buildNames, client.blueprintNames)
	}
}

func fmtError(err error) error { return errors.Join(errors.New("remote"), err) }
