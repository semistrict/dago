package dacode

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func renderThreadSelector(state *threadSelectorState, width, height int, glyphs uiGlyphs) string {
	if state == nil || width < 24 || height < 8 {
		return ""
	}
	contentWidth := min(max(width-8, 16), 140)
	listHeight := min(max(height-13, 3), 20)
	now := state.now()
	agent := "All agents"
	if !state.allAgents {
		agent = state.agent
		if agent == "" {
			agent = "(unnamed)"
		}
	}
	searchCursor, agentCursor := "  ", "  "
	if state.focus == threadSelectorSearchFocus {
		searchCursor = glyphs.Cursor + " "
	}
	if state.focus == threadSelectorAgentFocus {
		agentCursor = glyphs.Cursor + " "
	}
	query := state.query
	if query == "" {
		query = "type to filter"
	}
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Threads"),
		lipgloss.NewStyle().Foreground(colorBody).Render(selectorFit(searchCursor+"Search: "+query, contentWidth, glyphs.Ellipsis)),
		lipgloss.NewStyle().Foreground(colorBody).Render(selectorFit(agentCursor+"Agent: "+agent, contentWidth, glyphs.Ellipsis)),
		"",
	}

	if len(state.visible) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No matching threads."))
	} else {
		start := boundedWindowStart(state.selected, listHeight, len(state.visible))
		end := min(start+listHeight, len(state.visible))
		lines = append(lines, lipgloss.NewStyle().Foreground(colorSecondary).Bold(true).Render(
			renderThreadSelectorRow(sessionInfo{}, false, false, contentWidth, state.relativeTime, now, glyphs, true)))
		for visibleIndex := start; visibleIndex < end; visibleIndex++ {
			session := state.sessions[state.visible[visibleIndex]]
			selected := visibleIndex == state.selected
			line := renderThreadSelectorRow(session, selected, session.ThreadID == state.currentThread, contentWidth, state.relativeTime, now, glyphs, false)
			style := lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth)
			if selected && state.focus == threadSelectorListFocus {
				style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
			}
			lines = append(lines, style.Render(line))
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
			fmt.Sprintf("Showing %d-%d of %d", start+1, end, len(state.visible))))
	}

	switch {
	case state.confirmingDelete != nil:
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render(
			selectorFit("Delete "+state.confirmingDelete.ThreadID+"? Enter/y confirms; Esc/n cancels.", contentWidth, glyphs.Ellipsis)))
	case state.deleting != nil:
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorWarning).Render(
			selectorFit(glyphs.Hourglass+" Deleting "+state.deleting.ThreadID+glyphs.Ellipsis, contentWidth, glyphs.Ellipsis)))
	case state.deleteError != "":
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorError).Render(
			selectorFit("Delete failed: "+state.deleteError, contentWidth, glyphs.Ellipsis)))
	}

	separator := "  " + glyphs.Bullet + "  "
	footer := "Tab focus" + separator + glyphs.ArrowUp + "/" + glyphs.ArrowDown + " navigate" + separator + "Enter resume"
	footer += "\nPgUp/PgDn page" + separator + "Ctrl+D delete" + separator + "Ctrl+R time" + separator + "Esc close"
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(selectorFitMultiline(footer, contentWidth, glyphs.Ellipsis)))

	panel := lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, lipgloss.Width(panel)), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

func renderThreadSelectorRow(session sessionInfo, selected, current bool, width int, relative bool, now time.Time, glyphs uiGlyphs, header bool) string {
	prefix := "  "
	if selected {
		prefix = glyphs.Cursor + " "
	}
	threadID, agent, messages, created, updated, branch, directory, preview := session.ThreadID, session.Agent,
		fmt.Sprintf("%d", session.MessageCount), formatThreadSelectorTime(session.CreatedAt, relative, now),
		formatThreadSelectorTime(session.UpdatedAt, relative, now), session.Branch, selectorDisplayPath(session.Directory), session.Preview
	if branch == "" {
		branch = "-"
	}
	if current {
		threadID += "*"
	}
	if header {
		prefix, threadID, agent, messages, created, updated, branch, directory, preview = "  ", "THREAD", "AGENT", "MSGS", "CREATED", "UPDATED", "BRANCH", "CWD", "PROMPT"
	}
	if width >= 110 {
		fixed := 92
		return prefix + selectorFit(threadID, 18, glyphs.Ellipsis) + " " + selectorFit(agent, 12, glyphs.Ellipsis) + " " +
			selectorFit(messages, 5, glyphs.Ellipsis) + " " + selectorFit(created, 10, glyphs.Ellipsis) + " " +
			selectorFit(updated, 10, glyphs.Ellipsis) + " " + selectorFit(branch, 12, glyphs.Ellipsis) + " " +
			selectorFit(directory, 16, glyphs.Ellipsis) + " " + selectorFit(preview, width-fixed, glyphs.Ellipsis)
	}
	if width >= 76 {
		fixed := 70
		return prefix + selectorFit(threadID, 18, glyphs.Ellipsis) + " " + selectorFit(agent, 12, glyphs.Ellipsis) + " " +
			selectorFit(messages, 5, glyphs.Ellipsis) + " " + selectorFit(updated, 10, glyphs.Ellipsis) + " " +
			selectorFit(directory, 18, glyphs.Ellipsis) + " " + selectorFit(preview, width-fixed, glyphs.Ellipsis)
	}
	if width >= 38 {
		fixed := 34
		return prefix + selectorFit(threadID, 16, glyphs.Ellipsis) + " " + selectorFit(messages, 5, glyphs.Ellipsis) + " " +
			selectorFit(updated, 8, glyphs.Ellipsis) + " " + selectorFit(preview, width-fixed, glyphs.Ellipsis)
	}
	threadWidth := max(width/2, 6)
	return prefix + selectorFit(threadID, threadWidth, glyphs.Ellipsis) + " " + selectorFit(preview, width-threadWidth-3, glyphs.Ellipsis)
}

func formatThreadSelectorTime(value time.Time, relative bool, now time.Time) string {
	if value.IsZero() {
		return "-"
	}
	if !relative {
		return value.Format("2006-01-02")
	}
	delta := now.Sub(value)
	if delta < 0 {
		delta = -delta
		if delta < time.Minute {
			return "now"
		}
		return "in " + formatThreadSelectorAge(delta)
	}
	if delta < time.Minute {
		return "now"
	}
	return formatThreadSelectorAge(delta) + " ago"
}

func formatThreadSelectorAge(delta time.Duration) string {
	switch {
	case delta < time.Hour:
		return fmt.Sprintf("%dm", int(delta/time.Minute))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh", int(delta/time.Hour))
	case delta < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(delta/(24*time.Hour)))
	case delta < 30*24*time.Hour:
		return fmt.Sprintf("%dw", int(delta/(7*24*time.Hour)))
	case delta < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(delta/(30*24*time.Hour)))
	default:
		return fmt.Sprintf("%dy", int(delta/(365*24*time.Hour)))
	}
}

func selectorDisplayPath(value string) string {
	if value == "" {
		return "-"
	}
	cleaned := filepath.Clean(value)
	if base := filepath.Base(cleaned); base != "." && base != string(filepath.Separator) {
		return base
	}
	return cleaned
}

func selectorFit(value string, width int, ellipsis string) string {
	if width <= 0 {
		return ""
	}
	value = ansi.Truncate(value, width, ellipsis)
	return value + strings.Repeat(" ", max(width-ansi.StringWidth(value), 0))
}

func selectorFitMultiline(value string, width int, ellipsis string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, ellipsis)
	}
	return strings.Join(lines, "\n")
}
