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

func TestDagoHarnessRemovesOverlappingShelleyExecutables(t *testing.T) {
	set := NewToolSet(context.Background(), ToolSetConfig{
		WorkingDir: t.TempDir(),
	})
	defer set.Cleanup()
	seen := map[string]bool{}
	for _, item := range set.NativeTools() {
		seen[item.Definition().Name] = true
	}
	for _, duplicate := range []string{"bash", "patch", "keyword_search"} {
		if seen[duplicate] {
			t.Fatalf("overlapping Shelley tool %q remains in native harness: %#v", duplicate, seen)
		}
	}
	definitions := map[string]bool{}
	for _, item := range set.Tools() {
		definitions[item.Name] = true
	}
	for _, name := range []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"} {
		if !definitions[name] {
			t.Fatalf("canonical tool %q missing from display definitions: %#v", name, definitions)
		}
	}
}

func TestDagoFilesystemSelectionMapsShelleySettings(t *testing.T) {
	set := NewToolSet(context.Background(), ToolSetConfig{
		WorkingDir:    t.TempDir(),
		ToolOverrides: map[string]string{"bash": "off", "patch": "off", "keyword_search": "off"},
	})
	if got := set.FilesystemTools(); len(got) != 2 || got[0] != "ls" || got[1] != "read_file" {
		t.Fatalf("filesystem tools = %#v", got)
	}

	disabled := NewToolSet(context.Background(), ToolSetConfig{
		WorkingDir: t.TempDir(), DisableAllTools: true,
	})
	if got := disabled.FilesystemTools(); got == nil || len(got) != 0 {
		t.Fatalf("disabled filesystem tools = %#v, want a non-nil empty selection", got)
	}
}
