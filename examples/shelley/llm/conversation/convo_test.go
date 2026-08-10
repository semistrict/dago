package conversation_test

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
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)

func compileAgent(t *testing.T, chat model.Chat, tools []tool.Tool, saver checkpoint.Saver, middleware ...agent.Middleware) *agent.Agent {
	t.Helper()
	compiled, err := agent.New(agent.Options{Model: chat, Tools: tools, Saver: saver, Middleware: middleware, FailOnToolError: true})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func namedTool(name string, run func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error)) tool.Tool {
	return tool.Func{Spec: tool.Definition{Name: name, Description: name, InputSchema: emptyObjectSchema}, Run: run}
}

func toolCall(id, name string) message.ToolCall {
	return message.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func overBudget(messages []message.Message, maximum float64) error {
	if maximum > 0 && message.AggregateUsage(messages).CostUSD >= maximum {
		return fmt.Errorf("usage exceeded $%.2f budget", maximum)
	}
	return nil
}

func resetBudget(maximum float64, messages []message.Message) float64 {
	if maximum <= 0 {
		return maximum
	}
	return maximum + message.AggregateUsage(messages).CostUSD
}

func TestBasicConvo(t *testing.T) {
	script := modeltest.New(model.Profile{},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) != 1 || !strings.Contains(request.Messages[0].TextContent(), "Cornelius") {
				return fmt.Errorf("first request = %#v", request.Messages)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("Hello, Cornelius.")}},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) != 3 || request.Messages[1].TextContent() != "Hello, Cornelius." {
				return fmt.Errorf("history = %#v", request.Messages)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("Your name is Cornelius.")}},
	)
	compiled := compileAgent(t, script, nil, checkpoint.NewMemorySaver())
	config := checkpoint.Config{ThreadID: "basic"}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("Hi, my name is Cornelius")}}); err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("What is my name?")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Messages[len(result.Messages)-1].TextContent(); !strings.Contains(got, "Cornelius") {
		t.Fatalf("response = %q", got)
	}
}

func TestCancelToolUse(t *testing.T) {
	started := make(chan struct{})
	blocking := namedTool("blocking", func(ctx context.Context, _ json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		close(started)
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	})
	script := modeltest.New(model.Profile{ToolCalling: true}, modeltest.Step{Response: model.Response{Message: message.Message{
		Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("cancel-me", "blocking")},
	}}})
	compiled := compileAgent(t, script, []tool.Tool{blocking}, checkpoint.NewMemorySaver())
	config := checkpoint.Config{ThreadID: "cancel"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := compiled.Invoke(ctx, agent.Input{Config: config, Messages: []message.Message{message.Human("start")}})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v", err)
	}
	cancelled := message.Tool("cancel-me", "Tool execution cancelled by user")
	cancelled.Name = "blocking"
	cancelled.ToolStatus = message.ToolStatusError
	result, err := compiled.Cancel(context.Background(), agent.Input{Config: config, Messages: []message.Message{cancelled}})
	if err != nil {
		t.Fatal(err)
	}
	last := result.Messages[len(result.Messages)-1]
	if last.ToolCallID != "cancel-me" || last.ToolStatus != message.ToolStatusError {
		t.Fatalf("cancel result = %#v", last)
	}
}

func TestInsertMissingToolResults(t *testing.T) {
	tools := []tool.Tool{}
	for _, name := range []string{"one", "two", "three"} {
		name := name
		tools = append(tools, namedTool(name, func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
			return tool.TextResult(name), nil
		}))
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			toolCall("1", "one"), toolCall("2", "two"), toolCall("3", "three"),
		}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Messages) != 5 {
				return fmt.Errorf("messages = %#v", request.Messages)
			}
			for index, id := range []string{"1", "2", "3"} {
				result := request.Messages[index+2]
				if result.Role != message.RoleTool || result.ToolCallID != id {
					return fmt.Errorf("tool result %d = %#v", index, result)
				}
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	result, err := compileAgent(t, script, tools, nil).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("run all")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestSubConvo(t *testing.T) {
	script := modeltest.New(model.Profile{},
		modeltest.Step{Check: onlyHuman("parent"), Response: model.Response{Message: message.Assistant("parent reply")}},
		modeltest.Step{Check: onlyHuman("child"), Response: model.Response{Message: message.Assistant("child reply")}},
	)
	compiled := compileAgent(t, script, nil, checkpoint.NewMemorySaver())
	parent, err := compiled.Invoke(context.Background(), agent.Input{Config: checkpoint.Config{ThreadID: "parent"}, Messages: []message.Message{message.Human("parent")}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := compiled.Invoke(context.Background(), agent.Input{Config: checkpoint.Config{ThreadID: "child"}, Messages: []message.Message{message.Human("child")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Messages) != 2 || len(child.Messages) != 2 || child.Messages[0].TextContent() != "child" {
		t.Fatalf("parent = %#v, child = %#v", parent.Messages, child.Messages)
	}
}

func onlyHuman(text string) func(model.Request) error {
	return func(request model.Request) error {
		if len(request.Messages) != 1 || request.Messages[0].Role != message.RoleHuman || request.Messages[0].TextContent() != text {
			return fmt.Errorf("request = %#v", request.Messages)
		}
		return nil
	}
}

func TestFindTool(t *testing.T) {
	duplicate := namedTool("same", func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("ok"), nil
	})
	if _, err := agent.New(agent.Options{Model: modeltest.New(model.Profile{}), Tools: []tool.Tool{duplicate, duplicate}}); !errors.Is(err, agent.ErrDuplicateTool) {
		t.Fatalf("duplicate tool error = %v", err)
	}
	unknown := modeltest.New(model.Profile{ToolCalling: true}, modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("x", "missing")}}}})
	_, err := compileAgent(t, unknown, nil, nil).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("go")}})
	if !errors.Is(err, agent.ErrUnknownTool) {
		t.Fatalf("unknown tool error = %v", err)
	}
}

func TestToolCallInfoFromContext(t *testing.T) {
	var got tool.Runtime
	inspect := namedTool("inspect", func(_ context.Context, _ json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
		got = runtime
		return tool.TextResult("inspected"), nil
	})
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("call-17", "inspect")}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := agent.New(agent.Options{Model: script, Tools: []tool.Tool{inspect}, Context: "trusted", Saver: checkpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Config: checkpoint.Config{ThreadID: "runtime"}, Messages: []message.Message{message.Human("inspect")}}); err != nil {
		t.Fatal(err)
	}
	if got.CallID != "call-17" || got.ThreadID != "runtime" || got.Context != "trusted" {
		t.Fatalf("tool runtime = %#v", got)
	}
}

func TestCumulativeUsageMethods(t *testing.T) {
	first := message.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6, CostUSD: 0.1, InputDetails: map[string]int{"cached": 1}}
	second := message.Usage{InputTokens: 6, OutputTokens: 3, TotalTokens: 9, CostUSD: 0.2, InputDetails: map[string]int{"cached": 2}}
	messages := []message.Message{{Role: message.RoleAssistant, Usage: &first}, {Role: message.RoleAssistant, Usage: &second}}
	usage := message.AggregateUsage(messages)
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 || usage.CostUSD < 0.299 || usage.CostUSD > 0.301 || usage.InputDetails["cached"] != 3 {
		t.Fatalf("usage = %#v", usage)
	}
	usage.InputDetails["cached"] = 99
	if first.InputDetails["cached"] != 1 {
		t.Fatal("aggregate usage aliased source data")
	}
}

func TestUsageMethods(t *testing.T) {
	usage := &message.Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11, CostUSD: 0.25}
	script := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, Content: message.Assistant("ok").Content, Usage: usage}}})
	result, err := compileAgent(t, script, nil, nil).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := message.AggregateUsage(result.Messages); got.TotalTokens != 11 || got.CostUSD != 0.25 {
		t.Fatalf("usage = %#v", got)
	}
}

func TestLastUsage(t *testing.T) {
	first := message.Assistant("first")
	first.Usage = &message.Usage{TotalTokens: 2}
	last := message.Assistant("last")
	last.Usage = &message.Usage{TotalTokens: 9}
	usage, ok := message.LastUsage([]message.Message{message.Human("hi"), first, last})
	if !ok || usage.TotalTokens != 9 {
		t.Fatalf("last usage = %#v, %v", usage, ok)
	}
}

func TestOverBudget(t *testing.T) {
	response := message.Assistant("ok")
	response.Usage = &message.Usage{CostUSD: 0.25}
	messages := []message.Message{response}
	if err := overBudget(messages, 0); err != nil {
		t.Fatalf("unlimited budget = %v", err)
	}
	if err := overBudget(messages, 1); err != nil {
		t.Fatalf("remaining budget = %v", err)
	}
}

func TestResetBudget(t *testing.T) {
	response := message.Assistant("ok")
	response.Usage = &message.Usage{CostUSD: 1.25}
	if got := resetBudget(5, []message.Message{response}); got != 6.25 {
		t.Fatalf("reset budget = %v", got)
	}
}

func TestOverBudgetFunction(t *testing.T) {
	response := message.Assistant("ok")
	response.OtherUsage = []message.PurposedUsage{{Purpose: "subagent", Usage: message.Usage{CostUSD: 0.4}}}
	if err := overBudget([]message.Message{response}, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := overBudget([]message.Message{response}, 0.4); err == nil {
		t.Fatal("nested model usage did not count toward budget")
	}
}

func TestListenerMethods(t *testing.T) {
	var events []string
	listener := agent.Middleware{
		Name: "listener",
		BeforeModel: func(context.Context, state.Values, agent.Runtime) (state.Values, error) {
			events = append(events, "request")
			return nil, nil
		},
		AfterModel: func(context.Context, state.Values, agent.Runtime) (state.Values, error) {
			events = append(events, "response")
			return nil, nil
		},
	}
	script := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}})
	if _, err := compileAgent(t, script, nil, nil, listener).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"request", "response"}) {
		t.Fatalf("events = %v", events)
	}
}

func TestIncrementToolUse(t *testing.T) {
	messages := []message.Message{{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
		toolCall("1", "bash"), toolCall("2", "bash"), toolCall("3", "read_file"),
	}}}
	counts := message.ToolUseCounts(messages)
	if counts["bash"] != 2 || counts["read_file"] != 1 {
		t.Fatalf("counts = %#v", counts)
	}
}

func TestToolResultCancelContents(t *testing.T) {
	result := message.Tool("call-1", "cancelled by user")
	result.Name = "bash"
	result.ToolStatus = message.ToolStatusError
	result.Artifact = json.RawMessage(`{"cause":"context canceled"}`)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Role != message.RoleTool || result.ToolCallID != "call-1" || result.ToolStatus != message.ToolStatusError || !json.Valid(result.Artifact) {
		t.Fatalf("cancelled tool result = %#v", result)
	}
}

func TestNewToolUseContext(t *testing.T) {
	var got tool.Runtime
	inspect := namedTool("inspect", func(_ context.Context, _ json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
		got = runtime
		return tool.TextResult("ok"), nil
	})
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("ctx-call", "inspect")}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := agent.New(agent.Options{Model: script, Tools: []tool.Tool{inspect}, Context: map[string]string{"scope": "conversation"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), agent.Input{Config: checkpoint.Config{ThreadID: "ctx-thread"}, Messages: []message.Message{message.Human("go")}, State: state.Values{"extra": "value"}}); err != nil {
		t.Fatal(err)
	}
	extra, _ := got.State.Get("extra")
	if got.CallID != "ctx-call" || got.ThreadID != "ctx-thread" || extra != "value" {
		t.Fatalf("runtime = %#v", got)
	}
}

func TestToolResultContents(t *testing.T) {
	artifact := json.RawMessage(`{"path":"/tmp/out"}`)
	produce := namedTool("produce", func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.Result{Content: []message.ContentBlock{{Type: message.BlockText, Text: "created"}}, Artifact: artifact, OtherUsage: []message.PurposedUsage{{Purpose: "helper", Usage: message.Usage{TotalTokens: 3}}}}, nil
	})
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("produce-1", "produce")}}}},
		modeltest.Step{Check: func(request model.Request) error {
			result := request.Messages[len(request.Messages)-1]
			if result.Role != message.RoleTool || result.TextContent() != "created" || string(result.Artifact) != string(artifact) || len(result.OtherUsage) != 1 {
				return fmt.Errorf("tool result = %#v", result)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []tool.Tool{produce}, nil).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("produce")}}); err != nil {
		t.Fatal(err)
	}
}

func TestListenerInterface(t *testing.T) {
	var requests, responses, calls, results int
	listener := agent.Middleware{
		Name: "listener",
		WrapModelCall: func(ctx context.Context, request agent.ModelRequest, next agent.ModelHandler) (agent.ModelResponse, error) {
			requests++
			response, err := next(ctx, request)
			responses++
			return response, err
		},
		WrapToolCall: func(ctx context.Context, request agent.ToolCallRequest, next agent.ToolHandler) (agent.ToolCallResponse, error) {
			calls++
			response, err := next(ctx, request)
			results++
			return response, err
		},
	}
	ping := namedTool("ping", func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("pong"), nil
	})
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("ping-1", "ping")}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []tool.Tool{ping}, nil, listener).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("ping")}}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || responses != 2 || calls != 1 || results != 1 {
		t.Fatalf("listener counts = %d %d %d %d", requests, responses, calls, results)
	}
}

func TestToolResultContentsWithToolUse(t *testing.T) {
	ping := namedTool("ping", func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("pong"), nil
	})
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("ping-1", "ping")}}}},
		modeltest.Step{Check: func(request model.Request) error {
			call := request.Messages[len(request.Messages)-2]
			result := request.Messages[len(request.Messages)-1]
			if len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != result.ToolCallID || result.TextContent() != "pong" {
				return fmt.Errorf("call/result = %#v / %#v", call, result)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []tool.Tool{ping}, nil).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("ping")}}); err != nil {
		t.Fatal(err)
	}
}

func TestOverBudgetWithExceeded(t *testing.T) {
	response := message.Assistant("expensive")
	response.Usage = &message.Usage{CostUSD: 0.75}
	if err := overBudget([]message.Message{response}, 0.5); err == nil {
		t.Fatal("expected exceeded budget error")
	}
}

func TestResetBudgetWithUsage(t *testing.T) {
	response := message.Assistant("used")
	response.Usage = &message.Usage{CostUSD: 2.5}
	response.OtherUsage = []message.PurposedUsage{{Purpose: "nested", Usage: message.Usage{CostUSD: 0.5}}}
	if got := resetBudget(10, []message.Message{response}); got != 13 {
		t.Fatalf("reset budget = %v", got)
	}
}

func TestConvoListenerIntegration(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(value string) { mu.Lock(); events = append(events, value); mu.Unlock() }
	listener := agent.Middleware{
		Name: "listener",
		BeforeModel: func(context.Context, state.Values, agent.Runtime) (state.Values, error) {
			record("model.before")
			return nil, nil
		},
		AfterModel: func(context.Context, state.Values, agent.Runtime) (state.Values, error) {
			record("model.after")
			return nil, nil
		},
		WrapToolCall: func(ctx context.Context, request agent.ToolCallRequest, next agent.ToolHandler) (agent.ToolCallResponse, error) {
			record("tool.before")
			response, err := next(ctx, request)
			record("tool.after")
			return response, err
		},
	}
	ping := namedTool("ping", func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
		return tool.TextResult("pong"), nil
	})
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{toolCall("ping-1", "ping")}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	if _, err := compileAgent(t, script, []tool.Tool{ping}, nil, listener).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("ping")}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"model.before", "model.after", "tool.before", "tool.after", "model.before", "model.after"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestSubConvoWithHistoryAdditional(t *testing.T) {
	parentScript := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("parent answer")}})
	parent, err := compileAgent(t, parentScript, nil, nil).Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("parent question")}})
	if err != nil {
		t.Fatal(err)
	}
	childScript := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 3 || request.Messages[0].TextContent() != "parent question" || request.Messages[2].TextContent() != "follow up" {
			return fmt.Errorf("child history = %#v", request.Messages)
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("child answer")}})
	childMessages := append(append([]message.Message(nil), parent.Messages...), message.Human("follow up"))
	child, err := compileAgent(t, childScript, nil, nil).Invoke(context.Background(), agent.Input{Config: checkpoint.Config{ThreadID: "child-history"}, Messages: childMessages})
	if err != nil {
		t.Fatal(err)
	}
	if len(parent.Messages) != 2 || len(child.Messages) != 4 {
		t.Fatalf("parent = %d messages, child = %d", len(parent.Messages), len(child.Messages))
	}
}

func TestDepthAdditional(t *testing.T) {
	grandchildModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("deep result")}})
	grandchild := compileAgent(t, grandchildModel, nil, nil)
	childModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "inner", Name: "task", Arguments: json.RawMessage(`{"description":"deep work","subagent_type":"deep"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("child done")}},
	)
	child, err := dago.New(dago.Options{Model: childModel, Subagents: []dago.Subagent{{Name: "deep", Description: "Deep worker", Runnable: grandchild}}, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	parentModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "outer", Name: "task", Arguments: json.RawMessage(`{"description":"child work","subagent_type":"child"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("parent done")}},
	)
	parent, err := dago.New(dago.Options{Model: parentModel, Subagents: []dago.Subagent{{Name: "child", Description: "Child worker", Runnable: child}}, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := parent.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("delegate twice")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "parent done" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestGetIDAdditional(t *testing.T) {
	script := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}})
	result, err := compileAgent(t, script, nil, checkpoint.NewMemorySaver()).Invoke(context.Background(), agent.Input{Config: checkpoint.Config{ThreadID: "conversation-id"}, Messages: []message.Message{message.Human("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.ThreadID != "conversation-id" || result.Config.CheckpointID == "" {
		t.Fatalf("config = %#v", result.Config)
	}
}

func TestDebugJSONAdditional(t *testing.T) {
	messages := []message.Message{message.Human("hello"), message.Assistant("world")}
	encoded, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var decoded []message.Message
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].Role != message.RoleHuman || decoded[1].TextContent() != "world" {
		t.Fatalf("decoded = %#v", decoded)
	}
}
