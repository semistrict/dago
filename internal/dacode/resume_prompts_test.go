package dacode

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestCWDResumePromptChoicesAndModes(t *testing.T) {
	state := newCWDResumePrompt("/current", "/original", true, cwdResumeAbortLaunch)
	if state.handleKey("enter") != resumePromptSwitchCWD || state.handleKey("esc") != resumePromptStayCWD || state.handleKey("a") != resumePromptAbort {
		t.Fatal("launch prompt choices are incomplete")
	}
	view := ansi.Strip(renderCWDResumePrompt(state, 80, 24, unicodeUIGlyphs))
	for _, wanted := range []string{"Resume from the thread's original directory?", "/original", "/current", "reload project-specific config", "A: don't resume"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}

	state = newCWDResumePrompt("/current", "/original", false, cwdResumeAbortThreadSwitch)
	view = ansi.Strip(renderCWDResumePrompt(state, 80, 24, unicodeUIGlyphs))
	if !strings.Contains(view, "Switch to the thread's original directory?") || !strings.Contains(view, "A: don't switch") || strings.Contains(view, "start a new session") {
		t.Fatalf("thread-switch view:\n%s", view)
	}

	state = newCWDResumePrompt("/current", "/original", false, cwdResumeAbortNone)
	if state.handleKey("a") != resumePromptNoAction {
		t.Fatal("abort was enabled without an abort mode")
	}
}

func TestAgentResumePromptChoicesAndSafeRendering(t *testing.T) {
	state := newAgentResumePrompt("thread-1\x1b", "current", "other")
	if state.handleKey("enter") != resumePromptSwitchAgent || state.handleKey("esc") != resumePromptCancelAgent {
		t.Fatal("agent prompt choices are incomplete")
	}
	view := ansi.Strip(renderAgentResumePrompt(state, 80, 24, unicodeUIGlyphs))
	for _, wanted := range []string{"Switch agents to resume?", "thread-1<U+001B CONTROL>", "belongs to agent other", "saved default agent will not change"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
}

func TestCompactResumePromptChoicesAndCounts(t *testing.T) {
	state := newCompactResumePrompt(12500, 10000)
	if state.handleKey("enter") != resumePromptCompactNow || state.handleKey("esc") != resumePromptKeepContext {
		t.Fatal("compact prompt choices are incomplete")
	}
	view := ansi.Strip(renderCompactResumePrompt(state, 80, 24, unicodeUIGlyphs))
	for _, wanted := range []string{"Compact this thread?", "12.5k context tokens", "10k", "token threshold", "Enter: compact now"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
}

func TestResumePromptsRenderASCIIAndBoundInput(t *testing.T) {
	state := newCWDResumePrompt(strings.Repeat("x", 3000), "/original", false, cwdResumeAbortNone)
	if len([]rune(state.currentCWD)) > 2051 {
		t.Fatalf("current cwd length = %d", len([]rune(state.currentCWD)))
	}
	views := []string{
		renderCWDResumePrompt(state, 80, 24, asciiUIGlyphs),
		renderAgentResumePrompt(newAgentResumePrompt("thread", "current", "other"), 80, 24, asciiUIGlyphs),
		renderCompactResumePrompt(newCompactResumePrompt(-1, -1), 80, 24, asciiUIGlyphs),
	}
	for _, rendered := range views {
		view := ansi.Strip(rendered)
		if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
			t.Fatalf("ASCII resume prompt contains Unicode UI glyphs:\n%s", view)
		}
	}
}
