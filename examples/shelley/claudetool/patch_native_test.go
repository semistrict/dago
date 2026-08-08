package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dtool "github.com/semistrict/dago/tool"
)

func TestPatchNativeToolUsesDagoContract(t *testing.T) {
	root := t.TempDir()
	executable := (&PatchTool{WorkingDir: NewMutableWorkingDir(root)}).NativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{
		"path":"native.txt",
		"patches":[{"operation":"overwrite","newText":"native patch\n"}]
	}`), dtool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "<patches_applied>all</patches_applied>") {
		t.Fatalf("content = %#v", result.Content)
	}
	got, err := os.ReadFile(filepath.Join(root, "native.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "native patch\n" {
		t.Fatalf("file = %q", got)
	}
	var display PatchDisplayData
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display.Path != filepath.Join(root, "native.txt") || !strings.Contains(display.Diff, "+native patch") {
		t.Fatalf("display = %#v", display)
	}
}
