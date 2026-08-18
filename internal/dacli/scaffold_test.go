package dacli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScaffoldCreatesPinnedStarterLayout(t *testing.T) {
	parent := t.TempDir()
	project, err := Scaffold(parent, "my-agent", ScaffoldOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if project != filepath.Join(parent, "my-agent") {
		t.Fatalf("project = %q", project)
	}
	var agent map[string]any
	readJSON(t, filepath.Join(project, "agent.json"), &agent)
	if agent["name"] != "my-agent" || agent["model"] != "openai:gpt-5.5" {
		t.Fatalf("agent = %#v", agent)
	}
	if _, exists := agent["runtime"]; exists {
		t.Fatalf("unexpected runtime config: %#v", agent)
	}
	backend, ok := agent["backend"].(map[string]any)
	if !ok || backend["type"] != "state" {
		t.Fatalf("backend = %#v", agent["backend"])
	}
	extras, ok := agent["extras"].(map[string]any)
	deploymentID, _ := extras[deploymentIdentityField].(string)
	if !ok || len(deploymentID) != 32 {
		t.Fatalf("deployment identity = %#v", agent["extras"])
	}
	var tools struct {
		Tools []any `json:"tools"`
	}
	readJSON(t, filepath.Join(project, "tools.json"), &tools)
	if tools.Tools == nil || len(tools.Tools) != 0 {
		t.Fatalf("tools = %#v", tools.Tools)
	}
	for _, path := range []string{
		"AGENTS.md", ".gitignore", "skills/example-skill/SKILL.md",
		"subagents/researcher/agent.json", "subagents/researcher/AGENTS.md",
	} {
		info, err := os.Stat(filepath.Join(project, filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("starter file %s: info=%v err=%v", path, info, err)
		}
	}
	gitignore, err := os.ReadFile(filepath.Join(project, ".gitignore"))
	if err != nil || string(gitignore) != ".env\n" {
		t.Fatalf("gitignore = %q, %v", gitignore, err)
	}
	if _, err := os.Stat(filepath.Join(project, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env unexpectedly exists: %v", err)
	}
}

func TestScaffoldExistingTargetRequiresForceAndPreservesUnrelatedFiles(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "agent")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "agent.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(parent, "agent", ScaffoldOptions{}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v", err)
	}
	if _, err := Scaffold(parent, "agent", ScaffoldOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(project, "notes.txt"))
	if err != nil || string(notes) != "keep" {
		t.Fatalf("notes = %q, %v", notes, err)
	}
	var agent map[string]any
	readJSON(t, filepath.Join(project, "agent.json"), &agent)
	if agent["name"] != "agent" {
		t.Fatalf("agent = %#v", agent)
	}
}

func TestScaffoldRejectsUnsafeNamesAndLinkedManagedPaths(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"", ".", "..", "../escape", "a/b", `a\\b`, " a", "a\n"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Scaffold(parent, name, ScaffoldOptions{}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if runtime.GOOS == "windows" {
		return
	}
	project := filepath.Join(parent, "linked")
	outside := t.TempDir()
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, "skills")); err != nil {
		t.Fatal(err)
	}
	if _, err := Scaffold(parent, "linked", ScaffoldOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside entries = %v, %v", entries, err)
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}
