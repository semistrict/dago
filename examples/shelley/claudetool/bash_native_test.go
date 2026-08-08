package claudetool

import (
	"context"
	"encoding/json"
	"testing"

	dtool "github.com/semistrict/dago/tool"
)

func TestBashNativeToolUsesDagoContract(t *testing.T) {
	root := t.TempDir()
	executable := (&BashTool{WorkingDir: NewMutableWorkingDir(root)}).NativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{"command":"printf native"}`), dtool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "native" {
		t.Fatalf("content = %#v", result.Content)
	}
	var display BashDisplayData
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display.WorkingDir != root {
		t.Fatalf("display = %#v", display)
	}
}
