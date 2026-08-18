package dacode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon/tracing"
)

type traceResolverFunc func(context.Context, string, string) (string, error)

func (function traceResolverFunc) ThreadURL(ctx context.Context, project, threadID string) (string, error) {
	return function(ctx, project, threadID)
}

func TestTraceCommandUsesUsefulUnconfiguredAndEmptySessionStates(t *testing.T) {
	command := newTraceCommand(nil)
	if result := command.resolve(t.Context(), traceCommandRequest{}); result.Message != "No active session." || result.URL != "" {
		t.Fatalf("empty session result = %#v", result)
	}
	result := command.resolve(t.Context(), traceCommandRequest{ThreadID: "thread-1"})
	if !strings.Contains(result.Message, "Run /auth") || result.URL != "" {
		t.Fatalf("unconfigured result = %#v", result)
	}
}

func TestTraceCommandReturnsValidatedLinkAndEmptyThreadHint(t *testing.T) {
	command := newTraceCommand(traceResolverFunc(func(_ context.Context, project, threadID string) (string, error) {
		if project != "project" || threadID != "thread-1" {
			t.Fatalf("resolver inputs = %q, %q", project, threadID)
		}
		return "https://smith.example/projects/p/t/thread-1?utm_source=dago", nil
	}))
	result := command.resolve(t.Context(), traceCommandRequest{Project: "project", ThreadID: "thread-1"})
	if result.URL == "" || !strings.Contains(result.Message, result.URL) || !strings.Contains(result.Message, "empty until") {
		t.Fatalf("trace result = %#v", result)
	}
	result = command.resolve(t.Context(), traceCommandRequest{Project: "project", ThreadID: "thread-1", HasMessages: true})
	if strings.Contains(result.Message, "empty until") {
		t.Fatalf("non-empty thread result = %#v", result)
	}
}

func TestTraceCommandBoundsFailuresAndNeverLeaksResolverDetails(t *testing.T) {
	secret := "trace-secret"
	tests := []struct {
		name     string
		resolver traceResolverFunc
		want     string
	}{
		{name: "provider", resolver: func(context.Context, string, string) (string, error) {
			return "", errors.Join(tracing.ErrProjectLookup, errors.New(secret))
		}, want: "Verify"},
		{name: "timeout", resolver: func(context.Context, string, string) (string, error) { return "", context.DeadlineExceeded }, want: "network"},
		{name: "panic", resolver: func(context.Context, string, string) (string, error) { panic(secret) }, want: "Verify"},
		{name: "unsafe URL", resolver: func(context.Context, string, string) (string, error) {
			return "https://user:" + secret + "@example.com/path", nil
		}, want: "Failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := newTraceCommand(test.resolver).resolve(t.Context(), traceCommandRequest{Project: "project", ThreadID: "thread"})
			if result.URL != "" || !strings.Contains(result.Message, test.want) || strings.Contains(result.Message, secret) {
				t.Fatalf("trace result = %#v", result)
			}
		})
	}
}

func TestTraceCommandCancellationWinsAndExternalTextIsTerminalSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	command := newTraceCommand(traceResolverFunc(func(context.Context, string, string) (string, error) {
		called = true
		return "", nil
	}))
	result := command.resolve(ctx, traceCommandRequest{Project: "project", ThreadID: "thread"})
	if called || result.Message != "Trace lookup was cancelled." {
		t.Fatalf("cancelled result = %#v, called=%v", result, called)
	}

	result = newTraceCommand(nil).resolve(t.Context(), traceCommandRequest{Project: "project\x1b]52;c;evil\a", ThreadID: "thread"})
	if strings.ContainsAny(result.Message, "\x1b\a") {
		t.Fatalf("unsafe result = %q", result.Message)
	}
}

func TestTraceCommandAddsFiniteDefaultDeadlineAndKeepsShorterCallerDeadline(t *testing.T) {
	var defaultRemaining time.Duration
	command := newTraceCommand(traceResolverFunc(func(ctx context.Context, _, _ string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("resolver context has no deadline")
		}
		defaultRemaining = time.Until(deadline)
		return "https://smith.example/thread", nil
	}))
	result := command.resolve(t.Context(), traceCommandRequest{Project: "project", ThreadID: "thread", HasMessages: true})
	if result.URL == "" || defaultRemaining <= 0 || defaultRemaining > defaultTraceLookupTimeout {
		t.Fatalf("default deadline remaining = %v, result = %#v", defaultRemaining, result)
	}

	callerContext, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var callerRemaining time.Duration
	newTraceCommand(traceResolverFunc(func(ctx context.Context, _, _ string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("caller deadline was lost")
		}
		callerRemaining = time.Until(deadline)
		return "https://smith.example/thread", nil
	})).resolve(callerContext, traceCommandRequest{Project: "project", ThreadID: "thread", HasMessages: true})
	if callerRemaining <= 0 || callerRemaining > 50*time.Millisecond {
		t.Fatalf("caller deadline remaining = %v", callerRemaining)
	}
}
