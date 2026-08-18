package dacode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

func approvalTestRequest() dagent.ApprovalRequest {
	return dagent.ApprovalRequest{Call: damessage.ToolCall{
		ID: "call-1", Name: "execute", Arguments: []byte(`{"command":"go test ./..."}`),
	}}
}

func TestApprovalReasonInputHasSafeUsefulDefaults(t *testing.T) {
	state := newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	if state.reason.Placeholder != "Reason (Enter to submit, Esc to cancel)" {
		t.Fatalf("placeholder = %q", state.reason.Placeholder)
	}
	if state.reason.CharLimit != maxApprovalReasonCharacters || state.reason.MaxHeight != 1 {
		t.Fatalf("reason limits = chars %d, height %d", state.reason.CharLimit, state.reason.MaxHeight)
	}
}

func TestApprovalReasonCanBeEditedCancelledAndSubmitted(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true

	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !model.approval.reasonMode {
		t.Fatal("Tab did not open the rejection reason input")
	}
	for _, character := range "avoid this call" {
		model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{character}})
	}
	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	if got := model.approval.reason.Value(); got != "avoid this call" {
		t.Fatalf("second Tab changed reason = %q", got)
	}
	model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if model.approval == nil || model.approval.reasonMode || len(runner.inputs) != 0 {
		t.Fatalf("Esc decided the approval: state=%#v inputs=%d", model.approval, len(runner.inputs))
	}

	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	for _, character := range "use safer read check" {
		model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{character}})
	}
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(runner.inputs) != 1 {
		t.Fatalf("resume inputs = %d", len(runner.inputs))
	}
	response := runner.inputs[0].Resume.(dagent.ApprovalResponse)
	choice := response.Decisions["call-1"]
	if choice.Decision != dagent.ApprovalReject || choice.Reason != "use safer read check" {
		t.Fatalf("choice = %#v", choice)
	}
	if choice.Message != "User rejected the tool call with reason: use safer read check" {
		t.Fatalf("model message = %q", choice.Message)
	}
	if got := model.items[len(model.items)-1].text; got != "Rejected: execute\nReason: use safer read check" {
		t.Fatalf("audit notice = %q", got)
	}
}

func TestApprovalReasonRejectKeysFirstCancelEditing(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'n'}},
		{Type: tea.KeyRunes, Runes: []rune{'N'}},
		{Type: tea.KeyEsc},
	} {
		t.Run(key.String(), func(t *testing.T) {
			runner := &fakeRunner{}
			model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
			model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
			model.approval.ready = true
			model.startApprovalReason()
			model.Update(key)
			if model.approval == nil || model.approval.reasonMode || len(runner.inputs) != 0 {
				t.Fatalf("first %q decided approval: state=%#v inputs=%d", key.String(), model.approval, len(runner.inputs))
			}
		})
	}
}

func TestApprovalReasonQuickApproveKeysAreText(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.startApprovalReason()
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := model.approval.reason.Value(); got != "y" || len(runner.inputs) != 0 {
		t.Fatalf("reason = %q, inputs = %d", got, len(runner.inputs))
	}
}

func TestApprovalBlankReasonUsesBareRejection(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.startApprovalReason()
	model.approval.reason.SetValue(" \t ")
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	choice := runner.inputs[0].Resume.(dagent.ApprovalResponse).Decisions["call-1"]
	if choice.Reason != "Rejected by user." || choice.Message != "" {
		t.Fatalf("blank reason choice = %#v", choice)
	}
}

func TestApprovalReasonIsBoundedAndTerminalSafeBeforeResume(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.startApprovalReason()
	model.approval.reason.SetValue("stop\x1b[2J\u202enow")
	model.sanitizeApprovalReasonInput()
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	choice := runner.inputs[0].Resume.(dagent.ApprovalResponse).Decisions["call-1"]
	if strings.ContainsRune(choice.Reason, '\x1b') || strings.ContainsRune(choice.Reason, '\u202e') {
		t.Fatalf("unsafe reason resumed = %q", choice.Reason)
	}
	for _, marker := range []string{"<U+202E RIGHT-TO-LEFT OVERRIDE>"} {
		if !strings.Contains(choice.Reason, marker) {
			t.Fatalf("reason missing %q: %q", marker, choice.Reason)
		}
	}
	if got := sanitizeApprovalRejectReason("stop\x1b[2J"); !strings.Contains(got, "<U+001B CONTROL>") {
		t.Fatalf("central reason sanitizer lost control marker: %q", got)
	}
	if got := sanitizeApprovalRejectReason("first\nsecond"); got != "first second" {
		t.Fatalf("single-line reason = %q", got)
	}
	bounded := sanitizeApprovalRejectReason(strings.Repeat("\x1b", maxApprovalReasonCharacters))
	if len([]rune(bounded)) > maxApprovalReasonCharacters || len(bounded) > maxApprovalReasonBytes {
		t.Fatalf("expanded markers exceeded bounds: %d chars, %d bytes", len([]rune(bounded)), len(bounded))
	}

	state := newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	state.ready = true
	state.reasonMode = true
	state.reason.SetValue(strings.Repeat("x", maxApprovalReasonCharacters+100))
	if got := len([]rune(state.reason.Value())); got > maxApprovalReasonCharacters {
		t.Fatalf("textarea accepted %d characters, maximum %d", got, maxApprovalReasonCharacters)
	}
	plain := ansi.Strip(renderApproval(state, 80))
	if !strings.Contains(plain, "leave blank to reject without a reason") {
		t.Fatalf("reason footer missing:\n%s", plain)
	}
}

func TestApprovalDefersWhileComposerWasRecentlyEdited(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.lastTypedAt = time.Now()
	state := model.presentApproval([]dagent.ApprovalRequest{approvalTestRequest()})
	state.ready = true
	if !state.deferred || model.status != "Waiting for typing to finish" {
		t.Fatalf("approval = %#v, status = %q", state, model.status)
	}
	plain := ansi.Strip(renderApproval(state, 80))
	if !strings.Contains(plain, "Waiting for typing to finish...") || strings.Contains(plain, "Review requested") {
		t.Fatalf("deferred approval leaked decision UI:\n%s", plain)
	}

	oldGeneration := state.deferGeneration
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := model.composer.Value(); got != "y" {
		t.Fatalf("deferred y did not reach composer = %q", got)
	}
	if model.approval == nil || !model.approval.deferred || state.deferGeneration <= oldGeneration {
		t.Fatalf("deferred approval was decided or not rescheduled: %#v", model.approval)
	}
	model.finishDeferredApproval(approvalDeferredTickMsg{generation: oldGeneration})
	if !state.deferred {
		t.Fatal("stale timer revealed approval")
	}
	model.lastTypedAt = time.Now().Add(-approvalTypingIdleDuration - time.Millisecond)
	model.finishDeferredApproval(approvalDeferredTickMsg{generation: state.deferGeneration})
	if state.deferred || state.typingProtected || model.status != "Review action" {
		t.Fatalf("idle approval stayed deferred: %#v, status=%q", state, model.status)
	}
}

func TestSubmittingDeferredDraftQueuesBehindApproval(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.approval.deferred = true
	model.approval.typingProtected = true
	model.composer.SetValue("follow up after the decision")

	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if model.approval == nil || len(runner.inputs) != 0 || len(model.inputQueue) != 1 {
		t.Fatalf("approval=%#v, runner inputs=%d, queue=%#v", model.approval, len(runner.inputs), model.inputQueue)
	}
	if model.inputQueue[0].value != "follow up after the decision" || model.composer.Value() != "" {
		t.Fatalf("queued draft=%#v, composer=%q", model.inputQueue[0], model.composer.Value())
	}
}

func TestAutomaticReviewKeepsTypingOutOfDecisionShortcuts(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")
	state := model.presentApproval([]dagent.ApprovalRequest{approvalTestRequest()})
	state.ready = true // Defensive: reviewing must disable shortcuts even if readiness is stale.
	model.running = true
	model.lastTypedAt = time.Now().Add(-approvalTypingIdleDuration - time.Second)

	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := model.composer.Value(); got != "y" {
		t.Fatalf("reviewing y did not reach composer = %q", got)
	}
	if len(runner.inputs) != 0 || model.approval == nil {
		t.Fatalf("reviewing y decided approval: inputs=%d approval=%#v", len(runner.inputs), model.approval)
	}
	updated, command := model.finishReview(reviewDoneMsg{err: errTestApprovalReview})
	result := updated.(*tuiModel)
	if result.approval == nil || result.approval.reviewing || !result.approval.deferred || command == nil {
		t.Fatalf("review fallback = %#v, command=%v", result.approval, command)
	}
}

func TestCancellingAutomaticReviewCannotResumeApproval(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.reviewing = true
	model.cancelling = true
	model.running = true
	cancelledReview := false
	model.turnCancel = func() { cancelledReview = true }
	result := approvalReviewResult{Assessments: map[string]approvalAssessment{
		"call-1": {RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "routine"},
	}}

	updated, command := model.finishReview(reviewDoneMsg{result: result})
	state := updated.(*tuiModel)
	if !cancelledReview || state.approval != nil || len(runner.inputs) != 0 || command == nil {
		t.Fatalf("cancelled=%t approval=%#v inputs=%d command=%v", cancelledReview, state.approval, len(runner.inputs), command)
	}
	command()
	if len(runner.cancelled) != 1 || runner.cancelled[0] != "thread-1" {
		t.Fatalf("runner cancellations = %#v", runner.cancelled)
	}
}

func TestSwitchingAutomaticReviewToManualIgnoresReviewerApproval(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.reviewing = true
	model.running = true
	model.lastTypedAt = time.Now()
	reviewCancelled := false
	model.turnCancel = func() { reviewCancelled = true }

	model.applyApprovalMode(approvalManual)
	result := approvalReviewResult{Assessments: map[string]approvalAssessment{
		"call-1": {RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "routine"},
	}}
	updated, command := model.finishReview(reviewDoneMsg{result: result})
	state := updated.(*tuiModel)
	if !reviewCancelled || len(runner.inputs) != 0 || state.approval == nil || !state.approval.ready || !state.approval.deferred || command == nil {
		t.Fatalf("cancelled=%t inputs=%d approval=%#v command=%v", reviewCancelled, len(runner.inputs), state.approval, command)
	}
}

func TestManualCommandBypassesQueueDuringAutomaticReview(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.reviewing = true
	model.running = true
	reviewCancelled := false
	model.turnCancel = func() { reviewCancelled = true }
	model.composer.SetValue("/manual")

	model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if model.approvalMode != approvalManual || !reviewCancelled || len(model.inputQueue) != 0 || model.approval == nil {
		t.Fatalf("mode=%v cancelled=%t queue=%#v approval=%#v", model.approvalMode, reviewCancelled, model.inputQueue, model.approval)
	}
}

func TestPersistedManualRequestFreezesReviewerBeforeSaveCompletes(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.approvalModeStore = newApprovalModeStore(t.TempDir() + "/approval.json")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.reviewing = true
	model.running = true
	reviewCancelled := false
	model.turnCancel = func() { reviewCancelled = true }

	save := model.setApprovalMode(approvalManual)
	if save == nil || !reviewCancelled || !model.approval.freezeReview || model.approvalMode != approvalAuto {
		t.Fatalf("save=%v cancelled=%t approval=%#v mode=%v", save, reviewCancelled, model.approval, model.approvalMode)
	}
	result := approvalReviewResult{Assessments: map[string]approvalAssessment{
		"call-1": {RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "routine"},
	}}
	model.finishReview(reviewDoneMsg{result: result})
	if len(model.runner.(*fakeRunner).inputs) != 0 || model.approval == nil || !model.approval.freezeReview {
		t.Fatalf("review escaped freeze: approval=%#v", model.approval)
	}
	saved := save().(approvalModeSavedMsg)
	model.Update(saved)
	if model.approvalMode != approvalManual || model.approval == nil || !model.approval.ready || model.approval.freezeReview {
		t.Fatalf("saved Manual state: mode=%v approval=%#v", model.approvalMode, model.approval)
	}
}

func TestPendingManualSaveFreezesApprovalThatArrivesLater(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.approvalModeStore = newApprovalModeStore(t.TempDir() + "/approval.json")
	model.running = true
	model.lastTypedAt = time.Now()
	save := model.setApprovalMode(approvalManual)
	if save == nil || !model.approvalModePendingSet || model.effectiveApprovalMode() != approvalManual {
		t.Fatalf("save=%v pending=%t effective=%v", save, model.approvalModePendingSet, model.effectiveApprovalMode())
	}
	state := model.presentApproval([]dagent.ApprovalRequest{approvalTestRequest()})
	if state.preparingReview || state.reviewing || !state.deferred || !state.typingProtected {
		t.Fatalf("approval during pending Manual save = %#v", state)
	}
	_, command := model.finishStream(streamDoneMsg{})
	if command == nil || !state.ready || state.reviewing {
		t.Fatalf("finished pending Manual approval = %#v, command=%v", state, command)
	}
}

func TestAutoModeStartsPendingApprovalReviewWithoutNotice(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.approvalMode = approvalAuto
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true

	command := model.beginAutomaticApprovalReview()
	if command == nil || model.autoModeNotice || !model.approval.reviewing || model.approval.ready || !model.running {
		t.Fatalf("notice=%t approval=%#v running=%t command=%v", model.autoModeNotice, model.approval, model.running, command)
	}
}

func TestSwitchingDeferredManualApprovalToAutoStartsReview(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.ready = true
	model.approval.deferred = true
	model.approval.typingProtected = true
	model.approval.deferGeneration = 7

	command := model.applyApprovalMode(approvalAuto)
	if command == nil || !model.running || model.approval.deferred || !model.approval.reviewing || model.approval.ready {
		t.Fatalf("approval=%#v running=%t command=%v", model.approval, model.running, command)
	}
	if model.approval.deferGeneration != 8 || model.status != "Reviewing action" {
		t.Fatalf("generation=%d status=%q", model.approval.deferGeneration, model.status)
	}
}

func TestSwitchingPreparingAutoApprovalToManualKeepsAgentStream(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.lastTypedAt = time.Now()
	state := model.presentApproval([]dagent.ApprovalRequest{approvalTestRequest()})
	model.running = true
	agentStreamCancelled := false
	model.turnCancel = func() { agentStreamCancelled = true }

	model.applyApprovalMode(approvalManual)
	if agentStreamCancelled || state.preparingReview || state.reviewing {
		t.Fatalf("agent stream cancelled=%t approval=%#v", agentStreamCancelled, state)
	}
	updated, command := model.finishStream(streamDoneMsg{})
	result := updated.(*tuiModel)
	if result.approval == nil || !result.approval.ready || !result.approval.deferred || command == nil {
		t.Fatalf("manual reconciliation=%#v command=%v", result.approval, command)
	}
}

func TestCancellingDeferredApprovalStreamClearsPendingState(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.approval.deferred = true
	model.approval.typingProtected = true
	model.running = true
	model.cancelling = true

	updated, command := model.finishStream(streamDoneMsg{err: context.Canceled})
	state := updated.(*tuiModel)
	if state.approval != nil || command == nil {
		t.Fatalf("approval=%#v command=%v", state.approval, command)
	}
	command()
	if len(runner.cancelled) != 1 || runner.cancelled[0] != "thread-1" {
		t.Fatalf("runner cancellations = %#v", runner.cancelled)
	}
}

func TestStreamFailureDrainsDraftQueuedBehindApproval(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, false, "")
	model.running = true
	model.inputQueue = []queuedInput{{mode: inputNormal, value: "follow-up", display: "follow-up"}}

	_, command := model.finishStream(streamDoneMsg{err: errors.New("approval continuation failed")})
	if command == nil || len(model.inputQueue) != 0 || len(runner.inputs) != 1 {
		t.Fatalf("command=%v queue=%#v runner inputs=%d", command, model.inputQueue, len(runner.inputs))
	}
	if got := runner.inputs[0].Messages[0].TextContent(); got != "follow-up" {
		t.Fatalf("drained message = %q", got)
	}
}

func TestApprovalDeferralHasBoundedDeadline(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.lastTypedAt = time.Now()
	state := model.presentApproval([]dagent.ApprovalRequest{approvalTestRequest()})
	state.ready = true
	state.deferredAt = time.Now().Add(-approvalDeferralTimeout - time.Millisecond)
	model.finishDeferredApproval(approvalDeferredTickMsg{generation: state.deferGeneration})
	if state.deferred || !state.typingProtected {
		t.Fatalf("deadline state = %#v", state)
	}
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if got := model.composer.Value(); got != "y" {
		t.Fatalf("deadline y did not remain composer text = %q", got)
	}
	model.lastTypedAt = time.Now().Add(-approvalTypingIdleDuration - time.Millisecond)
	model.finishDeferredApproval(approvalDeferredTickMsg{generation: state.deferGeneration})
	if state.typingProtected {
		t.Fatal("idle typing protection did not clear")
	}
}

func TestResultOnlyApprovalDeferralSchedulesItsIdleCheck(t *testing.T) {
	request := approvalTestRequest()
	streamResult := dagent.Result{Interrupts: []dagent.Interrupt{{
		ID: "human_approval", Value: []dagent.ApprovalRequest{request},
	}}}
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.running = true
	model.lastTypedAt = time.Now()
	updated, command := model.finishStream(streamDoneMsg{result: streamResult})
	result := updated.(*tuiModel)
	if result.approval == nil || !result.approval.deferred || !result.approval.ready || command == nil {
		t.Fatalf("result-only deferral = %#v, command=%v", result.approval, command)
	}
}

func TestApprovalAppearsImmediatelyWhenComposerIsIdle(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	model.lastTypedAt = time.Now().Add(-approvalTypingIdleDuration - time.Millisecond)
	state := model.presentApproval([]dagent.ApprovalRequest{approvalTestRequest()})
	if state.deferred {
		t.Fatal("idle composer deferred approval")
	}
}

func TestAutomaticReviewFallbackAlsoDefersWhileTyping(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.approval = newApprovalState([]dagent.ApprovalRequest{approvalTestRequest()})
	model.lastTypedAt = time.Now()
	updated, command := model.finishReview(reviewDoneMsg{err: errTestApprovalReview})
	result := updated.(*tuiModel)
	if result.approval == nil || !result.approval.ready || !result.approval.deferred || command == nil {
		t.Fatalf("review fallback = %#v, command=%v", result.approval, command)
	}
}

var errTestApprovalReview = &approvalReviewTestError{}

type approvalReviewTestError struct{}

func (*approvalReviewTestError) Error() string { return "human fallback threshold reached" }
