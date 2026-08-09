package loop

import (
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
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/gitstate"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
)

func TestNewLoop(t *testing.T) {
	history := []llm.Message{
		{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "Hello"}}},
	}
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		return nil
	}

	loop := NewLoop(Config{
		Model:         NewPredictableService(),
		History:       history,
		RecordMessage: recordFunc,
	})
	if loop == nil {
		t.Fatal("NewLoop returned nil")
	}

	if len(loop.history) != 1 {
		t.Errorf("expected history length 1, got %d", len(loop.history))
	}

	if len(loop.messageQueue) != 0 {
		t.Errorf("expected empty message queue, got %d", len(loop.messageQueue))
	}
}

func TestQueueUserMessage(t *testing.T) {
	loop := NewLoop(Config{
		Model:   NewPredictableService(),
		History: []llm.Message{},
	})

	message := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "Test message"}},
	}

	loop.QueueUserMessage(message)

	loop.mu.Lock()
	queueLen := len(loop.messageQueue)
	loop.mu.Unlock()

	if queueLen != 1 {
		t.Errorf("expected message queue length 1, got %d", queueLen)
	}
}

func TestPredictableService(t *testing.T) {
	service := NewPredictableService()
	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("hello")}})
	if err != nil {
		t.Fatalf("predictable model Invoke failed: %v", err)
	}
	if resp.Message.Role != dmessage.RoleAssistant {
		t.Errorf("expected assistant role, got %v", resp.Message.Role)
	}
	if len(resp.Message.Content) == 0 {
		t.Error("expected non-empty content")
	}
	if resp.Message.Content[0].Type != dmessage.BlockText {
		t.Errorf("expected text content, got %v", resp.Message.Content[0].Type)
	}
	if resp.Message.TextContent() != "Well, hi there!" {
		t.Errorf("unexpected response text: %s", resp.Message.TextContent())
	}
}

func TestPredictableServiceEcho(t *testing.T) {
	service := NewPredictableService()

	ctx := context.Background()
	resp, err := service.Invoke(ctx, dmodel.Request{Messages: []dmessage.Message{dmessage.Human("echo: foo")}})
	if err != nil {
		t.Fatalf("echo test failed: %v", err)
	}

	if resp.Message.TextContent() != "foo" {
		t.Errorf("expected 'foo', got '%s'", resp.Message.TextContent())
	}

	resp, err = service.Invoke(ctx, dmodel.Request{Messages: []dmessage.Message{dmessage.Human("echo: hello world")}})
	if err != nil {
		t.Fatalf("echo hello world test failed: %v", err)
	}

	if resp.Message.TextContent() != "hello world" {
		t.Errorf("expected 'hello world', got '%s'", resp.Message.TextContent())
	}
}

func TestPredictableServiceBashTool(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("bash: ls -la")}})
	if err != nil {
		t.Fatalf("bash tool test failed: %v", err)
	}

	if len(resp.Message.ToolCalls) != 1 {
		t.Fatal("no tool use content found")
	}
	toolCall := resp.Message.ToolCalls[0]
	if toolCall.Name != "bash" {
		t.Errorf("expected tool name 'bash', got '%s'", toolCall.Name)
	}

	// Check tool input contains the command
	var toolInput map[string]interface{}
	if err := json.Unmarshal(toolCall.Arguments, &toolInput); err != nil {
		t.Fatalf("failed to parse tool input: %v", err)
	}

	if toolInput["command"] != "ls -la" {
		t.Errorf("expected command 'ls -la', got '%v'", toolInput["command"])
	}
}

func TestPredictableServiceDefaultResponse(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("some unknown input")}})
	if err != nil {
		t.Fatalf("default response test failed: %v", err)
	}

	if resp.Message.TextContent() != "edit predictable.go to add a response for that one..." {
		t.Errorf("unexpected default response: %s", resp.Message.TextContent())
	}
}

func TestPredictableServiceDelay(t *testing.T) {
	service := NewPredictableService()

	start := time.Now()
	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("delay: 0.1")}})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("delay test failed: %v", err)
	}

	if elapsed < 100*time.Millisecond {
		t.Errorf("expected delay of at least 100ms, got %v", elapsed)
	}

	if resp.Message.TextContent() != "Delayed for 0.1 seconds" {
		t.Errorf("unexpected response text: %s", resp.Message.TextContent())
	}
}

func TestLoopWithPredictableService(t *testing.T) {
	var recordedMessages []llm.Message
	var recordedUsages []llm.Usage

	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recordedMessages = append(recordedMessages, message)
		recordedUsages = append(recordedUsages, usage)
		return nil
	}

	service := NewPredictableService()
	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
	})

	// Queue a user message that triggers a known response
	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
	}
	loop.QueueUserMessage(userMessage)

	// Run the loop with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := loop.Go(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}

	// Check that messages were recorded
	if len(recordedMessages) < 1 {
		t.Errorf("expected at least 1 recorded message, got %d", len(recordedMessages))
	}

	// Check usage tracking
	usage := loop.GetUsage()
	if usage.IsZero() {
		t.Error("expected non-zero usage")
	}
}

func TestLoopWithTools(t *testing.T) {
	var toolCalls []string

	testTool := dtool.Func{
		Spec: dtool.Definition{Name: "bash", Description: "A test bash tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
		Run: func(ctx context.Context, input json.RawMessage, _ dtool.Runtime) (dtool.Result, error) {
			toolCalls = append(toolCalls, string(input))
			return dtool.TextResult("Command executed successfully"), nil
		},
	}

	service := NewPredictableService()
	loop := NewLoop(Config{
		Model:   service,
		History: []llm.Message{},
		Tools:   []dtool.Tool{testTool},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			return nil
		},
	})

	// Queue a user message that triggers the bash tool
	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "bash: echo hello"}},
	}
	loop.QueueUserMessage(userMessage)

	// Run the loop with a short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := loop.Go(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}

	// Check that the tool was called
	if len(toolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(toolCalls))
	}

	if toolCalls[0] != `{"command":"echo hello"}` {
		t.Errorf("unexpected tool call input: %s", toolCalls[0])
	}
}

func TestGetHistory(t *testing.T) {
	initialHistory := []llm.Message{
		{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "Hello"}}},
	}

	loop := NewLoop(Config{
		Model:   NewPredictableService(),
		History: initialHistory,
	})

	history := loop.GetHistory()
	if len(history) != 1 {
		t.Errorf("expected history length 1, got %d", len(history))
	}

	// Modify returned slice to ensure it's a copy
	history[0].Content[0].Text = "Modified"

	// Original should be unchanged
	original := loop.GetHistory()
	if original[0].Content[0].Text != "Hello" {
		t.Error("GetHistory should return a copy, not the original slice")
	}
}

func TestLoopWithKeywordTool(t *testing.T) {
	// Test that keyword tool doesn't crash with nil pointer dereference
	service := NewPredictableService()

	var messages []llm.Message
	recordMessage := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		messages = append(messages, message)
		return nil
	}

	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		RecordMessage: recordMessage,
	})

	// Send a user message that will trigger the default response
	userMessage := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{
			{Type: llm.ContentTypeText, Text: "Please search for some files"},
		},
	}

	loop.QueueUserMessage(userMessage)

	// Process one turn
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := loop.ProcessOneTurn(ctx)
	if err != nil {
		t.Fatalf("ProcessOneTurn failed: %v", err)
	}

	// Verify we got expected messages
	// Note: User messages are recorded by ConversationManager, not by Loop,
	// so we only expect the assistant response to be recorded here
	if len(messages) < 1 {
		t.Fatalf("Expected at least 1 message (assistant), got %d", len(messages))
	}

	// Should have assistant response
	if messages[0].Role != llm.MessageRoleAssistant {
		t.Errorf("Expected first recorded message to be assistant, got %s", messages[0].Role)
	}
}

func TestLoopWithActualKeywordTool(t *testing.T) {
	// Test that actual keyword tool works with Loop
	service := NewPredictableService()

	var messages []llm.Message
	recordMessage := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		messages = append(messages, message)
		return nil
	}

	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		RecordMessage: recordMessage,
	})

	// Send a user message that will trigger the default response
	userMessage := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{
			{Type: llm.ContentTypeText, Text: "Please search for some files"},
		},
	}

	loop.QueueUserMessage(userMessage)

	// Process one turn
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := loop.ProcessOneTurn(ctx)
	if err != nil {
		t.Fatalf("ProcessOneTurn failed: %v", err)
	}

	// Verify we got expected messages
	// Note: User messages are recorded by ConversationManager, not by Loop,
	// so we only expect the assistant response to be recorded here
	if len(messages) < 1 {
		t.Fatalf("Expected at least 1 message (assistant), got %d", len(messages))
	}

	// Should have assistant response
	if messages[0].Role != llm.MessageRoleAssistant {
		t.Errorf("Expected first recorded message to be assistant, got %s", messages[0].Role)
	}

	t.Log("Keyword tool test passed - no nil pointer dereference occurred")
}

func TestInsertMissingToolResults(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
		wantLen  int
		wantText string
	}{
		{
			name: "no missing tool results",
			messages: []llm.Message{
				{
					Role: llm.MessageRoleAssistant,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "Let me help you"},
					},
				},
				{
					Role: llm.MessageRoleUser,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "Thanks"},
					},
				},
			},
			wantLen:  1,
			wantText: "", // No synthetic result expected
		},
		{
			name: "missing tool result - should insert synthetic result",
			messages: []llm.Message{
				{
					Role: llm.MessageRoleAssistant,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "I'll use a tool"},
						{Type: llm.ContentTypeToolUse, ID: "tool_123", ToolName: "bash"},
					},
				},
				{
					Role: llm.MessageRoleUser,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "Error occurred"},
					},
				},
			},
			wantLen:  2, // Should have synthetic tool_result + error message
			wantText: "not executed; retry possible",
		},
		{
			name: "multiple missing tool results",
			messages: []llm.Message{
				{
					Role: llm.MessageRoleAssistant,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "I'll use multiple tools"},
						{Type: llm.ContentTypeToolUse, ID: "tool_1", ToolName: "bash"},
						{Type: llm.ContentTypeToolUse, ID: "tool_2", ToolName: "read"},
					},
				},
				{
					Role: llm.MessageRoleUser,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "Error occurred"},
					},
				},
			},
			wantLen: 3, // Should have 2 synthetic tool_results + error message
		},
		{
			name: "has tool results - should not insert",
			messages: []llm.Message{
				{
					Role: llm.MessageRoleAssistant,
					Content: []llm.Content{
						{Type: llm.ContentTypeText, Text: "I'll use a tool"},
						{Type: llm.ContentTypeToolUse, ID: "tool_123", ToolName: "bash"},
					},
				},
				{
					Role: llm.MessageRoleUser,
					Content: []llm.Content{
						{
							Type:       llm.ContentTypeToolResult,
							ToolUseID:  "tool_123",
							ToolResult: []llm.Content{{Type: llm.ContentTypeText, Text: "result"}},
						},
					},
				},
			},
			wantLen: 1, // Should not insert anything
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop := NewLoop(Config{
				Model:   NewPredictableService(),
				History: []llm.Message{},
			})

			messages := loop.repairMessageHistory(tt.messages)

			got := messages[len(messages)-1]
			if len(got.Content) != tt.wantLen {
				t.Errorf("expected %d content items, got %d", tt.wantLen, len(got.Content))
			}

			if tt.wantText != "" {
				// Find the synthetic tool result
				found := false
				for _, c := range got.Content {
					if c.Type == llm.ContentTypeToolResult && len(c.ToolResult) > 0 {
						if c.ToolResult[0].Text == tt.wantText {
							found = true
							if !c.ToolError {
								t.Error("synthetic tool result should have ToolError=true")
							}
							break
						}
					}
				}
				if !found {
					t.Errorf("expected to find synthetic tool result with text %q", tt.wantText)
				}
			}
		})
	}
}

func TestInsertMissingToolResultsWithEdgeCases(t *testing.T) {
	// Test for the bug: when an assistant error message is recorded after a tool_use
	// but before tool execution, the tool_use is "hidden" from history repair
	// because it only checks the last two messages.
	t.Run("tool_use hidden by subsequent assistant message", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		// Scenario:
		// 1. LLM responds with tool_use
		// 2. Something fails, error message recorded (assistant message)
		// 3. User sends new message
		// The tool_use in message 0 is never followed by a tool_result
		messages := []llm.Message{
			{
				Role: llm.MessageRoleAssistant,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "I'll run a command"},
					{Type: llm.ContentTypeToolUse, ID: "tool_hidden", ToolName: "bash"},
				},
			},
			{
				Role: llm.MessageRoleAssistant,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "LLM request failed: some error"},
				},
			},
			{
				Role: llm.MessageRoleUser,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "Please try again"},
				},
			},
		}

		messages = loop.repairMessageHistory(messages)

		// The function should have inserted a tool_result for tool_hidden
		// It should be inserted as a user message after the assistant message with tool_use
		// Since we can't insert in the middle, we need to ensure the history is valid

		// Check that there's a tool_result for tool_hidden somewhere in the messages
		found := false
		for _, msg := range messages {
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolResult && c.ToolUseID == "tool_hidden" {
					found = true
					if !c.ToolError {
						t.Error("synthetic tool result should have ToolError=true")
					}
					break
				}
			}
		}
		if !found {
			t.Error("expected to find synthetic tool result for tool_hidden - the bug is that tool_use is hidden by subsequent assistant message")
		}
	})

	// Test for tool_use in earlier message (not the second-to-last)
	t.Run("tool_use in earlier message without result", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		messages := []llm.Message{
			{
				Role: llm.MessageRoleUser,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "Do something"},
				},
			},
			{
				Role: llm.MessageRoleAssistant,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "I'll use a tool"},
					{Type: llm.ContentTypeToolUse, ID: "tool_earlier", ToolName: "bash"},
				},
			},
			// Missing: user message with tool_result for tool_earlier
			{
				Role: llm.MessageRoleAssistant,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "Something went wrong"},
				},
			},
			{
				Role: llm.MessageRoleUser,
				Content: []llm.Content{
					{Type: llm.ContentTypeText, Text: "Try again"},
				},
			},
		}

		messages = loop.repairMessageHistory(messages)

		// Should have inserted a tool_result for tool_earlier
		found := false
		for _, msg := range messages {
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolResult && c.ToolUseID == "tool_earlier" {
					found = true
					break
				}
			}
		}
		if !found {
			t.Error("expected to find synthetic tool result for tool_earlier")
		}
	})

	t.Run("empty message list", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		loop.repairMessageHistory(nil)
		// Should not panic
	})

	t.Run("single message", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		messages := []llm.Message{
			{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}}},
		}

		messages = loop.repairMessageHistory(messages)
		// Should not panic, should not modify
		if len(messages[0].Content) != 1 {
			t.Error("should not modify single message")
		}
	})

	t.Run("wrong role order - user then assistant", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		messages := []llm.Message{
			{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}}},
			{Role: llm.MessageRoleAssistant, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hi"}}},
		}

		messages = loop.repairMessageHistory(messages)
		// Should not modify when roles are wrong order
		if len(messages[1].Content) != 1 {
			t.Error("should not modify when roles are in wrong order")
		}
	})
}

func TestInsertMissingToolResults_EmptyAssistantContent(t *testing.T) {
	// Test for the bug: when an assistant message has empty content (can happen when
	// the model ends its turn without producing any output), we need to add placeholder
	// content if it's not the last message. Otherwise the API will reject with:
	// "messages.N: all messages must have non-empty content except for the optional
	// final assistant message"

	t.Run("empty assistant content in middle of conversation", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		messages := []llm.Message{
			{
				Role:    llm.MessageRoleUser,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: "run git fetch"}},
			},
			{
				Role:    llm.MessageRoleAssistant,
				Content: []llm.Content{{Type: llm.ContentTypeToolUse, ID: "tool1", ToolName: "bash"}},
			},
			{
				Role: llm.MessageRoleUser,
				Content: []llm.Content{{
					Type:       llm.ContentTypeToolResult,
					ToolUseID:  "tool1",
					ToolResult: []llm.Content{{Type: llm.ContentTypeText, Text: "success"}},
				}},
			},
			{
				// Empty assistant message - this can happen when model ends turn without output
				Role:      llm.MessageRoleAssistant,
				Content:   []llm.Content{},
				EndOfTurn: true,
			},
			{
				Role:    llm.MessageRoleUser,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: "next question"}},
			},
		}

		messages = loop.repairMessageHistory(messages)

		// The empty assistant message (index 3) should now have placeholder content
		if len(messages[3].Content) == 0 {
			t.Error("expected placeholder content to be added to empty assistant message")
		}
		if messages[3].Content[0].Type != llm.ContentTypeText {
			t.Error("expected placeholder to be text content")
		}
		if messages[3].Content[0].Text != "(no response)" {
			t.Errorf("expected placeholder text '(no response)', got %q", messages[3].Content[0].Text)
		}
	})

	t.Run("empty assistant content at end of conversation - no modification needed", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		messages := []llm.Message{
			{
				Role:    llm.MessageRoleUser,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
			},
			{
				// Empty assistant message at end is allowed by the API
				Role:      llm.MessageRoleAssistant,
				Content:   []llm.Content{},
				EndOfTurn: true,
			},
		}

		messages = loop.repairMessageHistory(messages)

		// The empty assistant message at the end should NOT be modified
		// because the API allows empty content for the final assistant message
		if len(messages[1].Content) != 0 {
			t.Error("expected final empty assistant message to remain empty")
		}
	})

	t.Run("non-empty assistant content - no modification needed", func(t *testing.T) {
		loop := NewLoop(Config{
			Model:   NewPredictableService(),
			History: []llm.Message{},
		})

		messages := []llm.Message{
			{
				Role:    llm.MessageRoleUser,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
			},
			{
				Role:    llm.MessageRoleAssistant,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hi there"}},
			},
			{
				Role:    llm.MessageRoleUser,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: "goodbye"}},
			},
		}

		messages = loop.repairMessageHistory(messages)

		// The assistant message should not be modified
		if len(messages[1].Content) != 1 {
			t.Errorf("expected assistant message to have 1 content item, got %d", len(messages[1].Content))
		}
		if messages[1].Content[0].Text != "hi there" {
			t.Errorf("expected assistant message text 'hi there', got %q", messages[1].Content[0].Text)
		}
	})
}

func TestGitStateTracking(t *testing.T) {
	// Create a test repo
	tmpDir := t.TempDir()

	// Initialize git repo
	runGit(t, tmpDir, "init")
	runGit(t, tmpDir, "config", "user.email", "test@test.com")
	runGit(t, tmpDir, "config", "user.name", "Test")

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmpDir, "add", ".")
	runGit(t, tmpDir, "commit", "-m", "initial")

	// Track git state changes
	var mu sync.Mutex
	var gitStateChanges []*gitstate.GitState

	loop := NewLoop(Config{
		Model:         NewPredictableService(),
		History:       []llm.Message{},
		WorkingDir:    tmpDir,
		GetWorkingDir: func() string { return tmpDir },
		OnGitStateChange: func(ctx context.Context, state *gitstate.GitState) {
			mu.Lock()
			gitStateChanges = append(gitStateChanges, state)
			mu.Unlock()
		},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			return nil
		},
	})

	// Verify initial state was captured
	if loop.lastGitState == nil {
		t.Fatal("expected initial git state to be captured")
	}
	if !loop.lastGitState.IsRepo {
		t.Error("expected IsRepo to be true")
	}

	// Process a turn (no state change should occur)
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := loop.ProcessOneTurn(ctx)
	if err != nil {
		t.Fatalf("ProcessOneTurn failed: %v", err)
	}

	// No state change should have occurred
	mu.Lock()
	numChanges := len(gitStateChanges)
	mu.Unlock()
	if numChanges != 0 {
		t.Errorf("expected no git state changes, got %d", numChanges)
	}

	// Now make a commit
	if err := os.WriteFile(testFile, []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmpDir, "add", ".")
	runGit(t, tmpDir, "commit", "-m", "update")

	// Process another turn - this should detect the commit change
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello again"}},
	})

	err = loop.ProcessOneTurn(ctx)
	if err != nil {
		t.Fatalf("ProcessOneTurn failed: %v", err)
	}

	// Now a state change should have been detected
	mu.Lock()
	numChanges = len(gitStateChanges)
	mu.Unlock()
	if numChanges != 1 {
		t.Errorf("expected 1 git state change, got %d", numChanges)
	}
}

func TestGitStateTrackingWorktree(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mainRepo := filepath.Join(tmpDir, "main")
	worktreeDir := filepath.Join(tmpDir, "worktree")

	// Create main repo
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainRepo, "init")
	runGit(t, mainRepo, "config", "user.email", "test@test.com")
	runGit(t, mainRepo, "config", "user.name", "Test")

	// Create initial commit
	testFile := filepath.Join(mainRepo, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, mainRepo, "add", ".")
	runGit(t, mainRepo, "commit", "-m", "initial")

	// Create a worktree
	runGit(t, mainRepo, "worktree", "add", "-b", "feature", worktreeDir)

	// Track git state changes in the worktree
	var mu sync.Mutex
	var gitStateChanges []*gitstate.GitState

	loop := NewLoop(Config{
		Model:         NewPredictableService(),
		History:       []llm.Message{},
		WorkingDir:    worktreeDir,
		GetWorkingDir: func() string { return worktreeDir },
		OnGitStateChange: func(ctx context.Context, state *gitstate.GitState) {
			mu.Lock()
			gitStateChanges = append(gitStateChanges, state)
			mu.Unlock()
		},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			return nil
		},
	})

	// Verify initial state
	if loop.lastGitState == nil {
		t.Fatal("expected initial git state to be captured")
	}
	if loop.lastGitState.Branch != "feature" {
		t.Errorf("expected branch 'feature', got %q", loop.lastGitState.Branch)
	}
	if loop.lastGitState.Worktree != worktreeDir {
		t.Errorf("expected worktree %q, got %q", worktreeDir, loop.lastGitState.Worktree)
	}

	// Make a commit in the worktree
	worktreeFile := filepath.Join(worktreeDir, "feature.txt")
	if err := os.WriteFile(worktreeFile, []byte("feature content"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktreeDir, "add", ".")
	runGit(t, worktreeDir, "commit", "-m", "feature commit")

	// Process a turn to detect the change
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = loop.ProcessOneTurn(ctx)
	if err != nil {
		t.Fatalf("ProcessOneTurn failed: %v", err)
	}

	mu.Lock()
	numChanges := len(gitStateChanges)
	mu.Unlock()

	if numChanges != 1 {
		t.Errorf("expected 1 git state change in worktree, got %d", numChanges)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	// For commits, use --no-verify to skip hooks
	if len(args) > 0 && args[0] == "commit" {
		newArgs := []string{"commit", "--no-verify"}
		newArgs = append(newArgs, args[1:]...)
		args = newArgs
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}

func TestPredictableServiceTokenContextWindow(t *testing.T) {
	service := NewPredictableService()
	window := service.Profile().ContextWindow
	if window != 200000 {
		t.Errorf("expected TokenContextWindow to return 200000, got %d", window)
	}
}

func TestPredictableServiceMaxImageDimension(t *testing.T) {
	service := NewPredictableService()
	dimension := service.Profile().MaxImageDimension
	if dimension != 2000 {
		t.Errorf("expected MaxImageDimension to return 2000, got %d", dimension)
	}
}

func TestPredictableServiceThinking(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("think: This is a test thought")}})
	if err != nil {
		t.Fatalf("thinking test failed: %v", err)
	}

	var thinkingContent *dmessage.ContentBlock
	for _, content := range resp.Message.Content {
		if content.Type == dmessage.BlockReasoning {
			thinkingContent = &content
			break
		}
	}

	if thinkingContent == nil {
		t.Fatal("no thinking content found")
	}

	// Check thinking content contains the thoughts
	if thinkingContent.Reasoning != "This is a test thought" {
		t.Errorf("expected thinking 'This is a test thought', got '%v'", thinkingContent.Reasoning)
	}
}

func TestPredictableServicePatchTool(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("patch: /tmp/test.txt")}})
	if err != nil {
		t.Fatalf("patch tool test failed: %v", err)
	}

	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "patch" {
		t.Fatal("no patch tool use content found")
	}

	// Check tool input contains the file path
	var toolInput map[string]interface{}
	if err := json.Unmarshal(resp.Message.ToolCalls[0].Arguments, &toolInput); err != nil {
		t.Fatalf("failed to parse tool input: %v", err)
	}

	if toolInput["path"] != "/tmp/test.txt" {
		t.Errorf("expected path '/tmp/test.txt', got '%v'", toolInput["path"])
	}
}

func TestPredictableServiceMalformedPatchTool(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("patch bad json")}})
	if err != nil {
		t.Fatalf("malformed patch tool test failed: %v", err)
	}

	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "patch" {
		t.Fatal("no patch tool use content found")
	}

	// Check that the tool input is malformed JSON (as expected)
	toolInputStr := string(resp.Message.ToolCalls[0].Arguments)
	if !strings.Contains(toolInputStr, "parameter name") {
		t.Errorf("expected malformed JSON in tool input, got: %s", toolInputStr)
	}
}

func TestPredictableServiceError(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("error: test error")}})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "predictable error: test error") {
		t.Errorf("expected error message to contain 'predictable error: test error', got: %v", err)
	}

	if resp.Message.Role != "" {
		t.Error("expected empty response when error occurs")
	}
}

func TestPredictableServiceRequestTracking(t *testing.T) {
	service := NewPredictableService()

	// Initially no requests
	requests := service.GetRecentRequests()
	if requests != nil {
		t.Errorf("expected nil requests initially, got %v", requests)
	}

	lastReq := service.GetLastRequest()
	if lastReq != nil {
		t.Errorf("expected nil last request initially, got %v", lastReq)
	}

	// Make a request
	ctx := context.Background()
	req := dmodel.Request{Messages: []dmessage.Message{dmessage.Human("hello")}}

	_, err := service.Invoke(ctx, req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}

	// Check that request was tracked
	requests = service.GetRecentRequests()
	if len(requests) != 1 {
		t.Errorf("expected 1 request, got %d", len(requests))
	}

	lastReq = service.GetLastRequest()
	if lastReq == nil {
		t.Fatal("expected last request to be non-nil")
	}

	if len(lastReq.Messages) != 1 {
		t.Errorf("expected 1 message in last request, got %d", len(lastReq.Messages))
	}

	// Test clearing requests
	service.ClearRequests()
	requests = service.GetRecentRequests()
	if requests != nil {
		t.Errorf("expected nil requests after clearing, got %v", requests)
	}

	lastReq = service.GetLastRequest()
	if lastReq != nil {
		t.Errorf("expected nil last request after clearing, got %v", lastReq)
	}

	// Test that only last 10 requests are kept
	for i := range 15 {
		testReq := dmodel.Request{Messages: []dmessage.Message{dmessage.Human(fmt.Sprintf("test %d", i))}}
		_, err := service.Invoke(ctx, testReq)
		if err != nil {
			t.Fatalf("Do failed on iteration %d: %v", i, err)
		}
	}

	requests = service.GetRecentRequests()
	if len(requests) != 10 {
		t.Errorf("expected 10 requests (last 10), got %d", len(requests))
	}

	// Check that we have requests 5-14 (0-indexed)
	for i, req := range requests {
		expectedText := fmt.Sprintf("test %d", i+5)
		if len(req.Messages) == 0 || len(req.Messages[0].Content) == 0 {
			t.Errorf("request %d has no content", i)
			continue
		}
		if req.Messages[0].TextContent() != expectedText {
			t.Errorf("expected request %d to have text '%s', got '%s'", i, expectedText, req.Messages[0].Content[0].Text)
		}
	}
}

func TestPredictableServiceScreenshotTool(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("screenshot: .test-class")}})
	if err != nil {
		t.Fatalf("screenshot tool test failed: %v", err)
	}

	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "browser" {
		t.Fatal("no screenshot tool use content found")
	}

	// Check tool input contains the selector
	var toolInput map[string]interface{}
	if err := json.Unmarshal(resp.Message.ToolCalls[0].Arguments, &toolInput); err != nil {
		t.Fatalf("failed to parse tool input: %v", err)
	}

	if toolInput["selector"] != ".test-class" {
		t.Errorf("expected selector '.test-class', got '%v'", toolInput["selector"])
	}
}

func TestPredictableServiceToolSmorgasbord(t *testing.T) {
	service := NewPredictableService()

	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("tool smorgasbord")}})
	if err != nil {
		t.Fatalf("tool smorgasbord test failed: %v", err)
	}

	toolUseCount := len(resp.Message.ToolCalls)

	if toolUseCount != 19 {
		t.Errorf("expected the complete 19-call tool fixture, got %d", toolUseCount)
	}
	counts := map[string]int{}
	for _, call := range resp.Message.ToolCalls {
		counts[call.Name]++
	}
	for _, name := range []string{"browser_emulate", "browser_network", "browser_accessibility", "browser_profile"} {
		if counts[name] != 1 {
			t.Errorf("tool fixture contains %d %q calls, want 1", counts[name], name)
		}
	}
}

func TestProcessLLMRequestError(t *testing.T) {
	// Test error handling when LLM service returns an error
	errorService := &errorLLMService{err: fmt.Errorf("test LLM error")}

	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recordedMessages = append(recordedMessages, message)
		return nil
	}

	loop := NewLoop(Config{
		Model:         errorService,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
	})

	// Queue a user message
	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "test message"}},
	}
	loop.QueueUserMessage(userMessage)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := loop.ProcessOneTurn(ctx)
	if err == nil {
		t.Fatal("expected error from ProcessOneTurn, got nil")
	}

	if !strings.Contains(err.Error(), "LLM request failed") {
		t.Errorf("expected error to contain 'LLM request failed', got: %v", err)
	}

	// Check that error message was recorded
	if len(recordedMessages) < 1 {
		t.Fatalf("expected 1 recorded message (error), got %d", len(recordedMessages))
	}

	if recordedMessages[0].Role != llm.MessageRoleAssistant {
		t.Errorf("expected recorded message to be assistant role, got %s", recordedMessages[0].Role)
	}

	if len(recordedMessages[0].Content) != 1 {
		t.Fatalf("expected 1 content item in recorded message, got %d", len(recordedMessages[0].Content))
	}

	if recordedMessages[0].Content[0].Type != llm.ContentTypeText {
		t.Errorf("expected text content, got %s", recordedMessages[0].Content[0].Type)
	}

	if !strings.Contains(recordedMessages[0].Content[0].Text, "LLM request failed") {
		t.Errorf("expected error message to contain 'LLM request failed', got: %s", recordedMessages[0].Content[0].Text)
	}

	// Verify EndOfTurn is set so the agent working state is properly updated
	if !recordedMessages[0].EndOfTurn {
		t.Error("expected error message to have EndOfTurn=true so agent working state is updated")
	}
}

// errorLLMService is a test LLM service that always returns an error
type errorLLMService struct {
	err error
}

func (e *errorLLMService) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "test", Model: "error", ContextWindow: 200000, SupportsImages: true, MaxImageDimension: 2000, MaxImageBytes: 5 * 1024 * 1024}
}
func (e *errorLLMService) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	return dmodel.Response{}, e.err
}
func (e *errorLLMService) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return nil, e.err
}

// retryableLLMService fails with a retryable error a specified number of times, then succeeds
type retryableLLMService struct {
	failuresRemaining int
	callCount         int
	mu                sync.Mutex
}

func (r *retryableLLMService) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "test", Model: "retryable", ContextWindow: 200000, SupportsImages: true, MaxImageDimension: 2000, MaxImageBytes: 5 * 1024 * 1024}
}
func (r *retryableLLMService) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	r.mu.Lock()
	r.callCount++
	if r.failuresRemaining > 0 {
		r.failuresRemaining--
		r.mu.Unlock()
		return dmodel.Response{}, fmt.Errorf("connection error: EOF")
	}
	r.mu.Unlock()
	return dmodel.Response{Message: dmessage.Assistant("Success after retry")}, nil
}
func (r *retryableLLMService) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return nil, fmt.Errorf("retryable test model does not stream")
}
func (r *retryableLLMService) getCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callCount
}

func TestLLMRequestRetryOnEOF(t *testing.T) {
	// Test that LLM requests are retried on EOF errors
	retryService := &retryableLLMService{failuresRemaining: 1}

	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recordedMessages = append(recordedMessages, message)
		return nil
	}
	var warnings []string

	loop := NewLoop(Config{
		Model:         retryService,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
		RecordWarning: func(ctx context.Context, text string) error {
			warnings = append(warnings, text)
			return nil
		},
	})

	// Queue a user message
	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "test message"}},
	}
	loop.QueueUserMessage(userMessage)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := loop.ProcessOneTurn(ctx)
	if err != nil {
		t.Fatalf("expected no error after retry, got: %v", err)
	}

	// Should have been called twice (1 failure + 1 success)
	if retryService.getCallCount() != 2 {
		t.Errorf("expected 2 LLM calls (retry), got %d", retryService.getCallCount())
	}

	if len(warnings) != 0 {
		t.Fatalf("expected outer EOF retry to stay silent, got %d warnings: %v", len(warnings), warnings)
	}

	// Check that success message was recorded
	if len(recordedMessages) != 1 {
		t.Fatalf("expected 1 recorded message (success), got %d", len(recordedMessages))
	}

	if !strings.Contains(recordedMessages[0].Content[0].Text, "Success after retry") {
		t.Errorf("expected success message, got: %s", recordedMessages[0].Content[0].Text)
	}
}

func TestLLMRequestRetryExhausted(t *testing.T) {
	// Test that after max retries, error is returned
	retryService := &retryableLLMService{failuresRemaining: 10} // More than maxRetries

	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recordedMessages = append(recordedMessages, message)
		return nil
	}

	loop := NewLoop(Config{
		Model:         retryService,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
	})

	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "test message"}},
	}
	loop.QueueUserMessage(userMessage)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := loop.ProcessOneTurn(ctx)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	// Should have been called maxRetries times (2)
	if retryService.getCallCount() != 2 {
		t.Errorf("expected 2 LLM calls (maxRetries), got %d", retryService.getCallCount())
	}

	// Check error message was recorded
	if len(recordedMessages) != 1 {
		t.Fatalf("expected 1 recorded message (error), got %d", len(recordedMessages))
	}

	if !strings.Contains(recordedMessages[0].Content[0].Text, "LLM request failed") {
		t.Errorf("expected error message, got: %s", recordedMessages[0].Content[0].Text)
	}
}

func TestIsRetryableError(t *testing.T) {
	// isRetryableError is the loop's TIGHT inner-retry classifier: only
	// transport-layer hiccups that have a good chance of succeeding on a
	// quick re-do. Provider 5xx, rate limits, etc. are handled by the
	// user-facing Retry button, not here.
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"nil error", nil, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"EOF error string", fmt.Errorf("EOF"), true},
		{"wrapped EOF", fmt.Errorf("connection error: EOF"), true},
		{"connection reset", fmt.Errorf("connection reset by peer"), true},
		{"connection refused", fmt.Errorf("connection refused"), true},
		{"timeout", fmt.Errorf("i/o timeout"), true},
		{"idle stall timeout", fmt.Errorf("stream: %w", llmhttp.ErrIdleTimeout), true},
		{"rate limit not in tight set", fmt.Errorf("rate limit exceeded"), false},
		{"503 not in tight set", fmt.Errorf("upstream returned 503"), false},
		{"generic error", fmt.Errorf("something went wrong"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.retryable {
				t.Errorf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.retryable)
			}
		})
	}
}

// TestIsRetryableLLMError covers the broader user-facing classifier used
// by the Retry button.
func TestIsRetryableLLMError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"nil error", nil, false},
		{"EOF", io.EOF, true},
		{"rate limit retryable", fmt.Errorf("rate limit exceeded"), true},
		{"503 retryable", fmt.Errorf("upstream returned 503"), true},
		{"max_tokens 500 number not retryable", fmt.Errorf("max_tokens=500 is too small"), false},
		{"gateway proxy error retryable", fmt.Errorf("gateway proxy error: dial tcp ..."), true},
		{"upstream connect error retryable", fmt.Errorf("upstream connect error or disconnect/reset before headers"), true},
		{"deadline exceeded retryable", fmt.Errorf("context deadline exceeded"), true},
		{"idle stall timeout retryable", fmt.Errorf("stream: %w", llmhttp.ErrIdleTimeout), true},
		{"deployment scaling retryable", fmt.Errorf("DEPLOYMENT_SCALING_UP scale-up in progress"), true},
		{"credits exhausted not retryable", fmt.Errorf("LLM credits exhausted; credits refresh over time"), false},
		{"invalid api key not retryable", fmt.Errorf("invalid api key"), false},
		{"invalid_request_error not retryable", fmt.Errorf("invalid_request_error: messages.0.content.0.thinking.signature: Field required"), false},
		{"model_not_found not retryable", fmt.Errorf("model_not_found: The model gpt-foo does not exist"), false},
		{"generic error not retryable", fmt.Errorf("something weird happened"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableLLMError(tt.err); got != tt.retryable {
				t.Errorf("IsRetryableLLMError(%v) = %v, want %v", tt.err, got, tt.retryable)
			}
		})
	}
}

func TestCheckGitStateChange(t *testing.T) {
	// Create a test repo
	tmpDir := t.TempDir()

	// Initialize git repo
	runGit(t, tmpDir, "init")
	runGit(t, tmpDir, "config", "user.email", "test@test.com")
	runGit(t, tmpDir, "config", "user.name", "Test")

	// Create initial commit
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmpDir, "add", ".")
	runGit(t, tmpDir, "commit", "-m", "initial")

	// Test with nil OnGitStateChange - should not panic
	loop := NewLoop(Config{
		Model:         NewPredictableService(),
		History:       []llm.Message{},
		WorkingDir:    tmpDir,
		GetWorkingDir: func() string { return tmpDir },
		// OnGitStateChange is nil
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			return nil
		},
	})

	// This should not panic
	loop.checkGitStateChange(context.Background())

	// Test with actual callback
	var gitStateChanges []*gitstate.GitState
	loop = NewLoop(Config{
		Model:         NewPredictableService(),
		History:       []llm.Message{},
		WorkingDir:    tmpDir,
		GetWorkingDir: func() string { return tmpDir },
		OnGitStateChange: func(ctx context.Context, state *gitstate.GitState) {
			gitStateChanges = append(gitStateChanges, state)
		},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			return nil
		},
	})

	// Make a change
	if err := os.WriteFile(testFile, []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmpDir, "add", ".")
	runGit(t, tmpDir, "commit", "-m", "update")

	// Check git state change
	loop.checkGitStateChange(context.Background())

	if len(gitStateChanges) != 1 {
		t.Errorf("expected 1 git state change, got %d", len(gitStateChanges))
	}

	// Call again - should not trigger another change since state is the same
	loop.checkGitStateChange(context.Background())

	if len(gitStateChanges) != 1 {
		t.Errorf("expected still 1 git state change (no new changes), got %d", len(gitStateChanges))
	}
}

func TestExecuteToolCallsWithMissingTool(t *testing.T) {
	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recordedMessages = append(recordedMessages, message)
		return nil
	}

	service := &customPredictableService{responseFunc: func(request dmodel.Request) (dmodel.Response, error) {
		if nativeToolResultCount(request) == 0 {
			return nativeToolResponse("", "test_tool_123", "nonexistent_tool", json.RawMessage(`{"test":"input"}`)), nil
		}
		return nativeTextResponse("done"), nil
	}}
	loop := NewLoop(Config{Model: service, RecordMessage: recordFunc})
	loop.QueueUserMessage(llm.UserStringMessage("call a missing tool"))
	if err := loop.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Should have recorded a user message with tool result
	if len(recordedMessages) < 1 {
		t.Fatalf("expected 1 recorded message, got %d", len(recordedMessages))
	}

	msg, ok := recordedToolResult(recordedMessages)
	if !ok {
		t.Fatalf("no tool-result message in %#v", recordedMessages)
	}
	if msg.Role != llm.MessageRoleUser {
		t.Errorf("expected user role, got %s", msg.Role)
	}

	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(msg.Content))
	}

	toolResult := msg.Content[0]
	if toolResult.Type != llm.ContentTypeToolResult {
		t.Errorf("expected tool result content, got %s", toolResult.Type)
	}

	if toolResult.ToolUseID != "test_tool_123" {
		t.Errorf("expected tool use ID 'test_tool_123', got %s", toolResult.ToolUseID)
	}

	if !toolResult.ToolError {
		t.Error("expected ToolError to be true")
	}

	if len(toolResult.ToolResult) != 1 {
		t.Fatalf("expected 1 tool result content item, got %d", len(toolResult.ToolResult))
	}

	if toolResult.ToolResult[0].Type != llm.ContentTypeText {
		t.Errorf("expected text content in tool result, got %s", toolResult.ToolResult[0].Type)
	}

	expectedText := "Tool 'nonexistent_tool' not found"
	if toolResult.ToolResult[0].Text != expectedText {
		t.Errorf("expected tool result text '%s', got '%s'", expectedText, toolResult.ToolResult[0].Text)
	}
}

func TestExecuteToolCallsWithErrorTool(t *testing.T) {
	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recordedMessages = append(recordedMessages, message)
		return nil
	}

	// Create a tool that always returns an error
	errorTool := dtool.Func{
		Spec: dtool.Definition{Name: "error_tool", Description: "A tool that always errors", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
		Run: func(context.Context, json.RawMessage, dtool.Runtime) (dtool.Result, error) {
			return dtool.Result{}, fmt.Errorf("intentional test error")
		},
	}

	service := &customPredictableService{responseFunc: func(request dmodel.Request) (dmodel.Response, error) {
		if nativeToolResultCount(request) == 0 {
			return nativeToolResponse("", "error_tool_123", "error_tool", json.RawMessage(`{}`)), nil
		}
		return nativeTextResponse("done"), nil
	}}
	loop := NewLoop(Config{
		Model:         service,
		Tools:         []dtool.Tool{errorTool},
		RecordMessage: recordFunc,
	})
	loop.QueueUserMessage(llm.UserStringMessage("call an error tool"))
	if err := loop.ProcessOneTurn(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Should have recorded a user message with tool result
	if len(recordedMessages) < 1 {
		t.Fatalf("expected 1 recorded message, got %d", len(recordedMessages))
	}

	msg, ok := recordedToolResult(recordedMessages)
	if !ok {
		t.Fatalf("no tool-result message in %#v", recordedMessages)
	}
	if msg.Role != llm.MessageRoleUser {
		t.Errorf("expected user role, got %s", msg.Role)
	}

	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content item, got %d", len(msg.Content))
	}

	toolResult := msg.Content[0]
	if toolResult.Type != llm.ContentTypeToolResult {
		t.Errorf("expected tool result content, got %s", toolResult.Type)
	}

	if toolResult.ToolUseID != "error_tool_123" {
		t.Errorf("expected tool use ID 'error_tool_123', got %s", toolResult.ToolUseID)
	}

	if !toolResult.ToolError {
		t.Error("expected ToolError to be true")
	}

	if len(toolResult.ToolResult) != 1 {
		t.Fatalf("expected 1 tool result content item, got %d", len(toolResult.ToolResult))
	}

	if toolResult.ToolResult[0].Type != llm.ContentTypeText {
		t.Errorf("expected text content in tool result, got %s", toolResult.ToolResult[0].Type)
	}

	expectedText := "intentional test error"
	if !strings.Contains(toolResult.ToolResult[0].Text, expectedText) {
		t.Errorf("expected tool result text to contain %q, got %q", expectedText, toolResult.ToolResult[0].Text)
	}
}

func recordedToolResult(messages []llm.Message) (llm.Message, bool) {
	for _, item := range messages {
		for _, content := range item.Content {
			if content.Type == llm.ContentTypeToolResult {
				return item, true
			}
		}
	}
	return llm.Message{}, false
}

func TestMaxTokensTruncation(t *testing.T) {
	var mu sync.Mutex
	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		mu.Lock()
		recordedMessages = append(recordedMessages, message)
		mu.Unlock()
		return nil
	}

	service := NewPredictableService()
	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
	})

	// Queue a user message that triggers max tokens truncation
	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "maxTokens"}},
	}
	loop.QueueUserMessage(userMessage)

	// Run the loop - it should stop after handling truncation
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := loop.Go(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}

	// Check recorded messages
	mu.Lock()
	numMessages := len(recordedMessages)
	messages := make([]llm.Message, len(recordedMessages))
	copy(messages, recordedMessages)
	mu.Unlock()

	// We should see two messages:
	// 1. The truncated message (with ExcludedFromContext=true) for cost tracking
	// 2. The truncation error message (with ErrorType=truncation)
	if numMessages != 2 {
		t.Errorf("Expected 2 recorded messages (truncated + error), got %d", numMessages)
		for i, msg := range messages {
			t.Logf("Message %d: Role=%v, EndOfTurn=%v, ExcludedFromContext=%v, ErrorType=%v",
				i, msg.Role, msg.EndOfTurn, msg.ExcludedFromContext, msg.ErrorType)
		}
		return
	}

	// First message: truncated response (for cost tracking, excluded from context)
	truncatedMsg := messages[0]
	if truncatedMsg.Role != llm.MessageRoleAssistant {
		t.Errorf("Truncated message should be assistant, got %v", truncatedMsg.Role)
	}
	if !truncatedMsg.ExcludedFromContext {
		t.Error("Truncated message should have ExcludedFromContext=true")
	}

	// Second message: truncation error
	errorMsg := messages[1]
	if errorMsg.Role != llm.MessageRoleAssistant {
		t.Errorf("Error message should be assistant, got %v", errorMsg.Role)
	}
	if !errorMsg.EndOfTurn {
		t.Error("Error message should have EndOfTurn=true")
	}
	if errorMsg.ErrorType != llm.ErrorTypeTruncation {
		t.Errorf("Error message should have ErrorType=truncation, got %v", errorMsg.ErrorType)
	}
	if errorMsg.ExcludedFromContext {
		t.Error("Error message should not be excluded from context")
	}
	if !strings.Contains(errorMsg.Content[0].Text, "SYSTEM ERROR") {
		t.Errorf("Error message should contain SYSTEM ERROR, got: %s", errorMsg.Content[0].Text)
	}

	// Verify history contains user message + error message, but NOT the truncated response
	loop.mu.Lock()
	history := loop.history
	loop.mu.Unlock()

	// History should have: user message + error message (the truncated response is NOT added to history)
	if len(history) != 2 {
		t.Errorf("History should have 2 messages (user + error), got %d", len(history))
	}
}

// TestRefusal verifies that a stop_reason=refusal response is surfaced to the
// user as a visible, non-retryable error message rather than a silent empty
// end-of-turn. Regression test: previously refusals fell through as an empty
// assistant bubble, so "continue" produced an endless string of blank turns.
func TestRefusal(t *testing.T) {
	var mu sync.Mutex
	var recordedMessages []llm.Message
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		mu.Lock()
		recordedMessages = append(recordedMessages, message)
		mu.Unlock()
		return nil
	}

	service := NewPredictableService()
	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
	})

	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "refusal"}},
	}
	loop.QueueUserMessage(userMessage)

	// The loop should end the turn after handling the refusal, so Go returns
	// when the queue drains (context deadline) rather than spinning.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if err := loop.Go(ctx); err != context.DeadlineExceeded {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}

	mu.Lock()
	messages := make([]llm.Message, len(recordedMessages))
	copy(messages, recordedMessages)
	mu.Unlock()

	// We should see two recorded messages:
	// 1. The raw refusal response (excluded from context, for cost tracking).
	// 2. A visible refusal error message (ErrorType=refusal, EndOfTurn=true).
	if len(messages) != 2 {
		t.Fatalf("expected 2 recorded messages (refusal + error), got %d", len(messages))
	}

	rawMsg := messages[0]
	if !rawMsg.ExcludedFromContext {
		t.Error("raw refusal message should have ExcludedFromContext=true")
	}

	errMsg := messages[1]
	if errMsg.Role != llm.MessageRoleAssistant {
		t.Errorf("error message should be assistant, got %v", errMsg.Role)
	}
	if errMsg.ErrorType != llm.ErrorTypeRefusal {
		t.Errorf("error message should have ErrorType=refusal, got %v", errMsg.ErrorType)
	}
	if !errMsg.EndOfTurn {
		t.Error("error message should have EndOfTurn=true")
	}
	if errMsg.ErrorRetryable {
		t.Error("refusal error should not be retryable (retrying the same context refuses again)")
	}
	if errMsg.ExcludedFromContext {
		t.Error("error message should not be excluded from context")
	}
	if len(errMsg.Content) == 0 || errMsg.Content[0].Text == "" {
		t.Fatal("error message should have non-empty text")
	}
	if !strings.Contains(errMsg.Content[0].Text, "declined") {
		t.Errorf("error message should explain the refusal, got: %s", errMsg.Content[0].Text)
	}
	// The user-visible refusal notice must guide the user toward continuing on a
	// more capable model: mention switching to Opus and the /model command.
	if !strings.Contains(errMsg.Content[0].Text, "Opus") {
		t.Errorf("error message should suggest switching to Opus, got: %s", errMsg.Content[0].Text)
	}
	if !strings.Contains(errMsg.Content[0].Text, "/model") {
		t.Errorf("error message should mention the /model command, got: %s", errMsg.Content[0].Text)
	}
	// The provider's structured refusal reason must be captured on the message
	// and surfaced in the notice text (the predictable service returns a "cyber"
	// category with an explanation).
	if errMsg.RefusalCategory != "cyber" {
		t.Errorf("error message should carry RefusalCategory=cyber, got %q", errMsg.RefusalCategory)
	}
	if errMsg.RefusalExplanation == "" {
		t.Error("error message should carry a RefusalExplanation")
	}
	if !strings.Contains(errMsg.Content[0].Text, "Reason:") {
		t.Errorf("error message should include the refusal reason, got: %s", errMsg.Content[0].Text)
	}
	// The coarse category the provider returned must also be visible.
	if !strings.Contains(errMsg.Content[0].Text, "Category: cyber") {
		t.Errorf("error message should include the refusal category, got: %s", errMsg.Content[0].Text)
	}

	// Neither the raw empty refusal nor the synthetic error message may live in
	// context history: both are user-visible-only artifacts. The cold-start path
	// (partitionMessages + ListMessagesForContext) excludes them, and the active
	// loop must match. So history holds only the user message.
	loop.mu.Lock()
	history := loop.history
	loop.mu.Unlock()
	if len(history) != 1 {
		t.Fatalf("history should have 1 message (user only), got %d", len(history))
	}
	for i, m := range history {
		if m.ErrorType != llm.ErrorTypeNone {
			t.Errorf("history[%d] should not be an error message, got ErrorType=%v", i, m.ErrorType)
		}
	}
}

// requestCapturingService records every request it receives (a deep-ish copy of
// the Messages slice header is enough since the loop rebuilds it each turn) then
// delegates to a PredictableService so keyword triggers like "refusal" and
// "hello" still drive realistic responses.
type requestCapturingService struct {
	*PredictableService
	mu  *sync.Mutex
	out *[]dmodel.Request
}

func NewRequestCapturingService(mu *sync.Mutex, out *[]dmodel.Request) *requestCapturingService {
	return &requestCapturingService{PredictableService: NewPredictableService(), mu: mu, out: out}
}

func (service *requestCapturingService) Invoke(ctx context.Context, request dmodel.Request) (dmodel.Response, error) {
	service.mu.Lock()
	*service.out = append(*service.out, request)
	service.mu.Unlock()
	return service.PredictableService.Invoke(ctx, request)
}
func (service *requestCapturingService) Stream(ctx context.Context, request dmodel.Request) (dmodel.Stream, error) {
	service.mu.Lock()
	*service.out = append(*service.out, request)
	service.mu.Unlock()
	return service.PredictableService.Stream(ctx, request)
}

// TestRefusalThenRephraseNotInContext is the reviewer's regression test: after
// a refusal, a rephrased user message in the SAME active session must not send
// the synthetic refusal artifact back to the model. This guards against the
// active-vs-rehydrated divergence where l.history leaked the error message.
func TestRefusalThenRephraseNotInContext(t *testing.T) {
	var mu sync.Mutex
	var sentRequests []dmodel.Request
	service := NewRequestCapturingService(&mu, &sentRequests)

	loop := NewLoop(Config{
		Model:         service,
		History:       []llm.Message{},
		RecordMessage: func(context.Context, llm.Message, llm.Usage, []llm.PurposedUsage) error { return nil },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First turn triggers a refusal. Drive it as its own complete turn so the
	// refusal is fully handled before the user rephrases (mirroring a real
	// session: send, turn ends, then send again).
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "refusal"}},
	})
	if err := loop.ProcessOneTurn(ctx); err != nil {
		t.Fatalf("first turn: %v", err)
	}

	// Second (rephrased) turn should get a normal answer.
	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hello"}},
	})
	if err := loop.ProcessOneTurn(ctx); err != nil {
		t.Fatalf("second turn: %v", err)
	}

	mu.Lock()
	reqs := make([]dmodel.Request, len(sentRequests))
	copy(reqs, sentRequests)
	mu.Unlock()

	if len(reqs) < 2 {
		t.Fatalf("expected at least 2 LLM requests (refusal + rephrase), got %d", len(reqs))
	}

	// The last request is the rephrase turn. It must not contain the synthetic
	// refusal text or any ErrorTypeRefusal message.
	last := reqs[len(reqs)-1]
	for i, m := range last.Messages {
		if strings.Contains(m.TextContent(), "declined to continue") {
			t.Errorf("rephrase request message[%d] leaks synthetic refusal text: %q", i, m.TextContent())
		}
	}
}

func TestPredictableServiceFailEmitsRetryWarning(t *testing.T) {
	service := NewPredictableService()
	resp, err := service.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("fail nope")}})
	if err == nil {
		t.Fatal("expected failure, got nil")
	}
	if !strings.Contains(err.Error(), "predictable failure: nope") {
		t.Fatalf("expected predictable failure, got %v", err)
	}
	if resp.Message.Role != "" {
		t.Fatal("expected empty response")
	}
	retryable, ok := err.(dmodel.RetryReporter)
	if !ok {
		t.Fatalf("error %T does not expose retry metadata", err)
	}
	warning := retryable.RetryEvent(1, time.Second)
	if !warning.Retryable || warning.Provider != "predictable" || warning.Model != "predictable-v1" || warning.Err != "nope" {
		t.Fatalf("unexpected warning: %#v", warning)
	}
}

// switchableLLM returns the configured error until switched to succeed.
type switchableLLM struct {
	mu       sync.Mutex
	calls    int
	failWith error
}

func (s *switchableLLM) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "test", Model: "switchable", ContextWindow: 200000, SupportsImages: true, MaxImageDimension: 2000, MaxImageBytes: 5 * 1024 * 1024}
}
func (s *switchableLLM) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	s.mu.Lock()
	s.calls++
	err := s.failWith
	s.mu.Unlock()
	if err != nil {
		return dmodel.Response{}, err
	}
	return dmodel.Response{Message: dmessage.Assistant("ok")}, nil
}
func (s *switchableLLM) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return nil, fmt.Errorf("switchable test model does not stream")
}
func (s *switchableLLM) succeed() {
	s.mu.Lock()
	s.failWith = nil
	s.mu.Unlock()
}

func (s *switchableLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestLoopRetryAfterPersistentFailure exercises Loop.Retry(): the loop's first
// attempt exhausts all internal retries and records an error message; calling
// Retry() after the upstream recovers should drive a new processLLMRequest
// without queueing any new user message.
func TestLoopRetryAfterPersistentFailure(t *testing.T) {
	svc := &switchableLLM{failWith: fmt.Errorf("connection error: EOF")}

	var (
		mu       sync.Mutex
		recorded []llm.Message
	)
	record := func(ctx context.Context, m llm.Message, _ llm.Usage, _ []llm.PurposedUsage) error {
		mu.Lock()
		recorded = append(recorded, m)
		mu.Unlock()
		return nil
	}

	loop := NewLoop(Config{
		Model:         svc,
		History:       []llm.Message{},
		RecordMessage: record,
		RecordWarning: func(ctx context.Context, _ string) error { return nil },
	})

	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "hi"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First turn: exhausts retries and records an error message.
	if err := loop.ProcessOneTurn(ctx); err == nil {
		t.Fatalf("expected error from first turn, got nil")
	}

	mu.Lock()
	initialCount := len(recorded)
	var errMsg llm.Message
	if initialCount > 0 {
		errMsg = recorded[initialCount-1]
	}
	mu.Unlock()
	if initialCount == 0 || errMsg.ErrorType != llm.ErrorTypeLLMRequest {
		t.Fatalf("expected error message recorded, got %d messages", initialCount)
	}
	if !errMsg.ErrorRetryable {
		t.Errorf("expected error message to be ErrorRetryable=true")
	}

	// Recover upstream and trigger Retry.
	svc.succeed()
	callsBefore := svc.callCount()
	loop.Retry()

	// Use the loop's Go() so the retry signal is consumed.
	goCtx, goCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer goCancel()
	done := make(chan error, 1)
	go func() { done <- loop.Go(goCtx) }()

	// Wait until we observe a successful assistant message recorded after Retry.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(recorded)
		var last llm.Message
		if n > 0 {
			last = recorded[n-1]
		}
		mu.Unlock()
		if n > initialCount && last.ErrorType == llm.ErrorTypeNone && len(last.Content) > 0 && last.Content[0].Text == "ok" {
			goCancel()
			<-done
			if svc.callCount() != callsBefore+1 {
				t.Errorf("expected exactly one new LLM call from Retry, got %d (was %d)", svc.callCount(), callsBefore)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	goCancel()
	<-done
	t.Fatalf("Retry() did not produce a successful assistant message; calls=%d, recorded=%d", svc.callCount(), len(recorded))
}

// pauseLLMService preserves the upstream fixture name while modeling the
// OpenAI Responses server-side web-search shape used by the native port.
type pauseLLMService struct {
	calls    int
	lastSent []dmodel.Request
}

func (p *pauseLLMService) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "openai", Model: "gpt-5.6-luna", ContextWindow: 200000, SupportsImages: true, MaxImageDimension: 2000, MaxImageBytes: 5 * 1024 * 1024}
}
func (p *pauseLLMService) Invoke(_ context.Context, request dmodel.Request) (dmodel.Response, error) {
	p.calls++
	p.lastSent = append(p.lastSent, request)
	response := dmessage.Assistant("The answer is 42.")
	response.Content = []dmessage.ContentBlock{
		{Type: dmessage.BlockText, Text: "Let me search."},
		{Type: dmessage.BlockServerTool, ID: "srv_1", Name: "web_search", Extra: map[string]json.RawMessage{"arguments": json.RawMessage(`{"query":"x"}`)}},
		{Type: dmessage.BlockSearchResult, ID: "result_1", Name: "t", URL: "https://example.com"},
		{Type: dmessage.BlockText, Text: "The answer is 42."},
	}
	response.Usage = &dmessage.Usage{InputTokens: 30, OutputTokens: 13, TotalTokens: 43, Provider: "openai", Model: "gpt-5.6-luna"}
	return dmodel.Response{Message: response}, nil
}

func (*pauseLLMService) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return nil, fmt.Errorf("web-search test model does not stream")
}

// TestLoopResolvesPauseTurn preserves the upstream name while verifying that
// OpenAI Responses server-side search blocks remain in one assistant message.
func TestLoopResolvesPauseTurn(t *testing.T) {
	var recorded []llm.Message
	var recordedUsage []llm.Usage
	recordFunc := func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
		recorded = append(recorded, message)
		recordedUsage = append(recordedUsage, usage)
		return nil
	}

	svc := &pauseLLMService{}
	loop := NewLoop(Config{
		Model:         svc,
		History:       []llm.Message{},
		RecordMessage: recordFunc,
	})

	loop.QueueUserMessage(llm.UserStringMessage("search the web for the answer"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := loop.ProcessOneTurn(ctx); err != nil {
		t.Fatalf("ProcessOneTurn: %v", err)
	}

	if svc.calls != 1 {
		t.Fatalf("expected 1 native Responses call, got %d", svc.calls)
	}

	if len(svc.lastSent) != 1 {
		t.Fatalf("expected 1 recorded request, got %d", len(svc.lastSent))
	}

	// Find the recorded assistant message and assert it merges server_tool_use
	// with its web_search_tool_result in ONE message.
	var asst *llm.Message
	for i := range recorded {
		if recorded[i].Role == llm.MessageRoleAssistant {
			asst = &recorded[i]
			break
		}
	}
	if asst == nil {
		t.Fatal("no assistant message recorded")
	}
	var hasServerUse, hasResult bool
	for _, c := range asst.Content {
		switch c.Type {
		case llm.ContentTypeServerToolUse:
			hasServerUse = true
		case llm.ContentTypeWebSearchResult:
			hasResult = true
		}
	}
	if !hasServerUse || !hasResult {
		t.Fatalf("merged assistant message missing pair: serverUse=%v result=%v, content=%+v", hasServerUse, hasResult, asst.Content)
	}

	// The merged message must end the turn (no client tools to run).
	if !asst.EndOfTurn {
		t.Errorf("merged assistant message should be EndOfTurn")
	}

	// Usage comes directly from the single native Responses result.
	found := false
	for _, u := range recordedUsage {
		if u.InputTokens == 30 && u.OutputTokens == 13 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected summed usage input=30 output=13 in %+v", recordedUsage)
	}
}

func TestUserFacingLLMError(t *testing.T) {
	idleErr := fmt.Errorf("stream: %w after 3m0s: context canceled", llmhttp.ErrIdleTimeout)

	// Idle/stall timeout is explained, not surfaced as a raw deadline error.
	msg := userFacingLLMError(idleErr, nil)
	if !strings.Contains(msg, "idle/stall timeout") {
		t.Errorf("idle error message missing explanation: %q", msg)
	}
	if strings.Contains(msg, "context deadline exceeded") {
		t.Errorf("idle error message should not surface opaque deadline text: %q", msg)
	}
	if !strings.Contains(msg, llmhttp.DefaultIdleTimeout.String()) {
		t.Errorf("idle error message missing timeout duration %s: %q", llmhttp.DefaultIdleTimeout, msg)
	}

	// A non-idle error falls through to its raw text.
	generic := userFacingLLMError(fmt.Errorf("boom"), nil)
	if !strings.Contains(generic, "boom") {
		t.Errorf("generic error message = %q, want it to contain the cause", generic)
	}

	// Trace ids are appended when present.
	ctx, trace := llmhttp.WithRequestTrace(context.Background())
	_ = ctx
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req_abc")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := llmhttp.NewClient(nil).Do(req)
	if err != nil {
		t.Fatalf("seed request: %v", err)
	}
	resp.Body.Close()

	withIDs := userFacingLLMError(idleErr, trace)
	if !strings.Contains(withIDs, "req_abc") {
		t.Errorf("error message missing upstream request id: %q", withIDs)
	}
	if !strings.Contains(withIDs, trace.ShelleyRequestID()) {
		t.Errorf("error message missing shelley request id: %q", withIDs)
	}
}

// TestToolOtherUsageAttachedToToolResult verifies that executeToolCalls
// installs a usage collector in the tool ctx and attaches collected entries
// (from indirect LLM calls made by tools, e.g. keyword_search) to the
// tool-result message record.
func TestToolOtherUsageAttachedToToolResult(t *testing.T) {
	testTool := dtool.Func{
		Spec: dtool.Definition{Name: "bash", Description: "A test tool that makes an indirect model call", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)},
		Run: func(context.Context, json.RawMessage, dtool.Runtime) (dtool.Result, error) {
			return dtool.Result{
				Content: []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: "ok"}},
				OtherUsage: []dmessage.PurposedUsage{{Purpose: "keyword_search", Usage: dmessage.Usage{
					InputTokens: 42, OutputTokens: 7, CostUSD: 0.001, Model: "m1",
				}}},
			}, nil
		},
	}

	var mu sync.Mutex
	type recorded struct {
		message    llm.Message
		otherUsage []llm.PurposedUsage
	}
	var records []recorded
	loop := NewLoop(Config{
		Model: NewPredictableService(),
		Tools: []dtool.Tool{testTool},
		RecordMessage: func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error {
			mu.Lock()
			defer mu.Unlock()
			records = append(records, recorded{message, otherUsage})
			return nil
		},
	})

	loop.QueueUserMessage(llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "bash: echo hello"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := loop.Go(ctx); err != context.DeadlineExceeded {
		t.Errorf("expected context deadline exceeded, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var toolResult *recorded
	for i := range records {
		for _, c := range records[i].message.Content {
			if c.Type == llm.ContentTypeToolResult {
				toolResult = &records[i]
			}
		}
	}
	if toolResult == nil {
		t.Fatal("no tool-result message recorded")
	}
	if len(toolResult.otherUsage) != 1 {
		t.Fatalf("tool-result otherUsage = %+v, want 1 entry", toolResult.otherUsage)
	}
	e := toolResult.otherUsage[0]
	if e.Purpose != "keyword_search" || e.InputTokens != 42 || e.Model != "m1" {
		t.Errorf("entry = %+v", e)
	}
	// Non-tool-result messages must not carry other usage.
	for i := range records {
		if &records[i] != toolResult && records[i].otherUsage != nil {
			t.Errorf("unexpected otherUsage on message %d: %+v", i, records[i].otherUsage)
		}
	}
}
