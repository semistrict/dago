package agentcore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
)

type startCall struct {
	region   string
	source   string
	deadline bool
}

type executeCall struct {
	sessionID string
	command   string
	maxOutput int
	maxEvents int
	deadline  bool
}

type writeCall struct {
	sessionID string
	files     []UploadFile
	maxBytes  int64
}

type readCall struct {
	sessionID string
	paths     []string
	maxBytes  int64
}

type fakeTransport struct {
	starts   []startCall
	stops    []string
	executes []executeCall
	writes   []writeCall
	reads    []readCall

	sessionIDs      []string
	startErr        error
	stopErr         error
	executeEvents   []ExecuteEvent
	executeErr      error
	writeErr        error
	resources       []FileResource
	readErr         error
	executeFunc     func(string) ([]ExecuteEvent, error)
	cancelStart     func()
	cancelExecute   func()
	cancelWrite     func()
	cancelRead      func()
	stopHasDeadline []bool
}

func (transport *fakeTransport) Start(ctx context.Context, region, source string) (string, error) {
	_, deadline := ctx.Deadline()
	transport.starts = append(transport.starts, startCall{region: region, source: source, deadline: deadline})
	if transport.cancelStart != nil {
		transport.cancelStart()
	}
	sessionID := "session-1"
	if len(transport.sessionIDs) > 0 {
		sessionID = transport.sessionIDs[0]
		transport.sessionIDs = transport.sessionIDs[1:]
	}
	return sessionID, transport.startErr
}

func (transport *fakeTransport) Stop(ctx context.Context, sessionID string) error {
	transport.stops = append(transport.stops, sessionID)
	_, deadline := ctx.Deadline()
	transport.stopHasDeadline = append(transport.stopHasDeadline, deadline)
	return transport.stopErr
}

func (transport *fakeTransport) Execute(ctx context.Context, sessionID, command string, maxOutput, maxEvents int) ([]ExecuteEvent, error) {
	_, deadline := ctx.Deadline()
	transport.executes = append(transport.executes, executeCall{sessionID: sessionID, command: command, maxOutput: maxOutput, maxEvents: maxEvents, deadline: deadline})
	if transport.cancelExecute != nil {
		transport.cancelExecute()
	}
	if transport.executeFunc != nil {
		return transport.executeFunc(command)
	}
	return append([]ExecuteEvent(nil), transport.executeEvents...), transport.executeErr
}

func (transport *fakeTransport) WriteFiles(_ context.Context, sessionID string, files []UploadFile, maxBytes int64) error {
	cloned := make([]UploadFile, len(files))
	for index, file := range files {
		cloned[index] = UploadFile{Path: file.Path, Text: file.Text, Blob: append([]byte(nil), file.Blob...)}
	}
	transport.writes = append(transport.writes, writeCall{sessionID: sessionID, files: cloned, maxBytes: maxBytes})
	if transport.cancelWrite != nil {
		transport.cancelWrite()
	}
	return transport.writeErr
}

func (transport *fakeTransport) ReadFiles(_ context.Context, sessionID string, paths []string, maxBytes int64) ([]FileResource, error) {
	transport.reads = append(transport.reads, readCall{sessionID: sessionID, paths: append([]string(nil), paths...), maxBytes: maxBytes})
	if transport.cancelRead != nil {
		transport.cancelRead()
	}
	return append([]FileResource(nil), transport.resources...), transport.readErr
}

func text(value string) *string { return &value }
func blob(value []byte) *[]byte {
	cloned := append([]byte(nil), value...)
	return &cloned
}

func TestExecuteExtractsStreamSemanticsAndBoundsOutput(t *testing.T) {
	transport := &fakeTransport{executeEvents: []ExecuteEvent{
		{Content: []OutputItem{{Type: OutputText, Text: text("line 1")}}},
		{Content: []OutputItem{{Type: OutputError, Text: text("failed")}}},
		{ExitCode: new(42), Content: []OutputItem{{Type: OutputText, Text: text("ééé")}}},
	}}
	backend := New(transport, " session-1 ", Options{MaxOutput: 24})
	result, err := backend.Execute(t.Context(), "echo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if backend.ID() != "session-1" || result.Output != "line 1\nError: failed\né" || result.ExitCode == nil || *result.ExitCode != 42 || !result.Truncated {
		t.Fatalf("Execute = %#v, ID = %q", result, backend.ID())
	}
	if len(transport.executes) != 1 || transport.executes[0].maxOutput != 24 || transport.executes[0].maxEvents != defaultMaxResults || !transport.executes[0].deadline {
		t.Fatalf("execute call = %#v", transport.executes)
	}
}

func TestExecuteFailsClosedOnTooManyEvents(t *testing.T) {
	transport := &fakeTransport{executeEvents: []ExecuteEvent{{}, {}}}
	backend := New(transport, "session", Options{MaxResults: 1})
	result, err := backend.Execute(t.Context(), "true", time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 1 || !strings.Contains(result.Output, "event limit") {
		t.Fatalf("Execute = %#v, %v", result, err)
	}
	if transport.executes[0].maxEvents != 1 {
		t.Fatalf("max events = %d", transport.executes[0].maxEvents)
	}
}

func TestExecuteDefaultsExitCodeAndHandlesErrorsAsResults(t *testing.T) {
	transport := &fakeTransport{executeEvents: []ExecuteEvent{{Content: []OutputItem{{Type: OutputText, Text: text("ok")}}}}}
	backend := New(transport, "session", Options{})
	result, err := backend.Execute(t.Context(), "true", time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 0 || result.Output != "ok" {
		t.Fatalf("default exit = %#v, %v", result, err)
	}

	transport.executeErr = ErrSessionExpired
	result, err = backend.Execute(t.Context(), "true", time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 1 || !strings.Contains(result.Output, "expired") {
		t.Fatalf("expired result = %#v, %v", result, err)
	}

	transport.executeErr = errors.New(strings.Repeat("x", 1000))
	result, err = backend.Execute(t.Context(), "true", time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 1 || len(result.Output) > len("Error executing command: ")+303 {
		t.Fatalf("bounded error result = %#v, %v", result, err)
	}
}

func TestExecuteExplicitZeroAndCancellationIgnoringTransport(t *testing.T) {
	transport := &fakeTransport{}
	backend := New(transport, "session", Options{})
	zero := time.Duration(0)
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &zero}); err != nil {
		t.Fatal(err)
	}
	if transport.executes[0].deadline {
		t.Fatal("explicit zero unexpectedly installed a deadline")
	}

	ctx, cancel := context.WithCancel(t.Context())
	transport.cancelExecute = cancel
	if _, err := backend.ExecuteWithOptions(ctx, "true", dabackend.ExecuteOptions{Timeout: &zero}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestUploadNormalizesPathsAndSelectsTextOrBlob(t *testing.T) {
	cwd := "/opt/sandbox/"
	transport := &fakeTransport{}
	backend := New(transport, "session", Options{WorkingDir: &cwd, MaxFileSize: 4, MaxTransferFiles: 3})
	result := backend.Upload(t.Context(), []dabackend.Upload{
		{Path: "/opt/sandbox/hello.txt", Content: []byte("text")},
		{Path: "/./data.bin", Content: []byte{0xff}},
		{Path: "/large", Content: []byte("12345")},
		{Path: "/batch", Content: []byte("x")},
	})
	if result[0].Error != "" || result[1].Error != "" || !strings.Contains(result[2].Error, "payload too large") || !strings.Contains(result[3].Error, "batch limit") {
		t.Fatalf("Upload = %#v", result)
	}
	files := transport.writes[0].files
	if files[0].Path != "hello.txt" || files[0].Text == nil || *files[0].Text != "text" || files[0].Blob != nil || files[1].Path != "data.bin" || files[1].Text != nil || !reflect.DeepEqual(files[1].Blob, []byte{0xff}) {
		t.Fatalf("write files = %#v", files)
	}
	if transport.writes[0].maxBytes != 4 {
		t.Fatalf("max bytes = %d", transport.writes[0].maxBytes)
	}
}

func TestUploadMapsFailuresAndCancellation(t *testing.T) {
	transport := &fakeTransport{writeErr: ErrSessionExpired}
	backend := New(transport, "session", Options{})
	result := backend.Upload(t.Context(), []dabackend.Upload{{Path: "/file", Content: []byte("x")}})
	if result[0].Error != "permission_denied" {
		t.Fatalf("expired Upload = %#v", result)
	}

	ctx, cancel := context.WithCancel(t.Context())
	transport.writeErr = nil
	transport.cancelWrite = cancel
	result = backend.Upload(ctx, []dabackend.Upload{{Path: "/file", Content: []byte("x")}})
	if !strings.Contains(result[0].Error, context.Canceled.Error()) {
		t.Fatalf("canceled Upload = %#v", result)
	}
}

func TestDownloadNormalizesPathsPreservesBinaryAndPartialSuccess(t *testing.T) {
	cwd := "/opt/sandbox"
	transport := &fakeTransport{resources: []FileResource{
		{URI: "file:///workspace/hello.txt", Text: text("hello")},
		{URI: "file:///data/image.png", Blob: blob([]byte{0x89, 0x50})},
	}}
	backend := New(transport, "session", Options{WorkingDir: &cwd})
	result := backend.Download(t.Context(), []string{
		"/opt/sandbox/workspace/hello.txt",
		"./data/image.png",
		"/missing",
	})
	if string(result[0].Content) != "hello" || !reflect.DeepEqual(result[1].Content, []byte{0x89, 0x50}) || result[2].Error != "file_not_found" {
		t.Fatalf("Download = %#v", result)
	}
	if !reflect.DeepEqual(transport.reads[0].paths, []string{"workspace/hello.txt", "data/image.png", "missing"}) {
		t.Fatalf("read paths = %#v", transport.reads[0].paths)
	}
}

func TestDownloadFailsClosedOnAdversarialResponsesAndBounds(t *testing.T) {
	root := "/"
	transport := &fakeTransport{resources: []FileResource{{URI: "file:///unexpected", Text: text("secret")}}}
	backend := New(transport, "session", Options{WorkingDir: &root, MaxFileSize: 4})
	result := backend.Download(t.Context(), []string{"/expected"})
	if result[0].Error != "file_not_found" || len(result[0].Content) != 0 {
		t.Fatalf("unexpected resource = %#v", result)
	}

	transport.resources = []FileResource{{URI: "file:///expected", Text: text("first")}, {URI: "file:///expected", Text: text("again")}}
	result = backend.Download(t.Context(), []string{"/expected"})
	if result[0].Error != "file_not_found" || len(result[0].Content) != 0 {
		t.Fatalf("duplicate resource = %#v", result)
	}

	transport.resources = []FileResource{{URI: "file:///expected", Blob: blob([]byte("12345"))}}
	result = backend.Download(t.Context(), []string{"/expected"})
	if !strings.Contains(result[0].Error, "payload too large") || len(result[0].Content) != 0 {
		t.Fatalf("oversized resource = %#v", result)
	}

	ctx, cancel := context.WithCancel(t.Context())
	transport.cancelRead = cancel
	transport.resources = []FileResource{{URI: "file:///expected", Text: text("hidden")}}
	result = backend.Download(ctx, []string{"/expected"})
	if !strings.Contains(result[0].Error, context.Canceled.Error()) || len(result[0].Content) != 0 {
		t.Fatalf("canceled resource = %#v", result)
	}
}

func TestDownloadMapsSessionExpiryDifferentlyFromOtherFailures(t *testing.T) {
	root := "/"
	transport := &fakeTransport{readErr: ErrSessionExpired}
	backend := New(transport, "session", Options{WorkingDir: &root})
	if result := backend.Download(t.Context(), []string{"/a"}); result[0].Error != "permission_denied" {
		t.Fatalf("expired Download = %#v", result)
	}
	transport.readErr = errors.New("network")
	if result := backend.Download(t.Context(), []string{"/a"}); result[0].Error != "file_not_found" {
		t.Fatalf("failed Download = %#v", result)
	}
}

func TestLazyWorkingDirectoryAndWritePathMatchExecution(t *testing.T) {
	transport := &fakeTransport{}
	transport.executeFunc = func(command string) ([]ExecuteEvent, error) {
		if command == "pwd" {
			return []ExecuteEvent{{ExitCode: new(0), Content: []OutputItem{{Type: OutputText, Text: text("/opt/sandbox\n")}}}}, nil
		}
		return []ExecuteEvent{{ExitCode: new(0), Content: []OutputItem{{Type: OutputText, Text: text("{}")}}}}, nil
	}
	backend := New(transport, "session", Options{})
	result, err := backend.Write(t.Context(), "/workspace/hello.py", "print('hi')")
	if err != nil || result.Path != "/opt/sandbox/workspace/hello.py" {
		t.Fatalf("Write = %#v, %v", result, err)
	}
	if len(transport.executes) != 2 || transport.executes[0].command != "pwd" || len(transport.writes) != 1 || transport.writes[0].files[0].Path != "workspace/hello.py" {
		t.Fatalf("calls = executes %#v writes %#v", transport.executes, transport.writes)
	}
	resolved, err := backend.absolutePath(t.Context(), "/tmp/script.py")
	if err != nil || resolved != "/opt/sandbox/tmp/script.py" || len(transport.executes) != 2 {
		t.Fatalf("cached cwd = %q, %v; executes %#v", resolved, err, transport.executes)
	}
}

func TestRootAndVirtualPathNormalization(t *testing.T) {
	root := "/"
	backend := New(&fakeTransport{}, "session", Options{WorkingDir: &root})
	resolved, err := backend.absolutePath(t.Context(), "/workspace/a")
	if err != nil || resolved != "/workspace/a" {
		t.Fatalf("root path = %q, %v", resolved, err)
	}
	if backend.relativePath("/././data/a") != "data/a" || normalizeRelative("///triple") != "triple" {
		t.Fatalf("relative normalization failed")
	}
}

func TestBaseSandboxOperationsUseResolvedWorkingDirectory(t *testing.T) {
	cwd := "/opt/sandbox"
	transport := &fakeTransport{executeEvents: []ExecuteEvent{{ExitCode: new(0), Content: []OutputItem{{Type: OutputText, Text: text(`{"entries":[]}`)}}}}}
	backend := New(transport, "session", Options{WorkingDir: &cwd})
	result, err := backend.List(t.Context(), "/workspace")
	if err != nil || len(result.Entries) != 0 {
		t.Fatalf("List = %#v, %v", result, err)
	}
	if len(transport.executes) != 1 || !strings.Contains(transport.executes[0].command, "python3 - ") {
		t.Fatalf("derived execute = %#v", transport.executes)
	}
}

func TestProviderRegionLifecycleLimitsAndNoReconnect(t *testing.T) {
	env := map[string]string{"AWS_DEFAULT_REGION": "ap-southeast-1"}
	transport := &fakeTransport{sessionIDs: []string{"session-1", "session-2"}}
	provider := NewProvider(transport, ProviderOptions{
		ResolveEnv:        func(name string) (string, bool) { value, ok := env[name]; return value, ok },
		MaxActiveSessions: 1,
	})
	backend, err := provider.Create(t.Context())
	if err != nil || backend.ID() != "session-1" || provider.Region() != "ap-southeast-1" {
		t.Fatalf("Create = %#v, %v; region %q", backend, err, provider.Region())
	}
	if len(transport.starts) != 1 || transport.starts[0].source != defaultSource || !transport.starts[0].deadline {
		t.Fatalf("start = %#v", transport.starts)
	}
	if _, err := provider.Create(t.Context()); !errors.Is(err, ErrActiveSessionLimit) {
		t.Fatalf("active limit = %v", err)
	}
	if _, err := provider.Attach(t.Context(), "session-1"); !errors.Is(err, ErrReconnectUnsupported) {
		t.Fatalf("Attach = %v", err)
	}
	if err := provider.Delete(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transport.stops, []string{"session-1"}) || !transport.stopHasDeadline[0] {
		t.Fatalf("stops = %#v deadlines %#v", transport.stops, transport.stopHasDeadline)
	}
	if err := provider.Delete(t.Context(), "session-1"); err != nil || len(transport.stops) != 1 {
		t.Fatalf("idempotent Delete = %v, stops %#v", err, transport.stops)
	}
}

func TestProviderRegionPrecedenceAndStartFailureCleanup(t *testing.T) {
	env := map[string]string{"AWS_REGION": "eu-west-1", "AWS_DEFAULT_REGION": "ignored"}
	startErr := errors.New(strings.Repeat("x", 1000))
	transport := &fakeTransport{sessionIDs: []string{"failed-session"}, startErr: startErr}
	provider := NewProvider(transport, ProviderOptions{ResolveEnv: func(name string) (string, bool) { value, ok := env[name]; return value, ok }})
	_, err := provider.Create(t.Context())
	if !errors.Is(err, startErr) || len(err.Error()) > len("agentcore provider: start: ")+303 || provider.Region() != "eu-west-1" {
		t.Fatalf("Create error = %v, region %q", err, provider.Region())
	}
	if !reflect.DeepEqual(transport.stops, []string{"failed-session"}) || !transport.stopHasDeadline[0] {
		t.Fatalf("cleanup = stops %#v deadlines %#v", transport.stops, transport.stopHasDeadline)
	}
}

func TestProviderExplicitAndDefaultRegionPrecedence(t *testing.T) {
	env := map[string]string{"AWS_REGION": "eu-west-1", "AWS_DEFAULT_REGION": "ap-south-1"}
	resolve := func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	if region := NewProvider(&fakeTransport{}, ProviderOptions{Region: " ca-central-1 ", ResolveEnv: resolve}).Region(); region != "ca-central-1" {
		t.Fatalf("explicit region = %q", region)
	}
	if region := NewProvider(&fakeTransport{}, ProviderOptions{ResolveEnv: func(string) (string, bool) { return "", false }}).Region(); region != defaultRegion {
		t.Fatalf("default region = %q", region)
	}
}

func TestProviderCancellationIgnoringStartCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	transport := &fakeTransport{sessionIDs: []string{"canceled-session"}, cancelStart: cancel}
	provider := NewProvider(transport, ProviderOptions{})
	_, err := provider.Create(ctx)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(transport.stops, []string{"canceled-session"}) {
		t.Fatalf("Create = %v, stops %#v", err, transport.stops)
	}
}

func TestProviderDeleteReturnsBoundedStopErrorAndForgetsSession(t *testing.T) {
	stopClass := errors.New("stop failed")
	transport := &fakeTransport{stopErr: errors.Join(stopClass, errors.New(strings.Repeat("x", 1000)))}
	provider := NewProvider(transport, ProviderOptions{})
	backend, err := provider.Create(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = provider.Delete(t.Context(), backend.ID())
	if !errors.Is(err, stopClass) || len(err.Error()) > len("agentcore provider: stop: ")+303 {
		t.Fatalf("Delete error = %v", err)
	}
	transport.stopErr = nil
	if err := provider.Delete(t.Context(), backend.ID()); err != nil || len(transport.stops) != 1 {
		t.Fatalf("second Delete = %v, stops %#v", err, transport.stops)
	}
}

func TestNewRejectsInvalidStaticInputs(t *testing.T) {
	tests := []func(){
		func() { New(nil, "session", Options{}) },
		func() {
			var transport *fakeTransport
			New(transport, "session", Options{})
		},
		func() { New(&fakeTransport{}, " ", Options{}) },
		func() { New(&fakeTransport{}, "session", Options{MaxOutput: -1}) },
		func() { New(&fakeTransport{}, "session", Options{WorkingDir: new("relative")}) },
		func() { NewProvider(nil, ProviderOptions{}) },
		func() { NewProvider(&fakeTransport{}, ProviderOptions{MaxActiveSessions: -1}) },
	}
	for index, call := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("constructor did not panic")
				}
			}()
			call()
		})
	}
}
