package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/datool"
)

func TestInvokeMapsOpenRouterResponsesRequest(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if value := request.Header.Get("Authorization"); value != "Bearer secret" {
			t.Fatalf("authorization = %q", value)
		}
		if value := request.Header.Get("HTTP-Referer"); value != "https://example.test/agent" {
			t.Fatalf("HTTP-Referer = %q", value)
		}
		if value := request.Header.Get("X-OpenRouter-Title"); value != "Example Agent" {
			t.Fatalf("X-OpenRouter-Title = %q", value)
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
          "id":"resp_1","status":"completed","model":"anthropic/claude-sonnet-4.6",
          "output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}],
          "usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12,"cost":0.0012}
        }`)
	}))
	defer server.Close()

	noFallbacks := false
	requireParameters := true
	routing := &ProviderRouting{
		Order: []string{"anthropic", "google-vertex"}, Ignore: []string{"azure"},
		AllowFallbacks: &noFallbacks, RequireParameters: &requireParameters,
		DataCollection: "deny", Sort: "throughput",
	}
	client := New("secret", "anthropic/claude-sonnet-4.6", Options{BaseURL: server.URL + "/responses", HTTPClient: server.Client(),
		AppURL: "https://example.test/agent", AppTitle: "Example Agent", Routing: routing,
		ContextWindow: 200_000, MaxOutputTokens: 32_000, RetryBackoff: []time.Duration{},
	})
	routing.Order[0] = "mutated-after-construction"
	noFallbacks = true
	requireParameters = false

	response, err := client.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("hello")},
		Tools: []datool.Definition{{
			Name: "lookup", Description: "Look up a value.", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "done" || response.Message.Usage == nil || response.Message.Usage.TotalTokens != 12 ||
		response.Message.Usage.CostUSD != 0.0012 || response.Message.Usage.Provider != "openrouter" || response.Message.Usage.Model != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("response = %#v", response)
	}
	profile := client.Profile()
	if profile.Provider != "openrouter" || profile.Model != "anthropic/claude-sonnet-4.6" || profile.ContextWindow != 200_000 {
		t.Fatalf("profile = %#v", profile)
	}
	provider, ok := got["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider = %#v", got["provider"])
	}
	order, ok := provider["order"].([]any)
	if !ok || len(order) != 2 || order[0] != "anthropic" || provider["allow_fallbacks"] != false || provider["require_parameters"] != true || provider["data_collection"] != "deny" {
		t.Fatalf("provider = %#v", provider)
	}
	if got["model"] != "anthropic/claude-sonnet-4.6" || got["max_output_tokens"] != float64(32_000) {
		t.Fatalf("request = %#v", got)
	}
}

func TestStreamIgnoresOpenRouterKeepaliveComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, ": OPENROUTER PROCESSING\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n")
	}))
	defer server.Close()
	client := New("secret", "openai/gpt-5", Options{BaseURL: server.URL, HTTPClient: server.Client(), RetryBackoff: []time.Duration{}})
	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		text += chunk.MessageDelta.TextContent()
	}
	if text != "hello" {
		t.Fatalf("text = %q", text)
	}
}

func TestErrorUsesOpenRouterIdentityAndContextOverflowType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error_type":"context_length_exceeded","error":{"code":"invalid_prompt","message":"too long"}}`)
	}))
	defer server.Close()
	client := New("secret", "google/gemini-3.1-pro", Options{BaseURL: server.URL, HTTPClient: server.Client(), RetryBackoff: []time.Duration{}})
	_, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if !errors.Is(err, damodel.ErrContextOverflow) {
		t.Fatalf("error = %v, want context overflow", err)
	}
	var providerErr *openai.Error
	if !errors.As(err, &providerErr) || providerErr.Provider != "openrouter" || providerErr.Model != "google/gemini-3.1-pro" {
		t.Fatalf("provider error = %#v", providerErr)
	}
	if !strings.Contains(err.Error(), "openrouter: status 400") {
		t.Fatalf("error = %q", err)
	}
}

func TestRetryEventUsesOpenRouterIdentity(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		if attempts == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(writer, `{"error":{"code":"provider_unavailable","message":"try again"}}`)
			return
		}
		_, _ = io.WriteString(writer, `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer server.Close()
	client := New("secret", "meta-llama/llama-3.3-70b-instruct", Options{BaseURL: server.URL, HTTPClient: server.Client(),
		RetryBackoff: []time.Duration{time.Nanosecond},
	})
	var events []damodel.RetryEvent
	ctx := damodel.WithRetryObserver(context.Background(), func(_ context.Context, event damodel.RetryEvent) {
		events = append(events, event)
	})
	if _, err := client.Invoke(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Provider != "openrouter" || events[0].Model != "meta-llama/llama-3.3-70b-instruct" || events[0].Status != http.StatusServiceUnavailable {
		t.Fatalf("retry events = %#v", events)
	}
}

func TestRoutingValidation(t *testing.T) {
	tests := []struct {
		name    string
		routing ProviderRouting
		want    string
	}{
		{name: "empty provider", routing: ProviderRouting{Only: []string{""}}, want: "routing only contains an empty value"},
		{name: "data policy", routing: ProviderRouting{DataCollection: "sometimes"}, want: "data collection must be allow or deny"},
		{name: "sort", routing: ProviderRouting{Sort: "random"}, want: `routing sort "random" is unsupported`},
		{name: "throughput", routing: ProviderRouting{PreferredMinThroughput: -1}, want: "minimum throughput cannot be negative"},
		{name: "latency", routing: ProviderRouting{PreferredMaxLatency: -1}, want: "maximum latency cannot be negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				value := recover()
				if value == nil || !strings.Contains(fmt.Sprint(value), test.want) {
					t.Fatalf("panic = %v, want %q", value, test.want)
				}
			}()
			New("secret", "model", Options{Routing: &test.routing})
		})
	}
}
