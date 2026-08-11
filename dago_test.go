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

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
)

func TestDeepAgentDefaultVerticalSlice(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	script := modeltest.New(damodel.Profile{ToolCalling: true, ContextWindow: 10000},
		modeltest.Step{Check: func(request damodel.Request) error {
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
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/note.txt","content":"hello"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/note.txt"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "1  hello") {
				return errors.New("read result missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Backend: memory})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("make a note")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestDeepAgentAddsConstructionMetadataAndTags(t *testing.T) {
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Metadata) != 0 || len(request.Tags) != 0 {
			return fmt.Errorf("construction metadata leaked into provider request: metadata=%#v tags=%#v", request.Metadata, request.Tags)
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	inspected := false
	metadataMiddleware := dagent.Middleware{Name: "inspect_invocation_metadata", WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
		if string(request.InvocationMetadata["ls_integration"]) != `"deepagents"` || string(request.InvocationMetadata["lc_agent_name"]) != `"researcher"` || string(request.InvocationMetadata["tenant"]) != `"alpha"` {
			return dagent.ModelResponse{}, fmt.Errorf("invocation metadata = %#v", request.InvocationMetadata)
		}
		if len(request.InvocationTags) != 1 || request.InvocationTags[0] != "integration" {
			return dagent.ModelResponse{}, fmt.Errorf("invocation tags = %#v", request.InvocationTags)
		}
		inspected = true
		return next(ctx, request)
	}}
	compiled, err := New(Options{
		Name: "researcher", Model: script, DisableSubagents: true, DisableSummary: true,
		Metadata: map[string]json.RawMessage{"tenant": json.RawMessage(`"alpha"`)}, Tags: []string{"integration"}, Middleware: []dagent.Middleware{metadataMiddleware},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if !inspected {
		t.Fatal("invocation metadata middleware was not called")
	}
}

func TestDeepAgentBindsRuntimeScopedStoreBackend(t *testing.T) {
	values := dastore.NewMemory()
	files, err := dabackend.NewStoreWithOptions(dabackend.StoreOptions{Namespace: func(runtime *dabackend.Runtime) (dastore.Namespace, error) {
		user, _ := runtime.Context.(string)
		return dastore.Namespace{"files", user}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/note.txt","content":"private"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Backend: files, Store: values, Context: "alice", DisableSubagents: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("write")}}); err != nil {
		t.Fatal(err)
	}
	item, err := values.Get(context.Background(), dastore.Namespace{"files", "alice"}, "/note.txt")
	if err != nil || item == nil || item.Value["content"] != "private" {
		t.Fatalf("runtime-scoped store item = %#v, %v", item, err)
	}
}

func TestDeepAgentStreamProjectsNestedSubagentLifecycle(t *testing.T) {
	echo := datool.Func{Spec: datool.Definition{
		Name: "echo", Description: "Echo text", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
	}, Run: func(_ context.Context, raw json.RawMessage, _ datool.Runtime) (datool.Result, error) {
		var input struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return datool.Result{}, err
		}
		return datool.TextResult(input.Value), nil
	}}
	child := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "child-echo", Name: "echo", Arguments: json.RawMessage(`{"value":"nested"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("child done")}},
	)
	parent := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "parent-task", Name: "task", Arguments: json.RawMessage(`{"description":"do work","subagent_type":"worker"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("parent done")}},
	)
	compiled, err := New(Options{
		Model: parent, DisableSummary: true,
		Subagents: []Subagent{{Name: "worker", Description: "Worker", SystemPrompt: "Work.", Model: child, Tools: []datool.Tool{echo}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := compiled.Stream(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}, 32)
	defer stream.Close()
	var childEvents []dagent.ChildEvent
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Mode == dagent.EventChild && event.Child != nil {
			childEvents = append(childEvents, *event.Child)
		}
	}
	if _, err := stream.Result(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(childEvents) < 3 || childEvents[0].Phase != dagent.ChildStarted || childEvents[len(childEvents)-1].Phase != dagent.ChildCompleted {
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
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 4 {
			return fmt.Errorf("messages = %#v", request.Messages)
		}
		result := request.Messages[2]
		if result.Role != damessage.RoleTool || result.ToolCallID != "call-1" || result.Name != "lookup" || result.ToolStatus != damessage.ToolStatusError {
			return fmt.Errorf("patched tool result = %#v", result)
		}
		if !strings.Contains(result.TextContent(), "was cancelled") {
			return fmt.Errorf("patched tool result text = %q", result.TextContent())
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("continued")}})
	compiled, err := New(Options{Model: script, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{
		damessage.Human("start"),
		{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}},
		damessage.Human("continue without running it"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "continued" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestDeepAgentPlanningIsOptInAndPromptCachingIsAutomatic(t *testing.T) {
	script := modeltest.New(damodel.Profile{SupportsPromptCaching: true}, modeltest.Step{Check: func(request damodel.Request) error {
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
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled, err := New(Options{
		Model: script, EnableTodo: true, PromptCacheRetention: "24h",
		DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{
		Config: dacheckpoint.Config{ThreadID: "cache-thread"}, Messages: []damessage.Message{damessage.Human("go")},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMainPlanningOptInDoesNotLeakIntoGeneralSubagent(t *testing.T) {
	hasTodo := func(request damodel.Request) bool {
		for _, definition := range request.Tools {
			if definition.Name == "write_todos" {
				return true
			}
		}
		return false
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !hasTodo(request) {
				return errors.New("main planning tool missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
			ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"work","subagent_type":"general-purpose"}`),
		}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if hasTodo(request) {
				return errors.New("main planning opt-in leaked into general subagent")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("child done")}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("parent done")}},
	)
	compiled, err := New(Options{Model: script, EnableTodo: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestDeepAgentInterruptOnWiresHumanApproval(t *testing.T) {
	danger := datool.Func{Spec: datool.Definition{Name: "danger", Description: "dangerous action", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("ran"), nil
	}}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "danger-1", Name: "danger", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []datool.Tool{danger}, Saver: dacheckpoint.NewMemorySaver(),
		DisableSubagents: true, DisableSummary: true,
		InterruptOn: []dagent.ApprovalRule{{Pattern: "danger", Description: "Allow danger?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := dacheckpoint.Config{ThreadID: "generic-approval"}
	paused, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || paused.Interrupts[0].ID != "human_approval" {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Resume: dagent.ApprovalResponse{Decisions: map[string]dagent.ApprovalChoice{
		"danger-1": {Decision: dagent.ApprovalApprove},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("messages = %#v", resumed.Messages)
	}
}

func TestExplicitApprovalOverridesFilesystemPermissionApproval(t *testing.T) {
	memory, err := dabackend.NewMemory(map[string]dabackend.FileData{
		"/secret.txt": {Content: "backend secret", Encoding: dabackend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/secret.txt"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.TextContent() != "reviewer supplied" || last.ToolStatus != damessage.ToolStatusSuccess {
				return fmt.Errorf("tool response = %#v", last)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Backend: memory, Saver: dacheckpoint.NewMemorySaver(),
		DisableSubagents: true, DisableSummary: true,
		Permissions: []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/secret.txt"}, Mode: PermissionInterrupt}},
		InterruptOn: []dagent.ApprovalRule{{Pattern: "read_file", AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalRespond}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := dacheckpoint.Config{ThreadID: "approval-precedence"}
	paused, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || paused.Interrupts[0].ID != "human_approval" {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Resume: dagent.ApprovalResponse{Decisions: map[string]dagent.ApprovalChoice{
		"read": {Decision: dagent.ApprovalRespond, Message: "reviewer supplied"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("messages = %#v", resumed.Messages)
	}
}

func TestDefaultStateBackendPersistsParallelWritesPerThread(t *testing.T) {
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			{ID: "a", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/a.txt","content":"alpha"}`)},
			{ID: "b", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/b.txt","content":"beta"}`)},
		}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("written")}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/a.txt"}`)},
		}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "alpha") {
				return errors.New("checkpointed state file missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("read")}},
	)
	compiled, err := New(Options{Model: script, DisableSubagents: true, DisableSummary: true, Saver: dacheckpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	config := dacheckpoint.Config{ThreadID: "state-files"}
	first, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("write")}})
	if err != nil {
		t.Fatal(err)
	}
	files, ok := first.State["files"].(map[string]any)
	if !ok || len(files) != 2 {
		t.Fatalf("files = %#v", first.State["files"])
	}
	second, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("read")}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Messages[len(second.Messages)-1].TextContent() != "read" {
		t.Fatalf("result = %#v", second.Messages)
	}
}

func TestFilesystemPermissionDenyAndApproval(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	_, _ = memory.Write(context.Background(), "/public.txt", "public")
	_, _ = memory.Write(context.Background(), "/secret.txt", "secret")
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			{ID: "deny", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/secret.txt"}`)},
			{ID: "ask", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/public.txt","content":"changed"}`)},
		}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-2].ToolStatus != damessage.ToolStatusError {
				return errors.New("deny result missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Backend: memory, DisableSubagents: true, DisableSummary: true, Permissions: []FilesystemPermission{
		{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/secret.txt"}, Mode: PermissionDeny},
		{Operations: []FilesystemOperation{FilesystemWrite}, Paths: []string{"/public.txt"}, Mode: PermissionAsk},
	}, Saver: dacheckpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	config := dacheckpoint.Config{ThreadID: "permissions"}
	paused, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Resume: dagent.ApprovalResponse{Decisions: map[string]dagent.ApprovalChoice{"ask": {Decision: dagent.ApprovalApprove}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("resumed = %#v", resumed.Messages)
	}
}

func TestMemoryAndSkillsPromptInjection(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	_, _ = memory.Write(context.Background(), "/AGENTS.md", "<!-- hidden -->Remember blue.")
	_, _ = memory.Write(context.Background(), "/skills/research/SKILL.md", "---\nname: research\ndescription: Research carefully\nallowed-tools: read_file, grep\n---\nInstructions")
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) == 0 || request.Messages[0].Role != damessage.RoleSystem {
			return errors.New("system prompt missing")
		}
		prompt := request.Messages[0].TextContent()
		if !strings.Contains(prompt, "Remember blue") || strings.Contains(prompt, "hidden") || !strings.Contains(prompt, "research") || !strings.Contains(prompt, "/skills/research/SKILL.md") {
			return errors.New("memory or skill prompt missing")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled, err := New(Options{Model: script, Backend: memory, Memory: []string{"/AGENTS.md"}, Skills: []string{"/skills"}, DisableSubagents: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitEmptyMemoryAndSkillsStillInstallMiddleware(t *testing.T) {
	script := modeltest.New(damodel.Profile{Provider: "anthropic", SupportsPromptCaching: true}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) == 0 || request.Messages[0].Role != damessage.RoleSystem {
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
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled, err := New(Options{
		Model: script, Skills: []string{}, Memory: []string{}, DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredSystemMessagePreservesBlocksAndAppendsProfilePrompt(t *testing.T) {
	system := damessage.Message{Role: damessage.RoleSystem, Content: []damessage.ContentBlock{
		{Type: damessage.BlockText, Text: "first", Extra: map[string]json.RawMessage{"provider_field": json.RawMessage(`true`)}},
		{Type: damessage.BlockText, Text: "second"},
	}}
	script := modeltest.New(damodel.Profile{Provider: "anthropic", SupportsPromptCaching: true}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) == 0 || request.Messages[0].Role != damessage.RoleSystem {
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
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled, err := New(Options{
		Model: script, SystemMessage: &system, Profiles: []Profile{{SystemPrompt: "profile fragment"}},
		DisableSubagents: true, DisableSummary: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
	if len(system.Content) != 2 || system.Content[1].Extra != nil {
		t.Fatalf("New mutated source system message: %#v", system)
	}
}

func TestStructuredSystemMessageValidation(t *testing.T) {
	chat := modeltest.New(damodel.Profile{})
	human := damessage.Human("not system")
	if _, err := New(Options{Model: chat, SystemMessage: &human}); err == nil {
		t.Fatal("expected non-system message to fail")
	}
	system := damessage.System("system")
	if _, err := New(Options{Model: chat, SystemPrompt: "string", SystemMessage: &system}); err == nil {
		t.Fatal("expected conflicting system inputs to fail")
	}
}

func TestSubagentTaskUsesIsolatedInput(t *testing.T) {
	childModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 1 || request.Messages[0].TextContent() != "child work" {
			return errors.New("child input leaked")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("child result")}})
	child, err := dagent.New(dagent.Options{Model: childModel})
	if err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "task", Name: "task", Arguments: json.RawMessage(`{"description":"child work","subagent_type":"special"}`)}}}}}, modeltest.Step{Check: func(request damodel.Request) error {
		if request.Messages[len(request.Messages)-1].TextContent() != "child result" {
			return errors.New("child result missing")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("parent done")}})
	compiled, err := New(Options{Model: parentModel, Subagents: []Subagent{{Name: "special", Description: "Specialized", Runnable: child, InheritedState: []string{dagent.MessagesKey}}}, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("parent context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent done" {
		t.Fatal("parent did not finish")
	}
}

func TestDeclarativeSubagentInheritsToolsAndUsesOwnModelAndPrompt(t *testing.T) {
	lookup := datool.Func{Spec: datool.Definition{Name: "lookup", Description: "look up a value", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("lookup result"), nil
	}}
	childModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[0].Role != damessage.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "specialist instructions") {
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
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "lookup-1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "lookup result" {
				return errors.New("inherited tool result missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("child complete")}},
	)
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "delegate-declarative", Name: "task", Arguments: json.RawMessage(`{"description":"research this","subagent_type":"specialist"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "child complete" {
				return errors.New("declarative result missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("parent complete")}},
	)
	compiled, err := New(Options{
		Model: parentModel, Tools: []datool.Tool{lookup}, DisableSummary: true,
		Subagents: []Subagent{{
			Name: "specialist", Description: "Research specialist", SystemPrompt: "specialist instructions", Model: childModel,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("parent context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent complete" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestDeclarativeSubagentPropagatesNonMessageState(t *testing.T) {
	files, err := dabackend.NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	childModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "declarative-write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/declarative.txt","content":"from declarative child"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("created")}},
	)
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "delegate-writer", Name: "task", Arguments: json.RawMessage(`{"description":"create a file","subagent_type":"writer"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "read-written", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/declarative.txt"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "from declarative child") {
				return errors.New("declarative state update was not propagated")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
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
	result, err := parent.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("parent-only context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestPrivateParentStateDoesNotReachDeclarativeSubagent(t *testing.T) {
	childModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("child done")}})
	guard := dagent.Middleware{Name: "private_guard", BeforeModel: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
		if _, exists := values["parent_secret"]; exists {
			return nil, errors.New("private parent state leaked to child")
		}
		return nil, nil
	}}
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "private-task", Name: "task", Arguments: json.RawMessage(`{"description":"work","subagent_type":"worker"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: parentModel, DisableSummary: true,
		StateFields: map[string]dagent.StateField{"parent_secret": {Kind: dagent.FieldLast, Contract: "parent.secret.v1", Private: true, Clone: func(value any) any { return value }}},
		Subagents:   []Subagent{{Name: "worker", Description: "Works", SystemPrompt: "Work.", Model: childModel, Middleware: []dagent.Middleware{guard}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}, State: dastate.Values{"parent_secret": "hidden"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := result.State["parent_secret"]; exists {
		t.Fatalf("private state leaked from result: %#v", result.State)
	}
}

func TestDeclarativeSubagentStructuredResponseWinsAndEmptyToolsOverrideInheritance(t *testing.T) {
	parentOnly := datool.Func{Spec: datool.Definition{Name: "parent_only", Description: "parent tool", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("parent"), nil
	}}
	childModel := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{Check: func(request damodel.Request) error {
		for _, definition := range request.Tools {
			if definition.Name == "parent_only" {
				return errors.New("explicit empty child tools did not override inheritance")
			}
		}
		if request.ResponseFormat == nil || request.ResponseFormat.Name != "findings" {
			return errors.New("child structured response format missing")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("fallback text"), Structured: json.RawMessage(`{"answer":"structured"}`)}})
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "structured-task", Name: "task", Arguments: json.RawMessage(`{"description":"analyze","subagent_type":"analyst"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != `{"answer":"structured"}` {
				return errors.New("structured child response did not win")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: parentModel, Tools: []datool.Tool{parentOnly}, DisableSummary: true,
		Subagents: []Subagent{{
			Name: "analyst", Description: "Analyzes", SystemPrompt: "Analyze.", Model: childModel, Tools: []datool.Tool{},
			StructuredOutput: &dagent.StructuredOutput{Name: "findings", Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestSubagentInvocationPropagatesCancellation(t *testing.T) {
	childModel := &blockingChat{started: make(chan struct{})}
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "cancel-task", Name: "task", Arguments: json.RawMessage(`{"description":"block","subagent_type":"worker"}`)}}}}})
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
		_, err := compiled.Invoke(ctx, dagent.Input{Messages: []damessage.Message{damessage.Human("go")}})
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

func (chat *blockingChat) Invoke(ctx context.Context, _ damodel.Request) (damodel.Response, error) {
	close(chat.started)
	<-ctx.Done()
	return damodel.Response{}, ctx.Err()
}

func (*blockingChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

func (*blockingChat) Profile() damodel.Profile { return damodel.Profile{} }

func TestDeclarativeSubagentApprovalInterruptResumesChild(t *testing.T) {
	dangerRuns := 0
	danger := datool.Func{Spec: datool.Definition{Name: "danger", Description: "dangerous child action", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		dangerRuns++
		return datool.TextResult("ran"), nil
	}}
	childModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "child-danger", Name: "danger", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("child approved")}},
	)
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "delegate-approval", Name: "task", Arguments: json.RawMessage(`{"description":"perform action","subagent_type":"operator"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "child approved" {
				return errors.New("resumed child result missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("parent done")}},
	)
	saver := dacheckpoint.NewMemorySaver()
	compiled, err := New(Options{
		Model: parentModel, Tools: []datool.Tool{danger}, Saver: saver, DisableSummary: true,
		InterruptOn: []dagent.ApprovalRule{{Pattern: "danger", Description: "Allow danger?"}},
		Subagents: []Subagent{{
			Name: "operator", Description: "Performs approved actions", SystemPrompt: "Perform the action.", Model: childModel,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := dacheckpoint.Config{ThreadID: "nested-approval"}
	paused, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("delegate")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || dangerRuns != 0 {
		t.Fatalf("paused = %#v, danger runs = %d", paused.Interrupts, dangerRuns)
	}
	resumed, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Resume: dagent.ApprovalResponse{Decisions: map[string]dagent.ApprovalChoice{
		"child-danger": {Decision: dagent.ApprovalApprove},
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
		Model: modeltest.New(damodel.Profile{}), DisableSubagents: true, DisableSummary: true,
		InterruptOn: []dagent.ApprovalRule{{Pattern: "danger"}},
	})
	if err == nil || !strings.Contains(err.Error(), "checkpointer") {
		t.Fatalf("error = %v", err)
	}
}

func TestSubagentCanPropagateSelectedStateWithoutMessageLeak(t *testing.T) {
	files, err := dabackend.NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	childModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) != 1 || request.Messages[0].TextContent() != "create child file" {
				return errors.New("parent messages leaked into child")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "child-write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"/child.txt","content":"from child"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("created")}},
	)
	child, err := New(Options{Model: childModel, Backend: files, DisableSubagents: true, DisableSummary: true})
	if err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"create child file","subagent_type":"writer"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "parent-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/child.txt"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "from child") {
				return errors.New("selected child state was not propagated")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	parent, err := New(Options{
		Model: parentModel, Backend: files, DisableSummary: true,
		Subagents: []Subagent{{Name: "writer", Description: "Writes files", Runnable: child, InheritedState: []string{"files"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := parent.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("parent-only context")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestSummarizationPreservesRawHistoryAndOffloads(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{ContextWindow: 100}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("facts")}})
	mainModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "conversation that has been summarized") {
			return errors.New("compacted history missing")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{Model: summaryModel, Backend: memory, TriggerTokens: 1, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := dagent.New(dagent.Options{Model: mainModel, Middleware: []dagent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	messages := []damessage.Message{damessage.Human("old one"), damessage.Assistant("old two"), damessage.Human("recent one"), damessage.Assistant("recent two")}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: messages})
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
	memory, _ := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("durable summary")}})
	firstModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "durable summary") {
			return fmt.Errorf("first effective messages = %#v", request.Messages)
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("first response")}})
	firstMiddleware, err := SummarizationMiddleware(SummarizationOptions{Model: summaryModel, Backend: memory, TriggerMessages: 4, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	first, err := dagent.New(dagent.Options{Model: firstModel, Middleware: []dagent.Middleware{firstMiddleware}, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	config := dacheckpoint.Config{ThreadID: "summary-replay"}
	initial := []damessage.Message{damessage.Human("old one"), damessage.Assistant("old two"), damessage.Human("recent one"), damessage.Assistant("recent two")}
	if _, err := first.Invoke(context.Background(), dagent.Input{Config: config, Messages: initial}); err != nil {
		t.Fatal(err)
	}

	secondModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 5 || !strings.Contains(request.Messages[0].TextContent(), "durable summary") || strings.Contains(request.Messages[0].TextContent(), "old one") {
			return fmt.Errorf("replayed effective messages = %#v", request.Messages)
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("second response")}})
	unusedSummaryModel := modeltest.New(damodel.Profile{})
	secondMiddleware, err := SummarizationMiddleware(SummarizationOptions{Model: unusedSummaryModel, Backend: memory, TriggerMessages: 100, KeepMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := dagent.New(dagent.Options{Model: secondModel, Middleware: []dagent.Middleware{secondMiddleware}, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := second.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("continue")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 7 || result.Messages[0].TextContent() != "old one" {
		t.Fatalf("raw replayed messages = %#v", result.Messages)
	}
}

func TestSummarizationOffloadsLargeOldMedia(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "/conversation_history/media/") {
			return errors.New("large old media was not replaced with a reference")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("media summary")}})
	mainModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}})
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 1,
		KeepMessages: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := dagent.New(dagent.Options{Model: mainModel, Middleware: []dagent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	old := damessage.Message{Role: damessage.RoleHuman, Content: []damessage.ContentBlock{{Type: damessage.BlockImage, MIMEType: "image/png", Name: "sample.png", Data: []byte("large-image")}}}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{old, damessage.Human("recent")}}); err != nil {
		t.Fatal(err)
	}
	files, err := memory.Glob(context.Background(), "**/*.png", "/conversation_history/media")
	if err != nil || len(files.Matches) != 1 {
		t.Fatalf("offloaded media = %#v, %v", files, err)
	}
}

func TestSummarizationRetriesContextOverflowWithCompactedHistory(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("fallback facts")}})
	mainModel := modeltest.New(damodel.Profile{},
		modeltest.Step{Error: damodel.ErrContextOverflow},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) != 3 || !strings.Contains(request.Messages[0].TextContent(), "fallback facts") {
				return fmt.Errorf("retry messages = %#v", request.Messages)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
	)
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 1_000_000, KeepMessages: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := dagent.New(dagent.Options{Model: mainModel, Middleware: []dagent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{
		damessage.Human("old one"), damessage.Assistant("old two"), damessage.Human("recent one"), damessage.Assistant("recent two"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 5 || result.Messages[len(result.Messages)-1].TextContent() != "recovered" {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestSummarizationClipsTrailingToolBatchOnOverflow(t *testing.T) {
	memory, _ := dabackend.NewMemory(nil)
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("summary")}})
	mainModel := modeltest.New(damodel.Profile{},
		modeltest.Step{Error: damodel.ErrContextOverflow},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) != 4 {
				return fmt.Errorf("retry messages = %#v", request.Messages)
			}
			for _, item := range request.Messages[len(request.Messages)-2:] {
				if !strings.Contains(item.TextContent(), "Tool result too large") || !strings.Contains(item.TextContent(), "/large_tool_results/") {
					return fmt.Errorf("tool tail was not clipped: %#v", item)
				}
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
	)
	middleware, err := SummarizationMiddleware(SummarizationOptions{
		Model: summaryModel, Backend: memory, TriggerTokens: 1_000_000, KeepMessages: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := dagent.New(dagent.Options{Model: mainModel, Middleware: []dagent.Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	toolCalls := damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
		{ID: "lookup/one", Name: "lookup", Arguments: json.RawMessage(`{}`)},
		{ID: "lookup.two", Name: "lookup", Arguments: json.RawMessage(`{}`)},
	}}
	first := damessage.Tool("lookup/one", strings.Repeat("first line\n", 1_200))
	first.ID = "result-one"
	second := damessage.Tool("lookup.two", strings.Repeat("second line\n", 1_200))
	second.ID = "result-two"
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{
		damessage.Human("old"), damessage.Assistant("old answer"), damessage.Human("lookup"), toolCalls, first, second,
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
	files, err := dabackend.NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	summaryModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("summary")}})
	mainModel := modeltest.New(damodel.Profile{},
		modeltest.Step{Error: damodel.ErrContextOverflow},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if !strings.Contains(last.TextContent(), "Tool result too large") || !strings.Contains(last.TextContent(), "/large_tool_results/lookup_state") {
				return fmt.Errorf("tool result was not replaced with a durable reference: %#v", last)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
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
	call := damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
		ID: "lookup/state", Name: "lookup", Arguments: json.RawMessage(`{}`),
	}}}
	resultMessage := damessage.Tool("lookup/state", strings.Repeat("state-backed result\n", 1_200))
	result, err := compiled.Invoke(context.Background(), dagent.Input{
		Config: dacheckpoint.Config{ThreadID: "overflow-state"},
		Messages: []damessage.Message{
			damessage.Human("old question"), damessage.Assistant("old answer"), damessage.Human("lookup"), call, resultMessage,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	boundCtx, err := dabackend.BindRuntime(context.Background(), files, result.State)
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
	memory, _ := dabackend.NewMemory(nil)
	content := strings.Repeat("content line\n", 2_000)
	call := damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
		ID: "read-one", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/source.txt"}`),
	}}}
	resultMessage := damessage.Tool("read-one", content)
	clipped := clipOverflowToolTail(context.Background(), []damessage.Message{call, resultMessage}, []damessage.Message{call, resultMessage}, SummarizationOptions{
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
