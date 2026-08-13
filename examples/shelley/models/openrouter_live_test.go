package models

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/examples/shelley/db/generated"
)

func TestOpenRouterShelleyLive(t *testing.T) {
	if os.Getenv("SHELLEY_OPENROUTER_E2E") != "1" {
		t.Skip("run make openrouter-e2e to enable the live Shelley OpenRouter test")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		t.Fatal("OPENROUTER_API_KEY is not set")
	}
	const modelID = "deepseek/deepseek-v4-flash-0731"
	manager := &Manager{}
	chat, err := manager.createChatFromModel(&generated.Model{
		ProviderType: string(APITypeOpenRouterResponses), Endpoint: "https://openrouter.ai/api/v1",
		ApiKey: apiKey, ModelName: modelID, MaxTokens: 512,
		ReasoningSupport: "yes", ImageSupport: "no",
	})
	if err != nil {
		t.Fatal(err)
	}
	if profile := chat.Profile(); profile.Provider != "openrouter" || profile.Model != modelID {
		t.Fatalf("profile = %#v", profile)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	response, err := chat.Invoke(ctx, damodel.Request{
		Messages: []damessage.Message{damessage.Human(`Call lookup_code with code "shelley-e2e".`)},
		Tools: []datool.Definition{{
			Name: "lookup_code", Description: "Look up a test code.", Strict: true,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`),
		}},
		ToolChoice: &damodel.ToolChoice{Mode: "tool", Name: "lookup_code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.InvalidToolCalls) != 0 || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup_code" {
		t.Fatalf("tool calls = %#v; invalid = %#v", response.Message.ToolCalls, response.Message.InvalidToolCalls)
	}
	var arguments struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Message.ToolCalls[0].Arguments, &arguments); err != nil {
		t.Fatal(err)
	}
	if arguments.Code != "shelley-e2e" {
		t.Fatalf("tool arguments = %s", response.Message.ToolCalls[0].Arguments)
	}
	usage := response.Message.Usage
	if usage == nil || usage.Provider != "openrouter" || usage.Model == "" || usage.TotalTokens <= 0 {
		t.Fatalf("usage = %#v", usage)
	}
}
