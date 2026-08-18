package dacode

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type pluginUIControllerStub struct {
	snapshot pluginManagerSnapshot
	action   pluginManagerMutation
	target   string
	reload   pluginReloadResult
	err      error
}

func (stub *pluginUIControllerStub) PluginSnapshot(context.Context) (pluginManagerSnapshot, error) {
	return stub.snapshot, stub.err
}
func (stub *pluginUIControllerStub) InstallPlugin(_ context.Context, id string) error {
	stub.action, stub.target = pluginMutationInstall, id
	return stub.err
}
func (stub *pluginUIControllerStub) SetPluginEnabled(_ context.Context, id string, enabled bool) error {
	stub.action, stub.target = pluginMutationDisable, id
	if enabled {
		stub.action = pluginMutationEnable
	}
	return stub.err
}
func (stub *pluginUIControllerStub) UninstallPlugin(_ context.Context, id string) error {
	stub.action, stub.target = pluginMutationUninstall, id
	return stub.err
}
func (stub *pluginUIControllerStub) AddPluginMarketplace(_ context.Context, source string) error {
	stub.action, stub.target = pluginMutationAddMarketplace, source
	return stub.err
}
func (stub *pluginUIControllerStub) RemovePluginMarketplace(_ context.Context, name string) error {
	stub.action, stub.target = pluginMutationRemoveMarketplace, name
	return stub.err
}
func (stub *pluginUIControllerStub) ReloadPlugins(context.Context) (pluginReloadResult, error) {
	return stub.reload, stub.err
}

func TestPluginManagerUICommandsRenderEveryVisibleState(t *testing.T) {
	controller := &pluginUIControllerStub{snapshot: pluginManagerSnapshot{
		Available:    []pluginManagerPlugin{{ID: "review@local", Name: "Review\x1b[31m", Marketplace: "local", Description: "Review changes"}},
		Installed:    []pluginManagerPlugin{{ID: "active@local", Name: "Active", Marketplace: "local", Enabled: true, Loaded: false, Pending: true, Skills: 1, MCP: 2, Hooks: 3}},
		Marketplaces: []pluginManagerMarketplace{{Name: "local", Source: "directory", PluginCount: 2, InstalledCount: 1}},
	}}
	state := newPluginManagerState()
	message, ok := loadPluginManager(t.Context(), controller)().(pluginManagerSnapshotMsg)
	if !ok {
		t.Fatal("snapshot command returned an unexpected message")
	}
	state.applySnapshot(message)
	view := renderPluginManager(state, 100, 30)
	plain := ansi.Strip(view)
	if !strings.Contains(plain, "Plugins") || !strings.Contains(plain, "Review") || strings.Contains(plain, "\x1b[31m") {
		t.Fatalf("discover view = %q", view)
	}
	state.switchTab(1)
	view = renderPluginManager(state, 100, 30)
	for _, expected := range []string{"Installed", "pending /reload", "1 skills", "2 MCP", "3 hooks"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("installed view missing %q: %q", expected, view)
		}
	}
	state.switchTab(1)
	view = renderPluginManager(state, 100, 30)
	if !strings.Contains(view, "Marketplaces") || !strings.Contains(view, "2 plugins") {
		t.Fatalf("marketplaces view = %q", view)
	}
	state.confirmRemoval = true
	if view = renderPluginManager(state, 70, 18); !strings.Contains(view, "Remove marketplace local?") || !strings.Contains(view, "Enter remove") {
		t.Fatalf("remove confirmation view = %q", view)
	}
	state.confirmRemoval = false
	state.addingMarketplace = true
	state.marketplaceInput.Focus()
	state.marketplaceInput.SetValue("/tmp/local")
	if view = renderPluginManager(state, 48, 14); !strings.Contains(view, "Marketplace source") || !strings.Contains(view, "Enter add") {
		t.Fatalf("add marketplace view = %q", view)
	}
	if prompt := renderPluginReloadPrompt(80, 24); !strings.Contains(prompt, "Reload plugins?") || !strings.Contains(prompt, "Enter reload") {
		t.Fatalf("reload prompt = %q", prompt)
	}
	if loading := renderPluginReloading(80, 24); !strings.Contains(loading, "Reloading configuration") || !strings.Contains(loading, "Rebuilding plugin skills") {
		t.Fatalf("reload progress = %q", loading)
	}
}

func TestPluginManagerUIActionsAndReloadMessagesAreDeterministic(t *testing.T) {
	controller := &pluginUIControllerStub{reload: pluginReloadResult{Added: []string{"new@local"}, Removed: []string{"old@local"}, Warnings: []string{"one warning"}}}
	for _, test := range []struct {
		action pluginManagerMutation
		target string
	}{
		{pluginMutationInstall, "new@local"}, {pluginMutationEnable, "new@local"},
		{pluginMutationDisable, "new@local"}, {pluginMutationUninstall, "new@local"},
		{pluginMutationAddMarketplace, "/tmp/local"}, {pluginMutationRemoveMarketplace, "local"},
	} {
		message, ok := mutatePluginManager(t.Context(), controller, test.action, test.target)().(pluginManagerMutationMsg)
		if !ok || message.err != nil || controller.action != test.action || controller.target != test.target {
			t.Fatalf("action %q = %#v controller=%q/%q", test.action, message, controller.action, controller.target)
		}
		state := newPluginManagerState()
		state.applyMutation(message)
		if !state.dirty || !state.loading || !strings.Contains(state.status, "Reload pending") {
			t.Fatalf("mutation state = %#v", state)
		}
	}
	message, ok := reloadPluginRuntime(t.Context(), controller)().(pluginReloadMsg)
	if !ok || message.err != nil {
		t.Fatalf("reload message = %#v", message)
	}
	summary := pluginReloadSummary(message.result)
	for _, expected := range []string{"Configuration reloaded", "new@local", "old@local", "one warning"} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("reload summary missing %q: %q", expected, summary)
		}
	}
	controller.err = context.Canceled
	mutation := mutatePluginManager(t.Context(), controller, pluginMutationInstall, "new@local")().(pluginManagerMutationMsg)
	state := newPluginManagerState()
	state.applyMutation(mutation)
	if state.error != "Plugin action cancelled." || state.dirty {
		t.Fatalf("cancelled mutation state = %#v", state)
	}
	controller.err = errors.New("credential-shaped remote failure")
	snapshot := loadPluginManager(t.Context(), controller)().(pluginManagerSnapshotMsg)
	state.applySnapshot(snapshot)
	if strings.Contains(state.error, "credential") {
		t.Fatalf("unsafe error reached UI: %q", state.error)
	}
}

func TestPluginManagerKeyHandlingCoversNavigationAndEveryMutation(t *testing.T) {
	controller := &pluginUIControllerStub{}
	state := newPluginManagerState()
	state.applySnapshot(pluginManagerSnapshotMsg{snapshot: pluginManagerSnapshot{
		Available:    []pluginManagerPlugin{{ID: "new@local"}},
		Installed:    []pluginManagerPlugin{{ID: "active@local", Enabled: true}},
		Marketplaces: []pluginManagerMarketplace{{Name: "local"}},
	}})
	command, close := state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEnter})
	if close || command == nil {
		t.Fatalf("install key = command:%v close:%v", command != nil, close)
	}
	message := command().(pluginManagerMutationMsg)
	if message.action != pluginMutationInstall || message.target != "new@local" {
		t.Fatalf("install = %#v", message)
	}
	state.mutating = false
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRight})
	command, _ = state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEnter})
	message = command().(pluginManagerMutationMsg)
	if message.action != pluginMutationDisable || message.target != "active@local" {
		t.Fatalf("disable = %#v", message)
	}
	state.mutating = false
	command, _ = state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	message = command().(pluginManagerMutationMsg)
	if message.action != pluginMutationUninstall {
		t.Fatalf("uninstall = %#v", message)
	}
	state.mutating = false
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyTab})
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	if !state.addingMarketplace {
		t.Fatal("add marketplace input did not open")
	}
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" /tmp/local ")})
	command, _ = state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEnter})
	message = command().(pluginManagerMutationMsg)
	if message.action != pluginMutationAddMarketplace || message.target != "/tmp/local" {
		t.Fatalf("add marketplace = %#v", message)
	}
	state.mutating, state.addingMarketplace = false, false
	command, _ = state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	if command != nil || !state.confirmRemoval {
		t.Fatalf("remove confirmation = command:%v active:%v", command != nil, state.confirmRemoval)
	}
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEsc})
	if state.confirmRemoval {
		t.Fatal("remove confirmation did not cancel")
	}
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	command, _ = state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEnter})
	message = command().(pluginManagerMutationMsg)
	if message.action != pluginMutationRemoveMarketplace || message.target != "local" {
		t.Fatalf("remove marketplace = %#v", message)
	}
	state.mutating = false
	if command, close = state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEsc}); command != nil || !close {
		t.Fatalf("escape = command:%v close:%v", command != nil, close)
	}
}

func TestPluginManagerMarketplaceInputIsBoundedAndCancellable(t *testing.T) {
	controller := &pluginUIControllerStub{}
	state := newPluginManagerState()
	state.loading = false
	state.tab = pluginTabMarketplaces
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	command, close := state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || close || state.error != "Marketplace source is required." {
		t.Fatalf("empty source = command:%v close:%v error:%q", command != nil, close, state.error)
	}
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("x", 5000))})
	if len([]rune(state.marketplaceInput.Value())) != state.marketplaceInput.CharLimit {
		t.Fatalf("input length = %d", len([]rune(state.marketplaceInput.Value())))
	}
	state.handleKey(t.Context(), controller, tea.KeyMsg{Type: tea.KeyEsc})
	if state.addingMarketplace || state.marketplaceInput.Value() != "" || state.error != "" {
		t.Fatalf("cancelled input state = %#v", state)
	}
}
