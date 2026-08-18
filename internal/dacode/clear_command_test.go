package dacode

import (
	"slices"
	"testing"
)

func TestClearCommandQueuesWhileBusyWithoutInterrupting(t *testing.T) {
	plan := planClearCommand(false, clearCommandState{
		AgentRunning: true, ShellRunning: true, ApprovalPending: true,
		QuestionPending: true, GoalPending: true, QueuedMessages: 2, DeferredActions: 3,
	})
	if !plan.QueueUntilIdle {
		t.Fatal("ordinary clear did not wait for idle")
	}
	if plan.CancelAgent || plan.CancelShell || plan.RejectApproval || plan.CancelQuestion || plan.CancelGoal || plan.ResetTranscript {
		t.Fatalf("ordinary busy clear performed immediate work: %#v", plan)
	}
}

func TestForceClearInterruptsAndResetsAllPerThreadState(t *testing.T) {
	plan := planClearCommand(true, clearCommandState{
		AgentRunning: true, ShellRunning: true, ApprovalPending: true,
		QuestionPending: true, GoalPending: true, QueuedMessages: 2, DeferredActions: 3,
		PreviousThread: "old-thread", HasCheckpoint: true,
	})
	if plan.QueueUntilIdle || !plan.CancelAgent || !plan.CancelShell || !plan.RejectApproval || !plan.CancelQuestion || !plan.CancelGoal {
		t.Fatalf("force interrupt plan = %#v", plan)
	}
	if !plan.DiscardQueued || !plan.DiscardDeferred || !plan.ResetTranscript || !plan.ResetPerThreadUI || !plan.ResetGoalAndRubric ||
		!plan.ResetApprovalState || !plan.ResetUsageAndCost || !plan.ResetInputPlaceholder || !plan.CreateNewThread {
		t.Fatalf("force reset plan = %#v", plan)
	}
	if plan.DroppedQueuedMessages != 2 || plan.DroppedDeferredActions != 3 {
		t.Fatalf("drop counts = queued %d deferred %d", plan.DroppedQueuedMessages, plan.DroppedDeferredActions)
	}
	wantEffects := []forceClearEffect{
		forceClearCancelAgent, forceClearCancelShell, forceClearRejectApproval, forceClearCancelQuestion,
		forceClearCancelGoal, forceClearDropPromptQueue, forceClearDiscardDeferred,
	}
	if !slices.Equal(plan.ForceEffects, wantEffects) {
		t.Fatalf("force effects = %#v, want %#v", plan.ForceEffects, wantEffects)
	}
	if plan.PreviousThread != "old-thread" {
		t.Fatalf("previous thread = %q", plan.PreviousThread)
	}
}

func TestIdleClearStillBuildsACompleteNewThreadReset(t *testing.T) {
	plan := planClearCommand(false, clearCommandState{QueuedMessages: 1, DeferredActions: 1})
	if plan.QueueUntilIdle || !plan.CreateNewThread || !plan.ResetTranscript || !plan.ResetPerThreadUI ||
		!plan.ResetGoalAndRubric || !plan.ResetApprovalState || !plan.ResetUsageAndCost || !plan.ResetInputPlaceholder {
		t.Fatalf("idle clear plan = %#v", plan)
	}
	if len(plan.ForceEffects) != 0 || plan.DroppedQueuedMessages != 1 || plan.DroppedDeferredActions != 1 {
		t.Fatalf("idle clear reporting = %#v", plan)
	}
}

func TestClearOnlyAdvertisesSafeResumablePreviousThread(t *testing.T) {
	for _, test := range []struct {
		name  string
		state clearCommandState
	}{
		{name: "no checkpoint", state: clearCommandState{PreviousThread: "old-thread"}},
		{name: "unsafe id", state: clearCommandState{PreviousThread: "old\x1b-thread", HasCheckpoint: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if previous := planClearCommand(false, test.state).PreviousThread; previous != "" {
				t.Fatalf("previous thread = %q", previous)
			}
		})
	}
}
