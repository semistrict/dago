package dacode

import "strings"

type interruptKeyAction string

const (
	interruptNone              interruptKeyAction = "none"
	interruptDismissModal      interruptKeyAction = "dismiss_modal"
	interruptCancelDelete      interruptKeyAction = "cancel_delete"
	interruptDismissCompletion interruptKeyAction = "dismiss_completion"
	interruptExitInputMode     interruptKeyAction = "exit_input_mode"
	interruptCopySelection     interruptKeyAction = "copy_selection"
	interruptCopyInput         interruptKeyAction = "copy_input"
	interruptCancelShell       interruptKeyAction = "cancel_shell"
	interruptRejectApproval    interruptKeyAction = "reject_approval"
	interruptCancelQuestion    interruptKeyAction = "cancel_question"
	interruptCancelGoal        interruptKeyAction = "cancel_goal"
	interruptRetractQueued     interruptKeyAction = "retract_queued"
	interruptCancelAgent       interruptKeyAction = "cancel_agent"
	interruptArmClearInput     interruptKeyAction = "arm_clear_input"
	interruptClearInput        interruptKeyAction = "clear_input"
	interruptUndoClear         interruptKeyAction = "undo_clear_input"
	interruptArmQuit           interruptKeyAction = "arm_quit"
	interruptArmDeleteQuit     interruptKeyAction = "arm_delete_quit"
	interruptQuit              interruptKeyAction = "quit"
	interruptDeleteQuit        interruptKeyAction = "delete_confirm_quit"
	interruptDeleteForward     interruptKeyAction = "delete_forward"
	interruptDeleteThread      interruptKeyAction = "delete_thread"
	interruptDeleteCredential  interruptKeyAction = "delete_credential"
)

type interruptKeyState struct {
	ModalOpen       bool
	DeleteConfirm   bool
	CompletionOpen  bool
	InputMode       inputMode
	ShellRunning    bool
	ApprovalPending bool
	QuestionPending bool
	GoalPending     bool
	QueuedMessages  int
	AgentRunning    bool
	Draft           string
	CursorOffset    int
	FocusedInput    bool
	Selection       bool
	PasswordInput   bool
	RapidQuit       bool
	ThreadSelector  bool
	AuthPrompt      bool
	ClearArmed      bool
	QuitArmed       bool
	DeleteQuitArmed bool
	UndoAvailable   bool
}

func escapeKeyAction(state interruptKeyState) interruptKeyAction {
	switch {
	case state.ModalOpen:
		return interruptDismissModal
	case state.DeleteConfirm:
		return interruptCancelDelete
	case state.CompletionOpen:
		return interruptDismissCompletion
	case state.InputMode != inputNormal:
		return interruptExitInputMode
	case state.ShellRunning:
		return interruptCancelShell
	case state.ApprovalPending:
		return interruptRejectApproval
	case state.QuestionPending:
		return interruptCancelQuestion
	case state.GoalPending:
		return interruptCancelGoal
	case state.QueuedMessages > 0:
		return interruptRetractQueued
	case state.AgentRunning:
		return interruptCancelAgent
	case state.Draft != "" && state.ClearArmed:
		return interruptClearInput
	case state.Draft != "":
		return interruptArmClearInput
	default:
		return interruptNone
	}
}

func controlCAction(state interruptKeyState) interruptKeyAction {
	switch {
	case state.Selection && state.FocusedInput && !state.PasswordInput && !state.RapidQuit:
		return interruptCopySelection
	case state.ShellRunning:
		return interruptCancelShell
	case state.ApprovalPending:
		return interruptRejectApproval
	case state.QuestionPending:
		return interruptCancelQuestion
	case state.GoalPending:
		return interruptCancelGoal
	case state.AgentRunning:
		return interruptCancelAgent
	case state.QuitArmed:
		return interruptQuit
	case state.FocusedInput && strings.TrimSpace(state.Draft) != "" && !state.PasswordInput && !state.RapidQuit:
		return interruptCopyInput
	default:
		return interruptArmQuit
	}
}

func controlDAction(state interruptKeyState) interruptKeyAction {
	if state.ThreadSelector && !state.DeleteConfirm {
		return interruptDeleteThread
	}
	if state.AuthPrompt && !state.DeleteConfirm {
		return interruptDeleteCredential
	}
	if state.DeleteConfirm {
		if state.DeleteQuitArmed {
			return interruptDeleteQuit
		}
		return interruptArmDeleteQuit
	}
	characters := []rune(state.Draft)
	if state.FocusedInput && (state.CursorOffset < 0 || state.CursorOffset > len(characters)) {
		// Dynamic cursor state can briefly lag text replacement. Fail closed
		// instead of turning malformed state into an accidental quit.
		return interruptNone
	}
	if state.FocusedInput && (state.Selection || state.CursorOffset < len(characters)) {
		return interruptDeleteForward
	}
	return interruptQuit
}

func controlZAction(state interruptKeyState) interruptKeyAction {
	if state.FocusedInput && state.UndoAvailable && !state.PasswordInput {
		return interruptUndoClear
	}
	return interruptNone
}
