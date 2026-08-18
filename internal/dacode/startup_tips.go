package dacode

import (
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/internal/unicodesecurity"
)

const hideSplashTipsEnvironment = "DEEPAGENTS_CODE_HIDE_SPLASH_TIPS"

const (
	maxStartupTips    = 256
	maxStartupTipText = 256
)

type weightedStartupTip struct {
	Text   string
	Weight uint32
}

type startupTipAudience uint8

const (
	startupTipFresh startupTipAudience = iota
	startupTipResumed
	startupTipFallback
)

const fallbackStartupTip = "Use /help to see available commands"

type startupTipState struct {
	Text      string
	Visible   bool
	Dismissed bool
}

func newStartupTipState(audience startupTipAudience, editor string, yoloSwitcher bool, sample uint64, visible bool) startupTipState {
	tip := chooseStartupTip(startupTipsForAudience(audience, editor, yoloSwitcher), sample)
	if tip == "" {
		tip = fallbackStartupTip
	}
	return startupTipState{Text: boundedStartupTipText(tip), Visible: visible}
}

func (state *startupTipState) dismissOnFirstSubmit() bool {
	if state == nil {
		panic("dacode: initialized startup tip state is required")
	}
	if state.Dismissed {
		return false
	}
	state.Dismissed = true
	wasVisible := state.Visible
	state.Visible = false
	return wasVisible
}

func (state *startupTipState) show() bool {
	if state == nil {
		panic("dacode: initialized startup tip state is required")
	}
	if state.Dismissed {
		return false
	}
	state.Visible = true
	return true
}

func startupTipsForAudience(audience startupTipAudience, editor string, yoloSwitcher bool) []weightedStartupTip {
	tips := activeStartupTips(editor, yoloSwitcher)
	switch audience {
	case startupTipFresh:
		return tips
	case startupTipResumed:
		return append([]weightedStartupTip{{Text: "Use /threads to switch back to another conversation", Weight: 3}}, tips...)
	default:
		return []weightedStartupTip{{Text: fallbackStartupTip, Weight: 1}}
	}
}

func activeStartupTips(editor string, yoloSwitcher bool) []weightedStartupTip {
	editorTip := "Press Ctrl+X to compose prompts in your external editor"
	editor = strings.TrimSpace(editor)
	if editor != "" && utf8.RuneCountInString(editor) <= 80 && !hasModelSelectorControl(editor) {
		editorTip = "Press Ctrl+X to compose prompts in " + editor
	}
	modeTip := "Press Shift+Tab to cycle Manual, Auto, and YOLO modes"
	if !yoloSwitcher {
		modeTip = "Press Shift+Tab to toggle Manual and Auto modes"
	}
	return []weightedStartupTip{
		{Text: "Use @ to reference files and / for commands", Weight: 3},
		{Text: "Try /threads to resume a previous conversation", Weight: 2},
		{Text: "Use /offload to summarize older messages and free context space", Weight: 2},
		{Text: "Use /context to see context usage and remaining space", Weight: 1},
		{Text: "Use /copy to copy the latest response", Weight: 3},
		{Text: "Use /cost to see a breakdown of estimated spend", Weight: 1},
		{Text: "Use /tools to list the tools available to the agent", Weight: 1},
		{Text: "Use /mcp login <server> to authenticate MCP servers", Weight: 1},
		{Text: "Use /remember to save learnings from this conversation", Weight: 1},
		{Text: "Use /model to switch models mid-conversation", Weight: 2},
		{Text: "Use /effort to change the current model's reasoning effort", Weight: 1},
		{Text: editorTip, Weight: 1},
		{Text: "Use /skill:<name> to invoke a skill directly", Weight: 1},
		{Text: "Use /theme to customize the terminal colors", Weight: 1},
		{Text: "Use /skill-creator to build reusable agent skills", Weight: 1},
		{Text: "Ask for a workflow to fan work out to subagents in parallel", Weight: 3},
		{Text: "Use /timestamps to show or hide message timestamp footers", Weight: 1},
		{Text: "Click a collapsed message or press Ctrl+O to expand it", Weight: 1},
		{Text: "Use /agents to browse and switch available agents", Weight: 2},
		{Text: "Use /auto model to review Auto actions with a faster model", Weight: 1},
		{Text: modeTip, Weight: 2},
		{Text: "Use !! for private shell commands that stay out of model context", Weight: 1},
		{Text: "Ask the agent to explain a feature when you are unsure how it works", Weight: 3},
	}
}

func chooseStartupTip(tips []weightedStartupTip, sample uint64) string {
	var total uint64
	valid := 0
	for _, tip := range tips {
		if tip.Text == "" || tip.Weight == 0 {
			continue
		}
		if valid == maxStartupTips {
			break
		}
		total += uint64(tip.Weight)
		valid++
	}
	if total == 0 {
		return ""
	}
	target := sample % total
	valid = 0
	for _, tip := range tips {
		if tip.Text == "" || tip.Weight == 0 {
			continue
		}
		if valid == maxStartupTips {
			break
		}
		valid++
		weight := uint64(tip.Weight)
		if target < weight {
			return boundedStartupTipText(tip.Text)
		}
		target -= weight
	}
	return ""
}

func boundedStartupTipText(value string) string {
	value = strings.TrimSpace(value)
	characters := []rune(value)
	if len(characters) > maxStartupTipText {
		characters = characters[:maxStartupTipText]
	}
	value = strings.Map(func(character rune) rune {
		switch character {
		case '\n', '\r', '\t':
			return ' '
		default:
			return character
		}
	}, string(characters))
	return unicodesecurity.RenderTerminalSafe(strings.Join(strings.Fields(value), " "))
}

func startupTipsVisible(lookup func(string) (string, bool)) bool {
	if lookup == nil {
		return true
	}
	value, exists := lookup(hideSplashTipsEnvironment)
	return !exists || !truthyEnvironmentValue(value)
}

func truthyEnvironmentValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}
