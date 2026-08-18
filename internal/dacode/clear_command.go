package dacode

// clearCommandState is the minimum dynamic state needed to distinguish the
// queued /clear path from the always-immediate /force-clear escape hatch.
type clearCommandState struct {
	AgentRunning    bool
	ShellRunning    bool
	ApprovalPending bool
	QuestionPending bool
	GoalPending     bool
	QueuedMessages  int
	DeferredActions int
	PreviousThread  string
	HasCheckpoint   bool
}

type clearCommandPlan struct {
	QueueUntilIdle         bool
	CancelAgent            bool
	CancelShell            bool
	RejectApproval         bool
	CancelQuestion         bool
	CancelGoal             bool
	DiscardQueued          bool
	DiscardDeferred        bool
	ResetTranscript        bool
	ResetPerThreadUI       bool
	ResetGoalAndRubric     bool
	ResetApprovalState     bool
	ResetUsageAndCost      bool
	ResetInputPlaceholder  bool
	CreateNewThread        bool
	DroppedQueuedMessages  int
	DroppedDeferredActions int
	ForceEffects           []forceClearEffect
	PreviousThread         string
}

type forceClearEffect string

const (
	forceClearCancelAgent     forceClearEffect = "cancel_agent"
	forceClearCancelShell     forceClearEffect = "cancel_shell"
	forceClearRejectApproval  forceClearEffect = "reject_approval"
	forceClearCancelQuestion  forceClearEffect = "cancel_question"
	forceClearCancelGoal      forceClearEffect = "cancel_goal"
	forceClearDropPromptQueue forceClearEffect = "drop_prompt_queue"
	forceClearDiscardDeferred forceClearEffect = "discard_deferred_actions"
)

// planClearCommand returns the deterministic reset work for /clear. Required
// runtime state is positional; force selects the explicitly requested
// /force-clear behavior. New thread ID generation remains an effect owned by
// the lifecycle layer.
func planClearCommand(force bool, state clearCommandState) clearCommandPlan {
	busy := state.AgentRunning || state.ShellRunning || state.ApprovalPending || state.QuestionPending || state.GoalPending
	if busy && !force {
		return clearCommandPlan{QueueUntilIdle: true}
	}
	plan := clearCommandPlan{
		CancelAgent:            force && state.AgentRunning,
		CancelShell:            force && state.ShellRunning,
		RejectApproval:         force && state.ApprovalPending,
		CancelQuestion:         force && state.QuestionPending,
		CancelGoal:             force && state.GoalPending,
		DiscardQueued:          state.QueuedMessages > 0,
		DiscardDeferred:        state.DeferredActions > 0,
		ResetTranscript:        true,
		ResetPerThreadUI:       true,
		ResetGoalAndRubric:     true,
		ResetApprovalState:     true,
		ResetUsageAndCost:      true,
		ResetInputPlaceholder:  true,
		CreateNewThread:        true,
		DroppedQueuedMessages:  max(state.QueuedMessages, 0),
		DroppedDeferredActions: max(state.DeferredActions, 0),
	}
	if force {
		if plan.CancelAgent {
			plan.ForceEffects = append(plan.ForceEffects, forceClearCancelAgent)
		}
		if plan.CancelShell {
			plan.ForceEffects = append(plan.ForceEffects, forceClearCancelShell)
		}
		if plan.RejectApproval {
			plan.ForceEffects = append(plan.ForceEffects, forceClearRejectApproval)
		}
		if plan.CancelQuestion {
			plan.ForceEffects = append(plan.ForceEffects, forceClearCancelQuestion)
		}
		if plan.CancelGoal {
			plan.ForceEffects = append(plan.ForceEffects, forceClearCancelGoal)
		}
		if plan.DiscardQueued {
			plan.ForceEffects = append(plan.ForceEffects, forceClearDropPromptQueue)
		}
		if plan.DiscardDeferred {
			plan.ForceEffects = append(plan.ForceEffects, forceClearDiscardDeferred)
		}
	}
	if state.HasCheckpoint {
		plan.PreviousThread = validThreadSelectorID(state.PreviousThread)
	}
	return plan
}
