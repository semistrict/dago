package langsmith

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ls "github.com/langchain-ai/langsmith-go"
	"github.com/langchain-ai/langsmith-go/option"
	"github.com/semistrict/dago/dabackend"
)

type fakeSandbox struct {
	files    map[string][]byte
	commands []string
	runs     []ls.SandboxBoxRunParams
	run      func(string) (*ls.SandboxExecutionResult, error)
}

func (fake *fakeSandbox) ReadFile(_ context.Context, path string, _ ...option.RequestOption) ([]byte, error) {
	value, ok := fake.files[path]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), value...), nil
}

func (fake *fakeSandbox) WriteFile(_ context.Context, path string, content []byte, _ ...option.RequestOption) error {
	if fake.files == nil {
		fake.files = map[string][]byte{}
	}
	fake.files[path] = append([]byte(nil), content...)
	return nil
}

func (fake *fakeSandbox) Run(_ context.Context, params ls.SandboxBoxRunParams, _ ...option.RequestOption) (*ls.SandboxExecutionResult, error) {
	fake.commands = append(fake.commands, params.Command.Value)
	fake.runs = append(fake.runs, params)
	if fake.run != nil {
		return fake.run(params.Command.Value)
	}
	return &ls.SandboxExecutionResult{}, nil
}

func TestBackendConformsAndUsesNativeFileTransfer(t *testing.T) {
	fake := &fakeSandbox{files: map[string][]byte{"/work/a.txt": []byte("one\r\ntwo\rthree\n")}}
	remote := newBackend("sandbox", fake, Options{})
	var _ dabackend.Sandbox = remote
	read, err := remote.Read(context.Background(), "/work/a.txt", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if read.Data.Content != "two" || read.NextOffset == nil || *read.NextOffset != 2 {
		t.Fatalf("read = %#v", read)
	}
	edit, err := remote.Edit(context.Background(), "/work/a.txt", "two", "second", false)
	if err != nil || edit.Occurrences != 1 || !strings.Contains(string(fake.files["/work/a.txt"]), "second") {
		t.Fatalf("edit = %#v, %v, content = %q", edit, err, fake.files["/work/a.txt"])
	}
	_, err = remote.Write(context.Background(), "/new dir/quote's.txt", "content")
	if err != nil {
		t.Fatal(err)
	}
	if fake.commands[0] != "mkdir -p -- '/new dir'" {
		t.Fatalf("mkdir command = %q", fake.commands[0])
	}
	if string(fake.files["/new dir/quote's.txt"]) != "content" {
		t.Fatal("native write did not preserve content")
	}
}

func TestListParsesNULTerminatedMetadata(t *testing.T) {
	fake := &fakeSandbox{run: func(command string) (*ls.SandboxExecutionResult, error) {
		if !strings.HasPrefix(command, "find '/work'") {
			t.Fatalf("command = %q", command)
		}
		return &ls.SandboxExecutionResult{Stdout: "f\t12\t10.500000000\t/work/z.txt\x00d\t0\t9.000000000\t/work/a\x00"}, nil
	}}
	remote := newBackend("sandbox", fake, Options{})
	result, err := remote.List(context.Background(), "/work")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 2 || result.Entries[0].Path != "/work/a/" || !result.Entries[0].IsDir || result.Entries[1].Size != 12 {
		t.Fatalf("entries = %#v", result.Entries)
	}
}

func TestExecuteCombinesOutputAndBoundsIt(t *testing.T) {
	fake := &fakeSandbox{run: func(command string) (*ls.SandboxExecutionResult, error) {
		return &ls.SandboxExecutionResult{Stdout: "12345", Stderr: "error", ExitCode: 7}, nil
	}}
	remote := newBackend("sandbox", fake, Options{MaxOutput: 8})
	result, err := remote.Execute(context.Background(), "false", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "12345\ner" || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}
}

func TestExecutePreservesOmittedAndZeroTimeouts(t *testing.T) {
	fake := &fakeSandbox{}
	remote := newBackend("sandbox", fake, Options{})
	if _, err := remote.ExecuteWithOptions(context.Background(), "true", dabackend.ExecuteOptions{}); err != nil {
		t.Fatal(err)
	}
	zero := time.Duration(0)
	if _, err := remote.ExecuteWithOptions(context.Background(), "true", dabackend.ExecuteOptions{Timeout: &zero}); err != nil {
		t.Fatal(err)
	}
	partial := 1500 * time.Millisecond
	if _, err := remote.ExecuteWithOptions(context.Background(), "true", dabackend.ExecuteOptions{Timeout: &partial}); err != nil {
		t.Fatal(err)
	}
	if len(fake.runs) != 3 || fake.runs[0].Timeout.Present {
		t.Fatalf("omitted timeout = %#v", fake.runs)
	}
	if !fake.runs[1].Timeout.Present || fake.runs[1].Timeout.Value != 0 {
		t.Fatalf("zero timeout = %#v", fake.runs[1].Timeout)
	}
	if !fake.runs[2].Timeout.Present || fake.runs[2].Timeout.Value != 2 {
		t.Fatalf("rounded timeout = %#v", fake.runs[2].Timeout)
	}
}

func TestRemotePathsRejectTraversalAndRootDelete(t *testing.T) {
	remote := newBackend("sandbox", &fakeSandbox{}, Options{})
	if _, err := remote.Read(context.Background(), "../secret", 0, 1); err == nil {
		t.Fatal("relative traversal should fail")
	}
	if _, err := remote.Read(context.Background(), "/work/../secret", 0, 1); err == nil {
		t.Fatal("absolute traversal should fail")
	}
	if _, err := remote.Delete(context.Background(), "/"); err == nil {
		t.Fatal("root delete should fail")
	}
}

func TestBinaryReadAndSizeLimit(t *testing.T) {
	fake := &fakeSandbox{files: map[string][]byte{
		"/binary":    {0xff, 0x00},
		"/large":     []byte("large"),
		"/ascii.mkv": []byte("plain text bytes"),
	}}
	remote := newBackend("sandbox", fake, Options{MaxFileSize: 4})
	result, err := remote.Read(context.Background(), "/binary", 0, 10)
	if err != nil || result.Data.Encoding != dabackend.EncodingBase64 || result.Data.Content != "/wA=" {
		t.Fatalf("binary result = %#v, %v", result, err)
	}
	if _, err := remote.Read(context.Background(), "/large", 0, 10); err == nil {
		t.Fatal("large read should fail")
	}
	remote = newBackend("sandbox", fake, Options{})
	result, err = remote.Read(context.Background(), "/ascii.mkv", 99, 0)
	if err != nil || result.Data.Encoding != dabackend.EncodingBase64 || result.NoLinesRequested {
		t.Fatalf("extension-routed binary result = %#v, %v", result, err)
	}
}

func TestReadBoundsBinaryPreviewsAndTextPages(t *testing.T) {
	fake := &fakeSandbox{files: map[string][]byte{
		"/exact.png": bytes.Repeat([]byte{0}, dabackend.MaxSandboxBinaryPreviewBytes),
		"/over.png":  bytes.Repeat([]byte{0}, dabackend.MaxSandboxBinaryPreviewBytes+1),
		"/big.txt":   []byte(strings.Repeat("x", 100_000) + "\n" + strings.Repeat("y", 100_000) + "\n" + strings.Repeat("z", 400_000) + "\ntail"),
	}}
	remote := newBackend("sandbox", fake, Options{})
	exact, err := remote.Read(context.Background(), "/exact.png", 0, 1)
	if err != nil || exact.Data == nil || exact.Data.Encoding != dabackend.EncodingBase64 {
		t.Fatalf("exact binary read = %#v, %v", exact, err)
	}
	if _, err := remote.Read(context.Background(), "/over.png", 0, 1); err == nil || !strings.Contains(err.Error(), "maximum preview size") {
		t.Fatalf("oversized binary error = %v", err)
	}
	page, err := remote.Read(context.Background(), "/big.txt", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if page.Data == nil || !strings.HasSuffix(page.Data.Content, dabackend.SandboxReadTruncationMessage) || len(page.Data.Content) > dabackend.MaxSandboxReadOutputBytes {
		t.Fatalf("bounded text page = %#v", page)
	}
	if page.NextOffset == nil || page.EndLine == nil || *page.NextOffset != *page.EndLine || *page.NextOffset >= 4 {
		t.Fatalf("bounded text pagination = %#v", page)
	}
}

func TestReadNormalizesRemotePaginationWithoutTrailingNewline(t *testing.T) {
	fake := &fakeSandbox{files: map[string][]byte{"/lines.txt": []byte("one\r\ntwo\rthree\n")}}
	remote := newBackend("sandbox", fake, Options{})
	page, err := remote.Read(context.Background(), "/lines.txt", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Data == nil || page.Data.Content != "two" || page.StartLine == nil || *page.StartLine != 2 || page.NextOffset == nil || *page.NextOffset != 2 {
		t.Fatalf("remote page = %#v", page)
	}
}

func TestReadAndEditUseSharedTextSemantics(t *testing.T) {
	fake := &fakeSandbox{files: map[string][]byte{
		"/blank.txt": []byte(" \r\n\t"),
		"/eof.txt":   []byte("one\r\ntwo"),
	}}
	remote := newBackend("sandbox", fake, Options{})

	blank, err := remote.Read(context.Background(), "/blank.txt", 100, 0)
	if err != nil || blank.Data.Content != "" || !blank.NoLinesRequested {
		t.Fatalf("blank read = %#v, %v", blank, err)
	}
	if _, err := remote.Read(context.Background(), "/eof.txt", 2, 1); err == nil {
		t.Fatal("out-of-range read succeeded")
	}
	if _, err := remote.Edit(context.Background(), "/eof.txt", "two\n", "second\n", false); err == nil || !strings.Contains(err.Error(), "trailing newline removed") {
		t.Fatalf("EOF edit error = %v", err)
	}
	edited, err := remote.Edit(context.Background(), "/eof.txt", "one\ntwo", "done", false)
	if err != nil || edited.Occurrences != 1 || string(fake.files["/eof.txt"]) != "done" {
		t.Fatalf("normalized edit = %#v, %v, content = %q", edited, err, fake.files["/eof.txt"])
	}
}

func TestShellQuoteCannotInject(t *testing.T) {
	quoted := shellQuote("/tmp/a'; touch /bad; echo '")
	if quoted != `'/tmp/a'"'"'; touch /bad; echo '"'"''` {
		t.Fatalf("quoted = %q", quoted)
	}
}
