package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

func TestChatCompletionsInvoke(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "example" {
			t.Errorf("model = %#v", payload["model"])
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-1","choices":[{"message":{"role":"assistant","content":"done","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	chat, err := NewChatCompletions("secret", ChatOptions{Model: "example", BaseURL: server.URL + "/v1", HTTPClient: server.Client(), ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := chat.Invoke(context.Background(), model.Request{
		Messages: []message.Message{message.Human("hello")},
		Tools:    []tool.Definition{{Name: "lookup", Description: "look up", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "done" || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("response = %#v", response)
	}
	if response.Message.Usage == nil || response.Message.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
}

func TestChatAPIBaseURL(t *testing.T) {
	t.Parallel()
	if got := chatAPIBaseURL(" https://example.test/v1/chat/completions/ "); got != "https://example.test/v1" {
		t.Fatalf("chatAPIBaseURL = %q", got)
	}
}
