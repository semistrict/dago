package modal

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
	argv      []string
	timeout   time.Duration
	maxOutput int
}

type transferCall struct {
	sandboxID string
	path      string
	content   []byte
	maxBytes  int64
}

type fakeTransport struct {
	executeCalls  []executeCall
	uploadCalls   []transferCall
	downloadCalls []transferCall

	executeResult CommandResult
	executeErr    error
	uploadErr     error
	downloadData  []byte
	downloadErr   error
}

func (transport *fakeTransport) Execute(_ context.Context, sandboxID string, argv []string, timeout time.Duration, maxOutput int) (CommandResult, error) {
	transport.executeCalls = append(transport.executeCalls, executeCall{
		sandboxID: sandboxID, argv: append([]string(nil), argv...), timeout: timeout, maxOutput: maxOutput,
	})
	return transport.executeResult, transport.executeErr
}

func (transport *fakeTransport) Upload(_ context.Context, sandboxID, path string, content []byte, maxBytes int64) error {
	transport.uploadCalls = append(transport.uploadCalls, transferCall{
		sandboxID: sandboxID, path: path, content: append([]byte(nil), content...), maxBytes: maxBytes,
	})
	return transport.uploadErr
}

func (transport *fakeTransport) Download(_ context.Context, sandboxID, path string, maxBytes int64) ([]byte, error) {
	transport.downloadCalls = append(transport.downloadCalls, transferCall{sandboxID: sandboxID, path: path, maxBytes: maxBytes})
	return append([]byte(nil), transport.downloadData...), transport.downloadErr
}

func TestBackendUsesStableIdentityBashBoundaryAndUsefulDefaults(t *testing.T) {
	transport := &fakeTransport{executeResult: CommandResult{Stdout: "out", Stderr: "err", ExitCode: 9}}
	backend := New(transport, " modal-1 ", Options{})
	if backend.ID() != "modal-1" {
		t.Fatalf("ID = %q", backend.ID())
	}
	result, err := backend.Execute(t.Context(), "printf hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "out\nerr" || result.ExitCode == nil || *result.ExitCode != 9 || result.Truncated {
		t.Fatalf("Execute = %#v", result)
	}
	want := []executeCall{{
		sandboxID: "modal-1", argv: []string{"bash", "-c", "printf hello"}, timeout: 30 * time.Minute, maxOutput: 1 << 20,
	}}
	if !reflect.DeepEqual(transport.executeCalls, want) {
		t.Fatalf("calls = %#v, want %#v", transport.executeCalls, want)
	}
}

func TestBackendPreservesExplicitZeroAndBoundsUTF8Output(t *testing.T) {
	transport := &fakeTransport{executeResult: CommandResult{Stdout: "1234é", ExitCode: 0}}
	backend := New(transport, "modal-1", Options{MaxOutput: 5})
	result, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zero := time.Duration(0)
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &zero}); err != nil {
		t.Fatal(err)
	}
	if result.Output != "1234" || !result.Truncated || transport.executeCalls[0].timeout != defaultTimeout || transport.executeCalls[1].timeout != 0 {
		t.Fatalf("result = %#v, calls = %#v", result, transport.executeCalls)
	}
	negative := -time.Second
	if _, err := backend.ExecuteWithOptions(t.Context(), "true", dabackend.ExecuteOptions{Timeout: &negative}); err == nil {
		t.Fatal("negative timeout succeeded")
	}
}

func TestBackendTransfersValidatePathsBoundsAndClassifications(t *testing.T) {
	transport := &fakeTransport{}
	backend := New(transport, "modal-1", Options{MaxFileSize: 4, MaxTransferFiles: 3})
	uploads := backend.Upload(t.Context(), []dabackend.Upload{
		{Path: "relative", Content: []byte("x")},
		{Path: "/large", Content: []byte("12345")},
		{Path: "/ok", Content: []byte("1234")},
		{Path: "/batch", Content: []byte("x")},
	})
	if uploads[0].Error != "invalid_path" || !strings.Contains(uploads[1].Error, "payload too large") || uploads[2].Error != "" || !strings.Contains(uploads[3].Error, "batch limit") {
		t.Fatalf("Upload = %#v", uploads)
	}
	if len(transport.uploadCalls) != 1 || transport.uploadCalls[0].path != "/ok" || transport.uploadCalls[0].maxBytes != 4 {
		t.Fatalf("upload calls = %#v", transport.uploadCalls)
	}

	tests := []struct {
		err     error
		wrapped bool
		want    string
	}{
		{err: ErrFileNotFound, wrapped: true, want: "file_not_found"},
		{err: ErrIsDirectory, wrapped: true, want: "is_directory"},
		{err: ErrPermissionDenied, wrapped: true, want: "permission_denied"},
		{err: errors.New(strings.Repeat("x", 500)), want: "modal backend: " + strings.Repeat("x", 300) + "..."},
	}
	for _, test := range tests {
		transport.downloadErr = test.err
		if test.wrapped {
			transport.downloadErr = errors.Join(errors.New("remote"), test.err)
		}
		result := backend.Download(t.Context(), []string{"/file"})
		if result[0].Error != test.want {
			t.Fatalf("Download error for %v = %q, want %q", test.err, result[0].Error, test.want)
		}
	}
	transport.downloadErr = nil
	transport.downloadData = []byte("12345")
	result := backend.Download(t.Context(), []string{"relative", "/file"})
	if result[0].Error != "invalid_path" || !strings.Contains(result[1].Error, "payload too large") || len(result[1].Content) != 0 {
		t.Fatalf("Download = %#v", result)
	}
}

func TestBackendCancellationStopsRemoteOperations(t *testing.T) {
	transport := &fakeTransport{}
	backend := New(transport, "modal-1", Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := backend.Execute(ctx, "true", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v", err)
	}
	uploads := backend.Upload(ctx, []dabackend.Upload{{Path: "/never", Content: []byte("x")}})
	downloads := backend.Download(ctx, []string{"/never"})
	if uploads[0].Error != context.Canceled.Error() || downloads[0].Error != context.Canceled.Error() || len(transport.uploadCalls) != 0 || len(transport.downloadCalls) != 0 {
		t.Fatalf("uploads = %#v, downloads = %#v, transport = %#v", uploads, downloads, transport)
	}
}

func TestBackendPreservesOpaqueAbsolutePathsAndDerivesFilesystem(t *testing.T) {
	transport := &fakeTransport{executeResult: CommandResult{Stdout: `{"entries":[]}`, ExitCode: 0}}
	backend := New(transport, "modal-1", Options{})
	path := "/workspace/name with spaces;$(opaque)"
	backend.Upload(t.Context(), []dabackend.Upload{{Path: path, Content: []byte("x")}})
	backend.Download(t.Context(), []string{path})
	if transport.uploadCalls[0].path != path || transport.downloadCalls[0].path != path {
		t.Fatalf("paths were changed: upload %#v download %#v", transport.uploadCalls, transport.downloadCalls)
	}
	listing, err := backend.List(t.Context(), "/workspace")
	if err != nil || len(listing.Entries) != 0 {
		t.Fatalf("List = %#v, %v", listing, err)
	}
	last := transport.executeCalls[len(transport.executeCalls)-1]
	if len(last.argv) != 3 || last.argv[0] != "bash" || !strings.HasPrefix(last.argv[2], "python3 - ") || !strings.Contains(last.argv[2], "__DAGO_SANDBOX_PY__") {
		t.Fatalf("derived command = %#v", last)
	}
}

func TestBackendPreservesTransportErrorIdentity(t *testing.T) {
	sentinel := errors.New("sentinel")
	transport := &fakeTransport{executeErr: sentinel}
	backend := New(transport, "modal-1", Options{})
	_, err := backend.Execute(t.Context(), "true", 0)
	if !errors.Is(err, sentinel) || !strings.HasPrefix(err.Error(), "modal backend: execute:") {
		t.Fatalf("Execute error = %v", err)
	}
}

func TestNewRejectsInvalidStaticConfiguration(t *testing.T) {
	tests := []struct {
		name string
		call func()
	}{
		{name: "nil transport", call: func() { New(nil, "id", Options{}) }},
		{name: "typed nil transport", call: func() {
			var transport *fakeTransport
			New(transport, "id", Options{})
		}},
		{name: "empty id", call: func() { New(&fakeTransport{}, " ", Options{}) }},
		{name: "negative timeout", call: func() { New(&fakeTransport{}, "id", Options{DefaultTimeout: -1}) }},
		{name: "negative bound", call: func() { New(&fakeTransport{}, "id", Options{MaxFileSize: -1}) }},
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
