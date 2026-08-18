package dacode

import (
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestDebugConsoleOverlayPollFilterSelectionCopyAndClear(t *testing.T) {
	buffer := newDebugConsoleBuffer(4)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	buffer.append(debugRecord("started", "INFO", int(slog.LevelInfo)))
	buffer.append(debugRecord("details", "DEBUG", int(slog.LevelDebug)))
	buffer.append(debugRecord("failed", "ERROR", int(slog.LevelError)))

	view := overlay.poll()
	if got := strings.Join(debugRecordMessages(view.Records), ","); got != "started,details,failed" {
		t.Fatalf("poll records = %q", got)
	}
	if view.State.RenderedUpto != 3 || view.State.VisibleCount != 3 {
		t.Fatalf("poll state = %#v", view.State)
	}
	if !overlay.setFilter("min:ERROR") {
		t.Fatal("valid filter rejected")
	}
	copyVisible := overlay.handleKey("c")
	if copyVisible.Action != debugKeyCopyVisible || !strings.Contains(copyVisible.CopyPayload, "failed") || strings.Contains(copyVisible.CopyPayload, "started") {
		t.Fatalf("filtered copy = %#v", copyVisible)
	}

	_ = overlay.handleKey("tab")
	_ = overlay.handleKey("tab")
	copySelected := overlay.handleKey("enter")
	if copySelected.Action != debugKeyCopySelected || !strings.Contains(copySelected.CopyPayload, "failed") {
		t.Fatalf("selected copy = %#v", copySelected)
	}
	cleared := overlay.handleKey("ctrl+l")
	if cleared.Action != debugKeyClear || cleared.ClearCursor != 3 {
		t.Fatalf("clear interaction = %#v", cleared)
	}
	if view := overlay.snapshotView(); len(view.Records) != 0 || view.Dropped != 0 || view.State.ClearedUpto != 3 {
		t.Fatalf("cleared view = %#v", view)
	}

	buffer.append(debugRecord("after clear", "ERROR", int(slog.LevelError)))
	view = overlay.poll()
	if got := strings.Join(debugRecordMessages(view.Records), ","); got != "after clear" || view.State.RenderedUpto != 4 {
		t.Fatalf("post-clear poll = %#v", view)
	}
}

func TestDebugConsoleOverlayMirrorsPerLevelRetentionAndAccountsForLoss(t *testing.T) {
	buffer := newDebugConsoleBuffer(2)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	buffer.append(debugRecord("warning", "WARNING", int(slog.LevelWarn)))
	for index := 0; index < 5; index++ {
		buffer.append(debugRecord("info"+string(rune('0'+index)), "INFO", int(slog.LevelInfo)))
		_ = overlay.poll()
	}
	view := overlay.snapshotView()
	if got := strings.Join(debugRecordMessages(view.Records), ","); got != "warning,info3,info4" {
		t.Fatalf("retained records = %q", got)
	}
	if view.Dropped != 3 {
		t.Fatalf("dropped = %d, want 3", view.Dropped)
	}

	late := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	lateView := late.poll()
	if got := strings.Join(debugRecordMessages(lateView.Records), ","); got != "warning,info3,info4" || lateView.Dropped != 3 {
		t.Fatalf("late open view = records %q dropped %d", got, lateView.Dropped)
	}
}

func TestDebugConsoleOverlaySnapshotIsBoundedSafeDetachedAndExplicitlyCopyable(t *testing.T) {
	buffer := newDebugConsoleBuffer(1)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	fields := []debugConsoleSnapshotField{
		{Label: "Thread\x1b]8;;bad\a", Value: "thread\nvalue\u202e", Copyable: true},
		{Label: "Secret", Value: "not-copyable"},
		{Label: "", Value: "ignored", Copyable: true},
	}
	view := overlay.updateSnapshot(fields)
	fields[0].Value = "mutated"
	if len(view.Fields) != 2 || strings.Contains(view.Fields[0].Label, "\x1b") || strings.Contains(view.Fields[0].Value, "\n") {
		t.Fatalf("normalized fields = %#v", view.Fields)
	}
	if value, ok := overlay.copySnapshotField(0); !ok || value == "mutated" || strings.Contains(value, "\n") {
		t.Fatalf("copyable field = %q, %t", value, ok)
	}
	if value, ok := overlay.copySnapshotField(1); ok || value != "" {
		t.Fatalf("non-copyable field leaked = %q, %t", value, ok)
	}
	if value, ok := overlay.copySnapshotField(99); ok || value != "" {
		t.Fatalf("out-of-range copy = %q, %t", value, ok)
	}
}

func TestDebugConsoleOverlayFilterMenuAndClickPreference(t *testing.T) {
	buffer := newDebugConsoleBuffer(2)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	buffer.append(debugRecord("debug", "DEBUG", int(slog.LevelDebug)))
	buffer.append(debugRecord("info", "INFO", int(slog.LevelInfo)))
	_ = overlay.poll()

	if action := overlay.handleKey("enter").Action; action != debugKeyFocusChanged || !overlay.snapshotView().State.FilterExpanded {
		t.Fatalf("open filter action = %q", action)
	}
	if action := overlay.handleKey("tab").Action; action != debugKeyFilterNext || overlay.snapshotView().FilterCursor != 1 {
		t.Fatalf("filter next action = %q", action)
	}
	if action := overlay.handleKey("enter").Action; action != debugKeyFilterApplied {
		t.Fatalf("filter apply action = %q", action)
	}
	view := overlay.snapshotView()
	if view.State.Filter != "min:DEBUG" || view.State.FilterExpanded || len(view.Records) != 2 {
		t.Fatalf("applied filter view = %#v", view)
	}

	_ = overlay.handleKey("tab")
	if action := overlay.handleKey("space").Action; action != debugKeyFocusChanged || !overlay.snapshotView().ClickToCopy {
		t.Fatalf("toggle action = %q", action)
	}
	clicked := overlay.selectRecord(0)
	if clicked.Action != debugKeySelectionMoved || !strings.Contains(clicked.CopyPayload, "debug") {
		t.Fatalf("click result = %#v", clicked)
	}
	overlay.setClickToCopy(false)
	if clicked := overlay.selectRecord(1); clicked.CopyPayload != "" {
		t.Fatalf("disabled click copied %q", clicked.CopyPayload)
	}
	if clicked := overlay.selectRecord(-1); clicked.Action != debugKeyNoAction {
		t.Fatalf("invalid click action = %q", clicked.Action)
	}
}

func TestRenderDebugConsoleOverlayIsBoundedTerminalSafeAndSupportsGlyphModes(t *testing.T) {
	buffer := newDebugConsoleBuffer(4)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{MaxBodyLines: 8, MaxRenderWidth: 80})
	overlay.updateSnapshot([]debugConsoleSnapshotField{{Label: "Model", Value: "provider:model"}})
	buffer.append(debugConsoleRecord{
		Time: time.Unix(0, 0), Level: "ERROR", LevelNumber: int(slog.LevelError), Logger: "remote\x1b[2J",
		Message: "first line\nsecond line\x1b]8;;https://bad\a\u202eevil",
	})
	view := overlay.poll()
	if result := overlay.selectRecord(0); result.Action != debugKeySelectionMoved {
		t.Fatalf("selection action = %q", result.Action)
	}
	view = overlay.snapshotView()

	unicodeView := renderDebugConsoleOverlay(view, 80, 18, unicodeUIGlyphs)
	if !strings.HasPrefix(unicodeView, "╭") || !strings.Contains(unicodeView, unicodeUIGlyphs.Cursor) {
		t.Fatalf("unicode view missing chrome:\n%s", unicodeView)
	}
	assertDebugConsoleRenderBounds(t, unicodeView, 80, 18)
	assertNoUnsafeTerminalControls(t, unicodeView)

	asciiView := renderDebugConsoleOverlay(view, 42, 14, asciiUIGlyphs)
	if !strings.HasPrefix(asciiView, "+") || strings.ContainsAny(asciiView, "╭╮╰╯│") {
		t.Fatalf("ASCII view used Unicode border:\n%s", asciiView)
	}
	assertDebugConsoleRenderBounds(t, asciiView, 42, 14)
	assertNoUnsafeTerminalControls(t, asciiView)

	if got := renderDebugConsoleOverlay(view, 3, 10, asciiUIGlyphs); got != "" {
		t.Fatalf("impossibly narrow render = %q", got)
	}
}

func TestDebugConsolePointerHitAtMapsSnapshotToolbarFilterAndRecords(t *testing.T) {
	buffer := newDebugConsoleBuffer(4)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{MaxBodyLines: 8, MaxRenderWidth: 80})
	overlay.updateSnapshot([]debugConsoleSnapshotField{
		{Label: "Thread", Value: "thread-1", Copyable: true},
		{Label: "Model", Value: "provider:model", Copyable: true},
	})
	buffer.append(debugRecord("first", "INFO", int(slog.LevelInfo)))
	buffer.append(debugRecord("second", "ERROR", int(slog.LevelError)))
	snapshot := overlay.poll()

	if hit := debugConsolePointerHitAt(snapshot, 100, 24, 4, 3); hit.Target != debugConsolePointerSnapshot || hit.Index != 1 {
		t.Fatalf("model snapshot hit = %#v", hit)
	}
	if hit := debugConsolePointerHitAt(snapshot, 100, 24, 4, 4); hit.Target != debugConsolePointerFilter {
		t.Fatalf("filter hit = %#v", hit)
	}
	if hit := debugConsolePointerHitAt(snapshot, 100, 24, 60, 4); hit.Target != debugConsolePointerCopyToggle {
		t.Fatalf("copy toggle hit = %#v", hit)
	}
	if hit := debugConsolePointerHitAt(snapshot, 100, 24, 4, 6); hit.Target != debugConsolePointerRecord || hit.Index != 1 {
		t.Fatalf("record hit = %#v", hit)
	}

	overlay.setFilterExpanded(true)
	snapshot = overlay.snapshotView()
	if hit := debugConsolePointerHitAt(snapshot, 100, 24, 4, 5); hit.Target != debugConsolePointerFilterOption || hit.Index != 0 {
		t.Fatalf("filter option hit = %#v", hit)
	}
	if hit := debugConsolePointerHitAt(snapshot, 100, 24, 0, 0); hit.Target != debugConsolePointerNone {
		t.Fatalf("border hit = %#v", hit)
	}
}

func TestDebugConsoleOverlayCopyPayloadIsBounded(t *testing.T) {
	buffer := newDebugConsoleBuffer(70)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	message := strings.Repeat("x", maxDebugConsoleMessageBytes)
	for index := 0; index < 70; index++ {
		buffer.append(debugRecord(message, "INFO", int(slog.LevelInfo)))
	}
	_ = overlay.poll()
	payload := overlay.handleKey("c").CopyPayload
	if len(payload) != maximumDebugConsoleCopyBytes || !utf8.ValidString(payload) {
		t.Fatalf("copy bytes=%d utf8=%t", len(payload), utf8.ValidString(payload))
	}
}

func TestDebugConsoleHiddenErrorCommandIsExactInertAndBounded(t *testing.T) {
	buffer := newDebugConsoleBuffer(2)
	for _, command := range []string{"/debug-error now", "/debug", "please /debug-error"} {
		result := resolveDebugConsoleHiddenCommand(command)
		if result.Action != debugConsoleNoCommandAction || result.apply(buffer) {
			t.Fatalf("command %q unexpectedly resolved: %#v", command, result)
		}
	}
	result := resolveDebugConsoleHiddenCommand("  /DEBUG-ERROR  ")
	if result.Action != debugConsoleInjectErrorAction || result.VisibleError == "" || !result.apply(buffer) {
		t.Fatalf("injection result = %#v", result)
	}
	view := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{}).poll()
	if len(view.Records) != 1 || view.Records[0].Level != "ERROR" || view.Records[0].Message != result.VisibleError {
		t.Fatalf("injected records = %#v", view.Records)
	}
}

func TestDebugConsoleOverlayConcurrentPollSnapshotAndSelection(t *testing.T) {
	buffer := newDebugConsoleBuffer(50)
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{})
	const workers, iterations = 6, 200
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for index := 0; index < iterations; index++ {
				buffer.append(debugRecord("concurrent", "INFO", int(slog.LevelInfo)))
				_ = overlay.poll()
				_ = overlay.updateSnapshot([]debugConsoleSnapshotField{{Label: "Worker", Value: string(rune('a' + worker)), Copyable: true}})
				_ = overlay.selectRecord(0)
				_ = overlay.snapshotView()
			}
		}(worker)
	}
	group.Wait()
	view := overlay.poll()
	if view.State.RenderedUpto != workers*iterations || len(view.Records) > 50 {
		t.Fatalf("concurrent view cursor=%d records=%d", view.State.RenderedUpto, len(view.Records))
	}
}

func TestDebugConsoleOverlayStaticValidationPanicsAndDynamicValidationFailsClosed(t *testing.T) {
	buffer := newDebugConsoleBuffer(1)
	for _, operation := range []func(){
		func() { _ = newDebugConsoleOverlay(nil, debugConsoleOverlayOptions{}) },
		func() { _ = newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{MaxBodyLines: -1}) },
		func() { _ = newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{MaxRenderWidth: 10}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid static input did not panic")
				}
			}()
			operation()
		}()
	}
	overlay := newDebugConsoleOverlay(buffer, debugConsoleOverlayOptions{ClearedUpto: 99})
	if overlay.snapshotView().State.ClearedUpto != 0 {
		t.Fatal("future clear cursor was not clamped")
	}
	if overlay.setFilter("minimum:error") || overlay.snapshotView().State.Filter != debugFilterAll {
		t.Fatal("invalid dynamic filter changed state")
	}
}

func assertDebugConsoleRenderBounds(t *testing.T, rendered string, width, height int) {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	if len(lines) > height {
		t.Fatalf("rendered %d lines, limit %d:\n%s", len(lines), height, rendered)
	}
	for _, line := range lines {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("rendered width %d, limit %d: %q", got, width, line)
		}
	}
}

func assertNoUnsafeTerminalControls(t *testing.T, rendered string) {
	t.Helper()
	for _, character := range rendered {
		if character < 0x20 && character != '\n' {
			t.Fatalf("unsafe terminal control U+%04X in %q", character, rendered)
		}
	}
}
