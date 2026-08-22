package claudeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

func TestInstalledCLIRestartReconstructsVariedToolHistory(t *testing.T) {
	if os.Getenv("DAGO_CLAUDE_AGENT_CLI_INTEGRATION") != "1" {
		t.Skip("set DAGO_CLAUDE_AGENT_CLI_INTEGRATION=1 to exercise the installed Claude CLI")
	}
	cliPath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	isolationRoot := t.TempDir()
	authenticatedHome := filepath.Join(isolationRoot, "home")
	if err := initializeHome(authenticatedHome, isolationRoot); err != nil {
		t.Fatal(err)
	}
	const customizationSentinel = "LOCAL_CUSTOMIZATION_MUST_NOT_LOAD"
	if err := os.WriteFile(filepath.Join(authenticatedHome, ".claude", "CLAUDE.md"), []byte(customizationSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	hookMarker := filepath.Join(isolationRoot, "hook-ran")
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "/usr/bin/touch " + hookMarker}},
	}}}})
	if err := os.WriteFile(filepath.Join(authenticatedHome, ".claude", "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var resultCounts []int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			http.NotFound(writer, request)
			return
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Error(readErr)
			return
		}
		if bytes.Contains(body, []byte(customizationSentinel)) {
			t.Errorf("CLI request loaded local customization: %s", customizationSentinel)
		}
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode CLI request: %v: %s", err, body)
			return
		}
		toolResults := 0
		for _, message := range payload.Messages {
			var blocks []struct{ Type string }
			if json.Unmarshal(message.Content, &blocks) == nil {
				for _, block := range blocks {
					if block.Type == "tool_result" {
						toolResults++
					}
				}
			}
		}
		mu.Lock()
		resultCounts = append(resultCounts, toolResults)
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		switch toolResults {
		case 0:
			writeFakeAnthropicToolResponse(writer, payload.Model, []fakeToolCall{
				{id: "toolu_user_1", name: "mcp__model_tools__lookup_user", arguments: `{"id":"u1"}`},
				{id: "toolu_user_2", name: "mcp__model_tools__lookup_user", arguments: `{"id":"u2"}`},
				{id: "toolu_order_1", name: "mcp__model_tools__lookup_order", arguments: `{"id":"o1"}`},
			})
		case 3:
			writeFakeAnthropicToolResponse(writer, payload.Model, []fakeToolCall{
				{id: "toolu_finalize_1", name: "mcp__model_tools__finalize", arguments: `{"value":"ready"}`},
			})
		case 4:
			writeFakeAnthropicTextResponse(writer, payload.Model, "RESTART_RECONSTRUCTION_OK")
		default:
			t.Errorf("unexpected reconstructed tool-result count %d", toolResults)
			writeFakeAnthropicTextResponse(writer, payload.Model, "UNEXPECTED_HISTORY")
		}
	}))
	defer server.Close()
	t.Setenv("ANTHROPIC_API_KEY", "fixture-key")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")

	client := New("sonnet", Options{CLIPath: cliPath})
	client.homeDirectory = func() (string, error) { return authenticatedHome, nil }
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	system := damessage.System("Follow the supplied tool protocol.")
	tools := liveRestartTools()
	messages := []damessage.Message{damessage.Human("Begin.")}

	first, err := client.Invoke(ctx, damodel.Request{SystemMessage: &system, Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	assertInitialLiveCalls(t, first.Message.ToolCalls)
	messages = append(messages, first.Message)
	for _, call := range first.Message.ToolCalls {
		result := damessage.Tool(call.ID, "user fixture")
		if call.Name == "lookup_order" {
			result.Content[0].Text = "intentional order failure"
			result.ToolStatus = damessage.ToolStatusError
		}
		messages = append(messages, result)
	}
	client.stopProcess()

	second, err := client.Invoke(ctx, damodel.Request{SystemMessage: &system, Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Message.ToolCalls) != 1 || second.Message.ToolCalls[0].Name != "finalize" || canonicalJSON(second.Message.ToolCalls[0].Arguments) != `{"value":"ready"}` {
		t.Fatalf("post-restart calls = %#v", second.Message.ToolCalls)
	}
	messages = append(messages, second.Message, damessage.Tool(second.Message.ToolCalls[0].ID, "finalized"))
	client.stopProcess()

	third, err := client.Invoke(ctx, damodel.Request{SystemMessage: &system, Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Message.ToolCalls) != 0 || third.Message.TextContent() != "RESTART_RECONSTRUCTION_OK" {
		t.Fatalf("final response = %#v", third.Message)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(resultCounts) != "[0 3 4]" {
		t.Fatalf("reconstructed result counts = %v", resultCounts)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("local hook ran despite disabled setting sources: %v", err)
	}
	projectDirectory := client.process.projectDirectory
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(projectDirectory); !os.IsNotExist(err) {
		t.Fatalf("installed CLI project directory survived close: %v", err)
	}
}

func TestInstalledCLILoadsSessionSkillAndPreservesLongToolManual(t *testing.T) {
	if os.Getenv("DAGO_CLAUDE_AGENT_CLI_INTEGRATION") != "1" {
		t.Skip("set DAGO_CLAUDE_AGENT_CLI_INTEGRATION=1 to exercise the installed Claude CLI")
	}
	cliPath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	authenticatedHome := filepath.Join(t.TempDir(), "home")
	if err := initializeHome(authenticatedHome, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	const skillSentinel = "SESSION_SKILL_FULL_INSTRUCTIONS"
	longManual := "Workflow manual:\n" + skillSentinel + "\n" + strings.Repeat("Detailed workflow instruction. ", 120)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			http.NotFound(writer, request)
			return
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Error(readErr)
			return
		}
		var payload struct {
			Model string `json:"model"`
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode CLI request: %v: %s", err, body)
			return
		}
		requestNumber := requests.Add(1)
		if requestNumber == 1 {
			for _, tool := range payload.Tools {
				if strings.Contains(tool.Name, "workflow") {
					if len(tool.Description) > maximumInlineToolDescription || strings.Contains(tool.Description, skillSentinel) || !strings.Contains(tool.Description, "Skill") {
						t.Errorf("forwarded workflow description = %q", tool.Description)
					}
				}
			}
			writeFakeAnthropicToolResponse(writer, payload.Model, []fakeToolCall{{
				id: "toolu_skill_1", name: "Skill", arguments: `{"skill":"dago-session:` + toolManualSkillName("workflow") + `"}`,
			}})
			return
		}
		if !bytes.Contains(body, []byte(skillSentinel)) {
			t.Errorf("skill result did not contain the complete instructions")
		}
		writeFakeAnthropicTextResponse(writer, payload.Model, "SESSION_SKILL_OK")
	}))
	defer server.Close()
	t.Setenv("ANTHROPIC_API_KEY", "fixture-key")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")

	client := New("sonnet", Options{CLIPath: cliPath})
	client.homeDirectory = func() (string, error) { return authenticatedHome, nil }
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	response, err := client.Invoke(ctx, damodel.Request{
		Messages: []damessage.Message{damessage.Human("Use the workflow manual.")},
		Tools:    []datool.Definition{{Name: "workflow", Description: longManual, InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "SESSION_SKILL_OK" || len(response.Message.ToolCalls) != 0 || requests.Load() != 2 {
		t.Fatalf("response = %#v, requests = %d", response.Message, requests.Load())
	}
}

type fakeToolCall struct {
	id, name, arguments string
}

func writeFakeAnthropicToolResponse(writer io.Writer, model string, calls []fakeToolCall) {
	writeSSE(writer, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + calls[0].id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0},
	}})
	for index, call := range calls {
		writeSSE(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{
			"type": "tool_use", "id": call.id, "name": call.name, "input": map[string]any{},
		}})
		writeSSE(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{
			"type": "input_json_delta", "partial_json": call.arguments,
		}})
		writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	}
	writeSSE(writer, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": len(calls) * 10}})
	writeSSE(writer, "message_stop", map[string]any{"type": "message_stop"})
}

func writeFakeAnthropicTextResponse(writer io.Writer, model, text string) {
	writeSSE(writer, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_fixture_text", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0},
	}})
	writeSSE(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	writeSSE(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	writeSSE(writer, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 8}})
	writeSSE(writer, "message_stop", map[string]any{"type": "message_stop"})
}

func writeSSE(writer io.Writer, event string, value any) {
	encoded, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded)
}

func TestLiveRestartReconstructsVariedToolHistory(t *testing.T) {
	if os.Getenv("DAGO_CLAUDE_AGENT_LIVE") != "1" {
		t.Skip("set DAGO_CLAUDE_AGENT_LIVE=1 to call the installed Claude CLI")
	}
	cliPath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("DAGO_CLAUDE_AGENT_LIVE_MODEL")
	if model == "" {
		model = "sonnet"
	}
	client := New(model, Options{CLIPath: cliPath})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	system := damessage.System(`This is a deterministic tool-protocol integration test. Follow this state machine exactly:
1. Before any tool results exist, emit exactly three tool calls in one assistant turn and no text: lookup_user with {"id":"u1"}, lookup_user with {"id":"u2"}, and lookup_order with {"id":"o1"}.
2. After results for those three calls exist, emit exactly one finalize call with {"value":"ready"} and no text. Continue even though the order result is an error.
3. After the finalize result exists, emit only the exact text RESTART_RECONSTRUCTION_OK and make no tool calls.`)
	tools := liveRestartTools()
	messages := []damessage.Message{damessage.Human("Run the tool-protocol state machine now.")}

	first, err := client.Invoke(ctx, damodel.Request{SystemMessage: &system, Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.ToolCalls) == 0 {
		t.Fatalf("initial response contained no calls: %#v", first.Message)
	}
	assertInitialLiveCalls(t, first.Message.ToolCalls)
	messages = append(messages, first.Message)
	for _, call := range first.Message.ToolCalls {
		result := damessage.Tool(call.ID, "user fixture")
		if call.Name == "lookup_order" {
			result.Content[0].Text = "intentional order failure"
			result.ToolStatus = damessage.ToolStatusError
		}
		messages = append(messages, result)
	}

	client.stopProcess()
	second, err := client.Invoke(ctx, damodel.Request{SystemMessage: &system, Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Message.ToolCalls) != 1 || second.Message.ToolCalls[0].Name != "finalize" || canonicalJSON(second.Message.ToolCalls[0].Arguments) != `{"value":"ready"}` {
		t.Fatalf("post-restart calls = %#v", second.Message.ToolCalls)
	}
	messages = append(messages, second.Message, damessage.Tool(second.Message.ToolCalls[0].ID, "finalized"))

	client.stopProcess()
	third, err := client.Invoke(ctx, damodel.Request{SystemMessage: &system, Messages: messages, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Message.ToolCalls) != 0 || third.Message.TextContent() != "RESTART_RECONSTRUCTION_OK" {
		t.Fatalf("final response = %#v", third.Message)
	}
	projectDirectory := client.process.projectDirectory
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(projectDirectory); !os.IsNotExist(err) {
		t.Fatalf("live CLI project directory survived close: %v", err)
	}
}

func TestLiveStreamsTextBeforeCompletion(t *testing.T) {
	if os.Getenv("DAGO_CLAUDE_AGENT_LIVE") != "1" {
		t.Skip("set DAGO_CLAUDE_AGENT_LIVE=1 to call the installed Claude CLI")
	}
	cliPath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("DAGO_CLAUDE_AGENT_LIVE_MODEL")
	if model == "" {
		model = "sonnet"
	}
	client := New(model, Options{CLIPath: cliPath})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	stream, err := client.Stream(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human(
		"Write one sentence of at least twenty words about incremental streaming. Return only that sentence.",
	)}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var text string
	sawTextBeforeDone := false
	sawDone := false
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if value := chunk.MessageDelta.TextContent(); value != "" {
			text += value
			if !chunk.Done {
				sawTextBeforeDone = true
			}
		}
		if chunk.Done {
			sawDone = true
		}
	}
	if !sawTextBeforeDone || !sawDone || len(strings.Fields(text)) < 20 {
		t.Fatalf("streamed text = %q, text before done = %v, done = %v", text, sawTextBeforeDone, sawDone)
	}
}

func objectWithRequiredString(name string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"type": "object", "properties": map[string]any{name: map[string]any{"type": "string"}},
		"required": []string{name}, "additionalProperties": false,
	})
	return value
}

func liveRestartTools() []datool.Definition {
	return []datool.Definition{
		{Name: "lookup_user", Description: "Return one test user.", InputSchema: objectWithRequiredString("id")},
		{Name: "lookup_order", Description: "Return one test order or an intentional error.", InputSchema: objectWithRequiredString("id")},
		{Name: "finalize", Description: "Finalize the protocol test after lookup results.", InputSchema: objectWithRequiredString("value")},
	}
}

func assertInitialLiveCalls(t *testing.T, calls []damessage.ToolCall) {
	t.Helper()
	if len(calls) != 3 {
		t.Fatalf("initial calls = %#v", calls)
	}
	want := map[string]map[string]bool{
		"lookup_user":  {`{"id":"u1"}`: false, `{"id":"u2"}`: false},
		"lookup_order": {`{"id":"o1"}`: false},
	}
	for _, call := range calls {
		arguments := canonicalJSON(call.Arguments)
		seen, ok := want[call.Name][arguments]
		if !ok || seen {
			t.Fatalf("unexpected or duplicate call = %#v", call)
		}
		want[call.Name][arguments] = true
	}
	for name, arguments := range want {
		for value, seen := range arguments {
			if !seen {
				t.Fatalf("missing %s(%s)", name, value)
			}
		}
	}
}
