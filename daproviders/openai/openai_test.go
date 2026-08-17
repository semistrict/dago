package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

func requireConstructorPanic(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("constructor did not panic")
		}
	}()
	call()
}

func TestConstructorsPanicForMissingRequiredValues(t *testing.T) {
	tests := map[string]func(){
		"API key":           func() { NewAPIKey("", "model") },
		"model":             func() { NewAPIKey("secret", "") },
		"credential source": func() { NewOAuth(nil, "model") },
		"provider":          func() { NewCompatibleAPIKey("secret", "", "model") },
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) { requireConstructorPanic(t, call) })
	}
}

func TestInvokeMapsResponsesAPI(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if value := request.Header.Get("Authorization"); value != "Bearer secret" {
			t.Fatalf("authorization = %q", value)
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
          "id":"resp_1","status":"completed",
          "output":[
            {"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]},
            {"type":"function_call","id":"item_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"go\"}"}
          ],
          "usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}
        }`)
	}))
	defer server.Close()

	client := NewAPIKey("secret", "test-model", Options{BaseURL: server.URL, HTTPClient: server.Client(), MaxOutputTokens: 4096})

	response, err := client.Invoke(context.Background(), damodel.Request{
		Messages:    []damessage.Message{damessage.System("be useful"), damessage.Human("hello")},
		Tools:       []datool.Definition{{Name: "lookup", Description: "look up a value", InputSchema: json.RawMessage(`{"type":"object"}`), Strict: true}},
		PromptCache: &damodel.PromptCache{Key: "thread-key", Retention: "24h"},
		Reasoning:   &damodel.Reasoning{Effort: "high", Summary: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "done" || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].ID != "call_1" || string(response.Message.ToolCalls[0].Arguments) != `{"q":"go"}` {
		t.Fatalf("response = %#v", response)
	}
	if response.Message.Usage == nil || response.Message.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
	if got["model"] != "test-model" || got["parallel_tool_calls"] != true {
		t.Fatalf("request = %#v", got)
	}
	if got["max_output_tokens"] != float64(4096) {
		t.Fatalf("max_output_tokens = %#v", got["max_output_tokens"])
	}
	if got["store"] != false {
		t.Fatalf("store = %#v, want false", got["store"])
	}
	if got["prompt_cache_key"] != "thread-key" || got["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt cache = %#v", got)
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", got["reasoning"])
	}
	include, ok := got["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", got["include"])
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["strict"] != true {
		t.Fatalf("tools = %#v", got["tools"])
	}
}

func TestRequestUsesInstructionsAndOmitsEmptySystemMessages(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(writer, `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})

	separate := damessage.System("separate")
	_, err := client.Invoke(context.Background(), damodel.Request{
		SystemMessage: &separate,
		Messages: []damessage.Message{
			damessage.System("first"), damessage.System("  "), damessage.System("second"), damessage.Human("hello"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["instructions"] != "separate\n\nfirst\n\nsecond" {
		t.Fatalf("instructions = %#v", got["instructions"])
	}
	input, ok := got["input"].([]any)
	if !ok || len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %#v", got["input"])
	}
}

func TestProfileReportsReasoningLevelsAndDefault(t *testing.T) {
	client := NewAPIKey("secret", "m", Options{DefaultReasoning: &damodel.Reasoning{Effort: "medium", Summary: "auto"}})

	profile := client.Profile()
	if profile.DefaultReasoningLevel != "medium" || len(profile.ReasoningLevels) != 5 || profile.ReasoningLevels[4] != "xhigh" || !profile.SupportsSeparateSystemMessage {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestResponsesWebSocketDefaultsAndOverrides(t *testing.T) {
	standard := NewAPIKey("secret", "m")

	if standard.websockets == nil {
		t.Fatal("standard API client did not enable websocket transport")
	}
	if standard.options.ServerCompaction == nil || !*standard.options.ServerCompaction || standard.options.CompactionThreshold != defaultServerCompactionThreshold {
		t.Fatalf("standard compaction options = enabled %#v, threshold %d", standard.options.ServerCompaction, standard.options.CompactionThreshold)
	}
	knownWindow := NewAPIKey("secret", "m", Options{ContextWindow: 1000})

	if knownWindow.options.CompactionThreshold != 900 {
		t.Fatalf("derived compaction threshold = %d, want 900", knownWindow.options.CompactionThreshold)
	}
	clampedWindow := NewAPIKey("secret", "m", Options{ContextWindow: 1000, CompactionThreshold: 950})

	if clampedWindow.options.CompactionThreshold != 900 {
		t.Fatalf("clamped compaction threshold = %d, want 900", clampedWindow.options.CompactionThreshold)
	}
	httpOnly := NewAPIKey("secret", "m", Options{ResponsesWebSocket: new(false)})

	if httpOnly.websockets != nil {
		t.Fatal("explicit websocket disable was ignored")
	}
	custom := NewAPIKey("secret", "m", Options{BaseURL: "https://provider.example/v1"})

	if custom.websockets != nil {
		t.Fatal("custom endpoint enabled websocket transport without an explicit capability")
	}
	if custom.options.ServerCompaction == nil || *custom.options.ServerCompaction {
		t.Fatal("custom endpoint enabled server compaction without an explicit capability")
	}
	customCompaction := NewAPIKey("secret",
		"m", Options{
			BaseURL: "https://provider.example/v1", CompactionThreshold: 100,
		})

	if customCompaction.options.ServerCompaction == nil || !*customCompaction.options.ServerCompaction || customCompaction.options.CompactionThreshold != 100 {
		t.Fatalf("custom compaction opt-in = enabled %#v, threshold %d", customCompaction.options.ServerCompaction, customCompaction.options.CompactionThreshold)
	}
	disabled := false
	customDisabled := NewAPIKey("secret",
		"m", Options{
			BaseURL: "https://provider.example/v1", ServerCompaction: &disabled, CompactionThreshold: 100,
		})

	if customDisabled.options.ServerCompaction == nil || *customDisabled.options.ServerCompaction {
		t.Fatal("explicit server compaction disable was ignored")
	}
	requireConstructorPanic(t, func() { NewAPIKey("secret", "m", Options{CompactionThreshold: -1}) })
	customWebSocket := NewAPIKey("secret",
		"m", Options{
			BaseURL: "https://provider.example/v1", ResponsesWebSocket: new(true),
		})

	if customWebSocket.websockets == nil {
		t.Fatal("custom endpoint websocket opt-in was ignored")
	}
}

func TestResponsesRemoteCompactionV2TriggerRetentionAndReplay(t *testing.T) {
	requests := make(chan map[string]any, 3)
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		for turn := range 3 {
			_, data, err := connection.Read(request.Context())
			if err != nil {
				serverErrors <- err
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				serverErrors <- err
				return
			}
			requests <- payload
			events := []string{`{"type":"response.completed","response":{"id":"resp-setup","status":"completed","output":[{"type":"message","id":"msg-setup","role":"assistant","content":[{"type":"output_text","text":"ready"}]}]}}`}
			if turn == 1 {
				events = []string{
					`{"type":"response.output_item.done","item":{"type":"compaction","id":"cmp_1","encrypted_content":"opaque-state"}}`,
					`{"type":"response.completed","response":{"id":"compact-1","status":"completed","output":[]}}`,
				}
			} else if turn == 2 {
				events = []string{`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"continued"}]}]}}`}
			}
			for _, event := range events {
				if err := connection.Write(request.Context(), websocket.MessageText, []byte(event)); err != nil {
					serverErrors <- err
					return
				}
			}
		}
	}))
	defer server.Close()
	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(),
			ResponsesWebSocket: new(true), ServerCompaction: new(true), CompactionThreshold: 1000,
		})

	setup := damessage.Human("set up the conversation")
	firstResponse, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{setup}})
	if err != nil {
		t.Fatal(err)
	}
	client.options.CompactionThreshold = 1
	newInput := damessage.Human("remember this context")
	response, err := client.Invoke(context.Background(), damodel.Request{
		Messages: []damessage.Message{setup, firstResponse.Message, newInput},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "continued" || len(response.Message.Content) != 2 {
		t.Fatalf("response = %#v", response.Message)
	}
	compaction := response.Message.Content[0]
	if compaction.Type != damessage.BlockNonStandard || compaction.ID != "cmp_1" {
		t.Fatalf("compaction block = %#v", compaction)
	}
	var state map[string]any
	if err := json.Unmarshal(compaction.NonStandard, &state); err != nil || state["type"] != "compaction" || state["encrypted_content"] != "opaque-state" {
		t.Fatalf("compaction state = %#v, error = %v", state, err)
	}
	replayed, err := inputItems(response.Message)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 {
		t.Fatalf("replayed output = %#v", replayed)
	}
	setupRequest := <-requests
	compactionRequest := <-requests
	resumeRequest := <-requests
	if _, exists := setupRequest["previous_response_id"]; exists {
		t.Fatalf("setup request unexpectedly chained a response: %#v", setupRequest)
	}
	compactionInput := compactionRequest["input"].([]any)
	if compactionRequest["previous_response_id"] != "resp-setup" || len(compactionInput) != 2 || compactionInput[1].(map[string]any)["type"] != "compaction_trigger" {
		t.Fatalf("incremental compaction request = %#v", compactionRequest)
	}
	resumeInput := resumeRequest["input"].([]any)
	if len(resumeInput) != 3 || resumeInput[0].(map[string]any)["role"] != "user" || resumeInput[1].(map[string]any)["role"] != "user" || resumeInput[2].(map[string]any)["type"] != "compaction" {
		t.Fatalf("post-compaction input = %#v", resumeInput)
	}
	if _, exists := resumeRequest["previous_response_id"]; exists {
		t.Fatalf("post-compaction request incorrectly chained old response: %#v", resumeRequest)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestRemoteCompactionRetentionKeepsNewestUserMessagesWithinBudget(t *testing.T) {
	message := func(role, text string) map[string]any {
		return map[string]any{
			"type": "message", "role": role,
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		}
	}
	input := []any{
		message("user", "old-old"),
		message("developer", "discard developer context"),
		message("assistant", "discard final answer"),
		message("user", "middle12"),
		message("user", "new!"),
	}
	retained := retainedCompactionInputWithBudget(input, 2)
	if len(retained) != 2 {
		t.Fatalf("retained = %#v", retained)
	}
	textAt := func(item any) string {
		return item.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if got := textAt(retained[0]); got != "midd" {
		t.Fatalf("truncated boundary message = %q, want %q", got, "midd")
	}
	if got := textAt(retained[1]); got != "new!" {
		t.Fatalf("newest message = %q, want %q", got, "new!")
	}
}

func TestErrorReportsNativeRetryMetadataAndRetryAfter(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"2"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down","type":"rate_limit"}}`)),
	}
	err := responseError(response)
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error type = %T", err)
	}
	providerErr.Provider = "openai"
	providerErr.Model = "gpt-native"
	event := providerErr.RetryEvent(1, time.Second)
	if !event.Retryable || event.Status != http.StatusTooManyRequests || event.Delay != 2*time.Second || event.Provider != "openai" || event.Model != "gpt-native" || event.Err != "slow down" {
		t.Fatalf("retry event = %#v", event)
	}
	clientErr := (&Error{Status: http.StatusForbidden}).RetryEvent(1, 0)
	if clientErr.Retryable {
		t.Fatalf("client error marked retryable: %#v", clientErr)
	}
	continuationErr := apiErrorValue(&apiError{Code: "previous_response_not_found", Message: "expired"}, http.StatusBadRequest)
	var continuationReporter damodel.RetryReporter
	if !errors.As(continuationErr, &continuationReporter) || !continuationReporter.RetryEvent(1, 0).Retryable {
		t.Fatalf("continuation error is not retryable: %v", continuationErr)
	}
}

func TestInvokeMapsIncompleteAndRefusalOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantReason  damodel.FinishReason
		wantRefusal string
	}{
		{
			name:       "max output tokens",
			body:       `{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`,
			wantReason: damodel.FinishReasonMaxTokens,
		},
		{
			name:       "refusal",
			body:       `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"not allowed"}]}]}`,
			wantReason: damodel.FinishReasonRefusal, wantRefusal: "not allowed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})

			response, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
			if err != nil {
				t.Fatal(err)
			}
			reason, refusal := damodel.Outcome(response.Message)
			if reason != test.wantReason {
				t.Fatalf("finish reason = %q, want %q", reason, test.wantReason)
			}
			if test.wantRefusal != "" && (refusal == nil || refusal.Explanation != test.wantRefusal) {
				t.Fatalf("refusal = %#v", refusal)
			}
		})
	}
}

func TestInvokeMapsProviderWebSearch(t *testing.T) {
	var got map[string]any
	start, end := 0, 6
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(writer, `{
          "id":"resp_search","status":"completed",
          "output":[
            {"type":"web_search_call","id":"search_1","action":{"type":"search","queries":["dago docs"]}},
            {"type":"message","content":[{"type":"output_text","text":"result","annotations":[{"type":"url_citation","url":"https://example.test","title":"Example","start_index":0,"end_index":6}]}]}
          ]
        }`)
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client(), WebSearch: true})

	response, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("search")}})
	if err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tools = %#v", tools)
	}
	if len(response.Message.Content) != 2 || response.Message.Content[0].Type != damessage.BlockServerTool {
		t.Fatalf("content = %#v", response.Message.Content)
	}
	if got := response.Message.Content[0].Extra["arguments"]; string(got) != `{"query":"dago docs"}` {
		t.Fatalf("arguments = %s", got)
	}
	citations := response.Message.Content[1].Citations
	if len(citations) != 1 || citations[0].URL != "https://example.test" || citations[0].StartIndex == nil || *citations[0].StartIndex != start || citations[0].EndIndex == nil || *citations[0].EndIndex != end {
		t.Fatalf("citations = %#v", citations)
	}
}

func TestInvokeReplaysProviderWebSearchOutput(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			_, _ = io.WriteString(writer, `{
			  "id":"resp_search","status":"completed",
			  "output":[
			    {"type":"web_search_call","id":"search_1","status":"completed","action":{"type":"search","queries":["NYC weather"]},"provider_extension":{"opaque":true}},
			    {"type":"message","content":[{"type":"output_text","text":"Cloudy."}]}
			  ]
			}`)
			return
		}
		_, _ = io.WriteString(writer, `{"id":"resp_followup","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"I used web search."}]}]}`)
	}))
	defer server.Close()

	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client(), WebSearch: true})

	first, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("weather in NYC")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{
		damessage.Human("weather in NYC"), first.Message, damessage.Human("which tool did you use?"),
	}}); err != nil {
		t.Fatal(err)
	}

	input := requests[1]["input"].([]any)
	var replay map[string]any
	for _, value := range input {
		item, ok := value.(map[string]any)
		if ok && item["type"] == "web_search_call" {
			replay = item
			break
		}
	}
	if replay == nil {
		t.Fatalf("input does not contain replayed web search: %#v", input)
	}
	if replay["id"] != "search_1" || replay["status"] != "completed" {
		t.Fatalf("replayed web search identity = %#v", replay)
	}
	extension, ok := replay["provider_extension"].(map[string]any)
	if !ok || extension["opaque"] != true {
		t.Fatalf("replayed provider extension = %#v", replay["provider_extension"])
	}
}

func TestInvokeOmitsLegacyDisplayOnlyServerToolBlock(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(writer, `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer server.Close()

	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client(), WebSearch: true})

	legacy := damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{
		{Type: damessage.BlockServerTool, ID: "search_legacy", Name: "web_search", Extra: map[string]json.RawMessage{"arguments": json.RawMessage(`{"query":"NYC weather"}`)}},
		{Type: damessage.BlockText, Text: "Cloudy."},
	}}
	if _, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{legacy, damessage.Human("which tool?")}}); err != nil {
		t.Fatal(err)
	}
	for _, value := range got["input"].([]any) {
		item := value.(map[string]any)
		if item["type"] == "web_search_call" {
			t.Fatalf("legacy display-only block was replayed: %#v", item)
		}
	}
}

func TestInvokeMapsToolHistoryAndStructuredOutput(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&got)
		_, _ = io.WriteString(writer, `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"{\"answer\":42}"}]}]}`)
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	assistant := damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "c", Name: "calculate", Arguments: json.RawMessage(`{"x":40}`)}}}
	response, err := client.Invoke(context.Background(), damodel.Request{
		Messages:       []damessage.Message{assistant, damessage.Tool("c", "42")},
		ResponseFormat: &damodel.ResponseFormat{Name: "answer", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Structured) != `{"answer":42}` {
		t.Fatalf("structured = %s", response.Structured)
	}
	input := got["input"].([]any)
	if input[0].(map[string]any)["type"] != "function_call" || input[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
}

func TestInvokePreservesAndReplaysEncryptedReasoning(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			_, _ = io.WriteString(writer, `{
                  "id":"r1","output":[
                    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Need a lookup."}],"encrypted_content":"opaque-state"},
                    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"go\"}"}
                  ]}`)
			return
		}
		_, _ = io.WriteString(writer, `{"id":"r2","output":[{"type":"message","id":"m2","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()

	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	first, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("look it up")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.Content) != 1 || first.Message.Content[0].Type != damessage.BlockReasoning || first.Message.Content[0].Reasoning != "Need a lookup." {
		t.Fatalf("first reasoning = %#v", first.Message.Content)
	}
	if len(first.Message.ToolCalls) != 1 {
		t.Fatalf("first tool calls = %#v", first.Message.ToolCalls)
	}
	if _, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{first.Message, damessage.Tool("call_1", "result")}}); err != nil {
		t.Fatal(err)
	}
	input := requests[1]["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("replayed input = %#v", input)
	}
	reasoning := input[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "opaque-state" {
		t.Fatalf("replayed reasoning = %#v", reasoning)
	}
	if input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("replayed tool turn = %#v", input)
	}
}

func TestReplayReasoningWithoutSummaryUsesEmptyArray(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			_, _ = io.WriteString(writer, `{
                  "id":"r1","output":[
                    {"type":"reasoning","id":"rs_1","encrypted_content":"opaque-state"},
                    {"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"first"}]}
                  ]}`)
			return
		}
		_, _ = io.WriteString(writer, `{"id":"r2","output":[{"type":"message","id":"m2","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()

	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	first, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("think")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{first.Message}}); err != nil {
		t.Fatal(err)
	}
	input := requests[1]["input"].([]any)
	reasoning := input[0].(map[string]any)
	summary, ok := reasoning["summary"].([]any)
	if !ok || summary == nil || len(summary) != 0 {
		t.Fatalf("replayed reasoning summary = %#v, want []", reasoning["summary"])
	}
}

func TestStreamPreservesReasoningSummaryAndOpaqueState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"output_index\":0,\"delta\":\"thinking\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"thinking\"}],\"encrypted_content\":\"opaque\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	delta, err := stream.Next(context.Background())
	if err != nil || delta.MessageDelta.Content[0].Reasoning != "thinking" {
		t.Fatalf("reasoning delta = %#v, %v", delta, err)
	}
	state, err := stream.Next(context.Background())
	if err != nil || len(state.MessageDelta.Content[0].Extra[reasoningStateKey]) == 0 {
		t.Fatalf("reasoning state = %#v, %v", state, err)
	}
	done, err := stream.Next(context.Background())
	if err != nil || !done.Done {
		t.Fatalf("done = %#v, %v", done, err)
	}
}

func TestStreamYieldsTextToolCallUsageAndDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"i\",\"call_id\":\"c\",\"name\":\"lookup\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i\",\"delta\":\"{\\\"q\\\":\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i\",\"delta\":\"\\\"go\\\"}\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"i\",\"arguments\":\"{\\\"q\\\":\\\"go\\\"}\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.MessageDelta.TextContent() != "hi" {
		t.Fatalf("first = %#v, %v", first, err)
	}
	second, err := stream.Next(context.Background())
	if err != nil || len(second.MessageDelta.ToolCalls) != 1 || string(second.MessageDelta.ToolCalls[0].Arguments) != `{"q":"go"}` {
		t.Fatalf("second = %#v, %v", second, err)
	}
	third, err := stream.Next(context.Background())
	if err != nil || !third.Done || third.MessageDelta.Usage.TotalTokens != 3 {
		t.Fatalf("third = %#v, %v", third, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestStreamPreservesStructuredResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"value\":\"streamed\"}"}]}],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}}`+"\n\n")
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	stream, err := client.Stream(t.Context(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("return structured output")},
		ResponseFormat: &damodel.ResponseFormat{
			Name: "answer", Schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`), Strict: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunk, err := stream.Next(t.Context())
	if err != nil || !chunk.Done || string(chunk.Structured) != `{"value":"streamed"}` || chunk.MessageDelta.Usage.TotalTokens != 7 {
		t.Fatalf("structured chunk = %#v, %v", chunk, err)
	}
}

func TestResponsesWebSocketReusesConnectionWithIncrementalInput(t *testing.T) {
	requests := make(chan map[string]any, 3)
	headers := make(chan http.Header, 1)
	serverErrors := make(chan error, 1)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connections.Add(1)
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		headers <- request.Header.Clone()
		for turn := range 3 {
			messageType, data, err := connection.Read(request.Context())
			if err != nil {
				serverErrors <- err
				return
			}
			if messageType != websocket.MessageText {
				serverErrors <- fmt.Errorf("message type = %v", messageType)
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				serverErrors <- err
				return
			}
			requests <- payload
			var event string
			switch turn {
			case 0:
				event = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"lookup","arguments":"{\"q\":\"go\"}"}]}}`
			case 1:
				event = `{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`
			default:
				event = `{"type":"response.completed","response":{"id":"resp-3","status":"completed","output":[{"type":"message","id":"msg-3","role":"assistant","content":[{"type":"output_text","text":"fresh"}]}]}}`
			}
			if err := connection.Write(request.Context(), websocket.MessageText, []byte(event)); err != nil {
				serverErrors <- err
				return
			}
		}
	}))
	defer server.Close()

	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(), ResponsesWebSocket: new(true),
		})

	human := damessage.Human("hello")
	first, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{human}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.ToolCalls) != 1 || first.Message.ToolCalls[0].ID != "call-1" {
		t.Fatalf("first response = %#v", first)
	}
	second, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{
		human, first.Message, damessage.Tool("call-1", "result"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Message.TextContent() != "done" {
		t.Fatalf("second response = %#v", second)
	}
	third, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("unrelated")}})
	if err != nil {
		t.Fatal(err)
	}
	if third.Message.TextContent() != "fresh" {
		t.Fatalf("third response = %#v", third)
	}

	firstRequest := <-requests
	secondRequest := <-requests
	thirdRequest := <-requests
	if firstRequest["type"] != "response.create" {
		t.Fatalf("first request type = %#v", firstRequest["type"])
	}
	if _, exists := firstRequest["stream"]; exists {
		t.Fatalf("websocket request includes stream: %#v", firstRequest)
	}
	if _, exists := firstRequest["previous_response_id"]; exists {
		t.Fatalf("first request previous response = %#v", firstRequest["previous_response_id"])
	}
	firstInput, _ := firstRequest["input"].([]any)
	if len(firstInput) != 1 {
		t.Fatalf("first input = %#v", firstRequest["input"])
	}
	if secondRequest["previous_response_id"] != "resp-1" {
		t.Fatalf("second request previous response = %#v", secondRequest["previous_response_id"])
	}
	secondInput, _ := secondRequest["input"].([]any)
	if len(secondInput) != 1 || secondInput[0].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("second input = %#v", secondRequest["input"])
	}
	if _, exists := thirdRequest["previous_response_id"]; exists {
		t.Fatalf("unrelated history reused previous response: %#v", thirdRequest)
	}
	thirdInput, _ := thirdRequest["input"].([]any)
	if len(thirdInput) != 1 || thirdInput[0].(map[string]any)["role"] != "user" {
		t.Fatalf("third input = %#v", thirdRequest["input"])
	}
	if connections.Load() != 1 {
		t.Fatalf("connections = %d", connections.Load())
	}
	header := <-headers
	if header.Get("Authorization") != "Bearer secret" || header.Get("OpenAI-Beta") != responsesWebSocketBeta {
		t.Fatalf("websocket headers = %#v", header)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestResponsesWebSocketAnswersPingWhileIdle(t *testing.T) {
	idle := make(chan struct{})
	pongReceived := make(chan struct{})
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()

		if _, _, err := connection.Read(request.Context()); err != nil {
			serverErrors <- err
			return
		}
		firstEvent := `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"first"}]}]}}`
		if err := connection.Write(request.Context(), websocket.MessageText, []byte(firstEvent)); err != nil {
			serverErrors <- err
			return
		}

		<-idle
		secondRequest := make(chan error, 1)
		go func() {
			_, _, err := connection.Read(request.Context())
			secondRequest <- err
		}()
		pingContext, cancelPing := context.WithTimeout(request.Context(), time.Second)
		defer cancelPing()
		if err := connection.Ping(pingContext); err != nil {
			serverErrors <- fmt.Errorf("idle ping: %w", err)
			return
		}
		close(pongReceived)

		if err := <-secondRequest; err != nil {
			serverErrors <- err
			return
		}
		secondEvent := `{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","id":"msg-2","role":"assistant","content":[{"type":"output_text","text":"second"}]}]}}`
		if err := connection.Write(request.Context(), websocket.MessageText, []byte(secondEvent)); err != nil {
			serverErrors <- err
		}
	}))
	defer server.Close()

	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(), ResponsesWebSocket: new(true),
		})

	human := damessage.Human("hello")
	first, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{human}})
	if err != nil {
		t.Fatal(err)
	}
	close(idle)
	select {
	case <-pongReceived:
	case err := <-serverErrors:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("client did not answer websocket ping while connection was idle")
	}

	second, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{
		human, first.Message, damessage.Human("again"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Message.TextContent() != "second" {
		t.Fatalf("second response = %#v", second)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestResponsesWebSocketReadCancellationClosesConnection(t *testing.T) {
	requestReceived := make(chan struct{})
	connectionClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(request.Context()); err != nil {
			return
		}
		close(requestReceived)
		_, _, _ = connection.Read(context.Background())
		close(connectionClosed)
	}))
	defer server.Close()
	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(), ResponsesWebSocket: new(true),
		})

	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	<-requestReceived
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v", err)
	}
	select {
	case <-connectionClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("websocket connection remained open after cancellation")
	}
}

func TestResponsesWebSocketPrewarmChainsFirstGeneratedTurn(t *testing.T) {
	requests := make(chan map[string]any, 2)
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.CloseNow()
		for turn := range 2 {
			_, data, err := connection.Read(request.Context())
			if err != nil {
				serverErrors <- err
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				serverErrors <- err
				return
			}
			requests <- payload
			event := `{"type":"response.completed","response":{"id":"warm-1","status":"completed","output":[]}}`
			if turn == 1 {
				event = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}]}}`
			}
			if err := connection.Write(request.Context(), websocket.MessageText, []byte(event)); err != nil {
				serverErrors <- err
				return
			}
		}
	}))
	defer server.Close()
	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(), ResponsesWebSocket: new(true),
		})

	if err := client.Prewarm(context.Background(), damodel.Request{SystemMessage: new(damessage.System("be concise"))}); err != nil {
		t.Fatal(err)
	}
	response, err := client.Invoke(context.Background(), damodel.Request{
		SystemMessage: new(damessage.System("be concise")),
		Messages:      []damessage.Message{damessage.Human("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "ready" {
		t.Fatalf("response = %#v", response)
	}
	warmup := <-requests
	generated := <-requests
	if warmup["generate"] != false {
		t.Fatalf("warmup generate = %#v", warmup["generate"])
	}
	if input, _ := warmup["input"].([]any); len(input) != 0 {
		t.Fatalf("warmup input = %#v", warmup["input"])
	}
	if generated["previous_response_id"] != "warm-1" {
		t.Fatalf("generated previous response = %#v", generated["previous_response_id"])
	}
	if _, exists := generated["generate"]; exists {
		t.Fatalf("generated request includes generate: %#v", generated)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	default:
	}
}

func TestResponsesWebSocketHandshakeFailureFallsBackToHTTP(t *testing.T) {
	methods := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods <- request.Method
		if request.Method == http.MethodGet {
			http.Error(writer, "websocket unavailable", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"http\"}]}]}}\n\n")
	}))
	defer server.Close()
	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(), ResponsesWebSocket: new(true),
		})

	response, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "http" {
		t.Fatalf("response = %#v", response)
	}
	if first, second := <-methods, <-methods; first != http.MethodGet || second != http.MethodPost {
		t.Fatalf("methods = %q, %q", first, second)
	}
	if client.websockets.enabled() {
		t.Fatal("websocket transport remained enabled after handshake failure")
	}
}

func TestContextOverflowIsTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, `{"error":{"message":"too long","code":"context_length_exceeded"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	_, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if !errors.Is(err, damodel.ErrContextOverflow) {
		t.Fatalf("error = %v", err)
	}
}

func TestOAuthLoginPKCEPersistenceAndRefresh(t *testing.T) {
	var authorizationForm url.Values
	refreshes := 0
	idToken := testJWT(map[string]any{"chatgpt_account_id": "account-1"})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth/token" {
			http.NotFound(writer, request)
			return
		}
		if strings.HasPrefix(request.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			_ = request.ParseForm()
			authorizationForm = request.Form
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "id_token": idToken, "expires_in": 1})
			return
		}
		refreshes++
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "access-2", "refresh_token": "refresh-2", "id_token": idToken, "expires_in": 3600})
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "auth", "tokens.json")
	session, err := Login(context.Background(), OAuthOptions{
		Issuer: server.URL, HTTPClient: server.Client(), Listener: listener, StorePath: storePath,
		OpenURL: func(authorizeURL string) error {
			parsed, err := url.Parse(authorizeURL)
			if err != nil {
				return err
			}
			if parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("state") == "" {
				t.Fatalf("authorize URL = %s", authorizeURL)
			}
			callback, _ := url.Parse(parsed.Query().Get("redirect_uri"))
			query := callback.Query()
			query.Set("code", "authorization-code")
			query.Set("state", parsed.Query().Get("state"))
			callback.RawQuery = query.Encode()
			response, err := http.Get(callback.String())
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("callback status = %s", response.Status)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorizationForm.Get("code_verifier") == "" || authorizationForm.Get("code") != "authorization-code" {
		t.Fatalf("authorization form = %#v", authorizationForm)
	}
	info, err := os.Stat(storePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, %v", info, err)
	}
	credentials, err := session.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessToken != "access-2" || credentials.AccountID != "account-1" || refreshes != 1 {
		t.Fatalf("credentials = %#v, refreshes = %d", credentials, refreshes)
	}
	loaded, err := LoadOAuthSession(storePath, OAuthOptions{Issuer: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tokens().RefreshToken != "refresh-2" {
		t.Fatalf("loaded tokens = %#v", loaded.Tokens())
	}
}

func TestOAuthRejectsWrongState(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	_, err := Login(context.Background(), OAuthOptions{
		Issuer: server.URL, HTTPClient: server.Client(), Listener: listener,
		OpenURL: func(authorizeURL string) error {
			parsed, _ := url.Parse(authorizeURL)
			callback := parsed.Query().Get("redirect_uri") + "?code=x&state=wrong"
			response, requestErr := http.Get(callback)
			if response != nil {
				response.Body.Close()
			}
			return requestErr
		},
	})
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func testJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestOAuthCancellation(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Login(ctx, OAuthOptions{Listener: listener, OpenURL: func(string) error { return nil }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestSubscriptionAddsAccountHeader(t *testing.T) {
	var account string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		account = request.Header.Get("ChatGPT-Account-ID")
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()
	client := NewSubscription(staticCredentials{Credentials{AccessToken: "token", AccountID: "workspace"}}, "m", Options{BaseURL: server.URL, HTTPClient: server.Client(), MaxOutputTokens: 4096})

	response, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil || account != "workspace" || payload["store"] != false {
		t.Fatalf("account = %q, store = %#v, error = %v", account, payload["store"], err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %#v", payload["stream"])
	}
	if _, exists := payload["max_output_tokens"]; exists {
		t.Fatalf("subscription request unexpectedly set max_output_tokens: %#v", payload["max_output_tokens"])
	}
	if response.Message.TextContent() != "ok" || response.Message.Usage == nil || response.Message.Usage.TotalTokens != 2 {
		t.Fatalf("response = %#v", response)
	}
}

func TestOAuthTokenFileRejectsIncompleteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOAuthSession(path, OAuthOptions{})
	if err == nil {
		t.Fatal("expected incomplete token error")
	}
}

func TestExpiredOAuthRefreshHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	session := &OAuthSession{options: oauthDefaults(OAuthOptions{Issuer: server.URL, HTTPClient: server.Client()}), tokens: OAuthTokens{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := session.Credentials(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestInvokeRetriesTransientResponsesFailures(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(writer, "gateway trace abc123", http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(writer, `{"id":"ok","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()

	client := NewAPIKey("secret",
		"m", Options{
			BaseURL: server.URL, HTTPClient: server.Client(),
			RetryBackoff: []time.Duration{0, 0},
		})

	var retries []damodel.RetryEvent
	ctx := damodel.WithRetryObserver(context.Background(), func(_ context.Context, event damodel.RetryEvent) {
		retries = append(retries, event)
	})
	response, err := client.Invoke(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || response.Message.TextContent() != "done" {
		t.Fatalf("attempts = %d, response = %#v", attempts, response)
	}
	if len(retries) != 2 || retries[0].Attempt != 1 || retries[1].Attempt != 2 || retries[0].Status != http.StatusBadGateway || retries[0].Provider != "openai" || retries[0].Model != "m" {
		t.Fatalf("retry events = %#v", retries)
	}
}

func TestInvokeRetriesEmptyOrTruncatedJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "truncated", body: `{"id":"unfinished"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				attempts++
				if attempts == 1 {
					_, _ = io.WriteString(writer, test.body)
					return
				}
				_, _ = io.WriteString(writer, `{"id":"ok","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
			}))
			defer server.Close()
			client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client(), RetryBackoff: []time.Duration{0}})

			response, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
			if err != nil {
				t.Fatal(err)
			}
			if attempts != 2 || response.Message.TextContent() != "done" {
				t.Fatalf("attempts = %d, response = %#v", attempts, response)
			}
		})
	}
}

func TestInvokeDoesNotRetryClientErrorsAndBoundsBody(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		writer.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(writer, "useful prefix: "+strings.Repeat("x", 64<<10))
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client(), RetryBackoff: []time.Duration{0, 0}})

	_, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err == nil || !strings.Contains(err.Error(), "useful prefix") {
		t.Fatalf("error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if len(err.Error()) > 8<<10 {
		t.Fatalf("error length = %d, want bounded body", len(err.Error()))
	}
}

func TestInvokeMapsCachedAndReasoningUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{
			"id":"usage","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],
			"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":80},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":20},"total_tokens":150}
		}`)
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})

	response, err := client.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	usage := response.Message.Usage
	if usage == nil || usage.InputTokens != 20 || usage.InputDetails["cache_read"] != 80 || usage.OutputTokens != 50 || usage.OutputDetails["reasoning"] != 20 || usage.TotalTokens != 150 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestStreamParsesMultilineSSEAndNoTrailingNewline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, ": keepalive\n")
		_, _ = io.WriteString(writer, "event: response.output_text.delta\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\n")
		_, _ = io.WriteString(writer, "data: \"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{}}")
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})

	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	textChunk, err := stream.Next(context.Background())
	if err != nil || textChunk.MessageDelta.TextContent() != "hi" {
		t.Fatalf("text chunk = %#v, error = %v", textChunk, err)
	}
	done, err := stream.Next(context.Background())
	if err != nil || !done.Done {
		t.Fatalf("done chunk = %#v, error = %v", done, err)
	}
}

func TestStreamRejectsTruncatedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})

	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("error = %v, want ErrIncompleteStream", err)
	}
}

func TestStreamCompletedResponseRestoresCitationsAndReasoningState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"output_index\":0,\"delta\":\"think\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"think\"}],\"encrypted_content\":\"opaque\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n")
		_, _ = io.WriteString(writer, `data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"opaque"},{"type":"web_search_call","id":"search_1","action":{"queries":["Go"]}},{"type":"message","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","url":"https://example.test","title":"Example","start_index":0,"end_index":6}]}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
	}))
	defer server.Close()
	client := NewAPIKey("secret", "m", Options{BaseURL: server.URL, HTTPClient: server.Client()})

	stream, err := client.Stream(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	merged := damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant}}
	for {
		chunk, nextErr := stream.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		mergeChunk(&merged, chunk)
	}
	if merged.Message.TextContent() != "answer" {
		t.Fatalf("text = %q", merged.Message.TextContent())
	}
	if len(merged.Message.Content) != 3 || merged.Message.Content[0].Reasoning != "think" || len(merged.Message.Content[0].Extra[reasoningStateKey]) == 0 {
		t.Fatalf("content = %#v", merged.Message.Content)
	}
	if len(merged.Message.Content[1].Citations) != 1 || merged.Message.Content[1].Citations[0].URL != "https://example.test" || merged.Message.Content[2].Type != damessage.BlockServerTool {
		t.Fatalf("completed content = %#v", merged.Message.Content)
	}
	if merged.Message.Usage == nil || merged.Message.Usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v", merged.Message.Usage)
	}
}
