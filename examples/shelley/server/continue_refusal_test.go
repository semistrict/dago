package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"

	"github.com/semistrict/dago/examples/shelley/claudetool"
	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/loop"
	"github.com/semistrict/dago/examples/shelley/slug"
)

// refuseThenOKService refuses the first *agent* request (stop_reason=refusal)
// and then returns a normal assistant message on every subsequent request. This
// lets us exercise the "switch model and continue" flow end-to-end: the first
// model declines, and continuing on a new model succeeds.
//
// Slug generation runs concurrently on the first user message and shares this
// same service. It must NOT consume the "first request" refusal slot, or the
// refusal races into the throwaway slug request while the real agent turn gets
// the OK response — then no refusal is ever recorded and the test wedges. Slug
// requests carry a distinctive prompt preamble (slug.PromptPreamble), so we
// detect and delegate them to the inner PredictableService without touching the
// refusal counter.
type refuseThenOKService struct {
	inner *loop.PredictableService
	mu    sync.Mutex
	calls int
}

func (s *refuseThenOKService) Profile() damodel.Profile { return s.inner.Profile() }
func (s *refuseThenOKService) Invoke(ctx context.Context, req damodel.Request) (damodel.Response, error) {
	for _, message := range req.Messages {
		if strings.Contains(message.TextContent(), slug.PromptPreamble) {
			return s.inner.Invoke(ctx, req)
		}
	}
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.mu.Unlock()
	if n == 0 {
		return s.inner.Invoke(ctx, damodel.Request{Messages: []dmessage.Message{dmessage.Human("refusal")}})
	}
	message := dmessage.Assistant("Continuing after the switch.")
	message.Usage = &dmessage.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2, Provider: "builtin", Model: "predictable-v1"}
	return damodel.Response{Message: message}, nil
}
func (*refuseThenOKService) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return nil, fmt.Errorf("refusal test model does not stream")
}

// TestContinueAfterRefusalSwitchesModelAndResumes verifies the refusal
// affordance: after a refusal error, POST /continue with a target model
// switches the conversation to that model (recording a modelchange marker) and
// re-fires the declined request, which now succeeds. The refusal error row is
// left in the log (append-only).
func TestContinueAfterRefusalSwitchesModelAndResumes(t *testing.T) {
	t.Parallel()
	database, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	svc := &refuseThenOKService{inner: loop.NewPredictableService()}
	svr := NewServer(database, &twoModelLLMManager{chat: svc},
		claudetool.ToolSetConfig{EnableBrowser: false},
		slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})),
		false, "model-a", "")
	svr.hooksDir = t.TempDir()
	if svr.terminals != nil {
		svr.terminals.SetSpawner(InProcessSpawner)
	}

	modelA := "model-a"
	conv, err := database.CreateConversation(context.Background(), nil, true, nil, &modelA, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	conversationID := conv.ConversationID

	// First turn refuses.
	postChatModel(t, svr, conversationID, "please do X", "model-a")

	// Wait for a refusal error message to be recorded (error_type=refusal).
	waitFor(t, 10*time.Second, func() bool {
		for _, m := range listMessages(t, database, conversationID) {
			if m.Type == string(db.MessageTypeError) && m.UserData != nil {
				var ud map[string]any
				if json.Unmarshal([]byte(*m.UserData), &ud) == nil {
					if et, _ := ud["error_type"].(string); et == string(llm.ErrorTypeRefusal) {
						return true
					}
				}
			}
		}
		return false
	})

	// Agent must have stopped working before continuing.
	waitFor(t, 5*time.Second, func() bool { return !svr.IsAgentWorking(conversationID) })

	// The refusal error's user_data must carry the provider's structured reason
	// so the UI can show WHY the request was declined.
	{
		var found bool
		for _, m := range listMessages(t, database, conversationID) {
			if m.Type == string(db.MessageTypeError) && m.UserData != nil {
				var ud map[string]any
				if json.Unmarshal([]byte(*m.UserData), &ud) == nil {
					if cat, _ := ud["refusal_category"].(string); cat == "cyber" {
						if exp, _ := ud["refusal_explanation"].(string); exp != "" {
							found = true
						}
					}
				}
			}
		}
		if !found {
			t.Error("refusal error user_data should carry refusal_category and refusal_explanation")
		}
	}

	// Continue on model-b.
	body, _ := json.Marshal(continueRequest{Model: "model-b"})
	req := httptest.NewRequest("POST", "/api/conversation/"+conversationID+"/continue", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svr.handleContinueConversation(w, req, conversationID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	// A modelchange marker (model-a -> model-b) must have been recorded.
	waitFor(t, 5*time.Second, func() bool {
		mc := lastModelChange(listMessages(t, database, conversationID))
		if mc == nil || mc.UserData == nil {
			return false
		}
		var ud ModelChangeUserData
		if json.Unmarshal([]byte(*mc.UserData), &ud) != nil {
			return false
		}
		return ud.To == "model-b"
	})

	// A fresh, successful agent message must appear after the switch.
	waitFor(t, 10*time.Second, func() bool {
		for _, m := range listMessages(t, database, conversationID) {
			if m.Type == string(db.MessageTypeAgent) {
				return true
			}
		}
		return false
	})

	// The conversation now uses model-b.
	svr.mu.Lock()
	mgr := svr.activeConversations[conversationID]
	svr.mu.Unlock()
	if mgr == nil {
		t.Fatal("expected an active manager")
	}
	if got := mgr.GetModel(); got != "model-b" {
		t.Errorf("expected model-b after continue, got %q", got)
	}

	// The refusal error row must still be present (append-only log).
	foundRefusal := false
	for _, m := range listMessages(t, database, conversationID) {
		if m.Type == string(db.MessageTypeError) {
			foundRefusal = true
		}
	}
	if !foundRefusal {
		t.Error("refusal error row should remain in the log")
	}
}
