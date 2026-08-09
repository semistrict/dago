package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/claudetool"
	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
	"shelley.exe.dev/loop"
)

// gatingTestLLM errors until released, then blocks each Do on a gate channel so
// the test can hold the retried turn open (no new bottom message appended)
// while it issues a second retry.
type gatingTestLLM struct {
	inner dmodel.Chat
	mu    sync.Mutex
	err   error
	gate  chan struct{} // when non-nil, Do blocks on it after err clears
}

func (g *gatingTestLLM) Profile() dmodel.Profile { return g.inner.Profile() }
func (g *gatingTestLLM) Invoke(ctx context.Context, req dmodel.Request) (dmodel.Response, error) {
	g.mu.Lock()
	err := g.err
	gate := g.gate
	g.mu.Unlock()
	if err != nil {
		return dmodel.Response{}, err
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return dmodel.Response{}, ctx.Err()
		}
	}
	return g.inner.Invoke(ctx, req)
}
func (*gatingTestLLM) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return nil, fmt.Errorf("gating test model does not stream")
}

// TestRetryDoubleClickDeduped verifies that a second retry POST for the SAME
// bottom error message is rejected (MAJOR 6) — without mutating the error row.
func TestRetryDoubleClickDeduped(t *testing.T) {
	t.Parallel()
	database, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	ps := loop.NewPredictableService()
	gate := make(chan struct{})
	gllm := &gatingTestLLM{inner: ps, err: fmt.Errorf("connection error: EOF"), gate: gate}

	svr := NewServer(database, &testLLMManager{service: gllm},
		claudetool.ToolSetConfig{EnableBrowser: false},
		slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
		true, "predictable", "")
	if svr.terminals != nil {
		svr.terminals.SetSpawner(InProcessSpawner)
	}
	defer stopActiveConversationLoops(svr)

	ctx := context.Background()
	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	convID := conversation.ConversationID

	sendChat(t, svr, convID, "hello", false)

	// Wait for the retryable error message + idle.
	waitFor(t, 10*time.Second, func() bool {
		var msgs []generated.Message
		database.Queries(ctx, func(q *generated.Queries) error {
			var e error
			msgs, e = q.ListMessages(ctx, convID)
			return e
		})
		for _, m := range msgs {
			if m.Type == string(db.MessageTypeError) {
				return true
			}
		}
		return false
	})
	waitFor(t, 5*time.Second, func() bool { return !svr.IsAgentWorking(convID) })

	mgr, err := svr.getOrCreateConversationManager(ctx, convID, "")
	if err != nil {
		t.Fatalf("getOrCreateConversationManager: %v", err)
	}

	// Recover upstream but keep the gate closed so the retried turn blocks in
	// Do without appending a new bottom message.
	gllm.mu.Lock()
	gllm.err = nil
	gllm.mu.Unlock()

	// First retry: kicks the loop.
	if err := mgr.RetryLastLLMRequest(ctx); err != nil {
		t.Fatalf("first retry: unexpected error %v", err)
	}

	// Second retry (double-click) for the SAME bottom error must be rejected.
	err = mgr.RetryLastLLMRequest(ctx)
	if !errors.Is(err, errRetryNotApplicable) {
		t.Fatalf("second retry: want errRetryNotApplicable, got %v", err)
	}

	// Release the gate so the loop finishes cleanly.
	close(gate)
	time.Sleep(100 * time.Millisecond)
}
