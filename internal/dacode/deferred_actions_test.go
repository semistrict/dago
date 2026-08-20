package dacode

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type deferredTestMsg string

func TestDeferredActionsAreLastWriteWinsAndOrderedAcrossKinds(t *testing.T) {
	queue := &deferredActionQueue{}
	queue.deferAction(deferredAction{Kind: deferredModelSwitch, Execute: func() tea.Msg { return deferredTestMsg("first") }})
	queue.deferAction(deferredAction{Kind: deferredThreadSwitch, Execute: func() tea.Msg { return deferredTestMsg("thread") }})
	queue.deferAction(deferredAction{Kind: deferredModelSwitch, Execute: func() tea.Msg { return deferredTestMsg("second") }})
	if queue.length() != 2 {
		t.Fatalf("queue length = %d", queue.length())
	}
	command, exists := queue.pop()
	if !exists {
		t.Fatal("missing first command")
	}
	first := command().(deferredActionCompletedMsg)
	if first.Kind != deferredThreadSwitch || first.Message != deferredTestMsg("thread") || first.Failed {
		t.Fatalf("first = %#v", first)
	}
	command, exists = queue.pop()
	if !exists {
		t.Fatal("missing second command")
	}
	second := command().(deferredActionCompletedMsg)
	if second.Kind != deferredModelSwitch || second.Message != deferredTestMsg("second") || second.Failed {
		t.Fatalf("second = %#v", second)
	}
	if _, exists := queue.pop(); exists {
		t.Fatal("queue was not drained")
	}
}

func TestDeferredActionPayloadIsBoundedExactAndImmutable(t *testing.T) {
	queue := &deferredActionQueue{}
	arguments := []string{"provider:model", "high"}
	queue.deferAction(deferredAction{
		Kind:    deferredModelSwitch,
		Payload: deferredActionPayload{Identity: "request-1", Arguments: arguments, Generation: 7},
		ExecutePayload: func(payload deferredActionPayload) tea.Msg {
			payload.Arguments[0] = "mutated-by-executor"
			return deferredTestMsg(payload.Identity)
		},
	})
	arguments[0] = "mutated-by-caller"
	command, _ := queue.pop()
	message := command().(deferredActionCompletedMsg)
	if message.Message != deferredTestMsg("request-1") || message.Payload.Generation != 7 ||
		!slices.Equal(message.Payload.Arguments, []string{"provider:model", "high"}) {
		t.Fatalf("immutable completion = %#v", message)
	}
}

func TestDeferredDrainContainsPanicContinuesAndRunsPromptLast(t *testing.T) {
	queue := &deferredActionQueue{}
	order := make([]string, 0, 3)
	queue.deferAction(deferredAction{Kind: deferredModelSwitch, Execute: func() tea.Msg {
		order = append(order, "model")
		panic("private panic")
	}})
	queue.deferAction(deferredAction{Kind: deferredThreadSwitch, Execute: func() tea.Msg {
		order = append(order, "thread")
		return deferredTestMsg("thread")
	}})
	command, draining := queue.drainBeforePrompt(func() tea.Msg {
		order = append(order, "prompt")
		return deferredTestMsg("prompt")
	})
	if !draining || queue.length() != 0 {
		t.Fatalf("draining=%v length=%d", draining, queue.length())
	}
	message := command().(deferredDrainCompletedMsg)
	if !slices.Equal(order, []string{"model", "thread", "prompt"}) || len(message.Actions) != 2 ||
		!message.Actions[0].Failed || message.Actions[1].Failed || message.Prompt != deferredTestMsg("prompt") || message.PromptFailed {
		t.Fatalf("order=%#v message=%#v", order, message)
	}
}

func TestDeferredDiscardReportsExactPayloadAndReason(t *testing.T) {
	for _, reason := range []deferredDiscardReason{deferredDiscardForceClear, deferredDiscardMCPRecovery} {
		queue := &deferredActionQueue{}
		queue.deferAction(deferredAction{
			Kind:           deferredMCPReconnect,
			Payload:        deferredActionPayload{Identity: "server", Arguments: []string{"exact"}, Generation: 4},
			ExecutePayload: func(deferredActionPayload) tea.Msg { return nil },
		})
		report := queue.discardFor(reason)
		if report.Reason != reason || len(report.Actions) != 1 || report.Actions[0].Kind != deferredMCPReconnect ||
			report.Actions[0].Payload.Identity != "server" || !slices.Equal(report.Actions[0].Payload.Arguments, []string{"exact"}) || queue.length() != 0 {
			t.Fatalf("reason %q report=%#v", reason, report)
		}
	}
}

func TestDeferredQueueIsFiniteAcrossAllKinds(t *testing.T) {
	queue := &deferredActionQueue{}
	kinds := []deferredActionKind{
		deferredModelSwitch, deferredThreadSwitch, deferredChatOutput, deferredAgentSwitch,
		deferredMCPLogin, deferredMCPReconnect, deferredRubricModelSwitch, deferredRubricMaxIterationsSwitch,
	}
	for _, kind := range kinds {
		queue.deferAction(deferredAction{Kind: kind, Execute: func() tea.Msg { return nil }})
	}
	queue.deferAction(deferredAction{Kind: deferredModelSwitch, Execute: func() tea.Msg { return deferredTestMsg("latest") }})
	if queue.length() != maximumDeferredActionKinds {
		t.Fatalf("queue length = %d", queue.length())
	}
}

func TestDeferredActionPanicIsContained(t *testing.T) {
	queue := &deferredActionQueue{}
	queue.deferAction(deferredAction{Kind: deferredMCPReconnect, Execute: func() tea.Msg { panic("secret panic") }})
	command, _ := queue.pop()
	message := command().(deferredActionCompletedMsg)
	if message.Kind != deferredMCPReconnect || !message.Failed || message.Message != nil {
		t.Fatalf("message = %#v", message)
	}
}

func TestDeferredActionsDiscardReportsKindsWithoutExecuting(t *testing.T) {
	queue := &deferredActionQueue{}
	executed := false
	queue.deferAction(deferredAction{Kind: deferredMCPLogin, Execute: func() tea.Msg { executed = true; return nil }})
	queue.deferAction(deferredAction{Kind: deferredChatOutput, Execute: func() tea.Msg { executed = true; return nil }})
	if kinds := queue.discard(); !slices.Equal(kinds, []deferredActionKind{deferredMCPLogin, deferredChatOutput}) {
		t.Fatalf("discarded = %#v", kinds)
	}
	if executed || queue.length() != 0 {
		t.Fatalf("executed = %v length = %d", executed, queue.length())
	}
}

func TestDeferredActionsRejectInvalidStaticConfiguration(t *testing.T) {
	for _, action := range []deferredAction{
		{Kind: "unknown", Execute: func() tea.Msg { return nil }},
		{Kind: deferredModelSwitch},
		{Kind: deferredModelSwitch, Payload: deferredActionPayload{Identity: strings.Repeat("x", maximumDeferredPayloadBytes+1)}, ExecutePayload: func(deferredActionPayload) tea.Msg { return nil }},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("action did not panic: %#v", action)
				}
			}()
			new(deferredActionQueue).deferAction(action)
		}()
	}
}
