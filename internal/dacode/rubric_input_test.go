package dacode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRubricTextLiteralAndBoundedFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "criteria.md")
	if err := os.WriteFile(path, []byte("  - tests pass\n- no unrelated changes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "  tests pass  ", want: "tests pass"},
		{value: "@criteria.md", want: "- tests pass\n- no unrelated changes"},
	} {
		got, err := resolveRubricText(test.value, directory)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("resolveRubricText(%q) = %q", test.value, got)
		}
	}
}

func TestRubricFileReferenceAcceptsLiteralAndCompletedPaths(t *testing.T) {
	for _, value := range []string{"criteria.md", " @criteria.md "} {
		if got := rubricFileReference(value); got != "@criteria.md" {
			t.Fatalf("rubricFileReference(%q) = %q", value, got)
		}
	}
}

func TestResolveRubricTextRejectsUnsafeOrInvalidInputs(t *testing.T) {
	directory := t.TempDir()
	directoryPath := filepath.Join(directory, "not-a-file")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(directory, "invalid.md")
	if err := os.WriteFile(invalid, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(directory, "large.md")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", int(maxRubricFileBytes)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "@", "@missing.md", "@not-a-file", "@invalid.md", "@large.md"} {
		if _, err := resolveRubricText(value, directory); err == nil {
			t.Fatalf("resolveRubricText(%q) succeeded", value)
		}
	}
}
