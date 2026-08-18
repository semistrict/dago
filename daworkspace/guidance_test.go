package daworkspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverGuidancePreservesPrecedenceAndScopedFiles(t *testing.T) {
	root := t.TempDir()
	userFile := filepath.Join(t.TempDir(), "AGENTS.md")
	writeTestFile(t, userFile, "user instructions")
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "root instructions")
	writeTestFile(t, filepath.Join(root, "CLAUDE.md"), "root instructions")
	writeTestFile(t, filepath.Join(root, "README.md"), "not immediate guidance")
	writeTestFile(t, filepath.Join(root, "pkg", "AGENTS.md"), "package instructions")
	writeTestFile(t, filepath.Join(root, ".hidden", "AGENTS.md"), "hidden instructions")

	guidance := DiscoverGuidance(t.Context(), GuidanceOptions{
		Root: root, WorkingDirectory: root, UserFiles: []string{userFile}, TrustWorkspace: true,
	})
	if len(guidance.Root) != 2 || guidance.Root[0].Path != userFile || guidance.Root[1].Content != "root instructions" {
		t.Fatalf("root guidance = %#v", guidance.Root)
	}
	if len(guidance.Subdirectories) != 1 || guidance.Subdirectories[0] != filepath.Join(root, "pkg", "AGENTS.md") {
		t.Fatalf("subdirectory guidance = %#v", guidance.Subdirectories)
	}
}

func TestDiscoverGuidanceRejectsNegativeTimeout(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative discovery timeout did not panic")
		}
	}()
	DiscoverGuidance(t.Context(), GuidanceOptions{Timeout: -time.Second})
}

func TestDiscoverGuidanceCanExcludeWorkspace(t *testing.T) {
	root := t.TempDir()
	userFile := filepath.Join(t.TempDir(), "AGENTS.md")
	writeTestFile(t, userFile, "user instructions")
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "workspace instructions")

	guidance := DiscoverGuidance(context.Background(), GuidanceOptions{
		Root: root, WorkingDirectory: root, UserFiles: []string{userFile}, TrustWorkspace: false,
	})
	if len(guidance.Root) != 1 || guidance.Root[0].Path != userFile || len(guidance.Subdirectories) != 0 {
		t.Fatalf("guidance = %#v", guidance)
	}
}

func TestDiscoverGuidanceHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "workspace instructions")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	guidance := DiscoverGuidance(ctx, GuidanceOptions{
		Root: root, WorkingDirectory: root, TrustWorkspace: true,
	})
	if len(guidance.Root) != 0 || len(guidance.Subdirectories) != 0 {
		t.Fatalf("cancelled discovery returned %#v", guidance)
	}
}

func TestDiscoverGuidanceInjectsRootThenWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "packages", "app")
	writeTestFile(t, filepath.Join(root, "AGENTS.md"), "root instructions")
	writeTestFile(t, filepath.Join(workingDirectory, "AGENT.md"), "app instructions")

	guidance := DiscoverGuidance(t.Context(), GuidanceOptions{
		Root: root, WorkingDirectory: workingDirectory, TrustWorkspace: true,
	})
	if len(guidance.Root) != 2 || guidance.Root[0].Content != "root instructions" || guidance.Root[1].Content != "app instructions" {
		t.Fatalf("root guidance = %#v", guidance.Root)
	}
}

func TestExistingDirectoriesAndSummary(t *testing.T) {
	directory := t.TempDir()
	paths := ExistingDirectories(filepath.Join(directory, "missing"), directory)
	if len(paths) != 1 || paths[0] != directory {
		t.Fatalf("directories = %#v", paths)
	}
	summary := FormatSubdirectoryGuidance([]string{"one", "two", "three"}, 2)
	if !strings.Contains(summary, "one\ntwo\n") || !strings.Contains(summary, "...and 1 more") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestGuidanceJSONContract(t *testing.T) {
	encoded, err := json.Marshal(Guidance{
		Root:           []GuidanceFile{{Path: "/work/AGENTS.md", Content: "instructions"}},
		Subdirectories: []string{"/work/pkg/AGENTS.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"root":[{"path":"/work/AGENTS.md","content":"instructions"}],"subdirectories":["/work/pkg/AGENTS.md"]}`
	if string(encoded) != want {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
