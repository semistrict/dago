package dacode

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/semistrict/dago/daproviders/openai"
)

type authSubscriptionLogin func(context.Context, func(string) error, openai.OAuthOptions) (*openai.OAuthSession, error)

type authSubscriptionFlowOptions struct {
	OpenURL func(string) error
}

type authSubscriptionEvent struct {
	generation   uint64
	phase        authSubscriptionPhase
	authorizeURL string
}

func (event authSubscriptionEvent) String() string {
	return fmt.Sprintf("authSubscriptionEvent(generation=%d,phase=%d,<url redacted>)", event.generation, event.phase)
}

func (event authSubscriptionEvent) GoString() string { return event.String() }

// authSubscriptionFlow exposes a bounded stream of secret-free UI events.
// Cancelling the flow interrupts the provider login and closes Events.
type authSubscriptionFlow struct {
	Events <-chan authSubscriptionEvent
	cancel context.CancelFunc
	once   sync.Once
}

func (flow *authSubscriptionFlow) Cancel() {
	if flow == nil || flow.cancel == nil {
		return
	}
	flow.once.Do(flow.cancel)
}

// startAuthSubscriptionFlow starts the provider-owned login worker. Required
// dependencies are positional, optional browser opening is carried by options,
// and provider failures become fixed-copy events rather than secret-bearing
// error text.
func startAuthSubscriptionFlow(
	ctx context.Context,
	generation uint64,
	storePath string,
	login authSubscriptionLogin,
	options authSubscriptionFlowOptions,
) *authSubscriptionFlow {
	if ctx == nil {
		panic("dacode: subscription login context is required")
	}
	if generation == 0 {
		panic("dacode: subscription login generation is required")
	}
	if strings.TrimSpace(storePath) == "" || strings.ContainsRune(storePath, 0) || len(storePath) > 4096 {
		panic("dacode: bounded subscription store path is required")
	}
	if login == nil {
		panic("dacode: subscription login function is required")
	}
	flowContext, cancel := context.WithCancel(ctx)
	events := make(chan authSubscriptionEvent, 4)
	flow := &authSubscriptionFlow{Events: events, cancel: cancel}
	emit := func(event authSubscriptionEvent) bool {
		select {
		case events <- event:
			return true
		case <-flowContext.Done():
			return false
		}
	}
	go func() {
		defer close(events)
		defer flow.Cancel()
		_, err := login(flowContext, func(raw string) error {
			safeURL, validationErr := validateSubscriptionAuthorizeURL(raw)
			if validationErr != nil {
				return validationErr
			}
			phase := authSubscriptionAuthorize
			if options.OpenURL != nil && options.OpenURL(safeURL) == nil {
				phase = authSubscriptionWaiting
			}
			if !emit(authSubscriptionEvent{generation: generation, phase: phase, authorizeURL: safeURL}) {
				return flowContext.Err()
			}
			return nil
		}, openai.OAuthOptions{StorePath: storePath})
		if err == nil {
			emit(authSubscriptionEvent{generation: generation, phase: authSubscriptionSucceeded})
			return
		}
		if flowContext.Err() != nil {
			emit(authSubscriptionEvent{generation: generation, phase: authSubscriptionCancelled})
			return
		}
		emit(authSubscriptionEvent{generation: generation, phase: authSubscriptionFailed})
	}()
	return flow
}

// applySubscriptionEvent mutates the pure auth state only for its active
// generation. It returns false for stale or malformed worker events.
func (state *authManagerState) applySubscriptionEvent(event authSubscriptionEvent) bool {
	if state == nil || state.mode != authManagerSubscription || event.generation == 0 || event.generation != state.subscriptionGeneration {
		return false
	}
	switch event.phase {
	case authSubscriptionAuthorize, authSubscriptionWaiting:
		return state.setSubscriptionAuthorizeURLFor(event.generation, event.authorizeURL, event.phase == authSubscriptionWaiting) == nil
	case authSubscriptionSucceeded, authSubscriptionCancelled, authSubscriptionFailed:
		state.setSubscriptionPhaseFor(event.generation, event.phase)
		return true
	default:
		return false
	}
}
