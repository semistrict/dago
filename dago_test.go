package dago

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/checkpoint"
	checkpointsqlite "github.com/semistrict/dago/checkpoint/sqlite"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/store"
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
			for _, required := range []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "task"} {
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

func TestDeepAgentBindsRuntimeScopedStoreBackend(t *testing.T) {
	values := store.NewMemory()
	files, err := backend.NewStoreWithOptions(backend.StoreOptions{Namespace: func(runtime *backend.Runtime) (store.Namespace, error) {
		user, _ := runtime.Context.(string)
		return store.Namespace{"files", user}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/note.txt","content":"private"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Backend: files, Store: values, Context: "alice", DisableSubagents: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("write")}}); err != nil {
		t.Fatal(err)
	}
	item, err := values.Get(context.Background(), store.Namespace{"files", "alice"}, "/note.txt")
	if err != nil || item == nil || item.Value["content"] != "private" {
		t.Fatalf("runtime-scoped store item = %#v, %v", item, err)
	}
}

func TestDeepAgentStreamProjectsNestedSubagentLifecycle(t *testing.T) {
	echo := tool.Func{Spec: tool.Definition{
		Name: "echo", Description: "Echo text", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
	}, Run: func(_ context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return tool.Result{}, err
		}
		return tool.TextResult(input.Value), nil
	}}
	child := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "child-echo", Name: "echo", Arguments: json.RawMessage(`{"value":"nested"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("child done")}},
	)
	parent := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "parent-task", Name: "task", Arguments: json.RawMessage(`{"description":"do work","subagent_type":"worker"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("parent done")}},
	)
	compiled, err := New(Options{
		Model: parent, DisableSummary: true,
		Subagents: []Subagent{{Name: "worker", Description: "Worker", SystemPrompt: "Work.", Model: child, Tools: []tool.Tool{echo}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := compiled.Stream(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}, 32)
	defer stream.Close()
	var childEvents []agent.ChildEvent
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Mode == agent.EventChild && event.Child != nil {
			childEvents = append(childEvents, *event.Child)
		}
	}
	if _, err := stream.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(childEvents) < 3 || childEvents[0].Phase != agent.ChildStarted || childEvents[len(childEvents)-1].Phase != agent.ChildCompleted {
		t.Fatalf("child lifecycle = %#v", childEvents)
	}
	terminal := childEvents[len(childEvents)-1]
	if terminal.Name != "worker" || terminal.ToolCallID != "parent-task" || len(terminal.Messages) == 0 || terminal.Messages[len(terminal.Messages)-1].TextContent() != "child done" {
		t.Fatalf("child terminal event = %#v", terminal)
	}
	foundToolUpdate := false
	for _, childEvent := range childEvents {
		if childEvent.Event != nil && childEvent.Event.Node == "tools" {
			foundToolUpdate = true
		}
	}
	if !foundToolUpdate {
		t.Fatalf("nested tool events were not projected: %#v", childEvents)
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

func TestMainPlanningOptInDoesNotLeakIntoGeneralSubagent(t *testing.T) {
	hasTodo := func(request model.Request) bool {
		for _, definition := range request.Tools {
			if definition.Name == "write_todos" {
				return true
			}
		}
		return false
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request model.Request) error {
			if !hasTodo(request) {
				return errors.New("main planning tool missing")
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{
			ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"work","subagent_type":"general-purpose"}`),
		}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if hasTodo(request) {
				return errors.New("main planning opt-in leaked into general subagent")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("child done")}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("parent done")}},
	)
	compiled, err := New(Options{Model: script, EnableTodo: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}}); err != nil {
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

func TestExplicitApprovalOverridesFilesystemPermissionApproval(t *testing.T) {
	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/secret.txt": {Content: "backend secret", Encoding: backend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/secret.txt"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.TextContent() != "reviewer supplied" || last.ToolStatus != message.ToolStatusSuccess {
				return fmt.Errorf("tool response = %#v", last)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Backend: memory, Saver: checkpoint.NewMemorySaver(),
		DisableSubagents: true, DisableSummary: true,
		Permissions: []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/secret.txt"}, Mode: PermissionInterrupt}},
		InterruptOn: []agent.ApprovalRule{{Pattern: "read_file", AllowedDecisions: []agent.ApprovalDecision{agent.ApprovalRespond}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "approval-precedence"}
	paused, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || paused.Interrupts[0].ID != "human_approval" {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Resume: agent.ApprovalResponse{Decisions: map[string]agent.ApprovalChoice{
		"read": {Decision: agent.ApprovalRespond, Message: "reviewer supplied"},
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

func TestExplicitEmptyMemoryAndSkillsStillInstallMiddleware(t *testing.T) {
	script := modeltest.New(model.Profile{Provider: "anthropic", SupportsPromptCaching: true}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) == 0 || request.Messages[0].Role != message.RoleSystem {
			return errors.New("system prompt missing")
		}
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "Available Skills") || !strings.Contains(prompt, "No skills available") || !strings.Contains(prompt, "(No memory loaded)") {
			return fmt.Errorf("empty middleware prompts missing: %q", prompt)
		}
		if len(request.Messages[0].Content) < 2 {
			return fmt.Errorf("system content blocks = %#v", request.Messages[0].Content)
		}
		for index, block := range request.Messages[0].Content[len(request.Messages[0].Content)-2:] {
			if _, ok := block.Extra["cache_control"]; !ok {
				return fmt.Errorf("cache breakpoint %d missing: %#v", index, block)
			}
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{
		Model: script, Skills: []string{}, Memory: []string{}, DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredSystemMessagePreservesBlocksAndAppendsProfilePrompt(t *testing.T) {
	system := message.Message{Role: message.RoleSystem, Content: []message.ContentBlock{
		{Type: message.BlockText, Text: "first", Extra: map[string]json.RawMessage{"provider_field": json.RawMessage(`true`)}},
		{Type: message.BlockText, Text: "second"},
	}}
	script := modeltest.New(model.Profile{Provider: "anthropic", SupportsPromptCaching: true}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) == 0 || request.Messages[0].Role != message.RoleSystem {
			return errors.New("structured system message missing")
		}
		blocks := request.Messages[0].Content
		if len(blocks) != 3 || blocks[0].Text != "first" || blocks[1].Text != "second" || blocks[2].Text != "\n\nprofile fragment" {
			return fmt.Errorf("system blocks = %#v", blocks)
		}
		if string(blocks[0].Extra["provider_field"]) != "true" {
			return fmt.Errorf("provider metadata = %#v", blocks[0].Extra)
		}
		if _, ok := blocks[1].Extra["cache_control"]; ok {
			return errors.New("cache breakpoint applied before the final block")
		}
		if _, ok := blocks[2].Extra["cache_control"]; !ok {
			return errors.New("final profile block lacks cache breakpoint")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{
		Model: script, SystemMessage: &system, Profiles: []Profile{{SystemPrompt: "profile fragment"}},
		DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
	if len(system.Content) != 2 || system.Content[1].Extra != nil {
		t.Fatalf("New mutated source system message: %#v", system)
	}
}

func TestStructuredSystemMessageValidation(t *testing.T) {
	chat := modeltest.New(model.Profile{})
	human := message.Human("not system")
	if _, err := New(Options{Model: chat, SystemMessage: &human}); err == nil {
		t.Fatal("expected non-system message to fail")
	}
	system := message.System("system")
	if _, err := New(Options{Model: chat, SystemPrompt: "string", SystemMessage: &system}); err == nil {
		t.Fatal("expected conflicting system inputs to fail")
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

func TestDeclarativeSubagentStructuredResponseWinsAndEmptyToolsOverrideInheritance(t *testing.T) {
	parentOnly := tool.Func{Spec: tool.Definition{Name: "parent_only", Description: "parent tool", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("parent"), nil
	}}
	childModel := modeltest.New(model.Profile{StructuredOutput: true}, modeltest.Step{Check: func(request model.Request) error {
		for _, definition := range request.Tools {
			if definition.Name == "parent_only" {
				return errors.New("explicit empty child tools did not override inheritance")
			}
		}
		if request.ResponseFormat == nil || request.ResponseFormat.Name != "findings" {
			return errors.New("child structured response format missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("fallback text"), Structured: json.RawMessage(`{"answer":"structured"}`)}})
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "structured-task", Name: "task", Arguments: json.RawMessage(`{"description":"analyze","subagent_type":"analyst"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != `{"answer":"structured"}` {
				return errors.New("structured child response did not win")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: parentModel, Tools: []tool.Tool{parentOnly}, DisableSummary: true,
		Subagents: []Subagent{{
			Name: "analyst", Description: "Analyzes", SystemPrompt: "Analyze.", Model: childModel, Tools: []tool.Tool{},
			StructuredOutput: &agent.StructuredOutput{Name: "findings", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestSubagentInvocationPropagatesCancellation(t *testing.T) {
	childModel := &blockingChat{started: make(chan struct{})}
	parentModel := modeltest.New(model.Profile{ToolCalling: true}, modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "cancel-task", Name: "task", Arguments: json.RawMessage(`{"description":"block","subagent_type":"worker"}`)}}}}})
	compiled, err := New(Options{
		Model: parentModel, DisableSummary: true,
		Subagents: []Subagent{{Name: "worker", Description: "Blocks", SystemPrompt: "Wait.", Model: childModel}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := compiled.Invoke(ctx, agent.Input{Messages: []message.Message{message.Human("go")}})
		done <- err
	}()
	select {
	case <-childModel.started:
		cancel()
	case <-ctx.Done():
		t.Fatal("child model did not start")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

type blockingChat struct{ started chan struct{} }

func (chat *blockingChat) Invoke(ctx context.Context, _ model.Request) (model.Response, error) {
	close(chat.started)
	<-ctx.Done()
	return model.Response{}, ctx.Err()
}

func (*blockingChat) Stream(context.Context, model.Request) (model.Stream, error) {
	return model.EmptyStream{}, nil
}

func (*blockingChat) Profile() model.Profile { return model.Profile{} }

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

func TestSummarizationPreservesRawHistoryAndOffloads(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{ContextWindow: 100}, modeltest.Step{Response: model.Response{Message: message.Assistant("facts")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "conversation that has been summarized") {
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
	if len(result.Messages) != 5 || result.Messages[0].TextContent() != "old one" {
		t.Fatalf("messages = %#v", result.Messages)
	}
	glob, err := memory.Glob(context.Background(), "**/*.md", "/conversation_history")
	if err != nil || len(glob.Matches) != 1 {
		t.Fatalf("history files = %#v, %v", glob, err)
	}
}

func TestSummarizationEventSurvivesSQLiteReplay(t *testing.T) {
	saver, err := checkpointsqlite.Open(filepath.Join(t.TempDir(), "summaries.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("durable summary")}})
	firstModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "durable summary") {
			return fmt.Errorf("first effective messages = %#v", request.Messages)
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("first response")}})
	firstMiddleware, err := SummarizationMiddleware(SummarizationOptions{Model: summaryModel, Backend: memory, TriggerMessages: 4, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := agent.New(agent.Options{Model: firstModel, Middleware: []agent.Middleware{firstMiddleware}, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "summary-replay"}
	initial := []message.Message{message.Human("old one"), message.Assistant("old two"), message.Human("recent one"), message.Assistant("recent two")}
	if _, err := first.Invoke(context.Background(), agent.Input{Config: config, Messages: initial}); err != nil {
		t.Fatal(err)
	}

	secondModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 5 || !strings.Contains(request.Messages[0].TextContent(), "durable summary") || strings.Contains(request.Messages[0].TextContent(), "old one") {
			return fmt.Errorf("replayed effective messages = %#v", request.Messages)
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("second response")}})
	unusedSummaryModel := modeltest.New(model.Profile{})
	secondMiddleware, err := SummarizationMiddleware(SummarizationOptions{Model: unusedSummaryModel, Backend: memory, TriggerMessages: 100, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.New(agent.Options{Model: secondModel, Middleware: []agent.Middleware{secondMiddleware}, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := second.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("continue")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 7 || result.Messages[0].TextContent() != "old one" {
		t.Fatalf("raw replayed messages = %#v", result.Messages)
	}
}

func TestSummarizationOffloadsLargeOldMedia(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "/conversation_history/media/") {
			return errors.New("large old media was not replaced with a reference")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("media summary")}})
	mainModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 1,
		KeepMessages: 1,
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
	files, err := memory.Glob(context.Background(), "**/*.png", "/conversation_history/media")
	if err != nil || len(files.Matches) != 1 {
		t.Fatalf("offloaded media = %#v, %v", files, err)
	}
}

func TestSummarizationRetriesContextOverflowWithCompactedHistory(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("fallback facts")}})
	mainModel := modeltest.New(model.Profile{},
		modeltest.Step{Error: model.ErrContextOverflow},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "fallback facts") {
				return fmt.Errorf("retry messages = %#v", request.Messages)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("recovered")}},
	)
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 1_000_000, KeepMessages: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{Model: mainModel, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{
		message.Human("old one"), message.Assistant("old two"), message.Human("recent one"), message.Assistant("recent two"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 5 || result.Messages[len(result.Messages)-1].TextContent() != "recovered" {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestSummarizationClipsTrailingToolBatchOnOverflow(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("summary")}})
	mainModel := modeltest.New(model.Profile{},
		modeltest.Step{Error: model.ErrContextOverflow},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) != 4 {
				return fmt.Errorf("retry messages = %#v", request.Messages)
			}
			for _, item := range request.Messages[len(request.Messages)-2:] {
				if !strings.Contains(item.TextContent(), "Tool result too large") || !strings.Contains(item.TextContent(), "/large_tool_results/") {
					return fmt.Errorf("tool tail was not clipped: %#v", item)
				}
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("recovered")}},
	)
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 1_000_000, KeepMessages: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.New(agent.Options{Model: mainModel, Middleware: []agent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	toolCalls := message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
		{ID: "lookup/one", Name: "lookup", Arguments: json.RawMessage(`{}`)},
		{ID: "lookup.two", Name: "lookup", Arguments: json.RawMessage(`{}`)},
	}}
	first := message.Tool("lookup/one", strings.Repeat("first line\n", 1_200))
	first.ID = "result-one"
	second := message.Tool("lookup.two", strings.Repeat("second line\n", 1_200))
	second.ID = "result-two"
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{
		message.Human("old"), message.Assistant("old answer"), message.Human("lookup"), toolCalls, first, second,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "recovered" {
		t.Fatalf("messages = %#v", result.Messages)
	}
	for _, filePath := range []string{"/large_tool_results/lookup_one", "/large_tool_results/lookup_two"} {
		read, readErr := memory.Read(context.Background(), filePath, 0, 10_000)
		if readErr != nil || read.Data == nil || len(read.Data.Content) < 10_000 {
			t.Fatalf("offloaded %s = %#v, %v", filePath, read, readErr)
		}
	}
}

func TestSummarizationPersistsOverflowArtifactsInStateBackend(t *testing.T) {
	files, err := backend.NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	summaryModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("summary")}})
	mainModel := modeltest.New(model.Profile{},
		modeltest.Step{Error: model.ErrContextOverflow},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if !strings.Contains(last.TextContent(), "Tool result too large") || !strings.Contains(last.TextContent(), "/large_tool_results/lookup_state") {
				return fmt.Errorf("tool result was not replaced with a durable reference: %#v", last)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("recovered")}},
	)
	compiled, err := New(Options{
		Model: mainModel, Backend: files, DisableSubagents: true,
		Summarization: SummarizationOptions{
			Model: summaryModel, TriggerTokens: 1_000_000, KeepMessages: 2, OverflowClipTokens: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{
		ID: "lookup/state", Name: "lookup", Arguments: json.RawMessage(`{}`),
	}}}
	resultMessage := message.Tool("lookup/state", strings.Repeat("state-backed result\n", 1_200))
	result, err := compiled.Invoke(context.Background(), agent.Input{
		Config: checkpoint.Config{ThreadID: "overflow-state"},
		Messages: []message.Message{
			message.Human("old question"), message.Assistant("old answer"), message.Human("lookup"), call, resultMessage,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	boundCtx, err := backend.BindRuntime(context.Background(), files, result.State)
	if err != nil {
		t.Fatal(err)
	}
	for _, filePath := range []string{"/large_tool_results/lookup_state", "/conversation_history/overflow-state.md"} {
		read, readErr := files.Read(boundCtx, filePath, 0, 100_000)
		if readErr != nil || read.Data == nil || read.Data.Content == "" {
			t.Fatalf("persisted %s = %#v, %v", filePath, read, readErr)
		}
	}
}

func TestOverflowClipKeepsReadFileAtOriginalPath(t *testing.T) {
	memory, _ := backend.NewMemory(nil)
	content := strings.Repeat("content line\n", 2_000)
	call := message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{
		ID: "read-one", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/source.txt"}`),
	}}}
	resultMessage := message.Tool("read-one", content)
	clipped := clipOverflowToolTail(context.Background(), []message.Message{call, resultMessage}, []message.Message{call, resultMessage}, SummarizationOptions{
		Backend: memory, OverflowClipTokens: 1, LargeToolResultsRoot: "/large_tool_results",
	})
	if len(clipped) != 2 || !strings.Contains(clipped[1].TextContent(), "The full content is at /source.txt") || len(clipped[1].TextContent()) >= len(content) {
		t.Fatalf("clipped messages = %#v", clipped)
	}
	files, err := memory.Glob(context.Background(), "**/*", "/large_tool_results")
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Matches) != 0 {
		t.Fatalf("read_file result was redundantly offloaded: %#v", files.Matches)
	}
}
