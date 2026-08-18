package damanaged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadProjectBuildsPinnedPayloadAndDirectory(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "agent.json", `{"name":"agent","description":"help","model":"openai:gpt-5.5","backend":{"type":"state"}}`)
	writeProjectFile(t, root, "AGENTS.md", "main instructions\n")
	writeProjectFile(t, root, "tools.json", `{"tools":[],"interrupt_config":{}}`)
	writeProjectFile(t, root, "skills/review/SKILL.md", "---\nname: review\ndescription: Review changes.\n---\n\nReview carefully.\n")
	writeProjectFile(t, root, "skills/review/example.txt", "example")
	writeProjectFile(t, root, "subagents/researcher/agent.json", `{"description":"research","model":"openai:gpt-5.5"}`)
	writeProjectFile(t, root, "subagents/researcher/AGENTS.md", "research instructions\n")
	project, err := LoadProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "agent" || len(project.Skills) != 1 || len(project.Subagents) != 1 {
		t.Fatalf("project = %#v", project)
	}
	payload := project.CreatePayload()
	if payload["system_prompt"] != "main instructions\n" || payload["runtime"] == nil || payload["skills"] == nil || payload["subagents"] == nil {
		t.Fatalf("payload = %#v", payload)
	}
	metadata := project.MetadataPayload()
	if metadata["system_prompt"] != nil || metadata["skills"] != nil {
		t.Fatalf("metadata = %#v", metadata)
	}
	files := project.DirectoryFiles()
	if files["skills/review/example.txt"] != "example" || !strings.Contains(files["subagents/researcher/AGENTS.md"], "model_id: \"openai:gpt-5.5\"") {
		t.Fatalf("files = %#v", files)
	}
}

func TestLoadProjectNormalizesSandboxAndRejectsModelRuntimeConflict(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "agent.json", `{"name":"agent","backend":{"type":"agent_scoped_sandbox","sandbox":{"policy_ids":["p1"]}}}`)
	writeProjectFile(t, root, "AGENTS.md", "instructions")
	project, err := LoadProject(root)
	if err != nil {
		t.Fatal(err)
	}
	configuration, ok := project.Backend["sandbox_config"].(map[string]any)
	if project.Backend["type"] != "sandbox" || !ok || configuration["scope"] != "agent" {
		t.Fatalf("backend = %#v", project.Backend)
	}
	writeProjectFile(t, root, "agent.json", `{"name":"agent","model":"m","runtime":{"model":{}}}`)
	if _, err := LoadProject(root); err == nil || !strings.Contains(err.Error(), "either model or runtime") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadProjectSandboxCompatibilityValidation(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "AGENTS.md", "instructions")
	writeProjectFile(t, root, "agent.json", `{
		"name":"agent",
		"backend":{
			"type":"thread_scoped_sandbox",
			"sandbox":{"policy_ids":["old"],"idle_ttl_seconds":900},
			"sandbox_config":{"policy_ids":["new"],"delete_after_stop_seconds":30}
		}
	}`)
	project, err := LoadProject(root)
	if err != nil {
		t.Fatal(err)
	}
	configuration := project.Backend["sandbox_config"].(map[string]any)
	if configuration["scope"] != "thread" || configuration["idle_ttl_seconds"].(json.Number) != "900" || configuration["delete_after_stop_seconds"].(json.Number) != "30" {
		t.Fatalf("sandbox_config = %#v", configuration)
	}
	policies := configuration["policy_ids"].([]any)
	if len(policies) != 1 || policies[0] != "new" {
		t.Fatalf("policy precedence = %#v", policies)
	}

	for _, test := range []struct {
		name string
		json string
		want string
	}{
		{name: "legacy runtime field", json: `{"name":"agent","runtime":{"backend_type":"sandbox"}}`, want: "runtime.backend_type"},
		{name: "boolean ttl", json: `{"name":"agent","backend":{"type":"sandbox","sandbox":{"idle_ttl_seconds":true}}}`, want: "idle_ttl_seconds"},
		{name: "fractional ttl", json: `{"name":"agent","backend":{"type":"sandbox","sandbox":{"delete_after_stop_seconds":1.5}}}`, want: "delete_after_stop_seconds"},
		{name: "nonstring type", json: `{"name":"agent","backend":{"type":true}}`, want: "backend.type"},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeProjectFile(t, root, "agent.json", test.json)
			if _, err := LoadProject(root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadProject() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadProjectRejectsUnknownConfigLinkedAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "agent.json", `{"name":"agent","unknown":true}`)
	writeProjectFile(t, root, "AGENTS.md", "instructions")
	if _, err := LoadProject(root); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
	writeProjectFile(t, root, "agent.json", `{"name":"agent"}`)
	writeProjectFile(t, root, "AGENTS.md", strings.Repeat("x", maxProjectFileBytes+1))
	if _, err := LoadProject(root); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("error = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	writeProjectFile(t, root, "AGENTS.md", "instructions")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "tools.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProject(root); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("error = %v", err)
	}
}

func writeProjectFile(t *testing.T, root, relative, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
