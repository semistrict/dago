package openrouter

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
)

const liveTestModelDefault = "deepseek/deepseek-v4-flash-0731"

func TestOpenRouterLive(t *testing.T) {
	apiKey := liveAPIKey(t)
	model := strings.TrimSpace(os.Getenv("DAGO_OPENROUTER_E2E_MODEL"))
	if model == "" {
		model = liveTestModelDefault
	}
	requireParameters := true
	client := New(apiKey, model, Options{MaxOutputTokens: 512,
		Routing: &ProviderRouting{RequireParameters: &requireParameters},
	})

	t.Run("structured invoke", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		response, err := client.Invoke(ctx, damodel.Request{
			Messages: []damessage.Message{damessage.Human(`Return an object whose "status" field is "ok".`)},
			ResponseFormat: &damodel.ResponseFormat{
				Name: "live_status", Strict: true,
				Schema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["ok"]}},"required":["status"],"additionalProperties":false}`),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var structured struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(response.Structured, &structured); err != nil {
			t.Fatalf("decode structured response: %v; response = %q", err, response.Structured)
		}
		if structured.Status != "ok" {
			t.Fatalf("structured response = %s", response.Structured)
		}
		assertLiveUsage(t, response.Message.Usage)
	})

	t.Run("forced tool call", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		response, err := client.Invoke(ctx, damodel.Request{
			Messages: []damessage.Message{damessage.Human(`Call lookup_code with code "e2e-42".`)},
			Tools: []datool.Definition{{
				Name: "lookup_code", Description: "Look up a test code.", Strict: true,
				InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`),
			}},
			ToolChoice: &damodel.ToolChoice{Mode: "tool", Name: "lookup_code"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Message.InvalidToolCalls) != 0 {
			t.Fatalf("invalid tool calls = %#v", response.Message.InvalidToolCalls)
		}
		if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup_code" {
			t.Fatalf("tool calls = %#v", response.Message.ToolCalls)
		}
		var arguments struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(response.Message.ToolCalls[0].Arguments, &arguments); err != nil {
			t.Fatalf("decode tool arguments: %v", err)
		}
		if arguments.Code != "e2e-42" {
			t.Fatalf("tool arguments = %s", response.Message.ToolCalls[0].Arguments)
		}
		if reason, _ := damodel.Outcome(response.Message); reason != damodel.FinishReasonToolCalls {
			t.Fatalf("finish reason = %q", reason)
		}
		assertLiveUsage(t, response.Message.Usage)
	})

	t.Run("stream", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		stream, err := client.Stream(ctx, damodel.Request{
			Messages: []damessage.Message{damessage.Human("Reply with a brief greeting.")},
		})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		var usage *damessage.Usage
		done := false
		for chunk, nextErr := range stream.Chunks() {
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			text.WriteString(chunk.MessageDelta.TextContent())
			if chunk.MessageDelta.Usage != nil {
				usage = chunk.MessageDelta.Usage
			}
			done = done || chunk.Done
		}
		if strings.TrimSpace(text.String()) == "" {
			t.Fatal("stream returned no text")
		}
		if !done {
			t.Fatal("stream ended without a terminal chunk")
		}
		assertLiveUsage(t, usage)
	})
}

func liveAPIKey(t *testing.T) string {
	t.Helper()
	if os.Getenv("DAGO_OPENROUTER_E2E") != "1" {
		t.Skip("run make openrouter-e2e to enable live OpenRouter tests")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		t.Fatal("OPENROUTER_API_KEY is not set")
	}
	return apiKey
}

func assertLiveUsage(t *testing.T, usage *damessage.Usage) {
	t.Helper()
	if usage == nil {
		t.Fatal("response has no usage")
	}
	if usage.Provider != "openrouter" || usage.Model == "" || usage.TotalTokens <= 0 {
		t.Fatalf("usage = %#v", usage)
	}
}
