package dacode

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	defaultSubagentPanelMaxPhases      = 32
	defaultSubagentPanelMaxEntries     = 256
	defaultSubagentPanelMaxBodyLines   = 12
	defaultSubagentPanelMaxRenderWidth = 200
	defaultSubagentPanelLabelRunes     = 200
	defaultSubagentPanelErrorRunes     = 120
	defaultSubagentPanelModelRunes     = 16
	defaultSubagentPanelIDRunes        = 128
	defaultSubagentDescriptionRunes    = 60
	maximumSubagentPanelBodyLines      = 24
	maximumSubagentPanelRenderWidth    = 240
	maximumSubagentPanelPhases         = 64
	maximumSubagentPanelEntries        = 512
	maximumSubagentPanelLabelRunes     = 512
	maximumSubagentPanelErrorRunes     = 256
	maximumSubagentPanelModelRunes     = 64
)

type subagentPanelEventPhase uint8

const (
	subagentPanelEventInvalid subagentPanelEventPhase = iota
	subagentPanelEventStart
	subagentPanelEventComplete
	subagentPanelEventError
	subagentPanelEventCancelled
)

type subagentPanelStatus uint8

const (
	subagentPanelRunning subagentPanelStatus = iota
	subagentPanelDone
	subagentPanelError
	subagentPanelCancelled
)

// subagentPanelEvent is the app-neutral input seam for a later stream adapter.
// Duration is optional; zero causes terminal events to use the injected clock.
type subagentPanelEvent struct {
	Phase        subagentPanelEventPhase
	ID           string
	EvalID       string
	SubagentType string
	Label        string
	Description  string
	Error        string
	Duration     time.Duration
}

type subagentPanelOptions struct {
	MaxPhases      int
	MaxEntries     int
	MaxBodyLines   int
	MaxRenderWidth int
	MaxLabelRunes  int
	MaxErrorRunes  int
	MaxModelRunes  int
}

func (options subagentPanelOptions) normalized() subagentPanelOptions {
	if options.MaxPhases <= 0 {
		options.MaxPhases = defaultSubagentPanelMaxPhases
	}
	options.MaxPhases = min(options.MaxPhases, maximumSubagentPanelPhases)
	if options.MaxEntries <= 0 {
		options.MaxEntries = defaultSubagentPanelMaxEntries
	}
	options.MaxEntries = min(options.MaxEntries, maximumSubagentPanelEntries)
	if options.MaxBodyLines <= 0 {
		options.MaxBodyLines = defaultSubagentPanelMaxBodyLines
	}
	options.MaxBodyLines = min(options.MaxBodyLines, maximumSubagentPanelBodyLines)
	if options.MaxRenderWidth <= 0 {
		options.MaxRenderWidth = defaultSubagentPanelMaxRenderWidth
	}
	options.MaxRenderWidth = min(options.MaxRenderWidth, maximumSubagentPanelRenderWidth)
	if options.MaxLabelRunes <= 0 {
		options.MaxLabelRunes = defaultSubagentPanelLabelRunes
	}
	options.MaxLabelRunes = min(options.MaxLabelRunes, maximumSubagentPanelLabelRunes)
	if options.MaxErrorRunes <= 0 {
		options.MaxErrorRunes = defaultSubagentPanelErrorRunes
	}
	options.MaxErrorRunes = min(options.MaxErrorRunes, maximumSubagentPanelErrorRunes)
	if options.MaxModelRunes <= 0 {
		options.MaxModelRunes = defaultSubagentPanelModelRunes
	}
	options.MaxModelRunes = min(options.MaxModelRunes, maximumSubagentPanelModelRunes)
	return options
}

type subagentPanelRecord struct {
	id        string
	label     string
	status    subagentPanelStatus
	startedAt time.Time
	duration  time.Duration
	error     string
}

type subagentPanelPhase struct {
	evalID  string
	index   int
	records map[string]*subagentPanelRecord
	order   []string
}

func (phase *subagentPanelPhase) counts() (done, total, failed, cancelled int) {
	for _, id := range phase.order {
		record := phase.records[id]
		if record == nil {
			continue
		}
		total++
		if record.status != subagentPanelRunning {
			done++
		}
		if record.status == subagentPanelError {
			failed++
		} else if record.status == subagentPanelCancelled {
			cancelled++
		}
	}
	return done, total, failed, cancelled
}

func (phase *subagentPanelPhase) allTerminal() bool {
	done, total, _, _ := phase.counts()
	return total > 0 && done == total
}

type subagentPanelState struct {
	mu      sync.RWMutex
	now     func() time.Time
	options subagentPanelOptions

	phases      map[string]*subagentPanelPhase
	phaseOrder  []string
	active      string
	activeSet   bool
	selected    string
	selectedSet bool
	modelLabel  string
	visible     bool
	expanded    bool
	spinner     uint64
	dropped     uint64
	nextPhase   int
}

// newSubagentPanelState performs no I/O. The required clock is positional so
// event reduction and timing stay deterministic under tests and replay.
func newSubagentPanelState(now func() time.Time, options subagentPanelOptions) *subagentPanelState {
	if now == nil {
		panic("dacode: subagent panel clock is required")
	}
	return &subagentPanelState{
		now: now, options: options.normalized(), phases: make(map[string]*subagentPanelPhase), expanded: true,
	}
}

// apply reduces one validated lifecycle event. It is safe for concurrent
// producers and returns whether observable panel state changed.
func (state *subagentPanelState) apply(event subagentPanelEvent) bool {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	id, ok := normalizeSubagentPanelID(event.ID)
	if !ok {
		return false
	}
	evalID, ok := normalizeSubagentPanelID(event.EvalID)
	if event.EvalID == "" {
		evalID, ok = "", true
	}
	if !ok {
		evalID = ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	switch event.Phase {
	case subagentPanelEventStart:
		return state.startLocked(id, evalID, event)
	case subagentPanelEventComplete, subagentPanelEventError, subagentPanelEventCancelled:
		return state.finishLocked(id, evalID, event)
	default:
		return false
	}
}

func (state *subagentPanelState) startLocked(id, evalID string, event subagentPanelEvent) bool {
	phase, created := state.ensurePhaseLocked(evalID)
	if phase == nil {
		state.dropped++
		return false
	}
	if _, exists := phase.records[id]; !exists && !state.reserveEntryLocked() {
		if created && len(phase.records) == 0 {
			state.removePhaseLocked(evalID)
		}
		state.dropped++
		return false
	}
	label := subagentPanelRowLabel(event, state.options.MaxLabelRunes)
	record := &subagentPanelRecord{id: id, label: label, status: subagentPanelRunning, startedAt: state.now()}
	if _, exists := phase.records[id]; !exists {
		phase.order = append(phase.order, id)
	}
	phase.records[id] = record
	state.active, state.activeSet = evalID, true
	state.visible = true
	return true
}

func (state *subagentPanelState) finishLocked(id, evalID string, event subagentPanelEvent) bool {
	record := state.findRecordLocked(id, evalID)
	if record == nil {
		if event.Phase != subagentPanelEventError {
			return false
		}
		phase, created := state.ensurePhaseLocked(evalID)
		if phase == nil {
			state.dropped++
			return false
		}
		if !state.reserveEntryLocked() {
			if created && len(phase.records) == 0 {
				state.removePhaseLocked(evalID)
			}
			state.dropped++
			return false
		}
		record = &subagentPanelRecord{
			id: id, label: subagentPanelRowLabel(event, state.options.MaxLabelRunes), status: subagentPanelRunning, startedAt: state.now(),
		}
		phase.records[id] = record
		phase.order = append(phase.order, id)
		state.active, state.activeSet = evalID, true
		state.visible = true
	}
	if record.status != subagentPanelRunning {
		return false
	}
	if event.Phase == subagentPanelEventComplete {
		record.status = subagentPanelDone
	} else if event.Phase == subagentPanelEventError {
		record.status = subagentPanelError
		record.error = sanitizeSubagentPanelText(event.Error, state.options.MaxErrorRunes)
	} else {
		record.status = subagentPanelCancelled
	}
	record.duration = event.Duration
	if record.duration <= 0 {
		record.duration = max(state.now().Sub(record.startedAt), 0)
	}
	return true
}

func (state *subagentPanelState) ensurePhaseLocked(evalID string) (*subagentPanelPhase, bool) {
	if phase := state.phases[evalID]; phase != nil {
		return phase, false
	}
	if len(state.phaseOrder) >= state.options.MaxPhases && !state.evictOldestTerminalPhaseLocked() {
		return nil, false
	}
	state.nextPhase++
	phase := &subagentPanelPhase{evalID: evalID, index: state.nextPhase, records: make(map[string]*subagentPanelRecord)}
	state.phases[evalID] = phase
	state.phaseOrder = append(state.phaseOrder, evalID)
	return phase, true
}

func (state *subagentPanelState) reserveEntryLocked() bool {
	if state.entryCountLocked() < state.options.MaxEntries {
		return true
	}
	for _, evalID := range state.phaseOrder {
		phase := state.phases[evalID]
		for _, id := range append([]string(nil), phase.order...) {
			record := phase.records[id]
			if record != nil && record.status != subagentPanelRunning {
				delete(phase.records, id)
				phase.order = removeSubagentPanelValue(phase.order, id)
				if len(phase.order) == 0 {
					state.removePhaseLocked(evalID)
				}
				return true
			}
		}
	}
	return false
}

func (state *subagentPanelState) evictOldestTerminalPhaseLocked() bool {
	for _, evalID := range append([]string(nil), state.phaseOrder...) {
		if phase := state.phases[evalID]; phase != nil && phase.allTerminal() {
			state.removePhaseLocked(evalID)
			return true
		}
	}
	return false
}

func (state *subagentPanelState) removePhaseLocked(evalID string) {
	delete(state.phases, evalID)
	state.phaseOrder = removeSubagentPanelValue(state.phaseOrder, evalID)
	if state.activeSet && state.active == evalID {
		state.activeSet = false
		if len(state.phaseOrder) > 0 {
			state.active = state.phaseOrder[len(state.phaseOrder)-1]
			state.activeSet = true
		}
	}
	if state.selectedSet && state.selected == evalID {
		state.selectedSet = false
	}
}

func (state *subagentPanelState) findRecordLocked(id, evalID string) *subagentPanelRecord {
	if phase := state.phases[evalID]; phase != nil {
		if record := phase.records[id]; record != nil {
			return record
		}
	}
	for _, key := range state.phaseOrder {
		if record := state.phases[key].records[id]; record != nil {
			return record
		}
	}
	return nil
}

func (state *subagentPanelState) entryCountLocked() int {
	total := 0
	for _, phase := range state.phases {
		total += len(phase.records)
	}
	return total
}

func (state *subagentPanelState) anyRunningLocked() bool {
	for _, phase := range state.phases {
		for _, record := range phase.records {
			if record.status == subagentPanelRunning {
				return true
			}
		}
	}
	return false
}

// tick advances the deterministic spinner only while work is live. A TUI timer
// can stop scheduling when this returns false.
func (state *subagentPanelState) tick() bool {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.anyRunningLocked() {
		return false
	}
	state.spinner++
	return true
}

// handleKey is the focus-owned keyboard seam. Ctrl+G toggles the body; phase
// navigation follows the pinned up/down and j/k behavior and clamps at bounds.
func (state *subagentPanelState) handleKey(key string) bool {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "ctrl+g" {
		if !state.visible {
			return false
		}
		state.expanded = !state.expanded
		return true
	}
	if !state.visible || !state.expanded || len(state.phaseOrder) <= 1 {
		return false
	}
	delta := 0
	switch key {
	case "down", "j":
		delta = 1
	case "up", "k":
		delta = -1
	default:
		return false
	}
	current := state.displayedKeyLocked()
	index := 0
	for candidate, evalID := range state.phaseOrder {
		if evalID == current {
			index = candidate
			break
		}
	}
	index = max(0, min(len(state.phaseOrder)-1, index+delta))
	state.selected, state.selectedSet = state.phaseOrder[index], true
	return true
}

// selectPhase is the pointer/click integration seam. The renderer's visible
// phase rows preserve snapshot order, so an app can map a clicked row back to
// this bounded index without exposing internal maps.
func (state *subagentPanelState) selectPhase(index int) bool {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.visible || !state.expanded || index < 0 || index >= len(state.phaseOrder) {
		return false
	}
	state.selected, state.selectedSet = state.phaseOrder[index], true
	return true
}

func (state *subagentPanelState) displayedKeyLocked() string {
	if state.selectedSet {
		if _, exists := state.phases[state.selected]; exists {
			return state.selected
		}
	}
	if state.activeSet {
		return state.active
	}
	if len(state.phaseOrder) > 0 {
		return state.phaseOrder[len(state.phaseOrder)-1]
	}
	return ""
}

func (state *subagentPanelState) prepareTurn(modelLabel string) {
	state.reset(modelLabel)
}

func (state *subagentPanelState) reset(modelLabel string) {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.phases = make(map[string]*subagentPanelPhase)
	state.phaseOrder = nil
	state.active, state.selected = "", ""
	state.activeSet, state.selectedSet = false, false
	state.modelLabel = sanitizeSubagentPanelText(modelLabel, state.options.MaxModelRunes)
	state.visible = false
	state.spinner = 0
	state.dropped = 0
	state.nextPhase = 0
}

func (state *subagentPanelState) finalizeRunning() bool {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	now := state.now()
	changed := false
	for _, evalID := range state.phaseOrder {
		for _, id := range state.phases[evalID].order {
			record := state.phases[evalID].records[id]
			if record.status != subagentPanelRunning {
				continue
			}
			record.status = subagentPanelCancelled
			record.duration = max(now.Sub(record.startedAt), 0)
			changed = true
		}
	}
	return changed
}

type subagentPanelRecordSnapshot struct {
	ID      string
	Label   string
	Status  subagentPanelStatus
	Elapsed time.Duration
	Error   string
}

type subagentPanelPhaseSnapshot struct {
	EvalID    string
	Index     int
	Records   []subagentPanelRecordSnapshot
	Done      int
	Total     int
	Failed    int
	Cancelled int
	Elapsed   time.Duration
	Active    bool
	Selected  bool
}

type subagentPanelSnapshot struct {
	Visible        bool
	Expanded       bool
	ModelLabel     string
	Phases         []subagentPanelPhaseSnapshot
	DisplayedPhase int
	Done           int
	Total          int
	Failed         int
	Cancelled      int
	Dropped        uint64
	Spinner        uint64
	Running        bool
	MaxBodyLines   int
	MaxRenderWidth int
}

// snapshot is the immutable render seam. It samples the clock once so every
// row and phase in one frame agrees on elapsed time.
func (state *subagentPanelState) snapshot() subagentPanelSnapshot {
	if state == nil {
		panic("dacode: subagent panel state is required")
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	now := state.now()
	displayed := state.displayedKeyLocked()
	result := subagentPanelSnapshot{
		Visible: state.visible, Expanded: state.expanded, ModelLabel: state.modelLabel,
		DisplayedPhase: -1, Dropped: state.dropped, Spinner: state.spinner,
		MaxBodyLines: state.options.MaxBodyLines, MaxRenderWidth: state.options.MaxRenderWidth,
	}
	for phaseIndex, evalID := range state.phaseOrder {
		phase := state.phases[evalID]
		phaseSnapshot := subagentPanelPhaseSnapshot{EvalID: evalID, Index: phase.index, Active: state.activeSet && state.active == evalID, Selected: displayed == evalID}
		earliest := time.Time{}
		latest := time.Time{}
		for _, id := range phase.order {
			record := phase.records[id]
			elapsed := record.duration
			if record.status == subagentPanelRunning {
				elapsed = max(now.Sub(record.startedAt), 0)
				result.Running = true
			}
			phaseSnapshot.Records = append(phaseSnapshot.Records, subagentPanelRecordSnapshot{ID: record.id, Label: record.label, Status: record.status, Elapsed: elapsed, Error: record.error})
			if earliest.IsZero() || record.startedAt.Before(earliest) {
				earliest = record.startedAt
			}
			end := record.startedAt.Add(elapsed)
			if latest.IsZero() || end.After(latest) {
				latest = end
			}
		}
		phaseSnapshot.Done, phaseSnapshot.Total, phaseSnapshot.Failed, phaseSnapshot.Cancelled = phase.counts()
		if !earliest.IsZero() {
			if !phase.allTerminal() {
				latest = now
			}
			phaseSnapshot.Elapsed = max(latest.Sub(earliest), 0)
		}
		result.Done += phaseSnapshot.Done
		result.Total += phaseSnapshot.Total
		result.Failed += phaseSnapshot.Failed
		result.Cancelled += phaseSnapshot.Cancelled
		if displayed == evalID {
			result.DisplayedPhase = phaseIndex
		}
		result.Phases = append(result.Phases, phaseSnapshot)
	}
	return result
}

func normalizeSubagentPanelID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > defaultSubagentPanelIDRunes {
		return "", false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", false
		}
	}
	return value, true
}

func subagentPanelRowLabel(event subagentPanelEvent, limit int) string {
	subagentType := sanitizeSubagentPanelText(event.SubagentType, 40)
	if subagentType == "" {
		subagentType = "subagent"
	}
	label := sanitizeSubagentPanelText(event.Label, limit)
	if label == "" {
		label = sanitizeSubagentPanelText(event.Description, min(limit, defaultSubagentDescriptionRunes))
	}
	if label == "" {
		label = "unknown task"
	}
	return sanitizeSubagentPanelText(subagentType+": "+label, limit)
}

func sanitizeSubagentPanelText(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	value = unicodesecurity.RenderTerminalSafe(value)
	value = strings.Join(strings.Fields(value), " ")
	characters := []rune(value)
	if len(characters) <= maxRunes {
		return value
	}
	if maxRunes <= 3 {
		return string(characters[:maxRunes])
	}
	return string(characters[:maxRunes-3]) + "..."
}

func removeSubagentPanelValue(values []string, target string) []string {
	for index, value := range values {
		if value == target {
			return append(values[:index], values[index+1:]...)
		}
	}
	return values
}

// renderSubagentPanel renders bounded plain terminal content. Styling remains
// app-owned; passing asciiUIGlyphs produces ASCII-only chrome.
func renderSubagentPanel(snapshot subagentPanelSnapshot, width int, glyphs uiGlyphs) string {
	if !snapshot.Visible || width <= 0 {
		return ""
	}
	maxWidth := snapshot.MaxRenderWidth
	if maxWidth <= 0 {
		maxWidth = defaultSubagentPanelMaxRenderWidth
	}
	width = min(width, min(maxWidth, maximumSubagentPanelRenderWidth))
	caret := glyphs.DisclosureExpanded
	hint := "Ctrl+G collapse"
	if !snapshot.Expanded {
		caret, hint = glyphs.DisclosureClosed, "Ctrl+G expand"
	}
	icon := glyphs.Checkmark
	if snapshot.Running {
		frames := glyphs.SpinnerFrames
		if len(frames) == 0 {
			frames = []string{"*"}
		}
		icon = frames[int(snapshot.Spinner%uint64(len(frames)))]
	} else if snapshot.Failed > 0 {
		icon = glyphs.Error
	} else if snapshot.Cancelled > 0 {
		icon = glyphs.CircleEmpty
	}
	header := fmt.Sprintf("%s %s dynamic subagents", caret, icon)
	if snapshot.Expanded && snapshot.Total > 0 {
		phaseWord := "phases"
		if len(snapshot.Phases) == 1 {
			phaseWord = "phase"
		}
		header += fmt.Sprintf("  %d/%d done | %d %s", snapshot.Done, snapshot.Total, len(snapshot.Phases), phaseWord)
		if snapshot.Failed > 0 {
			header += fmt.Sprintf(" | %d failed", snapshot.Failed)
		}
		if snapshot.Cancelled > 0 {
			header += fmt.Sprintf(" | %d cancelled", snapshot.Cancelled)
		}
		if snapshot.Dropped > 0 {
			header += fmt.Sprintf(" | %d dropped", snapshot.Dropped)
		}
	}
	header = panelJoinHint(header, hint, width, glyphs.Ellipsis)
	if !snapshot.Expanded || len(snapshot.Phases) == 0 {
		return header
	}
	bodyLimit := snapshot.MaxBodyLines
	if bodyLimit <= 0 {
		bodyLimit = defaultSubagentPanelMaxBodyLines
	}
	bodyLimit = min(bodyLimit, maximumSubagentPanelBodyLines)
	phaseWidth := min(24, max(14, width/4))
	separator := " | "
	agentWidth := width - phaseWidth - ansi.StringWidth(separator)
	if agentWidth < 24 {
		return strings.Join(append([]string{header}, renderSubagentAgents(snapshot, width, bodyLimit, glyphs)...), "\n")
	}
	phases := renderSubagentPhases(snapshot, phaseWidth, bodyLimit, glyphs)
	agents := renderSubagentAgents(snapshot, agentWidth, bodyLimit, glyphs)
	rows := max(len(phases), len(agents))
	lines := make([]string, 0, rows+1)
	lines = append(lines, header)
	for row := 0; row < rows; row++ {
		left, right := "", ""
		if row < len(phases) {
			left = phases[row]
		}
		if row < len(agents) {
			right = agents[row]
		}
		lines = append(lines, panelPad(left, phaseWidth, glyphs.Ellipsis)+separator+panelFit(right, agentWidth, glyphs.Ellipsis))
	}
	return strings.Join(lines, "\n")
}

func renderSubagentPhases(snapshot subagentPanelSnapshot, width, limit int, glyphs uiGlyphs) []string {
	lines := []string{"PHASES"}
	available := max(limit-1, 0)
	start := panelViewportStart(snapshot.DisplayedPhase, len(snapshot.Phases), available)
	for index := start; index < min(start+available, len(snapshot.Phases)); index++ {
		phase := snapshot.Phases[index]
		mark := glyphs.Bullet
		if phase.Done == phase.Total && phase.Total > 0 {
			switch {
			case phase.Failed > 0:
				mark = glyphs.Error
			case phase.Cancelled > 0:
				mark = glyphs.CircleEmpty
			default:
				mark = glyphs.Checkmark
			}
		} else if phase.Active {
			mark = glyphs.DisclosureClosed
		}
		cursor := " "
		if phase.Selected {
			cursor = glyphs.Cursor
		}
		line := fmt.Sprintf("%s %s %d %d/%d %s", cursor, mark, phase.Index, phase.Done, phase.Total, formatSubagentPanelDuration(phase.Elapsed))
		lines = append(lines, panelFit(line, width, glyphs.Ellipsis))
	}
	return lines
}

func renderSubagentAgents(snapshot subagentPanelSnapshot, width, limit int, glyphs uiGlyphs) []string {
	if snapshot.DisplayedPhase < 0 || snapshot.DisplayedPhase >= len(snapshot.Phases) || limit <= 0 {
		return nil
	}
	phase := snapshot.Phases[snapshot.DisplayedPhase]
	lines := []string{panelAgentHeading(width)}
	available := max(limit-1, 0)
	show := min(len(phase.Records), available)
	if len(phase.Records) > available && available > 0 {
		show = max(available-1, 0)
	}
	for index := 0; index < show; index++ {
		lines = append(lines, panelAgentRow(phase.Records[index], snapshot.ModelLabel, snapshot.Spinner, width, glyphs))
	}
	if len(phase.Records) > show && available > 0 {
		lines = append(lines, panelFit(fmt.Sprintf("%s %d more", glyphs.Ellipsis, len(phase.Records)-show), width, glyphs.Ellipsis))
	}
	return lines
}

func panelAgentHeading(width int) string {
	if width < 44 {
		return panelFit("STATUS TASK", width, "...")
	}
	return panelFit("STATUS TASK                         MODEL        TIME", width, "...")
}

func panelAgentRow(record subagentPanelRecordSnapshot, model string, spinner uint64, width int, glyphs uiGlyphs) string {
	icon := glyphs.Checkmark
	switch record.Status {
	case subagentPanelRunning:
		frames := glyphs.SpinnerFrames
		if len(frames) > 0 {
			icon = frames[int(spinner%uint64(len(frames)))]
		} else {
			icon = "*"
		}
	case subagentPanelError:
		icon = glyphs.Error
	case subagentPanelCancelled:
		icon = glyphs.CircleEmpty
	}
	label := record.Label
	if record.Status == subagentPanelError && record.Error != "" {
		label += " - " + record.Error
	}
	if width < 44 {
		return panelFit(icon+" "+label+" "+formatSubagentPanelDuration(record.Elapsed), width, glyphs.Ellipsis)
	}
	modelWidth, timingWidth := 12, 6
	labelWidth := max(width-ansi.StringWidth(icon)-modelWidth-timingWidth-4, 8)
	return panelFit(icon+" "+panelPad(label, labelWidth, glyphs.Ellipsis)+" "+panelPad(model, modelWidth, glyphs.Ellipsis)+" "+panelPad(formatSubagentPanelDuration(record.Elapsed), timingWidth, glyphs.Ellipsis), width, glyphs.Ellipsis)
}

func panelViewportStart(selected, total, available int) int {
	if total <= available || available <= 0 {
		return 0
	}
	start := max(selected-available/2, 0)
	return min(start, total-available)
}

func panelJoinHint(left, hint string, width int, ellipsis string) string {
	left, hint = panelFit(left, width, ellipsis), panelFit(hint, width, ellipsis)
	space := width - ansi.StringWidth(left) - ansi.StringWidth(hint)
	if space < 2 {
		return panelFit(left, width, ellipsis)
	}
	return left + strings.Repeat(" ", space) + hint
}

func panelFit(value string, width int, ellipsis string) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(unicodesecurity.RenderTerminalSafe(value), width, ellipsis)
}

func panelPad(value string, width int, ellipsis string) string {
	value = panelFit(value, width, ellipsis)
	return value + strings.Repeat(" ", max(width-ansi.StringWidth(value), 0))
}

func formatSubagentPanelDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	if duration < time.Minute {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(duration/time.Minute), int(duration/time.Second)%60)
	}
	return fmt.Sprintf("%dh%02dm", int(duration/time.Hour), int(duration/time.Minute)%60)
}
