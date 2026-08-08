package server

import (
	"context"
	"testing"
	"time"

	"shelley.exe.dev/db"
	"shelley.exe.dev/llm"
)

// TestStreamFlusherAssignsMonotonicSeq verifies that each partial update the
// streamFlusher broadcasts carries a monotonically increasing per-conversation
// sequence number, regardless of whether the delta is text (batched) or a
// non-text delta (broadcast immediately). Clients use this to detect dropped
// or out-of-order partial updates.
func TestStreamFlusherAssignsMonotonicSeq(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)

	conversation, err := database.CreateConversation(context.Background(), nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("failed to create conversation: %v", err)
	}
	manager, err := server.getOrCreateConversationManager(context.Background(), conversation.ConversationID, "")
	if err != nil {
		t.Fatalf("failed to get conversation manager: %v", err)
	}

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	next := manager.subpub.Subscribe(subCtx, -1)

	seqs := make(chan int64, 16)
	go func() {
		for {
			data, ok := next()
			if !ok {
				return
			}
			if data.StreamDelta != nil {
				seqs <- data.StreamDelta.Seq
			}
		}
	}()

	// Use a long interval so the periodic timer never fires on its own; only
	// the explicit Flush below emits the batched text delta. This keeps the
	// expected sequence deterministic.
	sf := newStreamFlusher(manager, time.Hour)

	// A non-text delta broadcasts immediately.
	sf.Push(llm.StreamDelta{Type: "thinking", Text: "hmm", Index: 0})
	// Text deltas are batched; an explicit Flush emits one combined delta.
	sf.Push(llm.StreamDelta{Type: "text", Text: "hello ", Index: 1})
	sf.Push(llm.StreamDelta{Type: "text", Text: "world", Index: 1})
	sf.Flush()
	// Another non-text delta.
	sf.Push(llm.StreamDelta{Type: "thinking", Text: "done", Index: 0})

	var got []int64
	for len(got) < 3 {
		select {
		case s := <-seqs:
			got = append(got, s)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for stream deltas; got %v", got)
		}
	}

	want := []int64{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seq[%d] = %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}
}
