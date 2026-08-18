package dacode

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
)

func TestSubagentPanelLifecycleAndDeterministicTiming(t *testing.T) {
	now := time.Unix(100, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	if snapshot := state.snapshot(); snapshot.Visible || snapshot.Total != 0 || !snapshot.Expanded {
		t.Fatalf("initial snapshot = %#v", snapshot)
	}
	if !state.apply(subagentPanelStart("a", "E1", "research", "read source")) {
		t.Fatal("start event was not applied")
	}
	now = now.Add(1500 * time.Millisecond)
	snapshot := state.snapshot()
	if !snapshot.Visible || !snapshot.Running || snapshot.Total != 1 || snapshot.Phases[0].Records[0].Elapsed != 1500*time.Millisecond {
		t.Fatalf("running snapshot = %#v", snapshot)
	}
	if !state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "a", EvalID: "E1", Duration: 2 * time.Second}) {
		t.Fatal("completion was not applied")
	}
	now = now.Add(time.Hour)
	snapshot = state.snapshot()
	if snapshot.Running || snapshot.Done != 1 || snapshot.Phases[0].Elapsed != 2*time.Second || snapshot.Phases[0].Records[0].Elapsed != 2*time.Second {
		t.Fatalf("terminal snapshot = %#v", snapshot)
	}
}

func TestSubagentPanelPhasesSelectionAndCollapsePreference(t *testing.T) {
	now := time.Unix(200, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	state.apply(subagentPanelStart("a", "E1", "research", "phase one"))
	state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "a", EvalID: "E1", Duration: time.Second})
	state.apply(subagentPanelStart("b", "E2", "review", "phase two"))
	if got := state.snapshot().DisplayedPhase; got != 1 {
		t.Fatalf("active displayed phase = %d, want 1", got)
	}
	if !state.handleKey("k") || state.snapshot().DisplayedPhase != 0 {
		t.Fatal("k did not select the previous phase")
	}
	state.apply(subagentPanelStart("c", "E3", "test", "phase three"))
	if got := state.snapshot().DisplayedPhase; got != 0 {
		t.Fatalf("explicit selection moved to %d after a new active phase", got)
	}
	if !state.handleKey("up") || state.snapshot().DisplayedPhase != 0 {
		t.Fatal("selection did not clamp at the first phase")
	}
	if !state.handleKey("down") || state.snapshot().DisplayedPhase != 1 {
		t.Fatal("down did not select the second phase")
	}
	if !state.selectPhase(2) || state.snapshot().DisplayedPhase != 2 || state.selectPhase(99) {
		t.Fatal("phase click-selection seam did not select or reject an index")
	}
	if !state.handleKey("ctrl+g") || state.snapshot().Expanded {
		t.Fatal("Ctrl+G did not collapse the panel")
	}
	state.prepareTurn("provider:model")
	state.apply(subagentPanelStart("d", "E4", "research", "next turn"))
	snapshot := state.snapshot()
	if snapshot.Expanded || !snapshot.Visible || len(snapshot.Phases) != 1 || snapshot.ModelLabel != "provider:model" {
		t.Fatalf("next-turn snapshot = %#v", snapshot)
	}
}

func TestSubagentPanelOrphanAndTerminalEventsFailClosed(t *testing.T) {
	now := time.Unix(300, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	if state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "ghost", EvalID: "E1"}) {
		t.Fatal("orphan completion created a phantom row")
	}
	if state.snapshot().Visible {
		t.Fatal("orphan completion made the panel visible")
	}
	if !state.apply(subagentPanelEvent{Phase: subagentPanelEventError, ID: "orphan", EvalID: "E2", Error: "dropped start"}) {
		t.Fatal("orphan error was not surfaced")
	}
	snapshot := state.snapshot()
	record := snapshot.Phases[0].Records[0]
	if record.Status != subagentPanelError || record.Error != "dropped start" || record.Label != "subagent: unknown task" {
		t.Fatalf("orphan error record = %#v", record)
	}
	if state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "orphan", EvalID: "E2", Duration: time.Second}) {
		t.Fatal("late completion rewrote a terminal error")
	}
	if got := state.snapshot().Phases[0].Records[0].Status; got != subagentPanelError {
		t.Fatalf("terminal status = %v, want error", got)
	}
}

func TestSubagentPanelConvertsOnlyTopLevelChildLifecycleEvents(t *testing.T) {
	for _, test := range []struct {
		phase dagent.ChildEventPhase
		want  subagentPanelEventPhase
	}{
		{dagent.ChildStarted, subagentPanelEventStart},
		{dagent.ChildCompleted, subagentPanelEventComplete},
		{dagent.ChildFailed, subagentPanelEventError},
		{dagent.ChildInterrupted, subagentPanelEventCancelled},
	} {
		event, ok := subagentPanelEventFromAgentEvent(dagent.Event{
			Mode: dagent.EventChild, TaskID: "parent-task", Child: &dagent.ChildEvent{
				Phase: test.phase, Name: "researcher", ToolCallID: "child-call", Error: "failure",
			},
		})
		if !ok || event.Phase != test.want || event.ID != "child-call" || event.EvalID != "parent-task" || event.Label != "researcher" {
			t.Fatalf("phase %q converted to %#v, ok=%t", test.phase, event, ok)
		}
		if test.phase == dagent.ChildFailed && event.Error != "failure" {
			t.Fatalf("failure error = %q", event.Error)
		}
	}
	for _, event := range []dagent.Event{
		{Mode: dagent.EventUpdate},
		{Mode: dagent.EventChild},
		{Mode: dagent.EventChild, Child: &dagent.ChildEvent{Phase: dagent.ChildEventUpdate, ToolCallID: "child-call"}},
	} {
		if converted, ok := subagentPanelEventFromAgentEvent(event); ok {
			t.Fatalf("non-lifecycle event converted to %#v", converted)
		}
	}
}

func TestSubagentPanelInterruptedEventCancelsOneRow(t *testing.T) {
	now := time.Unix(350, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	state.apply(subagentPanelStart("a", "E1", "research", "a"))
	state.apply(subagentPanelStart("b", "E1", "research", "b"))
	now = now.Add(time.Second)
	if !state.apply(subagentPanelEvent{Phase: subagentPanelEventCancelled, ID: "a", EvalID: "E1"}) {
		t.Fatal("interrupted child did not cancel its row")
	}
	snapshot := state.snapshot()
	if snapshot.Cancelled != 1 || !snapshot.Running || snapshot.Done != 1 {
		t.Fatalf("partially interrupted snapshot = %#v", snapshot)
	}
}

func TestSubagentPanelFinalizeCancelsOnlyRunningRows(t *testing.T) {
	now := time.Unix(400, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	state.apply(subagentPanelStart("done", "E1", "review", "finished"))
	state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "done", EvalID: "E1", Duration: time.Second})
	state.apply(subagentPanelStart("live", "E1", "test", "running"))
	now = now.Add(2500 * time.Millisecond)
	if !state.finalizeRunning() {
		t.Fatal("finalize reported no change")
	}
	snapshot := state.snapshot()
	if snapshot.Running || snapshot.Cancelled != 1 || snapshot.Done != 2 {
		t.Fatalf("finalized counts = %#v", snapshot)
	}
	if snapshot.Phases[0].Records[0].Status != subagentPanelDone || snapshot.Phases[0].Records[1].Status != subagentPanelCancelled {
		t.Fatalf("finalized records = %#v", snapshot.Phases[0].Records)
	}
	if snapshot.Phases[0].Records[1].Elapsed != 2500*time.Millisecond {
		t.Fatalf("cancelled elapsed = %s", snapshot.Phases[0].Records[1].Elapsed)
	}
	if state.finalizeRunning() {
		t.Fatal("second finalize unexpectedly changed terminal rows")
	}
}

func TestSubagentPanelBoundsEvictTerminalBeforeDroppingLiveWork(t *testing.T) {
	now := time.Unix(500, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{MaxPhases: 2, MaxEntries: 3})
	state.apply(subagentPanelStart("old", "E1", "review", "old"))
	state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "old", EvalID: "E1", Duration: time.Second})
	state.apply(subagentPanelStart("b", "E2", "review", "b"))
	state.apply(subagentPanelStart("c", "E3", "review", "c"))
	snapshot := state.snapshot()
	if len(snapshot.Phases) != 2 || snapshot.Phases[0].EvalID != "E2" || snapshot.Phases[1].EvalID != "E3" {
		t.Fatalf("phase eviction = %#v", snapshot.Phases)
	}
	if snapshot.Phases[0].Index != 2 || snapshot.Phases[1].Index != 3 {
		t.Fatalf("retained phase ordinals changed after eviction: %#v", snapshot.Phases)
	}
	state.apply(subagentPanelStart("d", "E3", "review", "d"))
	if state.apply(subagentPanelStart("e", "E3", "review", "e")) {
		t.Fatal("capacity overflow evicted live work")
	}
	snapshot = state.snapshot()
	if snapshot.Total != 3 || snapshot.Dropped != 1 {
		t.Fatalf("bounded snapshot = %#v", snapshot)
	}

	entryState := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{MaxPhases: 4, MaxEntries: 2})
	entryState.apply(subagentPanelStart("terminal", "P1", "review", "terminal"))
	entryState.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "terminal", EvalID: "P1", Duration: time.Second})
	entryState.apply(subagentPanelStart("live", "P2", "review", "live"))
	if !entryState.apply(subagentPanelStart("replacement", "P3", "review", "replacement")) {
		t.Fatal("terminal row was not reclaimed for new work")
	}
	entrySnapshot := entryState.snapshot()
	if entrySnapshot.Total != 2 || len(entrySnapshot.Phases) != 2 || entrySnapshot.Phases[0].EvalID != "P2" {
		t.Fatalf("terminal entry reclamation = %#v", entrySnapshot)
	}
}

func TestSubagentPanelSanitizesAndBoundsUntrustedText(t *testing.T) {
	now := time.Unix(600, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{MaxLabelRunes: 40, MaxErrorRunes: 24, MaxModelRunes: 12})
	state.reset("model\x1b[31m\n" + strings.Repeat("m", 40))
	state.apply(subagentPanelEvent{
		Phase: subagentPanelEventStart, ID: "a", EvalID: "E1", SubagentType: "research\x1b[2J",
		Label: "first\nsecond\u202e" + strings.Repeat("x", 200),
	})
	state.apply(subagentPanelEvent{Phase: subagentPanelEventError, ID: "a", EvalID: "E1", Error: "boom\r\n\x1b[31m" + strings.Repeat("z", 200)})
	snapshot := state.snapshot()
	record := snapshot.Phases[0].Records[0]
	for name, value := range map[string]string{"model": snapshot.ModelLabel, "label": record.Label, "error": record.Error} {
		if strings.ContainsAny(value, "\x1b\r\n") {
			t.Fatalf("%s contains terminal control characters: %q", name, value)
		}
	}
	if len([]rune(record.Label)) > 40 || len([]rune(record.Error)) > 24 || len([]rune(snapshot.ModelLabel)) > 12 {
		t.Fatalf("unbounded sanitized values: model=%q record=%#v", snapshot.ModelLabel, record)
	}
	view := renderSubagentPanel(snapshot, 100, unicodeUIGlyphs)
	if strings.Contains(view, "\x1b") || strings.Contains(view, "\u202e") {
		t.Fatalf("unsafe terminal content reached render: %q", view)
	}
}

func TestSubagentPanelRenderIsBoundedAndHasASCIIChrome(t *testing.T) {
	now := time.Unix(700, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{MaxBodyLines: 5, MaxRenderWidth: 80})
	state.reset("model")
	for index := 0; index < 20; index++ {
		id := fmt.Sprintf("worker-%02d", index)
		state.apply(subagentPanelStart(id, "E1", "research", strings.Repeat("task ", 100)))
	}
	view := renderSubagentPanel(state.snapshot(), 400, asciiUIGlyphs)
	lines := strings.Split(view, "\n")
	if len(lines) > 6 {
		t.Fatalf("rendered lines = %d, want at most 6\n%s", len(lines), view)
	}
	for _, line := range lines {
		if width := ansi.StringWidth(line); width > 80 {
			t.Fatalf("line width = %d, want <= 80: %q", width, line)
		}
	}
	for _, forbidden := range []string{"✓", "✗", "○", "●", "•", "›", "▸", "▾", "…", "⠋"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("ASCII render contains %q:\n%s", forbidden, view)
		}
	}
	if !strings.Contains(view, "dynamic subagents") || !strings.Contains(view, "more") {
		t.Fatalf("render missing intended summary:\n%s", view)
	}
	narrow := renderSubagentPanel(state.snapshot(), 32, asciiUIGlyphs)
	for _, line := range strings.Split(narrow, "\n") {
		if ansi.StringWidth(line) > 32 {
			t.Fatalf("narrow line overflows: %q", line)
		}
	}
}

func TestSubagentPanelTickAdvancesHeaderAndRowSpinner(t *testing.T) {
	now := time.Unix(750, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	state.apply(subagentPanelStart("a", "E1", "research", "work"))
	before := renderSubagentPanel(state.snapshot(), 100, asciiUIGlyphs)
	if !state.tick() {
		t.Fatal("live panel refused a tick")
	}
	after := renderSubagentPanel(state.snapshot(), 100, asciiUIGlyphs)
	if before == after || !strings.Contains(before, "(-)") || !strings.Contains(after, "(\\)") {
		t.Fatalf("spinner did not advance consistently:\nbefore=%s\nafter=%s", before, after)
	}
	state.finalizeRunning()
	if state.tick() {
		t.Fatal("terminal panel accepted a tick")
	}
}

func TestSubagentPanelPhaseElapsedUsesWallClockSpan(t *testing.T) {
	now := time.Unix(800, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
	state.apply(subagentPanelStart("a", "E1", "research", "a"))
	now = now.Add(2 * time.Second)
	state.apply(subagentPanelStart("b", "E1", "research", "b"))
	state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "a", EvalID: "E1", Duration: 3 * time.Second})
	state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: "b", EvalID: "E1", Duration: 3 * time.Second})
	if got := state.snapshot().Phases[0].Elapsed; got != 5*time.Second {
		t.Fatalf("phase elapsed = %s, want 5s", got)
	}
}

func TestSubagentPanelReductionIsRepeatable(t *testing.T) {
	now := time.Unix(900, 0)
	events := []subagentPanelEvent{
		subagentPanelStart("a", "E1", "research", "a"),
		subagentPanelStart("b", "E1", "review", "b"),
		{Phase: subagentPanelEventComplete, ID: "a", EvalID: "E1", Duration: time.Second},
		{Phase: subagentPanelEventError, ID: "b", EvalID: "E1", Duration: 2 * time.Second, Error: "failed"},
		subagentPanelStart("c", "E2", "test", "c"),
	}
	reduce := func() subagentPanelSnapshot {
		state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{})
		state.reset("model")
		for _, event := range events {
			state.apply(event)
		}
		state.handleKey("up")
		state.tick()
		return state.snapshot()
	}
	first, second := reduce(), reduce()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same event sequence reduced differently:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestSubagentPanelConcurrentApplySnapshotAndFinalize(t *testing.T) {
	now := time.Unix(1000, 0)
	state := newSubagentPanelState(func() time.Time { return now }, subagentPanelOptions{MaxEntries: 128})
	const count = 64
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			state.apply(subagentPanelStart(fmt.Sprintf("worker-%02d", index), "E1", "research", "work"))
		}()
	}
	wait.Wait()
	stopSnapshots := make(chan struct{})
	var snapshotWait sync.WaitGroup
	snapshotWait.Add(1)
	go func() {
		defer snapshotWait.Done()
		for {
			select {
			case <-stopSnapshots:
				return
			default:
				_ = state.snapshot()
				state.tick()
			}
		}
	}()
	for index := 0; index < count; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			state.apply(subagentPanelEvent{Phase: subagentPanelEventComplete, ID: fmt.Sprintf("worker-%02d", index), EvalID: "E1", Duration: time.Second})
		}()
	}
	wait.Wait()
	close(stopSnapshots)
	snapshotWait.Wait()
	snapshot := state.snapshot()
	if snapshot.Total != count || snapshot.Done != count || snapshot.Running || snapshot.Dropped != 0 {
		t.Fatalf("concurrent snapshot = %#v", snapshot)
	}
}

func TestSubagentPanelInvalidInputsAndUsefulDefaults(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil clock did not panic")
		}
	}()
	state := newSubagentPanelState(time.Now, subagentPanelOptions{})
	options := state.options
	if options.MaxPhases != defaultSubagentPanelMaxPhases || options.MaxEntries != defaultSubagentPanelMaxEntries || options.MaxBodyLines != defaultSubagentPanelMaxBodyLines {
		t.Fatalf("zero-value options = %#v", options)
	}
	clamped := newSubagentPanelState(time.Now, subagentPanelOptions{
		MaxPhases: 1 << 30, MaxEntries: 1 << 30, MaxBodyLines: 1 << 30, MaxRenderWidth: 1 << 30,
		MaxLabelRunes: 1 << 30, MaxErrorRunes: 1 << 30, MaxModelRunes: 1 << 30,
	}).options
	if clamped.MaxPhases != maximumSubagentPanelPhases || clamped.MaxEntries != maximumSubagentPanelEntries ||
		clamped.MaxBodyLines != maximumSubagentPanelBodyLines || clamped.MaxRenderWidth != maximumSubagentPanelRenderWidth ||
		clamped.MaxLabelRunes != maximumSubagentPanelLabelRunes || clamped.MaxErrorRunes != maximumSubagentPanelErrorRunes || clamped.MaxModelRunes != maximumSubagentPanelModelRunes {
		t.Fatalf("oversized options were not clamped: %#v", clamped)
	}
	for _, event := range []subagentPanelEvent{
		{Phase: subagentPanelEventStart},
		{Phase: subagentPanelEventInvalid, ID: "a"},
		{Phase: subagentPanelEventStart, ID: "bad\nidentifier"},
	} {
		if state.apply(event) {
			t.Fatalf("invalid event changed state: %#v", event)
		}
	}
	if state.handleKey("ctrl+g") || state.tick() {
		t.Fatal("hidden idle panel handled a key or tick")
	}
	_ = newSubagentPanelState(nil, subagentPanelOptions{})
}

func subagentPanelStart(id, evalID, subagentType, label string) subagentPanelEvent {
	return subagentPanelEvent{Phase: subagentPanelEventStart, ID: id, EvalID: evalID, SubagentType: subagentType, Label: label, Description: label}
}
