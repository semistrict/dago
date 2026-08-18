package dacode

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	defaultDebugConsoleOverlayBodyLines = 18
	maximumDebugConsoleOverlayBodyLines = 64
	defaultDebugConsoleOverlayWidth     = 100
	maximumDebugConsoleOverlayWidth     = 240
	maximumDebugConsoleCopyBytes        = 1 << 20
	debugConsoleErrorCommand            = "/debug-error"
)

var debugConsoleFilterOptions = []debugConsoleFilter{
	debugFilterAll,
	"min:DEBUG", "only:DEBUG",
	"min:INFO", "min:WARNING", "min:ERROR", "min:CRITICAL",
	"only:INFO", "only:WARNING", "only:ERROR", "only:CRITICAL",
}

const debugKeyFilterApplied debugConsoleKeyAction = "filter_applied"

type debugConsoleOverlayOptions struct {
	ClearedUpto    uint64
	ClickToCopy    bool
	MaxBodyLines   int
	MaxRenderWidth int
}

// debugConsoleOverlay is an app-neutral, bounded view model over the shared
// debug buffer. It performs no scheduling, clipboard access, or terminal I/O.
type debugConsoleOverlay struct {
	mu sync.Mutex

	buffer       *debugConsoleBuffer
	state        debugConsoleViewState
	records      []debugConsoleRecord
	snapshot     []debugConsoleSnapshotField
	dropped      uint64
	clickToCopy  bool
	filterCursor int
	maxBodyLines int
	maxWidth     int
}

// newDebugConsoleOverlay requires its backing buffer positionally. Zero-valued
// options select finite defaults; invalid static limits panic at construction.
func newDebugConsoleOverlay(buffer *debugConsoleBuffer, options debugConsoleOverlayOptions) *debugConsoleOverlay {
	buffer.requireInitialized()
	if options.MaxBodyLines == 0 {
		options.MaxBodyLines = defaultDebugConsoleOverlayBodyLines
	}
	if options.MaxRenderWidth == 0 {
		options.MaxRenderWidth = defaultDebugConsoleOverlayWidth
	}
	if options.MaxBodyLines < 1 || options.MaxBodyLines > maximumDebugConsoleOverlayBodyLines {
		panic("dacode: debug console overlay body limit is out of range")
	}
	if options.MaxRenderWidth < 20 || options.MaxRenderWidth > maximumDebugConsoleOverlayWidth {
		panic("dacode: debug console overlay width is out of range")
	}
	total := buffer.totalEmitted()
	return &debugConsoleOverlay{
		buffer:       buffer,
		state:        newDebugConsoleViewState(options.ClearedUpto, total),
		clickToCopy:  options.ClickToCopy,
		maxBodyLines: options.MaxBodyLines,
		maxWidth:     options.MaxRenderWidth,
	}
}

// poll consumes the next atomic buffer snapshot and returns a detached view.
func (overlay *debugConsoleOverlay) poll() debugConsoleOverlaySnapshot {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()

	next := overlay.buffer.snapshotSince(overlay.state.RenderedUpto)
	overlay.records = append(overlay.records, next.Records...)
	overlay.dropped = saturatingDebugConsoleAdd(overlay.dropped, next.Dropped)
	if pruned := overlay.pruneRecords(); pruned > 0 {
		overlay.dropped = saturatingDebugConsoleAdd(overlay.dropped, pruned)
	}
	visible := filterDebugConsoleRecords(overlay.records, overlay.state.Filter)
	overlay.state = overlay.state.afterPoll(next.Next, len(visible))
	return overlay.snapshotLocked(visible)
}

func (overlay *debugConsoleOverlay) pruneRecords() uint64 {
	counts := make(map[string]int, len(debugConsoleLevelNumbers)+1)
	for _, record := range overlay.records {
		counts[debugConsoleRetentionBucket(record.Level)]++
	}
	overflow := make(map[string]int, len(counts))
	for bucket, count := range counts {
		if count > overlay.buffer.capacity {
			overflow[bucket] = count - overlay.buffer.capacity
		}
	}
	if len(overflow) == 0 {
		return 0
	}
	kept := make([]debugConsoleRecord, 0, len(overlay.records))
	var pruned uint64
	for _, record := range overlay.records {
		bucket := debugConsoleRetentionBucket(record.Level)
		if overflow[bucket] > 0 {
			overflow[bucket]--
			pruned++
			continue
		}
		kept = append(kept, record)
	}
	overlay.records = kept
	return pruned
}

// updateSnapshot replaces live header data with a bounded, detached copy.
func (overlay *debugConsoleOverlay) updateSnapshot(fields []debugConsoleSnapshotField) debugConsoleOverlaySnapshot {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	overlay.snapshot = normalizeDebugConsoleSnapshot(fields)
	return overlay.snapshotLocked(nil)
}

// setFilter changes the active level filter without consuming buffer records.
func (overlay *debugConsoleOverlay) setFilter(filter debugConsoleFilter) bool {
	overlay.requireInitialized()
	if _, valid := parseDebugConsoleFilter(string(filter)); !valid {
		return false
	}
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	visible := filterDebugConsoleRecords(overlay.records, filter)
	overlay.state = overlay.state.withFilter(filter, len(visible))
	overlay.filterCursor = debugConsoleFilterIndex(filter)
	return true
}

func (overlay *debugConsoleOverlay) setFilterExpanded(expanded bool) {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	overlay.state.FilterExpanded = expanded
	if expanded {
		overlay.filterCursor = debugConsoleFilterIndex(overlay.state.Filter)
	}
}

func (overlay *debugConsoleOverlay) setClickToCopy(enabled bool) {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	overlay.clickToCopy = enabled
}

func (overlay *debugConsoleOverlay) selectFilterOption(index int) bool {
	overlay.requireInitialized()
	if index < 0 || index >= len(debugConsoleFilterOptions) {
		return false
	}
	filter := debugConsoleFilterOptions[index]
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	visible := filterDebugConsoleRecords(overlay.records, filter)
	overlay.filterCursor = index
	overlay.state.FilterExpanded = false
	overlay.state = overlay.state.withFilter(filter, len(visible))
	return true
}

type debugConsoleInteraction struct {
	Action      debugConsoleKeyAction
	CopyPayload string
	ClearCursor uint64
}

// handleKey applies deterministic keyboard behavior and returns any app-owned
// side effect as data. The caller decides how to close, copy, or persist clear.
func (overlay *debugConsoleOverlay) handleKey(key string) debugConsoleInteraction {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()

	normalized := strings.ToLower(strings.TrimSpace(key))
	if overlay.state.FilterExpanded && normalized == "enter" {
		filter := debugConsoleFilterOptions[overlay.filterCursor]
		visible := filterDebugConsoleRecords(overlay.records, filter)
		overlay.state.FilterExpanded = false
		overlay.state = overlay.state.withFilter(filter, len(visible))
		return debugConsoleInteraction{Action: debugKeyFilterApplied}
	}
	if !overlay.state.FilterExpanded && overlay.state.Focus == debugFocusFilter && (normalized == "enter" || normalized == "space") {
		overlay.state.FilterExpanded = true
		overlay.filterCursor = debugConsoleFilterIndex(overlay.state.Filter)
		return debugConsoleInteraction{Action: debugKeyFocusChanged}
	}
	if !overlay.state.FilterExpanded && overlay.state.Focus == debugFocusCopyToggle && (normalized == "enter" || normalized == "space") {
		overlay.clickToCopy = !overlay.clickToCopy
		return debugConsoleInteraction{Action: debugKeyFocusChanged}
	}

	currentTotal := overlay.buffer.totalEmitted()
	next, action := reduceDebugConsoleKey(overlay.state, normalized, currentTotal)
	overlay.state = next
	switch action {
	case debugKeyFilterNext:
		overlay.filterCursor = (overlay.filterCursor + 1) % len(debugConsoleFilterOptions)
	case debugKeyFilterPrevious:
		overlay.filterCursor = (overlay.filterCursor + len(debugConsoleFilterOptions) - 1) % len(debugConsoleFilterOptions)
	case debugKeyClear:
		overlay.records = nil
		overlay.dropped = 0
		return debugConsoleInteraction{Action: action, ClearCursor: overlay.state.ClearedUpto}
	case debugKeyCopyVisible:
		return debugConsoleInteraction{Action: action, CopyPayload: debugConsoleCopyRecords(filterDebugConsoleRecords(overlay.records, overlay.state.Filter))}
	case debugKeyCopySelected:
		visible := filterDebugConsoleRecords(overlay.records, overlay.state.Filter)
		if overlay.state.Selected >= 0 && overlay.state.Selected < len(visible) {
			return debugConsoleInteraction{Action: action, CopyPayload: debugConsoleCopyRecords(visible[overlay.state.Selected : overlay.state.Selected+1])}
		}
	}
	return debugConsoleInteraction{Action: action}
}

// selectRecord models a pointer selection. Copy data is returned only when the
// persisted click-to-copy preference is enabled.
func (overlay *debugConsoleOverlay) selectRecord(index int) debugConsoleInteraction {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	visible := filterDebugConsoleRecords(overlay.records, overlay.state.Filter)
	if index < 0 || index >= len(visible) {
		return debugConsoleInteraction{Action: debugKeyNoAction}
	}
	overlay.state.Focus = debugFocusLog
	overlay.state.Selected = index
	result := debugConsoleInteraction{Action: debugKeySelectionMoved}
	if overlay.clickToCopy {
		result.CopyPayload = debugConsoleCopyRecords(visible[index : index+1])
	}
	return result
}

// copySnapshotField returns only explicitly copyable snapshot values.
func (overlay *debugConsoleOverlay) copySnapshotField(index int) (string, bool) {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	if index < 0 || index >= len(overlay.snapshot) || !overlay.snapshot[index].Copyable || overlay.snapshot[index].Value == "" {
		return "", false
	}
	return boundDebugConsoleText(overlay.snapshot[index].Value, maximumDebugConsoleCopyBytes), true
}

func (overlay *debugConsoleOverlay) snapshotView() debugConsoleOverlaySnapshot {
	overlay.requireInitialized()
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	return overlay.snapshotLocked(nil)
}

type debugConsoleOverlaySnapshot struct {
	State        debugConsoleViewState
	Records      []debugConsoleRecord
	Fields       []debugConsoleSnapshotField
	Dropped      uint64
	ClickToCopy  bool
	FilterCursor int
	MaxBodyLines int
	MaxWidth     int
}

func (overlay *debugConsoleOverlay) snapshotLocked(visible []debugConsoleRecord) debugConsoleOverlaySnapshot {
	if visible == nil {
		visible = filterDebugConsoleRecords(overlay.records, overlay.state.Filter)
	}
	records := make([]debugConsoleRecord, len(visible))
	for index, record := range visible {
		records[index] = cloneDebugConsoleRecord(record)
	}
	fields := append([]debugConsoleSnapshotField(nil), overlay.snapshot...)
	return debugConsoleOverlaySnapshot{
		State: overlay.state, Records: records, Fields: fields, Dropped: overlay.dropped,
		ClickToCopy: overlay.clickToCopy, FilterCursor: overlay.filterCursor,
		MaxBodyLines: overlay.maxBodyLines, MaxWidth: overlay.maxWidth,
	}
}

func (overlay *debugConsoleOverlay) requireInitialized() {
	if overlay == nil || overlay.buffer == nil || overlay.maxBodyLines < 1 || overlay.maxWidth < 20 {
		panic("dacode: initialized debug console overlay is required")
	}
}

func normalizeDebugConsoleSnapshot(fields []debugConsoleSnapshotField) []debugConsoleSnapshotField {
	if len(fields) > maxDebugSnapshotFields {
		fields = fields[:maxDebugSnapshotFields]
	}
	result := make([]debugConsoleSnapshotField, 0, len(fields))
	for _, field := range fields {
		label := safeDebugConsoleText(field.Label, false, maxDebugSnapshotLabelBytes)
		value := safeDebugConsoleText(field.Value, false, maxDebugSnapshotValueBytes)
		if label == "" {
			continue
		}
		result = append(result, debugConsoleSnapshotField{Label: label, Value: value, Copyable: field.Copyable})
	}
	return result
}

func debugConsoleCopyRecords(records []debugConsoleRecord) string {
	lines := make([]string, 0, len(records))
	for _, record := range records {
		lines = append(lines, record.plainLine())
	}
	return boundDebugConsoleText(strings.Join(lines, "\n"), maximumDebugConsoleCopyBytes)
}

func debugConsoleFilterIndex(filter debugConsoleFilter) int {
	for index, candidate := range debugConsoleFilterOptions {
		if candidate == filter {
			return index
		}
	}
	return 0
}

func saturatingDebugConsoleAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

// renderDebugConsoleOverlay returns a bounded terminal-safe modal panel. It is
// deliberately styling-free so the host can compose or color it independently.
func renderDebugConsoleOverlay(snapshot debugConsoleOverlaySnapshot, width, height int, glyphs uiGlyphs) string {
	if width < 4 || height < 2 {
		return ""
	}
	maxWidth := snapshot.MaxWidth
	if maxWidth < 20 || maxWidth > maximumDebugConsoleOverlayWidth {
		maxWidth = defaultDebugConsoleOverlayWidth
	}
	outerWidth := min(width, maxWidth)
	contentWidth := max(outerWidth-4, 1)
	contentRows := height - 2
	maxBody := snapshot.MaxBodyLines
	if maxBody < 1 || maxBody > maximumDebugConsoleOverlayBodyLines {
		maxBody = defaultDebugConsoleOverlayBodyLines
	}

	lines := []string{debugConsoleCenter("Debug Console", contentWidth)}
	fieldBudget := min(len(snapshot.Fields), min(6, max(contentRows-4, 0)))
	if len(snapshot.Fields) == 0 && contentRows > 4 {
		lines = append(lines, "(no session data)")
	} else {
		fieldLines := strings.Split(renderDebugConsoleSnapshot(snapshot.Fields[:fieldBudget]), "\n")
		lines = append(lines, fieldLines...)
	}
	copyMark := "[ ]"
	if snapshot.ClickToCopy {
		copyMark = "[x]"
	}
	filterMarker, copyMarker := " ", " "
	if snapshot.State.Focus == debugFocusFilter {
		filterMarker = glyphs.Cursor
	}
	if snapshot.State.Focus == debugFocusCopyToggle {
		copyMarker = glyphs.Cursor
	}
	lines = append(lines, fmt.Sprintf("%s Level [%s]  %s %s Click to copy", filterMarker, snapshot.State.Filter, copyMarker, copyMark))
	if snapshot.Dropped > 0 {
		lines = append(lines, fmt.Sprintf("%s %d older log records unavailable", glyphs.Warning, snapshot.Dropped))
	}

	footer := "Esc close | Ctrl+L clear | c copy visible | Enter copy line"
	remaining := max(contentRows-len(lines)-1, 0)
	if snapshot.State.FilterExpanded && remaining > 0 {
		available := min(remaining, 7)
		start := panelViewportStart(snapshot.FilterCursor, len(debugConsoleFilterOptions), available)
		for index := start; index < min(start+available, len(debugConsoleFilterOptions)); index++ {
			cursor := " "
			if index == snapshot.FilterCursor {
				cursor = glyphs.Cursor
			}
			lines = append(lines, fmt.Sprintf("%s %s", cursor, debugConsoleFilterOptions[index]))
		}
	} else if remaining > 0 {
		available := min(remaining, maxBody)
		selected := snapshot.State.Selected
		start := max(len(snapshot.Records)-available, 0)
		if selected >= 0 {
			start = panelViewportStart(selected, len(snapshot.Records), available)
		}
		for index := start; index < min(start+available, len(snapshot.Records)); index++ {
			cursor := " "
			if index == selected {
				cursor = glyphs.Cursor
			}
			line := strings.NewReplacer("\r\n", " "+glyphs.Newline+" ", "\r", " "+glyphs.Newline+" ", "\n", " "+glyphs.Newline+" ").Replace(snapshot.Records[index].plainLine())
			lines = append(lines, cursor+" "+line)
		}
		if len(snapshot.Records) == 0 && available > 0 {
			lines = append(lines, "(no visible log records)")
		}
	}
	lines = append(lines, footer)
	if len(lines) > contentRows {
		lines = lines[:contentRows]
	}

	horizontal, vertical := glyphs.BoxHorizontal, glyphs.BoxVertical
	topLeft, topRight := glyphs.BoxTopLeft, glyphs.BoxTopRight
	bottomLeft, bottomRight := glyphs.BoxBottomLeft, glyphs.BoxBottomRight
	var rendered strings.Builder
	rendered.WriteString(topLeft + strings.Repeat(horizontal, outerWidth-2) + topRight)
	for _, line := range lines {
		rendered.WriteByte('\n')
		line = debugConsoleFit(line, contentWidth, glyphs.Ellipsis)
		rendered.WriteString(vertical + " " + line + strings.Repeat(" ", max(contentWidth-ansi.StringWidth(line), 0)) + " " + vertical)
	}
	rendered.WriteByte('\n')
	rendered.WriteString(bottomLeft + strings.Repeat(horizontal, outerWidth-2) + bottomRight)
	return rendered.String()
}

func debugConsoleFit(value string, width int, ellipsis string) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(unicodesecurity.RenderTerminalSafe(value), width, ellipsis)
}

func debugConsoleCenter(value string, width int) string {
	value = debugConsoleFit(value, width, "...")
	padding := max(width-ansi.StringWidth(value), 0)
	return strings.Repeat(" ", padding/2) + value
}

type debugConsolePointerTarget uint8

const (
	debugConsolePointerNone debugConsolePointerTarget = iota
	debugConsolePointerSnapshot
	debugConsolePointerFilter
	debugConsolePointerCopyToggle
	debugConsolePointerFilterOption
	debugConsolePointerRecord
)

type debugConsolePointerHit struct {
	Target debugConsolePointerTarget
	Index  int
}

// debugConsolePointerHitAt maps panel-local cell coordinates to stable model
// indices. It mirrors the bounded row allocation used by the plain renderer.
func debugConsolePointerHitAt(snapshot debugConsoleOverlaySnapshot, width, height, x, y int) debugConsolePointerHit {
	if width < 4 || height < 2 || x <= 0 || y <= 0 {
		return debugConsolePointerHit{}
	}
	maxWidth := snapshot.MaxWidth
	if maxWidth < 20 || maxWidth > maximumDebugConsoleOverlayWidth {
		maxWidth = defaultDebugConsoleOverlayWidth
	}
	outerWidth := min(width, maxWidth)
	if x >= outerWidth-1 {
		return debugConsolePointerHit{}
	}
	contentRows := height - 2
	fieldBudget := min(len(snapshot.Fields), min(6, max(contentRows-4, 0)))
	fieldRows := fieldBudget
	if len(snapshot.Fields) == 0 && contentRows > 4 || len(snapshot.Fields) > 0 && fieldBudget == 0 {
		fieldRows = 1
	}
	if y >= 2 && y < 2+fieldRows && y-2 < len(snapshot.Fields) {
		return debugConsolePointerHit{Target: debugConsolePointerSnapshot, Index: y - 2}
	}
	toolbarRow := 2 + fieldRows
	if y == toolbarRow {
		contentX := x - 2
		filterText := fmt.Sprintf("%s Level [%s]", " ", snapshot.State.Filter)
		if contentX > ansi.StringWidth(filterText)+1 {
			return debugConsolePointerHit{Target: debugConsolePointerCopyToggle}
		}
		return debugConsolePointerHit{Target: debugConsolePointerFilter}
	}
	row := toolbarRow + 1
	if snapshot.Dropped > 0 {
		row++
	}
	remaining := max(contentRows-row, 0)
	if snapshot.State.FilterExpanded {
		available := min(remaining, 7)
		start := panelViewportStart(snapshot.FilterCursor, len(debugConsoleFilterOptions), available)
		if y >= row && y < row+available {
			return debugConsolePointerHit{Target: debugConsolePointerFilterOption, Index: start + y - row}
		}
		return debugConsolePointerHit{}
	}
	maxBody := snapshot.MaxBodyLines
	if maxBody < 1 || maxBody > maximumDebugConsoleOverlayBodyLines {
		maxBody = defaultDebugConsoleOverlayBodyLines
	}
	available := min(remaining, maxBody)
	selected := snapshot.State.Selected
	start := max(len(snapshot.Records)-available, 0)
	if selected >= 0 {
		start = panelViewportStart(selected, len(snapshot.Records), available)
	}
	if y >= row && y < row+min(available, len(snapshot.Records)) {
		return debugConsolePointerHit{Target: debugConsolePointerRecord, Index: start + y - row}
	}
	return debugConsolePointerHit{}
}

type debugConsoleCommandAction string

const (
	debugConsoleNoCommandAction   debugConsoleCommandAction = "none"
	debugConsoleInjectErrorAction debugConsoleCommandAction = "inject_error"
)

type debugConsoleHiddenCommandResult struct {
	Action       debugConsoleCommandAction
	VisibleError string
	Record       debugConsoleRecord
}

// resolveDebugConsoleHiddenCommand recognizes the exact hidden injection
// command and returns inert data. It never executes arbitrary command text.
func resolveDebugConsoleHiddenCommand(command string) debugConsoleHiddenCommandResult {
	if strings.ToLower(strings.TrimSpace(command)) != debugConsoleErrorCommand {
		return debugConsoleHiddenCommandResult{Action: debugConsoleNoCommandAction}
	}
	message := "Server failed to start: RuntimeError: Server process exited with code 3"
	return debugConsoleHiddenCommandResult{
		Action:       debugConsoleInjectErrorAction,
		VisibleError: message,
		Record: debugConsoleRecord{
			Level: "ERROR", LevelNumber: int(slog.LevelError), Logger: "dacode.debug", Message: message,
		},
	}
}

// apply performs only the bounded in-memory diagnostic append. Visible error
// presentation remains an explicit caller-owned action.
func (result debugConsoleHiddenCommandResult) apply(buffer *debugConsoleBuffer) bool {
	buffer.requireInitialized()
	if result.Action != debugConsoleInjectErrorAction {
		return false
	}
	buffer.append(result.Record)
	return true
}
