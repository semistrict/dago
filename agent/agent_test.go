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
			result := tool.TextResult("sunny")
			result.OtherUsage = []message.PurposedUsage{{Purpose: "weather_lookup", Usage: message.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}}
			return result, nil
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
				if len(request.Messages[3].OtherUsage) != 1 || request.Messages[3].OtherUsage[0].Purpose != "weather_lookup" {
					return errors.New("tool usage missing from second model request")
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
	if len(result.Messages[2].OtherUsage) != 1 || result.Messages[2].OtherUsage[0].TotalTokens != 3 {
		t.Fatalf("tool usage = %#v", result.Messages[2].OtherUsage)
	}
	if script.Remaining() != 0 {
		t.Fatalf("remaining model steps = %d", script.Remaining())
	}
}

func TestAgentMessageIDsAreAssignedAndStableAcrossCheckpoints(t *testing.T) {
	saver := checkpoint.NewMemorySaver()
	script := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("first")}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("second")}},
	)
	compiled, err := New(Options{Model: script, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "stable-message-ids"}
	first, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || first.Messages[0].ID == "" || first.Messages[1].ID == "" {
		t.Fatalf("first messages = %#v", first.Messages)
	}
	firstHumanID, firstAssistantID := first.Messages[0].ID, first.Messages[1].ID
	second, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("two")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 4 {
		t.Fatalf("second messages = %#v", second.Messages)
	}
	if second.Messages[0].ID != firstHumanID || second.Messages[1].ID != firstAssistantID {
		t.Fatalf("checkpointed IDs changed: %#v", second.Messages)
	}
	for index, value := range second.Messages {
		if value.ID == "" {
			t.Errorf("message %d has no ID: %#v", index, value)
		}
	}
}

func TestPrivateStateIsAvailableInternallyButHiddenFromResultsAndStreams(t *testing.T) {
	middleware := Middleware{
		Name: "private_state",
		Fields: map[string]StateField{
			"secret": {Kind: FieldLast, Contract: "secret.v1", Private: true, Clone: identityClone},
			"public": {Kind: FieldLast, Contract: "public.v1", Clone: identityClone},
		},
		BeforeAgent: func(context.Context, state.Values, Runtime) (state.Values, error) {
			return state.Values{"secret": "hidden", "public": "shown"}, nil
		},
		WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
			if request.State["secret"] != "hidden" {
				return ModelResponse{}, errors.New("private state unavailable to middleware")
			}
			return next(ctx, request)
		},
	}
	compiled, err := New(Options{Model: modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}}), Middleware: []Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	stream := compiled.Stream(context.Background(), Input{Messages: []message.Message{message.Human("go")}}, 16)
	defer stream.Close()
	for {
		event, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := event.Update["secret"]; exists {
			t.Fatalf("private update leaked: %#v", event.Update)
		}
		if _, exists := event.Values["secret"]; exists {
			t.Fatalf("private values leaked: %#v", event.Values)
		}
	}
	result, err := stream.Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := result.State["secret"]; exists || result.State["public"] != "shown" {
		t.Fatalf("state = %#v", result.State)
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

func TestAfterAgentCanJumpBackToModel(t *testing.T) {
	calls := 0
	middleware := Middleware{
		Name: "one_rewrite",
		Fields: map[string]StateField{"rewritten": {
			Kind: FieldLast, Contract: "test.rewritten.v1", Private: true, Clone: identityClone,
		}},
		WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
			calls++
			return next(ctx, request)
		},
		AfterAgent: func(_ context.Context, values state.Values, _ Runtime) (state.Values, error) {
			if calls != 1 {
				return nil, nil
			}
			update := JumpUpdate("model")
			update[MessagesKey] = []message.Message{message.Human("Rewrite with concrete detail.")}
			update["rewritten"] = true
			return update, nil
		},
	}
	script := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("Done.")}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "Rewrite with concrete detail." {
				return fmt.Errorf("rewrite nudge missing: %#v", request.Messages)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("Created issue 42.")}},
	)
	compiled, err := New(Options{Model: script, Middleware: []Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("Create the issue.")}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Messages[len(result.Messages)-1].TextContent() != "Created issue 42." {
		t.Fatalf("calls = %d, messages = %#v", calls, result.Messages)
	}
	if _, exposed := result.State["rewritten"]; exposed {
		t.Fatalf("private guard state leaked: %#v", result.State)
	}
}

func TestAfterAgentRejectsUnknownJumpDestination(t *testing.T) {
	middleware := Middleware{Name: "bad_jump", AfterAgent: func(context.Context, state.Values, Runtime) (state.Values, error) {
		return JumpUpdate("tools"), nil
	}}
	script := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("done")}})
	compiled, err := New(Options{Model: script, Middleware: []Middleware{middleware}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("go")}})
	if err == nil || !strings.Contains(err.Error(), "jump destination") {
		t.Fatalf("error = %v", err)
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

func TestPromptCachingAddsAnthropicBreakpointsWithoutMutatingInputs(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	firstTool := tool.Func{Spec: tool.Definition{Name: "first", Description: "First tool", InputSchema: schema}}
	secondTool := tool.Func{Spec: tool.Definition{
		Name:        "second",
		Description: "Second tool",
		InputSchema: schema,
		Extra:       map[string]json.RawMessage{"existing": json.RawMessage(`true`)},
	}}
	system := message.Message{Role: message.RoleSystem, Content: []message.ContentBlock{
		{Type: message.BlockText, Text: "first"},
		{Type: message.BlockText, Text: "second", Extra: map[string]json.RawMessage{"existing": json.RawMessage(`true`)}},
	}}
	chat := modeltest.New(model.Profile{Provider: "Anthropic", SupportsPromptCaching: true})
	middleware := PromptCaching("", "24h", nil)
	_, err := middleware.WrapModelCall(context.Background(), ModelRequest{
		Model: chat, SystemMessage: &system, Tools: []tool.Tool{firstTool, secondTool},
	}, func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		if request.PromptCache == nil || request.PromptCache.Retention != "24h" {
			return ModelResponse{}, fmt.Errorf("cache hint = %#v", request.PromptCache)
		}
		if _, ok := request.SystemMessage.Content[0].Extra["cache_control"]; ok {
			return ModelResponse{}, errors.New("first system block has cache breakpoint")
		}
		want := `{"ttl":"5m","type":"ephemeral"}`
		if got := string(request.SystemMessage.Content[1].Extra["cache_control"]); got != want {
			return ModelResponse{}, fmt.Errorf("system cache control = %s, want %s", got, want)
		}
		if _, ok := request.Tools[0].Definition().Extra["cache_control"]; ok {
			return ModelResponse{}, errors.New("first tool has cache breakpoint")
		}
		definition := request.Tools[1].Definition()
		if got := string(definition.Extra["cache_control"]); got != want || string(definition.Extra["existing"]) != "true" {
			return ModelResponse{}, fmt.Errorf("tool extras = %#v", definition.Extra)
		}
		return ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := system.Content[1].Extra["cache_control"]; ok {
		t.Fatal("prompt caching mutated the source system message")
	}
	if _, ok := secondTool.Definition().Extra["cache_control"]; ok {
		t.Fatal("prompt caching mutated the source tool definition")
	}
}

func TestPromptCachingUsesOneHourAnthropicTTL(t *testing.T) {
	system := message.System("system")
	chat := modeltest.New(model.Profile{Provider: "anthropic", SupportsPromptCaching: true})
	middleware := PromptCaching("", "1h", nil)
	_, err := middleware.WrapModelCall(context.Background(), ModelRequest{Model: chat, SystemMessage: &system}, func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		want := `{"ttl":"1h","type":"ephemeral"}`
		if got := string(request.SystemMessage.Content[0].Extra["cache_control"]); got != want {
			return ModelResponse{}, fmt.Errorf("cache control = %s, want %s", got, want)
		}
		return ModelResponse{}, nil
	})
	if err != nil {
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

func TestHumanApprovalRespondsWithoutExecutingTool(t *testing.T) {
	var executions atomic.Int32
	ask := tool.Func{
		Spec: tool.Definition{Name: "ask_user", Description: "Ask", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
			executions.Add(1)
			return tool.TextResult("implementation ran"), nil
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "ask", Name: "ask_user", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != message.RoleTool || last.ToolStatus != message.ToolStatusSuccess || last.TextContent() != "blue" {
				return fmt.Errorf("synthetic response = %#v", last)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []tool.Tool{ask}, Saver: checkpoint.NewMemorySaver(),
		Middleware: []Middleware{HumanApproval([]ApprovalRule{{Pattern: "ask_user", AllowedDecisions: []ApprovalDecision{ApprovalRespond}}})},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "approval-respond"}
	paused, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}
	resumed, err := compiled.Invoke(context.Background(), Input{Config: config, Resume: ApprovalResponse{Decisions: map[string]ApprovalChoice{
		"ask": {Decision: ApprovalRespond, Message: "blue"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 0 || resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("executions = %d, messages = %#v", executions.Load(), resumed.Messages)
	}
}

func TestHumanApprovalConditionalRuleCanAutoApprove(t *testing.T) {
	var executions atomic.Int32
	action := tool.Func{
		Spec: tool.Definition{Name: "conditional", Description: "Conditional", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
			executions.Add(1)
			return tool.TextResult("ran"), nil
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "conditional-call", Name: "conditional", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{
		Model: script, Tools: []tool.Tool{action},
		Middleware: []Middleware{HumanApproval([]ApprovalRule{{Pattern: "conditional", When: func(ToolCallRequest) bool { return false }}})},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Interrupts) != 0 || executions.Load() != 1 {
		t.Fatalf("result = %#v, executions = %d", result, executions.Load())
	}
}

func TestToolInterruptCheckpointsCompletedSiblingBeforeResume(t *testing.T) {
	var siblingRuns atomic.Int32
	interrupting := tool.Func{
		Spec: tool.Definition{Name: "interrupting", Description: "Interrupt once", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(_ context.Context, _ json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
			if runtime.Resume == nil {
				return tool.Result{Interrupt: &tool.Interrupt{ID: "review", Value: "continue?"}}, nil
			}
			return tool.TextResult("resumed"), nil
		},
	}
	sibling := tool.Func{
		Spec: tool.Definition{Name: "sibling", Description: "Side effect", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, tool.Runtime) (tool.Result, error) {
			siblingRuns.Add(1)
			return tool.TextResult("sibling done"), nil
		},
	}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			{ID: "interrupt-call", Name: "interrupting", Arguments: json.RawMessage(`{}`)},
			{ID: "sibling-call", Name: "sibling", Arguments: json.RawMessage(`{}`)},
		}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-2].TextContent() != "resumed" || request.Messages[len(request.Messages)-1].TextContent() != "sibling done" {
				return fmt.Errorf("tool results = %#v", request.Messages)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("done")}},
	)
	compiled, err := New(Options{Model: script, Tools: []tool.Tool{interrupting, sibling}, Saver: checkpoint.NewMemorySaver()})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "tool-interrupt-sibling"}
	paused, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || siblingRuns.Load() != 1 {
		t.Fatalf("paused = %#v, sibling runs = %d", paused.Interrupts, siblingRuns.Load())
	}
	resumed, err := compiled.Invoke(context.Background(), Input{Config: config, Resume: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Interrupts) != 0 || siblingRuns.Load() != 1 || resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("resumed = %#v, sibling runs = %d", resumed, siblingRuns.Load())
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

func TestMergeModelChunkPreservesTextAnnotations(t *testing.T) {
	response := model.Response{Message: message.Assistant("answer")}
	mergeModelChunk(&response, model.Chunk{MessageDelta: message.Message{
		Role: message.RoleAssistant,
		Content: []message.ContentBlock{{
			Type:      message.BlockText,
			Citations: []message.Citation{{URL: "https://example.test"}},
			Extra:     map[string]json.RawMessage{"provider": json.RawMessage(`true`)},
		}},
	}})
	block := response.Message.Content[0]
	if block.Text != "answer" || len(block.Citations) != 1 || block.Citations[0].URL != "https://example.test" || string(block.Extra["provider"]) != "true" {
		t.Fatalf("merged text block = %#v", block)
	}
}

func TestTodoListRejectsParallelReplacementAndRetriesModel(t *testing.T) {
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[0].TextContent(), "Never call write_todos multiple times in parallel") {
				return errors.New("todo planning prompt missing")
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{
			{ID: "todo-1", Name: "write_todos", Arguments: json.RawMessage(`{"todos":[{"content":"one","status":"in_progress"}]}`)},
			{ID: "todo-2", Name: "write_todos", Arguments: json.RawMessage(`{"todos":[{"content":"two","status":"pending"}]}`)},
		}}}},
		modeltest.Step{Check: func(request model.Request) error {
			var errorsSeen int
			for _, item := range request.Messages {
				if item.Role == message.RoleTool && item.ToolStatus == message.ToolStatusError && item.TextContent() == parallelTodoError {
					errorsSeen++
				}
			}
			if errorsSeen != 2 {
				return fmt.Errorf("parallel todo errors = %d", errorsSeen)
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("recovered")}},
	)
	compiled, err := New(Options{Model: script, Middleware: []Middleware{TodoList()}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), Input{Messages: []message.Message{message.Human("plan")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "recovered" {
		t.Fatalf("messages = %#v", result.Messages)
	}
	if len(todosFromState(result.State["todos"])) != 0 {
		t.Fatalf("parallel todo calls changed state: %#v", result.State["todos"])
	}
}

func TestTodoListStateRestartsFromSQLite(t *testing.T) {
	saver, err := checkpointsqlite.Open(filepath.Join(t.TempDir(), "todos.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	firstModel := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Response: model.Response{Message: message.Message{
			Role: message.RoleAssistant,
			ToolCalls: []message.ToolCall{{
				ID: "todo", Name: "write_todos", Arguments: json.RawMessage(`{"todos":[{"content":"portable","status":"in_progress"}]}`),
			}},
		}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("saved")}},
	)
	first, err := New(Options{Model: firstModel, Middleware: []Middleware{TodoList()}, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "todo-portable"}
	if _, err := first.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("plan")}}); err != nil {
		t.Fatal(err)
	}
	secondModel := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("resumed")}})
	observer := Middleware{Name: "todo_state_observer", WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
		todos := todosFromState(request.State["todos"])
		if len(todos) != 1 || todos[0].Content != "portable" || todos[0].Status != "in_progress" {
			return ModelResponse{}, fmt.Errorf("restored todos = %#v", todos)
		}
		return next(ctx, request)
	}}
	second, err := New(Options{Model: secondModel, Middleware: []Middleware{TodoList(), observer}, Saver: saver})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Invoke(context.Background(), Input{Config: config, Messages: []message.Message{message.Human("continue")}}); err != nil {
		t.Fatal(err)
	}
}
