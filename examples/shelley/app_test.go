package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
