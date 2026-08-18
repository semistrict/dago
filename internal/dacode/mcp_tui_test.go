package dacode

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMCPWorkspaceSelectionAcceptsOptionalEmptyValue(t *testing.T) {
	responses := make(chan string, 1)
	flow := &mcpLoginFlow{responses: responses}
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.mcpLogin = &mcpLoginState{phase: mcpLoginWorkspace, flow: flow}
	model.handleMCPLoginKey(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case value := <-responses:
		if value != "" {
			t.Fatalf("workspace value = %q", value)
		}
	default:
		t.Fatal("empty optional workspace was not submitted")
	}
}

func TestMCPAuthorizeEntryPreservesCopyShortcutRune(t *testing.T) {
	responses := make(chan string, 1)
	flow := &mcpLoginFlow{responses: responses}
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.mcpLogin = &mcpLoginState{phase: mcpLoginAuthorize, flow: flow}
	callback := "https://127.0.0.1/callback?code=ok"
	for _, character := range callback {
		model.handleMCPLoginKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{character}})
	}
	model.handleMCPLoginKey(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case value := <-responses:
		if value != callback {
			t.Fatalf("callback = %q, want %q", value, callback)
		}
	default:
		t.Fatal("callback was not submitted")
	}
}

func TestMCPAuthorizeURLValidationRejectsSecretAndTerminalInjection(t *testing.T) {
	for _, raw := range []string{
		"http://example.test/login",
		"https://user@example.test/login",
		"https://example.test/login#fragment",
		"https://example.test/login?access_token=hidden",
		"https://example.test/login\x1b]52;c;forged",
	} {
		if _, _, err := validateMCPAuthorizeURL(raw); err == nil {
			t.Fatalf("unsafe authorization URL accepted: %q", raw)
		}
	}
	full, display, err := validateMCPAuthorizeURL("https://example.test/login?client_id=public&state=opaque")
	if err != nil || !strings.Contains(full, "state=opaque") || strings.Contains(display, "?") {
		t.Fatalf("safe URL = %q / %q, %v", full, display, err)
	}
}

func TestMCPLoginIgnoresLateEventFromCancelledFlow(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	current := &mcpLoginFlow{}
	model.mcpLogin = &mcpLoginState{generation: 2, flow: current, phase: mcpLoginWaiting}
	late := &mcpLoginFlow{}
	if command := model.handleMCPLoginEvent(mcpLoginEventMsg{
		flow: late, event: mcpLoginEvent{generation: 1, phase: mcpLoginSucceeded},
	}); command != nil {
		t.Fatal("late flow scheduled follow-up work")
	}
	if model.mcpLogin == nil || model.mcpLogin.flow != current || model.mcpReconnectPrompt != nil {
		t.Fatal("late flow mutated the active login")
	}
}
