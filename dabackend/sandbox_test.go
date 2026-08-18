package dabackend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type localSandboxTransport struct {
	root       string
	options    []ExecuteOptions
	uploads    [][]Upload
	maxOutput  int
	malformed  bool
	executeErr error
}

func (transport *localSandboxTransport) ID() string { return "test-sandbox" }

func (transport *localSandboxTransport) Execute(ctx context.Context, command string, timeout time.Duration) (ExecuteResult, error) {
	return transport.run(ctx, command, ExecuteOptions{Timeout: new(timeout)})
}

func (transport *localSandboxTransport) ExecuteWithOptions(ctx context.Context, command string, options ExecuteOptions) (ExecuteResult, error) {
	transport.options = append(transport.options, options)
	return transport.run(ctx, command, options)
}

func (transport *localSandboxTransport) run(ctx context.Context, command string, options ExecuteOptions) (ExecuteResult, error) {
	if transport.executeErr != nil {
		return ExecuteResult{}, transport.executeErr
	}
	if transport.malformed {
		code := 0
		return ExecuteResult{Output: "not json", ExitCode: &code}, nil
	}
	if options.Timeout != nil && *options.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *options.Timeout)
		defer cancel()
	}
	process := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	process.Dir = transport.root
	output, err := process.CombinedOutput()
	result := ExecuteResult{Output: string(output)}
	if err == nil {
		result.ExitCode = new(0)
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return ExecuteResult{}, err
		}
		result.ExitCode = new(exit.ExitCode())
	}
	if transport.maxOutput > 0 && len(result.Output) > transport.maxOutput {
		result.Output = result.Output[:transport.maxOutput]
		result.Truncated = true
	}
	return result, nil
}

func (transport *localSandboxTransport) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	transport.uploads = append(transport.uploads, append([]Upload(nil), uploads...))
	results := make([]UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		if err := ctx.Err(); err != nil {
			results[index].Error = err.Error()
			continue
		}
		if err := os.MkdirAll(filepath.Dir(upload.Path), 0o755); err != nil {
			results[index].Error = err.Error()
			continue
		}
		if err := os.WriteFile(upload.Path, upload.Content, 0o600); err != nil {
			results[index].Error = err.Error()
		}
	}
	return results
}

func newTestBaseSandbox(t *testing.T, options BaseSandboxOptions) (*BaseSandbox, *localSandboxTransport, string) {
	t.Helper()
	root := t.TempDir()
	transport := &localSandboxTransport{root: root}
	sandbox := NewBaseSandbox(transport, options)
	return sandbox, transport, root
}

func TestBaseSandboxDerivesBackendOperations(t *testing.T) {
	sandbox, transport, root := newTestBaseSandbox(t, BaseSandboxOptions{MaxResults: 10})
	ctx := context.Background()
	first := filepath.Join(root, "nested", "first.txt")
	second := filepath.Join(root, "nested", "second.txt")

	if result, err := sandbox.Write(ctx, first, "alpha\r\nbeta\r\ngamma\r\n"); err != nil || result.Path != first {
		t.Fatalf("write = %#v, %v", result, err)
	}
	if result, err := sandbox.Write(ctx, second, "beta two\n"); err != nil || result.Path != second {
		t.Fatalf("write second = %#v, %v", result, err)
	}
	if len(transport.uploads) != 2 {
		t.Fatalf("write uploads = %d", len(transport.uploads))
	}

	read, err := sandbox.Read(ctx, first, 1, 1)
	if err != nil || read.Data == nil || read.Data.Content != "beta" || read.StartLine == nil || *read.StartLine != 2 || read.NextOffset == nil || *read.NextOffset != 2 {
		t.Fatalf("read = %#v, %v", read, err)
	}
	if empty, err := sandbox.Read(ctx, first, 0, 0); err != nil || empty.Data == nil || !empty.NoLinesRequested {
		t.Fatalf("empty read = %#v, %v", empty, err)
	}
	binary, err := sandbox.ReadBinary(ctx, first, 1<<20)
	if err != nil || binary.Data == nil || binary.Data.Encoding != EncodingBase64 {
		t.Fatalf("binary read = %#v, %v", binary, err)
	}

	edit, err := sandbox.Edit(ctx, first, "beta\ngamma", "changed\nlast", false)
	if err != nil || edit.Occurrences != 1 {
		t.Fatalf("edit = %#v, %v", edit, err)
	}
	raw, err := os.ReadFile(first)
	if err != nil || string(raw) != "alpha\r\nchanged\r\nlast\r\n" {
		t.Fatalf("edited content = %q, %v", raw, err)
	}

	listing, err := sandbox.List(ctx, filepath.Join(root, "nested"))
	if err != nil || len(listing.Entries) != 2 || listing.Entries[0].Path != first {
		t.Fatalf("list = %#v, %v", listing, err)
	}
	rootFile := filepath.Join(root, "root.txt")
	if err := os.WriteFile(rootFile, []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}
	matched, err := sandbox.Glob(ctx, "**/*.txt", root)
	if err != nil || len(matched.Matches) != 3 || matched.Truncated {
		t.Fatalf("glob = %#v, %v", matched, err)
	}
	grep, err := sandbox.Grep(ctx, "changed", GrepOptions{Path: root, Glob: "**/*.txt", ContextLines: 1, Uncapped: true})
	if err != nil || len(grep.Matches) != 1 || grep.Matches[0].Line != 2 || len(grep.Matches[0].ContextBefore) != 1 || len(grep.Matches[0].ContextAfter) != 1 {
		t.Fatalf("grep = %#v, %v", grep, err)
	}

	downloads := sandbox.Download(ctx, []string{first, filepath.Join(root, "missing")})
	if len(downloads) != 2 || string(downloads[0].Content) != string(raw) || downloads[1].Error == "" {
		t.Fatalf("downloads = %#v", downloads)
	}
	if result, err := sandbox.Delete(ctx, filepath.Join(root, "nested")); err != nil || result.Path == "" {
		t.Fatalf("delete = %#v, %v", result, err)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted file stat = %v", err)
	}
}

func TestBaseSandboxUsesUploadsForLargeEditPayloads(t *testing.T) {
	sandbox, transport, root := newTestBaseSandbox(t, BaseSandboxOptions{})
	name := filepath.Join(root, "large.txt")
	old := strings.Repeat("old", 6000)
	replacement := strings.Repeat("new", 6000)
	if err := os.WriteFile(name, []byte("prefix"+old+"suffix"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := sandbox.Edit(context.Background(), name, old, replacement, false)
	if err != nil || result.Occurrences != 1 {
		t.Fatalf("large edit = %#v, %v", result, err)
	}
	if len(transport.uploads) != 1 || len(transport.uploads[0]) != 2 {
		t.Fatalf("large edit uploads = %#v", transport.uploads)
	}
	for _, upload := range transport.uploads[0] {
		if _, err := os.Stat(upload.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary file %q was not removed: %v", upload.Path, err)
		}
	}
}

func TestBaseSandboxBoundsGlobAndDefaultGrepResults(t *testing.T) {
	sandbox, _, root := newTestBaseSandbox(t, BaseSandboxOptions{MaxResults: 1})
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("match\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	glob, err := sandbox.Glob(context.Background(), "*.txt", root)
	if err != nil || len(glob.Matches) != 1 || !glob.Truncated {
		t.Fatalf("bounded glob = %#v, %v", glob, err)
	}
	grep, err := sandbox.Grep(context.Background(), "match", GrepOptions{Path: root})
	if err != nil || len(grep.Matches) != 1 || !grep.Truncated {
		t.Fatalf("bounded grep = %#v, %v", grep, err)
	}
}

func TestBaseSandboxCaptureOffloadPreservesOutputAndExitStatus(t *testing.T) {
	sandbox, _, root := newTestBaseSandbox(t, BaseSandboxOptions{EnableCaptureOffload: true, MaxCaptureBytes: 4096})
	capture := filepath.Join(root, "artifacts", "command-output")
	result, err := sandbox.ExecuteWithOffload(context.Background(), "printf 'one\\ntwo\\nthree\\nfour\\nfive\\nsix\\nseven\\neight\\nnine\\nten\\neleven\\n'; exit 7", capture, ExecuteOffloadOptions{MaxInlineBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Offloaded || result.Response.ExitCode == nil || *result.Response.ExitCode != 7 || !strings.Contains(result.Response.Output, "lines truncated") {
		t.Fatalf("offload = %#v", result)
	}
	full, err := os.ReadFile(capture)
	if err != nil || !strings.Contains(string(full), "eleven") {
		t.Fatalf("captured output = %q, %v", full, err)
	}

	inlinePath := filepath.Join(root, "inline")
	inline, err := sandbox.ExecuteWithOffload(context.Background(), "printf small", inlinePath, ExecuteOffloadOptions{MaxInlineBytes: 10})
	if err != nil || inline.Offloaded || inline.Response.Output != "small" {
		t.Fatalf("inline = %#v, %v", inline, err)
	}
	if _, err := os.Stat(inlinePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inline capture remained: %v", err)
	}

	cappedPath := filepath.Join(root, "capped")
	capped, err := sandbox.ExecuteWithOffload(context.Background(), "head -c 5000 /dev/zero | tr '\\000' x", cappedPath, ExecuteOffloadOptions{MaxInlineBytes: 8})
	if err != nil || !capped.Offloaded || !capped.Response.Truncated {
		t.Fatalf("capped = %#v, %v", capped, err)
	}
	if info, err := os.Stat(cappedPath); err != nil || info.Size() != 4096 {
		t.Fatalf("capped file = %#v, %v", info, err)
	}
}

func TestBaseSandboxCaptureIsOptInAndPreservesTimeoutOmission(t *testing.T) {
	sandbox, transport, root := newTestBaseSandbox(t, BaseSandboxOptions{})
	result, err := sandbox.ExecuteWithOffload(context.Background(), "printf plain", filepath.Join(root, "capture"), ExecuteOffloadOptions{MaxInlineBytes: 1})
	if err != nil || result.Offloaded || result.Response.Output != "plain" {
		t.Fatalf("disabled capture = %#v, %v", result, err)
	}
	if len(transport.options) != 1 || transport.options[0].Timeout != nil {
		t.Fatalf("execute options = %#v", transport.options)
	}
}

func TestBaseSandboxRejectsInvalidServerResponse(t *testing.T) {
	sandbox, transport, _ := newTestBaseSandbox(t, BaseSandboxOptions{})
	transport.malformed = true
	if _, err := sandbox.List(context.Background(), "/"); err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("malformed response error = %v", err)
	}
}

func TestBaseSandboxOperationsHonorCancellation(t *testing.T) {
	sandbox, _, _ := newTestBaseSandbox(t, BaseSandboxOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sandbox.List(ctx, "/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list = %v", err)
	}
}
