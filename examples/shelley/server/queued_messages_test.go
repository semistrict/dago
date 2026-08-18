package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/llm"
)

// queuedMessages reads the conversation's queued_messages array from the DB.
func queuedMessages(t *testing.T, database *db.DB, convID string) []db.QueuedMessage {
	t.Helper()
	conv, err := database.GetConversationByID(context.Background(), convID)
	if err != nil {
		t.Fatalf("GetConversationByID: %v", err)
	}
	return db.ParseQueuedMessages(conv.QueuedMessages)
}

// userMessageRowExists reports whether a user messages row contains the text.
func userMessageRowExists(t *testing.T, database *db.DB, convID, text string) bool {
	t.Helper()
	msgs, err := database.ListMessages(context.Background(), convID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		if m.Type == string(db.MessageTypeUser) && m.LlmData != nil && strings.Contains(*m.LlmData, text) {
			return true
		}
	}
	return false
}

// sendChat sends a chat. queue=true requests queue-mode (held until the
// current turn ends); queue=false starts/continues a turn.
func sendChat(t *testing.T, server *Server, convID, msg string, queue bool) string {
	t.Helper()
	queueField := ""
	if queue {
		queueField = `,"queue":true`
	}
	req := httptest.NewRequest("POST", "/api/conversation/"+convID+"/chat",
		strings.NewReader(`{"message":`+jsonString(msg)+`,"model":"predictable"`+queueField+`}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handleChatConversation(w, req, convID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("chat: expected 202, got %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TestQueuedMessageImmutableFlow verifies that a message queued while the agent
// is busy lives only in the conversation's queued_messages array (no messages
// row), and on drain becomes a real immutable user row while the array entry is
// removed.
func TestQueuedMessageImmutableFlow(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	defer stopActiveConversationLoops(server)

	conversation, err := database.CreateConversation(context.Background(), nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	// Start a slow turn so the next message queues.
	sendChat(t, server, convID, "delay: 2", false)
	time.Sleep(200 * time.Millisecond)

	// Queue a message while busy.
	body := sendChat(t, server, convID, "echo: queued while busy", true)
	if !strings.Contains(body, "queued") {
		t.Fatalf("expected queued status, got %s", body)
	}

	// The queued message must be in the array and NOT a messages row.
	time.Sleep(100 * time.Millisecond)
	q := queuedMessages(t, database, convID)
	if len(q) != 1 {
		t.Fatalf("expected 1 queued array entry, got %d", len(q))
	}
	if !strings.Contains(string(q[0].Llm), "queued while busy") {
		t.Fatalf("array entry missing text: %s", q[0].Llm)
	}
	if userMessageRowExists(t, database, convID, "queued while busy") {
		t.Fatal("queued message must NOT exist as a messages row while queued")
	}

	// After the slow turn finishes, the queued message drains: it becomes a
	// real user row and is removed from the array.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if userMessageRowExists(t, database, convID, "queued while busy") &&
			len(queuedMessages(t, database, convID)) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("queued message did not drain: rowExists=%v queueLen=%d",
		userMessageRowExists(t, database, convID, "queued while busy"),
		len(queuedMessages(t, database, convID)))
}

// TestMessageDuringCancellationWaitsForReplacementLoop verifies that a send
// arriving after cancellation has made the conversation idle is not delivered
// to the loop that is still being torn down. It remains durable in the queue
// until teardown completes, then runs exactly once on a replacement loop.
func TestMessageDuringCancellationWaitsForReplacementLoop(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	defer stopActiveConversationLoops(server)
	ctx := context.Background()

	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	mgr, err := server.getOrCreateConversationManager(ctx, conversation.ConversationID, "")
	if err != nil {
		t.Fatalf("getOrCreateConversationManager: %v", err)
	}

	mgr.mu.Lock()
	mgr.cancelling = true
	mgr.agentWorking = false
	mgr.mu.Unlock()

	body := sendChat(t, server, conversation.ConversationID, "echo: after cancellation", false)
	if !strings.Contains(body, "queued") {
		t.Fatalf("send during cancellation = %s, want queued", body)
	}
	if userMessageRowExists(t, database, conversation.ConversationID, "after cancellation") {
		t.Fatal("message entered the active history before cancellation completed")
	}
	if got := len(queuedMessages(t, database, conversation.ConversationID)); got != 1 {
		t.Fatalf("queued messages = %d, want 1", got)
	}

	mgr.mu.Lock()
	mgr.cancelling = false
	mgr.mu.Unlock()
	mgr.drainPendingMessages(server)

	waitFor(t, 5*time.Second, func() bool {
		return userMessageRowExists(t, database, conversation.ConversationID, "after cancellation") &&
			len(queuedMessages(t, database, conversation.ConversationID)) == 0 &&
			!server.IsAgentWorking(conversation.ConversationID)
	})

	messages, err := database.ListMessages(ctx, conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	count := 0
	for _, message := range messages {
		if message.Type == string(db.MessageTypeUser) && message.LlmData != nil && strings.Contains(*message.LlmData, "after cancellation") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("drained user messages = %d, want exactly 1", count)
	}
}

// TestCancelQueuedClearsArray verifies whole-queue and per-message cancel clear
// the conversation's queued_messages array.
func TestCancelQueuedClearsArray(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	defer stopActiveConversationLoops(server)

	conversation, err := database.CreateConversation(context.Background(), nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	sendChat(t, server, convID, "delay: 3", false)
	time.Sleep(200 * time.Millisecond)
	sendChat(t, server, convID, "echo: first queued", true)
	sendChat(t, server, convID, "echo: second queued", true)
	time.Sleep(100 * time.Millisecond)

	q := queuedMessages(t, database, convID)
	if len(q) != 2 {
		t.Fatalf("expected 2 queued, got %d", len(q))
	}

	// Per-message cancel of the first.
	req := httptest.NewRequest("POST", "/api/conversation/"+convID+"/cancel-queued?queued_id="+q[0].ID, nil)
	w := httptest.NewRecorder()
	server.handleCancelQueued(w, req, convID)
	if w.Code != http.StatusOK {
		t.Fatalf("per-message cancel: got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if got := queuedMessages(t, database, convID); len(got) != 1 || got[0].ID != q[1].ID {
		t.Fatalf("after per-message cancel, queued = %+v, want [second]", got)
	}

	// Whole-queue cancel.
	req = httptest.NewRequest("POST", "/api/conversation/"+convID+"/cancel-queued", nil)
	w = httptest.NewRecorder()
	server.handleCancelQueued(w, req, convID)
	if w.Code != http.StatusOK {
		t.Fatalf("whole-queue cancel: got %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if got := queuedMessages(t, database, convID); len(got) != 0 {
		t.Fatalf("after whole-queue cancel, queued = %+v, want empty", got)
	}
}

// TestQueuedMessageNoDoubleFeedOnRestart simulates a server restart: queued
// entries persist in the array while the in-memory manager is gone. After the
// manager is re-created (Hydrate repopulates the in-memory batches from the
// array) and the queue drains, each queued message must become EXACTLY ONE
// real user row — never duplicated (CRITICAL 1). The atomic insert+removal in
// processBatch guarantees the array entry is gone the instant the row exists.
func TestQueuedMessageNoDoubleFeedOnRestart(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	defer stopActiveConversationLoops(server)
	ctx := context.Background()

	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	// Seed the queued_messages array directly (as if queued before a restart),
	// with no in-memory manager present.
	qm := db.QueuedMessage{
		ID:        "restart-1",
		Llm:       []byte(`{"Role":0,"Content":[{"Type":2,"Text":"echo: survived restart"}]}`),
		CreatedAt: time.Now(),
		Model:     "predictable",
	}
	if _, err := database.AppendQueuedMessage(ctx, convID, qm); err != nil {
		t.Fatalf("AppendQueuedMessage: %v", err)
	}

	// Re-create the manager (mirrors post-restart first open). Hydrate
	// repopulates the in-memory user batch from the array.
	mgr, err := server.getOrCreateConversationManager(ctx, convID, "")
	if err != nil {
		t.Fatalf("getOrCreateConversationManager: %v", err)
	}

	// Drain: creates the real row + atomically removes the array entry.
	mgr.drainPendingMessages(server)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if userMessageRowExists(t, database, convID, "survived restart") &&
			len(queuedMessages(t, database, convID)) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Exactly one real user row carrying the text; array empty.
	msgs, _ := database.ListMessages(ctx, convID)
	count := 0
	for _, m := range msgs {
		if m.Type == string(db.MessageTypeUser) && m.LlmData != nil &&
			strings.Contains(*m.LlmData, "survived restart") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 drained user row, got %d (double-feed?)", count)
	}
	if q := queuedMessages(t, database, convID); len(q) != 0 {
		t.Fatalf("queued array not emptied after drain: %+v", q)
	}

	// A SECOND drain must be a no-op (nothing left in array or memory).
	mgr.drainPendingMessages(server)
	time.Sleep(100 * time.Millisecond)
	msgs, _ = database.ListMessages(ctx, convID)
	count = 0
	for _, m := range msgs {
		if m.Type == string(db.MessageTypeUser) && m.LlmData != nil &&
			strings.Contains(*m.LlmData, "survived restart") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("second drain duplicated the message: count=%d", count)
	}
}

// TestCancelQueuedNoActiveManager verifies the /cancel-queued handler clears
// the persisted array even when there is NO active conversation manager
// (CRITICAL 4), for both whole-queue and per-message.
func TestCancelQueuedNoActiveManager(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	ctx := context.Background()

	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	for _, id := range []string{"q1", "q2"} {
		if _, err := database.AppendQueuedMessage(ctx, convID, db.QueuedMessage{
			ID: id, Llm: []byte(`{"Role":0}`), CreatedAt: time.Now(), Model: "predictable",
		}); err != nil {
			t.Fatalf("AppendQueuedMessage: %v", err)
		}
	}

	// Ensure no active manager.
	server.mu.Lock()
	delete(server.activeConversations, convID)
	server.mu.Unlock()

	// Per-message cancel via DB fallback.
	req := httptest.NewRequest("POST", "/api/conversation/"+convID+"/cancel-queued?queued_id=q1", nil)
	w := httptest.NewRecorder()
	server.handleCancelQueued(w, req, convID)
	if w.Code != http.StatusOK {
		t.Fatalf("per-message cancel (no manager): got %d: %s", w.Code, w.Body.String())
	}
	if q := queuedMessages(t, database, convID); len(q) != 1 || q[0].ID != "q2" {
		t.Fatalf("after per-message cancel, queued = %+v, want [q2]", q)
	}

	// Whole-queue cancel via DB fallback.
	req = httptest.NewRequest("POST", "/api/conversation/"+convID+"/cancel-queued", nil)
	w = httptest.NewRecorder()
	server.handleCancelQueued(w, req, convID)
	if w.Code != http.StatusOK {
		t.Fatalf("whole-queue cancel (no manager): got %d: %s", w.Code, w.Body.String())
	}
	if q := queuedMessages(t, database, convID); len(q) != 0 {
		t.Fatalf("after whole-queue cancel, queued = %+v, want empty", q)
	}
}

// TestCancelConversationClearsQueueUnconditionally verifies CancelConversation
// resets the persisted queued_messages array even when the in-memory batches
// are empty/diverged (CRITICAL 3).
func TestCancelConversationClearsQueueUnconditionally(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	defer stopActiveConversationLoops(server)
	ctx := context.Background()

	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	// Start a real turn so there's a loop to cancel.
	sendChat(t, server, convID, "delay: 30", false)
	time.Sleep(200 * time.Millisecond)

	mgr, err := server.getOrCreateConversationManager(ctx, convID, "")
	if err != nil {
		t.Fatalf("getOrCreateConversationManager: %v", err)
	}

	// Seed the array directly WITHOUT going through QueueMessage, so the
	// in-memory pendingBatches stay empty (simulating divergence/restart).
	if _, err := database.AppendQueuedMessage(ctx, convID, db.QueuedMessage{
		ID: "diverged", Llm: []byte(`{"Role":0}`), CreatedAt: time.Now(), Model: "predictable",
	}); err != nil {
		t.Fatalf("AppendQueuedMessage: %v", err)
	}

	if err := mgr.CancelConversation(ctx); err != nil {
		t.Fatalf("CancelConversation: %v", err)
	}

	if q := queuedMessages(t, database, convID); len(q) != 0 {
		t.Fatalf("CancelConversation must clear queue unconditionally, got %+v", q)
	}
}

// TestHydrateDedupesQueuedAgainstInMemory verifies that Hydrate does NOT
// restore a queued_messages array entry whose id is already present as an
// in-memory pendingBatchUser. Without this dedup, a message that QueueMessage
// persisted to BOTH the array and pendingBatches would be fed twice on the
// drain loop==nil/Hydrate path, inserting a duplicate immutable user row.
func TestHydrateDedupesQueuedAgainstInMemory(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	ctx := context.Background()

	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	// Persist id "dup" to the array AND a distinct id "only-array".
	for _, id := range []string{"dup", "only-array"} {
		if _, err := database.AppendQueuedMessage(ctx, convID, db.QueuedMessage{
			ID: id, Llm: []byte(`{"Role":0,"Content":[{"Type":2,"Text":"x"}]}`),
			CreatedAt: time.Now(), Model: "predictable",
		}); err != nil {
			t.Fatalf("AppendQueuedMessage: %v", err)
		}
	}

	// Build a fresh (un-hydrated) manager and inject an in-memory user batch
	// for "dup", mirroring QueueMessage having already enqueued it in memory.
	mgr := newConversationManager(convID, database, server.logger, server.toolSetConfig,
		func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
		func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
		func(context.Context, []recordMessageInput) error { return nil },
		func(ConversationState) {}, server.streamPub)
	mgr.mu.Lock()
	mgr.pendingBatches = []pendingBatch{{
		Kind: pendingBatchUser, Messages: []llm.Message{{}}, ModelID: "predictable",
		MessageIDs: []string{"dup"},
	}}
	mgr.mu.Unlock()

	if err := mgr.Hydrate(ctx); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	dupCount := 0
	ids := map[string]int{}
	for _, b := range mgr.pendingBatches {
		if b.Kind != pendingBatchUser {
			continue
		}
		for _, id := range b.MessageIDs {
			ids[id]++
			if id == "dup" {
				dupCount++
			}
		}
	}
	if dupCount != 1 {
		t.Fatalf("expected exactly 1 in-memory batch for id 'dup' after Hydrate, got %d", dupCount)
	}
	if ids["only-array"] != 1 {
		t.Fatalf("expected 'only-array' restored exactly once, got %d", ids["only-array"])
	}
}

// TestDrainNoDoubleFeedWhenInMemoryAndArrayBothHaveID covers Issue 2's exact
// scenario: a message is present BOTH as an in-memory pendingBatchUser AND in
// the queued_messages array (as QueueMessage leaves it), and the drain goes
// through the loop==nil/Hydrate path (e.g. post-cancel/restart). The drainer
// must insert EXACTLY ONE immutable user row — not two.
func TestDrainNoDoubleFeedWhenInMemoryAndArrayBothHaveID(t *testing.T) {
	t.Parallel()
	server, database, _ := newTestServer(t)
	defer stopActiveConversationLoops(server)
	ctx := context.Background()

	conversation, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := conversation.ConversationID

	mgr, err := server.getOrCreateConversationManager(ctx, convID, "")
	if err != nil {
		t.Fatalf("getOrCreateConversationManager: %v", err)
	}

	// Mirror QueueMessage: persist to the array AND inject the in-memory batch
	// with the SAME id. Then force the loop==nil/Hydrate drain path by marking
	// the manager un-hydrated with no loop (as CancelConversation does).
	const id = "both-places"
	if _, err := database.AppendQueuedMessage(ctx, convID, db.QueuedMessage{
		ID: id, Llm: []byte(`{"Role":0,"Content":[{"Type":2,"Text":"echo: exactly once"}]}`),
		CreatedAt: time.Now(), Model: "predictable",
	}); err != nil {
		t.Fatalf("AppendQueuedMessage: %v", err)
	}
	mgr.mu.Lock()
	mgr.pendingBatches = []pendingBatch{{
		Kind: pendingBatchUser, ModelID: "predictable",
		Messages:   []llm.Message{{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "echo: exactly once"}}}},
		MessageIDs: []string{id},
	}}
	mgr.loop = nil
	mgr.hydrated = false
	mgr.mu.Unlock()

	mgr.drainPendingMessages(server)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if len(queuedMessages(t, database, convID)) == 0 &&
			userMessageRowExists(t, database, convID, "exactly once") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	msgs, _ := database.ListMessages(ctx, convID)
	count := 0
	for _, m := range msgs {
		if m.Type == string(db.MessageTypeUser) && m.LlmData != nil &&
			strings.Contains(*m.LlmData, "exactly once") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 inserted user row, got %d (double-feed)", count)
	}
	if q := queuedMessages(t, database, convID); len(q) != 0 {
		t.Fatalf("queued array not emptied: %+v", q)
	}
}
