package browse

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	dtool "github.com/semistrict/dago/tool"
)

func TestNativeReadImageToolUsesDagoMultimodalContract(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "image.png")
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	browser := NewBrowseTools(context.Background(), 0)
	defer browser.Close()
	arguments, _ := json.Marshal(readImageInput{Path: path})
	result, err := browser.NativeReadImageTool().Execute(context.Background(), arguments, dtool.Runtime{CallID: "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 2 || result.Content[0].Text == "" || result.Content[1].MIMEType != "image/png" || len(result.Content[1].Data) == 0 {
		t.Fatalf("content = %#v", result.Content)
	}
	var display map[string]any
	if err := json.Unmarshal(result.Artifact, &display); err != nil {
		t.Fatal(err)
	}
	if display["type"] != "read_image" || display["path"] != path {
		t.Fatalf("display = %#v", display)
	}
}

func TestNativeCombinedBrowserToolDispatchesWithoutLegacyToolRun(t *testing.T) {
	browser := NewBrowseTools(context.Background(), 0)
	defer browser.Close()
	result, err := browser.NativeCombinedTool().Execute(
		context.Background(), json.RawMessage(`{"action":"emulate_help"}`), dtool.Runtime{CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text == "" {
		t.Fatalf("content = %#v", result.Content)
	}
}
