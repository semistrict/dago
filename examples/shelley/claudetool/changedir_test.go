package claudetool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/dago/datool"
)

func TestChangeDirTool(t *testing.T) {
	// Create a temp directory structure
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "subdir")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	wd := NewMutableWorkingDir(tmpDir)
	tool := &ChangeDirTool{WorkingDir: wd}

	t.Run("change to absolute path", func(t *testing.T) {
		// Reset
		wd.Set(tmpDir)

		input, _ := json.Marshal(changeDirInput{Path: subDir})
		_, err := tool.NativeTool().Execute(context.Background(), input, datool.Runtime{})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if wd.Get() != subDir {
			t.Errorf("expected working dir %q, got %q", subDir, wd.Get())
		}
	})

	t.Run("change to relative path", func(t *testing.T) {
		// Reset
		wd.Set(tmpDir)

		input, _ := json.Marshal(changeDirInput{Path: "subdir"})
		_, err := tool.NativeTool().Execute(context.Background(), input, datool.Runtime{})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if wd.Get() != subDir {
			t.Errorf("expected working dir %q, got %q", subDir, wd.Get())
		}
	})

	t.Run("change to parent directory", func(t *testing.T) {
		wd.Set(subDir)

		input, _ := json.Marshal(changeDirInput{Path: ".."})
		_, err := tool.NativeTool().Execute(context.Background(), input, datool.Runtime{})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if wd.Get() != tmpDir {
			t.Errorf("expected working dir %q, got %q", tmpDir, wd.Get())
		}
	})

	t.Run("error on non-existent path", func(t *testing.T) {
		wd.Set(tmpDir)

		input, _ := json.Marshal(changeDirInput{Path: "/nonexistent/path"})
		_, err := tool.NativeTool().Execute(context.Background(), input, datool.Runtime{})

		if err == nil {
			t.Fatal("expected error for non-existent path")
		}
	})

	t.Run("error on file path", func(t *testing.T) {
		// Create a file
		filePath := filepath.Join(tmpDir, "file.txt")
		if err := os.WriteFile(filePath, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}

		wd.Set(tmpDir)

		input, _ := json.Marshal(changeDirInput{Path: filePath})
		_, err := tool.NativeTool().Execute(context.Background(), input, datool.Runtime{})

		if err == nil {
			t.Fatal("expected error for file path")
		}
	})

	t.Run("OnChange callback is called", func(t *testing.T) {
		wd.Set(tmpDir)

		var callbackDir string
		toolWithCallback := &ChangeDirTool{
			WorkingDir: wd,
			OnChange: func(newDir string) {
				callbackDir = newDir
			},
		}

		input, _ := json.Marshal(changeDirInput{Path: subDir})
		_, err := toolWithCallback.NativeTool().Execute(context.Background(), input, datool.Runtime{})

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if callbackDir != subDir {
			t.Errorf("expected callback dir %q, got %q", subDir, callbackDir)
		}
	})
}

func TestChangeDirTool_Method(t *testing.T) {
	wd := NewMutableWorkingDir("/test")
	tool := &ChangeDirTool{WorkingDir: wd}
	nativeTool := tool.NativeTool()

	if nativeTool == nil {
		t.Fatal("NativeTool() returned nil")
	}
	definition := nativeTool.Definition()

	if definition.Name != changeDirName {
		t.Errorf("expected name %q, got %q", changeDirName, definition.Name)
	}

	if definition.Description != changeDirDescription {
		t.Errorf("expected description %q, got %q", changeDirDescription, definition.Description)
	}

	if err := definition.Validate(); err != nil {
		t.Fatalf("invalid definition: %v", err)
	}
}
