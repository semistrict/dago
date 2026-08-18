package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
)

type interactionCompactionRunner struct {
	*fakeRunner
	compactCalls int
}

type serializedForceClearStream struct {
	index    atomic.Int32
	inFlight atomic.Int32
	maximum  atomic.Int32
}

func (stream *serializedForceClearStream) Next(context.Context) (dagent.Event, error) {
	current := stream.inFlight.Add(1)
	defer stream.inFlight.Add(-1)
	for previous := stream.maximum.Load(); current > previous && !stream.maximum.CompareAndSwap(previous, current); previous = stream.maximum.Load() {
	}
	if stream.index.Add(1) == 1 {
		return dagent.Event{Mode: dagent.EventValues}, nil
	}
	return dagent.Event{}, io.EOF
}

func (*serializedForceClearStream) Result(context.Context) (dagent.Result, error) {
	return dagent.Result{}, nil
}

func (*serializedForceClearStream) Close() error { return nil }

type blockingCancelRunner struct {
	*fakeRunner
	started  chan struct{}
	release  chan struct{}
	start    sync.Once
	calls    atomic.Int32
	inFlight atomic.Int32
	maximum  atomic.Int32
}

func (runner *blockingCancelRunner) Cancel(ctx context.Context, threadID string) error {
	runner.calls.Add(1)
	current := runner.inFlight.Add(1)
	defer runner.inFlight.Add(-1)
	for previous := runner.maximum.Load(); current > previous && !runner.maximum.CompareAndSwap(previous, current); previous = runner.maximum.Load() {
	}
	runner.start.Do(func() { close(runner.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-runner.release:
		runner.fakeRunner.cancelled = append(runner.fakeRunner.cancelled, threadID)
		return nil
	}
}

func (runner *interactionCompactionRunner) CompactSession(context.Context, string, string) (sessionCompactionResult, error) {
	runner.compactCalls++
	return sessionCompactionResult{Output: "Conversation compacted."}, nil
}

func TestForceClearCreatesNewThreadResetsStateAndFencesLateTurn(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "openai:test", "old-thread", false, false, "")
	model.createThreadID = func() (string, error) { return "new-thread", nil }
	model.operationGeneration = 9
	model.running = true
	model.stream = &fakeEventStream{}
	cancelled := false
	model.turnCancel = func() { cancelled = true }
	model.inputQueue = []queuedInput{{mode: inputNormal, value: "later"}}
	model.deferredActions.deferAction(deferredAction{Kind: deferredChatOutput, Execute: func() tea.Msg { return nil }})
	model.items = []transcriptItem{{kind: itemUser, text: "private old prompt"}}
	model.goal = &dagoal.Goal{Objective: "old objective"}
	model.lastUsage = damessage.Usage{InputTokens: 99}

	command := model.applyClearCommand(true)
	if command == nil || !cancelled || model.threadID != "new-thread" || model.operationGeneration != 10 || model.running ||
		len(model.items) != 0 || len(model.inputQueue) != 0 || model.deferredActions.length() != 0 || model.goal != nil || model.lastUsage.InputTokens != 0 {
		t.Fatalf("force clear state thread=%q generation=%d running=%v items=%#v queue=%#v deferred=%d goal=%#v usage=%#v", model.threadID, model.operationGeneration, model.running, model.items, model.inputQueue, model.deferredActions.length(), model.goal, model.lastUsage)
	}
	model.Update(streamDoneMsg{generation: 9, err: errors.New("late private failure")})
	model.Update(goalActionMsg{generation: 9, action: "set", goal: &dagoal.Goal{Objective: "late old goal"}})
	if len(model.items) != 0 || model.goal != nil || strings.Contains(model.View(), "late private failure") {
		t.Fatalf("late old turn crossed generation fence: %#v", model.items)
	}
}

func TestForceClearFinalizesDurableCancellationOnlyAfterStreamStops(t *testing.T) {
	runner := &fakeRunner{}
	stream := &serializedForceClearStream{}
	model := newTUIModel(t.Context(), runner, "/work", "openai:test", "old-thread", false, false, "")
	model.createThreadID = func() (string, error) { return "new-thread", nil }
	model.running = true
	model.stream = stream
	model.turnCancel = func() {}
	model.operationGeneration = 4
	owner := waitForStreamGeneration(model.ctx, stream, 4)

	notice := model.applyClearCommand(true)
	if notice == nil || len(runner.cancelled) != 0 {
		t.Fatalf("durable cancellation started before stream stop: command=%v calls=%#v", notice != nil, runner.cancelled)
	}
	if notice() == nil {
		t.Fatal("force-clear notification command returned no message")
	}
	staleEvent := owner().(streamEventMsg)
	_, wait := model.Update(staleEvent)
	if wait == nil || len(runner.cancelled) != 0 {
		t.Fatalf("durable cancellation started while stale stream still emitted: command=%v calls=%#v", wait != nil, runner.cancelled)
	}
	stopped := wait().(streamDoneMsg)
	_, finalize := model.Update(stopped)
	if finalize == nil || len(runner.cancelled) != 0 {
		t.Fatalf("missing post-stream finalization: command=%v calls=%#v", finalize != nil, runner.cancelled)
	}
	finalized := finalize().(forceClearFinalizedMsg)
	model.Update(finalized)
	if !slices.Equal(runner.cancelled, []string{"old-thread"}) || len(model.forceClearPending) != 0 || stream.maximum.Load() != 1 {
		t.Fatalf("durable cancellation calls=%#v pending=%#v", runner.cancelled, model.forceClearPending)
	}
}

func TestForceClearRetainsQuarantineAcrossTimeoutCancelErrorAndRepeatedClear(t *testing.T) {
	runner := &fakeRunner{cancelErrs: []error{errors.New("durable cancel failed"), nil, nil}}
	model := newTUIModel(t.Context(), runner, "/work", "openai:test", "first-thread", false, false, "")
	threadIDs := []string{"second-thread", "third-thread"}
	model.createThreadID = func() (string, error) {
		value := threadIDs[0]
		threadIDs = threadIDs[1:]
		return value, nil
	}
	model.operationGeneration = 10
	model.running = true
	model.stream = &blockingEventStream{}
	model.turnCancel = func() {}
	model.forceClearTimeout = time.Millisecond
	model.applyClearCommand(true)

	timedOut := waitForForceClearStream(model.ctx, model.forceClearPending[10].stream, 10, model.forceClearTimeout)().(forceClearDrainTimeoutMsg)
	model.Update(timedOut)
	if model.forceClearPending[10] == nil {
		t.Fatal("stream timeout released the first quarantine")
	}

	model.running = true
	model.stream = &fakeEventStream{}
	model.turnCancel = func() {}
	model.applyClearCommand(true)
	if len(model.forceClearPending) != 2 || model.forceClearPending[10] == nil || model.forceClearPending[11] == nil {
		t.Fatalf("repeated force clear overwrote quarantine: %#v", model.forceClearPending)
	}

	model.forceClearPending[10].stream = nil
	failed := model.retryForceClear(10)().(forceClearFinalizedMsg)
	model.Update(failed)
	if failed.err == nil || model.forceClearPending[10] == nil {
		t.Fatalf("cancel error released quarantine: error=%v pending=%#v", failed.err, model.forceClearPending)
	}
	succeeded := model.retryForceClear(10)().(forceClearFinalizedMsg)
	model.Update(succeeded)
	if succeeded.err != nil || model.forceClearPending[10] != nil || model.forceClearPending[11] == nil {
		t.Fatalf("retry did not release only completed quarantine: error=%v pending=%#v", succeeded.err, model.forceClearPending)
	}

	model.forceClearPending[11].stream = &fakeEventStream{}
	model.forceClearPending[11].reading = false
	stopped := model.retryForceClear(11)().(streamDoneMsg)
	_, finalize := model.Update(stopped)
	finalized := finalize().(forceClearFinalizedMsg)
	model.Update(finalized)
	if finalized.err != nil || len(model.forceClearPending) != 0 {
		t.Fatalf("second quarantine did not finish independently: error=%v pending=%#v", finalized.err, model.forceClearPending)
	}
}

func TestForceClearRetrySchedulingIsBoundedAndCoalesced(t *testing.T) {
	runner := &fakeRunner{cancelErrs: make([]error, maximumForceClearRetries+2)}
	for index := range runner.cancelErrs {
		runner.cancelErrs[index] = errors.New("still busy")
	}
	model := newTUIModel(t.Context(), runner, "/work", "openai:test", "new-thread", false, false, "")
	model.forceClearPending = map[uint64]*forceClearPending{4: {generation: 4, threadID: "old-thread"}}

	finalize := model.retryForceClear(4)
	for attempt := 0; attempt < maximumForceClearRetries; attempt++ {
		finalized := finalize().(forceClearFinalizedMsg)
		_, scheduled := model.Update(finalized)
		pending := model.forceClearPending[4]
		if scheduled == nil || pending == nil || !pending.scheduled || int(pending.retries) != attempt+1 {
			t.Fatalf("retry %d was not scheduled once: command=%v pending=%#v", attempt+1, scheduled != nil, pending)
		}
		_, duplicate := model.Update(finalized)
		if duplicate != nil || int(pending.retries) != attempt+1 {
			t.Fatalf("retry %d duplicated schedule: command=%v pending=%#v", attempt+1, duplicate != nil, pending)
		}
		_, finalize = model.Update(forceClearRetryMsg{generation: 4})
		if finalize == nil {
			t.Fatalf("retry %d did not produce its single durable attempt", attempt+1)
		}
	}
	finalized := finalize().(forceClearFinalizedMsg)
	_, scheduled := model.Update(finalized)
	if scheduled != nil || model.forceClearPending[4] == nil || model.forceClearPending[4].scheduled || model.forceClearPending[4].retries != maximumForceClearRetries {
		t.Fatalf("exhausted retry loop was not bounded: command=%v pending=%#v", scheduled != nil, model.forceClearPending[4])
	}
	manual := model.applyClearCommand(true)
	if manual == nil || model.forceClearPending[4].retries != 0 {
		t.Fatalf("explicit cleanup retry unavailable: command=%v pending=%#v", manual != nil, model.forceClearPending[4])
	}
}

func TestForceClearCoalescesManualRetryDuringDurableCancellation(t *testing.T) {
	runner := &blockingCancelRunner{fakeRunner: &fakeRunner{}, started: make(chan struct{}), release: make(chan struct{})}
	model := newTUIModel(t.Context(), runner, "/work", "openai:test", "new-thread", false, false, "")
	model.forceClearPending = map[uint64]*forceClearPending{4: {generation: 4, threadID: "old-thread"}}
	first := model.retryForceClear(4)
	if first == nil || !model.forceClearPending[4].finalizing {
		t.Fatal("durable cancellation did not claim its generation")
	}
	result := make(chan forceClearFinalizedMsg, 1)
	go func() { result <- first().(forceClearFinalizedMsg) }()
	<-runner.started
	if duplicate := model.retryForceClear(4); duplicate != nil {
		t.Fatal("duplicate retry launched while durable cancellation was in flight")
	}
	manual := model.applyClearCommand(true)
	if manual == nil {
		t.Fatal("manual retry did not report quarantined cleanup")
	}
	if batch, ok := manual().(tea.BatchMsg); ok {
		for _, command := range batch {
			if command != nil {
				_ = command()
			}
		}
	}
	if runner.calls.Load() != 1 || runner.maximum.Load() != 1 {
		t.Fatalf("durable cancellation was duplicated: calls=%d max=%d", runner.calls.Load(), runner.maximum.Load())
	}
	close(runner.release)
	finalized := <-result
	model.Update(finalized)
	if finalized.err != nil || runner.calls.Load() != 1 || runner.maximum.Load() != 1 || len(model.forceClearPending) != 0 {
		t.Fatalf("durable cancellation completion calls=%d max=%d pending=%#v error=%v", runner.calls.Load(), runner.maximum.Load(), model.forceClearPending, finalized.err)
	}
}

func TestForceClearRefusesToGrowAFullQuarantineSet(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "openai:test", "current-thread", false, false, "")
	model.running = true
	model.stream = &fakeEventStream{}
	model.turnCancel = func() {}
	model.createThreadID = func() (string, error) { return "must-not-be-used", nil }
	model.forceClearPending = make(map[uint64]*forceClearPending, maximumForceClearQuarantines)
	for generation := range uint64(maximumForceClearQuarantines) {
		model.forceClearPending[generation+10] = &forceClearPending{generation: generation + 10, threadID: "old-thread"}
	}
	if command := model.applyClearCommand(true); command == nil || model.threadID != "current-thread" || !model.running || len(model.forceClearPending) != maximumForceClearQuarantines {
		t.Fatalf("full quarantine set was expanded: command=%v thread=%q running=%v pending=%d", command != nil, model.threadID, model.running, len(model.forceClearPending))
	}
}

func TestForceClearDiscardsActiveDeferredPromptBeforeLatePreferenceResult(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "openai:old", "old-thread", false, false, "")
	model.createThreadID = func() (string, error) { return "new-thread", nil }
	model.modelRecentGeneration = 8
	model.deferredDrain = &deferredDrainProgress{
		waiting: deferredModelSwitch,
		active: deferredActionCompletedMsg{Kind: deferredModelSwitch, Payload: deferredActionPayload{
			Identity: "7", Generation: 3, Arguments: []string{strconv.Itoa(int(modelSelectorSelect)), "openai:new", ""},
		}},
		prompt: queuedInputDispatchMsg{Input: queuedInput{mode: inputNormal, value: "must not run", display: "must not run"}},
	}
	model.applyClearCommand(true)
	model.Update(modelPreferenceMsg{action: "recent", spec: "openai:new", recentGeneration: 8})
	if model.threadID != "new-thread" || model.deferredDrain != nil || len(runner.inputs) != 0 {
		t.Fatalf("late deferred result crossed force-clear fence: thread=%q drain=%#v inputs=%#v", model.threadID, model.deferredDrain, runner.inputs)
	}
}

func TestClearLeavesCurrentThreadUntouchedWhenThreadCreationFails(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "openai:test", "old-thread", false, false, "")
	model.items = []transcriptItem{{kind: itemUser, text: "keep me"}}
	model.createThreadID = func() (string, error) { return "", errors.New("entropy unavailable") }
	if command := model.applyClearCommand(false); command == nil {
		t.Fatal("missing safe failure notification")
	}
	if model.threadID != "old-thread" || len(model.items) != 1 || model.items[0].text != "keep me" {
		t.Fatalf("failed clear mutated current thread: thread=%q items=%#v", model.threadID, model.items)
	}
}

func TestClearShowsResumeHintOnlyForPersistedThread(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "openai:test", "persisted-thread", false, false, "")
	model.createThreadID = func() (string, error) { return "next-thread", nil }
	model.threadHasCheckpoint = true
	if command := model.applyClearCommand(false); command != nil {
		t.Fatal("idle clear unexpectedly scheduled work")
	}
	if model.threadID != "next-thread" || len(model.items) != 1 || !strings.Contains(model.items[0].text, "/threads -r persisted-thread") {
		t.Fatalf("persisted thread resume hint missing: thread=%q items=%#v", model.threadID, model.items)
	}

	model = newTUIModel(t.Context(), &fakeRunner{}, "/work", "openai:test", "ephemeral-thread", false, false, "")
	model.createThreadID = func() (string, error) { return "another-thread", nil }
	if command := model.applyClearCommand(false); command != nil {
		t.Fatal("idle clear unexpectedly scheduled work")
	}
	if len(model.items) != 0 {
		t.Fatalf("ephemeral thread was advertised as resumable: %#v", model.items)
	}
}

func TestLateThreadListCannotPopulateReplacementSelector(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "openai:test", "thread", false, false, "")
	model.sessionPicker = &sessionPickerState{loading: true, requestID: 22}
	model.finishSessionList(sessionsLoadedMsg{
		requestID: 21,
		sessions:  []sessionInfo{{ThreadID: "stale-thread", CheckpointID: "stale-checkpoint"}},
	})
	if !model.sessionPicker.loading || model.sessionPicker.selector != nil || len(model.sessionPicker.sessions) != 0 {
		t.Fatalf("stale list populated replacement selector: %#v", model.sessionPicker)
	}
}

func TestEscapeClearHasExactBoundedUndo(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	draft := "alpha\n🙂 beta [paste-1] [media-1]"
	model.composer.SetValue(draft)
	model.pasteBindings["[paste-1]"] = "exact pasted value"
	model.inputMedia["[media-1]"] = damessage.ContentBlock{Data: []byte{1, 2, 3}, MIMEType: "image/png"}
	model.imageSequence = 4
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled || model.composer.Value() != draft {
		t.Fatal("first escape did not arm clear")
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled || model.composer.Value() != "" {
		t.Fatalf("second escape did not clear draft: %q", model.composer.Value())
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlZ}); !handled || model.composer.Value() != draft ||
		model.pasteBindings["[paste-1]"] != "exact pasted value" || !slices.Equal(model.inputMedia["[media-1]"].Data, []byte{1, 2, 3}) || model.imageSequence != 4 {
		t.Fatalf("Ctrl+Z did not restore exact Unicode draft and attachments: draft=%q bindings=%#v media=%#v sequence=%d", model.composer.Value(), model.pasteBindings, model.inputMedia, model.imageSequence)
	}
	model.composer.SetValue(strings.Repeat("x", maximumDraftUndoBytes+1))
	model.confirmations.press(confirmClearInput, testNow())
	if model.clearComposerWithUndo() || len(model.composer.Value()) != maximumDraftUndoBytes+1 {
		t.Fatal("oversized draft was partially cleared")
	}
}

func TestEscapeClearBoundsAndClonesHiddenAttachmentPayloads(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*tuiModel)
	}{
		{name: "paste", setup: func(model *tuiModel) {
			model.composer.SetValue("[paste-1]")
			model.pasteBindings["[paste-1]"] = strings.Repeat("p", maximumDraftUndoBytes)
		}},
		{name: "media", setup: func(model *tuiModel) {
			model.composer.SetValue("[media-1]")
			model.inputMedia["[media-1]"] = damessage.ContentBlock{Data: make([]byte, maximumDraftUndoBytes), MIMEType: "image/png"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
			test.setup(model)
			before := model.composer.Value()
			if model.clearComposerWithUndo() || model.composer.Value() != before || model.draftUndo.ready || model.draftAttachmentUndo.ready {
				t.Fatalf("oversized hidden payload was cleared: draft=%q undo=%#v attachments=%#v", model.composer.Value(), model.draftUndo, model.draftAttachmentUndo)
			}
		})
	}

	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	data := []byte{1, 2, 3}
	extra := []byte(`{"private":"value"}`)
	model.composer.SetValue("[media-1]")
	model.inputMedia["[media-1]"] = damessage.ContentBlock{Data: data, Extra: map[string]json.RawMessage{"details": extra}}
	if !model.clearComposerWithUndo() {
		t.Fatal("bounded attachment could not be cleared")
	}
	data[0] = 9
	extra[0] = 'x'
	if !model.undoComposerClear() || !slices.Equal(model.inputMedia["[media-1]"].Data, []byte{1, 2, 3}) || string(model.inputMedia["[media-1]"].Extra["details"]) != `{"private":"value"}` {
		t.Fatalf("attachment undo retained mutable aliases: %#v", model.inputMedia["[media-1]"])
	}
}

func TestControlCCopiesDraftAndControlDDeletesUnicodeRune(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.composer.SetValue("copy this")
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}); !handled || model.composer.Value() != "copy this" || model.clipboardSequence == "" {
		t.Fatalf("Ctrl+C did not copy without clearing: draft=%q sequence=%q", model.composer.Value(), model.clipboardSequence)
	}
	model.composer.SetValue("a🙂b")
	model.composer.SetCursor(1)
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}); !handled || model.composer.Value() != "ab" {
		t.Fatalf("Ctrl+D did not delete one Unicode rune: %q", model.composer.Value())
	}
}

func TestControlDMultilineDeletionPreservesLogicalCursor(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.composer.SetValue("ab\né界c")
	model.composer.SetCursor(1)
	if !model.forwardDeleteComposer() || model.composer.Value() != "ab\néc" || model.composer.Line() != 1 ||
		model.composer.LineInfo().StartColumn+model.composer.LineInfo().ColumnOffset != 1 {
		t.Fatalf("wide-rune deletion moved cursor: value=%q line=%d info=%#v", model.composer.Value(), model.composer.Line(), model.composer.LineInfo())
	}
	model.composer.SetValue("ab\né界c")
	model.composer.CursorUp()
	model.composer.SetCursor(2)
	if !model.forwardDeleteComposer() || model.composer.Value() != "abé界c" || model.composer.Line() != 0 ||
		model.composer.LineInfo().StartColumn+model.composer.LineInfo().ColumnOffset != 2 {
		t.Fatalf("newline deletion moved cursor: value=%q line=%d info=%#v", model.composer.Value(), model.composer.Line(), model.composer.LineInfo())
	}
}

func TestControlDDeletesMultilinePlaceholderAfterWideRunes(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.composer.SetValue("🙂x\né界[paste-1]z")
	model.composer.SetCursor(2)
	model.pasteBindings["[paste-1]"] = "private paste"
	if !model.forwardDeleteComposer() || model.composer.Value() != "🙂x\né界z" || model.composer.Line() != 1 ||
		model.composer.LineInfo().StartColumn+model.composer.LineInfo().ColumnOffset != 2 || len(model.pasteBindings) != 0 {
		t.Fatalf("placeholder deletion corrupted wide multiline draft: value=%q line=%d info=%#v bindings=%#v", model.composer.Value(), model.composer.Line(), model.composer.LineInfo(), model.pasteBindings)
	}
}

func TestSubmitComposerQueuesWhileDeferredActionIsInProgress(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "openai:old", "thread", false, false, "")
	model.deferredDrain = &deferredDrainProgress{waiting: deferredModelSwitch}
	model.composer.SetValue("after queued change")
	if command := model.submitComposer(); command != nil || len(model.inputQueue) != 1 || len(runner.inputs) != 0 {
		t.Fatalf("prompt bypassed deferred barrier: command=%v queue=%#v inputs=%#v", command != nil, model.inputQueue, runner.inputs)
	}
}

func TestDeferredCompactResumeHoldsLaterPromptUntilCompactionFinishes(t *testing.T) {
	runner := &interactionCompactionRunner{fakeRunner: &fakeRunner{streams: []eventStream{&fakeEventStream{}}}}
	model := newTUIModel(t.Context(), runner, "/work", "openai:old", "current-thread", false, false, "")
	model.operationGeneration = 7
	model.sessionPicker = &sessionPickerState{resuming: true}
	model.deferredDrain = &deferredDrainProgress{
		waiting:        deferredThreadSwitch,
		waitGeneration: 7,
		active: deferredActionCompletedMsg{Kind: deferredThreadSwitch, Payload: deferredActionPayload{
			Identity: "resumed-thread",
		}},
		prompt: queuedInputDispatchMsg{Input: queuedInput{mode: inputNormal, value: "after compact", display: "after compact"}},
	}
	_, compact := model.Update(sessionLoadedMsg{
		generation: 7,
		session:    sessionInfo{ThreadID: "resumed-thread", CheckpointID: "checkpoint-exact"},
		decision:   sessionResumeDecision{Compact: true},
	})
	if compact == nil || !model.running || !model.deferredCompactionWaiting() || len(runner.inputs) != 0 {
		t.Fatalf("compaction did not retain barrier: command=%v running=%v drain=%#v inputs=%#v", compact != nil, model.running, model.deferredDrain, runner.inputs)
	}
	model.composer.SetValue("typed during compact")
	if command := model.submitComposer(); command != nil || len(model.inputQueue) != 1 {
		t.Fatalf("prompt was not queued during compact barrier: command=%v queue=%#v", command != nil, model.inputQueue)
	}
	finished := compact().(compactionFinishedMsg)
	model.Update(finished)
	if runner.compactCalls != 1 || len(runner.inputs) != 1 || runner.inputs[0].Messages[0].TextContent() != "after compact" || model.deferredDrain != nil {
		t.Fatalf("prompt order after compaction calls=%d inputs=%#v drain=%#v", runner.compactCalls, runner.inputs, model.deferredDrain)
	}
}

func TestForceClearFencesLateCompactionCompletion(t *testing.T) {
	runner := &interactionCompactionRunner{fakeRunner: &fakeRunner{}}
	model := newTUIModel(t.Context(), runner, "/work", "openai:old", "old-thread", false, false, "")
	model.operationGeneration = 3
	compact := model.startCompaction()
	model.createThreadID = func() (string, error) { return "new-thread", nil }
	model.applyClearCommand(true)
	finished := compact().(compactionFinishedMsg)
	model.Update(finished)
	if model.threadID != "new-thread" || model.running || len(model.items) != 0 {
		t.Fatalf("late compaction crossed force-clear fence: thread=%q running=%v items=%#v", model.threadID, model.running, model.items)
	}
}

func TestDeferredModelSwitchRunsBeforeQueuedPrompt(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "openai:old", "thread", false, false, "")
	model.openModelSelector()
	model.modelSelector.setQuery("openai:new")
	model.running = true
	result := model.modelSelector.handleKey("enter", 10)
	model.applyModelSelectorResult(result)
	if model.deferredActions.length() != 1 || model.modelName != "openai:old" {
		t.Fatalf("busy selection mutated live model: model=%q deferred=%d", model.modelName, model.deferredActions.length())
	}
	model.running = false
	model.inputQueue = []queuedInput{{mode: inputNormal, value: "next", display: "next"}}
	drain := model.drainInputQueue()
	message := drain().(deferredDrainCompletedMsg)
	model.finishDeferredDrain(message)
	if model.modelName != "openai:new" || len(runner.inputs) != 1 || runner.inputs[0].Configurable["model"] != "openai:new" {
		t.Fatalf("deferred order model=%q inputs=%#v", model.modelName, runner.inputs)
	}
}

func TestDeferredAsyncActionCompletesBeforeLaterChangeAndPrompt(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}, agentName: defaultAgentName}
	model := newTUIModel(t.Context(), runner, "/work", "openai:old", "thread", false, false, "")
	model.deferAgentSwitch("reviewer", "reviewer-thread")
	model.openModelSelector()
	model.modelSelector.setQuery("openai:new")
	model.running = true
	model.applyModelSelectorResult(model.modelSelector.handleKey("enter", 10))
	model.running = false
	model.inputQueue = []queuedInput{{mode: inputNormal, value: "after changes", display: "after changes"}}

	drain := model.drainInputQueue()
	message := drain().(deferredDrainCompletedMsg)
	_, command := model.Update(message)
	if command == nil || len(runner.inputs) != 0 || model.modelName != "openai:old" {
		t.Fatalf("async action did not form a barrier: command=%v inputs=%d model=%q", command != nil, len(runner.inputs), model.modelName)
	}
	switchMessage := command().(agentSwitchedMsg)
	model.Update(switchMessage)
	if runner.agentName != "reviewer" || model.modelName != "openai:new" || len(runner.inputs) != 1 ||
		runner.inputs[0].Configurable["model"] != "openai:new" {
		t.Fatalf("deferred order agent=%q model=%q inputs=%#v", runner.agentName, model.modelName, runner.inputs)
	}
}

func TestCanonicalQuitAliasAndForceClearDispatch(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "old-thread", false, false, "")
	if command, handled := model.slashCommand("/q"); !handled || command == nil {
		t.Fatal("/q did not dispatch to the canonical quit handler")
	}
	model.createThreadID = func() (string, error) { return "new-thread", nil }
	model.running = true
	model.stream = &fakeEventStream{}
	model.turnCancel = func() {}
	if command, handled := model.slashCommand("/force-clear"); !handled || command == nil || model.threadID != "new-thread" || model.running {
		t.Fatalf("/force-clear result handled=%v command=%v thread=%q running=%v", handled, command != nil, model.threadID, model.running)
	}
}

func testNow() time.Time { return time.Unix(100, 0) }
