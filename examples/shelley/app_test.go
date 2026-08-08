package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func testApplication(t *testing.T) *application {
	t.Helper()
	application, err := newApplication(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.close)
	return application
}

func TestConversationAndAgentStreamRoundTrip(t *testing.T) {
	application := testApplication(t)
	application.modelOverride = modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("A measured answer.")}})
	handler := application.routes()

	created := performJSON(t, handler, http.MethodPost, "/api/conversations", map[string]any{"title": "Test expedition"})
	var conversation conversation
	if err := json.Unmarshal(created.Body.Bytes(), &conversation); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/"+conversation.ID+"/messages", bytes.NewBufferString(`{"message":"survey"}`))
	request.Header.Set("Content-Type", "application/json")
	streamed := httptest.NewRecorder()
	handler.ServeHTTP(streamed, request)
	if streamed.Code != http.StatusOK || !strings.Contains(streamed.Body.String(), "event: done") {
		t.Fatalf("stream = %d %s", streamed.Code, streamed.Body.String())
	}
	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/conversations/"+conversation.ID, nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "A measured answer.") {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
}

func TestFileEditorAndStatusRoutes(t *testing.T) {
	application := testApplication(t)
	handler := application.routes()
	write := performJSON(t, handler, http.MethodPut, "/api/file", map[string]any{"path": "/notes/a.txt", "content": "hello"})
	if write.Code != http.StatusOK {
		t.Fatalf("write = %d %s", write.Code, write.Body.String())
	}
	read := httptest.NewRecorder()
	handler.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/file?path=/notes/a.txt", nil))
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), "hello") {
		t.Fatalf("read = %d %s", read.Code, read.Body.String())
	}
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"backend":"local"`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
}

func TestDefaultModelIsSubscriptionCompatible(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "")
	application := testApplication(t)
	if application.settings.Model != defaultModel {
		t.Fatalf("model = %q, want %q", application.settings.Model, defaultModel)
	}
	status := httptest.NewRecorder()
	application.routes().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"model":"`+defaultModel+`"`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
}

func TestOAuthCompletionUpdatesMainWindow(t *testing.T) {
	script, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, behavior := range []string{
		`oauthPollTimer = setTimeout(pollOAuthStatus, 0)`,
		`if ($("#settings-dialog").open) $("#settings-dialog").close()`,
		`window.addEventListener("focus"`,
	} {
		if !bytes.Contains(script, []byte(behavior)) {
			t.Fatalf("app.js does not encode OAuth completion behavior %q", behavior)
		}
	}
}

func TestLoadedOAuthSessionReportsComplete(t *testing.T) {
	workspace := t.TempDir()
	dataDirectory := t.TempDir()
	tokenPath := filepath.Join(dataDirectory, "openai-oauth.json")
	if err := os.WriteFile(tokenPath, []byte(`{"access_token":"access","refresh_token":"refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	application, err := newApplication(workspace, dataDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(application.close)
	status := httptest.NewRecorder()
	application.routes().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"oauth_state":"complete"`) {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
}

func TestConversationRestoresPendingApproval(t *testing.T) {
	application := testApplication(t)
	application.modelOverride = modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Message{
		Role: message.RoleAssistant,
		ToolCalls: []message.ToolCall{{
			ID: "execute-1", Name: "execute", Arguments: json.RawMessage(`{"command":"pwd"}`),
		}},
	}}})
	handler := application.routes()
	created := performJSON(t, handler, http.MethodPost, "/api/conversations", map[string]any{"title": "Approval"})
	var conversation conversation
	if err := json.Unmarshal(created.Body.Bytes(), &conversation); err != nil {
		t.Fatal(err)
	}
	streamed := performJSON(t, handler, http.MethodPost, "/api/conversations/"+conversation.ID+"/messages", map[string]any{"message": "run pwd"})
	if streamed.Code != http.StatusOK || !strings.Contains(streamed.Body.String(), `"mode":"interrupt"`) {
		t.Fatalf("stream = %d %s", streamed.Code, streamed.Body.String())
	}
	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/conversations/"+conversation.ID, nil))
	var state struct {
		Interrupts []struct {
			ID    string                  `json:"id"`
			Value []agent.ApprovalRequest `json:"value"`
		} `json:"interrupts"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if detail.Code != http.StatusOK || len(state.Interrupts) != 1 || state.Interrupts[0].ID != "human_approval" || len(state.Interrupts[0].Value) != 1 || state.Interrupts[0].Value[0].Call.ID != "execute-1" {
		t.Fatalf("detail = %d %s", detail.Code, detail.Body.String())
	}
}

func TestRecoverApprovalInterruptsFromIncompleteTimeline(t *testing.T) {
	interrupts := recoverApprovalInterrupts([]message.Message{{
		Role: message.RoleAssistant,
		ToolCalls: []message.ToolCall{{
			ID: "execute-legacy", Name: "execute", Arguments: json.RawMessage(`"{\"command\":\"pwd\"}"`),
		}},
	}})
	data, err := json.Marshal(interrupts)
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupts) != 1 || !strings.Contains(string(data), `"id":"human_approval"`) || !strings.Contains(string(data), `"id":"execute-legacy"`) {
		t.Fatalf("interrupts = %s", data)
	}
}

func performJSON(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
