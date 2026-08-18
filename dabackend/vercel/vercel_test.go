package vercel

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

type startCall struct {
	sandboxID string
	program   string
	args      []string
}

type fakeCommand struct {
	release chan struct{}
	once    sync.Once

	finished Finished
	waitErr  error
	stdout   string
	stderr   string
	logsErr  error
	logBound int
	kills    int
	killErr  error
}

func immediateCommand(exitCode int) *fakeCommand {
	release := make(chan struct{})
	close(release)
	return &fakeCommand{release: release, finished: Finished{ExitCode: exitCode}}
}

func (command *fakeCommand) Wait() (Finished, error) {
	<-command.release
	return command.finished, command.waitErr
}

func (command *fakeCommand) Kill(context.Context) error {
	command.kills++
	command.once.Do(func() { close(command.release) })
	return command.killErr
}

func (command *fakeCommand) Logs(_ context.Context, maxOutput int) (string, string, bool, error) {
	command.logBound = maxOutput
	return command.stdout, command.stderr, false, command.logsErr
}

type fakeTransport struct {
	startCalls    []startCall
	uploadCalls   [][]dabackend.Upload
	downloadCalls []string
	startSignal   chan struct{}
	startOnce     sync.Once
	startDelay    time.Duration

	command      Command
	startErr     error
	uploadErr    error
	downloadData map[string][]byte
	downloadErr  map[string]error
}

func (transport *fakeTransport) Start(_ context.Context, sandboxID, program string, args []string) (Command, error) {
	if transport.startDelay > 0 {
		time.Sleep(transport.startDelay)
	}
	transport.startCalls = append(transport.startCalls, startCall{sandboxID: sandboxID, program: program, args: append([]string(nil), args...)})
	if transport.startSignal != nil {
		transport.startOnce.Do(func() { close(transport.startSignal) })
	}
	return transport.command, transport.startErr
}

func (transport *fakeTransport) Upload(_ context.Context, _ string, uploads []dabackend.Upload, _ int64) error {
	copyUploads := make([]dabackend.Upload, len(uploads))
	for index, upload := range uploads {
		copyUploads[index] = dabackend.Upload{Path: upload.Path, Content: append([]byte(nil), upload.Content...)}
	}
	transport.uploadCalls = append(transport.uploadCalls, copyUploads)
	return transport.uploadErr
}

func (transport *fakeTransport) Download(_ context.Context, _ string, path string, _ int64) ([]byte, error) {
	transport.downloadCalls = append(transport.downloadCalls, path)
	return append([]byte(nil), transport.downloadData[path]...), transport.downloadErr[path]
}

func TestBackendUsesStableIdentityBashLoginShellAndDefaults(t *testing.T) {
	command := immediateCommand(7)
	command.stdout = "out"
	command.stderr = " err\n"
	transport := &fakeTransport{command: command}
	backend := New(transport, " sb_123 ", Options{})
	if backend.ID() != "sb_123" {
		t.Fatalf("ID = %q", backend.ID())
	}
	result, err := backend.Execute(t.Context(), "exit 7", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "out\n<stderr>err</stderr>" || result.ExitCode == nil || *result.ExitCode != 7 || result.Truncated {
		t.Fatalf("Execute = %#v", result)
	}
	want := []startCall{{sandboxID: "sb_123", program: "bash", args: []string{"-lc", "exit 7"}}}
	if !reflect.DeepEqual(transport.startCalls, want) || command.logBound != defaultMaxOutput {
		t.Fatalf("start calls = %#v, log bound = %d", transport.startCalls, command.logBound)
	}
}

func TestBackendTimeoutKillsAndReturns124(t *testing.T) {
	command := &fakeCommand{release: make(chan struct{})}
	transport := &fakeTransport{command: command}
	backend := New(transport, "sb", Options{})
	result, err := backend.ExecuteWithOptions(t.Context(), "sleep 10", dabackend.ExecuteOptions{Timeout: new(2 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 124 || result.Output != "Command timed out after 2ms" || command.kills != 1 {
		t.Fatalf("Execute = %#v, kills = %d", result, command.kills)
	}
}

func TestBackendTimeoutIncludesDetachedCommandStartup(t *testing.T) {
	command := &fakeCommand{release: make(chan struct{})}
	transport := &fakeTransport{command: command, startDelay: 5 * time.Millisecond}
	backend := New(transport, "sb", Options{})
	timeout := time.Millisecond
	result, err := backend.ExecuteWithOptions(t.Context(), "sleep 10", dabackend.ExecuteOptions{Timeout: &timeout})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 124 || command.kills != 1 {
		t.Fatalf("Execute = %#v, kills = %d", result, command.kills)
	}
}

func TestBackendExplicitZeroWaitsAndCancellationKills(t *testing.T) {
	command := immediateCommand(0)
	command.stdout = "done"
	transport := &fakeTransport{command: command}
	backend := New(transport, "sb", Options{})
	zero := time.Duration(0)
	result, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &zero})
	if err != nil || result.Output != "done" {
		t.Fatalf("explicit zero = %#v, %v", result, err)
	}

	pending := &fakeCommand{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = backend.Execute(ctx, "sleep 10", time.Hour)
	if !errors.Is(err, context.Canceled) || pending.kills != 0 {
		// Cancellation before Start must not create or kill a remote process.
		t.Fatalf("pre-canceled Execute = %v, kills = %d", err, pending.kills)
	}
	started := make(chan struct{})
	transport = &fakeTransport{command: pending, startSignal: started}
	backend = New(transport, "sb", Options{})
	ctx, cancel = context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	_, err = backend.Execute(ctx, "sleep 10", time.Hour)
	if !errors.Is(err, context.Canceled) || pending.kills != 1 {
		t.Fatalf("active cancellation = %v, kills = %d", err, pending.kills)
	}
}

func TestBackendLogFailurePreservesExitAndOutputIsBoundedUTF8(t *testing.T) {
	command := immediateCommand(9)
	command.logsErr = errors.New("network down")
	transport := &fakeTransport{command: command}
	backend := New(transport, "sb", Options{MaxOutput: 5})
	result, err := backend.Execute(t.Context(), "false", 0)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 9 || !strings.Contains(result.Output, "output unavailable") {
		t.Fatalf("log failure = %#v, %v", result, err)
	}
	command.logsErr = nil
	command.stdout = "1234é"
	result, err = backend.Execute(t.Context(), "true", 0)
	if err != nil || result.Output != "1234\n\n... Output truncated at 5 bytes." || !result.Truncated {
		t.Fatalf("bounded output = %#v, %v", result, err)
	}
}

func TestBackendUploadBatchesOnlyValidFilesAndMapsFailure(t *testing.T) {
	transport := &fakeTransport{command: immediateCommand(0), uploadErr: errors.New("access denied by policy")}
	backend := New(transport, "sb", Options{MaxFileSize: 4, MaxTransferFiles: 4})
	results := backend.Upload(t.Context(), []dabackend.Upload{
		{Path: "relative", Content: []byte("x")},
		{Path: "/one", Content: []byte("1")},
		{Path: "/large", Content: []byte("12345")},
		{Path: "/two", Content: []byte("2")},
		{Path: "/batch", Content: []byte("3")},
	})
	wantErrors := []string{"invalid_path", "permission_denied", "vercel backend: payload too large (maximum 4 bytes)", "permission_denied", "vercel backend: transfer batch limit exceeded"}
	for index, want := range wantErrors {
		if results[index].Error != want {
			t.Fatalf("result %d = %#v, want error %q", index, results[index], want)
		}
	}
	if len(transport.uploadCalls) != 1 || len(transport.uploadCalls[0]) != 2 || transport.uploadCalls[0][0].Path != "/one" || transport.uploadCalls[0][1].Path != "/two" {
		t.Fatalf("upload calls = %#v", transport.uploadCalls)
	}
}

func TestBackendDownloadMapsProviderErrorsWithoutGuessing(t *testing.T) {
	transport := &fakeTransport{
		command:      immediateCommand(0),
		downloadData: map[string][]byte{"/ok": []byte("ok")},
		downloadErr: map[string]error{
			"/missing":   errors.New("No such file or directory"),
			"/not-dir":   errors.New("not a directory"),
			"/directory": errors.Join(errors.New("remote"), ErrIsDirectory),
			"/sandbox":   ErrSandboxNotFound,
		},
	}
	backend := New(transport, "sb", Options{})
	paths := []string{"relative", "/ok", "/missing", "/not-dir", "/directory", "/sandbox"}
	results := backend.Download(t.Context(), paths)
	want := []string{"invalid_path", "", "file_not_found", "not a directory", "is_directory", "sandbox not found"}
	for index := range want {
		if results[index].Path != paths[index] || results[index].Error != want[index] {
			t.Fatalf("result %d = %#v, want error %q", index, results[index], want[index])
		}
	}
	if string(results[1].Content) != "ok" {
		t.Fatalf("content = %q", results[1].Content)
	}
}

func TestBackendDerivesFilesystemAndPreservesTransportErrors(t *testing.T) {
	command := immediateCommand(0)
	command.stdout = `{"entries":[]}`
	transport := &fakeTransport{command: command}
	backend := New(transport, "sb", Options{})
	listing, err := backend.List(t.Context(), "/workspace")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("List = %#v, %v", listing, err)
	}
	last := transport.startCalls[len(transport.startCalls)-1]
	if last.program != "bash" || len(last.args) != 2 || last.args[0] != "-lc" || !strings.HasPrefix(last.args[1], "python3 - ") {
		t.Fatalf("derived command = %#v", last)
	}

	sentinel := errors.New("sentinel")
	transport.startErr = sentinel
	_, err = backend.Execute(t.Context(), "true", 0)
	if !errors.Is(err, sentinel) {
		t.Fatalf("transport identity lost: %v", err)
	}
}

func TestNewRejectsInvalidStaticConfiguration(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "nil transport", call: func() { New(nil, "sb", Options{}) }},
		{name: "typed nil transport", call: func() {
			var transport *fakeTransport
			New(transport, "sb", Options{})
		}},
		{name: "empty id", call: func() { New(&fakeTransport{}, " ", Options{}) }},
		{name: "negative timeout", call: func() { New(&fakeTransport{}, "sb", Options{DefaultTimeout: -1}) }},
		{name: "negative bound", call: func() { New(&fakeTransport{}, "sb", Options{MaxOutput: -1}) }},
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
