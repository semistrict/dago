package dacode

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/damessage"
)

type sessionInfo struct {
	ThreadID     string
	Preview      string
	Directory    string
	UpdatedAt    time.Time
	MessageCount int
}

type sessionPickerState struct {
	sessions []sessionInfo
	selected int
	loading  bool
	resuming bool
	startup  bool
	err      error
}

type sessionsLoadedMsg struct {
	sessions []sessionInfo
	err      error
}

type sessionLoadedMsg struct {
	session  sessionInfo
	messages []damessage.Message
	err      error
}

func listSessions(ctx context.Context, runner agentRunner) tea.Cmd {
	return func() tea.Msg {
		sessions, err := runner.ListSessions(ctx)
		return sessionsLoadedMsg{sessions: sessions, err: err}
	}
}

func loadSession(ctx context.Context, runner agentRunner, session sessionInfo) tea.Cmd {
	return func() tea.Msg {
		messages, err := runner.LoadSession(ctx, session.ThreadID)
		return sessionLoadedMsg{session: session, messages: messages, err: err}
	}
}

func (model *tuiModel) finishSessionList(message sessionsLoadedMsg) {
	if model.sessionPicker == nil {
		return
	}
	model.sessionPicker.loading = false
	model.sessionPicker.sessions = message.sessions
	model.sessionPicker.err = message.err
	for index, session := range message.sessions {
		if session.ThreadID == model.threadID {
			model.sessionPicker.selected = index
			break
		}
	}
}

func (model *tuiModel) finishSessionLoad(message sessionLoadedMsg) tea.Cmd {
	if model.sessionPicker == nil {
		return nil
	}
	if message.err != nil {
		model.sessionPicker.resuming = false
		model.sessionPicker.err = message.err
		return nil
	}
	model.threadID = message.session.ThreadID
	model.restoreTranscript(message.messages)
	model.sessionPicker = nil
	model.status = "Resumed session"
	model.refreshTranscript()
	if strings.TrimSpace(model.initial) != "" {
		return model.submitPrompt(model.initial)
	}
	return nil
}

func (model *tuiModel) handleSessionKey(message tea.KeyMsg) (tea.Cmd, bool) {
	picker := model.sessionPicker
	if picker == nil {
		return nil, false
	}
	switch message.String() {
	case "esc", "q", "ctrl+c":
		if picker.startup {
			return tea.Quit, true
		}
		model.sessionPicker = nil
		return nil, true
	case "up", "k":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = (picker.selected - 1 + len(picker.sessions)) % len(picker.sessions)
		}
		return nil, true
	case "down", "j":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = (picker.selected + 1) % len(picker.sessions)
		}
		return nil, true
	case "pgup":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = max(picker.selected-model.sessionPageSize(), 0)
		}
		return nil, true
	case "pgdown":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = min(picker.selected+model.sessionPageSize(), len(picker.sessions)-1)
		}
		return nil, true
	case "enter":
		if !picker.loading && !picker.resuming && picker.err == nil && len(picker.sessions) > 0 {
			picker.resuming = true
			return loadSession(model.ctx, model.runner, picker.sessions[picker.selected]), true
		}
		return nil, true
	}
	return nil, true
}

func (model *tuiModel) restoreTranscript(messages []damessage.Message) {
	model.items = nil
	model.toolItems = map[string]int{}
	model.currentAssistant = -1
	model.totalTokens = 0
	for _, message := range messages {
		switch message.Role {
		case damessage.RoleHuman:
			if text := message.TextContent(); text != "" {
				model.appendItem(transcriptItem{kind: itemUser, text: text})
			}
		case damessage.RoleAssistant:
			if text := message.TextContent(); text != "" {
				model.appendItem(transcriptItem{kind: itemAssistant, text: text})
			}
			for _, call := range message.ToolCalls {
				model.appendItem(transcriptItem{
					kind: itemTool, callID: call.ID, name: call.Name, args: compactJSON(call.Arguments),
				})
				model.toolItems[call.ID] = len(model.items) - 1
			}
			if message.Usage != nil {
				model.totalTokens = message.Usage.TotalTokens
			}
		case damessage.RoleTool:
			index, exists := model.toolItems[message.ToolCallID]
			if !exists {
				model.appendItem(transcriptItem{kind: itemTool, callID: message.ToolCallID, name: message.Name})
				index = len(model.items) - 1
				model.toolItems[message.ToolCallID] = index
			}
			item := &model.items[index]
			if message.Name != "" {
				item.name = message.Name
			}
			item.text = message.TextContent()
			item.done = true
			item.failed = message.ToolStatus == damessage.ToolStatusError
		case damessage.RoleSystem:
		}
	}
}

func (model *tuiModel) renderSessionPicker() string {
	picker := model.sessionPicker
	if picker == nil {
		return ""
	}
	width := max(model.width-4, 20)
	title := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Previous sessions")
	subtitle := lipgloss.NewStyle().Foreground(colorMuted).Render("Choose a session to continue")
	lines := []string{title, subtitle, ""}
	switch {
	case picker.loading:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(model.spinner.View()+" Loading sessions…"))
	case picker.err != nil:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Render(picker.err.Error()))
	case len(picker.sessions) == 0:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No previous sessions yet."))
	default:
		pageSize := model.sessionPageSize()
		start := max(picker.selected-pageSize+1, 0)
		end := min(start+pageSize, len(picker.sessions))
		directoryWidth := min(max(width/4, 12), 28)
		header := fmt.Sprintf("  %-9s  %-9s  %-*s  %7s  %s", "UPDATED", "SESSION", directoryWidth, "DIRECTORY", "MESSAGES", "PROMPT")
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(header, width-4)))
		for index := start; index < end; index++ {
			session := picker.sessions[index]
			marker := "  "
			if index == picker.selected {
				marker = "› "
			}
			current := ""
			if session.ThreadID == model.threadID {
				current = " (current)"
			}
			preview := strings.ReplaceAll(strings.TrimSpace(session.Preview), "\n", " ")
			if preview == "" {
				preview = "(no user prompt)"
			}
			directory := "-"
			if session.Directory != "" {
				directory = truncate(shortPath(session.Directory), directoryWidth)
			}
			metadata := fmt.Sprintf("%s%-9s  %-9s  %-*s  %3d msg  ", marker, sessionAge(session.UpdatedAt), truncate(session.ThreadID, 9), directoryWidth, directory, session.MessageCount)
			line := truncate(metadata+preview+current, width-4)
			style := lipgloss.NewStyle().Foreground(colorBody).Padding(0, 1).Width(max(width-2, 1))
			if index == picker.selected {
				style = style.Background(colorPrimary).Foreground(colorBackground).Bold(true)
			}
			lines = append(lines, style.Render(line))
		}
	}
	lines = append(lines, "")
	hint := "↑↓ navigate  •  Enter resume  •  Esc cancel"
	if picker.startup {
		hint = "↑↓ navigate  •  Enter resume  •  q quit"
	}
	if picker.resuming {
		hint = model.spinner.View() + " Resuming session…"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(hint))
	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Foreground(colorBody).Border(lipgloss.RoundedBorder()).
		BorderForeground(colorPrimary).Padding(1, 2).Width(max(model.width-2, 1)).Render(body)
}

func (model *tuiModel) sessionPageSize() int {
	return max(model.height-10, 1)
}

func sessionAge(updated time.Time) string {
	if updated.IsZero() {
		return "unknown"
	}
	difference := time.Since(updated)
	if difference < time.Minute {
		return "now"
	}
	if difference < time.Hour {
		return fmt.Sprintf("%dm ago", int(difference.Minutes()))
	}
	if difference < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(difference.Hours()))
	}
	if difference < 7*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(difference.Hours()/24))
	}
	return updated.Local().Format("Jan 2")
}
