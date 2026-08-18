package dacode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/dacredential"
)

func TestAuthManagerRefreshBuildsDeterministicProviderAndServiceRows(t *testing.T) {
	store := newAuthManagerTestStore(t)
	if err := store.SetAPIKey(t.Context(), "openai", "stored-value", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(t.Context(), "custom_gateway", "custom-value", "", ""); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"TAVILY_API_KEY": "environment-value"}
	manager := newAuthManager(store, func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	})
	if err := manager.refresh(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	first := append([]authManagerRow(nil), manager.state.rows...)
	if err := manager.refresh(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(first) != fmt.Sprint(manager.state.rows) {
		t.Fatalf("refresh was not deterministic:\nfirst=%v\nsecond=%v", first, manager.state.rows)
	}
	openAI := findAuthManagerRow(t, manager.state.rows, "openai")
	if openAI.label != "OpenAI" || openAI.status != "configured" || openAI.source != "stored" || !openAI.removable {
		t.Fatalf("stored row = %#v", openAI)
	}
	tavily := findAuthManagerRow(t, manager.state.rows, "tavily")
	if !tavily.service || tavily.source != "environment" || tavily.detail != "From TAVILY_API_KEY" || tavily.removable {
		t.Fatalf("service row = %#v", tavily)
	}
	custom := findAuthManagerRow(t, manager.state.rows, "custom_gateway")
	if custom.label != "Custom Gateway" || custom.source != "stored" {
		t.Fatalf("custom stored row = %#v", custom)
	}
	oauth := findOAuthAuthManagerRow(t, manager.state.rows)
	if !oauth.configured || oauth.source != "subscription" || !oauth.removable {
		t.Fatalf("subscription row = %#v", oauth)
	}
	seenMissing := false
	for _, row := range manager.state.rows {
		if !row.configured {
			seenMissing = true
			continue
		}
		if seenMissing {
			t.Fatalf("configured row %q appeared after missing rows", row.provider)
		}
	}
}

func TestAuthManagerProviderLimitPreservesPinnedRegistry(t *testing.T) {
	pinned := dacredential.Providers()
	stored := make([]string, maxAuthManagerRows*2)
	for index := range stored {
		stored[index] = fmt.Sprintf("custom_%03d", index)
	}
	providers, truncated := mergeAuthManagerProviders(pinned, stored)
	if !truncated || len(providers) != maxAuthManagerRows {
		t.Fatalf("bounded providers = %d, truncated=%t", len(providers), truncated)
	}
	for _, expected := range pinned {
		found := false
		for _, provider := range providers {
			if provider.Name == expected.Name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("pinned provider %q was displaced by custom entries", expected.Name)
		}
	}
	if got := providers[len(pinned)].Name; got != "custom_000" {
		t.Fatalf("first custom provider = %q", got)
	}
}

func TestAuthManagerRefreshPreservesSelectionAndHonorsCancellation(t *testing.T) {
	manager := newAuthManager(newAuthManagerTestStore(t), func(string) (string, bool) { return "", false })
	if err := manager.refresh(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	manager.state.move(7)
	want := manager.state.selectedProvider()
	if err := manager.refresh(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if got := manager.state.selectedProvider(); got != want {
		t.Fatalf("selected provider = %q, want %q", got, want)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := manager.refresh(cancelled, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled refresh error = %v", err)
	}
}

func TestAuthManagerRefreshSurfacesMalformedRecordsWithoutExposingThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	payload := `{"version":1,"credentials":{"invalid provider":{"type":"api_key","key":"never-decoded","added_at":"2026-01-01T00:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := newAuthManager(dacredential.NewStore(path, time.Now, dacredential.Options{}), func(string) (string, bool) { return "", false })
	if err := manager.refresh(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	view := manager.state.render(100, 24)
	if !strings.Contains(view, "malformed credential entries were ignored") || strings.Contains(view, "never-decoded") {
		t.Fatalf("malformed-record notice = %q", view)
	}
}

func TestAuthManagerNavigationWrapsAndZeroStateIsUseful(t *testing.T) {
	var empty authManagerState
	if got := empty.render(0, 0); !strings.Contains(got, "No providers") {
		t.Fatalf("zero-state render = %q", got)
	}
	empty.move(1)
	if action := empty.beginSelected(); action.kind != authManagerNoAction {
		t.Fatalf("empty selection action = %#v", action)
	}
	state := authManagerState{rows: []authManagerRow{{provider: "a"}, {provider: "b"}, {provider: "c"}}}
	state.move(-1)
	if got := state.selectedProvider(); got != "c" {
		t.Fatalf("wrapped up selection = %q", got)
	}
	state.move(1)
	if got := state.selectedProvider(); got != "a" {
		t.Fatalf("wrapped down selection = %q", got)
	}
}

func TestAuthManagerAPIKeyEntryNeverRendersOrFormatsSecret(t *testing.T) {
	secret := "unique-super-secret-value"
	state := authManagerState{rows: []authManagerRow{{provider: "openai", label: "OpenAI"}}}
	if action := state.beginSelected(); action.kind != authManagerNoAction || state.mode != authManagerAPIKey {
		t.Fatalf("begin API-key entry = %#v, mode=%d", action, state.mode)
	}
	if err := state.appendAPIKey(secret); err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{state.render(80, 24), fmt.Sprint(state), fmt.Sprintf("%#v", state), fmt.Sprint(state.secret), fmt.Sprintf("%#v", state.secret)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("secret leaked before submit: %q", rendered)
		}
	}
	if rendered := state.render(80, 24); !strings.Contains(rendered, "API key: <hidden>") || strings.Contains(rendered, strings.Repeat("*", len(secret))) {
		t.Fatalf("secret-safe input render = %q", rendered)
	}
	action := state.submitAPIKey()
	if action.kind != authManagerSaveAPIKey || action.provider != "openai" {
		t.Fatalf("save action = %#v", action)
	}
	for _, rendered := range []string{fmt.Sprint(action), fmt.Sprintf("%#v", action), state.render(80, 24)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("secret leaked after submit: %q", rendered)
		}
	}
	if got := action.consumeSecret(); got != secret {
		t.Fatalf("consumed secret = %q", got)
	}
	if got := action.consumeSecret(); got != "" {
		t.Fatalf("secret was consumable twice: %q", got)
	}
}

func TestAuthManagerAPIKeyEntryRejectsUnsafeAndOversizedInput(t *testing.T) {
	state := authManagerState{mode: authManagerAPIKey, target: "openai"}
	for _, unsafe := range []string{"line\nbreak", "nul\x00byte", string([]byte{0xff})} {
		if err := state.appendAPIKey(unsafe); err == nil {
			t.Fatalf("unsafe input %q was accepted", unsafe)
		}
	}
	if err := state.appendAPIKey(strings.Repeat("x", maxAuthManagerSecretSize+1)); err == nil {
		t.Fatal("oversized API key was accepted")
	}
	if action := state.submitAPIKey(); action.kind != authManagerNoAction {
		t.Fatalf("empty submit action = %#v", action)
	}
	if !strings.Contains(state.render(80, 24), "Enter an API key") {
		t.Fatalf("missing empty-input guidance: %q", state.render(80, 24))
	}
	if err := state.appendAPIKey("é"); err != nil {
		t.Fatal(err)
	}
	state.backspaceAPIKey()
	if len(state.secret.value) != 0 || !utf8.Valid(state.secret.value) {
		t.Fatalf("unicode backspace left %q", state.secret.value)
	}
	if err := state.appendAPIKey("temporary"); err != nil {
		t.Fatal(err)
	}
	state.cancel()
	if len(state.secret.value) != 0 || state.mode != authManagerList {
		t.Fatalf("cancel did not clear secret state: %#v", state)
	}
}

func TestAuthManagerSubscriptionStateUsesFixedMessagesAndSafeAuthorizeURLs(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{{provider: "subscription_provider", label: "Subscription", oauthOnly: true}}}
	action := state.beginSelected()
	if action.kind != authManagerStartSubscription || state.subscriptionPhase != authSubscriptionPreparing {
		t.Fatalf("start action = %#v, phase=%d", action, state.subscriptionPhase)
	}
	valid := "https://auth.openai.com/oauth/authorize?state=public-state&code_challenge=public-challenge"
	if err := state.setSubscriptionAuthorizeURL(valid, false); err != nil {
		t.Fatal(err)
	}
	if rendered := state.render(100, 20); !strings.Contains(rendered, valid) || !strings.Contains(rendered, "Open this URL") {
		t.Fatalf("manual authorization render = %q", rendered)
	}
	if copied, ok := state.subscriptionAuthorizeURL(); !ok || copied != valid || !strings.Contains(state.render(100, 20), "copy full URL") {
		t.Fatalf("manual authorization clipboard seam = %q, %t", copied, ok)
	}
	if err := state.setSubscriptionAuthorizeURL(valid, true); err != nil {
		t.Fatal(err)
	}
	if rendered := state.render(100, 20); !strings.Contains(rendered, "Waiting for sign-in") {
		t.Fatalf("waiting render = %q", rendered)
	}
	state.setSubscriptionPhase(authSubscriptionSucceeded)
	if copied, ok := state.subscriptionAuthorizeURL(); ok || copied != "" {
		t.Fatalf("completed flow retained clipboard URL = %q, %t", copied, ok)
	}
	if rendered := state.render(100, 20); strings.Contains(rendered, valid) || !strings.Contains(rendered, "sign-in complete") {
		t.Fatalf("success render = %q", rendered)
	}
	state.setSubscriptionPhase(authSubscriptionFailed)
	if rendered := state.render(100, 20); !strings.Contains(rendered, "Sign-in failed. Try again.") {
		t.Fatalf("failure render = %q", rendered)
	}
}

func TestAuthManagerSubscriptionRejectsSecretBearingAndUntrustedURLs(t *testing.T) {
	state := authManagerState{mode: authManagerSubscription, subscriptionGeneration: 1}
	secret := "do-not-render-this-token"
	unsafe := []string{
		"http://auth.openai.com/oauth/authorize",
		"https://auth.openai.com:444/oauth/authorize",
		"https://example.test/oauth/authorize",
		"https://user@auth.openai.com/oauth/authorize",
		"https://auth.openai.com/not-authorize",
		"https://auth.openai.com/oauth/authorize#fragment",
		"https://auth.openai.com/oauth/authorize?access_token=" + secret,
		"https://auth.openai.com/oauth/authorize?api-key=" + secret,
		"https://auth.openai.com/oauth/authorize?unexpected=" + secret,
		"https://auth.openai.com/oauth/authorize?state=public;access_token=" + secret,
		strings.Repeat("x", maxAuthManagerURLSize+1),
	}
	for _, candidate := range unsafe {
		if err := state.setSubscriptionAuthorizeURL(candidate, false); err == nil {
			t.Fatalf("unsafe authorization URL was accepted: %q", candidate)
		}
		if rendered := state.render(100, 20); strings.Contains(rendered, secret) || strings.Contains(rendered, candidate) {
			t.Fatalf("unsafe URL leaked into render: %q", rendered)
		}
	}
}

func TestAuthManagerSubscriptionIgnoresSupersededWorkerResults(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{{provider: "openai_oauth", oauthOnly: true}}}
	first := state.beginSelected()
	state.cancel()
	second := state.beginSelected()
	if first.generation == 0 || second.generation == 0 || first.generation == second.generation {
		t.Fatalf("subscription generations = %d, %d", first.generation, second.generation)
	}
	if err := state.setSubscriptionAuthorizeURLFor(first.generation, "https://auth.openai.com/oauth/authorize?state=stale", false); err == nil {
		t.Fatal("stale authorization URL was accepted")
	}
	if state.authorizeURL != "" || state.subscriptionPhase != authSubscriptionPreparing {
		t.Fatalf("stale result mutated active flow: %#v", state)
	}
	if err := state.setSubscriptionAuthorizeURLFor(second.generation, "https://auth.openai.com/oauth/authorize?state=current", false); err != nil {
		t.Fatal(err)
	}
	state.setSubscriptionPhaseFor(first.generation, authSubscriptionSucceeded)
	if state.subscriptionPhase != authSubscriptionAuthorize {
		t.Fatalf("stale completion changed phase = %d", state.subscriptionPhase)
	}
}

func TestAuthManagerRemovalRequiresConfirmationAndRejectsEnvironmentRemoval(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{
		{provider: "environment_provider", source: "environment", configured: true},
		{provider: "stored_provider", source: "stored", configured: true, removable: true},
	}}
	if state.beginRemoval() {
		t.Fatal("environment credential entered removal confirmation")
	}
	if !strings.Contains(state.render(100, 20), "must be changed outside") {
		t.Fatalf("environment removal guidance = %q", state.render(100, 20))
	}
	state.move(1)
	if !state.beginRemoval() || state.mode != authManagerRemoval {
		t.Fatalf("stored credential did not enter removal confirmation: %#v", state)
	}
	if action := state.confirmRemoval(); action.kind != authManagerRemoveCredential || action.provider != "stored_provider" {
		t.Fatalf("removal action = %#v", action)
	}
	state.selected = 1
	if !state.beginRemoval() {
		t.Fatal("stored credential did not reenter removal confirmation")
	}
	state.cancel()
	if action := state.confirmRemoval(); action.kind != authManagerNoAction {
		t.Fatalf("cancelled removal produced action: %#v", action)
	}
}

func TestAuthManagerRenderingIsBoundedAndMakesControlsInert(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{{
		provider: "provider", label: "Provider", source: "stored", status: "configured",
		detail: strings.Repeat("long", 100),
	}}}
	state.setNotice("warning\x1b[31m\nnext")
	rendered := state.render(32, 6)
	lines := strings.Split(rendered, "\n")
	if len(lines) > 6 {
		t.Fatalf("rendered line count = %d", len(lines))
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) > 32 {
			t.Fatalf("rendered line exceeds width: %q", line)
		}
	}
	if strings.ContainsRune(rendered, '\x1b') || !strings.Contains(rendered, "<U+001B CONTROL>") {
		t.Fatalf("control-bearing notice was not made inert: %q", rendered)
	}
}

func TestAuthManagerInvalidRequiredDependenciesPanic(t *testing.T) {
	store := newAuthManagerTestStore(t)
	assertPanicsAuthManager(t, func() { newAuthManager(nil, func(string) (string, bool) { return "", false }) })
	assertPanicsAuthManager(t, func() { newAuthManager(store, nil) })
	manager := newAuthManager(store, func(string) (string, bool) { return "", false })
	assertPanicsAuthManager(t, func() { _ = manager.refresh(nil, false) })
}

func TestAuthManagerResolutionErrorsDoNotEchoEnvironmentSecrets(t *testing.T) {
	secret := "unsafe\r\nsecret-material"
	manager := newAuthManager(newAuthManagerTestStore(t), func(name string) (string, bool) {
		if name == "OPENAI_API_KEY" {
			return secret, true
		}
		return "", false
	})
	err := manager.refresh(t.Context(), false)
	if err == nil {
		t.Fatal("invalid environment credential was accepted")
	}
	for _, rendered := range []string{err.Error(), manager.state.render(80, 24), fmt.Sprint(manager.state)} {
		if strings.Contains(rendered, secret) || strings.Contains(rendered, "secret-material") {
			t.Fatalf("environment secret leaked: %q", rendered)
		}
	}
	if len(manager.state.rows) != 0 || !strings.Contains(manager.state.render(80, 24), "status is unavailable") {
		t.Fatalf("failed refresh retained stale state: %#v", manager.state)
	}
}

func newAuthManagerTestStore(t *testing.T) *dacredential.Store {
	t.Helper()
	return dacredential.NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, dacredential.Options{})
}

func findAuthManagerRow(t *testing.T, rows []authManagerRow, provider string) authManagerRow {
	t.Helper()
	for _, row := range rows {
		if row.provider == provider {
			return row
		}
	}
	t.Fatalf("missing auth manager row %q", provider)
	return authManagerRow{}
}

func findOAuthAuthManagerRow(t *testing.T, rows []authManagerRow) authManagerRow {
	t.Helper()
	for _, row := range rows {
		if row.oauthOnly {
			return row
		}
	}
	t.Fatal("missing subscription auth manager row")
	return authManagerRow{}
}

func assertPanicsAuthManager(t *testing.T, operation func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("operation did not panic")
		}
	}()
	operation()
}
