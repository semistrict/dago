package dacode

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dahook"
)

type pluginTUIRunner struct {
	*fakeRunner
	*pluginUIControllerStub
}

func newPluginTUIRunner() *pluginTUIRunner {
	return &pluginTUIRunner{
		fakeRunner: &fakeRunner{},
		pluginUIControllerStub: &pluginUIControllerStub{snapshot: pluginManagerSnapshot{
			Available:    []pluginManagerPlugin{{ID: "new@local", Name: "New", Marketplace: "local"}},
			Installed:    []pluginManagerPlugin{{ID: "active@local", Name: "Active", Marketplace: "local", Enabled: true, Loaded: true}},
			Marketplaces: []pluginManagerMarketplace{{Name: "local", Source: "directory", PluginCount: 2, InstalledCount: 1}},
		}},
	}
}

func TestTUIPluginManagerMutationAndReloadPromptFlow(t *testing.T) {
	runner := newPluginTUIRunner()
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.resize(100, 30)
	command, handled := model.slashCommand("/plugins")
	if !handled || command == nil || model.pluginManager == nil {
		t.Fatalf("open plugins = handled:%v command:%v state:%#v", handled, command != nil, model.pluginManager)
	}
	model.Update(command())
	if view := ansi.Strip(model.View()); !strings.Contains(view, "Plugins") || !strings.Contains(view, "New @ local") {
		t.Fatalf("plugin manager view = %q", view)
	}
	command, handled = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatalf("install key = handled:%v command:%v", handled, command != nil)
	}
	_, refresh := model.Update(command())
	if refresh == nil {
		t.Fatal("successful mutation did not refresh the manager")
	}
	model.Update(refresh())
	if !model.pluginManager.dirty || !strings.Contains(model.pluginManager.status, "Reload pending") {
		t.Fatalf("post-install state = %#v", model.pluginManager)
	}
	model.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if model.pluginManager != nil || !model.pluginReloadPrompt {
		t.Fatalf("close manager = manager:%#v prompt:%v", model.pluginManager, model.pluginReloadPrompt)
	}
	if view := ansi.Strip(model.View()); !strings.Contains(view, "Reload plugins?") {
		t.Fatalf("reload prompt = %q", view)
	}
	model.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if model.pluginReloadPrompt {
		t.Fatal("reload-later choice did not close prompt")
	}
}

func TestTUIReloadCommandIsQueuedWhileRunningAndSummarizesSuccess(t *testing.T) {
	runner := newPluginTUIRunner()
	runner.reload = pluginReloadResult{
		Added: []string{"new@local"}, Changes: []string{"Environment key TAVILY_API_KEY changed"},
	}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.resize(100, 30)
	model.running = true
	model.setInputMode(inputCommand)
	model.composer.SetValue("reload")
	if command := model.submitComposer(); command != nil || len(model.inputQueue) != 1 {
		t.Fatalf("busy reload = command:%v queue:%d", command != nil, len(model.inputQueue))
	}
	model.running = false
	command := model.drainInputQueue()
	if command == nil || !model.pluginReloading {
		t.Fatalf("drained reload = command:%v loading:%v", command != nil, model.pluginReloading)
	}
	model.Update(command())
	if model.pluginReloading || model.pluginReloadPrompt {
		t.Fatalf("reload remained active: loading:%v prompt:%v", model.pluginReloading, model.pluginReloadPrompt)
	}
	transcript := model.renderTranscript()
	for _, expected := range []string{"Configuration reloaded", "Environment key TAVILY_API_KEY changed", "new@local"} {
		if !strings.Contains(transcript, expected) {
			t.Fatalf("reload transcript missing %q: %q", expected, transcript)
		}
	}
}

func TestTUIPluginsBypassesRunningQueueAndHookStatusHasPriority(t *testing.T) {
	runner := newPluginTUIRunner()
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.resize(100, 30)
	model.running = true
	model.setInputMode(inputCommand)
	model.composer.SetValue("plugins")
	command := model.submitComposer()
	if command == nil || model.pluginManager == nil || len(model.inputQueue) != 0 {
		t.Fatalf("busy plugins = command:%v manager:%#v queue:%d", command != nil, model.pluginManager, len(model.inputQueue))
	}
	model.Update(command())
	model.pluginManager.dirty = true
	model.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !model.pluginReloadPrompt {
		t.Fatal("dirty busy plugin manager did not offer reload")
	}
	model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if model.pluginReloadPrompt || len(model.inputQueue) != 1 || model.inputQueue[0].value != "reload" {
		t.Fatalf("busy reload prompt = prompt:%v queue:%#v", model.pluginReloadPrompt, model.inputQueue)
	}
	model.Update(hookStatusMsg{update: hookStatusUpdate{Status: "Checking workspace", Event: dahook.UserPromptSubmit, Active: true}})
	status := ansi.Strip(model.renderStatus())
	if !strings.Contains(status, "Checking workspace") || strings.Contains(status, "ctrl+c cancel") {
		t.Fatalf("active hook status = %q", status)
	}
	model.Update(hookStatusMsg{update: hookStatusUpdate{Event: dahook.UserPromptSubmit}})
	if status = ansi.Strip(model.renderStatus()); strings.Contains(status, "Checking workspace") || !strings.Contains(status, "ctrl+x editor") {
		t.Fatalf("cleared hook status = %q", status)
	}
}
