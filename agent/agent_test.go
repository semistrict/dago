package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/checkpoint"
	checkpointsqlite "github.com/semistrict/dago/checkpoint/sqlite"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

var objectSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)

func TestAgentRunsTextToolLoop(t *testing.T) {
	weather := tool.Func{
		Spec: tool.Definition{Name: "weather", Description: "Get weather", InputSchema: objectSchema},
		Run: func(_ context.Context, arguments json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
			if runtime.CallID != "call-1" {
				t.Fatalf("CallID = %q", runtime.CallID)
			}
			return tool.TextResult("sunny"), nil
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{
			Check: func(request model.Request) error {
				if len(request.Messages) != 2 || request.Messages[0].Role != message.RoleSystem || len(request.Tools) != 1 {
					return errors.New("unexpected first model request")
				}
				return nil
			},
			Response: model.Response{Message: message.Message{
				Role:      message.RoleAssistant,
				ToolCalls: []message.ToolCall{{ID: "call-1", Name: "weather", Arguments: json.RawMessage(`{"value":"sf"}`)}},
			}},
		},
		modeltest.Step{
			Check: func(request model.Request) error {
				if len(request.Messages) != 4 || request.Messages[3].Role != message.RoleTool || request.Messages[3].TextContent() != "sunny" {
					return errors.New("tool result missing from second model request")
				}
				return nil
			},
			Response: model.Response{Message: message.Assistant("It is sunny.")},
		},
	)
	compiled, err := New(Options{Model: script, Tools: []tool.Tool{weather}, SystemPrompt: "Be helpful"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("weather?")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 || result.Messages[3].TextContent() != "It is sunny." {
		t.Fatalf("messages = %#v", result.Messages)
	}
	if script.Remaining() != 0 {
		t.Fatalf("remaining model steps = %d", script.Remaining())
	}
}

func TestAgentCancelClearsPendingToolsAndPreservesState(t *testing.T) {
	started := make(chan struct{})
	blocking := tool.Func{
		Spec: tool.Definition{Name: "blocking", Description: "Block until cancelled", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(ctx context.Context, _ json.RawMessage, _ tool.Runtime) (tool.Result, error) {
			close(started)
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{
			ID: "assistant-tool", Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{ID: "call-1", Name: "blocking", Arguments: json.RawMessage(`{}`)}},
		}}},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != message.RoleHuman || last.TextContent() != "after cancel" {
				return fmt.Errorf("last message = %#v", last)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("resumed")}},
	)
	saver := checkpoint.NewMemorySaver()
	compiled, err := New(Options{Model: script, Tools: []tool.Tool{blocking}, Saver: saver, MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := compiled.Invoke(ctx, Input{Config: checkpoint.Config{ThreadID: "cancel"}, Messages: []message.Message{message.Human("start")}})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v", err)
	}
	cancelled := message.Tool("call-1", "Tool execution cancelled by user")
	cancelled.Name = "blocking"
	cancelled.ToolStatus = message.ToolStatusError
	if _, err := compiled.Cancel(context.Background(), Input{
		Config:   checkpoint.Config{ThreadID: "cancel"},
		Messages: []message.Message{cancelled, message.Assistant("[Operation cancelled]")},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{
		Config: checkpoint.Config{ThreadID: "cancel"}, Messages: []message.Message{message.Human("after cancel")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Messages[len(result.Messages)-1].TextContent(); got != "resumed" {
		t.Fatalf("last response = %q", got)
	}
}

func TestMiddlewareLifecycleAndWrapperNesting(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(value string) { mu.Lock(); events = append(events, value); mu.Unlock() }
	middleware := func(name string) Middleware {
		return Middleware{
			Name: name,
			BeforeAgent: func(context.Context, state.Values, Runtime) (state.Values, error) {
				record(name + ".before_agent")
				return nil, nil
			},
			BeforeModel: func(context.Context, state.Values, Runtime) (state.Values, error) {
				record(name + ".before_model")
				return nil, nil
			},
			WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
				record(name + ".model_in")
				response, err := next(ctx, request)
				record(name + ".model_out")
				return response, err
			},
			AfterModel: func(context.Context, state.Values, Runtime) (state.Values, error) {
				record(name + ".after_model")
				return nil, nil
			},
			AfterAgent: func(context.Context, state.Values, Runtime) (state.Values, error) {
				record(name + ".after_agent")
				return nil, nil
			},
		}
	}
	script := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{Model: script, Middleware: []Middleware{middleware("a"), middleware("b")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("hi")}}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"a.before_agent", "b.before_agent", "a.before_model", "b.before_model",
		"a.model_in", "b.model_in", "b.model_out", "a.model_out",
		"b.after_model", "a.after_model", "b.after_agent", "a.after_agent",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestToolWrapperNestingAndErrorMessage(t *testing.T) {
	var events []string
	wrap := func(name string) Middleware {
		return Middleware{Name: name, WrapToolCall: func(ctx context.Context, request ToolCallRequest, next ToolHandler) (ToolCallResponse, error) {
			events = append(events, name+".in")
			response, err := next(ctx, request)
			events = append(events, name+".out")
			return response, err
		}}
	}
	failing := tool.Func{
		Spec: tool.Definition{Name: "fail", Description: "Fail", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
			return tool.Result{}, errors.New("boom")
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "1", Name: "fail", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.ToolStatus != message.ToolStatusError || last.TextContent() == "" {
				return errors.New("expected tool error message")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("recovered")}},
	)
	compiled, err := New(Options{Model: script, Tools: []tool.Tool{failing}, Middleware: []Middleware{wrap("a"), wrap("b")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a.in", "b.in", "b.out", "a.out"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestStructuredOutputProviderAndToolStrategies(t *testing.T) {
	provider := modeltest.New(model.Profile{StructuredOutput: true}, modeltest.Step{
		Check: func(request model.Request) error {
			if request.ResponseFormat == nil || request.ResponseFormat.Name != "answer" {
				return errors.New("provider format missing")
			}
			return nil
		},
		Response: model.Response{Message: message.Assistant("done"), Structured: json.RawMessage(`{"value":"yes"}`)},
	})
	providerAgent, err := New(Options{Model: provider, StructuredOutput: &StructuredOutput{Strategy: StructuredAuto, Name: "answer", Description: "Answer", Schema: objectSchema}})
	if err != nil {
		t.Fatal(err)
	}
	providerResult, err := providerAgent.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("answer")}})
	if err != nil || string(providerResult.Structured) != `{"value":"yes"}` {
		t.Fatalf("provider result = %s, %v", providerResult.Structured, err)
	}

	toolModel := modeltest.New(model.Profile{ToolCalling: true}, modeltest.Step{
		Check: func(request model.Request) error {
			if len(request.Tools) != 1 || request.Tools[0].Name != "answer" {
				return errors.New("structured tool missing")
			}
			return nil
		},
		Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "structured", Name: "answer", Arguments: json.RawMessage(`{"value":"tool"}`)}}}},
	})
	toolAgent, err := New(Options{Model: toolModel, StructuredOutput: &StructuredOutput{Strategy: StructuredTool, Name: "answer", Description: "Answer", Schema: objectSchema}})
	if err != nil {
		t.Fatal(err)
	}
	toolResult, err := toolAgent.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("answer")}})
	if err != nil || string(toolResult.Structured) != `{"value":"tool"}` || len(toolResult.Messages) != 3 {
		t.Fatalf("tool result = %#v, %v", toolResult, err)
	}
}

func TestStructuredOutputValidatesSchemaAndRetriesWhenConfigured(t *testing.T) {
	script := modeltest.New(model.Profile{StructuredOutput: true},
		modeltest.Step{Response: model.Response{Message: message.Assistant("first"), Structured: json.RawMessage(`{"value":7}`)}},
		modeltest.Step{
			Check: func(request model.Request) error {
				if len(request.Messages) < 3 || !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "validation failed") {
					return fmt.Errorf("validation feedback missing from retry: %#v", request.Messages)
				}
				return nil
			},
			Response: model.Response{Message: message.Assistant("second"), Structured: json.RawMessage(`{"value":"fixed"}`)},
		},
	)
	compiled, err := New(Options{Model: script, StructuredOutput: &StructuredOutput{
		Strategy: StructuredProvider, Name: "answer", Description: "Answer", Schema: objectSchema, HandleErrors: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("answer")}})
	if err != nil || string(result.Structured) != `{"value":"fixed"}` {
		t.Fatalf("result = %#v, error = %v", result, err)
	}

	invalid := modeltest.New(model.Profile{StructuredOutput: true}, modeltest.Step{
		Response: model.Response{Message: message.Assistant("bad"), Structured: json.RawMessage(`{"value":7}`)},
	})
	strict, err := New(Options{Model: invalid, StructuredOutput: &StructuredOutput{
		Strategy: StructuredProvider, Name: "answer", Description: "Answer", Schema: objectSchema,
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = strict.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("answer")}})
	if !errors.Is(err, ErrStructuredValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestStructuredOutputRejectsRemoteSchemaReferences(t *testing.T) {
	_, err := New(Options{Model: modeltest.New(model.Profile{}), StructuredOutput: &StructuredOutput{
		Strategy: StructuredProvider, Name: "answer", Description: "Answer",
		Schema: json.RawMessage(`{"$ref":"https://example.invalid/schema.json"}`),
	}})
	if err == nil || !strings.Contains(err.Error(), "external JSON Schema") {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentStreamEmitsModelTokensAndReconstructsResponse(t *testing.T) {
	script := modeltest.New(model.Profile{NativeStreaming: true}, modeltest.Step{Chunks: []model.Chunk{
		{MessageDelta: message.Assistant("hel")},
		{MessageDelta: message.Assistant("lo")},
		{MessageDelta: message.Message{Role: message.RoleAssistant, Usage: &message.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}, Done: true},
	}})
	compiled, err := New(Options{Model: script})
	if err != nil {
		t.Fatal(err)
	}
	stream := compiled.Stream(context.Background(), Input{Messages: []message.Message{message.Human("hi")}}, 1)
	defer stream.Close()
	var tokens []model.Chunk
	for {
		event, nextErr := stream.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if event.Mode == EventToken {
			if event.Chunk == nil {
				t.Fatal("token event omitted chunk")
			}
			tokens = append(tokens, *event.Chunk)
		}
	}
	result, err := stream.Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 3 || result.Messages[len(result.Messages)-1].TextContent() != "hello" || result.Messages[len(result.Messages)-1].Usage.TotalTokens != 3 {
		t.Fatalf("tokens = %#v, result = %#v", tokens, result)
	}
}

func TestPromptCachingOnlyTargetsCapableModels(t *testing.T) {
	script := modeltest.New(model.Profile{SupportsPromptCaching: true}, modeltest.Step{
		Check: func(request model.Request) error {
			if request.PromptCache == nil || request.PromptCache.Key != "thread-key" || request.PromptCache.Retention != "24h" {
				return fmt.Errorf("cache hint = %#v", request.PromptCache)
			}
			return nil
		},
		Response: model.Response{Message: message.Assistant("done")},
	})
	compiled, err := New(Options{Model: script, Middleware: []Middleware{PromptCaching("", "24h", func(ModelRequest) string { return "thread-key" })}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestModelWrapperControlsProviderReasoning(t *testing.T) {
	script := modeltest.New(model.Profile{SupportsReasoning: true}, modeltest.Step{
		Check: func(request model.Request) error {
			if request.Reasoning == nil || request.Reasoning.Effort != "high" || request.Reasoning.Summary != "auto" {
				return fmt.Errorf("reasoning = %#v", request.Reasoning)
			}
			return nil
		},
		Response: model.Response{Message: message.Assistant("done")},
	})
	compiled, err := New(Options{Model: script, Middleware: []Middleware{{
		Name: "reasoning",
		WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
			request.Reasoning = &model.Reasoning{Effort: "high", Summary: "auto"}
			return next(ctx, request)
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointHistoryReplayForkAndDelete(t *testing.T) {
	saver := checkpoint.NewMemorySaver()
	script := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("one")}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("fork")}},
	)
	compiled, err := New(Options{Model: script, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{Config: checkpoint.Config{ThreadID: "source"}, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	history, err := compiled.History(context.Background(), checkpoint.Config{ThreadID: "source"}, checkpoint.ListOptions{})
	if err != nil || len(history) < 2 {
		t.Fatalf("history = %d, %v", len(history), err)
	}
	replayed, err := compiled.Replay(context.Background(), result.Config)
	if err != nil || replayed.Messages[len(replayed.Messages)-1].TextContent() != "one" {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	if err := compiled.Fork(context.Background(), "source", "target"); err != nil {
		t.Fatal(err)
	}
	forked, err := compiled.Invoke(context.Background(), Input{Config: checkpoint.Config{ThreadID: "target"}, Messages: []message.Message{message.Human("again")}})
	if err != nil || forked.Messages[len(forked.Messages)-1].TextContent() != "fork" {
		t.Fatalf("forked = %#v, %v", forked, err)
	}
	if err := compiled.DeleteThread(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	history, err = compiled.History(context.Background(), checkpoint.Config{ThreadID: "target"}, checkpoint.ListOptions{})
	if err != nil || len(history) != 0 {
		t.Fatalf("deleted history = %#v, %v", history, err)
	}
}

func TestAgentRejectsDuplicateContractsAndTools(t *testing.T) {
	script := modeltest.New(model.Profile{})
	duplicate := tool.Func{Spec: tool.Definition{Name: "same", Description: "same", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	_, err := New(Options{Model: script, Tools: []tool.Tool{duplicate, duplicate}})
	if !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("duplicate tool error = %v", err)
	}
	_, err = New(Options{Model: script, Middleware: []Middleware{
		{Name: "a", Fields: map[string]StateField{"field": {Kind: FieldLast, Contract: "one", Clone: identityClone}}},
		{Name: "b", Fields: map[string]StateField{"field": {Kind: FieldLast, Contract: "two", Clone: identityClone}}},
	}})
	if err == nil {
		t.Fatal("expected incompatible field error")
	}
}

func TestHumanApprovalPausesBeforeAnyToolAndResumesAllDecisions(t *testing.T) {
	var executions atomic.Int32
	action := func(name string) tool.Tool {
		return tool.Func{
			Spec: tool.Definition{Name: name, Description: "Action", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
				executions.Add(1)
				return tool.TextResult(name + " done"), nil
			},
		}
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			{ID: "a", Name: "danger_a", Arguments: json.RawMessage(`{}`)},
			{ID: "b", Name: "danger_b", Arguments: json.RawMessage(`{}`)},
		}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-2].TextContent() != "not allowed" ||
				request.Messages[len(request.Messages)-1].TextContent() != "danger_b done" {
				return errors.New("approval results are not in call order")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []tool.Tool{action("danger_a"), action("danger_b")},
		Middleware: []Middleware{HumanApproval([]ApprovalRule{{Pattern: "danger_*", Description: "dangerous"}})},
		Saver:      checkpoint.NewMemorySaver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "approval"}
	paused, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || executions.Load() != 0 {
		t.Fatalf("paused = %#v, executions = %d", paused.Interrupts, executions.Load())
	}
	resumed, err := compiled.Invoke(context.Background(), Input{Config: config, Resume: ApprovalResponse{Decisions: map[string]ApprovalChoice{
		"a": {Decision: ApprovalReject, Reason: "not allowed"},
		"b": {Decision: ApprovalApprove},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Interrupts) != 0 || executions.Load() != 1 || resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("resumed = %#v, executions = %d", resumed, executions.Load())
	}
}

func TestToolRetryAndTodoMiddleware(t *testing.T) {
	var calls atomic.Int32
	flaky := tool.Func{
		Spec: tool.Definition{Name: "flaky", Description: "Flaky", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
			if calls.Add(1) < 3 {
				return tool.Result{}, errors.New("retry")
			}
			return tool.TextResult("ok"), nil
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request model.Request) error {
			if len(request.Tools) != 2 {
				return errors.New("todo tool missing")
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "f", Name: "flaky", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Tools: []tool.Tool{flaky}, Middleware: []Middleware{
		ToolRetry("retry", 3, time.Nanosecond, nil), TodoList(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("tool calls = %d, want 3", calls.Load())
	}
}

func TestAgentRestartsFromSQLiteDeltaMessages(t *testing.T) {
	saver, err := checkpointsqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	firstModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("first")}})
	first, err := New(Options{Model: firstModel, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "sqlite-agent"}
	if _, err := first.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("one")}}); err != nil {
		t.Fatal(err)
	}
	secondModel := modeltest.New(model.Profile{}, modeltest.Step{Check: func(request model.Request) error {
		if len(request.Messages) != 3 || request.Messages[0].TextContent() != "one" || request.Messages[1].TextContent() != "first" {
			return errors.New("restored history missing")
		}
		return nil
	}, Response: model.Response{Message: message.Assistant("second")}})
	second, err := New(Options{Model: secondModel, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	result, err := second.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("two")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 || result.Messages[3].TextContent() != "second" {
		t.Fatalf("restored messages = %#v", result.Messages)
	}
}
