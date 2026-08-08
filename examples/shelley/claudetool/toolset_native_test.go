package claudetool

import (
	"context"
	"testing"
)

func TestToolSetPublishesOnlyEnabledNativeTools(t *testing.T) {
	set := NewToolSet(context.Background(), ToolSetConfig{
		WorkingDir: t.TempDir(), ToolOverrides: map[string]string{"change_dir": "off"},
	})
	defer set.Cleanup()
	seen := map[string]bool{}
	for _, item := range set.NativeTools() {
		seen[item.Definition().Name] = true
	}
	if seen["change_dir"] || !seen["output_iframe"] {
		t.Fatalf("native tools = %#v", seen)
	}
}
