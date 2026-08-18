package langsmithgateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
)

type gatewayChat struct{ provider, model string }

func (chat *gatewayChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{Message: damessage.Assistant(chat.model)}, nil
}
func (*gatewayChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}
func (chat *gatewayChat) Profile() damodel.Profile {
	return damodel.Profile{Provider: chat.provider, Model: chat.model}
}

func TestResolverRoutesPinnedProvidersWithPositionalCredential(t *testing.T) {
	key := "lsv2_gateway_secret"
	wantEndpoints := map[string]string{
		"anthropic:model":    DefaultEndpoint + "/anthropic",
		"baseten:model":      DefaultEndpoint + "/baseten",
		"fireworks:model":    DefaultEndpoint + "/fireworks",
		"google-genai:model": DefaultEndpoint + "/gemini",
		"openai:model":       DefaultEndpoint + "/openai/v1",
	}
	seen := map[string]string{}
	resolver := NewResolver(FactoryFunc(func(_ context.Context, endpoint, apiKey, modelSpec string) (damodel.Chat, error) {
		if apiKey != key {
			t.Fatal("factory did not receive positional gateway credential")
		}
		seen[modelSpec] = endpoint
		provider, model, _ := strings.Cut(modelSpec, ":")
		return &gatewayChat{provider: provider, model: model}, nil
	}), "", key, Options{})
	for spec := range wantEndpoints {
		if _, err := resolver.ResolveModel(t.Context(), spec); err != nil {
			t.Fatalf("resolve %q: %v", spec, err)
		}
	}
	if !reflect.DeepEqual(seen, wantEndpoints) {
		t.Fatalf("routes = %#v", seen)
	}
	if got := resolver.Providers(); !reflect.DeepEqual(got, []string{"anthropic", "baseten", "fireworks", "google_genai", "openai"}) {
		t.Fatalf("providers = %#v", got)
	}
}

func TestResolverCustomEndpointAndExactProviderSet(t *testing.T) {
	paths := map[string]string{"custom": "provider/v1"}
	resolver := NewResolver(FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) {
		return &gatewayChat{model: "model"}, nil
	}), "https://gateway.example.test/tenant/", "key", Options{ProviderPaths: paths})
	paths["custom"] = "mutated"
	endpoint, err := resolver.EndpointFor("custom:model")
	if err != nil || endpoint != "https://gateway.example.test/tenant/provider/v1" {
		t.Fatalf("endpoint = %q, %v", endpoint, err)
	}
	if _, err := resolver.EndpointFor("openai:model"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("explicit provider set leaked defaults: %v", err)
	}
}

func TestResolverNormalizesLoopbackAndRootEndpoints(t *testing.T) {
	resolver := NewResolver(FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) {
		return &gatewayChat{model: "model"}, nil
	}), "http://localhost:8080/", "key", Options{})
	endpoint, err := resolver.EndpointFor("openai:model")
	if err != nil || endpoint != "http://127.0.0.1:8080/openai/v1" {
		t.Fatalf("endpoint = %q, %v", endpoint, err)
	}
}

func TestResolverRejectsInvalidModelSpecsAndUnsupportedProviders(t *testing.T) {
	resolver := NewResolver(FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) {
		t.Fatal("invalid input reached factory")
		return nil, nil
	}), "", "key", Options{})
	for _, spec := range []string{"", "bare", ":model", "provider:", "provider: model", "provider:model ", " provider:model", "provider:model\n"} {
		if _, err := resolver.ResolveModel(t.Context(), spec); !errors.Is(err, ErrInvalidModelSpec) {
			t.Errorf("spec %q error = %v", spec, err)
		}
	}
	if _, err := resolver.ResolveModel(t.Context(), "groq:model"); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("unsupported provider error = %v", err)
	}
	bounded := NewResolver(resolver.factory, "", "key", Options{MaxModelSpecRunes: 8})
	if _, err := bounded.ResolveModel(t.Context(), "openai:model"); !errors.Is(err, ErrInvalidModelSpec) {
		t.Fatalf("oversized spec error = %v", err)
	}
}

func TestResolverStaticInputsPanicWithoutLeakingCredential(t *testing.T) {
	factory := FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) { return &gatewayChat{}, nil })
	var typedNil *testFactory
	for name, operation := range map[string]func(){
		"nil factory":       func() { NewResolver(nil, "", "key", Options{}) },
		"typed nil factory": func() { NewResolver(typedNil, "", "key", Options{}) },
		"empty key":         func() { NewResolver(factory, "", "", Options{}) },
		"padded key":        func() { NewResolver(factory, "", " secret ", Options{}) },
		"remote HTTP":       func() { NewResolver(factory, "http://gateway.example.test", "key", Options{}) },
		"userinfo":          func() { NewResolver(factory, "https://user@gateway.example.test", "key", Options{}) },
		"query":             func() { NewResolver(factory, "https://gateway.example.test?key=value", "key", Options{}) },
		"unsafe path":       func() { NewResolver(factory, "https://gateway.example.test/%2fescape", "key", Options{}) },
		"bad provider path": func() {
			NewResolver(factory, "", "key", Options{ProviderPaths: map[string]string{"openai": "../openai"}})
		},
		"bad spec bound": func() { NewResolver(factory, "", "key", Options{MaxModelSpecRunes: 4097}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				value := recover()
				if value == nil || strings.Contains(fmt.Sprint(value), "secret") {
					t.Fatalf("panic = %v", value)
				}
			}()
			operation()
		})
	}
}

func TestResolverCancellationAndFactoryFailuresAreSanitized(t *testing.T) {
	key := "lsv2_do_not_leak"
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	resolver := NewResolver(FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) {
		called = true
		return nil, nil
	}), "", key, Options{})
	if _, err := resolver.ResolveModel(canceled, "openai:model"); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("pre-cancellation = %v, called = %t", err, called)
	}

	var nilModel *gatewayChat
	for name, factory := range map[string]Factory{
		"error":     FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) { return nil, errors.New(key) }),
		"panic":     FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) { panic(key) }),
		"typed nil": FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) { return nilModel, nil }),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewResolver(factory, "", key, Options{}).ResolveModel(t.Context(), "openai:model")
			if !errors.Is(err, ErrFactory) || strings.Contains(err.Error(), key) {
				t.Fatalf("factory error = %v", err)
			}
		})
	}

	wrappedCancellation := NewResolver(FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) {
		return nil, fmt.Errorf("%s: %w", key, context.Canceled)
	}), "", key, Options{})
	if _, err := wrappedCancellation.ResolveModel(t.Context(), "openai:model"); err != context.Canceled {
		t.Fatalf("wrapped cancellation was not canonicalized: %v", err)
	}
}

func TestResolverFormattingDoesNotExposeCredential(t *testing.T) {
	key := "lsv2_do_not_format"
	resolver := NewResolver(FactoryFunc(func(context.Context, string, string, string) (damodel.Chat, error) {
		return &gatewayChat{}, nil
	}), "", key, Options{})
	for _, formatted := range []string{fmt.Sprint(resolver), fmt.Sprintf("%+v", resolver), fmt.Sprintf("%#v", resolver)} {
		if strings.Contains(formatted, key) {
			t.Fatalf("formatted resolver exposed credential: %s", formatted)
		}
	}
}

func TestResolverConcurrentCallsRemainIsolated(t *testing.T) {
	var lock sync.Mutex
	seen := map[string]string{}
	resolver := NewResolver(FactoryFunc(func(_ context.Context, endpoint, _ string, modelSpec string) (damodel.Chat, error) {
		lock.Lock()
		seen[modelSpec] = endpoint
		lock.Unlock()
		_, model, _ := strings.Cut(modelSpec, ":")
		return &gatewayChat{model: model}, nil
	}), "", "key", Options{})
	var wait sync.WaitGroup
	for _, spec := range []string{"openai:one", "anthropic:two", "fireworks:three"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := resolver.ResolveModel(t.Context(), spec); err != nil {
				t.Errorf("resolve %q: %v", spec, err)
			}
		}()
	}
	wait.Wait()
	lock.Lock()
	defer lock.Unlock()
	if len(seen) != 3 {
		t.Fatalf("seen = %#v", seen)
	}
}

type testFactory struct{}

func (*testFactory) NewGatewayModel(context.Context, string, string, string) (damodel.Chat, error) {
	return nil, nil
}
