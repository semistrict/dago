package dacode

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (model *tuiModel) handleNotificationShortcut(message tea.KeyPressMsg) (tea.Cmd, bool) {
	if message.String() != "ctrl+n" {
		return nil, false
	}
	// The model selector owns Ctrl+N for friendly-name/model-ID display.
	if model.modelSelector != nil {
		return nil, false
	}
	if model.notificationCenter != nil {
		return nil, true
	}
	if model.notificationDialogBlocked() {
		return model.notify("Close the current dialog before opening notifications.", toastInfo, ""), true
	}
	entries := model.notifications.list()
	if len(entries) == 0 {
		return model.notify("No pending notifications.", toastInfo, ""), true
	}
	if model.toasts != nil {
		for _, dismissed := range model.toasts.dismissActionable() {
			model.notifications.unbindToast(notificationToastIdentity(dismissed.ID))
		}
		model.relayout()
	}
	model.notificationCenter = newNotificationCenter(entries)
	return nil, true
}

func (model *tuiModel) notificationDialogBlocked() bool {
	return model.debugConsole != nil || model.yoloModeNotice || model.autoModeNotice || model.pluginManager != nil ||
		model.pluginReloadPrompt || model.pluginReloading || model.onboarding != nil ||
		model.updateModal != nil ||
		(model.authManager != nil && model.authManager.open) || model.mcpLogin != nil || model.mcpReconnectPrompt != nil ||
		model.mcpErrorDetail != "" || model.mcpViewer != nil || model.notificationSettings != nil ||
		model.installSelector != nil || model.restartPrompt != nil || model.restarting || model.themePicker != nil ||
		model.sessionPicker != nil || model.effortPicker != nil || model.agentPicker != nil || model.skillTrust != nil ||
		model.contextScreen || model.goalReview != nil || model.askUser != nil
}

func notificationToastIdentity(id uint64) string {
	return "toast:" + uint64Decimal(id)
}

func uint64Decimal(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}

func (model *tuiModel) renderModalWithToasts(content string) string {
	if model == nil || model.height < 1 {
		return content
	}
	input := lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorPrimary).
		Padding(0, 1).Width(max(model.width-4, 1)).Render(model.composer.View())
	chrome := input + "\n" + model.renderStatus()
	chromeHeight := len(nonBlankRenderedLines(chrome))
	maximumToastHeight := max(min(model.height/3, model.height-chromeHeight-3), 0)
	toast := renderToastsWithin(model.toasts, model.width, maximumToastHeight, model.glyphs, time.Now())
	toastHeight := 0
	if toast != "" {
		toastHeight = lipgloss.Height(toast)
	}
	remaining := max(model.height-toastHeight-chromeHeight, 1)
	lines := nonBlankRenderedLines(content)
	if len(lines) > remaining {
		head := max(remaining/2, 1)
		tail := max(remaining-head, 0)
		trimmed := append([]string(nil), lines[:head]...)
		if tail > 0 {
			trimmed = append(trimmed, lines[len(lines)-tail:]...)
		}
		lines = trimmed
	}
	if len(lines) < remaining {
		top := (remaining - len(lines)) / 2
		bottom := remaining - len(lines) - top
		padded := make([]string, 0, remaining)
		for range top {
			padded = append(padded, " ")
		}
		padded = append(padded, lines...)
		for range bottom {
			padded = append(padded, " ")
		}
		lines = padded
	}
	parts := make([]string, 0, 3)
	if toast != "" {
		parts = append(parts, toast)
	}
	parts = append(parts, strings.Join(lines, "\n"), chrome)
	return strings.Join(parts, "\n") + model.terminalSequences() + model.themeSequence
}

func nonBlankRenderedLines(content string) []string {
	lines := strings.Split(content, "\n")
	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(ansi.Strip(lines[start])) == "" {
		start++
	}
	for end > start && strings.TrimSpace(ansi.Strip(lines[end-1])) == "" {
		end--
	}
	if start == end {
		return []string{""}
	}
	return lines[start:end]
}
