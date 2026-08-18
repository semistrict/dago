package dacode

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRestartPromptUsesDefaultAndExplicitBody(t *testing.T) {
	state := newRestartPrompt("Tavily API key", "Saved")
	if state.body != defaultRestartPromptBody || state.handleKey("enter") != restartPromptRestart || state.handleKey("esc") != restartPromptLater {
		t.Fatalf("state = %#v", state)
	}
	view := ansi.Strip(renderRestartPrompt(state, 80, 24, unicodeUIGlyphs))
	for _, wanted := range []string{"Saved Tavily API key", defaultRestartPromptBody, "Enter to restart", "Esc to defer"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}

	override := newRestartPrompt("package", "Installed", "Restart to activate its tools.")
	if override.body != "Restart to activate its tools." {
		t.Fatalf("override body = %q", override.body)
	}
}

func TestRestartPromptRendersExternalTextSafelyAndBoundsIt(t *testing.T) {
	state := newRestartPrompt("unsafe\x1b[31m", "Saved", strings.Repeat("x", 3000))
	view := ansi.Strip(renderRestartPrompt(state, 80, 24, unicodeUIGlyphs))
	if strings.Contains(view, "\x1b[31m") || !strings.Contains(view, "<U+001B CONTROL>") || len([]rune(state.body)) > 2048 {
		t.Fatalf("unsafe or unbounded prompt:\n%s", view)
	}
}

func TestRestartPromptRendersASCIIUI(t *testing.T) {
	view := ansi.Strip(renderRestartPrompt(newRestartPrompt("package", "Installed"), 80, 24, asciiUIGlyphs))
	if !strings.Contains(view, "[OK] Installed package") || strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
		t.Fatalf("ASCII restart prompt contains Unicode UI glyphs:\n%s", view)
	}
}

func TestRestartPromptRejectsInvalidStaticArguments(t *testing.T) {
	for _, build := range []func(){
		func() { newRestartPrompt("", "Saved") },
		func() { newRestartPrompt("package", "") },
		func() { newRestartPrompt("package", "Saved", "one", "two") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid static restart arguments did not panic")
				}
			}()
			build()
		}()
	}
}
