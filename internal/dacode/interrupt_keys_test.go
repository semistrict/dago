package dacode

import "testing"

func TestEscapeInterruptCascadeUsesPinnedPriority(t *testing.T) {
	all := interruptKeyState{
		ModalOpen: true, DeleteConfirm: true, CompletionOpen: true, InputMode: inputCommand,
		ShellRunning: true, ApprovalPending: true, QuestionPending: true, GoalPending: true,
		QueuedMessages: 1, AgentRunning: true, Draft: "draft", ClearArmed: true,
	}
	tests := []struct {
		remove func(*interruptKeyState)
		want   interruptKeyAction
	}{
		{func(*interruptKeyState) {}, interruptDismissModal},
		{func(state *interruptKeyState) { state.ModalOpen = false }, interruptCancelDelete},
		{func(state *interruptKeyState) { state.DeleteConfirm = false }, interruptDismissCompletion},
		{func(state *interruptKeyState) { state.CompletionOpen = false }, interruptExitInputMode},
		{func(state *interruptKeyState) { state.InputMode = inputNormal }, interruptCancelShell},
		{func(state *interruptKeyState) { state.ShellRunning = false }, interruptRejectApproval},
		{func(state *interruptKeyState) { state.ApprovalPending = false }, interruptCancelQuestion},
		{func(state *interruptKeyState) { state.QuestionPending = false }, interruptCancelGoal},
		{func(state *interruptKeyState) { state.GoalPending = false }, interruptRetractQueued},
		{func(state *interruptKeyState) { state.QueuedMessages = 0 }, interruptCancelAgent},
		{func(state *interruptKeyState) { state.AgentRunning = false }, interruptClearInput},
		{func(state *interruptKeyState) { state.ClearArmed = false }, interruptArmClearInput},
		{func(state *interruptKeyState) { state.Draft = "" }, interruptNone},
	}
	state := all
	for index, test := range tests {
		test.remove(&state)
		if got := escapeKeyAction(state); got != test.want {
			t.Fatalf("step %d = %q, want %q", index, got, test.want)
		}
	}
}

func TestControlCInterruptsOrRequiresTwoIdlePresses(t *testing.T) {
	if got := controlCAction(interruptKeyState{AgentRunning: true, Draft: "unsent"}); got != interruptCancelAgent {
		t.Fatalf("running = %q", got)
	}
	if got := controlCAction(interruptKeyState{FocusedInput: true, Draft: "unsent"}); got != interruptCopyInput {
		t.Fatalf("draft = %q", got)
	}
	if got := controlCAction(interruptKeyState{ModalOpen: true}); got != interruptArmQuit {
		t.Fatalf("modal first press = %q", got)
	}
	if got := controlCAction(interruptKeyState{ModalOpen: true, QuitArmed: true}); got != interruptQuit {
		t.Fatalf("modal second press = %q", got)
	}
	if got := controlCAction(interruptKeyState{}); got != interruptArmQuit {
		t.Fatalf("first idle = %q", got)
	}
	if got := controlCAction(interruptKeyState{QuitArmed: true}); got != interruptQuit {
		t.Fatalf("second idle = %q", got)
	}
}

func TestControlCCopiesSelectionAndRapidPressEscapesCopyLoop(t *testing.T) {
	state := interruptKeyState{FocusedInput: true, Selection: true, Draft: "selected"}
	if got := controlCAction(state); got != interruptCopySelection {
		t.Fatalf("selection = %q", got)
	}
	state.Selection = false
	if got := controlCAction(state); got != interruptCopyInput {
		t.Fatalf("input = %q", got)
	}
	state.RapidQuit = true
	if got := controlCAction(state); got != interruptArmQuit {
		t.Fatalf("rapid input = %q", got)
	}
	state.QuitArmed = true
	if got := controlCAction(state); got != interruptQuit {
		t.Fatalf("armed rapid input = %q", got)
	}
	state.PasswordInput = true
	state.RapidQuit = false
	state.QuitArmed = false
	if got := controlCAction(state); got != interruptArmQuit {
		t.Fatalf("password input = %q", got)
	}
}

func TestControlDDeletesForwardAndQuitsOnlyInEmptyContext(t *testing.T) {
	if got := controlDAction(interruptKeyState{FocusedInput: true, Draft: "a🙂b", CursorOffset: 1}); got != interruptDeleteForward {
		t.Fatalf("middle draft = %q", got)
	}
	if got := controlDAction(interruptKeyState{FocusedInput: true, Draft: "a🙂b", CursorOffset: 3}); got != interruptQuit {
		t.Fatalf("end draft = %q", got)
	}
	if got := controlDAction(interruptKeyState{AgentRunning: true}); got != interruptQuit {
		t.Fatalf("running = %q", got)
	}
	if got := controlDAction(interruptKeyState{}); got != interruptQuit {
		t.Fatalf("empty = %q", got)
	}
	if got := controlDAction(interruptKeyState{DeleteConfirm: true}); got != interruptArmDeleteQuit {
		t.Fatalf("delete confirm first = %q", got)
	}
	if got := controlDAction(interruptKeyState{DeleteConfirm: true, QuitArmed: true}); got != interruptArmDeleteQuit {
		t.Fatalf("idle quit arm leaked into delete confirmation = %q", got)
	}
	if got := controlDAction(interruptKeyState{DeleteConfirm: true, DeleteQuitArmed: true}); got != interruptDeleteQuit {
		t.Fatalf("delete confirm second = %q", got)
	}
	if got := controlDAction(interruptKeyState{ModalOpen: true}); got != interruptQuit {
		t.Fatalf("ordinary modal = %q", got)
	}
	if got := controlDAction(interruptKeyState{ThreadSelector: true}); got != interruptDeleteThread {
		t.Fatalf("thread selector = %q", got)
	}
	if got := controlDAction(interruptKeyState{AuthPrompt: true}); got != interruptDeleteCredential {
		t.Fatalf("auth prompt = %q", got)
	}
	if got := controlDAction(interruptKeyState{FocusedInput: true, Selection: true, Draft: "text", CursorOffset: 4}); got != interruptDeleteForward {
		t.Fatalf("selection = %q", got)
	}
	if got := controlDAction(interruptKeyState{FocusedInput: true, Draft: "text", CursorOffset: 99}); got != interruptNone {
		t.Fatalf("invalid cursor = %q", got)
	}
}

func TestControlZOnlyRestoresAnArmedNonPasswordDraft(t *testing.T) {
	if got := controlZAction(interruptKeyState{FocusedInput: true, UndoAvailable: true}); got != interruptUndoClear {
		t.Fatalf("undo = %q", got)
	}
	for _, state := range []interruptKeyState{
		{FocusedInput: true},
		{UndoAvailable: true},
		{FocusedInput: true, UndoAvailable: true, PasswordInput: true},
	} {
		if got := controlZAction(state); got != interruptNone {
			t.Errorf("state %#v action = %q", state, got)
		}
	}
}
