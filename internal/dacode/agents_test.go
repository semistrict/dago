package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dastate"
)

func TestAgentDiscoveryIsSortedAndFailClosed(t *testing.T) {
	stateDir := t.TempDir()
	writeAgent := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(stateDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, name, agentInstructionsFilename), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("research", "Research carefully.")
	writeAgent("builder", "Build carefully.")
	writeAgent(".hidden", "Ignore me.")
	if err := os.Mkdir(filepath.Join(stateDir, "bare"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(stateDir, "research"), filepath.Join(stateDir, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := toggleDefaultAgent(t.Context(), stateDir, "research"); err != nil {
		t.Fatal(err)
	}

	agents, err := discoverAgents(t.Context(), stateDir, "builder")
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 || agents[0].Name != "builder" || agents[1].Name != defaultAgentName || agents[2].Name != "research" {
		t.Fatalf("agents = %#v", agents)
	}
	if !agents[0].Current || !agents[2].Default {
		t.Fatalf("agent markers = %#v", agents)
	}
}

func TestAgentDefaultToggleAndInstructionLoading(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(stateDir, "reviewer"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "reviewer", agentInstructionsFilename), []byte("  Review changes.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if instructions, err := loadAgentInstructions(t.Context(), stateDir, "reviewer"); err != nil || instructions != "Review changes." {
		t.Fatalf("instructions = %q, err = %v", instructions, err)
	}
	if selected, err := toggleDefaultAgent(t.Context(), stateDir, "reviewer"); err != nil || selected != "reviewer" {
		t.Fatalf("selected = %q, err = %v", selected, err)
	}
	if selected, err := toggleDefaultAgent(t.Context(), stateDir, "reviewer"); err != nil || selected != "" {
		t.Fatalf("cleared = %q, err = %v", selected, err)
	}
	if _, err := loadAgentInstructions(t.Context(), stateDir, "missing"); err == nil {
		t.Fatal("missing agent was accepted")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := discoverAgents(cancelled, stateDir, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery error = %v", err)
	}
}

func TestAgentConfigurationRejectsLinksOversizeAndUnsafeNames(t *testing.T) {
	stateDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-default")
	if err := os.WriteFile(outside, []byte("research\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defaultPath := filepath.Join(stateDir, defaultAgentFilename)
	if err := os.Symlink(outside, defaultPath); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredDefaultAgent(stateDir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlinked default error = %v", err)
	}
	if err := os.Remove(defaultPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultPath, []byte(strings.Repeat("x", maxDefaultAgentBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredDefaultAgent(stateDir); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized default error = %v", err)
	}
	if err := os.WriteFile(defaultPath, []byte(" padded \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredDefaultAgent(stateDir); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("padded default error = %v", err)
	}

	for _, name := range []string{"line\nbreak", string([]byte{'b', 'a', 'd', 0xff}), "../escape"} {
		if err := validateAgentName(name); err == nil {
			t.Errorf("unsafe agent name %q was accepted", name)
		}
	}
	for _, name := range []string{"line\nbreak"} {
		if err := os.Mkdir(filepath.Join(stateDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, name, agentInstructionsFilename), []byte("unsafe"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agents, err := discoverAgents(t.Context(), stateDir, defaultAgentName)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Name != defaultAgentName {
		t.Fatalf("unsafe names were discovered: %#v", agents)
	}
}

func TestAgentInstructionsRequireUTF8(t *testing.T) {
	stateDir := t.TempDir()
	agentDir := filepath.Join(stateDir, "invalid-content")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, agentInstructionsFilename), []byte{'o', 'k', 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentInstructions(t.Context(), stateDir, "invalid-content"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid instruction error = %v", err)
	}
}

func TestAgentDefaultCanChangeWithoutClearing(t *testing.T) {
	stateDir := t.TempDir()
	for _, name := range []string{"builder", "reviewer"} {
		if err := os.Mkdir(filepath.Join(stateDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, name, agentInstructionsFilename), []byte(name+" instructions"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if selected, err := toggleDefaultAgent(t.Context(), stateDir, "builder"); err != nil || selected != "builder" {
		t.Fatalf("first default = %q, err = %v", selected, err)
	}
	if selected, err := toggleDefaultAgent(t.Context(), stateDir, "reviewer"); err != nil || selected != "reviewer" {
		t.Fatalf("replacement default = %q, err = %v", selected, err)
	}
	if selected, err := configuredDefaultAgent(stateDir); err != nil || selected != "reviewer" {
		t.Fatalf("persisted default = %q, err = %v", selected, err)
	}
}

func TestRunnerStartsWithPersistedDefaultAgent(t *testing.T) {
	stateDir := t.TempDir()
	agentDir := filepath.Join(stateDir, "reviewer")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, agentInstructionsFilename), []byte("Review every change."), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := toggleDefaultAgent(t.Context(), stateDir, "reviewer"); err != nil {
		t.Fatal(err)
	}
	runner, closer, err := newRunner(runnerOptions{
		Authentication: modelAuthentication{apiKey: "test-key"},
		Model:          defaultModel,
		WorkingDir:     t.TempDir(),
		StateDir:       stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if runner.AgentName() != "reviewer" {
		t.Fatalf("startup agent = %q", runner.AgentName())
	}
	agents, err := runner.ListAgents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.Name == "reviewer" && !agent.Default {
			t.Fatalf("persisted default marker = %#v", agents)
		}
	}
}

func TestAgentIdentityMiddlewareTracksSwitches(t *testing.T) {
	identity := &agentIdentity{name: defaultAgentName}
	middleware := agentIdentityMiddleware(identity, t.TempDir())
	called := false
	_, err := middleware.WrapModelCall(t.Context(), dagent.ModelRequest{}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		called = true
		if request.SystemMessage != nil {
			t.Fatalf("default identity unexpectedly changed system message: %#v", request.SystemMessage)
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil || !called {
		t.Fatalf("default middleware called = %t, err = %v", called, err)
	}

	identity.set("reviewer")
	_, err = middleware.WrapModelCall(t.Context(), dagent.ModelRequest{}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		if request.SystemMessage == nil {
			t.Fatal("selected identity did not add its name")
		}
		text := request.SystemMessage.TextContent()
		if !strings.Contains(text, "reviewer") || strings.Contains(text, "Review every change.") {
			t.Fatalf("system message = %q", text)
		}
		return dagent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentIdentityMiddlewareEnforcesResetSessionNamespace(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := resetAgentProfile(t.Context(), stateDir, "reviewer", "", false); err != nil {
		t.Fatal(err)
	}
	identity := &agentIdentity{name: "reviewer"}
	middleware := agentIdentityMiddleware(identity, stateDir)
	updates, err := middleware.BeforeAgent(t.Context(), dastate.Values{}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	generation, ok := updates[sessionAgentGenerationKey].(string)
	if !ok || generation == "" {
		t.Fatalf("new session generation = %#v", updates[sessionAgentGenerationKey])
	}
	if _, err := middleware.BeforeAgent(t.Context(), dastate.Values{sessionAgentNameKey: "reviewer"}, dagent.Runtime{}); err == nil || !strings.Contains(err.Error(), "outside the current") {
		t.Fatalf("legacy session after reset error = %v", err)
	}
	if _, err := middleware.BeforeAgent(t.Context(), dastate.Values{
		sessionAgentNameKey: "reviewer", sessionAgentGenerationKey: generation,
	}, dagent.Runtime{}); err != nil {
		t.Fatalf("current session namespace rejected: %v", err)
	}
}

func TestTUIAgentSelectorNavigationDefaultAndSwitch(t *testing.T) {
	runner := &fakeRunner{agents: []agentInfo{
		{Name: defaultAgentName, Current: true},
		{Name: "research"},
	}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")
	model.resize(80, 24)
	command, handled := model.slashCommand("/agents")
	if !handled || command == nil {
		t.Fatal("/agents did not start discovery")
	}
	model.Update(command())
	if view := model.View(); !strings.Contains(view, "Select Agent") || !strings.Contains(view, "› dacode (current)") {
		t.Fatalf("agent selector =\n%s", view)
	}

	model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if view := model.View(); !strings.Contains(view, "› research") {
		t.Fatalf("tab did not select research:\n%s", view)
	}
	_, command = model.Update(testCtrlKey('s'))
	if command == nil {
		t.Fatal("ctrl+s did not save a default")
	}
	model.Update(command())
	if view := model.View(); !strings.Contains(view, "research (default)") || !strings.Contains(view, "Default set to research") {
		t.Fatalf("default marker =\n%s", view)
	}
	_, command = model.Update(testCtrlKey('s'))
	if command == nil {
		t.Fatal("repeated ctrl+s did not toggle the default")
	}
	model.Update(command())
	if view := model.View(); strings.Contains(view, "research (default)") || !strings.Contains(view, "Default cleared") {
		t.Fatalf("cleared default marker =\n%s", view)
	}

	_, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if command == nil || model.agentPicker != nil {
		t.Fatalf("switch command = %v, picker = %#v", command, model.agentPicker)
	}
	model.Update(command())
	if model.agentName != "research" || runner.agentName != "research" || model.threadID == "thread-1" {
		t.Fatalf("agent = %q, runner = %q, thread = %q", model.agentName, runner.agentName, model.threadID)
	}
	if len(model.items) != 1 || !strings.Contains(model.items[0].text, "Switched to agent research") {
		t.Fatalf("switch transcript = %#v", model.items)
	}
}

func TestTUIAgentSelectorCancelAndSwitchFailure(t *testing.T) {
	runner := &fakeRunner{agents: []agentInfo{{Name: defaultAgentName, Current: true}, {Name: "research"}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")
	model.agentPicker = &agentPickerState{agents: runner.agents}
	model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if model.agentPicker != nil {
		t.Fatal("escape did not cancel the selector")
	}

	runner.agentErr = errors.New("switch unavailable")
	model.agentPicker = &agentPickerState{agents: runner.agents, selected: 1}
	_, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if command == nil {
		t.Fatal("failed switch did not start")
	}
	model.Update(command())
	if model.agentName != defaultAgentName || model.threadID != "thread-1" || !strings.Contains(model.items[len(model.items)-1].text, "switch unavailable") {
		t.Fatalf("failed switch state = agent %q, thread %q, items %#v", model.agentName, model.threadID, model.items)
	}
}
