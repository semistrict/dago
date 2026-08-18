package dasubagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverUsesFallbackAndProjectOverridesUserDeterministically(t *testing.T) {
	user := filepath.Join(t.TempDir(), "agents")
	project := filepath.Join(t.TempDir(), "agents")
	writeDefinition(t, user, "researcher", `---
description: User research
model: openai:user-model
---
User instructions.
`)
	writeDefinition(t, user, "writer-folder", `---
name: writer
description: Write drafts
model: null
tools: [read_file]
---
Writer instructions.
`)
	writeDefinition(t, project, "researcher", `---
description: Project research
model: openai:project-model
---
Project instructions.
`)
	report, err := Discover(t.Context(), user, project, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Definitions) != 2 || report.Definitions[0].Name != "researcher" || report.Definitions[1].Name != "writer" {
		t.Fatalf("definitions = %#v", report.Definitions)
	}
	researcher := report.Definitions[0]
	if researcher.Source != ProjectSource || researcher.Description != "Project research" || researcher.Model != "openai:project-model" || researcher.SystemPrompt != "Project instructions." {
		t.Fatalf("researcher = %#v", researcher)
	}
	if report.Definitions[1].Source != UserSource || report.Definitions[1].Path != filepath.Join(user, "writer-folder", "AGENTS.md") {
		t.Fatalf("writer = %#v", report.Definitions[1])
	}
}

func TestDiscoverSkipsInvalidDefinitionsWithSecretFreeDiagnostics(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "stray.md"), []byte("private-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeDefinition(t, directory, "missing-description", "---\nname: missing\n---\nprivate-value\n")
	writeDefinition(t, directory, "bad-model", "---\ndescription: Bad\nmodel: [invalid]\n---\nprivate-value\n")
	writeDefinition(t, directory, "duplicate-one", "---\nname: duplicate\ndescription: First\n---\nFirst\n")
	writeDefinition(t, directory, "duplicate-two", "---\nname: duplicate\ndescription: Second\n---\nSecond\n")
	report, err := Discover(t.Context(), directory, "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Definitions) != 1 || report.Definitions[0].Description != "Second" || len(report.Diagnostics) != 4 {
		t.Fatalf("report = %#v", report)
	}
	for _, diagnostic := range report.Diagnostics {
		if strings.Contains(diagnostic.Reason, "private-value") {
			t.Fatalf("diagnostic leaked file content: %#v", diagnostic)
		}
	}
}

func TestDiscoverTreatsEmptyModelAsInheritance(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agents")
	writeDefinition(t, directory, "empty-model", "---\ndescription: Inherit\nmodel: ''\n---\nInherited model.\n")
	report, err := Discover(t.Context(), directory, "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Definitions) != 1 || report.Definitions[0].Model != "" {
		t.Fatalf("definitions = %#v", report.Definitions)
	}
}

func TestDiscoverRejectsLinksSpecialFilesAndBounds(t *testing.T) {
	t.Run("linked root", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "agents")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "agents")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := Discover(t.Context(), link, "", Options{}); err == nil {
			t.Fatal("linked root accepted")
		}
	})
	t.Run("linked definition", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "agents")
		folder := filepath.Join(directory, "linked")
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "AGENTS.md")
		if err := os.WriteFile(target, []byte("---\ndescription: Secret\n---\nsecret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(folder, "AGENTS.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		report, err := Discover(t.Context(), directory, "", Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Definitions) != 0 || len(report.Diagnostics) != 1 {
			t.Fatalf("report = %#v", report)
		}
	})
	t.Run("file size", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "agents")
		writeDefinition(t, directory, "large", "---\ndescription: Large\n---\n"+strings.Repeat("x", 64))
		report, err := Discover(t.Context(), directory, "", Options{MaxFileBytes: 32})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Definitions) != 0 || len(report.Diagnostics) != 1 {
			t.Fatalf("report = %#v", report)
		}
	})
	t.Run("definition count", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "agents")
		writeDefinition(t, directory, "one", "---\ndescription: One\n---\n")
		writeDefinition(t, directory, "two", "---\ndescription: Two\n---\n")
		if _, err := Discover(t.Context(), directory, "", Options{MaxDefinitions: 1}); err == nil {
			t.Fatal("definition count limit was not enforced")
		}
	})
}

func TestDiscoverCancellationAndStaticInputs(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agents")
	writeDefinition(t, directory, "one", "---\ndescription: One\n---\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, directory, "", Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	for _, invoke := range []func(){
		func() { _, _ = Discover(nil, "", "", Options{}) },
		func() { _, _ = Discover(t.Context(), "relative", "", Options{}) },
		func() { _, _ = Discover(t.Context(), "", "relative", Options{}) },
		func() { _, _ = Discover(t.Context(), "", "", Options{MaxDefinitions: -1}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected static input panic")
				}
			}()
			invoke()
		}()
	}
}

func writeDefinition(t *testing.T, directory, name, content string) {
	t.Helper()
	folder := filepath.Join(directory, name)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "AGENTS.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
