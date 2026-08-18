package dacode

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	defaultDebugConsoleCapacity = 1_000
	maxDebugConsoleCapacity     = 1_000
	maxDebugConsoleMessageBytes = 16 << 10
	maxDebugConsoleLoggerBytes  = 512
	maxDebugConsoleAttributes   = 32
	maxDebugConsoleKeyBytes     = 128
	maxDebugConsoleValueBytes   = 2 << 10
	maxDebugSnapshotFields      = 64
	maxDebugSnapshotLabelBytes  = 128
	maxDebugSnapshotValueBytes  = 4 << 10
	debugFallbackLevelBucket    = "_other"
)

var debugConsoleLevelNumbers = map[string]int{
	"DEBUG": int(slog.LevelDebug), "INFO": int(slog.LevelInfo),
	"WARNING": int(slog.LevelWarn), "ERROR": int(slog.LevelError), "CRITICAL": int(slog.LevelError + 4),
}

type debugConsoleAttribute struct{ Key, Value string }

// debugConsoleRecord is one bounded structured entry. Index is the absolute
// emission cursor and remains stable when older records are evicted.
type debugConsoleRecord struct {
	Index       uint64
	Time        time.Time
	Level       string
	LevelNumber int
	Logger      string
	Message     string
	Attributes  []debugConsoleAttribute
}

func (record debugConsoleRecord) plainLine() string {
	timestamp := "--:--:--"
	if !record.Time.IsZero() {
		timestamp = record.Time.Local().Format("15:04:05")
	}
	level := safeDebugConsoleText(record.Level, false, 64)
	logger := safeDebugConsoleText(record.Logger, false, maxDebugConsoleLoggerBytes)
	message := safeDebugConsoleText(record.Message, true, maxDebugConsoleMessageBytes)
	var line strings.Builder
	fmt.Fprintf(&line, "%s %-8s %s %s", timestamp, level, logger, message)
	for index, attribute := range record.Attributes {
		if index >= maxDebugConsoleAttributes {
			break
		}
		key := safeDebugConsoleText(attribute.Key, false, maxDebugConsoleKeyBytes)
		value := safeDebugConsoleText(attribute.Value, false, maxDebugConsoleValueBytes)
		if sensitiveDebugConsoleKey(key) {
			value = "[REDACTED]"
		}
		if key != "" {
			fmt.Fprintf(&line, " %s=%q", key, value)
		}
	}
	return boundDebugConsoleText(line.String(), maxDebugConsoleMessageBytes+(8<<10))
}

type debugConsoleEntry struct {
	sequence uint64
	record   debugConsoleRecord
}

type debugConsoleRing struct {
	entries []debugConsoleEntry
	start   int
}

func (ring *debugConsoleRing) append(entry debugConsoleEntry, capacity int) {
	if len(ring.entries) < capacity {
		ring.entries = append(ring.entries, entry)
		return
	}
	ring.entries[ring.start] = entry
	ring.start = (ring.start + 1) % capacity
}

func (ring *debugConsoleRing) since(index uint64) []debugConsoleEntry {
	result := make([]debugConsoleEntry, 0, len(ring.entries))
	for offset := 0; offset < len(ring.entries); offset++ {
		entry := ring.entries[(ring.start+offset)%len(ring.entries)]
		if entry.sequence >= index {
			result = append(result, entry)
		}
	}
	return result
}

type debugConsoleBuffer struct {
	mu       sync.Mutex
	capacity int
	total    uint64
	buckets  map[string]*debugConsoleRing
}

// newDebugConsoleBuffer constructs a per-level ring without performing I/O.
// Zero selects the pinned 1,000-record capacity; invalid static bounds panic.
func newDebugConsoleBuffer(capacity int) *debugConsoleBuffer {
	if capacity == 0 {
		capacity = defaultDebugConsoleCapacity
	}
	if capacity < 1 || capacity > maxDebugConsoleCapacity {
		panic("dacode: debug console capacity is out of range")
	}
	return &debugConsoleBuffer{capacity: capacity, buckets: make(map[string]*debugConsoleRing, len(debugConsoleLevelNumbers)+1)}
}

// append retains a detached, bounded copy and never exposes caller-owned
// attribute storage to concurrent readers.
func (buffer *debugConsoleBuffer) append(record debugConsoleRecord) {
	buffer.requireInitialized()
	record = normalizeDebugConsoleRecord(record)
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	// An exhausted cursor is a permanent fail-closed state. Do not silently
	// wrap and make an old cursor observe a new record as historical data.
	if buffer.total == math.MaxUint64 {
		return
	}
	record.Index = buffer.total
	bucketName := debugConsoleRetentionBucket(record.Level)
	bucket := buffer.buckets[bucketName]
	if bucket == nil {
		bucket = &debugConsoleRing{}
		buffer.buckets[bucketName] = bucket
	}
	bucket.append(debugConsoleEntry{sequence: buffer.total, record: record}, buffer.capacity)
	buffer.total++
}

type debugConsoleBufferSnapshot struct {
	Records []debugConsoleRecord
	Next    uint64
	Dropped uint64
}

func (buffer *debugConsoleBuffer) snapshotSince(index uint64) debugConsoleBufferSnapshot {
	buffer.requireInitialized()
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	entries := make([]debugConsoleEntry, 0, len(buffer.buckets)*buffer.capacity)
	for _, bucket := range buffer.buckets {
		entries = append(entries, bucket.since(index)...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].sequence < entries[j].sequence })
	snapshot := debugConsoleBufferSnapshot{Next: buffer.total}
	if index < buffer.total {
		expected := buffer.total - index
		if retained := uint64(len(entries)); retained < expected {
			snapshot.Dropped = expected - retained
		}
	}
	for _, entry := range entries {
		snapshot.Records = append(snapshot.Records, cloneDebugConsoleRecord(entry.record))
	}
	return snapshot
}

func (buffer *debugConsoleBuffer) totalEmitted() uint64 {
	buffer.requireInitialized()
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.total
}

func (buffer *debugConsoleBuffer) requireInitialized() {
	if buffer == nil || buffer.capacity < 1 || buffer.buckets == nil {
		panic("dacode: initialized debug console buffer is required")
	}
}

func debugConsoleRetentionBucket(level string) string {
	if _, exists := debugConsoleLevelNumbers[level]; exists {
		return level
	}
	return debugFallbackLevelBucket
}

func normalizeDebugConsoleRecord(record debugConsoleRecord) debugConsoleRecord {
	if record.Time.IsZero() {
		record.Time = time.Now()
	}
	record.Level = strings.ToUpper(boundDebugConsoleText(record.Level, 64))
	if record.Level == "WARN" {
		record.Level = "WARNING"
	}
	if record.Level == "" {
		record.Level = debugConsoleLevelName(record.LevelNumber)
	}
	record.Logger = boundDebugConsoleText(record.Logger, maxDebugConsoleLoggerBytes)
	if record.Logger == "" {
		record.Logger = "dacode"
	}
	record.Message = boundDebugConsoleText(record.Message, maxDebugConsoleMessageBytes)
	if len(record.Attributes) > maxDebugConsoleAttributes {
		record.Attributes = record.Attributes[:maxDebugConsoleAttributes]
	}
	attributes := make([]debugConsoleAttribute, 0, len(record.Attributes))
	for _, attribute := range record.Attributes {
		key := boundDebugConsoleText(attribute.Key, maxDebugConsoleKeyBytes)
		if key == "" {
			continue
		}
		value := boundDebugConsoleText(attribute.Value, maxDebugConsoleValueBytes)
		if sensitiveDebugConsoleKey(key) {
			value = "[REDACTED]"
		}
		attributes = append(attributes, debugConsoleAttribute{Key: key, Value: value})
	}
	record.Attributes = attributes
	return record
}

func cloneDebugConsoleRecord(record debugConsoleRecord) debugConsoleRecord {
	record.Attributes = append([]debugConsoleAttribute(nil), record.Attributes...)
	return record
}

func debugConsoleLevelName(level int) string {
	for name, number := range debugConsoleLevelNumbers {
		if number == level {
			return name
		}
	}
	return fmt.Sprintf("LEVEL%d", level)
}

type debugConsoleHandler struct {
	buffer *debugConsoleBuffer
	logger string
	level  slog.Level
	attrs  []slog.Attr
	groups []string
}

func newDebugConsoleHandler(buffer *debugConsoleBuffer, logger string, minimum slog.Level) *debugConsoleHandler {
	buffer.requireInitialized()
	logger = strings.TrimSpace(logger)
	if logger == "" {
		panic("dacode: debug console logger name is required")
	}
	return &debugConsoleHandler{buffer: buffer, logger: logger, level: minimum}
}

func (handler *debugConsoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return handler != nil && handler.buffer != nil && level >= handler.level
}

func (handler *debugConsoleHandler) Handle(_ context.Context, record slog.Record) error {
	if handler == nil || handler.buffer == nil {
		panic("dacode: initialized debug console handler is required")
	}
	attributes := make([]debugConsoleAttribute, 0, maxDebugConsoleAttributes)
	for _, attribute := range handler.attrs {
		appendDebugConsoleSlogAttribute(&attributes, handler.groups, attribute, 0)
	}
	record.Attrs(func(attribute slog.Attr) bool {
		appendDebugConsoleSlogAttribute(&attributes, handler.groups, attribute, 0)
		return len(attributes) < maxDebugConsoleAttributes
	})
	handler.buffer.append(debugConsoleRecord{
		Time: record.Time, Level: debugConsoleLevelName(int(record.Level)), LevelNumber: int(record.Level),
		Logger: handler.logger, Message: record.Message, Attributes: attributes,
	})
	return nil
}

func (handler *debugConsoleHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	if handler == nil || handler.buffer == nil {
		panic("dacode: initialized debug console handler is required")
	}
	copyHandler := *handler
	copyHandler.attrs = append(append([]slog.Attr(nil), handler.attrs...), attributes...)
	if len(copyHandler.attrs) > maxDebugConsoleAttributes {
		copyHandler.attrs = copyHandler.attrs[:maxDebugConsoleAttributes]
	}
	return &copyHandler
}

func (handler *debugConsoleHandler) WithGroup(name string) slog.Handler {
	if handler == nil || handler.buffer == nil {
		panic("dacode: initialized debug console handler is required")
	}
	copyHandler := *handler
	name = boundDebugConsoleText(name, maxDebugConsoleKeyBytes)
	if name != "" && len(copyHandler.groups) < 8 {
		copyHandler.groups = append(append([]string(nil), handler.groups...), name)
	}
	return &copyHandler
}

func appendDebugConsoleSlogAttribute(destination *[]debugConsoleAttribute, groups []string, attribute slog.Attr, depth int) {
	if len(*destination) >= maxDebugConsoleAttributes || depth > 8 {
		return
	}
	attribute.Value = resolveDebugConsoleValue(attribute.Value)
	if attribute.Value.Kind() == slog.KindGroup {
		nextGroups := groups
		if attribute.Key != "" {
			nextGroups = append(append([]string(nil), groups...), attribute.Key)
		}
		for _, child := range attribute.Value.Group() {
			appendDebugConsoleSlogAttribute(destination, nextGroups, child, depth+1)
		}
		return
	}
	keyParts := append(append([]string(nil), groups...), attribute.Key)
	key := boundDebugConsoleText(strings.Join(keyParts, "."), maxDebugConsoleKeyBytes)
	if key == "" {
		return
	}
	value := debugConsoleSlogValue(attribute.Value)
	if sensitiveDebugConsoleKey(key) {
		value = "[REDACTED]"
	}
	*destination = append(*destination, debugConsoleAttribute{Key: key, Value: value})
}

func resolveDebugConsoleValue(value slog.Value) (resolved slog.Value) {
	defer func() {
		if recover() != nil {
			resolved = slog.StringValue("[UNAVAILABLE]")
		}
	}()
	return value.Resolve()
}

func debugConsoleSlogValue(value slog.Value) (result string) {
	defer func() {
		if recover() != nil {
			result = "[UNAVAILABLE]"
		}
	}()
	return boundDebugConsoleText(value.String(), maxDebugConsoleValueBytes)
}

func sensitiveDebugConsoleKey(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
	for _, marker := range []string{"authorization", "password", "passwd", "secret", "token", "apikey", "cookie"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

type debugConsoleFilter string

const (
	debugFilterAll debugConsoleFilter = "all"
)

func parseDebugConsoleFilter(value string) (debugConsoleFilter, bool) {
	if value == string(debugFilterAll) {
		return debugFilterAll, true
	}
	mode, level, exists := strings.Cut(value, ":")
	if !exists || (mode != "min" && mode != "only") {
		return "", false
	}
	if _, valid := debugConsoleLevelNumbers[level]; !valid {
		return "", false
	}
	return debugConsoleFilter(value), true
}

func debugConsoleRecordMatches(record debugConsoleRecord, filter debugConsoleFilter) bool {
	if filter == debugFilterAll {
		return true
	}
	mode, level, exists := strings.Cut(string(filter), ":")
	threshold, valid := debugConsoleLevelNumbers[level]
	if !exists || !valid {
		return true
	}
	if mode == "only" {
		return record.Level == level
	}
	if mode != "min" {
		return true
	}
	return record.LevelNumber >= threshold
}

func filterDebugConsoleRecords(records []debugConsoleRecord, filter debugConsoleFilter) []debugConsoleRecord {
	result := make([]debugConsoleRecord, 0, len(records))
	for _, record := range records {
		if debugConsoleRecordMatches(record, filter) {
			result = append(result, cloneDebugConsoleRecord(record))
		}
	}
	return result
}

type debugConsoleSnapshotField struct {
	Label, Value string
	Copyable     bool
}

func renderDebugConsoleSnapshot(fields []debugConsoleSnapshotField) string {
	if len(fields) == 0 {
		return "(no session data)"
	}
	if len(fields) > maxDebugSnapshotFields {
		fields = fields[:maxDebugSnapshotFields]
	}
	labels := make([]string, len(fields))
	values := make([]string, len(fields))
	width := 0
	for index, field := range fields {
		labels[index] = safeDebugConsoleText(field.Label, false, maxDebugSnapshotLabelBytes)
		values[index] = safeDebugConsoleText(field.Value, false, maxDebugSnapshotValueBytes)
		if size := utf8.RuneCountInString(labels[index]); size > width {
			width = size
		}
	}
	var result strings.Builder
	for index := range fields {
		if index > 0 {
			result.WriteByte('\n')
		}
		fmt.Fprintf(&result, "%-*s  %s", width, labels[index], values[index])
	}
	return boundDebugConsoleText(result.String(), maxDebugSnapshotFields*(maxDebugSnapshotLabelBytes+maxDebugSnapshotValueBytes+4))
}

func safeDebugConsoleText(value string, keepNewlines bool, limit int) string {
	// Bound before escaping as well as after it. Terminal-safe expansion can
	// make control-heavy input several times larger than the source string.
	value = boundDebugConsoleText(value, limit)
	if !keepNewlines {
		value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	}
	return boundDebugConsoleText(unicodesecurity.RenderTerminalSafe(value), limit)
}

func boundDebugConsoleText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

type debugConsoleFocus uint8

const (
	debugFocusFilter debugConsoleFocus = iota
	debugFocusCopyToggle
	debugFocusLog
)

type debugConsoleKeyAction string

const (
	debugKeyNoAction       debugConsoleKeyAction = "none"
	debugKeyClose          debugConsoleKeyAction = "close"
	debugKeyClear          debugConsoleKeyAction = "clear"
	debugKeyCopyVisible    debugConsoleKeyAction = "copy_visible"
	debugKeyCopySelected   debugConsoleKeyAction = "copy_selected"
	debugKeyFocusChanged   debugConsoleKeyAction = "focus_changed"
	debugKeySelectionMoved debugConsoleKeyAction = "selection_moved"
	debugKeyFilterNext     debugConsoleKeyAction = "filter_next"
	debugKeyFilterPrevious debugConsoleKeyAction = "filter_previous"
	debugKeyFilterDismiss  debugConsoleKeyAction = "filter_dismiss"
)

type debugConsoleViewState struct {
	RenderedUpto   uint64
	ClearedUpto    uint64
	Filter         debugConsoleFilter
	Focus          debugConsoleFocus
	Selected       int
	VisibleCount   int
	FilterExpanded bool
}

func newDebugConsoleViewState(clearedUpto, currentTotal uint64) debugConsoleViewState {
	if clearedUpto > currentTotal {
		clearedUpto = currentTotal
	}
	return debugConsoleViewState{
		RenderedUpto: clearedUpto, ClearedUpto: clearedUpto,
		Filter: debugFilterAll, Focus: debugFocusFilter, Selected: -1,
	}
}

func (state debugConsoleViewState) afterPoll(next uint64, visibleCount int) debugConsoleViewState {
	state.validate()
	if next < state.RenderedUpto || visibleCount < 0 {
		panic("dacode: invalid debug console poll state")
	}
	state.RenderedUpto = next
	state.VisibleCount = visibleCount
	if visibleCount == 0 {
		state.Selected = -1
	} else if state.Selected >= visibleCount {
		state.Selected = visibleCount - 1
	}
	return state
}

func (state debugConsoleViewState) clear(currentTotal uint64) debugConsoleViewState {
	state.validate()
	if currentTotal < state.RenderedUpto {
		currentTotal = state.RenderedUpto
	}
	state.RenderedUpto, state.ClearedUpto = currentTotal, currentTotal
	state.VisibleCount, state.Selected = 0, -1
	return state
}

func (state debugConsoleViewState) withFilter(filter debugConsoleFilter, visibleCount int) debugConsoleViewState {
	state.validate()
	if _, valid := parseDebugConsoleFilter(string(filter)); !valid || visibleCount < 0 {
		panic("dacode: invalid debug console filter state")
	}
	state.Filter, state.VisibleCount, state.Selected = filter, visibleCount, -1
	return state
}

func (state debugConsoleViewState) validate() {
	if state.VisibleCount < 0 || state.Selected < -1 || state.Selected >= state.VisibleCount ||
		state.Focus > debugFocusLog || state.ClearedUpto > state.RenderedUpto {
		panic("dacode: invalid debug console view state")
	}
	if _, valid := parseDebugConsoleFilter(string(state.Filter)); !valid {
		panic("dacode: invalid debug console view filter")
	}
}

func reduceDebugConsoleKey(state debugConsoleViewState, key string, currentTotal uint64) (debugConsoleViewState, debugConsoleKeyAction) {
	state.validate()
	key = strings.ToLower(key)
	if state.FilterExpanded {
		switch key {
		case "tab":
			return state, debugKeyFilterNext
		case "shift+tab":
			return state, debugKeyFilterPrevious
		case "escape", "esc":
			state.FilterExpanded = false
			return state, debugKeyFilterDismiss
		default:
			return state, debugKeyNoAction
		}
	}
	switch key {
	case "escape", "esc", "ctrl+backslash", "ctrl+\\":
		return state, debugKeyClose
	case "ctrl+l":
		return state.clear(currentTotal), debugKeyClear
	case "c":
		if state.VisibleCount > 0 {
			return state, debugKeyCopyVisible
		}
	case "enter":
		if state.Focus == debugFocusLog && state.VisibleCount > 0 {
			if state.Selected < 0 {
				state.Selected = state.VisibleCount - 1
			}
			return state, debugKeyCopySelected
		}
	case "up":
		if state.Focus == debugFocusLog && state.VisibleCount > 0 {
			if state.Selected < 0 {
				state.Selected = state.VisibleCount - 1
			} else if state.Selected > 0 {
				state.Selected--
			}
			return state, debugKeySelectionMoved
		}
	case "down":
		if state.Focus == debugFocusLog && state.VisibleCount > 0 {
			if state.Selected < 0 {
				state.Selected = 0
			} else if state.Selected < state.VisibleCount-1 {
				state.Selected++
			}
			return state, debugKeySelectionMoved
		}
	case "tab":
		state.Focus = (state.Focus + 1) % 3
		if state.Focus == debugFocusLog && state.Selected < 0 && state.VisibleCount > 0 {
			state.Selected = state.VisibleCount - 1
		}
		return state, debugKeyFocusChanged
	case "shift+tab":
		state.Focus = (state.Focus + 2) % 3
		if state.Focus == debugFocusLog && state.Selected < 0 && state.VisibleCount > 0 {
			state.Selected = state.VisibleCount - 1
		}
		return state, debugKeyFocusChanged
	}
	return state, debugKeyNoAction
}

var _ slog.Handler = (*debugConsoleHandler)(nil)
