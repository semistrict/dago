package dagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

type runtimeModelChat struct{ provider, model string }

func (chat *runtimeModelChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{Message: damessage.Assistant(chat.model)}, nil
}
func (chat *runtimeModelChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}
func (chat *runtimeModelChat) Profile() damodel.Profile {
	return damodel.Profile{Provider: chat.provider, Model: chat.model}
}

func TestRuntimeModelSelectsPerRunAndPersistsSelection(t *testing.T) {
	base := &runtimeModelChat{provider: "base", model: "default"}
	selected := &runtimeModelChat{provider: "provider", model: "selected"}
	var specs []string
	middleware := RuntimeModel(ModelResolverFunc(func(_ context.Context, spec string) (damodel.Chat, error) {
		specs = append(specs, spec)
		return selected, nil
	}), RuntimeModelOptions{})
	request := ModelRequest{
		Model: base, State: dastate.Values{},
		Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{RuntimeModelConfigKey: "provider:selected"})},
	}
	response, err := middleware.WrapModelCall(t.Context(), request, func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		if request.Model != selected {
			t.Fatalf("selected model = %T %#v", request.Model, request.Model.Profile())
		}
		return ModelResponse{Messages: []damessage.Message{damessage.Assistant("ok")}, Update: dastate.Values{"kept": true}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0] != "provider:selected" || response.Update[RuntimeModelStateKey] != "provider:selected" || response.Update["kept"] != true {
		t.Fatalf("specs/update = %#v / %#v", specs, response.Update)
	}
	field := middleware.Fields[RuntimeModelStateKey]
	if !field.Private || field.Contract != "dago.runtime-model-spec.v1" {
		t.Fatalf("state field = %#v", field)
	}
}

func TestRuntimeModelUsesCheckpointFallbackAndSameModelIsNoOp(t *testing.T) {
	base := &runtimeModelChat{provider: "openai", model: "gpt-test"}
	resolverCalls := 0
	middleware := RuntimeModel(ModelResolverFunc(func(context.Context, string) (damodel.Chat, error) {
		resolverCalls++
		return base, nil
	}), RuntimeModelOptions{})
	for _, state := range []dastate.Values{nil, {RuntimeModelStateKey: "openai:gpt-test"}, {RuntimeModelStateKey: "gpt-test"}} {
		response, err := middleware.WrapModelCall(t.Context(), ModelRequest{Model: base, State: state}, func(_ context.Context, request ModelRequest) (ModelResponse, error) {
			if request.Model != base {
				t.Fatal("same-model request was replaced")
			}
			return ModelResponse{}, nil
		})
		if err != nil || len(response.Update) != 0 {
			t.Fatalf("response = %#v, %v", response, err)
		}
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d", resolverCalls)
	}
}

func TestRuntimeModelResolvesPersistedCheckpointSelection(t *testing.T) {
	base := &runtimeModelChat{provider: "base", model: "default"}
	selected := &runtimeModelChat{provider: "provider", model: "saved"}
	middleware := RuntimeModel(ModelResolverFunc(func(_ context.Context, spec string) (damodel.Chat, error) {
		if spec != "provider:saved" {
			t.Fatalf("resolved spec = %q", spec)
		}
		return selected, nil
	}), RuntimeModelOptions{})
	response, err := middleware.WrapModelCall(t.Context(), ModelRequest{
		Model: base, State: dastate.Values{RuntimeModelStateKey: "provider:saved"},
	}, func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		if request.Model != selected {
			t.Fatal("checkpoint-selected model was not used")
		}
		return ModelResponse{}, nil
	})
	if err != nil || len(response.Update) != 0 {
		t.Fatalf("response = %#v, %v", response, err)
	}
}

func TestRuntimeModelExplicitEmptySelectionClearsPersistedOverride(t *testing.T) {
	base := &runtimeModelChat{provider: "base", model: "default"}
	middleware := RuntimeModel(ModelResolverFunc(func(context.Context, string) (damodel.Chat, error) {
		t.Fatal("empty selection must not resolve a model")
		return nil, nil
	}), RuntimeModelOptions{})
	request := ModelRequest{
		Model: base, State: dastate.Values{RuntimeModelStateKey: "provider:old"},
		Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{RuntimeModelConfigKey: ""})},
	}
	response, err := middleware.WrapModelCall(t.Context(), request, func(context.Context, ModelRequest) (ModelResponse, error) { return ModelResponse{}, nil })
	if err != nil || response.Update[RuntimeModelStateKey] != "" {
		t.Fatalf("clear response = %#v, %v", response, err)
	}
}

func TestRuntimeModelRejectsInvalidSelectionAndSanitizesPanics(t *testing.T) {
	base := &runtimeModelChat{provider: "base", model: "default"}
	handler := func(context.Context, ModelRequest) (ModelResponse, error) {
		t.Fatal("handler should not run")
		return ModelResponse{}, nil
	}
	for name, value := range map[string]any{"wrong type": 42, "padded": " model", "control": "provider:model\n"} {
		t.Run(name, func(t *testing.T) {
			middleware := RuntimeModel(ModelResolverFunc(func(context.Context, string) (damodel.Chat, error) { return base, nil }), RuntimeModelOptions{})
			_, err := middleware.WrapModelCall(t.Context(), ModelRequest{Model: base, Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{"model": value})}}, handler)
			if !errors.Is(err, ErrInvalidRuntimeModel) {
				t.Fatalf("selection error = %v", err)
			}
		})
	}
	middleware := RuntimeModel(ModelResolverFunc(func(context.Context, string) (damodel.Chat, error) { panic("credential-value") }), RuntimeModelOptions{})
	_, err := middleware.WrapModelCall(t.Context(), ModelRequest{Model: base, Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{"model": "other"})}}, handler)
	if err == nil || stringsContains(err.Error(), "credential-value") {
		t.Fatalf("panic error = %v", err)
	}
	var nilModel *runtimeModelChat
	middleware = RuntimeModel(ModelResolverFunc(func(context.Context, string) (damodel.Chat, error) { return nilModel, nil }), RuntimeModelOptions{})
	if _, err := middleware.WrapModelCall(t.Context(), ModelRequest{Model: base, Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{"model": "other"})}}, handler); err == nil {
		t.Fatal("typed nil model was accepted")
	}
}

func TestRuntimeModelConcurrentRunsStayIsolatedAndPropagateCancellation(t *testing.T) {
	base := &runtimeModelChat{provider: "base", model: "default"}
	resolver := ModelResolverFunc(func(ctx context.Context, spec string) (damodel.Chat, error) {
		if spec == "blocked" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &runtimeModelChat{provider: "test", model: spec}, nil
	})
	middleware := RuntimeModel(resolver, RuntimeModelOptions{Ephemeral: true})
	seen := make(chan string, 2)
	var wait sync.WaitGroup
	for _, spec := range []string{"alpha", "beta"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := ModelRequest{Model: base, Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{"model": spec})}}
			_, err := middleware.WrapModelCall(t.Context(), request, func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				seen <- request.Model.Profile().Model
				return ModelResponse{}, nil
			})
			if err != nil {
				seen <- fmt.Sprintf("error:%v", err)
			}
		}()
	}
	wait.Wait()
	close(seen)
	values := map[string]bool{}
	for value := range seen {
		values[value] = true
	}
	if !values["alpha"] || !values["beta"] || len(values) != 2 {
		t.Fatalf("selected models = %#v", values)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := middleware.WrapModelCall(canceled, ModelRequest{Model: base, Runtime: Runtime{Configurable: datool.NewConfigurable(map[string]any{"model": "blocked"})}}, func(context.Context, ModelRequest) (ModelResponse, error) { return ModelResponse{}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestRuntimeModelRequiresResolverPositionally(t *testing.T) {
	var typedNil *testModelResolver
	for _, resolver := range []ModelResolver{nil, typedNil} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("nil resolver did not panic")
				}
			}()
			RuntimeModel(resolver, RuntimeModelOptions{})
		}()
	}
}

type testModelResolver struct{}

func (*testModelResolver) ResolveModel(context.Context, string) (damodel.Chat, error) {
	return nil, nil
}

func stringsContains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
