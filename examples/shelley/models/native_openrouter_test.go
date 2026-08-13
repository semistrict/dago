package models

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/examples/shelley/db/generated"
)

func TestOpenRouterCustomModelBuildsNativeChat(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/responses" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("X-OpenRouter-Title") != "Shelley" {
			t.Fatalf("OpenRouter title = %q", request.Header.Get("X-OpenRouter-Title"))
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"r","status":"completed","model":"deepseek/deepseek-v4-flash-0731","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"native"}]}],"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10,"cost":0.0001}}`)
	}))
	defer server.Close()

	const modelID = "deepseek/deepseek-v4-flash-0731"
	manager := &Manager{httpc: server.Client()}
	chat, err := manager.createChatFromModel(&generated.Model{
		ProviderType: string(APITypeOpenRouterResponses), Endpoint: server.URL + "/api/v1",
		ApiKey: "secret", ModelName: modelID, MaxTokens: 32_768,
		ReasoningSupport: "yes", ImageSupport: "no",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := chat.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "native" || response.Message.Usage == nil || response.Message.Usage.Provider != "openrouter" {
		t.Fatalf("response = %#v", response)
	}
	if requestBody["model"] != modelID {
		t.Fatalf("model = %#v", requestBody["model"])
	}
	provider, ok := requestBody["provider"].(map[string]any)
	if !ok || provider["require_parameters"] != true {
		t.Fatalf("provider routing = %#v", requestBody["provider"])
	}
	profile := chat.Profile()
	if profile.Provider != "openrouter" || profile.Model != modelID || !profile.SupportsReasoning {
		t.Fatalf("profile = %#v", profile)
	}
}
