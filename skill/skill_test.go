package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseContentPreservesMetadataAndWarnsOnDirectoryMismatch(t *testing.T) {
	parsed, warnings, err := ParseContent(`---
name: research
description: Research carefully
license: MIT
compatibility: Requires network access
allowed-tools: read_file, grep
metadata:
  owner: platform
---

# Research
`, "/skills/different/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "research" || parsed.Metadata["owner"] != "platform" || strings.Join(parsed.AllowedTools, ",") != "read_file,grep" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if parsed.Body != "# Research" {
		t.Fatalf("body = %q", parsed.Body)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "must match directory") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestParseContentAcceptsWhitespaceDelimitersAndNormalizesWindowsPath(t *testing.T) {
	parsed, warnings, err := ParseContent("---   \nname: café\ndescription: useful\n--- \nbody\n", `C:\skills\café\SKILL.md`)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 || parsed.Path != "C:/skills/café/SKILL.md" || parsed.Body != "body" {
		t.Fatalf("parsed = %#v, warnings = %#v", parsed, warnings)
	}
}

func TestParseContentRejectsFalseClosingDelimiter(t *testing.T) {
	_, _, err := ParseContent("---\nname: sample\ndescription: useful\n---not-a-delimiter\nbody\n", "/skills/sample/SKILL.md")
	if err == nil {
		t.Fatal("expected malformed frontmatter to fail")
	}
}

func TestParseFileRejectsInvalidName(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(filePath, []byte("---\nname: Bad_Name\ndescription: invalid\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(filePath); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestLocalDiscoveryAndClaimedSuppression(t *testing.T) {
	root := t.TempDir()
	validDir := filepath.Join(root, "valid")
	if err := os.MkdirAll(validDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validDir, "SKILL.md"), []byte("---\nname: valid\ndescription: valid skill\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	disabledDir := filepath.Join(root, "disabled")
	if err := os.MkdirAll(disabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disabledDir, "SKILL.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	values := DiscoverDirectories([]string{root})
	if len(values) != 1 || values[0].Name != "valid" {
		t.Fatalf("skills = %#v", values)
	}
	claimed := ClaimedNames([]string{root})
	if !claimed["valid"] || !claimed["disabled"] {
		t.Fatalf("claimed = %#v", claimed)
	}
	tree, treeClaimed := DiscoverTree(context.Background(), root)
	if len(tree) != 1 || !treeClaimed["disabled"] {
		t.Fatalf("tree = %#v, claimed = %#v", tree, treeClaimed)
	}
}

func TestRenderXMLEscapesMetadataAndActivation(t *testing.T) {
	output := RenderXML([]Skill{{Name: "a&b", Description: "x<y"}}, func(Skill) string { return "read <file>" })
	for _, expected := range []string{"<name>a&amp;b</name>", "<description>x&lt;y</description>", "<activate>read &lt;file&gt;</activate>"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output %q does not contain %q", output, expected)
		}
	}
}
