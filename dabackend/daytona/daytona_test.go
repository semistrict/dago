package daytona

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
)

type sessionCommandCall struct {
	sandboxID string
	sessionID string
	command   string
	timeout   time.Duration
}

type uploadBatch struct {
	sandboxID string
	files     []dabackend.Upload
	maxBytes  int64
}

type downloadBatch struct {
	sandboxID string
	paths     []string
	maxBytes  int64
}

type fakeTransport struct {
	created         []string
	deleted         []string
	commands        []sessionCommandCall
	statusCalls     int
	logCalls        int
	uploads         []uploadBatch
	downloads       []downloadBatch
	deleteCtxErrs   []error
	deleteDeadlines []bool

	commandID      string
	statuses       []CommandStatus
	logs           CommandLogs
	downloaded     []DownloadedFile
	createErr      error
	executeErr     error
	statusErr      error
	logsErr        error
	deleteErr      error
	uploadErr      error
	downloadErr    error
	cancelCreate   func()
	cancelStatus   func()
	cancelUpload   func()
	cancelDownload func()
}

func (transport *fakeTransport) CreateSession(_ context.Context, sandboxID, sessionID string) error {
	transport.created = append(transport.created, sandboxID+"|"+sessionID)
	if transport.cancelCreate != nil {
		transport.cancelCreate()
	}
	return transport.createErr
}

func (transport *fakeTransport) ExecuteSessionCommand(_ context.Context, sandboxID, sessionID, command string, timeout time.Duration) (string, error) {
	transport.commands = append(transport.commands, sessionCommandCall{sandboxID: sandboxID, sessionID: sessionID, command: command, timeout: timeout})
	if transport.commandID == "" {
		transport.commandID = "command-1"
	}
	return transport.commandID, transport.executeErr
}

func (transport *fakeTransport) GetSessionCommand(_ context.Context, _, _, _ string) (CommandStatus, error) {
	transport.statusCalls++
	if transport.cancelStatus != nil {
		transport.cancelStatus()
	}
	if len(transport.statuses) == 0 {
		return CommandStatus{}, transport.statusErr
	}
	status := transport.statuses[0]
	transport.statuses = transport.statuses[1:]
	return status, transport.statusErr
}

func (transport *fakeTransport) GetSessionCommandLogs(_ context.Context, _, _, _ string, _ int) (CommandLogs, error) {
	transport.logCalls++
	return transport.logs, transport.logsErr
}

func (transport *fakeTransport) DeleteSession(ctx context.Context, sandboxID, sessionID string) error {
	transport.deleted = append(transport.deleted, sandboxID+"|"+sessionID)
	transport.deleteCtxErrs = append(transport.deleteCtxErrs, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	transport.deleteDeadlines = append(transport.deleteDeadlines, hasDeadline)
	return transport.deleteErr
}

func (transport *fakeTransport) UploadFiles(_ context.Context, sandboxID string, files []dabackend.Upload, maxBytes int64) error {
	cloned := make([]dabackend.Upload, len(files))
	for index, file := range files {
		cloned[index] = dabackend.Upload{Path: file.Path, Content: append([]byte(nil), file.Content...)}
	}
	transport.uploads = append(transport.uploads, uploadBatch{sandboxID: sandboxID, files: cloned, maxBytes: maxBytes})
	if transport.cancelUpload != nil {
		transport.cancelUpload()
	}
	return transport.uploadErr
}

func (transport *fakeTransport) DownloadFiles(_ context.Context, sandboxID string, paths []string, maxBytes int64) ([]DownloadedFile, error) {
	transport.downloads = append(transport.downloads, downloadBatch{sandboxID: sandboxID, paths: append([]string(nil), paths...), maxBytes: maxBytes})
	if transport.cancelDownload != nil {
		transport.cancelDownload()
	}
	return append([]DownloadedFile(nil), transport.downloaded...), transport.downloadErr
}

func complete(code int) CommandStatus { return CommandStatus{ExitCode: new(code)} }

func fixedSessionID() (string, error) { return "session-1", nil }

func TestExecutePollsCombinesLogsAndCleansUp(t *testing.T) {
	transport := &fakeTransport{
		statuses: []CommandStatus{{}, {}, complete(7)},
		logs:     CommandLogs{Stdout: "hello", Stderr: "  warning\n"},
	}
	elapsedCalls := make([]time.Duration, 0, 2)
	backend := New(transport, " sandbox-1 ", Options{
		SessionID: fixedSessionID,
		PollingStrategy: func(elapsed time.Duration) time.Duration {
			elapsedCalls = append(elapsedCalls, elapsed)
			return 250 * time.Millisecond
		},
	})
	times := []time.Time{time.Unix(0, 0), time.Unix(0, 0), time.Unix(0, int64(500*time.Millisecond)), time.Unix(5, 0)}
	backend.now = func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	waits := make([]time.Duration, 0, 2)
	backend.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	result, err := backend.Execute(t.Context(), "false", 0)
	if err != nil {
		t.Fatal(err)
	}
	if backend.ID() != "sandbox-1" || result.Output != "hello\n<stderr>warning</stderr>" || result.ExitCode == nil || *result.ExitCode != 7 || result.Truncated {
		t.Fatalf("Execute = %#v, ID = %q", result, backend.ID())
	}
	if !reflect.DeepEqual(elapsedCalls, []time.Duration{0, 500 * time.Millisecond}) || !reflect.DeepEqual(waits, []time.Duration{250 * time.Millisecond, 250 * time.Millisecond}) {
		t.Fatalf("polling = elapsed %#v waits %#v", elapsedCalls, waits)
	}
	if len(transport.commands) != 1 || transport.commands[0].timeout != defaultTimeout || !reflect.DeepEqual(transport.created, []string{"sandbox-1|session-1"}) || !reflect.DeepEqual(transport.deleted, []string{"sandbox-1|session-1"}) {
		t.Fatalf("session lifecycle = commands %#v created %#v deleted %#v", transport.commands, transport.created, transport.deleted)
	}
}

func TestExecuteTimeoutReturns124AndStillDeletesSession(t *testing.T) {
	transport := &fakeTransport{statuses: []CommandStatus{{}}}
	backend := New(transport, "sandbox", Options{SessionID: fixedSessionID})
	times := []time.Time{time.Unix(0, 0), time.Unix(0, 0), time.Unix(11, 0)}
	backend.now = func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	backend.wait = func(context.Context, time.Duration) error { return nil }
	result, err := backend.Execute(t.Context(), "sleep forever", 10*time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != timeoutExitCode || result.Output != "Command timed out after 10 seconds" {
		t.Fatalf("Execute = %#v, %v", result, err)
	}
	if transport.statusCalls != 1 || len(transport.deleted) != 1 || transport.logCalls != 0 {
		t.Fatalf("timeout lifecycle = status %d logs %d deletes %#v", transport.statusCalls, transport.logCalls, transport.deleted)
	}
}

func TestExecutePreservesExplicitZeroAndClampsPolling(t *testing.T) {
	transport := &fakeTransport{statuses: []CommandStatus{{}, complete(0)}}
	backend := New(transport, "sandbox", Options{
		SessionID:       fixedSessionID,
		MaxPollInterval: time.Second,
		PollingStrategy: func(time.Duration) time.Duration { return time.Hour },
	})
	backend.now = func() time.Time { return time.Unix(0, 0) }
	var waited time.Duration
	backend.wait = func(_ context.Context, delay time.Duration) error { waited = delay; return nil }
	zero := time.Duration(0)
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &zero}); err != nil {
		t.Fatal(err)
	}
	if transport.commands[0].timeout != 0 || waited != time.Second {
		t.Fatalf("explicit zero/clamp = command %#v wait %s", transport.commands[0], waited)
	}
}

func TestExecuteRejectsNegativePollingAndBoundsUTF8Output(t *testing.T) {
	transport := &fakeTransport{statuses: []CommandStatus{{}}}
	backend := New(transport, "sandbox", Options{
		SessionID:       fixedSessionID,
		PollingStrategy: func(time.Duration) time.Duration { return -time.Second },
	})
	backend.now = func() time.Time { return time.Unix(0, 0) }
	if _, err := backend.Execute(t.Context(), "true", 0); err == nil || !strings.Contains(err.Error(), "negative interval") {
		t.Fatalf("negative polling error = %v", err)
	}
	if len(transport.deleted) != 1 {
		t.Fatalf("negative polling did not clean up: %#v", transport.deleted)
	}

	transport = &fakeTransport{statuses: []CommandStatus{complete(0)}, logs: CommandLogs{Stdout: "1234é"}}
	backend = New(transport, "sandbox", Options{SessionID: fixedSessionID, MaxOutput: 5})
	backend.now = func() time.Time { return time.Unix(0, 0) }
	result, err := backend.Execute(t.Context(), "true", 0)
	if err != nil || result.Output != "1234" || !result.Truncated {
		t.Fatalf("bounded output = %#v, %v", result, err)
	}
}

func TestExecuteCancellationIgnoringTransportStillCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	transport := &fakeTransport{statuses: []CommandStatus{{}}, cancelStatus: cancel}
	backend := New(transport, "sandbox", Options{SessionID: fixedSessionID})
	backend.now = func() time.Time { return time.Unix(0, 0) }
	_, err := backend.Execute(ctx, "true", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v", err)
	}
	if len(transport.deleted) != 1 || transport.deleteCtxErrs[0] != nil || !transport.deleteDeadlines[0] {
		t.Fatalf("cleanup = deleted %#v ctx errors %#v deadlines %#v", transport.deleted, transport.deleteCtxErrs, transport.deleteDeadlines)
	}
}

func TestExecuteCancellationDuringSessionCreationStillCleansUp(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	transport := &fakeTransport{cancelCreate: cancel}
	backend := New(transport, "sandbox", Options{SessionID: fixedSessionID})
	_, err := backend.Execute(ctx, "true", 0)
	if !errors.Is(err, context.Canceled) || len(transport.deleted) != 1 || transport.deleteCtxErrs[0] != nil {
		t.Fatalf("Execute = %v, deletes %#v, cleanup ctx %#v", err, transport.deleted, transport.deleteCtxErrs)
	}
}

func TestExecuteBoundsErrorsAndPreservesCleanupClassification(t *testing.T) {
	operationErr := errors.New("status failed")
	cleanupErr := errors.New("cleanup failed")
	transport := &fakeTransport{
		statusErr: operationErr,
		deleteErr: errors.Join(cleanupErr, errors.New(strings.Repeat("x", 1000))),
	}
	backend := New(transport, "sandbox", Options{SessionID: fixedSessionID})
	backend.now = func() time.Time { return time.Unix(0, 0) }
	_, err := backend.Execute(t.Context(), "true", 0)
	if !errors.Is(err, operationErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Execute classifications = %v", err)
	}
	if len(err.Error()) > 700 {
		t.Fatalf("Execute error was not bounded: %d bytes", len(err.Error()))
	}
}

func TestUploadValidatesPathsSizesBatchAndCancellation(t *testing.T) {
	transport := &fakeTransport{}
	backend := New(transport, "sandbox", Options{MaxFileSize: 4, MaxTransferFiles: 3})
	result := backend.Upload(t.Context(), []dabackend.Upload{
		{Path: "relative", Content: []byte("x")},
		{Path: "/ok", Content: []byte("1234")},
		{Path: "/large", Content: []byte("12345")},
		{Path: "/batch", Content: []byte("x")},
	})
	if result[0].Error != "invalid_path" || result[1].Error != "" || !strings.Contains(result[2].Error, "payload too large") || !strings.Contains(result[3].Error, "batch limit") {
		t.Fatalf("Upload = %#v", result)
	}
	if len(transport.uploads) != 1 || len(transport.uploads[0].files) != 1 || transport.uploads[0].files[0].Path != "/ok" || transport.uploads[0].maxBytes != 4 {
		t.Fatalf("upload batch = %#v", transport.uploads)
	}

	ctx, cancel := context.WithCancel(t.Context())
	transport.cancelUpload = cancel
	result = backend.Upload(ctx, []dabackend.Upload{{Path: "/one", Content: []byte("x")}})
	if !strings.Contains(result[0].Error, context.Canceled.Error()) {
		t.Fatalf("canceled Upload = %#v", result)
	}
}

func TestDownloadMapsByOpaqueAbsolutePathAndFailsClosed(t *testing.T) {
	path := "/work/a'; printf attacked\n"
	transport := &fakeTransport{downloaded: []DownloadedFile{
		{Path: path, Content: []byte("safe"), Found: true},
		{Path: "/missing", Found: false},
		{Path: "/large", Content: []byte("12345"), Found: true},
	}}
	backend := New(transport, "sandbox", Options{MaxFileSize: 4})
	result := backend.Download(t.Context(), []string{"relative", path, path, "/missing", "/large"})
	if result[0].Error != "invalid_path" || string(result[1].Content) != "safe" || string(result[2].Content) != "safe" || result[3].Error != "file_not_found" || !strings.Contains(result[4].Error, "payload too large") {
		t.Fatalf("Download = %#v", result)
	}
	if !reflect.DeepEqual(transport.downloads[0].paths, []string{path, "/missing", "/large"}) {
		t.Fatalf("paths = %#v", transport.downloads[0].paths)
	}

	transport.downloaded = []DownloadedFile{{Path: "/unexpected", Content: []byte("x"), Found: true}}
	result = backend.Download(t.Context(), []string{"/expected"})
	if !strings.Contains(result[0].Error, "invalid download response") || len(result[0].Content) != 0 {
		t.Fatalf("unexpected response = %#v", result)
	}
	transport.downloaded = []DownloadedFile{
		{Path: "/expected", Content: []byte("first"), Found: true},
		{Path: "/expected", Content: []byte("second"), Found: true},
	}
	result = backend.Download(t.Context(), []string{"/expected"})
	if !strings.Contains(result[0].Error, "invalid download response") || len(result[0].Content) != 0 {
		t.Fatalf("duplicate response = %#v", result)
	}

	ctx, cancel := context.WithCancel(t.Context())
	transport.cancelDownload = cancel
	transport.downloaded = []DownloadedFile{{Path: "/expected", Content: []byte("hidden"), Found: true}}
	result = backend.Download(ctx, []string{"/expected"})
	if !strings.Contains(result[0].Error, context.Canceled.Error()) || len(result[0].Content) != 0 {
		t.Fatalf("canceled response = %#v", result)
	}
}

func TestBackendDerivesFileOperationsThroughBaseSandbox(t *testing.T) {
	transport := &fakeTransport{
		statuses: []CommandStatus{complete(0)},
		logs:     CommandLogs{Stdout: `{"entries":[]}`},
	}
	backend := New(transport, "sandbox", Options{SessionID: fixedSessionID})
	backend.now = func() time.Time { return time.Unix(0, 0) }
	result, err := backend.List(t.Context(), "/workspace")
	if err != nil || len(result.Entries) != 0 {
		t.Fatalf("List = %#v, %v", result, err)
	}
	if len(transport.commands) != 1 || !strings.HasPrefix(transport.commands[0].command, "python3 - ") {
		t.Fatalf("derived command = %#v", transport.commands)
	}
}

func TestNewRejectsInvalidStaticInputs(t *testing.T) {
	tests := []func(){
		func() { New(nil, "sandbox", Options{}) },
		func() {
			var transport *fakeTransport
			New(transport, "sandbox", Options{})
		},
		func() { New(&fakeTransport{}, " ", Options{}) },
		func() { New(&fakeTransport{}, "sandbox", Options{MaxOutput: -1}) },
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

func TestDefaultSessionIDIsUUIDv4(t *testing.T) {
	id, err := randomSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[14] != '4' || id[18] != '-' || id[23] != '-' || (id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b') {
		t.Fatalf("session ID = %q", id)
	}
}
