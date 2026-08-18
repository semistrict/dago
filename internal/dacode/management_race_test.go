package dacode

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/daproviders/openai"
)

func TestAuthRefreshAppliesOnlyLatestImmutableGeneration(t *testing.T) {
	manager := newAuthManager(dacredential.NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, dacredential.Options{}), func(string) (string, bool) { return "", false })
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.authManager = newAuthTUIController(manager, filepath.Join(t.TempDir(), "oauth.json"), fixturelessSubscriptionLogin, func(string) error { return nil })
	model.authManager.open = true
	first := model.refreshAuthManagerWithNotice("first")
	second := model.refreshAuthManagerWithNotice("second")
	if len(manager.state.rows) != 0 {
		t.Fatal("refresh worker mutated live manager before Update")
	}
	secondMessage := second().(authRefreshMsg)
	model.handleAuthRefresh(secondMessage)
	firstMessage := first().(authRefreshMsg)
	model.handleAuthRefresh(firstMessage)
	if manager.state.notice != "second" || len(manager.state.rows) == 0 {
		t.Fatalf("auth refresh state = notice %q rows %d", manager.state.notice, len(manager.state.rows))
	}
}

func fixturelessSubscriptionLogin(context.Context, func(string) error, openai.OAuthOptions) (*openai.OAuthSession, error) {
	return nil, nil
}

func TestAutoClassifierValidationIgnoresStaleSameSpecIntent(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.configureAutoClassifier("fixture:main", "fixture:classifier", &fixtureClassifierPreferenceRecorder{})
	model.autoClassifier.pendingModel = "fixture:same"
	model.autoClassifier.pendingPersist = false
	model.autoClassifierValidationGeneration = 2
	command := model.finishAutoClassifierValidation(autoClassifierValidatedMsg{
		generation: 1, spec: "fixture:same", persist: true,
		validation: autoClassifierValidation{ModelAvailable: true, CredentialsAvailable: true, StructuredOutput: autoClassifierStructuredSupported},
	})
	if command != nil || model.autoClassifier.sessionModel != "" || model.autoClassifier.pendingPersist {
		t.Fatal("stale same-spec validation mutated classifier intent")
	}
}

type fixtureClassifierPreferenceRecorder struct{ value string }

func (recorder *fixtureClassifierPreferenceRecorder) Set(_ context.Context, value string) error {
	recorder.value = value
	return nil
}
func (recorder *fixtureClassifierPreferenceRecorder) Clear(context.Context) error {
	recorder.value = ""
	return nil
}

func TestMCPClosedLoginFlowLeavesDeterministicTerminalState(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	flow := &mcpLoginFlow{}
	model.mcpLogin = &mcpLoginState{generation: 7, phase: mcpLoginWaiting, input: []byte("private"), flow: flow}
	model.handleMCPLoginEvent(mcpLoginEventMsg{flow: flow, closed: true})
	if model.mcpLogin.phase != mcpLoginCancelled || model.mcpLogin.flow != nil || len(model.mcpLogin.input) != 0 {
		t.Fatalf("closed login state = %#v", model.mcpLogin)
	}
}

func TestManagementModalControlKeysClearSensitiveStateAndQuitDeliberately(t *testing.T) {
	manager := newAuthManager(dacredential.NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, dacredential.Options{}), func(string) (string, bool) { return "", false })
	manager.state.mode = authManagerAPIKey
	_ = manager.state.secret.append("private-value")
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.authManager = newAuthTUIController(manager, filepath.Join(t.TempDir(), "oauth.json"), fixturelessSubscriptionLogin, func(string) error { return nil })
	model.authManager.open = true
	if command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}); !handled || command != nil {
		t.Fatalf("Ctrl+C = handled %t command %v", handled, command)
	}
	if model.authManager.open || len(manager.state.secret.value) != 0 {
		t.Fatal("Ctrl+C did not close auth and clear secret")
	}
	model.mcpLogin = &mcpLoginState{input: []byte("private")}
	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !handled || command == nil || model.mcpLogin != nil {
		t.Fatalf("Ctrl+D = handled %t command %v login %#v", handled, command, model.mcpLogin)
	}
}
