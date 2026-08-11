package dagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	dago "github.com/semistrict/dago"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)

func compileAgent(t *testing.T, chat damodel.Chat, tools []datool.Tool, saver dacheckpoint.Saver, middleware ...dagent.Middleware) *dagent.Agent {
	t.Helper()
	compiled, err := dagent.New(dagent.Options{Model: chat, Tools: tools, Saver: saver, Middleware: middleware, FailOnToolError: true})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func namedTool(name string, run func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error)) datool.Tool {
	return datool.Func{Spec: datool.Definition{Name: name, Description: name, InputSchema: emptyObjectSchema}, Run: run}
}

func toolCall(id, name string) damessage.ToolCall {
	return damessage.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func TestBasicConvo(t *testing.T) {
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) != 1 || !strings.Contains(request.Messages[0].TextContent(), "Cornelius") {
				return fmt.Errorf("first request = %#v", request.Messages)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("Hello, Cornelius.")}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) != 3 || request.Messages[1].TextContent() != "Hello, Cornelius." {
				return fmt.Errorf("history = %#v", request.Messages)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("Your name is Cornelius.")}},
	)
	compiled := compileAgent(t, script, nil, dacheckpoint.NewMemorySaver())
	config := dacheckpoint.Config{ThreadID: "basic"}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("Hi, my name is Cornelius")}}); err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("What is my name?")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Messages[len(result.Messages)-1].TextContent(); !strings.Contains(got, "Cornelius") {
		t.Fatalf("response = %q", got)
	}
}

func TestCancelToolUse(t *testing.T) {
	started := make(chan struct{})
	blocking := namedTool("blocking", func(ctx context.Context, _ json.RawMessage, _ datool.Runtime) (datool.Result, error) {
		close(started)
		<-ctx.Done()
		return datool.Result{}, ctx.Err()
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{
		Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("cancel-me", "blocking")},
	}}})
	compiled := compileAgent(t, script, []datool.Tool{blocking}, dacheckpoint.NewMemorySaver())
	config := dacheckpoint.Config{ThreadID: "cancel"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := compiled.Invoke(ctx, dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("start")}})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v", err)
	}
	cancelled := damessage.Tool("cancel-me", "Tool execution cancelled by user")
	cancelled.Name = "blocking"
	cancelled.ToolStatus = damessage.ToolStatusError
	result, err := compiled.Cancel(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{cancelled}})
	if err != nil {
		t.Fatal(err)
	}
	last := result.Messages[len(result.Messages)-1]
	if last.ToolCallID != "cancel-me" || last.ToolStatus != damessage.ToolStatusError {
		t.Fatalf("cancel result = %#v", last)
	}
}

func TestInsertMissingToolResults(t *testing.T) {
	tools := []datool.Tool{}
	for _, name := range []string{"one", "two", "three"} {
		name := name
		tools = append(tools, namedTool(name, func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			return datool.TextResult(name), nil
		}))
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			toolCall("1", "one"), toolCall("2", "two"), toolCall("3", "three"),
		}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if len(request.Messages) != 5 {
				return fmt.Errorf("messages = %#v", request.Messages)
			}
			for index, id := range []string{"1", "2", "3"} {
				result := request.Messages[index+2]
				if result.Role != damessage.RoleTool || result.ToolCallID != id {
					return fmt.Errorf("tool result %d = %#v", index, result)
				}
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	result, err := compileAgent(t, script, tools, nil).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("run all")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestSubConvo(t *testing.T) {
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Check: onlyHuman("parent"), Response: damodel.Response{Message: damessage.Assistant("parent reply")}},
		modeltest.Step{Check: onlyHuman("child"), Response: damodel.Response{Message: damessage.Assistant("child reply")}},
	)
	compiled := compileAgent(t, script, nil, dacheckpoint.NewMemorySaver())
	parent, err := compiled.Invoke(context.Background(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "parent"}, Messages: []damessage.Message{damessage.Human("parent")}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := compiled.Invoke(context.Background(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "child"}, Messages: []damessage.Message{damessage.Human("child")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Messages) != 2 || len(child.Messages) != 2 || child.Messages[0].TextContent() != "child" {
		t.Fatalf("parent = %#v, child = %#v", parent.Messages, child.Messages)
	}
}

func onlyHuman(text string) func(damodel.Request) error {
	return func(request damodel.Request) error {
		if len(request.Messages) != 1 || request.Messages[0].Role != damessage.RoleHuman || request.Messages[0].TextContent() != text {
			return fmt.Errorf("request = %#v", request.Messages)
		}
		return nil
	}
}

func TestFindTool(t *testing.T) {
	duplicate := namedTool("same", func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("ok"), nil
	})
	if _, err := dagent.New(dagent.Options{Model: modeltest.New(damodel.Profile{}), Tools: []datool.Tool{duplicate, duplicate}}); !errors.Is(err, dagent.ErrDuplicateTool) {
		t.Fatalf("duplicate tool error = %v", err)
	}
	unknown := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("x", "missing")}}}})
	_, err := compileAgent(t, unknown, nil, nil).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("go")}})
	if !errors.Is(err, dagent.ErrUnknownTool) {
		t.Fatalf("unknown tool error = %v", err)
	}
}

func TestToolCallInfoFromContext(t *testing.T) {
	var got datool.Runtime
	inspect := namedTool("inspect", func(_ context.Context, _ json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
		got = runtime
		return datool.TextResult("inspected"), nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("call-17", "inspect")}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := dagent.New(dagent.Options{Model: script, Tools: []datool.Tool{inspect}, Context: "trusted", Saver: dacheckpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "runtime"}, Messages: []damessage.Message{damessage.Human("inspect")}}); err != nil {
		t.Fatal(err)
	}
	if got.CallID != "call-17" || got.ThreadID != "runtime" || got.Context != "trusted" {
		t.Fatalf("tool runtime = %#v", got)
	}
}

func TestUsageMethods(t *testing.T) {
	usage := &damessage.Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11, CostUSD: 0.25}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, Content: damessage.Assistant("ok").Content, Usage: usage}}})
	result, err := compileAgent(t, script, nil, nil).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := damessage.AggregateUsage(result.Messages); got.TotalTokens != 11 || got.CostUSD != 0.25 {
		t.Fatalf("usage = %#v", got)
	}
}

func TestListenerMethods(t *testing.T) {
	var events []string
	listener := dagent.Middleware{
		Name: "listener",
		BeforeModel: func(context.Context, dastate.Values, dagent.Runtime) (dastate.Values, error) {
			events = append(events, "request")
			return nil, nil
		},
		AfterModel: func(context.Context, dastate.Values, dagent.Runtime) (dastate.Values, error) {
			events = append(events, "response")
			return nil, nil
		},
	}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}})
	if _, err := compileAgent(t, script, nil, nil, listener).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"request", "response"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestNewToolUseContext(t *testing.T) {
	var got datool.Runtime
	inspect := namedTool("inspect", func(_ context.Context, _ json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
		got = runtime
		return datool.TextResult("ok"), nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("ctx-call", "inspect")}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled, err := dagent.New(dagent.Options{Model: script, Tools: []datool.Tool{inspect}, Context: map[string]string{"scope": "conversation"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "ctx-thread"}, Messages: []damessage.Message{damessage.Human("go")}, State: dastate.Values{"extra": "value"}}); err != nil {
		t.Fatal(err)
	}
	extra, _ := got.State.Get("extra")
	if got.CallID != "ctx-call" || got.ThreadID != "ctx-thread" || extra != "value" {
		t.Fatalf("runtime = %#v", got)
	}
}

func TestToolResultContents(t *testing.T) {
	artifact := json.RawMessage(`{"path":"/tmp/out"}`)
	produce := namedTool("produce", func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.Result{Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "created"}}, Artifact: artifact, OtherUsage: []damessage.PurposedUsage{{Purpose: "helper", Usage: damessage.Usage{TotalTokens: 3}}}}, nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("produce-1", "produce")}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			result := request.Messages[len(request.Messages)-1]
			if result.Role != damessage.RoleTool || result.TextContent() != "created" || string(result.Artifact) != string(artifact) || len(result.OtherUsage) != 1 {
				return fmt.Errorf("tool result = %#v", result)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []datool.Tool{produce}, nil).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("produce")}}); err != nil {
		t.Fatal(err)
	}
}

func TestListenerInterface(t *testing.T) {
	var requests, responses, calls, results int
	listener := dagent.Middleware{
		Name: "listener",
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			requests++
			response, err := next(ctx, request)
			responses++
			return response, err
		},
		WrapToolCall: func(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
			calls++
			response, err := next(ctx, request)
			results++
			return response, err
		},
	}
	ping := namedTool("ping", func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("pong"), nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("ping-1", "ping")}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []datool.Tool{ping}, nil, listener).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("ping")}}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || responses != 2 || calls != 1 || results != 1 {
		t.Fatalf("listener counts = %d %d %d %d", requests, responses, calls, results)
	}
}

func TestToolResultContentsWithToolUse(t *testing.T) {
	ping := namedTool("ping", func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("pong"), nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("ping-1", "ping")}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			call := request.Messages[len(request.Messages)-2]
			result := request.Messages[len(request.Messages)-1]
			if len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != result.ToolCallID || result.TextContent() != "pong" {
				return fmt.Errorf("call/result = %#v / %#v", call, result)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []datool.Tool{ping}, nil).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("ping")}}); err != nil {
		t.Fatal(err)
	}
}

func TestConvoListenerIntegration(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(value string) { mu.Lock(); events = append(events, value); mu.Unlock() }
	listener := dagent.Middleware{
		Name: "listener",
		BeforeModel: func(context.Context, dastate.Values, dagent.Runtime) (dastate.Values, error) {
			record("model.before")
			return nil, nil
		},
		AfterModel: func(context.Context, dastate.Values, dagent.Runtime) (dastate.Values, error) {
			record("model.after")
			return nil, nil
		},
		WrapToolCall: func(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
			record("tool.before")
			response, err := next(ctx, request)
			record("tool.after")
			return response, err
		},
	}
	ping := namedTool("ping", func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		return datool.TextResult("pong"), nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{toolCall("ping-1", "ping")}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []datool.Tool{ping}, nil, listener).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("ping")}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"model.before", "model.after", "tool.before", "tool.after", "model.before", "model.after"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestSubConvoWithHistoryAdditional(t *testing.T) {
	parentScript := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("parent answer")}})
	parent, err := compileAgent(t, parentScript, nil, nil).Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("parent question")}})
	if err != nil {
		t.Fatal(err)
	}
	childScript := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 3 || request.Messages[0].TextContent() != "parent question" || request.Messages[2].TextContent() != "follow up" {
			return fmt.Errorf("child history = %#v", request.Messages)
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("child answer")}})
	childMessages := append(append([]damessage.Message(nil), parent.Messages...), damessage.Human("follow up"))
	child, err := compileAgent(t, childScript, nil, nil).Invoke(context.Background(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "child-history"}, Messages: childMessages})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Messages) != 2 || len(child.Messages) != 4 {
		t.Fatalf("parent = %d messages, child = %d", len(parent.Messages), len(child.Messages))
	}
}

func TestDepthAdditional(t *testing.T) {
	grandchildModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("deep result")}})
	grandchild := compileAgent(t, grandchildModel, nil, nil)
	childModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "inner", Name: "task", Arguments: json.RawMessage(`{"description":"deep work","subagent_type":"deep"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("child done")}},
	)
	child, err := dago.New(dago.Options{Model: childModel, Subagents: []dago.Subagent{{Name: "deep", Description: "Deep worker", Runnable: grandchild}}, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "outer", Name: "task", Arguments: json.RawMessage(`{"description":"child work","subagent_type":"child"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("parent done")}},
	)
	parent, err := dago.New(dago.Options{Model: parentModel, Subagents: []dago.Subagent{{Name: "child", Description: "Child worker", Runnable: child}}, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := parent.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("delegate twice")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestGetIDAdditional(t *testing.T) {
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}})
	result, err := compileAgent(t, script, nil, dacheckpoint.NewMemorySaver()).Invoke(context.Background(), dagent.Input{Config: dacheckpoint.Config{ThreadID: "conversation-id"}, Messages: []damessage.Message{damessage.Human("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.ThreadID != "conversation-id" || result.Config.CheckpointID == "" {
		t.Fatalf("config = %#v", result.Config)
	}
}
