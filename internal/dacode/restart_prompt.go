package dacode

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const defaultRestartPromptBody = "Restart the local agent server to load it now."

type restartPromptAction uint8

const (
	restartPromptNoAction restartPromptAction = iota
	restartPromptRestart
	restartPromptLater
)

type restartPromptState struct {
	label string
	verb  string
	body  string
}

func newRestartPrompt(label, verb string, body ...string) *restartPromptState {
	label = boundedRestartPromptText(label, 256)
	verb = boundedRestartPromptText(verb, 64)
	if label == "" || verb == "" || len(body) > 1 {
		panic("dacode: restart prompt requires a label, verb, and at most one body")
	}
	message := defaultRestartPromptBody
	if len(body) == 1 && strings.TrimSpace(body[0]) != "" {
		message = boundedRestartPromptText(body[0], 2048)
	}
	return &restartPromptState{label: label, verb: verb, body: message}
}

func (state *restartPromptState) handleKey(key string) restartPromptAction {
	if state == nil {
		return restartPromptNoAction
	}
	switch strings.ToLower(key) {
	case "enter":
		return restartPromptRestart
	case "esc":
		return restartPromptLater
	default:
		return restartPromptNoAction
	}
}

func renderRestartPrompt(state *restartPromptState, width, height int, glyphs uiGlyphs) string {
	if state == nil {
		return ""
	}
	contentWidth := min(max(width-12, 36), 64)
	lines := []string{
		lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).Align(lipgloss.Center).Width(contentWidth).
			Render(glyphs.Checkmark + " " + state.verb + " " + state.label),
		"",
		lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth).Render(state.body),
		"",
		lipgloss.NewStyle().Foreground(colorMuted).Align(lipgloss.Center).Width(contentWidth).Render("Enter to restart" + resumePromptSeparator(glyphs) + "Esc to defer"),
	}
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, contentWidth+8), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

func boundedRestartPromptText(value string, maximum int) string {
	value = strings.TrimSpace(unicodesecurity.RenderTerminalSafe(value))
	characters := []rune(value)
	if len(characters) > maximum {
		return string(characters[:maximum-3]) + "..."
	}
	return value
}
