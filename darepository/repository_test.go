package darepository

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

func TestInspectorIsReadOnlyBoundedAndFailClosed(t *testing.T) {
	backend := dabackend.NewMemory(map[string]dabackend.FileData{
		"/repo/a.txt": {Content: "one\ntwo\nthree\n", Encoding: dabackend.EncodingUTF8},
		"/repo/b.txt": {Content: "one", Encoding: dabackend.EncodingUTF8},
		"/repo/c.txt": {Content: "one", Encoding: dabackend.EncodingUTF8},
		"/secret.txt": {Content: "secret", Encoding: dabackend.EncodingUTF8},
	})
	inspector := New(backend, Options{Root: "/repo", MaxCalls: 2, ReadLineLimit: 1, GlobMatchLimit: 2})
	tools := map[string]datool.Tool{}
	for _, tool := range inspector.Tools() {
		tools[tool.Definition().Name] = tool
	}
	for _, forbidden := range []string{"write_file", "edit_file", "execute", "delete_file"} {
		if tools[forbidden] != nil {
			t.Fatalf("mutation tool %q was exposed", forbidden)
		}
	}

	ctx := inspector.Operation(t.Context())
	read, err := tools["read_file"].Execute(ctx, json.RawMessage(`{"path":"/repo/a.txt","limit":999}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if text := read.Content[0].Text; !strings.Contains(text, `"content":"one\n"`) || strings.Contains(text, "two") {
		t.Fatalf("read result was not line-bounded: %s", text)
	}
	escape, err := tools["read_file"].Execute(ctx, json.RawMessage(`{"path":"/repo/../secret.txt"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if escape.Status != damessage.ToolStatusError || !strings.Contains(escape.Content[0].Text, "traversal") {
		t.Fatalf("escape result = %#v", escape)
	}
	exhausted, err := tools["ls"].Execute(ctx, json.RawMessage(`{"path":"/repo"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Status != damessage.ToolStatusError || !strings.Contains(exhausted.Content[0].Text, "call limit") {
		t.Fatalf("budget result = %#v", exhausted)
	}
}

func TestInspectorCannotReadSymlinkOutsideRootedBackend(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	backend, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	inspector := New(backend, Options{})
	var read datool.Tool
	for _, tool := range inspector.Tools() {
		if tool.Definition().Name == "read_file" {
			read = tool
		}
	}
	result, err := read.Execute(inspector.Operation(t.Context()), json.RawMessage(`{"path":"/leak.txt"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != damessage.ToolStatusError || strings.Contains(result.Content[0].Text, "secret") {
		t.Fatalf("symlink escape = %#v", result)
	}
}

func TestInspectorCapsSearchAndRejectsOversizedFilesBeforeRead(t *testing.T) {
	backend := dabackend.NewMemory(map[string]dabackend.FileData{
		"/repo/a.txt":     {Content: "needle", Encoding: dabackend.EncodingUTF8},
		"/repo/b.txt":     {Content: "needle", Encoding: dabackend.EncodingUTF8},
		"/repo/c.txt":     {Content: "needle", Encoding: dabackend.EncodingUTF8},
		"/repo/large.txt": {Content: strings.Repeat("x", 32), Encoding: dabackend.EncodingUTF8},
	})
	inspector := New(backend, Options{Root: "/repo", ReadByteLimit: 16, GlobMatchLimit: 2, GrepMatchLimit: 2})
	tools := map[string]datool.Tool{}
	for _, tool := range inspector.Tools() {
		tools[tool.Definition().Name] = tool
	}
	ctx := inspector.Operation(t.Context())

	large, err := tools["read_file"].Execute(ctx, json.RawMessage(`{"path":"/repo/large.txt"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if large.Status != damessage.ToolStatusError || !strings.Contains(large.Content[0].Text, "file size") {
		t.Fatalf("large = %#v", large)
	}
	glob, err := tools["glob"].Execute(ctx, json.RawMessage(`{"path":"/repo","pattern":"*.txt"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(glob.Content[0].Text, `"path"`) != 2 || !strings.Contains(glob.Content[0].Text, `"truncated":true`) {
		t.Fatalf("glob = %s", glob.Content[0].Text)
	}
	grep, err := tools["grep"].Execute(ctx, json.RawMessage(`{"path":"/repo","pattern":"needle","max_count":999}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(grep.Content[0].Text, `"path"`) != 2 {
		t.Fatalf("grep = %s", grep.Content[0].Text)
	}
}
