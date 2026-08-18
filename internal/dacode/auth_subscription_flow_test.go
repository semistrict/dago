package dacode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/daproviders/openai"
)

func TestAuthSubscriptionFlowEmitsSafeAuthorizationAndSuccess(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{{provider: "openai_oauth", oauthOnly: true}}}
	action := state.beginSelected()
	opened := ""
	storePath := t.TempDir() + "/oauth.json"
	flow := startAuthSubscriptionFlow(t.Context(), action.generation, storePath,
		func(ctx context.Context, openURL func(string) error, options openai.OAuthOptions) (*openai.OAuthSession, error) {
			if options.StorePath != storePath {
				t.Fatalf("store path = %q", options.StorePath)
			}
			if err := openURL("https://auth.openai.com/oauth/authorize?state=public&code_challenge=challenge"); err != nil {
				return nil, err
			}
			return &openai.OAuthSession{}, nil
		}, authSubscriptionFlowOptions{OpenURL: func(value string) error { opened = value; return nil }})
	var phases []authSubscriptionPhase
	for event := range flow.Events {
		for _, rendered := range []string{fmt.Sprint(event), fmt.Sprintf("%#v", event)} {
			if strings.Contains(rendered, "code_challenge") {
				t.Fatalf("formatted event leaked URL: %q", rendered)
			}
		}
		if !state.applySubscriptionEvent(event) {
			t.Fatalf("event was not applied: %#v", event)
		}
		phases = append(phases, event.phase)
	}
	if len(phases) != 2 || phases[0] != authSubscriptionWaiting || phases[1] != authSubscriptionSucceeded {
		t.Fatalf("phases = %#v", phases)
	}
	if opened == "" || state.subscriptionPhase != authSubscriptionSucceeded || state.authorizeURL != "" {
		t.Fatalf("completed state = %#v, opened=%q", state, opened)
	}
}

func TestAuthSubscriptionFlowManualURLAndFailureUseFixedCopy(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{{provider: "openai_oauth", oauthOnly: true}}}
	action := state.beginSelected()
	secret := "provider-secret-that-must-not-surface"
	flow := startAuthSubscriptionFlow(t.Context(), action.generation, t.TempDir()+"/oauth.json",
		func(_ context.Context, openURL func(string) error, _ openai.OAuthOptions) (*openai.OAuthSession, error) {
			if err := openURL("https://auth.openai.com/oauth/authorize?state=public"); err != nil {
				return nil, err
			}
			return nil, errors.New(secret)
		}, authSubscriptionFlowOptions{OpenURL: func(string) error { return errors.New("browser unavailable") }})
	for event := range flow.Events {
		if !state.applySubscriptionEvent(event) {
			t.Fatalf("event was not applied: %#v", event)
		}
	}
	view := state.render(100, 20)
	if state.subscriptionPhase != authSubscriptionFailed || strings.Contains(view, secret) || !strings.Contains(view, "Sign-in failed. Try again.") {
		t.Fatalf("failed view = %q", view)
	}
}

func TestAuthSubscriptionFlowCancellationAndStaleEventsFailClosed(t *testing.T) {
	state := authManagerState{rows: []authManagerRow{{provider: "openai_oauth", oauthOnly: true}}}
	action := state.beginSelected()
	started := make(chan struct{})
	flow := startAuthSubscriptionFlow(t.Context(), action.generation, t.TempDir()+"/oauth.json",
		func(ctx context.Context, _ func(string) error, _ openai.OAuthOptions) (*openai.OAuthSession, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}, authSubscriptionFlowOptions{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("login did not start")
	}
	state.cancel()
	flow.Cancel()
	for event := range flow.Events {
		if state.applySubscriptionEvent(event) {
			t.Fatalf("cancelled generation event applied: %#v", event)
		}
	}
	if state.mode != authManagerList {
		t.Fatalf("cancelled state mode = %d", state.mode)
	}
}
