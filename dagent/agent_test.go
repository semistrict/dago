package dagent

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
	"testing/synctest"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

var objectSchema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)

func TestNewPanicsOnNilModel(t *testing.T) {
	requirePanicContaining(t, "model is nil", func() { New(nil, Options{}) })
}

func TestAgentInvokeNormalizesMessage(t *testing.T) {
	script := modeltest.New(damodel.Profile{}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if len(request.Messages) != 1 || request.Messages[0].Role != damessage.RoleHuman || request.Messages[0].TextContent() != `{"question":"hello"}` {
				return fmt.Errorf("messages = %#v", request.Messages)
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("answer")},
	})
	agent := New(script, Options{})
	result, err := agent.Invoke(t.Context(), struct {
		Question string `json:"question"`
	}{Question: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.String(); got != "answer" {
		t.Fatalf("Invoke() response = %q", got)
	}
}

func TestResultString(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		want   string
	}{
		{name: "assistant", result: Result{Messages: []damessage.Message{damessage.Human("question"), damessage.Assistant("first"), damessage.Assistant("second")}}, want: "first\nsecond"},
		{name: "direct tool", result: Result{Messages: []damessage.Message{damessage.Human("question"), damessage.Tool("call", "result")}}, want: "result"},
		{name: "assistant then direct tool", result: Result{Messages: []damessage.Message{damessage.Human("question"), damessage.Assistant("working"), damessage.Tool("call", "result")}}, want: "working\nresult"},
		{name: "structured", result: Result{Messages: []damessage.Message{damessage.Assistant("fallback")}, Structured: json.RawMessage(`{"answer":42}`)}, want: "fallback\n" + `{"answer":42}`},
		{name: "empty", result: Result{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.result.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
		})
	}
}

type retryReporterError struct{}

func (retryReporterError) Error() string { return "transient model failure" }

func (retryReporterError) RetryEvent(attempt int, delay time.Duration) damodel.RetryEvent {
	return damodel.RetryEvent{Attempt: attempt, Delay: delay, Retryable: true, Err: "transient model failure"}
}

func TestAgentRunsTextToolLoop(t *testing.T) {
	weather := datool.Func{
		Spec: datool.Definition{Name: "weather", Description: "Get weather", InputSchema: objectSchema},
		Run: func(_ context.Context, arguments json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
			if runtime.CallID != "call-1" {
				t.Fatalf("CallID = %q", runtime.CallID)
			}
			result := datool.TextResult("sunny")
			result.OtherUsage = []damessage.PurposedUsage{{Purpose: "weather_lookup", Usage: damessage.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}}
			return result, nil
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if len(request.Messages) != 2 || request.Messages[0].Role != damessage.RoleSystem || len(request.Tools) != 1 {
					return errors.New("unexpected first model request")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Message{
				Role:      damessage.RoleAssistant,
				ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "weather", Arguments: json.RawMessage(`{"value":"sf"}`)}},
			}},
		},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if len(request.Messages) != 4 || request.Messages[3].Role != damessage.RoleTool || request.Messages[3].TextContent() != "sunny" {
					return errors.New("tool result missing from second model request")
				}
				if len(request.Messages[3].OtherUsage) != 1 || request.Messages[3].OtherUsage[0].Purpose != "weather_lookup" {
					return errors.New("tool usage missing from second model request")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Assistant("It is sunny.")},
		},
	)
	compiled := New(script, Options{Tools: []datool.Tool{weather}, SystemMessage: damessage.System("Be helpful")})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("weather?")}})
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

func TestRetainedThreadStateSendsSystemMessageSeparately(t *testing.T) {
	script := modeltest.New(damodel.Profile{SupportsSeparateSystemMessage: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if request.SystemMessage == nil || request.SystemMessage.Role != damessage.RoleSystem || request.SystemMessage.TextContent() != "Be concise" {
				return fmt.Errorf("system message = %#v", request.SystemMessage)
			}
			if len(request.Messages) != 1 || request.Messages[0].Role != damessage.RoleHuman {
				return fmt.Errorf("conversation messages = %#v", request.Messages)
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled := New(
		script, Options{SystemMessage: damessage.System("Be concise"), Saver: dacheckpoint.NewMemorySaver(), RetainThreadState: true})

	result, err := compiled.Invoke(context.Background(), Input{
		Config: dacheckpoint.Config{ThreadID: "retained-system"}, Messages: []damessage.Message{damessage.Human("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 || result.Messages[1].TextContent() != "done" {
		t.Fatalf("result messages = %#v", result.Messages)
	}
}

func TestToolParentHandoffIsCommittedAndReturned(t *testing.T) {
	transfer := datool.Func{
		Spec: datool.Definition{Name: "transfer", Description: "Transfer to a sibling agent", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			result := datool.TextResult("transferred")
			result.Update = map[string]any{"owner": "agent_b"}
			result.Handoff = &datool.Handoff{Destination: "agent_b"}
			return result, nil
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{
		Role:      damessage.RoleAssistant,
		ToolCalls: []damessage.ToolCall{{ID: "call-transfer", Name: "transfer", Arguments: json.RawMessage(`{}`)}},
	}}})
	compiled := New(script, Options{Tools: []datool.Tool{transfer}})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("transfer")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Handoff == nil || result.Handoff.Destination != "agent_b" {
		t.Fatalf("handoff = %#v", result.Handoff)
	}
	if result.State["owner"] != "agent_b" {
		t.Fatalf("state = %#v", result.State)
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Role != damessage.RoleTool || last.ToolCallID != "call-transfer" || last.TextContent() != "transferred" {
		t.Fatalf("tool result = %#v", last)
	}
	if script.Remaining() != 0 {
		t.Fatalf("remaining model steps = %d", script.Remaining())
	}
}

func TestTerminalHandoffDoesNotLeakAcrossUserTurns(t *testing.T) {
	handoff, _ := json.Marshal(datool.Handoff{Destination: "agent_b"})
	toolMessage := damessage.Tool("call-transfer", "transferred")
	toolMessage.ResponseMetadata = map[string]json.RawMessage{handoffMetadataKey: handoff}
	if got := terminalHandoff([]damessage.Message{damessage.Human("first"), toolMessage}); got == nil || got.Destination != "agent_b" {
		t.Fatalf("terminal handoff = %#v", got)
	}
	if got := terminalHandoff([]damessage.Message{damessage.Human("first"), toolMessage, damessage.Human("next"), damessage.Assistant("done")}); got != nil {
		t.Fatalf("stale handoff = %#v", got)
	}
}

func TestUnknownToolReturnsRecoverableToolMessage(t *testing.T) {
	known := datool.Func{
		Spec: datool.Definition{Name: "alpha", Description: "Known tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			return datool.TextResult("ok"), nil
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "missing-1", Name: "missing", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != damessage.RoleTool || last.Name != "missing" || last.ToolCallID != "missing-1" || last.ToolStatus != damessage.ToolStatusError {
				return fmt.Errorf("unknown tool result = %#v", last)
			}
			if last.TextContent() != "Error: missing is not a valid tool, try one of [alpha]." {
				return fmt.Errorf("unknown tool text = %q", last.TextContent())
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
	)
	compiled := New(script, Options{Tools: []datool.Tool{known}})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "recovered" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestUnknownToolCanRemainFatal(t *testing.T) {
	script := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{
		Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "missing-1", Name: "missing", Arguments: json.RawMessage(`{}`)}},
	}}})
	compiled := New(script, Options{FailOnToolError: true})

	_, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}})
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentMessageIDsAreAssignedAndStableAcrossCheckpoints(t *testing.T) {
	saver := dacheckpoint.NewMemorySaver()
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("first")}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("second")}},
	)
	compiled := New(script, Options{Saver: saver})

	config := dacheckpoint.Config{ThreadID: "stable-message-ids"}
	first, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("one")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || first.Messages[0].ID == "" || first.Messages[1].ID == "" {
		t.Fatalf("first messages = %#v", first.Messages)
	}
	firstHumanID, firstAssistantID := first.Messages[0].ID, first.Messages[1].ID
	second, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("two")}})
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
		BeforeAgent: func(context.Context, dastate.Values, Runtime) (dastate.Values, error) {
			return dastate.Values{"secret": "hidden", "public": "shown"}, nil
		},
		WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
			if request.State["secret"] != "hidden" {
				return ModelResponse{}, errors.New("private state unavailable to middleware")
			}
			return next(ctx, request)
		},
	}
	compiled := New(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}}), Options{Middleware: []Middleware{middleware}})

	stream := compiled.Stream(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}})
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
	synctest.Test(t, func(t *testing.T) {
		started := false
		blocking := datool.Func{
			Spec: datool.Definition{Name: "blocking", Description: "Block until cancelled", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(ctx context.Context, _ json.RawMessage, _ datool.Runtime) (datool.Result, error) {
				started = true
				<-ctx.Done()
				return datool.Result{}, ctx.Err()
			},
		}
		script := modeltest.New(damodel.Profile{ToolCalling: true},
			modeltest.Step{Response: damodel.Response{Message: damessage.Message{
				ID: "assistant-tool", Role: damessage.RoleAssistant,
				ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "blocking", Arguments: json.RawMessage(`{}`)}},
			}}},
			modeltest.Step{Check: func(request damodel.Request) error {
				last := request.Messages[len(request.Messages)-1]
				if last.Role != damessage.RoleHuman || last.TextContent() != "after cancel" {
					return fmt.Errorf("last message = %#v", last)
				}
				return nil
			}, Response: damodel.Response{Message: damessage.Assistant("resumed")}},
		)
		saver := dacheckpoint.NewMemorySaver()
		compiled := New(script, Options{Tools: []datool.Tool{blocking}, Saver: saver, MaxConcurrency: 1})

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := compiled.Invoke(ctx, Input{Config: dacheckpoint.Config{ThreadID: "cancel"}, Messages: []damessage.Message{damessage.Human("start")}})
			done <- err
		}()
		synctest.Wait()
		if !started {
			t.Fatal("blocking tool did not start")
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Invoke() error = %v", err)
		}
		cancelled := damessage.Tool("call-1", "Tool execution cancelled by user")
		cancelled.Name = "blocking"
		cancelled.ToolStatus = damessage.ToolStatusError
		if _, err := compiled.Cancel(t.Context(), Input{
			Config:   dacheckpoint.Config{ThreadID: "cancel"},
			Messages: []damessage.Message{cancelled, damessage.Assistant("[Operation cancelled]")},
		}); err != nil {
			t.Fatal(err)
		}
		result, err := compiled.Invoke(t.Context(), Input{
			Config: dacheckpoint.Config{ThreadID: "cancel"}, Messages: []damessage.Message{damessage.Human("after cancel")},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Messages[len(result.Messages)-1].TextContent(); got != "resumed" {
			t.Fatalf("last response = %q", got)
		}
	})
}

func TestMiddlewareLifecycleAndWrapperNesting(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(value string) { mu.Lock(); events = append(events, value); mu.Unlock() }
	middleware := func(name string) Middleware {
		return Middleware{
			Name: name,
			BeforeAgent: func(context.Context, dastate.Values, Runtime) (dastate.Values, error) {
				record(name + ".before_agent")
				return nil, nil
			},
			BeforeModel: func(context.Context, dastate.Values, Runtime) (dastate.Values, error) {
				record(name + ".before_model")
				return nil, nil
			},
			WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
				record(name + ".model_in")
				response, err := next(ctx, request)
				record(name + ".model_out")
				return response, err
			},
			AfterModel: func(context.Context, dastate.Values, Runtime) (dastate.Values, error) {
				record(name + ".after_model")
				return nil, nil
			},
			AfterAgent: func(context.Context, dastate.Values, Runtime) (dastate.Values, error) {
				record(name + ".after_agent")
				return nil, nil
			},
		}
	}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := New(script, Options{Middleware: []Middleware{middleware("a"), middleware("b")}})

	if _, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("hi")}}); err != nil {
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
		AfterAgent: func(_ context.Context, values dastate.Values, _ Runtime) (dastate.Values, error) {
			if calls != 1 {
				return nil, nil
			}
			update := JumpUpdate("model")
			update[MessagesKey] = []damessage.Message{damessage.Human("Rewrite with concrete detail.")}
			update["rewritten"] = true
			return update, nil
		},
	}
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("Done.")}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-1].TextContent() != "Rewrite with concrete detail." {
				return fmt.Errorf("rewrite nudge missing: %#v", request.Messages)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("Created issue 42.")}},
	)
	compiled := New(script, Options{Middleware: []Middleware{middleware}})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("Create the issue.")}})
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
	middleware := Middleware{Name: "bad_jump", AfterAgent: func(context.Context, dastate.Values, Runtime) (dastate.Values, error) {
		return JumpUpdate("tools"), nil
	}}
	script := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}})
	compiled := New(script, Options{Middleware: []Middleware{middleware}})

	_, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}})
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
	failing := datool.Func{
		Spec: datool.Definition{Name: "fail", Description: "Fail", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			return datool.Result{}, errors.New("boom")
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "1", Name: "fail", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.ToolStatus != damessage.ToolStatusError || last.TextContent() == "" {
				return errors.New("expected tool error message")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
	)
	compiled := New(script, Options{Tools: []datool.Tool{failing}, Middleware: []Middleware{wrap("a"), wrap("b")}})

	if _, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"a.in", "b.in", "b.out", "a.out"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestInvalidToolArgumentsRemainRecoverable(t *testing.T) {
	type weatherInput struct {
		City string `json:"city"`
	}
	weather := datool.MustNew("weather", "Get weather", func(context.Context, weatherInput) (string, error) {
		return "sunny", nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "bad-args", Name: "weather", Arguments: json.RawMessage(`{"city":`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != damessage.RoleTool || last.ToolStatus != damessage.ToolStatusError || last.ToolCallID != "bad-args" {
				return fmt.Errorf("invalid argument result = %#v", last)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
	)
	compiled := New(script, Options{Tools: []datool.Tool{weather}})
	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "recovered" {
		t.Fatalf("result = %#v", result.Messages)
	}
}

func TestStructuredOutputProviderAndToolStrategies(t *testing.T) {
	provider := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if request.ResponseFormat == nil || request.ResponseFormat.Name != "answer" {
				return errors.New("provider format missing")
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("done"), Structured: json.RawMessage(`{"value":"yes"}`)},
	})
	providerAgent := New(provider, Options{StructuredOutput: &StructuredOutput{Strategy: StructuredAuto, Name: "answer", Description: "Answer", Schema: objectSchema}})

	providerResult, err := providerAgent.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("answer")}})
	if err != nil || string(providerResult.Structured) != `{"value":"yes"}` {
		t.Fatalf("provider result = %s, %v", providerResult.Structured, err)
	}

	toolModel := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if len(request.Tools) != 1 || request.Tools[0].Name != "answer" {
				return errors.New("structured tool missing")
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "structured", Name: "answer", Arguments: json.RawMessage(`{"value":"tool"}`)}}}},
	})
	toolAgent := New(toolModel, Options{StructuredOutput: &StructuredOutput{Strategy: StructuredTool, Name: "answer", Description: "Answer", Schema: objectSchema}})

	toolResult, err := toolAgent.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("answer")}})
	if err != nil || string(toolResult.Structured) != `{"value":"tool"}` || len(toolResult.Messages) != 3 {
		t.Fatalf("tool result = %#v, %v", toolResult, err)
	}
}

func TestStructuredOutputValidatesSchemaAndRetriesWhenConfigured(t *testing.T) {
	script := modeltest.New(damodel.Profile{StructuredOutput: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("first"), Structured: json.RawMessage(`{"value":7}`)}},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if len(request.Messages) < 3 || !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "validation failed") {
					return fmt.Errorf("validation feedback missing from retry: %#v", request.Messages)
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Assistant("second"), Structured: json.RawMessage(`{"value":"fixed"}`)},
		},
	)
	compiled := New(script, Options{StructuredOutput: &StructuredOutput{
		Strategy: StructuredProvider, Name: "answer", Description: "Answer", Schema: objectSchema, HandleErrors: true,
	}})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("answer")}})
	if err != nil || string(result.Structured) != `{"value":"fixed"}` {
		t.Fatalf("result = %#v, error = %v", result, err)
	}

	invalid := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("bad"), Structured: json.RawMessage(`{"value":7}`)},
	})
	strict := New(invalid, Options{StructuredOutput: &StructuredOutput{
		Strategy: StructuredProvider, Name: "answer", Description: "Answer", Schema: objectSchema,
	}})

	_, err = strict.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("answer")}})
	if !errors.Is(err, ErrStructuredValidation) {
		t.Fatalf("error = %v", err)
	}
}

func TestStructuredOutputRejectsRemoteSchemaReferences(t *testing.T) {
	requirePanicContaining(t, "external JSON Schema", func() {
		New(modeltest.New(damodel.Profile{}), Options{StructuredOutput: &StructuredOutput{
			Strategy: StructuredProvider, Name: "answer", Description: "Answer",
			Schema: json.RawMessage(`{"$ref":"https://example.invalid/schema.json"}`),
		}})
	})
}

func TestAgentStreamEmitsModelTokensAndReconstructsResponse(t *testing.T) {
	script := modeltest.New(damodel.Profile{NativeStreaming: true}, modeltest.Step{Chunks: []damodel.Chunk{
		{MessageDelta: damessage.Assistant("hel")},
		{MessageDelta: damessage.Assistant("lo")},
		{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, Usage: &damessage.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}, Done: true},
	}})
	compiled := New(script, Options{})

	stream := compiled.Stream(context.Background(), Input{Messages: []damessage.Message{damessage.Human("hi")}})
	var tokens []damodel.Chunk
	for event, nextErr := range stream.Events() {
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

func TestAgentReportsRetryableModelError(t *testing.T) {
	compiled := New(modeltest.New(damodel.Profile{}, modeltest.Step{Error: retryReporterError{}}), Options{})

	var observed []damodel.RetryEvent
	ctx := damodel.WithRetryObserver(context.Background(), func(_ context.Context, event damodel.RetryEvent) {
		observed = append(observed, event)
	})
	_, err := compiled.Invoke(ctx, Input{Messages: []damessage.Message{damessage.Human("go")}})
	if err == nil {
		t.Fatal("expected model error")
	}
	if len(observed) != 1 || !observed[0].Retryable || observed[0].Err != "transient model failure" {
		t.Fatalf("retry events = %#v", observed)
	}
}

func TestPromptCachingOnlyTargetsCapableModels(t *testing.T) {
	script := modeltest.New(damodel.Profile{SupportsPromptCaching: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if request.PromptCache == nil || request.PromptCache.Key != "thread-key" || request.PromptCache.Retention != "24h" {
				return fmt.Errorf("cache hint = %#v", request.PromptCache)
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled := New(script, Options{Middleware: []Middleware{PromptCaching("24h", func(ModelRequest) string { return "thread-key" })}})

	if _, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestPromptCachingAddsAnthropicBreakpointsWithoutMutatingInputs(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	firstTool := datool.Func{Spec: datool.Definition{Name: "first", Description: "First tool", InputSchema: schema}}
	secondTool := datool.Func{Spec: datool.Definition{
		Name:        "second",
		Description: "Second tool",
		InputSchema: schema,
		Extra:       map[string]json.RawMessage{"existing": json.RawMessage(`true`)},
	}}
	system := damessage.Message{Role: damessage.RoleSystem, Content: []damessage.ContentBlock{
		{Type: damessage.BlockText, Text: "first"},
		{Type: damessage.BlockText, Text: "second", Extra: map[string]json.RawMessage{"existing": json.RawMessage(`true`)}},
	}}
	chat := modeltest.New(damodel.Profile{Provider: "Anthropic", SupportsPromptCaching: true})
	middleware := PromptCaching("24h", nil)
	_, err := middleware.WrapModelCall(context.Background(), ModelRequest{
		Model: chat, SystemMessage: &system, Tools: []datool.Tool{firstTool, secondTool},
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
	system := damessage.System("system")
	chat := modeltest.New(damodel.Profile{Provider: "anthropic", SupportsPromptCaching: true})
	middleware := PromptCaching("1h", nil)
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
	script := modeltest.New(damodel.Profile{SupportsReasoning: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if request.Reasoning == nil || request.Reasoning.Effort != "high" || request.Reasoning.Summary != "auto" {
				return fmt.Errorf("reasoning = %#v", request.Reasoning)
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	compiled := New(script, Options{Middleware: []Middleware{{
		Name: "reasoning",
		WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
			request.Reasoning = &damodel.Reasoning{Effort: "high", Summary: "auto"}
			return next(ctx, request)
		},
	}}})

	if _, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointHistoryReplayForkAndDelete(t *testing.T) {
	saver := dacheckpoint.NewMemorySaver()
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("one")}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("fork")}},
	)
	compiled := New(script, Options{Saver: saver})

	result, err := compiled.Invoke(context.Background(), Input{Config: dacheckpoint.Config{ThreadID: "source"}, Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	history, err := compiled.History(context.Background(), dacheckpoint.Config{ThreadID: "source"}, dacheckpoint.ListOptions{})
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
	forked, err := compiled.Invoke(context.Background(), Input{Config: dacheckpoint.Config{ThreadID: "target"}, Messages: []damessage.Message{damessage.Human("again")}})
	if err != nil || forked.Messages[len(forked.Messages)-1].TextContent() != "fork" {
		t.Fatalf("forked = %#v, %v", forked, err)
	}
	if err := compiled.DeleteThread(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	history, err = compiled.History(context.Background(), dacheckpoint.Config{ThreadID: "target"}, dacheckpoint.ListOptions{})
	if err != nil || len(history) != 0 {
		t.Fatalf("deleted history = %#v, %v", history, err)
	}
}

func TestAgentRejectsDuplicateContractsAndTools(t *testing.T) {
	script := modeltest.New(damodel.Profile{})
	duplicate := datool.Func{Spec: datool.Definition{Name: "same", Description: "same", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	requirePanicContaining(t, ErrDuplicateTool.Error(), func() {
		New(script, Options{Tools: []datool.Tool{duplicate, duplicate}})
	})
	requirePanicContaining(t, "incompatible", func() {
		New(script, Options{Middleware: []Middleware{
			{Name: "a", Fields: map[string]StateField{"field": {Kind: FieldLast, Contract: "one", Clone: identityClone}}},
			{Name: "b", Fields: map[string]StateField{"field": {Kind: FieldLast, Contract: "two", Clone: identityClone}}},
		}})
	})
}

func requirePanicContaining(t *testing.T, substring string, fn func()) {
	t.Helper()
	defer func() {
		value := recover()
		if value == nil || !strings.Contains(fmt.Sprint(value), substring) {
			t.Fatalf("panic = %v, want substring %q", value, substring)
		}
	}()
	fn()
}

func TestHumanApprovalPausesBeforeAnyToolAndResumesAllDecisions(t *testing.T) {
	var executions atomic.Int32
	action := func(name string) datool.Tool {
		return datool.Func{
			Spec: datool.Definition{Name: name, Description: "Action", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
				executions.Add(1)
				return datool.TextResult(name + " done"), nil
			},
		}
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			{ID: "a", Name: "danger_a", Arguments: json.RawMessage(`{}`)},
			{ID: "b", Name: "danger_b", Arguments: json.RawMessage(`{}`)},
		}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-2].TextContent() != "not allowed" ||
				request.Messages[len(request.Messages)-1].TextContent() != "danger_b done" {
				return errors.New("approval results are not in call order")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled := New(
		script, Options{
			Tools:      []datool.Tool{action("danger_a"), action("danger_b")},
			Middleware: []Middleware{HumanApproval([]ApprovalRule{{Pattern: "danger_*", Description: "dangerous"}})},
			Saver:      dacheckpoint.NewMemorySaver(),
		})

	config := dacheckpoint.Config{ThreadID: "approval"}
	paused, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
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

func TestHumanApprovalInterruptPersistsAndResumesFromSQLite(t *testing.T) {
	saver, err := checkpointsqlite.Open(filepath.Join(t.TempDir(), "approval.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()

	var executions atomic.Int32
	action := datool.Func{
		Spec: datool.Definition{Name: "danger", Description: "Danger", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			executions.Add(1)
			return datool.TextResult("done"), nil
		},
	}
	rules := []ApprovalRule{{
		Pattern: "danger", Description: "Allow danger?",
		AllowedDecisions: []ApprovalDecision{ApprovalApprove, ApprovalReject},
	}}
	firstModel := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
			ID: "call", Name: "danger", Arguments: json.RawMessage(`{}`),
		}}}},
	})
	config := dacheckpoint.Config{ThreadID: "sqlite-approval"}
	paused, err := New(firstModel, Options{Tools: []datool.Tool{action}, Middleware: []Middleware{HumanApproval(rules)}, Saver: saver}).Invoke(
		context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || executions.Load() != 0 {
		t.Fatalf("paused = %#v, executions = %d", paused.Interrupts, executions.Load())
	}

	secondModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("finished")}})
	resumed, err := New(secondModel, Options{Tools: []datool.Tool{action}, Middleware: []Middleware{HumanApproval(rules)}, Saver: saver}).Invoke(
		context.Background(), Input{Config: config, Resume: ApprovalResponse{Decisions: map[string]ApprovalChoice{
			"call": {Decision: ApprovalApprove},
		}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Interrupts) != 0 || executions.Load() != 1 || resumed.Messages[len(resumed.Messages)-1].TextContent() != "finished" {
		t.Fatalf("resumed = %#v, executions = %d", resumed, executions.Load())
	}
}

func TestHumanApprovalRespondsWithoutExecutingTool(t *testing.T) {
	var executions atomic.Int32
	ask := datool.Func{
		Spec: datool.Definition{Name: "ask_user", Description: "Ask", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			executions.Add(1)
			return datool.TextResult("implementation ran"), nil
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "ask", Name: "ask_user", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != damessage.RoleTool || last.ToolStatus != damessage.ToolStatusSuccess || last.TextContent() != "blue" {
				return fmt.Errorf("synthetic response = %#v", last)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled := New(
		script, Options{
			Tools: []datool.Tool{ask}, Saver: dacheckpoint.NewMemorySaver(),
			Middleware: []Middleware{HumanApproval([]ApprovalRule{{Pattern: "ask_user", AllowedDecisions: []ApprovalDecision{ApprovalRespond}}})},
		})

	config := dacheckpoint.Config{ThreadID: "approval-respond"}
	paused, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
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
	action := datool.Func{
		Spec: datool.Definition{Name: "conditional", Description: "Conditional", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			executions.Add(1)
			return datool.TextResult("ran"), nil
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "conditional-call", Name: "conditional", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled := New(
		script, Options{
			Tools:      []datool.Tool{action},
			Middleware: []Middleware{HumanApproval([]ApprovalRule{{Pattern: "conditional", When: func(ToolCallRequest) bool { return false }}})},
		})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Interrupts) != 0 || executions.Load() != 1 {
		t.Fatalf("result = %#v, executions = %d", result, executions.Load())
	}
}

func TestToolInterruptCheckpointsCompletedSiblingBeforeResume(t *testing.T) {
	var siblingRuns atomic.Int32
	interrupting := datool.Func{
		Spec: datool.Definition{Name: "interrupting", Description: "Interrupt once", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(_ context.Context, _ json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
			if runtime.Resume == nil {
				return datool.Result{Interrupt: &datool.Interrupt{ID: "review", Value: "continue?"}}, nil
			}
			return datool.TextResult("resumed"), nil
		},
	}
	sibling := datool.Func{
		Spec: datool.Definition{Name: "sibling", Description: "Side effect", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			siblingRuns.Add(1)
			return datool.TextResult("sibling done"), nil
		},
	}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			{ID: "interrupt-call", Name: "interrupting", Arguments: json.RawMessage(`{}`)},
			{ID: "sibling-call", Name: "sibling", Arguments: json.RawMessage(`{}`)},
		}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-2].TextContent() != "resumed" || request.Messages[len(request.Messages)-1].TextContent() != "sibling done" {
				return fmt.Errorf("tool results = %#v", request.Messages)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	compiled := New(script, Options{Tools: []datool.Tool{interrupting, sibling}, Saver: dacheckpoint.NewMemorySaver()})

	config := dacheckpoint.Config{ThreadID: "tool-interrupt-sibling"}
	paused, err := compiled.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
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
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		flaky := datool.Func{
			Spec: datool.Definition{Name: "flaky", Description: "Flaky", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
				if calls.Add(1) < 3 {
					return datool.Result{}, errors.New("retry")
				}
				return datool.TextResult("ok"), nil
			},
		}
		script := modeltest.New(damodel.Profile{ToolCalling: true},
			modeltest.Step{Check: func(request damodel.Request) error {
				if len(request.Tools) != 2 {
					return errors.New("todo tool missing")
				}
				return nil
			}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "f", Name: "flaky", Arguments: json.RawMessage(`{}`)}}}}},
			modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
		)
		compiled := New(script, Options{Tools: []datool.Tool{flaky}, Middleware: []Middleware{
			ToolRetry(ToolRetryOptions{Name: "retry", Attempts: 3, Backoff: 500 * time.Millisecond}), TodoList(),
		}})

		started := time.Now()
		if _, err := compiled.Invoke(t.Context(), Input{Messages: []damessage.Message{damessage.Human("go")}}); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 3 || time.Since(started) != time.Second {
			t.Fatalf("tool calls = %d, elapsed = %s; want 3 calls in 1s", calls.Load(), time.Since(started))
		}
	})
}

func TestToolRetryBackoffHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		middleware := ToolRetry(ToolRetryOptions{Name: "retry", Attempts: 3, Backoff: 500 * time.Millisecond})
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		started := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := middleware.WrapToolCall(ctx, ToolCallRequest{}, func(context.Context, ToolCallRequest) (ToolCallResponse, error) {
				calls++
				return ToolCallResponse{}, errors.New("retry")
			})
			done <- err
		}()

		synctest.Wait()
		if calls != 1 || time.Since(started) != 0 {
			t.Fatalf("before cancel: calls=%d elapsed=%s; want one call and active backoff", calls, time.Since(started))
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("after cancel: error = %v, want context.Canceled", err)
		}
		if calls != 1 || time.Since(started) != 0 {
			t.Fatalf("after cancel: calls=%d elapsed=%s; want no retry or clock advance", calls, time.Since(started))
		}
	})
}

func TestAgentRestartsFromSQLiteDeltaMessages(t *testing.T) {
	saver, err := checkpointsqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	firstModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("first")}})
	first := New(firstModel, Options{Saver: saver})

	config := dacheckpoint.Config{ThreadID: "sqlite-agent"}
	if _, err := first.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("one")}}); err != nil {
		t.Fatal(err)
	}
	secondModel := modeltest.New(damodel.Profile{}, modeltest.Step{Check: func(request damodel.Request) error {
		if len(request.Messages) != 3 || request.Messages[0].TextContent() != "one" || request.Messages[1].TextContent() != "first" {
			return errors.New("restored history missing")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("second")}})
	second := New(secondModel, Options{Saver: saver})

	result, err := second.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("two")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 4 || result.Messages[3].TextContent() != "second" {
		t.Fatalf("restored messages = %#v", result.Messages)
	}
}

func TestMergeModelChunkPreservesTextAnnotations(t *testing.T) {
	response := damodel.Response{Message: damessage.Assistant("answer")}
	mergeModelChunk(&response, damodel.Chunk{MessageDelta: damessage.Message{
		Role: damessage.RoleAssistant,
		Content: []damessage.ContentBlock{{
			Type:      damessage.BlockText,
			Citations: []damessage.Citation{{URL: "https://example.test"}},
			Extra:     map[string]json.RawMessage{"provider": json.RawMessage(`true`)},
		}},
	}})
	block := response.Message.Content[0]
	if block.Text != "answer" || len(block.Citations) != 1 || block.Citations[0].URL != "https://example.test" || string(block.Extra["provider"]) != "true" {
		t.Fatalf("merged text block = %#v", block)
	}
}

func TestTodoListRejectsParallelReplacementAndRetriesModel(t *testing.T) {
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[0].TextContent(), "Never call write_todos multiple times in parallel") {
				return errors.New("todo planning prompt missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{
			{ID: "todo-1", Name: "write_todos", Arguments: json.RawMessage(`{"todos":[{"content":"one","status":"in_progress"}]}`)},
			{ID: "todo-2", Name: "write_todos", Arguments: json.RawMessage(`{"todos":[{"content":"two","status":"pending"}]}`)},
		}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			var errorsSeen int
			for _, item := range request.Messages {
				if item.Role == damessage.RoleTool && item.ToolStatus == damessage.ToolStatusError && item.TextContent() == parallelTodoError {
					errorsSeen++
				}
			}
			if errorsSeen != 2 {
				return fmt.Errorf("parallel todo errors = %d", errorsSeen)
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("recovered")}},
	)
	compiled := New(script, Options{Middleware: []Middleware{TodoList()}})

	result, err := compiled.Invoke(context.Background(), Input{Messages: []damessage.Message{damessage.Human("plan")}})
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
	firstModel := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{
			Role: damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{
				ID: "todo", Name: "write_todos", Arguments: json.RawMessage(`{"todos":[{"content":"portable","status":"in_progress"}]}`),
			}},
		}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("saved")}},
	)
	first := New(firstModel, Options{Middleware: []Middleware{TodoList()}, Saver: saver})

	config := dacheckpoint.Config{ThreadID: "todo-portable"}
	if _, err := first.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("plan")}}); err != nil {
		t.Fatal(err)
	}
	secondModel := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("resumed")}})
	observer := Middleware{Name: "todo_state_observer", WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
		todos := todosFromState(request.State["todos"])
		if len(todos) != 1 || todos[0].Content != "portable" || todos[0].Status != "in_progress" {
			return ModelResponse{}, fmt.Errorf("restored todos = %#v", todos)
		}
		return next(ctx, request)
	}}
	second := New(secondModel, Options{Middleware: []Middleware{TodoList(), observer}, Saver: saver})

	if _, err := second.Invoke(context.Background(), Input{Config: config, Messages: []damessage.Message{damessage.Human("continue")}}); err != nil {
		t.Fatal(err)
	}
}
