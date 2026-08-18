package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/dago/datool"
)

func TestChangeDirNativeToolUsesNativeContract(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	workingDir := newMutableWorkingDir(root)
	executable := (&changeDirTool{WorkingDir: workingDir}).nativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{"path":"target"}`), datool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if workingDir.Get() != target || len(result.Content) != 1 || result.Content[0].Text == "" {
		t.Fatalf("working dir = %q, result = %#v", workingDir.Get(), result)
	}
}
