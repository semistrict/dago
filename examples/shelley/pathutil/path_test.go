package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogicalPreservesAliasedTree(t *testing.T) {
	realRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRoot, "repo", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(filepath.Join(realRoot, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(aliasRoot, "repo", "nested")
	if got, want := Logical(physical, reference), filepath.Join(aliasRoot, "repo"); got != want {
		t.Fatalf("Logical() = %q, want %q", got, want)
	}
}

func TestLogicalLeavesUnrelatedPathAlone(t *testing.T) {
	path := t.TempDir()
	if got := Logical(path, t.TempDir()); got != path {
		t.Fatalf("Logical() = %q, want %q", got, path)
	}
}
