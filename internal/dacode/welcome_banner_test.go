package dacode

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestWelcomeBannerShowsUsefulReadyState(t *testing.T) {
	view := ansi.Strip(renderWelcomeBanner(welcomeBannerState{
		Version: "v1.2.3", Model: "openai:gpt-test", WorkingDirectory: "/workspace/project", Agent: "reviewer",
		ApprovalMode: "auto review", ThreadID: "thread-123", MCPTools: 7, MCPLoginRequired: 1, MCPErrors: 2,
		ShowVersion: true, ShowModel: true, ShowWorkingDirectory: true, ShowThreadID: true, Ready: true,
	}, 100, unicodeUIGlyphs))
	for _, wanted := range []string{
		"dacode v1.2.3", "agent:reviewer", "openai:gpt-test", "auto review", "Working directory: /workspace/project",
		"Thread ID: thread-123", "MCP: 7 tools • 1 need login • 2 errors", "Ready to code", "⏎ newline",
	} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("banner missing %q:\n%s", wanted, view)
		}
	}
}

func TestWelcomeBannerUsesASCIIAndBoundsUnsafeFailure(t *testing.T) {
	view := ansi.Strip(renderWelcomeBanner(welcomeBannerState{
		StartupError: "failed\x1b[31m\nsecond line " + strings.Repeat("x", 200),
	}, 48, asciiUIGlyphs))
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
		t.Fatalf("ASCII banner contains Unicode UI glyphs:\n%s", view)
	}
	for _, wanted := range []string{"dacode", "[X] Startup failed", "failed<U+001B CONTROL>[31m", "[~] Starting"} {
		contained := strings.Contains(view, wanted)
		if wanted == "[~] Starting" {
			contained = !contained
		}
		if !contained {
			t.Fatalf("banner expectation %q failed:\n%s", wanted, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if len([]rune(line)) > 60 {
			t.Fatalf("unbounded line (%d): %q", len([]rune(line)), line)
		}
	}
}

func TestWelcomeMCPStatusUsesZeroValueUsefully(t *testing.T) {
	if got := welcomeMCPStatus(welcomeBannerState{}, unicodeUIGlyphs); got != "" {
		t.Fatalf("zero MCP status = %q", got)
	}
	if got := welcomeMCPStatus(welcomeBannerState{MCPAwaitingReconnect: 2}, asciiUIGlyphs); got != "MCP: 0 tools - 2 awaiting reconnect" {
		t.Fatalf("reconnect status = %q", got)
	}
}

func TestWelcomeBannerBoundsExternalStateAndWideCells(t *testing.T) {
	view := renderWelcomeBanner(welcomeBannerState{
		Version: strings.Repeat("v", 10_000), Model: strings.Repeat("模", 10_000),
		WorkingDirectory: strings.Repeat("/path", 10_000), Agent: strings.Repeat("a", 10_000),
		StartupError: strings.Repeat("界", 10_000), MCPTools: int(^uint(0) >> 1), MCPErrors: -100,
		ShowVersion: true, ShowModel: true, ShowWorkingDirectory: true,
	}, 40, unicodeUIGlyphs)
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if ansi.StringWidth(line) > 46 {
			t.Fatalf("line width = %d: %q", ansi.StringWidth(line), line)
		}
	}
	if strings.Contains(view, "1000001 tools") || strings.Contains(view, "-100") {
		t.Fatalf("unbounded counts rendered:\n%s", ansi.Strip(view))
	}
}

func TestWelcomeBannerProjectsBoundedSafeHitTargetsAcrossPhases(t *testing.T) {
	base := welcomeBannerState{
		Version: "v1.2.3", ThreadID: "thread-1", ProjectLabel: "project", ProjectURL: "https://example.test/project", WorkingDirectory: "/work/project",
		MCPTools: 2, ShowVersion: true, ShowThreadID: true, ShowProject: true,
	}
	for _, phase := range []struct {
		name  string
		ready bool
		err   string
		want  string
	}{
		{name: "starting", want: "Starting agent"},
		{name: "ready", ready: true, want: "Ready to code"},
		{name: "failure", err: "startup failed", want: "Startup failed"},
	} {
		t.Run(phase.name, func(t *testing.T) {
			state := base
			state.Ready, state.StartupError = phase.ready, phase.err
			layout := renderWelcomeBannerLayout(state, 72, asciiUIGlyphs)
			if !strings.Contains(ansi.Strip(layout.View), phase.want) {
				t.Fatalf("phase view:\n%s", ansi.Strip(layout.View))
			}
			kinds := make([]welcomeHitTargetKind, len(layout.HitTargets))
			for index, target := range layout.HitTargets {
				kinds[index] = target.Kind
				if target.Width < 1 || target.Width > 68 || target.X < 0 || target.Y < 0 || strings.ContainsAny(target.Label, "\x1b\n\r") {
					t.Fatalf("unsafe target = %#v", target)
				}
			}
			for _, kind := range []welcomeHitTargetKind{welcomeHitVersion, welcomeHitThread, welcomeHitMCP, welcomeHitProject} {
				if !slices.Contains(kinds, kind) {
					t.Fatalf("missing target %q: %#v", kind, layout.HitTargets)
				}
			}
		})
	}
}
