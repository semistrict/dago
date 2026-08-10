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
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

func TestDeepAgentDefaultVerticalSlice(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	script := modeltest.New(model.Profile{ToolCalling: true, ContextWindow: 10000},
		modeltest.Step{Check: func(request model.Request) error {
			names := map[string]bool{}
			for _, definition := range request.Tools {
				names[definition.Name] = true
			}
			for _, required := range []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "task", "compact_conversation"} {
				if !names[required] {
					return errors.New("missing tool " + required)
				}
			}
			if names["write_todos"] {
				return errors.New("write_todos should be opt-in")
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

func TestDeepAgentPlanningIsOptInAndPromptCachingIsAutomatic(t *testing.T) {
	script := modeltest.New(model.Profile{SupportsPromptCaching: true}, modeltest.Step{Check: func(request model.Request) error {
		if request.PromptCache == nil || request.PromptCache.Key != "cache-thread" || request.PromptCache.Retention != "24h" {
			return fmt.Errorf("prompt cache = %#v", request.PromptCache)
		}
		foundTodo := false
		for _, definition := range request.Tools {
			foundTodo = foundTodo || definition.Name == "write_todos"
		}
		if !foundTodo {
			return errors.New("opt-in planning tool missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{
		Model: script, EnableTodo: true, PromptCacheRetention: "24h",
		DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{
		Config: checkpoint.Config{ThreadID: "cache-thread"}, Messages: []message.Message{message.Human("go")},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeepAgentInterruptOnWiresHumanApproval(t *testing.T) {
	danger := tool.Func{Spec: tool.Definition{Name: "danger", Description: "dangerous action", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("ran"), nil
	}}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "danger-1", Name: "danger", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []tool.Tool{danger}, Saver: checkpoint.NewMemorySaver(),
		DisableSubagents: true, DisableSummary: true,
		InterruptOn: []agent.ApprovalRule{{Pattern: "danger", Description: "Allow danger?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "generic-approval"}
	paused, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || paused.Interrupts[0].ID != "human_approval" {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Resume: agent.ApprovalResponse{Decisions: map[string]agent.ApprovalChoice{
		"danger-1": {Decision: agent.ApprovalApprove},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("messages = %#v", resumed.Messages)
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
	compiled, err := New(Options{Model: parentModel, Subagents: []Subagent{{Name: "special", Description: "Specialized", Runnable: child, InheritedState: []string{agent.MessagesKey}}}, DisableSummary: true})
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

func TestDeclarativeSubagentInheritsToolsAndUsesOwnModelAndPrompt(t *testing.T) {
	lookup := tool.Func{Spec: tool.Definition{Name: "lookup", Description: "look up a value", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("lookup result"), nil
	}}
	childModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[0].Role != message.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "specialist instructions") {
				return errors.New("declarative system prompt missing")
			}
			if request.Messages[len(request.Messages)-1].TextContent() != "research this" {
				return errors.New("parent conversation leaked into declarative subagent")
			}
			names := map[string]bool{}
			for _, definition := range request.Tools {
				names[definition.Name] = true
			}
			if !names["lookup"] || names["task"] {
				return fmt.Errorf("declarative tools = %#v", names)
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "lookup-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "lookup result" {
				return errors.New("inherited tool result missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("child complete")}},
	)
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "delegate-declarative", Name: "task", Arguments: json.RawMessage(`{"description":"research this","subagent_type":"specialist"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "child complete" {
				return errors.New("declarative result missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("parent complete")}},
	)
	compiled, err := New(Options{
		Model: parentModel, Tools: []tool.Tool{lookup}, DisableSummary: true,
		Subagents: []Subagent{{
			Name: "specialist", Description: "Research specialist", SystemPrompt: "specialist instructions", Model: childModel,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("parent context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent complete" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestDeclarativeSubagentPropagatesNonMessageState(t *testing.T) {
	files, err := backend.NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	childModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "declarative-write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/declarative.txt","content":"from declarative child"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("created")}},
	)
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "delegate-writer", Name: "task", Arguments: json.RawMessage(`{"description":"create a file","subagent_type":"writer"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "read-written", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/declarative.txt"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "from declarative child") {
				return errors.New("declarative state update was not propagated")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	parent, err := New(Options{
		Model: parentModel, Backend: files, DisableSummary: true,
		Subagents: []Subagent{{
			Name: "writer", Description: "Writes files", SystemPrompt: "Write the requested file.", Model: childModel,
		}},
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

func TestPrivateParentStateDoesNotReachDeclarativeSubagent(t *testing.T) {
	childModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("child done")}})
	guard := agent.Middleware{Name: "private_guard", BeforeModel: func(_ context.Context, values state.Values, _ agent.Runtime) (state.Values, error) {
		if _, exists := values["parent_secret"]; exists {
			return nil, errors.New("private parent state leaked to child")
		}
		return nil, nil
	}}
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "private-task", Name: "task", Arguments: json.RawMessage(`{"description":"work","subagent_type":"worker"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: parentModel, DisableSummary: true,
		StateFields: map[string]agent.StateField{"parent_secret": {Kind: agent.FieldLast, Contract: "parent.secret.v1", Private: true, Clone: func(value any) any { return value }}},
		Subagents:   []Subagent{{Name: "worker", Description: "Works", SystemPrompt: "Work.", Model: childModel, Middleware: []agent.Middleware{guard}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}, State: state.Values{"parent_secret": "hidden"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := result.State["parent_secret"]; exists {
		t.Fatalf("private state leaked from result: %#v", result.State)
	}
}

func TestDeclarativeSubagentApprovalInterruptResumesChild(t *testing.T) {
	dangerRuns := 0
	danger := tool.Func{Spec: tool.Definition{Name: "danger", Description: "dangerous child action", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		dangerRuns++
		return tool.TextResult("ran"), nil
	}}
	childModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "child-danger", Name: "danger", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("child approved")}},
	)
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "delegate-approval", Name: "task", Arguments: json.RawMessage(`{"description":"perform action","subagent_type":"operator"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "child approved" {
				return errors.New("resumed child result missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("parent done")}},
	)
	saver := checkpoint.NewMemorySaver()
	compiled, err := New(Options{
		Model: parentModel, Tools: []tool.Tool{danger}, Saver: saver, DisableSummary: true,
		InterruptOn: []agent.ApprovalRule{{Pattern: "danger", Description: "Allow danger?"}},
		Subagents: []Subagent{{
			Name: "operator", Description: "Performs approved actions", SystemPrompt: "Perform the action.", Model: childModel,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "nested-approval"}
	paused, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("delegate")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || dangerRuns != 0 {
		t.Fatalf("paused = %#v, danger runs = %d", paused.Interrupts, dangerRuns)
	}
	resumed, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Resume: agent.ApprovalResponse{Decisions: map[string]agent.ApprovalChoice{
		"child-danger": {Decision: agent.ApprovalApprove},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if dangerRuns != 1 || resumed.Messages[len(resumed.Messages)-1].TextContent() != "parent done" {
		t.Fatalf("danger runs = %d, messages = %#v", dangerRuns, resumed.Messages)
	}
}

func TestApprovalConfigurationRequiresCheckpointer(t *testing.T) {
	_, err := New(Options{
		Model: modeltest.New(model.Profile{}), DisableSubagents: true, DisableSummary: true,
		InterruptOn: []agent.ApprovalRule{{Pattern: "danger"}},
	})
	if err == nil || !strings.Contains(err.Error(), "checkpointer") {
		t.Fatalf("error = %v", err)
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
