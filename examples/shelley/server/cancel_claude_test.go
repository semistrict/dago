package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/damessage"

	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/db/generated"
	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/loop"
)

// nativeCancellationHarness exercises the same HTTP, database projection, and
// conversation-manager path as the application while using Shelley's native
// deterministic dago model.
type nativeCancellationHarness struct {
	t      *testing.T
	server *Server
	db     *db.DB
	model  *loop.PredictableService
	convID string
}

func newNativeCancellationHarness(t *testing.T) *nativeCancellationHarness {
	t.Helper()
	server, database, model := newTestServer(t)
	conversation, err := database.CreateConversation(context.Background(), nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &nativeCancellationHarness{t: t, server: server, db: database, model: model, convID: conversation.ConversationID}
}

func (h *nativeCancellationHarness) chat(text string) {
	h.t.Helper()
	body, err := json.Marshal(ChatRequest{Message: text, Model: "predictable"})
	if err != nil {
		h.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/conversation/"+h.convID+"/chat", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.server.handleChatConversation(response, request, h.convID)
	if response.Code != http.StatusAccepted {
		h.t.Fatalf("chat status = %d: %s", response.Code, response.Body.String())
	}
}

func (h *nativeCancellationHarness) cancel() {
	h.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/conversation/"+h.convID+"/cancel", nil)
	response := httptest.NewRecorder()
	h.server.handleCancelConversation(response, request, h.convID)
	if response.Code != http.StatusOK {
		h.t.Fatalf("cancel status = %d: %s", response.Code, response.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		h.t.Fatal(err)
	}
	if result["status"] != "cancelled" {
		h.t.Fatalf("cancel response = %#v", result)
	}
	h.waitIdle()
}

func (h *nativeCancellationHarness) waitWorking() {
	h.t.Helper()
	waitFor(h.t, 5*time.Second, func() bool { return h.server.IsAgentWorking(h.convID) })
}

func (h *nativeCancellationHarness) waitIdle() {
	h.t.Helper()
	waitFor(h.t, 5*time.Second, func() bool { return !h.server.IsAgentWorking(h.convID) })
}

func (h *nativeCancellationHarness) messages() []generated.Message {
	h.t.Helper()
	var messages []generated.Message
	if err := h.db.Queries(context.Background(), func(queries *generated.Queries) error {
		var err error
		messages, err = queries.ListMessages(context.Background(), h.convID)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return messages
}

func (h *nativeCancellationHarness) waitAssistantText(want string) {
	h.t.Helper()
	waitFor(h.t, 5*time.Second, func() bool {
		for _, row := range h.messages() {
			if row.Type != string(db.MessageTypeAgent) || row.LlmData == nil {
				continue
			}
			var item llm.Message
			if json.Unmarshal([]byte(*row.LlmData), &item) == nil && item.EndOfTurn {
				for _, content := range item.Content {
					if content.Type == llm.ContentTypeText && strings.Contains(content.Text, want) {
						return true
					}
				}
			}
		}
		return false
	})
}

func (h *nativeCancellationHarness) assertCancellationProjection(expectToolResult bool) {
	h.t.Helper()
	foundUser := false
	foundCancellation := false
	foundCancelledTool := false
	for _, row := range h.messages() {
		if row.Type == string(db.MessageTypeUser) {
			foundUser = true
		}
		if row.LlmData == nil {
			continue
		}
		var item llm.Message
		if json.Unmarshal([]byte(*row.LlmData), &item) != nil {
			continue
		}
		for _, content := range item.Content {
			if content.Type == llm.ContentTypeText && strings.Contains(strings.ToLower(content.Text), "operation cancelled") {
				foundCancellation = true
			}
			if content.Type == llm.ContentTypeToolResult && content.ToolError {
				for _, result := range content.ToolResult {
					if strings.Contains(strings.ToLower(result.Text), "cancelled") {
						foundCancelledTool = true
					}
				}
			}
		}
	}
	if !foundUser || !foundCancellation || foundCancelledTool != expectToolResult {
		h.t.Fatalf("cancellation projection: user=%v cancellation=%v cancelled_tool=%v want_tool=%v", foundUser, foundCancellation, foundCancelledTool, expectToolResult)
	}
}

func (h *nativeCancellationHarness) cancelModelTurn() {
	h.t.Helper()
	h.model.SetResponseDelay(time.Second)
	h.chat("echo: model turn")
	h.waitWorking()
	h.cancel()
	h.model.SetResponseDelay(0)
}

func (h *nativeCancellationHarness) cancelToolTurn() {
	h.t.Helper()
	h.chat("bash: sleep 5")
	waitFor(h.t, 5*time.Second, func() bool {
		for _, row := range h.messages() {
			if row.Type != string(db.MessageTypeAgent) || row.LlmData == nil {
				continue
			}
			var item llm.Message
			if json.Unmarshal([]byte(*row.LlmData), &item) != nil {
				continue
			}
			for _, content := range item.Content {
				if content.Type == llm.ContentTypeToolUse {
					return true
				}
			}
		}
		return false
	})
	h.cancel()
}

func (h *nativeCancellationHarness) resume(text string) {
	h.t.Helper()
	h.chat("echo: " + text)
	h.waitIdle()
	h.waitAssistantText(text)
}

func TestClaudeCancelDuringToolCall(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelToolTurn()
	h.assertCancellationProjection(true)
}

func TestClaudeCancelDuringLLMCall(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelModelTurn()
	h.assertCancellationProjection(false)
}

func TestClaudeCancelDuringLLMCallThenResume(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelModelTurn()
	h.resume("resumed-after-model-cancel")
}

func TestClaudeCancelDuringLLMCallMultipleTimes(t *testing.T) {
	h := newNativeCancellationHarness(t)
	for range 3 {
		h.cancelModelTurn()
	}
	h.resume("recovered-after-three-cancels")
}

func TestClaudeCancelDuringLLMCallAndVerifyMessageStructure(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelModelTurn()
	h.assertCancellationProjection(false)
	h.resume("valid-message-structure")
}

func TestClaudeResumeAfterCancellation(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelToolTurn()
	before := len(h.messages())
	h.resume("resumed-after-tool-cancel")
	if len(h.messages()) <= before {
		t.Fatalf("message count did not grow after resume")
	}
}

func TestClaudeTokensMonotonicallyIncreasing(t *testing.T) {
	h := newNativeCancellationHarness(t)
	var prior int
	for _, text := range []string{"first", "second", "third"} {
		h.resume(text)
		request := h.model.GetLastRequest()
		if request == nil {
			t.Fatal("predictable model did not record a request")
		}
		tokens := dmessage.ApproximateTokens(request.Messages)
		if tokens < prior {
			t.Fatalf("context tokens decreased: previous=%d current=%d", prior, tokens)
		}
		prior = tokens
	}
}

func TestClaudeResumeAfterCancellationPreservesContext(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.resume("BLUE42")
	h.cancelToolTurn()
	h.resume("context-check")
	request := h.model.GetLastRequest()
	if request == nil {
		t.Fatal("predictable model did not record a request")
	}
	found := false
	for _, item := range request.Messages {
		if strings.Contains(item.TextContent(), "BLUE42") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("resumed native request lost pre-cancellation context: %#v", request.Messages)
	}
}

func TestClaudeMultipleCancellations(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelToolTurn()
	h.cancelToolTurn()
	h.resume("stable-after-multiple-tool-cancels")
}

func TestClaudeCancelImmediately(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.model.SetResponseDelay(time.Second)
	h.chat("echo: cancelled-immediately")
	h.waitWorking()
	h.cancel()
	h.model.SetResponseDelay(0)
	h.resume("recovered-from-immediate-cancel")
}

func TestClaudeCancelWithPendingToolResult(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelToolTurn()
	h.assertCancellationProjection(true)
	h.resume("pending-tool-result-recovered")
}

func TestClaudeCancelDuringLLMCallRapidFire(t *testing.T) {
	h := newNativeCancellationHarness(t)
	for range 3 {
		h.cancelModelTurn()
	}
	h.resume("stable-after-rapid-cancels")
}

func TestClaudeCancelDuringLLMCallWithToolUseResponse(t *testing.T) {
	h := newNativeCancellationHarness(t)
	h.cancelToolTurn()
	h.assertCancellationProjection(true)
	h.resume("tool-use-response-recovered")
}
