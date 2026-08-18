package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func TestGLM52HarnessProfileMatchesExactModels(t *testing.T) {
	models := []damodel.Profile{
		{Provider: "fireworks", Model: "accounts/fireworks/models/glm-5p2"},
		{Provider: "openrouter", Model: "z-ai/glm-5.2"},
		{Provider: "baseten", Model: "zai-org/GLM-5.2"},
	}
	for _, model := range models {
		model := model
		t.Run(model.Provider, func(t *testing.T) {
			profile, ok := builtinHarnessProfile(model.Provider, model.Model)
			if !ok || profile.SystemPromptSuffix == nil {
				t.Fatalf("profile = %#v, found = %v", profile, ok)
			}
			suffix := *profile.SystemPromptSuffix
			if !strings.Contains(suffix, "Execute the task directly") ||
				!strings.Contains(suffix, "Never place binary or encoded media in model context") {
				t.Fatalf("profile suffix is missing execution or media guidance: %q", suffix)
			}
			words := len(strings.Fields(suffix))
			if words < 240 || words > 360 {
				t.Fatalf("profile suffix has %d words, want 240..360", words)
			}
			if len(profile.Middleware) != 0 {
				t.Fatalf("built-in profile installed interactive recovery: %#v", profile.Middleware)
			}
		})
	}

	for _, model := range []damodel.Profile{
		{Provider: "fireworks", Model: "accounts/fireworks/models/glm-5.2"},
		{Provider: "custom", Model: "accounts/fireworks/models/glm-5p2"},
		{Provider: "openrouter", Model: "z-ai/glm-5.2 "},
		{Provider: "baseten", Model: "zai-org/glm-5.2"},
	} {
		if _, ok := builtinHarnessProfile(model.Provider, model.Model); ok {
			t.Fatalf("near-miss model matched: %#v", model)
		}
	}
}

func TestGLM52HarnessProfileIsAppliedDuringAgentConstruction(t *testing.T) {
	script := modeltest.New(damodel.Profile{
		Provider: fireworksGLM52Provider,
		Model:    fireworksGLM52Model,
	}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if len(request.Messages) == 0 ||
				!strings.Contains(request.Messages[0].TextContent(), "<execution>") ||
				!strings.Contains(request.Messages[0].TextContent(), "text-only model") {
				return errors.New("GLM-5.2 execution profile missing")
			}
			return nil
		},
		Response: damodel.Response{Message: damessage.Assistant("done")},
	})
	agent := NewAgent(script, WithoutSubagents(), WithoutSummary())
	if _, err := agent.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("work")},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGLM52TerminalStallRecoveryRetriesOnce(t *testing.T) {
	middleware := GLM52TerminalStallRecovery()
	tool := datool.Func{Spec: datool.Definition{
		Name: "write_file", Description: "write", InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	originalChoice := &damodel.ToolChoice{Mode: "auto"}
	originalReasoning := &damodel.Reasoning{Effort: "max", Summary: "auto"}
	request := dagent.ModelRequest{
		Model:         modeltest.New(damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}),
		SystemMessage: new(damessage.System("base prompt")),
		Tools:         []datool.Tool{tool},
		ToolChoice:    originalChoice,
		Reasoning:     originalReasoning,
		Metadata:      map[string]json.RawMessage{"trace": json.RawMessage(`"kept"`)},
	}
	stalled := dagent.ModelResponse{Messages: []damessage.Message{glm52Response(damodel.FinishReasonMaxTokens, false)}}
	recovered := dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("recovered")}}
	var calls []dagent.ModelRequest
	response, err := middleware.WrapModelCall(context.Background(), request, func(_ context.Context, actual dagent.ModelRequest) (dagent.ModelResponse, error) {
		calls = append(calls, actual.Clone())
		if len(calls) == 1 {
			return stalled, nil
		}
		return recovered, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || response.Messages[0].TextContent() != "recovered" {
		t.Fatalf("calls = %d, response = %#v", len(calls), response)
	}
	retry := calls[1]
	if retry.ToolChoice == nil || retry.ToolChoice.Mode != "required" {
		t.Fatalf("retry tool choice = %#v", retry.ToolChoice)
	}
	if retry.Reasoning == nil || retry.Reasoning.Effort != "none" || retry.Reasoning.Summary != "auto" {
		t.Fatalf("retry reasoning = %#v", retry.Reasoning)
	}
	if retry.SystemMessage == nil || !strings.Contains(retry.SystemMessage.TextContent(), "call a tool now") ||
		!strings.Contains(retry.SystemMessage.TextContent(), "base prompt") {
		t.Fatalf("retry system message = %#v", retry.SystemMessage)
	}
	if len(retry.Tools) != 1 || string(retry.Metadata["trace"]) != `"kept"` {
		t.Fatalf("retry lost caller settings: %#v", retry)
	}
	if request.ToolChoice != originalChoice || request.ToolChoice.Mode != "auto" ||
		request.Reasoning != originalReasoning || request.Reasoning.Effort != "max" ||
		!strings.Contains(request.SystemMessage.TextContent(), "base prompt") ||
		strings.Contains(request.SystemMessage.TextContent(), "terminal_stall_recovery") {
		t.Fatalf("original request was mutated: %#v", request)
	}
}

func TestGLM52TerminalStallRecoveryIsOneShot(t *testing.T) {
	middleware := GLM52TerminalStallRecovery()
	request := dagent.ModelRequest{Model: modeltest.New(damodel.Profile{
		Provider: fireworksGLM52Provider,
		Model:    fireworksGLM52Model,
	})}
	stalled := dagent.ModelResponse{Messages: []damessage.Message{glm52Response(damodel.FinishReasonMaxTokens, false)}}
	calls := 0
	response, err := middleware.WrapModelCall(context.Background(), request, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		calls++
		return stalled, nil
	})
	if err != nil || calls != 2 || !isGLM52TerminalStall(response) {
		t.Fatalf("calls = %d, response = %#v, error = %v", calls, response, err)
	}
}

func TestGLM52TerminalStallRecoveryIgnoresNearMisses(t *testing.T) {
	stalled := func(reason damodel.FinishReason, tool bool) dagent.ModelResponse {
		return dagent.ModelResponse{Messages: []damessage.Message{glm52Response(reason, tool)}}
	}
	cases := []struct {
		name     string
		profile  damodel.Profile
		response dagent.ModelResponse
	}{
		{name: "non GLM", profile: damodel.Profile{Provider: "openai", Model: "gpt-5.5"}, response: stalled(damodel.FinishReasonMaxTokens, false)},
		{name: "other provider", profile: damodel.Profile{Provider: "custom", Model: fireworksGLM52Model}, response: stalled(damodel.FinishReasonMaxTokens, false)},
		{name: "OpenRouter GLM", profile: damodel.Profile{Provider: "openrouter", Model: "z-ai/glm-5.2"}, response: stalled(damodel.FinishReasonMaxTokens, false)},
		{name: "stop", profile: damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}, response: stalled(damodel.FinishReasonStop, false)},
		{name: "tool call", profile: damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}, response: stalled(damodel.FinishReasonMaxTokens, true)},
		{name: "structured", profile: damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}, response: dagent.ModelResponse{Messages: stalled(damodel.FinishReasonMaxTokens, false).Messages, Structured: json.RawMessage(`{"done":true}`)}},
		{name: "zero messages", profile: damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}, response: dagent.ModelResponse{}},
		{name: "multiple messages", profile: damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}, response: dagent.ModelResponse{Messages: []damessage.Message{glm52Response(damodel.FinishReasonMaxTokens, false), glm52Response(damodel.FinishReasonMaxTokens, false)}}},
		{name: "non assistant", profile: damodel.Profile{Provider: fireworksGLM52Provider, Model: fireworksGLM52Model}, response: dagent.ModelResponse{Messages: []damessage.Message{damessage.Human("truncated")}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			middleware := GLM52TerminalStallRecovery()
			got, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{
				Model: modeltest.New(test.profile),
			}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
				calls++
				return test.response, nil
			})
			if err != nil || calls != 1 || len(got.Messages) != len(test.response.Messages) {
				t.Fatalf("calls = %d, response = %#v, error = %v", calls, got, err)
			}
		})
	}
}

func TestGLM52TerminalStallRecoveryHonorsCancellationAndErrors(t *testing.T) {
	middleware := GLM52TerminalStallRecovery()
	request := dagent.ModelRequest{Model: modeltest.New(damodel.Profile{
		Provider: fireworksGLM52Provider,
		Model:    fireworksGLM52Model,
	})}
	stalled := dagent.ModelResponse{Messages: []damessage.Message{glm52Response(damodel.FinishReasonMaxTokens, false)}}

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := middleware.WrapModelCall(ctx, request, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		calls++
		cancel()
		return stalled, nil
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel error = %v, calls = %d", err, calls)
	}

	sentinel := errors.New("provider failed")
	calls = 0
	_, err = middleware.WrapModelCall(context.Background(), request, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		calls++
		return stalled, sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("provider error = %v, calls = %d", err, calls)
	}
}

func glm52Response(reason damodel.FinishReason, tool bool) damessage.Message {
	response := damessage.Assistant("response")
	damodel.SetOutcome(&response, reason, nil)
	if tool {
		response.ToolCalls = []damessage.ToolCall{{
			ID: "call-write", Name: "write_file", Arguments: json.RawMessage(`{}`),
		}}
	}
	return response
}
