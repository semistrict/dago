package dacode

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const maxStatusBarWidth = 4096

type statusBarState struct {
	InputMode        string
	ApprovalMode     string
	Connection       string
	AgentStatus      string
	HookStatus       string
	BusyStatus       string
	WorkingDirectory string
	Branch           string
	Rubric           string
	Model            string
	Effort           string
	Queued           int
	Tokens           int64
	ContextLimit     int64
	CacheInput       int64
	CacheRead        int64
	CacheWrite       int64
	CostUSD          float64
	TokensPending    bool
	Approximate      bool
	Spinner          bool
	QueuePending     bool
	QueueStale       bool
	ContextStale     bool
	CachePending     bool
	CacheStale       bool
	CostPending      bool
	CostStale        bool
}

func newStatusBarState() statusBarState {
	return statusBarState{InputMode: "normal", ApprovalMode: "manual"}
}

func normalizeStatusBarState(state statusBarState) statusBarState {
	switch state.InputMode {
	case "normal", "shell", "shell_incognito", "command":
	default:
		state.InputMode = "normal"
	}
	switch state.ApprovalMode {
	case "manual", "auto", "yolo":
	default:
		state.ApprovalMode = "manual"
	}
	switch state.Connection {
	case "", "connecting", "reconnecting", "resuming":
	default:
		state.Connection = ""
	}
	state.AgentStatus = safeStatusText(state.AgentStatus, 256)
	state.HookStatus = safeStatusText(state.HookStatus, 256)
	state.BusyStatus = safeStatusText(state.BusyStatus, 256)
	state.WorkingDirectory = safeStatusText(state.WorkingDirectory, 1024)
	state.Branch = safeStatusText(state.Branch, 256)
	state.Rubric = safeStatusText(state.Rubric, 128)
	state.Model = safeStatusText(state.Model, 1024)
	state.Effort = safeStatusText(state.Effort, 64)
	state.Queued = min(max(state.Queued, 0), 1_000_000)
	state.Tokens = clampStatusCount(state.Tokens)
	state.ContextLimit = clampStatusCount(state.ContextLimit)
	state.CacheInput = clampStatusCount(state.CacheInput)
	state.CacheRead = min(clampStatusCount(state.CacheRead), state.CacheInput)
	state.CacheWrite = clampStatusCount(state.CacheWrite)
	if math.IsNaN(state.CostUSD) || math.IsInf(state.CostUSD, 0) || state.CostUSD < 0 {
		state.CostUSD = 0
	} else if state.CostUSD > 1_000_000_000 {
		state.CostUSD = 1_000_000_000
	}
	return state
}

func safeStatusText(value string, limit int) string {
	value = strings.TrimSpace(value)
	characters := []rune(value)
	if len(characters) > limit {
		characters = characters[:limit]
	}
	return unicodesecurity.RenderTerminalSafe(string(characters))
}

func clampStatusCount(value int64) int64 { return min(max(value, 0), 1_000_000_000_000_000) }

func statusSpinnerActive(state statusBarState) bool {
	state = normalizeStatusBarState(state)
	return state.Spinner || state.Connection != "" || state.BusyStatus != ""
}

func renderTwoLineStatusBar(state statusBarState, width int, spinnerFrame string, glyphs uiGlyphs) string {
	state = normalizeStatusBarState(state)
	width = min(max(width, 20), maxStatusBarWidth)
	// Spinner frames are application-owned styled text. Strip their styling
	// before applying the untrusted-text sanitizer so ANSI does not become a
	// visible "<U+001B CONTROL>" marker in the status line.
	spinnerFrame = ansi.Strip(spinnerFrame)
	spinnerFrame = safeStatusText(spinnerFrame, 16)
	if spinnerFrame == "" && len(glyphs.SpinnerFrames) > 0 {
		spinnerFrame = glyphs.SpinnerFrames[0]
	}

	firstLeft := make([]string, 0, 5)
	if mode := statusInputModeLabel(state.InputMode); mode != "" {
		firstLeft = append(firstLeft, mode)
	}
	firstLeft = append(firstLeft, statusApprovalLabel(state.ApprovalMode))
	if width >= 70 && state.WorkingDirectory != "" {
		firstLeft = append(firstLeft, shortPath(state.WorkingDirectory))
	}
	if state.Branch != "" {
		firstLeft = append(firstLeft, glyphs.GitBranch+" "+state.Branch)
	}
	if state.Rubric != "" {
		firstLeft = append(firstLeft, "rubric:"+state.Rubric)
	}
	model := state.Model
	if state.Effort != "" {
		model = strings.TrimSpace(model + " " + state.Effort)
	}
	first := joinStatusSides(strings.Join(firstLeft, " "+glyphs.Bullet+" "), model, width, glyphs.Ellipsis)

	secondParts := make([]string, 0, 5)
	connection := statusConnectionLabel(state.Connection)
	if connection != "" {
		connection = strings.TrimSpace(spinnerFrame + " " + connection)
	}
	if state.Queued > 0 {
		queued := fmt.Sprintf("%d messages queued", state.Queued)
		if state.Queued == 1 {
			queued = "1 message queued"
		}
		queued += statusMetricMarker(state.QueuePending, state.QueueStale, glyphs)
		connection = joinNonEmpty(connection, renderStatusThreshold(queued, statusQueueThreshold(state.Queued)), " "+glyphs.Bullet+" ")
	} else if state.QueuePending || state.QueueStale {
		connection = joinNonEmpty(connection, "Queue"+statusMetricMarker(state.QueuePending, state.QueueStale, glyphs), " "+glyphs.Bullet+" ")
	}
	if connection != "" {
		secondParts = append(secondParts, connection)
	}
	status := state.HookStatus
	if status == "" {
		status = state.AgentStatus
	}
	if state.BusyStatus != "" {
		status = strings.TrimSpace(spinnerFrame + " " + state.BusyStatus)
	}
	if status != "" {
		secondParts = append(secondParts, status)
	}
	if state.CacheInput > 0 || state.CacheWrite > 0 || state.CachePending || state.CacheStale {
		hitRate := float64(state.CacheRead) / float64(max(state.CacheInput, 1)) * 100
		cache := fmt.Sprintf("Cache: %.0f%% read / %s write", hitRate, compactStatusCount(state.CacheWrite))
		cache += statusMetricMarker(state.CachePending, state.CacheStale, glyphs)
		secondParts = append(secondParts, renderStatusThreshold(cache, statusCacheThreshold(state.CacheInput, state.CacheRead)))
	}
	contextLabel := statusContextLabel(state) + statusMetricMarker(false, state.ContextStale, glyphs)
	secondParts = append(secondParts, renderStatusThreshold(contextLabel, statusContextThreshold(state)))
	if state.CostUSD > 0 || state.CostPending || state.CostStale {
		cost := formatStatusCost(state.CostUSD) + statusMetricMarker(state.CostPending, state.CostStale, glyphs)
		secondParts = append(secondParts, renderStatusThreshold(cost, statusCostThreshold(state.CostUSD)))
	}
	second := truncateStatusLine(strings.Join(secondParts, " "+glyphs.Bullet+" "), width, glyphs.Ellipsis)
	return first + "\n" + second
}

type statusThreshold uint8

const (
	statusThresholdNormal statusThreshold = iota
	statusThresholdWarning
	statusThresholdCritical
)

func statusMetricMarker(pending, stale bool, glyphs uiGlyphs) string {
	marker := ""
	if pending {
		marker = " " + glyphs.Hourglass + " pending"
	}
	if stale {
		marker += " ~ stale"
	}
	return marker
}

func statusQueueThreshold(queued int) statusThreshold {
	if queued >= 20 {
		return statusThresholdCritical
	}
	if queued >= 5 {
		return statusThresholdWarning
	}
	return statusThresholdNormal
}

func statusContextThreshold(state statusBarState) statusThreshold {
	if state.ContextLimit <= 0 {
		return statusThresholdNormal
	}
	percentage := float64(state.Tokens) / float64(state.ContextLimit) * 100
	if percentage >= 80 {
		return statusThresholdCritical
	}
	if percentage >= 60 {
		return statusThresholdWarning
	}
	return statusThresholdNormal
}

func statusCacheThreshold(input, read int64) statusThreshold {
	if input <= 0 {
		return statusThresholdNormal
	}
	rate := float64(read) / float64(input) * 100
	if rate < 60 {
		return statusThresholdCritical
	}
	if rate < 90 {
		return statusThresholdWarning
	}
	return statusThresholdNormal
}

func statusCostThreshold(cost float64) statusThreshold {
	if cost >= 100 {
		return statusThresholdCritical
	}
	if cost >= 10 {
		return statusThresholdWarning
	}
	return statusThresholdNormal
}

func renderStatusThreshold(value string, threshold statusThreshold) string {
	switch threshold {
	case statusThresholdCritical:
		return lipgloss.NewStyle().Foreground(colorError).Bold(true).Render(value)
	case statusThresholdWarning:
		return lipgloss.NewStyle().Foreground(colorWarning).Render(value)
	default:
		return value
	}
}

func statusInputModeLabel(mode string) string {
	switch mode {
	case "shell", "shell_incognito":
		return "SHELL"
	case "command":
		return "CMD"
	default:
		return ""
	}
}

func statusApprovalLabel(mode string) string {
	if mode == "yolo" {
		return "YOLO"
	}
	return mode
}

func statusConnectionLabel(connection string) string {
	switch connection {
	case "connecting":
		return "Connecting"
	case "reconnecting":
		return "Reconnecting"
	case "resuming":
		return "Resuming"
	default:
		return ""
	}
}

func statusContextLabel(state statusBarState) string {
	if state.TokensPending {
		return "Context: ... / Tokens: ..."
	}
	suffix := ""
	if state.Approximate {
		suffix = "+"
	}
	percentage := "0%"
	if state.ContextLimit <= 0 && state.Tokens > 0 {
		percentage = "--"
	} else if state.ContextLimit > 0 {
		percent := min(max(float64(state.Tokens)/float64(state.ContextLimit)*100, 0), 100)
		percentage = fmt.Sprintf("%.0f%%", percent)
	}
	return fmt.Sprintf("Context: %s / Tokens: %s%s", percentage, compactStatusCount(state.Tokens), suffix)
}

func compactStatusCount(value int64) string {
	value = clampStatusCount(value)
	if value < 1_000 {
		return fmt.Sprintf("%d", value)
	}
	amount := float64(value) / 1_000
	text := fmt.Sprintf("%.1fk", amount)
	if value%1_000 == 0 {
		text = fmt.Sprintf("%.0fk", amount)
	}
	return text
}

func formatStatusCost(value float64) string {
	if value >= 1 {
		return fmt.Sprintf("$%.2f", value)
	}
	return fmt.Sprintf("$%.4f", value)
}

func joinNonEmpty(left, right, separator string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + separator + right
}

func joinStatusSides(left, right string, width int, ellipsis string) string {
	if right == "" {
		return truncateStatusLine(left, width, ellipsis)
	}
	rightWidth := lipgloss.Width(right)
	if rightWidth >= width {
		if left != "" {
			left = ansi.Truncate(left, max(width/3, 8), ellipsis)
			available := max(width-lipgloss.Width(left)-1, 1)
			right = truncateStatusLeft(right, available, ellipsis)
			return truncateStatusLine(left+strings.Repeat(" ", max(width-lipgloss.Width(left)-lipgloss.Width(right), 1))+right, width, ellipsis)
		}
		return truncateStatusLeft(right, width, ellipsis)
	}
	left = ansi.Truncate(left, max(width-rightWidth-1, 0), ellipsis)
	padding := max(width-lipgloss.Width(left)-rightWidth, 1)
	return truncateStatusLine(left+strings.Repeat(" ", padding)+right, width, ellipsis)
}

func truncateStatusLeft(value string, width int, ellipsis string) string {
	valueWidth := lipgloss.Width(value)
	if valueWidth <= width {
		return value
	}
	prefixWidth := lipgloss.Width(ellipsis)
	if width <= prefixWidth {
		return ansi.Truncate(value, width, "")
	}
	return ansi.TruncateLeft(value, valueWidth-width+prefixWidth, ellipsis)
}

func truncateStatusLine(value string, width int, ellipsis string) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(value, width, ellipsis)
}
