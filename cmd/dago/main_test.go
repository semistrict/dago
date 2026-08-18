package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/damanaged"
	"github.com/semistrict/dago/internal/dacli"
)

type fakeManagedAgents struct {
	agents      []damanaged.Agent
	agent       damanaged.Agent
	deleted     []string
	includeFile bool
}

type fakeManagedDeployment struct {
	fakeManagedAgents
	created int
	patched []string
	health  any
}

func (fake *fakeManagedDeployment) Endpoint() string { return "https://api.example.test" }
func (fake *fakeManagedDeployment) CreateAgent(context.Context, map[string]any, string) (damanaged.Agent, error) {
	fake.created++
	return damanaged.Agent{"id": "a1", "name": "agent", "revision": "r1"}, nil
}
func (fake *fakeManagedDeployment) PatchAgent(_ context.Context, id string, _ map[string]any) (damanaged.Agent, error) {
	fake.patched = append(fake.patched, id)
	return damanaged.Agent{"id": id, "name": "agent", "revision": "r2"}, nil
}
func (fake *fakeManagedDeployment) GetAgentDirectory(context.Context, string) (map[string]any, error) {
	return map[string]any{"files": map[string]any{"AGENTS.md": map[string]any{"type": "file", "content": "instructions"}}}, nil
}
func (fake *fakeManagedDeployment) CommitAgentDirectory(context.Context, string, map[string]*damanaged.DirectoryFile, string) (map[string]any, error) {
	return map[string]any{"commit_hash": "c1"}, nil
}
func (fake *fakeManagedDeployment) GetAgentHealth(context.Context, string) (any, error) {
	return fake.health, nil
}

func (fake *fakeManagedAgents) ListAgents(context.Context, string) ([]damanaged.Agent, error) {
	return fake.agents, nil
}

func (fake *fakeManagedAgents) GetAgent(_ context.Context, _ string, includeFiles bool) (damanaged.Agent, error) {
	fake.includeFile = includeFiles
	return fake.agent, nil
}

func (fake *fakeManagedAgents) DeleteAgent(_ context.Context, agentID string) error {
	fake.deleted = append(fake.deleted, agentID)
	return nil
}

func TestRunInitScaffoldsNamedProject(t *testing.T) {
	workingDirectory := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	var stdout, stderr bytes.Buffer
	if err := runWithIO([]string{"init", "my-agent"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var agent map[string]any
	data, err := os.ReadFile(filepath.Join(workingDirectory, "my-agent", "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &agent); err != nil {
		t.Fatal(err)
	}
	if agent["name"] != "my-agent" || !strings.Contains(stdout.String(), "Next steps") || stderr.Len() != 0 {
		t.Fatalf("agent=%#v stdout=%q stderr=%q", agent, stdout.String(), stderr.String())
	}
}

func TestRunInitPromptsAndForceIsExplicit(t *testing.T) {
	workingDirectory := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	var stdout bytes.Buffer
	if err := runWithIO([]string{"init"}, strings.NewReader("prompted\n"), &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "Project name: ") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if err := runWithIO([]string{"init", "prompted"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v", err)
	}
	if err := runWithIO([]string{"init", "--force", "prompted"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	err := runWithIO([]string{"unknown"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v", err)
	}
}

func TestBareRunRedirectsInteractiveUsersToDacode(t *testing.T) {
	err := runWithIO(nil, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "moved to dacode") || !strings.Contains(err.Error(), "run `dacode`") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunAgentsListAndGet(t *testing.T) {
	fake := &fakeManagedAgents{
		agents: []damanaged.Agent{{"id": "a1", "name": "alpha", "updated_at": "now"}},
		agent:  damanaged.Agent{"id": "a1", "name": "alpha"},
	}
	var output bytes.Buffer
	if err := runAgentsWithClient(context.Background(), fake, []string{"list"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "a1\talpha\tnow\n" {
		t.Fatalf("list = %q", output.String())
	}
	output.Reset()
	if err := runAgentsWithClient(context.Background(), fake, []string{"get", "--include-files", "a1"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !fake.includeFile || !strings.Contains(output.String(), `"id": "a1"`) {
		t.Fatalf("include=%v get=%q", fake.includeFile, output.String())
	}
}

func TestRunAgentsDeleteRequiresConfirmation(t *testing.T) {
	fake := &fakeManagedAgents{}
	var output bytes.Buffer
	if err := runAgentsWithClient(context.Background(), fake, []string{"delete", "a1"}, strings.NewReader("no\n"), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 || !strings.Contains(output.String(), "Aborted") {
		t.Fatalf("deleted=%v output=%q", fake.deleted, output.String())
	}
	output.Reset()
	if err := runAgentsWithClient(context.Background(), fake, []string{"delete", "--yes", "a1"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "a1" || output.String() != "Deleted a1\n" {
		t.Fatalf("deleted=%v output=%q", fake.deleted, output.String())
	}
}

func TestManagedClientEnvironmentFailsClosed(t *testing.T) {
	t.Setenv("LANGSMITH_API_KEY", "")
	t.Setenv("LANGCHAIN_API_KEY", "")
	if _, err := managedClientFromEnvironment(); err == nil {
		t.Fatal("expected missing-key error")
	}
	t.Setenv("LANGSMITH_API_KEY", "secret")
	t.Setenv("LANGSMITH_ENDPOINT", "http://insecure.example")
	if _, err := managedClientFromEnvironment(); err == nil {
		t.Fatal("expected insecure-endpoint error")
	}
	t.Setenv("LANGSMITH_ENDPOINT", "https://api.example.test")
	if _, err := managedClientFromEnvironment(); err != nil {
		t.Fatal(err)
	}
}

func TestRunDeployDryRunNeedsNoCredentials(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "agent.json"), []byte(`{"name":"agent","model":"openai:gpt-5.5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANGSMITH_API_KEY", "")
	t.Setenv("LANGCHAIN_API_KEY", "")
	var output bytes.Buffer
	if err := runWithIO([]string{"deploy", "--dir", root, "--dry-run"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"agent_payload"`) || !strings.Contains(output.String(), `"directory_files"`) {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunDeployCreatesReportsHealthAndConfirmsDeclaredTarget(t *testing.T) {
	project := damanaged.Project{
		Root: t.TempDir(), Name: "agent", SystemPrompt: "instructions",
		Extras: map[string]any{"dago_deployment_id": "test-deployment"},
	}
	fake := &fakeManagedDeployment{health: map[string]any{"status": "ready"}}
	var output bytes.Buffer
	if err := runDeployWithClient(context.Background(), fake, project, t.TempDir(), dacli.DeployOptions{}, false, false, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if fake.created != 1 || !strings.Contains(output.String(), "agent_id: a1") || !strings.Contains(output.String(), `"status":"ready"`) {
		t.Fatalf("created=%d output=%q", fake.created, output.String())
	}

	project.AgentID = "declared"
	fake.agent = damanaged.Agent{"id": "declared", "name": "remote"}
	if err := runDeployWithClient(context.Background(), fake, project, t.TempDir(), dacli.DeployOptions{}, false, true, strings.NewReader("no\n"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.patched) != 0 {
		t.Fatalf("patched = %v", fake.patched)
	}
}
