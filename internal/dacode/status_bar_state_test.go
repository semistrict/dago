package dacode

import (
	"math"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTwoLineStatusBarHasUsefulDefault(t *testing.T) {
	view := ansi.Strip(renderTwoLineStatusBar(newStatusBarState(), 80, "", unicodeUIGlyphs))
	lines := strings.Split(view, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "manual") || !strings.Contains(lines[1], "Context: 0% / Tokens: 0") {
		t.Fatalf("status bar:\n%s", view)
	}
}

func TestTwoLineStatusBarRendersSessionAndMetricsByPriority(t *testing.T) {
	state := statusBarState{
		InputMode: "shell", ApprovalMode: "auto", Connection: "connecting",
		AgentStatus: "thinking", HookStatus: "running hook", WorkingDirectory: "/workspace/project",
		Branch: "feature", Rubric: "active", Model: "provider:model", Effort: "high", Queued: 2,
		Tokens: 60_000, ContextLimit: 100_000, CacheInput: 10_000, CacheRead: 7_500, CacheWrite: 2_000, CostUSD: 1.25,
	}
	view := ansi.Strip(renderTwoLineStatusBar(state, 220, "*", asciiUIGlyphs))
	for _, wanted := range []string{
		"SHELL", "auto", "/workspace/project", "git: feature", "rubric:active", "provider:model high",
		"* Connecting - 2 messages queued", "running hook", "Cache: 75% read / 2k write", "Context: 60% / Tokens: 60k", "$1.25",
	} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("status missing %q:\n%s", wanted, view)
		}
	}
	if strings.Contains(view, "thinking") {
		t.Fatalf("agent status outranked hook status:\n%s", view)
	}
}

func TestStatusBusyOverridesSourcesAndControlsSpinner(t *testing.T) {
	state := statusBarState{Connection: "invalid", AgentStatus: "agent", HookStatus: "hook", BusyStatus: "Switching model"}
	if !statusSpinnerActive(state) {
		t.Fatal("busy status did not activate spinner")
	}
	view := ansi.Strip(renderTwoLineStatusBar(state, 80, "#", asciiUIGlyphs))
	if !strings.Contains(view, "# Switching model") || strings.Contains(view, "hook") || strings.Contains(view, "agent") || strings.Contains(view, "Connecting") {
		t.Fatalf("busy status priority:\n%s", view)
	}
	if statusSpinnerActive(statusBarState{Connection: "invalid"}) {
		t.Fatal("invalid connection activated spinner")
	}
}

func TestStatusBarBoundsAndSanitizesDynamicState(t *testing.T) {
	state := statusBarState{
		ApprovalMode: "invalid", HookStatus: "unsafe\x1b[31m\n" + strings.Repeat("界", 10_000),
		Model: strings.Repeat("model", 10_000), Queued: int(^uint(0) >> 1), Tokens: math.MaxInt64,
		ContextLimit: -1, CacheInput: -1, CacheRead: math.MaxInt64, CostUSD: math.Inf(1),
	}
	view := renderTwoLineStatusBar(state, 40, "\x1b[31m", unicodeUIGlyphs)
	if strings.Contains(view, "\x1b[31m") {
		t.Fatalf("terminal control survived:\n%q", view)
	}
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Fatalf("line width = %d: %q", ansi.StringWidth(line), line)
		}
	}
	if !strings.Contains(ansi.Strip(view), "manual") {
		t.Fatalf("invalid approval mode did not fail closed:\n%s", ansi.Strip(view))
	}
}

func TestStatusBarStripsSpinnerStylingWithoutEscapingIt(t *testing.T) {
	view := renderTwoLineStatusBar(statusBarState{BusyStatus: "Responding"}, 80, "\x1b[38;5;111m⠹\x1b[0m", unicodeUIGlyphs)
	if strings.Contains(view, "<U+001B CONTROL>") || strings.Contains(view, "\x1b[38;5;111m") || !strings.Contains(view, "⠹ Responding") {
		t.Fatalf("styled spinner was not normalized safely:\n%q", view)
	}
}

func TestStatusBarProjectsPendingStaleBadgesAndThresholds(t *testing.T) {
	state := statusBarState{
		Spinner: true, Queued: 20, QueuePending: true, QueueStale: true,
		Tokens: 96, ContextLimit: 100, TokensPending: true, ContextStale: true,
		CacheInput: 100, CacheRead: 10, CachePending: true, CacheStale: true,
		CostUSD: 100, CostPending: true, CostStale: true,
	}
	if !statusSpinnerActive(state) {
		t.Fatal("explicit spinner projection was ignored")
	}
	view := ansi.Strip(renderTwoLineStatusBar(state, 300, "*", asciiUIGlyphs))
	for _, expected := range []string{
		"20 messages queued [~] pending ~ stale", "Context: ... / Tokens: ... ~ stale",
		"Cache: 10% read / 0 write [~] pending ~ stale", "$100.00 [~] pending ~ stale",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q:\n%s", expected, view)
		}
	}
	if statusQueueThreshold(19) != statusThresholdWarning || statusQueueThreshold(20) != statusThresholdCritical ||
		statusContextThreshold(statusBarState{Tokens: 80, ContextLimit: 100}) != statusThresholdCritical ||
		statusContextThreshold(statusBarState{Tokens: 60, ContextLimit: 100}) != statusThresholdWarning ||
		statusCacheThreshold(100, 89) != statusThresholdWarning || statusCacheThreshold(100, 59) != statusThresholdCritical ||
		statusCostThreshold(10) != statusThresholdWarning || statusCostThreshold(100) != statusThresholdCritical {
		t.Fatal("status threshold boundary changed")
	}
}

func TestStatusBarTruncatesANSIAndWideRunesToExactWidth(t *testing.T) {
	state := statusBarState{Branch: strings.Repeat("界", 100), Model: strings.Repeat("模", 100), BusyStatus: "busy"}
	view := renderTwoLineStatusBar(state, 21, "*", unicodeUIGlyphs)
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if ansi.StringWidth(line) > 21 {
			t.Fatalf("line width = %d: %q", ansi.StringWidth(line), line)
		}
	}
}
