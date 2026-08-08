package claudetool

import (
	"context"
	"encoding/json"
	"testing"

	dtool "github.com/semistrict/dago/tool"
)

func TestShellNativeToolUsesDagoContract(t *testing.T) {
	root := t.TempDir()
	executable := (&ShellTool{
		WorkingDir: NewMutableWorkingDir(root),
		TempDir:    t.TempDir(),
	}).NativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{"command":"printf native-shell"}`), dtool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "native-shell" {
		t.Fatalf("content = %#v", result.Content)
	}
	var display ShellDisplayData
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display.WorkingDir != root || display.PID == 0 || display.LogPath == "" || display.Yielded {
		t.Fatalf("display = %#v", display)
	}
}
