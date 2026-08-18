package dacode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/damessage"
)

func TestLoadFilesystemSubagentsUsesProfileAndProjectDefinitions(t *testing.T) {
	stateDirectory := t.TempDir()
	projectDirectory := t.TempDir()
	agentName := "reviewer"
	userDirectory := filepath.Join(stateDirectory, agentName, agentSubagentsDirectory)
	projectAgents := filepath.Join(projectDirectory, ".deepagents", agentSubagentsDirectory)
	writeFilesystemSubagentDefinition(t, userDirectory, "investigator", `---
description: Investigate the workspace
---
Inspect evidence carefully.
`)
	writeFilesystemSubagentDefinition(t, userDirectory, "general", `---
name: general-purpose
description: User general agent
---
User instructions.
`)
	writeFilesystemSubagentDefinition(t, projectAgents, "general", `---
name: general-purpose
description: Project general agent
model: test-model
---
Project instructions.
`)
	writeFilesystemSubagentDefinition(t, projectAgents, "reviewer", `---
description: Review changes
model: ''
---
Review intended behavior.
`)
	subagents, err := loadFilesystemSubagents(
		t.Context(), modelAuthentication{apiKey: "test-key"}, "", stateDirectory, projectDirectory, agentName,
		damessage.System("Main system."),
	)
	if err != nil {
		t.Fatal(err)
	}
	// The custom general-purpose definition replaces the built-in rather than
	// creating a second agent with the same name.
	if len(subagents) != 3 {
		t.Fatalf("subagent count = %d, want 3", len(subagents))
	}
}

func TestLoadFilesystemSubagentsAddsBuiltInAndValidatesModelOverrides(t *testing.T) {
	stateDirectory := t.TempDir()
	projectDirectory := t.TempDir()
	agentName := "default"
	projectAgents := filepath.Join(projectDirectory, ".deepagents", agentSubagentsDirectory)
	writeFilesystemSubagentDefinition(t, projectAgents, "investigator", `---
description: Investigate
---
Inspect.
`)
	subagents, err := loadFilesystemSubagents(
		t.Context(), modelAuthentication{apiKey: "test-key"}, "", stateDirectory, projectDirectory, agentName,
		damessage.System("Main system."),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(subagents) != 2 {
		t.Fatalf("subagent count = %d, want custom plus built-in", len(subagents))
	}

	writeFilesystemSubagentDefinition(t, projectAgents, "modeled", `---
description: Modeled
model: explicit-model
---
Use an explicit model.
`)
	_, err = loadFilesystemSubagents(
		t.Context(), modelAuthentication{}, "", stateDirectory, projectDirectory, agentName,
		damessage.System("Main system."),
	)
	if err == nil || !strings.Contains(err.Error(), `custom subagent "modeled" model`) {
		t.Fatalf("model override error = %v", err)
	}
}

func TestLoadFilesystemSubagentsRequiresStaticPaths(t *testing.T) {
	for _, invoke := range []func(){
		func() {
			_, _ = loadFilesystemSubagents(nil, modelAuthentication{}, "", "state", "project", "agent", damessage.System("system"))
		},
		func() {
			_, _ = loadFilesystemSubagents(t.Context(), modelAuthentication{}, "", "", "project", "agent", damessage.System("system"))
		},
		func() {
			_, _ = loadFilesystemSubagents(t.Context(), modelAuthentication{}, "", "state", "", "agent", damessage.System("system"))
		},
		func() {
			_, _ = loadFilesystemSubagents(t.Context(), modelAuthentication{}, "", "state", "project", "", damessage.System("system"))
		},
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

func TestNewRunnerPublishesFilesystemSubagentsFromConfigurationDirectory(t *testing.T) {
	stateDirectory := t.TempDir()
	configurationDirectory := t.TempDir()
	workingDirectory := t.TempDir()
	writeFilesystemSubagentDefinition(
		t,
		filepath.Join(configurationDirectory, ".deepagents", agentSubagentsDirectory),
		"project-researcher",
		"---\nname: researcher\ndescription: Research project evidence\n---\nInspect the project.\n",
	)
	runner, closer, err := newRunner(runnerOptions{
		Authentication:   modelAuthentication{apiKey: "test-key"},
		Model:            defaultModel,
		WorkingDir:       workingDirectory,
		ConfigurationDir: configurationDirectory,
		StateDir:         stateDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	compiled := runner.(*dagoRunner)
	var taskSchema string
	for _, tool := range compiled.agent.Tools() {
		definition := tool.Definition()
		if definition.Name == "task" {
			taskSchema = string(definition.InputSchema)
			break
		}
	}
	for _, expected := range []string{"researcher", "general-purpose"} {
		if !strings.Contains(taskSchema, expected) {
			t.Fatalf("task schema does not contain %q: %s", expected, taskSchema)
		}
	}
}

func writeFilesystemSubagentDefinition(t *testing.T, directory, name, content string) {
	t.Helper()
	folder := filepath.Join(directory, name)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "AGENTS.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
