package dacode

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type effortPickerState struct {
	context  reasoningEffortContext
	selected int
}

type reasoningEffortChangedMsg struct {
	spec        string
	level       string
	hadOverride bool
	err         error
}

func (model *tuiModel) effortCommand(arguments string) tea.Cmd {
	context := model.runner.ReasoningEffort()
	if arguments == "" {
		if len(context.Levels) == 0 {
			model.appendItem(transcriptItem{kind: itemUser, text: "/effort"})
			model.appendItem(transcriptItem{kind: itemNotice, text: effortUnavailableMessage(context)})
			model.refreshTranscript()
			return nil
		}
		selected := effortSelectionIndex(context)
		model.effortPicker = &effortPickerState{context: context, selected: selected}
		return nil
	}

	model.appendItem(transcriptItem{kind: itemUser, text: "/effort " + arguments})
	if isEffortClear(arguments) {
		if context.ModelSpec == "" {
			model.appendItem(transcriptItem{kind: itemNotice, text: effortUnavailableMessage(context)})
			model.refreshTranscript()
			return nil
		}
		return model.changeReasoningEffort(context, "")
	}
	matched := matchReasoningEffort(context.Levels, arguments)
	if matched == "" {
		if len(context.Levels) == 0 {
			model.appendItem(transcriptItem{kind: itemNotice, text: effortUnavailableMessage(context)})
		} else {
			model.appendItem(transcriptItem{kind: itemError, text: fmt.Sprintf(
				"Unsupported reasoning effort %q for %s. Supported efforts: %s",
				arguments, context.ModelSpec, strings.Join(context.Levels, ", "),
			)})
		}
		model.refreshTranscript()
		return nil
	}
	return model.changeReasoningEffort(context, matched)
}

func (model *tuiModel) changeReasoningEffort(context reasoningEffortContext, level string) tea.Cmd {
	return func() tea.Msg {
		return reasoningEffortChangedMsg{
			spec: context.ModelSpec, level: level, hadOverride: context.Current != "",
			err: model.runner.SetReasoningEffort(level),
		}
	}
}

func (model *tuiModel) finishReasoningEffortChange(message reasoningEffortChangedMsg) {
	if message.level == "" {
		if message.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: fmt.Sprintf(
				"Reasoning effort cleared for %s in this session, but the saved preference could not be removed.", message.spec,
			)})
		} else if message.hadOverride {
			model.appendItem(transcriptItem{kind: itemNotice, text: "Reasoning effort override cleared for " + message.spec + "."})
		} else {
			model.appendItem(transcriptItem{kind: itemNotice, text: "No reasoning effort override was set for " + message.spec + "."})
		}
	} else if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: fmt.Sprintf(
			"Reasoning effort for %s set to %s in this session, but the preference could not be saved.", message.spec, message.level,
		)})
	} else {
		model.appendItem(transcriptItem{kind: itemNotice, text: fmt.Sprintf("Reasoning effort for %s set to %s.", message.spec, message.level)})
	}
	model.refreshTranscript()
}

func (model *tuiModel) handleEffortKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	picker := model.effortPicker
	if picker == nil {
		return nil, false
	}
	switch message.String() {
	case "esc", "q", "ctrl+c":
		model.effortPicker = nil
		return nil, true
	case "up", "k", "shift+tab":
		picker.selected = (picker.selected - 1 + len(picker.context.Levels)) % len(picker.context.Levels)
		return nil, true
	case "down", "j", "tab":
		picker.selected = (picker.selected + 1) % len(picker.context.Levels)
		return nil, true
	case "enter":
		level := picker.context.Levels[picker.selected]
		context := picker.context
		model.effortPicker = nil
		return model.changeReasoningEffort(context, level), true
	}
	return nil, true
}

func (model *tuiModel) renderEffortPicker() string {
	picker := model.effortPicker
	if picker == nil {
		return ""
	}
	contentWidth := min(max(model.width-16, 36), 54)
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Select Reasoning Effort"),
		lipgloss.NewStyle().Foreground(colorMuted).Render(unicodesecurity.RenderTerminalSafe(picker.context.ModelSpec)),
		"",
	}
	for index, effort := range picker.context.Levels {
		marker := "  "
		if index == picker.selected {
			marker = model.glyphs.Cursor + " "
		}
		labels := make([]string, 0, 2)
		if effort == picker.context.Current {
			labels = append(labels, "current")
		}
		if effort == picker.context.Default {
			labels = append(labels, "default")
		}
		label := ""
		if len(labels) > 0 {
			label = " (" + strings.Join(labels, ", ") + ")"
		}
		line := marker + unicodesecurity.RenderTerminalSafe(effort) + label
		style := lipgloss.NewStyle().Foreground(colorBody).Padding(0, 1).Width(contentWidth)
		if index == picker.selected {
			style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
		}
		lines = append(lines, style.Render(line))
	}
	separator := "  " + model.glyphs.Bullet + "  "
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(model.glyphs.ArrowUp+"/"+model.glyphs.ArrowDown+" or Tab switch"+separator+"Enter select"+separator+"Esc cancel"))
	panel := lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
}

func effortSelectionIndex(context reasoningEffortContext) int {
	wanted := context.Current
	if wanted == "" {
		wanted = context.Default
	}
	for index, level := range context.Levels {
		if level == wanted {
			return index
		}
	}
	return 0
}

func matchReasoningEffort(levels []string, value string) string {
	for _, level := range levels {
		if strings.EqualFold(level, value) {
			return level
		}
	}
	return ""
}

func isEffortClear(value string) bool {
	switch strings.ToLower(value) {
	case "clear", "--clear", "reset":
		return true
	default:
		return false
	}
}

func effortUnavailableMessage(context reasoningEffortContext) string {
	if context.ModelSpec == "" {
		return "No model is configured yet. Run `/model` to choose one."
	}
	return "Reasoning effort is not configurable for " + context.ModelSpec + "."
}

func effectiveReasoningEffort(context reasoningEffortContext) string {
	if context.Current != "" {
		return context.Current
	}
	return context.Default
}
