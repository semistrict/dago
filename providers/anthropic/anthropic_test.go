package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

func TestInvoke(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "secret" {
			t.Errorf("x-api-key = %q", request.Header.Get("x-api-key"))
		}
		var payload requestPayload
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Tools) != 1 || payload.Tools[0].Name != "lookup" {
			t.Errorf("tools = %#v", payload.Tools)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"msg-1","model":"claude-example","content":[{"type":"thinking","thinking":"checking","signature":"sig"},{"type":"text","text":"done"},{"type":"tool_use","id":"call-1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":1}}`))
	}))
	defer server.Close()

	chat, err := New("secret", Options{Model: "claude-example", BaseURL: server.URL, HTTPClient: server.Client(), AdaptiveThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := chat.Invoke(context.Background(), model.Request{
		Messages:  []message.Message{message.System("system"), message.Human("hello")},
		Tools:     []tool.Definition{{Name: "lookup", Description: "look up", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Reasoning: &model.Reasoning{Effort: "high", Summary: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "done" || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("response = %#v", response)
	}
	if response.Message.Usage == nil || response.Message.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
}

func TestMessagesURL(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"https://example.test":             "https://example.test/v1/messages",
		"https://example.test/v1":          "https://example.test/v1/messages",
		"https://example.test/v1/messages": "https://example.test/v1/messages",
	} {
		if got := messagesURL(input); got != want {
			t.Errorf("messagesURL(%q) = %q, want %q", input, got, want)
		}
	}
}
