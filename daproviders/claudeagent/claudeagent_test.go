package claudeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

func TestInvokeUsesIsolatedBidirectionalProtocol(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "snapshot")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	system := damessage.System("Answer compactly.")
	response, err := client.Invoke(t.Context(), damodel.Request{
		SystemMessage: &system,
		Messages:      []damessage.Message{damessage.Human("hello")},
		Reasoning:     &damodel.Reasoning{Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Args  []string       `json:"args"`
		Frame map[string]any `json:"frame"`
		CWD   string         `json:"cwd"`
		Home  string         `json:"home"`
	}
	if err := json.Unmarshal([]byte(response.Message.TextContent()), &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{"--input-format", "stream-json"}, {"--output-format", "stream-json"},
		{"--tools", ""}, {"--settings", "{}"}, {"--system-prompt", "Answer compactly."},
		{"--model", "sonnet"}, {"--permission-mode", "dontAsk"}, {"--effort", "high"},
	} {
		if !hasArgumentPair(snapshot.Args, pair[0], pair[1]) {
			t.Errorf("arguments missing %q %q: %q", pair[0], pair[1], snapshot.Args)
		}
	}
	for _, flag := range []string{"-p", "--verbose", "--include-partial-messages", "--no-session-persistence", "--no-chrome", "--disable-slash-commands", "--strict-mcp-config", "--setting-sources="} {
		if !slices.Contains(snapshot.Args, flag) {
			t.Errorf("arguments missing %q: %q", flag, snapshot.Args)
		}
	}
	if slices.Contains(snapshot.Args, "--safe-mode") || slices.Contains(snapshot.Args, "--bare") {
		t.Fatalf("arguments disable the explicit MCP boundary or local subscription authentication: %q", snapshot.Args)
	}
	message := snapshot.Frame["message"].(map[string]any)
	if snapshot.Frame["type"] != "user" || snapshot.Frame["session_id"] == "" || message["role"] != "user" || message["content"] != "hello" {
		t.Fatalf("streaming frame = %#v", snapshot.Frame)
	}
	if snapshot.CWD == "" || snapshot.CWD == mustWorkingDirectory(t) || !strings.Contains(snapshot.CWD, "dago-claude-agent-") {
		t.Fatalf("CLI working directory = %q", snapshot.CWD)
	}
	expectedHome, err := client.homeDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Home != expectedHome || strings.HasPrefix(snapshot.CWD, snapshot.Home) {
		t.Fatalf("authenticated home = %q, cwd = %q", snapshot.Home, snapshot.CWD)
	}
	if response.Message.Usage == nil || response.Message.Usage.InputTokens != 6 || response.Message.Usage.OutputTokens != 2 || response.Message.Usage.Provider != providerName {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
	reason, _ := damodel.Outcome(response.Message)
	if reason != damodel.FinishReasonStop {
		t.Fatalf("finish reason = %q", reason)
	}
}

func TestProfileAdvertisesNativeStreaming(t *testing.T) {
	if profile := New("sonnet").Profile(); !profile.NativeStreaming {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestStreamEmitsTextBeforeCLICompletes(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "streaming")
	t.Setenv("DAGO_CLAUDE_AGENT_STREAM_RELEASE", release)
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })

	started := time.Now()
	stream, err := client.Stream(t.Context(), damodel.Request{Messages: []damessage.Message{damessage.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Stream waited for CLI completion: %v", elapsed)
	}

	first, err := stream.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first.Done || first.MessageDelta.TextContent() != "hel" {
		t.Fatalf("first chunk = %#v", first)
	}
	if err := os.WriteFile(release, []byte("continue"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := stream.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Done || second.MessageDelta.TextContent() != "lo" {
		t.Fatalf("second chunk = %#v", second)
	}
	terminal, err := stream.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !terminal.Done || terminal.MessageDelta.TextContent() != "" || terminal.MessageDelta.Usage == nil || terminal.MessageDelta.Usage.OutputTokens != 2 {
		t.Fatalf("terminal chunk = %#v", terminal)
	}
	if _, err := stream.Next(t.Context()); err != io.EOF {
		t.Fatalf("stream end error = %v", err)
	}
}

func TestRestartRebuildsNativeTranscriptAndResumes(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "snapshot")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	first, err := client.Invoke(t.Context(), damodel.Request{Messages: []damessage.Message{damessage.Human("first")}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Invoke(t.Context(), damodel.Request{Messages: []damessage.Message{
		damessage.Human("first"), first.Message, damessage.Human("second"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Args       []string         `json:"args"`
		Frame      map[string]any   `json:"frame"`
		Transcript []map[string]any `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(second.Message.TextContent()), &snapshot); err != nil {
		t.Fatal(err)
	}
	resumeID := argumentValue(snapshot.Args, "--resume")
	if resumeID == "" || slices.Contains(snapshot.Args, "--session-id") {
		t.Fatalf("restart arguments = %q", snapshot.Args)
	}
	if snapshot.Frame["session_id"] != resumeID {
		t.Fatalf("frame session = %#v, resume = %q", snapshot.Frame["session_id"], resumeID)
	}
	if len(snapshot.Transcript) != 2 || snapshot.Transcript[0]["type"] != "user" || snapshot.Transcript[1]["type"] != "assistant" {
		t.Fatalf("transcript = %#v", snapshot.Transcript)
	}
	home, err := client.homeDirectory()
	if err != nil {
		t.Fatal(err)
	}
	projectDirectory := client.process.projectDirectory
	if !strings.HasPrefix(projectDirectory, filepath.Join(home, ".claude", "projects")+string(filepath.Separator)) {
		t.Fatalf("resume transcript directory escaped authenticated home: %q", projectDirectory)
	}
	if err := os.MkdirAll(projectDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDirectory, "cleanup-sentinel"), []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(projectDirectory); !os.IsNotExist(err) {
		t.Fatalf("resume transcript directory survived close: %v", err)
	}
}

func TestRestartTranscriptIncludesCompletedToolResults(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := initializeHome(home, workspace); err != nil {
		t.Fatal(err)
	}
	bridge, err := newToolBridge([]datool.Definition{{Name: "lookup", Description: "Look up.", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	assistant := damessage.Message{ID: "msg_tools", Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "tool_1", Name: "lookup", Arguments: json.RawMessage(`{"id":"1"}`)}}}
	failed := damessage.Tool("tool_1", "failed")
	failed.ToolStatus = damessage.ToolStatusError
	current, replaying, err := writeTranscript(home, workspace, "11111111-1111-4111-8111-111111111111", "sonnet", []damessage.Message{
		damessage.Human("start"), assistant, failed,
	}, bridge)
	if err != nil {
		t.Fatal(err)
	}
	if !replaying || current != "Continue from the completed tool results without repeating any tool call whose result is already present." {
		t.Fatalf("current = %#v, replaying = %v", current, replaying)
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("transcripts = %q", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"type":"tool_result"`) || !strings.Contains(lines[2], `"is_error":true`) {
		t.Fatalf("transcript = %s", data)
	}
}

func TestOrdinaryTurnsReuseOneBidirectionalProcess(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "persistent")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	first, err := client.Invoke(t.Context(), damodel.Request{Messages: []damessage.Message{damessage.Human("first")}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Invoke(t.Context(), damodel.Request{Messages: []damessage.Message{
		damessage.Human("first"), first.Message, damessage.Human("second"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var firstValue, secondValue struct {
		PID       int      `json:"pid"`
		Turn      int      `json:"turn"`
		Args      []string `json:"args"`
		SessionID string   `json:"session_id"`
	}
	if json.Unmarshal([]byte(first.Message.TextContent()), &firstValue) != nil || json.Unmarshal([]byte(second.Message.TextContent()), &secondValue) != nil {
		t.Fatal("decode helper responses")
	}
	if firstValue.PID != secondValue.PID || firstValue.Turn != 1 || secondValue.Turn != 2 || firstValue.SessionID != secondValue.SessionID {
		t.Fatalf("first = %#v, second = %#v", firstValue, secondValue)
	}
	if argumentValue(firstValue.Args, "--session-id") == "" || argumentValue(firstValue.Args, "--resume") != "" {
		t.Fatalf("initial arguments = %q", firstValue.Args)
	}
}

func TestInvokeReturnsMCPRequestsForCallerExecution(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "tools")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	response, err := client.Invoke(t.Context(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("look it up")},
		Tools: []datool.Definition{
			{Name: "lookup", Description: "Look up a record.", InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)},
			{Name: "namespace.tool", Description: "Use a namespaced tool.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls = %#v", response.Message.ToolCalls)
	}
	if response.Message.ToolCalls[0].Name != "lookup" || string(response.Message.ToolCalls[0].Arguments) != `{"id":"42"}` {
		t.Fatalf("first tool call = %#v", response.Message.ToolCalls[0])
	}
	if response.Message.ToolCalls[1].Name != "namespace.tool" {
		t.Fatalf("sanitized tool mapping = %#v", response.Message.ToolCalls[1])
	}
	reason, _ := damodel.Outcome(response.Message)
	if reason != damodel.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", reason)
	}
}

func TestStreamEmitsToolCallsInTerminalChunk(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "tools")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	stream, err := client.Stream(t.Context(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("look it up")},
		Tools: []datool.Definition{
			{Name: "lookup", Description: "Look up a record.", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "namespace.tool", Description: "Use a namespaced tool.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunk, err := stream.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !chunk.Done || len(chunk.MessageDelta.ToolCalls) != 2 {
		t.Fatalf("tool chunk = %#v", chunk)
	}
	if reason, _ := damodel.Outcome(chunk.MessageDelta); reason != damodel.FinishReasonToolCalls {
		t.Fatalf("finish reason = %q", reason)
	}
	if _, err := stream.Next(t.Context()); err != io.EOF {
		t.Fatalf("stream end error = %v", err)
	}
}

func TestToolResultsContinueTheSameProcessThroughMCP(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "mcp_continuation")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	tools := []datool.Definition{{Name: "lookup", Description: "Look up a record.", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	first, err := client.Invoke(t.Context(), damodel.Request{Messages: []damessage.Message{damessage.Human("look it up")}, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", first.Message.ToolCalls)
	}
	result := damessage.Tool(first.Message.ToolCalls[0].ID, "record 42")
	second, err := client.Invoke(t.Context(), damodel.Request{
		Messages: []damessage.Message{damessage.Human("look it up"), first.Message, result}, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Message.TextContent() != "MCP returned: record 42" {
		t.Fatalf("continued response = %#v", second.Message)
	}
}

func TestToolBridgePublishesExactRequestSchemasAndRejectsUnauthenticatedClients(t *testing.T) {
	bridge, err := newToolBridge([]datool.Definition{{
		Name: "lookup", Description: "Look up a record.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	if response, err := http.Get(bridge.url); err != nil {
		t.Fatal(err)
	} else {
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unauthenticated status = %d", response.StatusCode)
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: authorizationTransport{token: bridge.token}}
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: bridge.url, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "lookup" || result.Tools[0].Description != "Look up a record." {
		t.Fatalf("tools = %#v", result.Tools)
	}
	encoded, err := json.Marshal(result.Tools[0].InputSchema)
	if err != nil || !strings.Contains(string(encoded), `"required":["id"]`) {
		t.Fatalf("schema = %s, %v", encoded, err)
	}
}

func TestInvokeCancellationStopsCLI(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "block")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Invoke(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human("wait")}})
	if !errorsIsDeadline(err) {
		t.Fatalf("error = %v", err)
	}
}

func TestStreamCloseStopsCLIAndReleasesNextTurn(t *testing.T) {
	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "block")
	client := helperClient(t)
	t.Cleanup(func() { _ = client.Close() })
	stream, err := client.Stream(t.Context(), damodel.Request{Messages: []damessage.Message{damessage.Human("wait")}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("Close did not promptly stop the producer: %v", elapsed)
	}

	t.Setenv("DAGO_CLAUDE_AGENT_HELPER", "snapshot")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := client.Invoke(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human("next")}}); err != nil {
		t.Fatalf("next turn after Close: %v", err)
	}
}

type authorizationTransport struct{ token string }

func (transport authorizationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header.Set("Authorization", "Bearer "+transport.token)
	return http.DefaultTransport.RoundTrip(copy)
}

func helperClient(t *testing.T) *Client {
	t.Helper()
	client := New("sonnet", Options{CLIPath: os.Args[0]})
	home := filepath.Join(t.TempDir(), "authenticated-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	client.homeDirectory = func() (string, error) { return home, nil }
	client.command = func(ctx context.Context, _ string, arguments ...string) *exec.Cmd {
		values := append([]string{"-test.run=TestClaudeAgentHelperProcess", "--"}, arguments...)
		return exec.CommandContext(ctx, os.Args[0], values...)
	}
	return client
}

func TestClaudeAgentHelperProcess(t *testing.T) {
	mode := os.Getenv("DAGO_CLAUDE_AGENT_HELPER")
	if mode == "" {
		return
	}
	decoder := json.NewDecoder(os.Stdin)
	var frame map[string]any
	if err := decoder.Decode(&frame); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	separator := slices.Index(os.Args, "--")
	arguments := append([]string(nil), os.Args[separator+1:]...)
	switch mode {
	case "snapshot":
		cwd, _ := os.Getwd()
		messageID := "msg_1"
		var transcript []map[string]any
		if sessionID := argumentValue(arguments, "--resume"); sessionID != "" {
			messageID = "msg_2"
			matches, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".claude", "projects", "*", sessionID+".jsonl"))
			if len(matches) == 1 {
				data, _ := os.ReadFile(matches[0])
				for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
					var entry map[string]any
					if json.Unmarshal([]byte(line), &entry) == nil {
						transcript = append(transcript, entry)
					}
				}
			}
		}
		text, _ := json.Marshal(map[string]any{"args": arguments, "frame": frame, "cwd": cwd, "home": os.Getenv("HOME"), "transcript": transcript})
		writeHelperJSON(map[string]any{"type": "assistant", "message": map[string]any{
			"id": messageID, "model": "claude-sonnet", "content": []any{map[string]any{"type": "text", "text": string(text)}},
			"usage": map[string]any{"input_tokens": 3, "cache_read_input_tokens": 3, "output_tokens": 2},
		}})
		writeHelperJSON(map[string]any{"type": "result", "subtype": "success", "usage": map[string]any{"input_tokens": 3, "cache_read_input_tokens": 3, "output_tokens": 2}})
	case "streaming":
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "message_start", "message": map[string]any{"id": "msg_stream", "model": "claude-sonnet", "usage": map[string]any{"input_tokens": 3, "output_tokens": 0}},
		}})
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""},
		}})
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "hel"},
		}})
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv("DAGO_CLAUDE_AGENT_STREAM_RELEASE")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "lo"},
		}})
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_stop", "index": 0}})
		writeHelperJSON(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_stream", "model": "claude-sonnet", "content": []any{map[string]any{"type": "text", "text": "hello"}},
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 2},
		}})
		writeHelperJSON(map[string]any{"type": "result", "subtype": "success", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}})
	case "tools":
		config := argumentValue(arguments, "--mcp-config")
		var payload struct {
			MCPServers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(config), &payload); err != nil || payload.MCPServers[toolServerName].URL == "" {
			fmt.Fprintln(os.Stderr, "missing generated MCP server")
			os.Exit(3)
		}
		writeHelperJSON(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_tools", "model": "claude-sonnet", "content": []any{
				map[string]any{"type": "tool_use", "id": "tool_1", "name": "mcp__model_tools__lookup", "input": map[string]any{"id": "42"}},
				map[string]any{"type": "tool_use", "id": "tool_2", "name": "mcp__model_tools__tool_2", "input": map[string]any{}},
			}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 1},
		}})
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_stop"}})
	case "mcp_continuation":
		config := argumentValue(arguments, "--mcp-config")
		var payload struct {
			MCPServers map[string]struct {
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(config), &payload); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(5)
		}
		server := payload.MCPServers[toolServerName]
		token := strings.TrimPrefix(server.Headers["Authorization"], "Bearer ")
		mcpClient := mcp.NewClient(&mcp.Implementation{Name: "helper", Version: "1"}, nil)
		session, err := mcpClient.Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint: server.URL, HTTPClient: &http.Client{Transport: authorizationTransport{token: token}}, DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(6)
		}
		defer session.Close()
		writeHelperJSON(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_tools", "model": "claude-sonnet", "content": []any{
				map[string]any{"type": "tool_use", "id": "tool_1", "name": "mcp__model_tools__lookup", "input": map[string]any{"id": "42"}},
			},
		}})
		writeHelperJSON(map[string]any{"type": "stream_event", "event": map[string]any{"type": "message_stop"}})
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "lookup", Arguments: map[string]any{"id": "42"}})
		if err != nil || len(result.Content) != 1 {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(7)
		}
		textResult, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			os.Exit(8)
		}
		writeHelperJSON(map[string]any{"type": "assistant", "message": map[string]any{
			"id": "msg_answer", "model": "claude-sonnet", "content": []any{map[string]any{"type": "text", "text": "MCP returned: " + textResult.Text}},
		}})
		writeHelperJSON(map[string]any{"type": "result", "subtype": "success"})
	case "persistent":
		for turn := 1; ; turn++ {
			text, _ := json.Marshal(map[string]any{
				"pid": os.Getpid(), "turn": turn, "args": arguments, "session_id": frame["session_id"],
			})
			writeHelperJSON(map[string]any{"type": "assistant", "message": map[string]any{
				"id": fmt.Sprintf("msg_%d", turn), "model": "claude-sonnet", "content": []any{map[string]any{"type": "text", "text": string(text)}},
			}})
			writeHelperJSON(map[string]any{"type": "result", "subtype": "success"})
			if err := decoder.Decode(&frame); err != nil {
				return
			}
		}
	case "block":
		time.Sleep(time.Hour)
	default:
		os.Exit(4)
	}
	os.Exit(0)
}

func writeHelperJSON(value any) {
	_ = json.NewEncoder(os.Stdout).Encode(value)
}

func hasArgumentPair(arguments []string, name, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name && arguments[index+1] == value {
			return true
		}
	}
	return false
}

func argumentValue(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func mustWorkingDirectory(t *testing.T) string {
	t.Helper()
	value, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func errorsIsDeadline(err error) bool {
	return err == context.DeadlineExceeded || err != nil && strings.Contains(err.Error(), context.DeadlineExceeded.Error())
}
