package dacode

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
)

func TestTUIDebugConsoleToggleHiddenCommandsCopyClearAndLiveSnapshot(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.resize(100, 30)

	command, handled := model.slashCommand("/debug-error")
	if !handled || command != nil || len(model.items) != 1 || !strings.Contains(model.items[0].text, "Server failed to start") {
		t.Fatalf("debug error dispatch: handled=%t command=%v items=%#v", handled, command, model.items)
	}
	command, handled = model.slashCommand("/debug")
	if !handled || command == nil || model.debugConsole == nil {
		t.Fatalf("debug open: handled=%t command=%v overlay=%v", handled, command, model.debugConsole)
	}
	view := ansi.Strip(model.View())
	for _, expected := range []string{"Debug Console", "Thread", "thread-1", "Model", "openai:main-model", "Server failed to start"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("debug view missing %q:\n%s", expected, view)
		}
	}

	model.status = "Inspecting"
	model.Update(debugConsoleTickMsg{generation: model.debugConsoleGeneration})
	if view := ansi.Strip(model.View()); !strings.Contains(view, "Inspecting") {
		t.Fatalf("live snapshot did not refresh:\n%s", view)
	}

	// Filter -> copy toggle -> log. Enter on the selected log returns an OSC52
	// payload to the host without performing clipboard I/O in the reducer.
	for range 2 {
		if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyTab}); !handled {
			t.Fatal("debug console did not own Tab")
		}
	}
	copyCommand, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || copyCommand == nil || !strings.HasPrefix(model.clipboardSequence, "\x1b]52;c;") {
		t.Fatalf("debug selected copy: handled=%t command=%v sequence=%q", handled, copyCommand, model.clipboardSequence)
	}

	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlL}); !handled || model.debugConsoleClearedUpto == 0 {
		t.Fatalf("debug clear: handled=%t cursor=%d", handled, model.debugConsoleClearedUpto)
	}
	if got := len(model.debugConsole.snapshotView().Records); got != 0 {
		t.Fatalf("clear retained %d visible records", got)
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlBackslash}); !handled || model.debugConsole != nil {
		t.Fatalf("debug close: handled=%t overlay=%v", handled, model.debugConsole)
	}
	model.slashCommand("/debug")
	if got := len(model.debugConsole.snapshotView().Records); got != 0 {
		t.Fatalf("persisted clear reopened %d historical records", got)
	}
}

func TestTUISubagentPanelConsumesLifecycleReservesLayoutAndTogglesWithCtrlG(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.resize(100, 30)
	initialViewportHeight := model.viewport.Height
	started := dagent.Event{
		Mode: dagent.EventChild, TaskID: "fanout-phase", Child: &dagent.ChildEvent{
			Phase: dagent.ChildStarted, Name: "general-purpose", ToolCallID: "fanout-one",
		},
	}
	model.applyEvent(started)
	view := ansi.Strip(model.View())
	expandedViewportHeight := model.viewport.Height
	if !strings.Contains(view, "dynamic subagents") || !strings.Contains(view, "general-purpose") || model.viewport.Height >= initialViewportHeight {
		t.Fatalf("live panel/layout missing: initial=%d current=%d\n%s", initialViewportHeight, model.viewport.Height, view)
	}

	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG}); !handled {
		t.Fatal("Ctrl+G was not consumed for visible panel")
	}
	collapsed := ansi.Strip(model.View())
	if !strings.Contains(collapsed, "dynamic subagents") || strings.Contains(collapsed, "general-purpose") {
		t.Fatalf("collapsed panel retained body:\n%s", collapsed)
	}
	if model.viewport.Height <= expandedViewportHeight || model.viewport.Height >= initialViewportHeight {
		t.Fatalf("collapsed panel height: initial=%d expanded=%d collapsed=%d", initialViewportHeight, expandedViewportHeight, model.viewport.Height)
	}

	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG}); !handled {
		t.Fatal("second Ctrl+G was not consumed")
	}
	model.applyEvent(dagent.Event{
		Mode: dagent.EventChild, TaskID: "fanout-phase", Child: &dagent.ChildEvent{
			Phase: dagent.ChildCompleted, Name: "general-purpose", ToolCallID: "fanout-one",
		},
	})
	completed := ansi.Strip(model.View())
	if !strings.Contains(completed, "1/1 done") || !strings.Contains(completed, "general-purpose") {
		t.Fatalf("completed panel missing terminal status:\n%s", completed)
	}
}

func TestHiddenDebugCommandsBypassOnlyAsExactCommands(t *testing.T) {
	busy := commandQueueState{AgentRunning: true, ModalRunning: true}
	for _, command := range []string{"/debug", "/DEBUG-ERROR"} {
		if !canBypassCommandQueue(command, busy) {
			t.Fatalf("exact hidden command %q did not bypass", command)
		}
	}
	for _, command := range []string{"/debug extra", "/debug-error now"} {
		if canBypassCommandQueue(command, busy) {
			t.Fatalf("argument-bearing hidden command %q bypassed", command)
		}
	}
}
