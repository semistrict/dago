package dacode

import (
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDebugConsoleBufferRetainsNewestPerLevelInChronologicalOrder(t *testing.T) {
	buffer := newDebugConsoleBuffer(2)
	buffer.append(debugRecord("info0", "INFO", int(slog.LevelInfo)))
	buffer.append(debugRecord("debug0", "DEBUG", int(slog.LevelDebug)))
	buffer.append(debugRecord("info1", "INFO", int(slog.LevelInfo)))
	buffer.append(debugRecord("debug1", "DEBUG", int(slog.LevelDebug)))
	buffer.append(debugRecord("info2", "INFO", int(slog.LevelInfo)))
	buffer.append(debugRecord("debug2", "DEBUG", int(slog.LevelDebug)))
	snapshot := buffer.snapshotSince(0)
	if got := debugRecordMessages(snapshot.Records); strings.Join(got, ",") != "info1,debug1,info2,debug2" {
		t.Fatalf("records = %#v", got)
	}
	if snapshot.Next != 6 || snapshot.Dropped != 2 || buffer.totalEmitted() != 6 {
		t.Fatalf("snapshot = %#v total=%d", snapshot, buffer.totalEmitted())
	}
}

func TestDebugConsoleBufferCustomLevelsShareFallbackBudget(t *testing.T) {
	buffer := newDebugConsoleBuffer(2)
	for index := 0; index < 4; index++ {
		buffer.append(debugRecord("custom"+string(rune('0'+index)), "NOTICE"+string(rune('0'+index)), 25+index))
	}
	snapshot := buffer.snapshotSince(0)
	if got := strings.Join(debugRecordMessages(snapshot.Records), ","); got != "custom2,custom3" {
		t.Fatalf("custom records = %s", got)
	}
}

func TestDebugConsoleBufferLevelFloodCannotEvictSparseHigherLevels(t *testing.T) {
	buffer := newDebugConsoleBuffer(3)
	buffer.append(debugRecord("info", "INFO", int(slog.LevelInfo)))
	buffer.append(debugRecord("warning", "WARNING", int(slog.LevelWarn)))
	buffer.append(debugRecord("error", "ERROR", int(slog.LevelError)))
	for index := 0; index < 30; index++ {
		buffer.append(debugRecord("debug"+string(rune('a'+index%26)), "DEBUG", int(slog.LevelDebug)))
	}
	snapshot := buffer.snapshotSince(0)
	if got := strings.Join(debugRecordMessages(snapshot.Records), ","); got != "info,warning,error,debugb,debugc,debugd" {
		t.Fatalf("records after debug flood = %s", got)
	}
	if snapshot.Dropped != 27 {
		t.Fatalf("dropped = %d", snapshot.Dropped)
	}
}

func TestDebugConsoleSnapshotUsesAbsoluteCursorAndDetachedRecords(t *testing.T) {
	buffer := newDebugConsoleBuffer(3)
	for index := 0; index < 3; index++ {
		buffer.append(debugConsoleRecord{
			Level: "INFO", LevelNumber: int(slog.LevelInfo), Message: "record",
			Attributes: []debugConsoleAttribute{{Key: "value", Value: string(rune('a' + index))}},
		})
	}
	first := buffer.snapshotSince(1)
	if first.Next != 3 || len(first.Records) != 2 || first.Records[0].Index != 1 {
		t.Fatalf("first snapshot = %#v", first)
	}
	first.Records[0].Attributes[0].Value = "mutated"
	second := buffer.snapshotSince(1)
	if second.Records[0].Attributes[0].Value == "mutated" {
		t.Fatal("snapshot exposed buffer-owned attributes")
	}
}

func TestDebugConsoleBufferFutureAndExhaustedCursorsNeverWrap(t *testing.T) {
	buffer := newDebugConsoleBuffer(2)
	buffer.append(debugRecord("first", "INFO", int(slog.LevelInfo)))
	future := buffer.snapshotSince(math.MaxUint64)
	if len(future.Records) != 0 || future.Next != 1 || future.Dropped != 0 {
		t.Fatalf("future snapshot = %#v", future)
	}

	buffer.mu.Lock()
	buffer.total = math.MaxUint64
	buffer.mu.Unlock()
	buffer.append(debugRecord("must not wrap", "INFO", int(slog.LevelInfo)))
	if got := buffer.totalEmitted(); got != math.MaxUint64 {
		t.Fatalf("exhausted cursor wrapped to %d", got)
	}
	if got := buffer.snapshotSince(math.MaxUint64 - 1); len(got.Records) != 0 || got.Next != math.MaxUint64 || got.Dropped != 1 {
		t.Fatalf("exhausted snapshot = %#v", got)
	}
}

func TestDebugConsoleBufferConcurrentAppendAndSnapshot(t *testing.T) {
	buffer := newDebugConsoleBuffer(50)
	const writers, perWriter = 8, 250
	var group sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < perWriter; index++ {
				buffer.append(debugRecord("concurrent", "INFO", int(slog.LevelInfo)))
				_ = buffer.snapshotSince(0)
			}
		}()
	}
	group.Wait()
	if got := buffer.totalEmitted(); got != writers*perWriter {
		t.Fatalf("total = %d", got)
	}
	snapshot := buffer.snapshotSince(0)
	if len(snapshot.Records) != 50 || snapshot.Next != writers*perWriter {
		t.Fatalf("snapshot len=%d next=%d", len(snapshot.Records), snapshot.Next)
	}
}

func TestDebugConsoleHandlerBoundsRedactsAndGroupsAttributes(t *testing.T) {
	buffer := newDebugConsoleBuffer(10)
	handler := newDebugConsoleHandler(buffer, "dacode.runtime", slog.LevelInfo)
	logger := slog.New(handler.WithGroup("request").WithAttrs([]slog.Attr{
		slog.String("api_token", "never-print-this"), slog.String("safe", strings.Repeat("v", maxDebugConsoleValueBytes+100)),
	}))
	logger.Debug("excluded")
	logger.Info("accepted", slog.Group("nested", slog.String("name", "value")))
	snapshot := buffer.snapshotSince(0)
	if len(snapshot.Records) != 1 {
		t.Fatalf("records = %#v", snapshot.Records)
	}
	line := snapshot.Records[0].plainLine()
	if strings.Contains(line, "never-print-this") || !strings.Contains(line, `request.api_token="[REDACTED]"`) || !strings.Contains(line, "request.nested.name") {
		t.Fatalf("line = %q", line)
	}
	if len(snapshot.Records[0].Attributes[1].Value) != maxDebugConsoleValueBytes {
		t.Fatalf("bounded value length = %d", len(snapshot.Records[0].Attributes[1].Value))
	}
}

func TestDebugConsoleRenderingMakesControlsInertAndStaysBounded(t *testing.T) {
	record := debugConsoleRecord{
		Time:  time.Date(2026, 8, 17, 12, 34, 56, 0, time.UTC),
		Level: "INFO\x1b[31m", LevelNumber: int(slog.LevelInfo), Logger: "log\rname",
		Message: "first\nsecond\x1b]8;;https://evil.example\a",
	}
	line := record.plainLine()
	if strings.ContainsRune(line, '\x1b') || strings.ContainsRune(line, '\r') || !strings.Contains(line, "<U+001B CONTROL>") || !strings.Contains(line, "first\nsecond") {
		t.Fatalf("unsafe line = %q", line)
	}
	fields := []debugConsoleSnapshotField{{Label: "Thread\nInjected", Value: "abc\x1b[2J"}}
	rendered := renderDebugConsoleSnapshot(fields)
	if strings.ContainsRune(rendered, '\x1b') || strings.Count(rendered, "\n") != 0 || !strings.Contains(rendered, "<U+001B CONTROL>") {
		t.Fatalf("unsafe snapshot = %q", rendered)
	}
	many := make([]debugConsoleSnapshotField, maxDebugSnapshotFields+20)
	for index := range many {
		many[index] = debugConsoleSnapshotField{Label: strings.Repeat("L", 200), Value: strings.Repeat("V", 5<<10)}
	}
	if result := renderDebugConsoleSnapshot(many); len(result) > maxDebugSnapshotFields*(maxDebugSnapshotLabelBytes+maxDebugSnapshotValueBytes+4) {
		t.Fatalf("snapshot length = %d", len(result))
	}
}

func TestDebugConsoleDirectRecordRenderingRedactsAndBoundsAttributes(t *testing.T) {
	attributes := make([]debugConsoleAttribute, maxDebugConsoleAttributes+100)
	for index := range attributes {
		attributes[index] = debugConsoleAttribute{Key: "safe", Value: strings.Repeat("v", maxDebugConsoleValueBytes+100)}
	}
	attributes[0] = debugConsoleAttribute{Key: "authorization", Value: "Bearer never-print-this"}
	record := debugConsoleRecord{
		Level: "INFO", Logger: strings.Repeat("l", maxDebugConsoleLoggerBytes+100),
		Message: strings.Repeat("\x1b", maxDebugConsoleMessageBytes+100), Attributes: attributes,
	}
	line := record.plainLine()
	if strings.Contains(line, "never-print-this") || strings.ContainsRune(line, '\x1b') || !strings.Contains(line, "[REDACTED]") {
		t.Fatalf("unsafe direct record line = %q", line)
	}
	if len(line) > maxDebugConsoleMessageBytes+(8<<10) {
		t.Fatalf("direct line length = %d", len(line))
	}
}

func TestDebugConsoleFiltersAllThresholdExactAndCustomLevels(t *testing.T) {
	info := debugRecord("info", "INFO", int(slog.LevelInfo))
	warning := debugRecord("warning", "WARNING", int(slog.LevelWarn))
	notice := debugRecord("notice", "NOTICE", 2)
	minimum, ok := parseDebugConsoleFilter("min:WARNING")
	if !ok || debugConsoleRecordMatches(info, minimum) || !debugConsoleRecordMatches(warning, minimum) {
		t.Fatal("minimum filter mismatch")
	}
	only, ok := parseDebugConsoleFilter("only:WARNING")
	if !ok || debugConsoleRecordMatches(info, only) || !debugConsoleRecordMatches(warning, only) {
		t.Fatal("only filter mismatch")
	}
	minimumInfo, _ := parseDebugConsoleFilter("min:INFO")
	if !debugConsoleRecordMatches(notice, minimumInfo) || debugConsoleRecordMatches(notice, minimum) {
		t.Fatal("custom numeric level mismatch")
	}
	if _, ok := parseDebugConsoleFilter("min:BOGUS"); ok || !debugConsoleRecordMatches(info, debugConsoleFilter("min:BOGUS")) {
		t.Fatal("invalid diagnostic filter did not fail open")
	}
	filtered := filterDebugConsoleRecords([]debugConsoleRecord{info, warning}, only)
	if len(filtered) != 1 || filtered[0].Message != "warning" {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func TestDebugConsoleClearCursorPersistsAndClampsAcrossBufferReset(t *testing.T) {
	state := newDebugConsoleViewState(7, 10).afterPoll(10, 3)
	state = state.clear(12)
	if state.ClearedUpto != 12 || state.RenderedUpto != 12 || state.VisibleCount != 0 || state.Selected != -1 {
		t.Fatalf("cleared state = %#v", state)
	}
	reopened := newDebugConsoleViewState(state.ClearedUpto, 15)
	if reopened.RenderedUpto != 12 {
		t.Fatalf("reopened cursor = %d", reopened.RenderedUpto)
	}
	reset := newDebugConsoleViewState(state.ClearedUpto, 2)
	if reset.RenderedUpto != 2 || reset.ClearedUpto != 2 {
		t.Fatalf("reset cursor = %#v", reset)
	}
}

func TestReduceDebugConsoleKeyTransitionsArePureAndBounded(t *testing.T) {
	initial := newDebugConsoleViewState(0, 10).afterPoll(10, 3)
	state, action := reduceDebugConsoleKey(initial, "shift+tab", 10)
	if action != debugKeyFocusChanged || state.Focus != debugFocusLog || state.Selected != 2 {
		t.Fatalf("reverse focus = %#v, %s", state, action)
	}
	state, action = reduceDebugConsoleKey(state, "up", 10)
	if action != debugKeySelectionMoved || state.Selected != 1 {
		t.Fatalf("up = %#v, %s", state, action)
	}
	state, action = reduceDebugConsoleKey(state, "enter", 10)
	if action != debugKeyCopySelected || state.Selected != 1 {
		t.Fatalf("enter = %#v, %s", state, action)
	}
	cleared, action := reduceDebugConsoleKey(state, "ctrl+l", 14)
	if action != debugKeyClear || cleared.ClearedUpto != 14 || cleared.VisibleCount != 0 {
		t.Fatalf("clear = %#v, %s", cleared, action)
	}
	if original := initial; original.RenderedUpto != 10 || original.Selected != -1 {
		t.Fatalf("reducer mutated input = %#v", original)
	}
	state.FilterExpanded = true
	if _, action := reduceDebugConsoleKey(state, "tab", 10); action != debugKeyFilterNext {
		t.Fatalf("expanded tab action = %s", action)
	}
	closed, action := reduceDebugConsoleKey(state, "escape", 10)
	if action != debugKeyFilterDismiss || closed.FilterExpanded {
		t.Fatalf("expanded escape = %#v, %s", closed, action)
	}
	if _, action := reduceDebugConsoleKey(initial, "ctrl+\\", 10); action != debugKeyClose {
		t.Fatalf("toggle action = %s", action)
	}
}

func TestDebugConsoleConstructorsRejectInvalidStaticInputs(t *testing.T) {
	for _, invoke := range []func(){
		func() { _ = newDebugConsoleBuffer(-1) },
		func() { _ = newDebugConsoleBuffer(maxDebugConsoleCapacity + 1) },
		func() { _ = newDebugConsoleHandler(nil, "logger", slog.LevelDebug) },
		func() { _ = newDebugConsoleHandler(newDebugConsoleBuffer(1), "", slog.LevelDebug) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid static input did not panic")
				}
			}()
			invoke()
		}()
	}
}

func debugRecord(message, level string, number int) debugConsoleRecord {
	return debugConsoleRecord{Time: time.Unix(0, 0), Level: level, LevelNumber: number, Logger: "dacode.test", Message: message}
}

func debugRecordMessages(records []debugConsoleRecord) []string {
	result := make([]string, len(records))
	for index, record := range records {
		result[index] = record.Message
	}
	return result
}
