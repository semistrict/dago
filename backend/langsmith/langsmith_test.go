package langsmith

import (
	"context"
	"errors"
	"strings"
	"testing"

	ls "github.com/langchain-ai/langsmith-go"
	"github.com/langchain-ai/langsmith-go/option"
	"github.com/semistrict/dago/backend"
)

type fakeSandbox struct {
	files    map[string][]byte
	commands []string
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
	if fake.run != nil {
		return fake.run(params.Command.Value)
	}
	return &ls.SandboxExecutionResult{}, nil
}

func TestBackendConformsAndUsesNativeFileTransfer(t *testing.T) {
	fake := &fakeSandbox{files: map[string][]byte{"/work/a.txt": []byte("one\r\ntwo\rthree\n")}}
	remote, err := newBackend("sandbox", fake, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var _ backend.Sandbox = remote
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
	remote, _ := newBackend("sandbox", fake, Options{})
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
	remote, _ := newBackend("sandbox", fake, Options{MaxOutput: 8})
	result, err := remote.Execute(context.Background(), "false", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "12345\ner" || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}
}

func TestRemotePathsRejectTraversalAndRootDelete(t *testing.T) {
	remote, _ := newBackend("sandbox", &fakeSandbox{}, Options{})
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
	fake := &fakeSandbox{files: map[string][]byte{"/binary": {0xff, 0x00}, "/large": []byte("large")}}
	remote, _ := newBackend("sandbox", fake, Options{MaxFileSize: 4})
	result, err := remote.Read(context.Background(), "/binary", 0, 10)
	if err != nil || result.Data.Encoding != backend.EncodingBase64 || result.Data.Content != "/wA=" {
		t.Fatalf("binary result = %#v, %v", result, err)
	}
	if _, err := remote.Read(context.Background(), "/large", 0, 10); err == nil {
		t.Fatal("large read should fail")
	}
}

func TestShellQuoteCannotInject(t *testing.T) {
	quoted := shellQuote("/tmp/a'; touch /bad; echo '")
	if quoted != `'/tmp/a'"'"'; touch /bad; echo '"'"''` {
		t.Fatalf("quoted = %q", quoted)
	}
}
