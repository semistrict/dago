package dago

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
)

func TestDeepAgentDefaultVerticalSlice(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	script := modeltest.New(model.Profile{ToolCalling: true, ContextWindow: 10000},
		modeltest.Step{Check: func(request model.Request) error {
			names := map[string]bool{}
			for _, definition := range request.Tools {
				names[definition.Name] = true
			}
			for _, required := range []string{"write_todos", "ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "task", "compact_conversation"} {
				if !names[required] {
					return errors.New("missing tool " + required)
				}
			}
			if names["execute"] {
				return errors.New("execute exposed without sandbox")
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/note.txt","content":"hello"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/note.txt"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "1  hello") {
				return errors.New("read result missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Backend: memory})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("make a note")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestDeepAgentRepairsDanglingToolCallsBeforeModel(t *testing.T) {
	script := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 4 {
			return fmt.Errorf("messages = %#v", request.Messages)
		}
		result := request.Messages[2]
		if result.Role != message.RoleTool || result.ToolCallID != "call-1" || result.Name != "lookup" || result.ToolStatus != message.ToolStatusError {
			return fmt.Errorf("patched tool result = %#v", result)
		}
		if !strings.Contains(result.TextContent(), "was cancelled") {
			return fmt.Errorf("patched tool result text = %q", result.TextContent())
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("continued")}})
	compiled, err := New(Options{Model: script, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{
		message.Human("start"),
		{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
		message.Human("continue without running it"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "continued" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestDefaultStateBackendPersistsParallelWritesPerThread(t *testing.T) {
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			{ID: "a", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/a.txt","content":"alpha"}`)},
			{ID: "b", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/b.txt","content":"beta"}`)},
		}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("written")}},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/a.txt"}`)},
		}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "alpha") {
				return errors.New("checkpointed state file missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("read")}},
	)
	compiled, err := New(Options{Model: script, DisableSubagents: true, DisableSummary: true, Saver: checkpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "state-files"}
	first, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("write")}})
	if err != nil {
		t.Fatal(err)
	}
	files, ok := first.State["files"].(map[string]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %#v", first.State["files"])
	}
	second, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("read")}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Messages[len(second.Messages)-1].TextContent() != "read" {
		t.Fatalf("result = %#v", second.Messages)
	}
}

func TestFilesystemPermissionDenyAndApproval(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	_, _ = memory.Write(context.Background(), "/public.txt", "public")
	_, _ = memory.Write(context.Background(), "/secret.txt", "secret")
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			{ID: "deny", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/secret.txt"}`)},
			{ID: "ask", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/public.txt","content":"changed"}`)},
		}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-2].ToolStatus != message.ToolStatusError {
				return errors.New("deny result missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Backend: memory, DisableSubagents: true, DisableSummary: true, Permissions: []FilesystemPermission{
		{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/secret.txt"}, Mode: PermissionDeny},
		{Operations: []FilesystemOperation{FilesystemWrite}, Paths: []string{"/public.txt"}, Mode: PermissionAsk},
	}, Saver: checkpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "permissions"}
	paused, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Resume: agent.ApprovalResponse{Decisions: map[string]agent.ApprovalChoice{"ask": {Decision: agent.ApprovalApprove}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("resumed = %#v", resumed.Messages)
	}
}

func TestMemoryAndSkillsPromptInjection(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	_, _ = memory.Write(context.Background(), "/AGENTS.md", "<!-- hidden -->Remember blue.")
	_, _ = memory.Write(context.Background(), "/skills/research/SKILL.md", "---\nname: research\ndescription: Research carefully\nallowed-tools: read_file, grep\n---\nInstructions")
	script := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) == 0 || request.Messages[0].Role != message.RoleSystem {
			return errors.New("system prompt missing")
		}
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "Remember blue") || strings.Contains(prompt, "hidden") || !strings.Contains(prompt, "research") || !strings.Contains(prompt, "/skills/research/SKILL.md") {
			return errors.New("memory or skill prompt missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{Model: script, Backend: memory, Memory: []string{"/AGENTS.md"}, Skills: []string{"/skills"}, DisableSubagents: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentTaskUsesIsolatedInput(t *testing.T) {
	childModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 1 || request.Messages[0].TextContent() != "child work" {
			return errors.New("child input leaked")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("child result")}})
	child, err := agent.New(agent.Options{Model: childModel})
	if err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(model.Profile{ToolCalling: true}, modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "task", Name: "task", Arguments: json.RawMessage(`{"description":"child work","subagent_type":"special"}`)}}}}}, modeltest.Step{Check: func(request model.Request) error {
		if request.Messages[len(request.Messages)-1].TextContent() != "child result" {
			return errors.New("child result missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("parent done")}})
	compiled, err := New(Options{Model: parentModel, Subagents: []Subagent{{Name: "special", Description: "Specialized", Runnable: child}}, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("parent context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent done" {
		t.Fatal("parent did not finish")
	}
}

func TestSubagentCanPropagateSelectedStateWithoutMessageLeak(t *testing.T) {
	files, err := backend.NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	childModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) != 1 || request.Messages[0].TextContent() != "create child file" {
				return errors.New("parent messages leaked into child")
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "child-write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/child.txt","content":"from child"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("created")}},
	)
	child, err := New(Options{Model: childModel, Backend: files, DisableSubagents: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"create child file","subagent_type":"writer"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "parent-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/child.txt"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "from child") {
				return errors.New("selected child state was not propagated")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	parent, err := New(Options{
		Model: parentModel, Backend: files, DisableSummary: true,
		Subagents: []Subagent{{Name: "writer", Description: "Writes files", Runnable: child, InheritedState: []string{"files"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := parent.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("parent-only context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestSummarizationOverwritesOlderHistoryAndOffloads(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{ContextWindow: 100}, modeltest.Step{Response: model.Response{Message: message.Assistant("facts")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "Summary of earlier") {
			return errors.New("compacted history missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{Model: summaryModel, Backend: memory, TriggerTokens: 1, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{Model: mainModel, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	messages := []message.Message{message.Human("old one"), message.Assistant("old two"), message.Human("recent one"), message.Assistant("recent two")}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("messages = %#v", result.Messages)
	}
	glob, err := memory.Glob(context.Background(), "**/*.md", "/conversation_history")
	if err != nil || len(glob.Matches) != 1 {
		t.Fatalf("history files = %#v, %v", glob, err)
	}
}

func TestSummarizationOffloadsLargeOldMedia(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 2 || len(request.Messages[0].Content) != 1 {
			return errors.New("unexpected prepared message history")
		}
		block := request.Messages[0].Content[0]
		if block.Type != message.BlockText || !strings.Contains(block.Text, "/conversation_media/") || len(block.Data) != 0 {
			return errors.New("large old media was not replaced with a reference")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: mainModel, Backend: memory, TriggerTokens: 1_000_000,
		KeepMessages: 1, MediaOffloadBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{Model: mainModel, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	old := message.Message{Role: message.RoleHuman, Content: []message.ContentBlock{{Type: message.BlockImage, MIMEType: "image/png", Name: "sample.png", Data: []byte("large-image")}}}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{old, message.Human("recent")}}); err != nil {
		t.Fatal(err)
	}
	files, err := memory.Glob(context.Background(), "**/*.png", "/conversation_media")
	if err != nil || len(files.Matches) != 1 {
		t.Fatalf("offloaded media = %#v, %v", files, err)
	}
}
