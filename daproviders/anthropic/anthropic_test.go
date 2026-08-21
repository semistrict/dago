package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

func TestInvokeSupportsCurrentAnthropicToolsBetasAndRichBlocks(t *testing.T) {
	var requestPayload map[string]any
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("x-api-key") != "secret" || request.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("headers = %#v", request.Header)
		}
		betas := request.Header.Get("anthropic-beta")
		for _, beta := range []string{"web-fetch-2025-09-10", "code-execution-2025-08-25", "mcp-client-2025-11-20", "task-budgets-2026-03-13", "user-profiles-2026-03-24"} {
			if !strings.Contains(betas, beta) {
				t.Errorf("betas %q missing %q", betas, beta)
			}
		}
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &requestPayload); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
          "id":"msg_1","model":"claude-opus-5","stop_reason":"tool_use",
          "content":[
            {"type":"thinking","thinking":"consider","signature":"sig"},
            {"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"news"}},
            {"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","url":"https://example.test","title":"Example"}]},
			{"type":"text","text":"Found it.","citations":[{"type":"web_search_result_location","url":"https://example.test","title":"Example","cited_text":"Found","start_char_index":0,"end_char_index":5}]},
            {"type":"tool_use","id":"tool_1","name":"lookup","input":{"id":"42"}}
          ],
          "usage":{"input_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}
        }`)
	}))
	defer server.Close()

	system := damessage.System("Be precise.")
	client := New("secret", "claude-opus-5", Options{
		BaseURL: server.URL, HTTPClient: server.Client(), WebSearch: true,
		HostedTools: []json.RawMessage{
			json.RawMessage(`{"type":"web_fetch_20250910","name":"web_fetch","max_uses":3}`),
			json.RawMessage(`{"type":"code_execution_20250825","name":"code_execution"}`),
		},
		MCPServers: []json.RawMessage{json.RawMessage(`{"type":"url","url":"https://mcp.example/mcp","name":"example"}`)},
		Parameters: map[string]json.RawMessage{
			"output_config":      json.RawMessage(`{"task_budget":{"total":1000}}`),
			"user_profile_id":    json.RawMessage(`"profile_1"`),
			"context_management": json.RawMessage(`{"edits":[{"type":"clear_tool_uses_20250919"}]}`),
		},
	})
	response, err := client.Invoke(t.Context(), damodel.Request{
		SystemMessage: &system, Messages: []damessage.Message{damessage.Human("Find it")},
		Tools:          []datool.Definition{{Name: "lookup", Description: "Look up a record.", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`), Strict: true}},
		Reasoning:      &damodel.Reasoning{Effort: "xhigh"},
		ResponseFormat: &damodel.ResponseFormat{Name: "answer", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)},
	})
	// The fixture deliberately returns text rather than schema JSON to exercise
	// rich blocks, so normalize it without a structured format on this call.
	if err == nil || !strings.Contains(err.Error(), "structured response") {
		t.Fatalf("structured validation error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("non-transient response was retried %d times", requests.Load())
	}
	if requestPayload["model"] != "claude-opus-5" || requestPayload["thinking"] == nil || requestPayload["context_management"] == nil {
		t.Fatalf("payload = %#v", requestPayload)
	}
	tools := requestPayload["tools"].([]any)
	if len(tools) != 4 || tools[0].(map[string]any)["strict"] != true {
		t.Fatalf("tools = %#v", tools)
	}
	output := requestPayload["output_config"].(map[string]any)
	if output["effort"] != "xhigh" || output["format"] == nil || output["task_budget"] == nil {
		t.Fatalf("output config = %#v", output)
	}

	response, err = normalizeResponse([]byte(`{
      "id":"msg_1","model":"claude-opus-5","stop_reason":"tool_use",
      "content":[
        {"type":"thinking","thinking":"consider","signature":"sig"},
        {"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"news"}},
        {"type":"web_search_tool_result","tool_use_id":"srv_1","content":[]},
		{"type":"text","text":"Found it.","citations":[{"type":"web_search_result_location","url":"https://example.test","title":"Example","cited_text":"Found","start_char_index":0,"end_char_index":5}]},
        {"type":"tool_use","id":"tool_1","name":"lookup","input":{"id":"42"}}
      ],"usage":{"input_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":4}
    }`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].Name != "lookup" || len(response.Message.Content) != 4 {
		t.Fatalf("response = %#v", response)
	}
	if response.Message.Content[0].Type != damessage.BlockReasoning || response.Message.Content[1].Type != damessage.BlockServerTool || response.Message.Content[2].Type != damessage.BlockSearchResult {
		t.Fatalf("content = %#v", response.Message.Content)
	}
	if citations := response.Message.Content[3].Citations; len(citations) != 1 || citations[0].URL != "https://example.test" || citations[0].StartIndex == nil || *citations[0].StartIndex != 0 {
		t.Fatalf("citations = %#v", citations)
	}
	if response.Message.Usage.InputTokens != 10 || response.Message.Usage.TotalTokens != 14 {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
	reason, _ := damodel.Outcome(response.Message)
	if reason != damodel.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", reason)
	}
}

func TestPromptCacheBreakpointsAreSerialized(t *testing.T) {
	cache := json.RawMessage(`{"type":"ephemeral","ttl":"1h"}`)
	system := damessage.System("Stable instructions")
	system.Content[0].Extra = map[string]json.RawMessage{"cache_control": cache}
	client := New("secret", "claude-sonnet-5", Options{RetryBackoff: []time.Duration{}})
	payload, _, err := client.payload(damodel.Request{
		SystemMessage: &system,
		Messages:      []damessage.Message{damessage.Human("Hello")},
		Tools: []datool.Definition{{
			Name: "lookup", Description: "Look up a value.", InputSchema: json.RawMessage(`{"type":"object"}`),
			Extra: map[string]json.RawMessage{"cache_control": cache},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	systemBlocks := payload["system"].([]any)
	if got := systemBlocks[0].(map[string]any)["cache_control"].(map[string]any)["ttl"]; got != "1h" {
		t.Fatalf("system cache ttl = %#v", got)
	}
	tools := payload["tools"].([]any)
	if got := tools[0].(map[string]any)["cache_control"].(map[string]any)["ttl"]; got != "1h" {
		t.Fatalf("tool cache ttl = %#v", got)
	}
}

func TestStreamEmitsIncrementalTextCitationsToolsAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true {
			t.Fatalf("stream = %#v", payload["stream"])
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg_stream","model":"claude-sonnet-5","usage":{"input_tokens":4,"cache_read_input_tokens":2}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Found"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.test","title":"Example","cited_text":"Found","start_char_index":0,"end_char_index":5}}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":\"42\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = fmt.Fprintf(writer, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	client := New("secret", "claude-sonnet-5", Options{BaseURL: server.URL, HTTPClient: server.Client()})
	stream, err := client.Stream(t.Context(), damodel.Request{Messages: []damessage.Message{damessage.Human("search")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var text string
	var citations []damessage.Citation
	var calls []damessage.ToolCall
	var usage *damessage.Usage
	var reason damodel.FinishReason
	done := false
	for {
		chunk, nextErr := stream.Next(t.Context())
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		text += chunk.MessageDelta.TextContent()
		for _, block := range chunk.MessageDelta.Content {
			citations = append(citations, block.Citations...)
		}
		calls = append(calls, chunk.MessageDelta.ToolCalls...)
		if chunk.MessageDelta.Usage != nil {
			usage = chunk.MessageDelta.Usage
		}
		if value, _ := damodel.Outcome(chunk.MessageDelta); value != "" {
			reason = value
		}
		done = done || chunk.Done
	}
	if text != "Found" || len(citations) != 1 || citations[0].URL != "https://example.test" {
		t.Fatalf("text = %q, citations = %#v", text, citations)
	}
	if len(calls) != 1 || calls[0].Name != "lookup" || string(calls[0].Arguments) != `{"id":"42"}` {
		t.Fatalf("tool calls = %#v", calls)
	}
	if usage == nil || usage.InputTokens != 6 || usage.OutputTokens != 3 || reason != damodel.FinishReasonToolCalls || !done {
		t.Fatalf("usage = %#v, reason = %q, done = %v", usage, reason, done)
	}
}

func TestAssistantHostedBlocksReplayExactly(t *testing.T) {
	raw := json.RawMessage(`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"go"}}`)
	request := damodel.Request{Messages: []damessage.Message{{
		Role:    damessage.RoleAssistant,
		Content: []damessage.ContentBlock{{Type: damessage.BlockServerTool, ID: "srv_1", Name: "web_search", Extra: map[string]json.RawMessage{rawBlockMetadataKey: raw}}},
	}}}
	_, messages, err := formatMessages(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(messages[0]["content"])
	if !strings.Contains(string(encoded), `"type":"server_tool_use"`) || !strings.Contains(string(encoded), `"query":"go"`) {
		t.Fatalf("replayed content = %s", encoded)
	}
}

func TestContextOverflowIsRecoverable(t *testing.T) {
	err := responseError(http.StatusBadRequest, []byte(`{"error":{"type":"invalid_request_error","message":"prompt exceeds context token limit"}}`))
	if !errors.Is(err, damodel.ErrContextOverflow) {
		t.Fatalf("error = %v", err)
	}
}
