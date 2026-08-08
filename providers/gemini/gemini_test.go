package gemini

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
		if request.URL.Path != "/v1beta/models/gemini-example:generateContent" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("key") != "secret" {
			t.Errorf("key = %q", request.URL.Query().Get("key"))
		}
		var payload requestPayload
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Tools) != 1 || payload.Tools[0].FunctionDeclarations[0].Name != "lookup" {
			t.Errorf("tools = %#v", payload.Tools)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"sig"},{"text":"done"},{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"tool-sig"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`))
	}))
	defer server.Close()

	chat, err := New("secret", Options{Model: "gemini-example", BaseURL: server.URL + "/v1beta", HTTPClient: server.Client(), SupportsReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := chat.Invoke(context.Background(), model.Request{
		Messages: []message.Message{message.System("system"), message.Human("hello")},
		Tools:    []tool.Definition{{Name: "lookup", Description: "look up", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
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
