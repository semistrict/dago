package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/dago/datool"
)

func TestOutputIframeNativeToolReturnsDisplayArtifact(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "view.html"), []byte("<h1>hello</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	executable := (&outputIframeTool{WorkingDir: newMutableWorkingDir(root)}).nativeTool()
	result, err := executable.Execute(context.Background(), json.RawMessage(`{"path":"view.html","title":"View"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	var display OutputIframeDisplay
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display.Type != "output_iframe" || display.Title != "View" || display.HTML != "<h1>hello</h1>" {
		t.Fatalf("display = %#v", display)
	}
}
