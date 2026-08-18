package dacli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/damanaged"
)

type fakeDeployer struct {
	mu           sync.Mutex
	endpoint     string
	created      int
	patched      []string
	committed    []map[string]*damanaged.DirectoryFile
	patchErr     error
	directory    map[string]any
	commit       map[string]any
	createAgent  damanaged.Agent
	createAgents []damanaged.Agent
	patchAgent   damanaged.Agent
	agents       []damanaged.Agent
	getAgents    map[string]damanaged.Agent
	createErr    error
	createErrors []error
	createBody   map[string]any
	createKey    string
	createKeys   []string
	patchBody    map[string]any
}

func deployStateJSONPath(t *testing.T, stateRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			return filepath.Join(stateRoot, entry.Name())
		}
	}
	t.Fatalf("no deploy state JSON in %v", entries)
	return ""
}

func testDeployProject(root string) damanaged.Project {
	return damanaged.Project{
		Root: root, Name: "agent", SystemPrompt: "instructions",
		Extras: map[string]any{deploymentIdentityField: "test-deployment"},
	}
}

func (fake *fakeDeployer) Endpoint() string { return fake.endpoint }
func (fake *fakeDeployer) ListAgents(context.Context, string) ([]damanaged.Agent, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.agents, nil
}
func (fake *fakeDeployer) GetAgent(_ context.Context, id string, _ bool) (damanaged.Agent, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	agent := fake.getAgents[id]
	if agent == nil {
		return nil, &damanaged.APIError{Status: 404}
	}
	return agent, nil
}
func (fake *fakeDeployer) CreateAgent(_ context.Context, payload map[string]any, key string) (damanaged.Agent, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	createIndex := fake.created
	fake.created++
	fake.createBody = payload
	fake.createKey = key
	fake.createKeys = append(fake.createKeys, key)
	createdTemplate := fake.createAgent
	if createIndex < len(fake.createAgents) {
		createdTemplate = fake.createAgents[createIndex]
	}
	createErr := fake.createErr
	if createIndex < len(fake.createErrors) {
		createErr = fake.createErrors[createIndex]
	}
	if createErr != nil || createdTemplate == nil {
		return createdTemplate, createErr
	}
	created := make(damanaged.Agent, len(createdTemplate)+1)
	for name, value := range createdTemplate {
		created[name] = value
	}
	if _, exists := created["extras"]; !exists {
		created["extras"] = payload["extras"]
	}
	id, _ := created["id"].(string)
	if id != "" {
		if fake.getAgents == nil {
			fake.getAgents = map[string]damanaged.Agent{}
		}
		fake.getAgents[id] = created
		found := false
		for _, listed := range fake.agents {
			if listedID, _ := listed["id"].(string); listedID == id {
				found = true
				break
			}
		}
		if !found {
			fake.agents = append(fake.agents, damanaged.Agent{"id": id})
		}
	}
	return created, nil
}

func TestDeployRecoversUncertainCreateWithoutIssuingAnotherMutation(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	uncertain := &fakeDeployer{
		endpoint:  "https://api.example.test",
		createErr: errors.New("response lost"),
	}
	if _, err := Deploy(context.Background(), uncertain, project, stateRoot, DeployOptions{}); err == nil {
		t.Fatal("expected uncertain create failure")
	}
	if uncertain.created != 1 {
		t.Fatalf("create calls = %d", uncertain.created)
	}
	extras, _ := uncertain.createBody["extras"].(map[string]any)
	key, _ := extras[deploymentKeyField].(string)
	if len(key) != 32 {
		t.Fatalf("deployment key = %q", key)
	}

	recovery := &fakeDeployer{
		endpoint:   "https://api.example.test",
		agents:     []damanaged.Agent{{"id": "a1"}},
		patchAgent: damanaged.Agent{"id": "a1", "name": "agent", "revision": "r2"},
		getAgents: map[string]damanaged.Agent{
			"a1": {"id": "a1", "name": "agent", "revision": "r1", "extras": map[string]any{deploymentKeyField: key}},
		},
	}
	project.Name = "renamed-agent"
	result, err := Deploy(context.Background(), recovery, project, stateRoot, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentID != "a1" || !result.Created || recovery.created != 0 || len(recovery.patched) != 1 || len(recovery.committed) != 1 {
		t.Fatalf("result=%#v create calls=%d", result, recovery.created)
	}
	patchExtras, _ := recovery.patchBody["extras"].(map[string]any)
	if patchExtras[deploymentKeyField] != key {
		t.Fatalf("recovery patch extras = %#v", patchExtras)
	}
}

func TestDeployPendingCreateReplaysTheSameIdempotentMutation(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	first := &fakeDeployer{endpoint: "https://api.example.test", createErr: errors.New("response lost")}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{}); err == nil {
		t.Fatal("expected uncertain create failure")
	}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{Reset: true}); err == nil || !strings.Contains(err.Error(), "new extras.dago_deployment_id") {
		t.Fatalf("pending reset error = %v", err)
	}
	rotated := project
	rotated.Extras = map[string]any{deploymentIdentityField: "rotated-deployment"}
	reset := &fakeDeployer{
		endpoint: "https://api.example.test",
		createAgents: []damanaged.Agent{
			{"id": "settled-old-agent"},
			{"id": "reset-agent"},
		},
	}
	resetResult, err := Deploy(context.Background(), reset, rotated, stateRoot, DeployOptions{Reset: true})
	if err != nil || resetResult.AgentID != "reset-agent" || reset.created != 2 {
		t.Fatalf("reset result=%#v created=%d error=%v", resetResult, reset.created, err)
	}
	if len(reset.createKeys) != 2 || reset.createKeys[0] != first.createKey || reset.createKeys[1] == reset.createKeys[0] {
		t.Fatalf("reset replay keys = %v; old key = %q", reset.createKeys, first.createKey)
	}

	stateRoot = t.TempDir()
	first = &fakeDeployer{endpoint: "https://api.example.test", createErr: errors.New("response lost")}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{}); err == nil {
		t.Fatal("expected second uncertain create failure")
	}
	second := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1"}}
	result, err := Deploy(context.Background(), second, project, stateRoot, DeployOptions{})
	if err != nil || result.AgentID != "a1" || !result.Created {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if second.created != 1 || second.createKey != first.createKey {
		t.Fatalf("create calls=%d first key=%q replay key=%q", second.created, first.createKey, second.createKey)
	}
}

func TestDeployRotatedResetKeepsPendingStateWhenOriginalReplayRemainsUncertain(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	first := &fakeDeployer{endpoint: "https://api.example.test", createErr: errors.New("response lost")}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{}); err == nil {
		t.Fatal("expected initial uncertain create")
	}
	rotated := project
	rotated.Extras = map[string]any{deploymentIdentityField: "rotated-deployment"}
	stillUncertain := &fakeDeployer{endpoint: "https://api.example.test", createErr: errors.New("still uncertain")}
	_, err := Deploy(context.Background(), stillUncertain, rotated, stateRoot, DeployOptions{Reset: true})
	if err == nil || !strings.Contains(err.Error(), "remains uncertain") {
		t.Fatalf("error = %v", err)
	}
	if stillUncertain.created != 1 || stillUncertain.createKey != first.createKey {
		t.Fatalf("created=%d key=%q old=%q", stillUncertain.created, stillUncertain.createKey, first.createKey)
	}

	replay := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "old-agent"}}
	result, err := Deploy(context.Background(), replay, project, stateRoot, DeployOptions{})
	if err != nil || result.AgentID != "old-agent" || replay.createKey != first.createKey {
		t.Fatalf("result=%#v key=%q error=%v", result, replay.createKey, err)
	}
}

func TestDeployRotatedResetPersistsReplacementBeforeItsUncertainCreate(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	first := &fakeDeployer{endpoint: "https://api.example.test", createErr: errors.New("response lost")}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{}); err == nil {
		t.Fatal("expected initial uncertain create")
	}
	rotated := project
	rotated.Extras = map[string]any{deploymentIdentityField: "rotated-deployment"}
	reset := &fakeDeployer{
		endpoint: "https://api.example.test",
		createAgents: []damanaged.Agent{
			{"id": "settled-old-agent"},
			nil,
		},
		createErrors: []error{nil, errors.New("replacement response lost")},
	}
	if _, err := Deploy(context.Background(), reset, rotated, stateRoot, DeployOptions{Reset: true}); err == nil {
		t.Fatal("expected uncertain replacement create")
	}
	if len(reset.createKeys) != 2 || reset.createKeys[0] == reset.createKeys[1] {
		t.Fatalf("create keys = %v", reset.createKeys)
	}
	data, err := os.ReadFile(deployStateJSONPath(t, stateRoot))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["create_key"] != reset.createKeys[1] || stored["project_identity"] != "rotated-deployment" {
		t.Fatalf("stored state = %#v", stored)
	}

	replay := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "replacement"}}
	result, err := Deploy(context.Background(), replay, rotated, stateRoot, DeployOptions{})
	if err != nil || result.AgentID != "replacement" || replay.createKey != reset.createKeys[1] {
		t.Fatalf("result=%#v key=%q error=%v", result, replay.createKey, err)
	}
}

func TestDeploySerializesConcurrentCreationForOneProject(t *testing.T) {
	project := testDeployProject(t.TempDir())
	fake := &fakeDeployer{
		endpoint:    "https://api.example.test",
		createAgent: damanaged.Agent{"id": "a1", "name": "agent", "revision": "r1"},
		patchAgent:  damanaged.Agent{"id": "a1", "name": "agent", "revision": "r1"},
		directory: map[string]any{"files": map[string]any{
			"AGENTS.md": map[string]any{"type": "file", "content": "instructions"},
		}},
	}
	stateRoot := t.TempDir()
	start := make(chan struct{})
	errorsSeen := make(chan error, 8)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := Deploy(context.Background(), fake, project, stateRoot, DeployOptions{})
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	created := fake.created
	fake.mu.Unlock()
	if created != 1 {
		t.Fatalf("create calls = %d, want 1", created)
	}
}

func TestDeploySynchronizesCompleteDirectoryOnInitialCreation(t *testing.T) {
	project := damanaged.Project{
		Root: t.TempDir(), Name: "agent", SystemPrompt: "instructions",
		Extras: map[string]any{deploymentIdentityField: "test-deployment"},
		Subagents: []damanaged.ProjectSubagent{{
			Name: "researcher", Instructions: "research", ExtraFiles: map[string]string{"notes.txt": "context"},
		}},
	}
	fake := &fakeDeployer{
		endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1", "name": "agent"},
		commit: map[string]any{"commit_hash": "c1"},
	}
	result, err := Deploy(context.Background(), fake, project, t.TempDir(), DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != "c1" || len(fake.committed) != 1 || fake.committed[0]["subagents/researcher/notes.txt"] == nil {
		t.Fatalf("result=%#v commits=%#v", result, fake.committed)
	}
}

func TestProjectDeploymentKeyIsStableAcrossLocalStateRoots(t *testing.T) {
	project := testDeployProject("/one/checkout")
	first, err := projectDeploymentKey(project, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	project.Root = "/another/machine/checkout"
	second, err := projectDeploymentKey(project, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 32 {
		t.Fatalf("keys = %q %q", first, second)
	}
	project.Extras = map[string]any{deploymentIdentityField: "other-agent"}
	third, err := projectDeploymentKey(project, "https://api.example.test")
	if err != nil || third == first {
		t.Fatalf("explicit identity key = %q err=%v", third, err)
	}
}

func (fake *fakeDeployer) PatchAgent(_ context.Context, id string, payload map[string]any) (damanaged.Agent, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.patched = append(fake.patched, id)
	fake.patchBody = payload
	return fake.patchAgent, fake.patchErr
}
func (fake *fakeDeployer) GetAgentDirectory(context.Context, string) (map[string]any, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.directory, nil
}
func (fake *fakeDeployer) CommitAgentDirectory(_ context.Context, _ string, files map[string]*damanaged.DirectoryFile, _ string) (map[string]any, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.committed = append(fake.committed, files)
	return fake.commit, nil
}

func TestDeployCreatesThenUsesDurableStateForMetadataPatch(t *testing.T) {
	projectRoot := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "deployments")
	project := testDeployProject(projectRoot)
	create := &fakeDeployer{
		endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1", "name": "agent", "revision": "r1"},
	}
	result, err := Deploy(context.Background(), create, project, stateRoot, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.AgentID != "a1" || create.created != 1 {
		t.Fatalf("result=%#v fake=%#v", result, create)
	}
	_ = deployStateJSONPath(t, stateRoot)
	key, err := projectDeploymentKey(project, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}

	update := &fakeDeployer{
		endpoint: "https://api.example.test", patchAgent: damanaged.Agent{"name": "agent", "revision": "agent-r2"},
		getAgents: map[string]damanaged.Agent{
			"a1": {"id": "a1", "extras": map[string]any{deploymentKeyField: key}},
		},
		directory: map[string]any{"commit_hash": "c1", "files": map[string]any{
			"AGENTS.md":           map[string]any{"type": "file", "content": "old"},
			"skills/old/SKILL.md": map[string]any{"type": "file", "content": "old"},
		}},
		commit: map[string]any{"commit_hash": "c2"},
	}
	result, err = Deploy(context.Background(), update, project, stateRoot, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.Revision != "c2" || len(update.patched) != 1 || update.patched[0] != "a1" || len(update.committed) != 1 {
		t.Fatalf("result=%#v fake=%#v", result, update)
	}
	delta := update.committed[0]
	if delta["AGENTS.md"] == nil || delta["AGENTS.md"].Content != "instructions" {
		t.Fatalf("delta = %#v", delta)
	}
	if value, exists := delta["skills/old/SKILL.md"]; !exists || value != nil {
		t.Fatalf("managed deletion = %#v", delta)
	}
}

func TestDeployStaleCachedAgentRecreatesButDeclaredIDFailsClosed(t *testing.T) {
	projectRoot := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "deployments")
	project := testDeployProject(projectRoot)
	seed := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "old"}}
	if _, err := Deploy(context.Background(), seed, project, stateRoot, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	stale := &fakeDeployer{
		endpoint: "https://api.example.test", patchErr: &damanaged.APIError{Status: 404},
		createAgent: damanaged.Agent{"id": "new"},
	}
	result, err := Deploy(context.Background(), stale, project, stateRoot, DeployOptions{})
	if err != nil || !result.Created || result.AgentID != "new" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	project.AgentID = "declared"
	declared := &fakeDeployer{endpoint: "https://api.example.test", patchErr: &damanaged.APIError{Status: 404}}
	if _, err := Deploy(context.Background(), declared, project, stateRoot, DeployOptions{}); err == nil {
		t.Fatal("expected declared target failure")
	}
}

func TestDeployRejectsDeclaredAgentThatConflictsWithDurableBinding(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	seed := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1"}}
	if _, err := Deploy(context.Background(), seed, project, stateRoot, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	project.AgentID = "a2"
	conflict := &fakeDeployer{endpoint: "https://api.example.test", patchAgent: damanaged.Agent{"id": "a2"}}
	if _, err := Deploy(context.Background(), conflict, project, stateRoot, DeployOptions{}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v", err)
	}
	if len(conflict.patched) != 0 {
		t.Fatalf("patched = %v", conflict.patched)
	}
}

func TestDeployRejectsRemoteTargetThatLostItsDurableBinding(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	seed := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1"}}
	if _, err := Deploy(context.Background(), seed, project, stateRoot, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, extras := range []map[string]any{
		{},
		{deploymentKeyField: "0123456789abcdef0123456789abcdef"},
	} {
		fake := &fakeDeployer{
			endpoint: "https://api.example.test",
			getAgents: map[string]damanaged.Agent{
				"a1": {"id": "a1", "extras": extras},
			},
			patchAgent: damanaged.Agent{"id": "a1"},
		}
		_, err := Deploy(context.Background(), fake, project, stateRoot, DeployOptions{})
		if err == nil || !strings.Contains(err.Error(), "expected remote identity binding") {
			t.Fatalf("extras=%#v error=%v", extras, err)
		}
		if len(fake.patched) != 0 || len(fake.committed) != 0 {
			t.Fatalf("extras=%#v patched=%v committed=%v", extras, fake.patched, fake.committed)
		}
	}
}

func TestDeployValidatesDeclaredAgentRemoteBindingsBeforePatch(t *testing.T) {
	project := testDeployProject(t.TempDir())
	project.AgentID = "declared"
	key, err := projectDeploymentKey(project, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		agents    []damanaged.Agent
		getAgents map[string]damanaged.Agent
		want      string
	}{
		{
			name:   "same identity belongs to another agent",
			agents: []damanaged.Agent{{"id": "other"}},
			getAgents: map[string]damanaged.Agent{
				"other": {"id": "other", "extras": map[string]any{deploymentKeyField: key}},
			},
			want: "remote deployment binding",
		},
		{
			name:   "declared agent belongs to another identity",
			agents: []damanaged.Agent{{"id": "declared"}},
			getAgents: map[string]damanaged.Agent{
				"declared": {"id": "declared", "extras": map[string]any{deploymentKeyField: "0123456789abcdef0123456789abcdef"}},
			},
			want: "current remote deployment identity binding",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeDeployer{
				endpoint: "https://api.example.test", agents: test.agents, getAgents: test.getAgents,
				patchAgent: damanaged.Agent{"id": "declared"},
			}
			_, err := Deploy(context.Background(), fake, project, t.TempDir(), DeployOptions{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
			if len(fake.patched) != 0 {
				t.Fatalf("patched = %v", fake.patched)
			}
		})
	}
}

func TestDeployRefusesNonAtomicAdoptionOfDeclaredUnboundAgent(t *testing.T) {
	project := testDeployProject(t.TempDir())
	project.AgentID = "declared"
	fake := &fakeDeployer{
		endpoint: "https://api.example.test",
		getAgents: map[string]damanaged.Agent{
			"declared": {"id": "declared", "extras": map[string]any{}},
		},
		patchAgent: damanaged.Agent{"id": "declared", "name": "agent"},
	}
	result, err := Deploy(context.Background(), fake, project, t.TempDir(), DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "already carry") || len(fake.patched) != 0 {
		t.Fatalf("result=%#v patched=%v err=%v", result, fake.patched, err)
	}
}

func TestDeployResetAndDryRun(t *testing.T) {
	project := testDeployProject(t.TempDir())
	project.AgentID = "declared"
	fake := &fakeDeployer{endpoint: "https://api.example.test"}
	if _, err := Deploy(context.Background(), fake, project, t.TempDir(), DeployOptions{Reset: true}); err == nil {
		t.Fatal("expected reset conflict")
	}
	payload := DryRunPayload(project)
	directory, ok := payload["directory_files"].(map[string]any)
	if !ok || directory["AGENTS.md"] == nil {
		t.Fatalf("dry run = %#v", payload)
	}
}

func TestDeployResetRequiresRotatedProjectOwnedIdentity(t *testing.T) {
	project := testDeployProject(t.TempDir())
	stateRoot := t.TempDir()
	first := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1"}}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{Reset: true}); err == nil || !strings.Contains(err.Error(), "new extras.dago_deployment_id") {
		t.Fatalf("reset error = %v", err)
	}
	project.Extras[deploymentIdentityField] = "rotated-deployment"
	if _, err := Deploy(context.Background(), first, project, stateRoot, DeployOptions{}); err == nil || !strings.Contains(err.Error(), "use --reset") {
		t.Fatalf("identity drift error = %v", err)
	}
	reset := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a2"}}
	result, err := Deploy(context.Background(), reset, project, stateRoot, DeployOptions{Reset: true})
	if err != nil || result.AgentID != "a2" || reset.created != 1 {
		t.Fatalf("result=%#v created=%d err=%v", result, reset.created, err)
	}
}

func TestDeployResetWithoutLocalStateRejectsExistingRemoteIdentity(t *testing.T) {
	project := testDeployProject(t.TempDir())
	key, err := projectDeploymentKey(project, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDeployer{
		endpoint: "https://api.example.test",
		agents:   []damanaged.Agent{{"id": "existing"}},
		getAgents: map[string]damanaged.Agent{
			"existing": {"id": "existing", "extras": map[string]any{deploymentKeyField: key}},
		},
		createAgent: damanaged.Agent{"id": "new"},
	}
	_, err = Deploy(context.Background(), fake, project, t.TempDir(), DeployOptions{Reset: true})
	if err == nil || !strings.Contains(err.Error(), "already bound remotely") {
		t.Fatalf("error = %v", err)
	}
	if fake.created != 0 || len(fake.patched) != 0 {
		t.Fatalf("created=%d patched=%v", fake.created, fake.patched)
	}
}

func TestDeployRejectsCorruptOrLinkedState(t *testing.T) {
	stateRoot := t.TempDir()
	project := testDeployProject(t.TempDir())
	fake := &fakeDeployer{endpoint: "https://api.example.test", createAgent: damanaged.Agent{"id": "a1"}}
	if _, err := Deploy(context.Background(), fake, project, stateRoot, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	statePath := deployStateJSONPath(t, stateRoot)
	if err := os.WriteFile(statePath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Deploy(context.Background(), fake, project, stateRoot, DeployOptions{})
	if err == nil {
		t.Fatal("expected corrupt-state error")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}
