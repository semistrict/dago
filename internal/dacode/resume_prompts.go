package dacode

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type resumePromptAction uint8

const (
	resumePromptNoAction resumePromptAction = iota
	resumePromptSwitchCWD
	resumePromptStayCWD
	resumePromptAbort
	resumePromptSwitchAgent
	resumePromptCancelAgent
	resumePromptCompactNow
	resumePromptKeepContext
)

type cwdResumeAbortMode uint8

const (
	cwdResumeAbortNone cwdResumeAbortMode = iota
	cwdResumeAbortLaunch
	cwdResumeAbortThreadSwitch
)

type cwdResumePromptState struct {
	currentCWD            string
	threadCWD             string
	projectSettingsChange bool
	abortMode             cwdResumeAbortMode
}

func newCWDResumePrompt(currentCWD, threadCWD string, projectSettingsChange bool, abortMode cwdResumeAbortMode) *cwdResumePromptState {
	if abortMode > cwdResumeAbortThreadSwitch {
		abortMode = cwdResumeAbortNone
	}
	return &cwdResumePromptState{
		currentCWD:            boundedResumePromptText(currentCWD, 2048),
		threadCWD:             boundedResumePromptText(threadCWD, 2048),
		projectSettingsChange: projectSettingsChange,
		abortMode:             abortMode,
	}
}

func (state *cwdResumePromptState) handleKey(key string) resumePromptAction {
	if state == nil {
		return resumePromptNoAction
	}
	switch strings.ToLower(key) {
	case "enter":
		return resumePromptSwitchCWD
	case "esc":
		return resumePromptStayCWD
	case "a":
		if state.abortMode != cwdResumeAbortNone {
			return resumePromptAbort
		}
	}
	return resumePromptNoAction
}

func renderCWDResumePrompt(state *cwdResumePromptState, width, height int, glyphs uiGlyphs) string {
	if state == nil {
		return ""
	}
	title := "Resume from the thread's original directory?"
	if state.abortMode == cwdResumeAbortThreadSwitch {
		title = "Switch to the thread's original directory?"
	}
	body := "This thread was last used from:\n  " + state.threadCWD + "\n\nYou're currently in:\n  " + state.currentCWD +
		"\n\nSwitch if you want local context, project instructions, skills, MCP config, and env files to match the original directory. " +
		"Stay here if you intentionally want to continue this thread against the current directory."
	if state.projectSettingsChange {
		body += "\n\nSwitching may also reload project-specific config like .env, MCP, skills, and AGENTS.md."
	}
	if state.abortMode == cwdResumeAbortLaunch {
		body += "\n\nOr abort to start a new session instead of resuming."
	}
	help := "Enter: switch" + resumePromptSeparator(glyphs) + "Esc: stay in cwd"
	if state.abortMode == cwdResumeAbortLaunch {
		help += resumePromptSeparator(glyphs) + "A: don't resume"
	} else if state.abortMode == cwdResumeAbortThreadSwitch {
		help += resumePromptSeparator(glyphs) + "A: don't switch"
	}
	return renderResumePromptPanel(title, body, help, width, height, glyphs)
}

type agentResumePromptState struct {
	threadID     string
	currentAgent string
	threadAgent  string
}

func newAgentResumePrompt(threadID, currentAgent, threadAgent string) *agentResumePromptState {
	return &agentResumePromptState{
		threadID:     boundedResumePromptText(threadID, 512),
		currentAgent: boundedResumePromptText(currentAgent, 256),
		threadAgent:  boundedResumePromptText(threadAgent, 256),
	}
}

func (state *agentResumePromptState) handleKey(key string) resumePromptAction {
	if state == nil {
		return resumePromptNoAction
	}
	switch strings.ToLower(key) {
	case "enter":
		return resumePromptSwitchAgent
	case "esc":
		return resumePromptCancelAgent
	default:
		return resumePromptNoAction
	}
}

func renderAgentResumePrompt(state *agentResumePromptState, width, height int, glyphs uiGlyphs) string {
	if state == nil {
		return ""
	}
	body := fmt.Sprintf("Thread %s belongs to agent %s, but %s is active.\n\nSwitch to %s and resume this thread? The local agent server will restart. Your saved default agent will not change.",
		state.threadID, state.threadAgent, state.currentAgent, state.threadAgent)
	return renderResumePromptPanel("Switch agents to resume?", body,
		"Enter: switch and resume"+resumePromptSeparator(glyphs)+"Esc: cancel", width, height, glyphs)
}

type compactResumePromptState struct {
	contextTokens int
	threshold     int
}

func newCompactResumePrompt(contextTokens, threshold int) *compactResumePromptState {
	return &compactResumePromptState{contextTokens: max(contextTokens, 0), threshold: max(threshold, 0)}
}

func (state *compactResumePromptState) handleKey(key string) resumePromptAction {
	if state == nil {
		return resumePromptNoAction
	}
	switch strings.ToLower(key) {
	case "enter":
		return resumePromptCompactNow
	case "esc":
		return resumePromptKeepContext
	default:
		return resumePromptNoAction
	}
}

func renderCompactResumePrompt(state *compactResumePromptState, width, height int, glyphs uiGlyphs) string {
	if state == nil {
		return ""
	}
	body := fmt.Sprintf("This thread uses %s context tokens, above the configured %s token threshold. Compacting summarizes older messages so later turns cost less.",
		formatTokenCount(state.contextTokens), formatTokenCount(state.threshold))
	return renderResumePromptPanel("Compact this thread?", body,
		"Enter: compact now"+resumePromptSeparator(glyphs)+"Esc: keep full context", width, height, glyphs)
}

func renderResumePromptPanel(title, body, help string, width, height int, glyphs uiGlyphs) string {
	contentWidth := min(max(width-12, 40), 72)
	lines := []string{
		lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Align(lipgloss.Center).Width(contentWidth).Render(unicodesecurity.RenderTerminalSafe(title)),
		"",
		lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth).Render(unicodesecurity.RenderTerminalSafe(body)),
		"",
		lipgloss.NewStyle().Foreground(colorMuted).Align(lipgloss.Center).Width(contentWidth).Render(unicodesecurity.RenderTerminalSafe(help)),
	}
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorWarning).Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, contentWidth+8), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

func resumePromptSeparator(glyphs uiGlyphs) string {
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		return " - "
	}
	return " " + glyphs.Bullet + " "
}

func boundedResumePromptText(value string, maximum int) string {
	value = unicodesecurity.RenderTerminalSafe(value)
	characters := []rune(value)
	if len(characters) > maximum {
		value = string(characters[:maximum]) + "..."
	}
	return value
}
