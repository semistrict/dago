package profile

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

type mapChat map[string]any

func (mapChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{}, nil
}

func (mapChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) { return nil, nil }
func (mapChat) Profile() damodel.Profile                                        { return damodel.Profile{Provider: "openai", Model: "gpt-5"} }

func TestResolverAppliesProfilesAndSelectsNormalizedFactory(t *testing.T) {
	var gotSpec string
	var gotOptions map[string]any
	wantModel := modeltest.New(damodel.Profile{Provider: "azure", Model: "gpt-5"})
	resolver := Resolver{
		Profiles: Profiles{"azure_openai": {Options: map[string]any{"profile": true, "shared": "profile"}}},
		Factories: map[string]Factory{"azure": func(spec string, options map[string]any) (damodel.Chat, error) {
			gotSpec = spec
			gotOptions = options
			return wantModel, nil
		}},
	}
	got, err := resolver.Resolve("azure_openai:gpt-5", map[string]any{"shared": "caller"})
	if err != nil {
		t.Fatal(err)
	}
	if got != wantModel {
		t.Fatal("Resolve did not return the factory model")
	}
	if gotSpec != "azure_openai:gpt-5" || !reflect.DeepEqual(gotOptions, map[string]any{"profile": true, "shared": "caller"}) {
		t.Fatalf("factory input = %q, %#v", gotSpec, gotOptions)
	}
}

func TestResolverErrorsAreStable(t *testing.T) {
	resolver := Resolver{}
	if _, err := resolver.Resolve("gpt-5", nil); !errors.Is(err, ErrInvalidModelSpec) {
		t.Fatalf("bare spec error = %v", err)
	}
	if _, err := resolver.Resolve("openai:gpt-5", nil); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("missing factory error = %v", err)
	}
	resolver.Factories = map[string]Factory{"openai": func(string, map[string]any) (damodel.Chat, error) { return nil, nil }}
	if _, err := resolver.Resolve("openai:gpt-5", nil); err == nil {
		t.Fatal("nil factory model did not fail")
	}
}

func TestResolverAndMatchingRejectTypedNilModels(t *testing.T) {
	var model mapChat
	resolver := Resolver{Factories: map[string]Factory{
		"openai": func(string, map[string]any) (damodel.Chat, error) { return model, nil },
	}}
	if _, err := resolver.Resolve("openai:gpt-5", nil); err == nil || !strings.Contains(err.Error(), "returned nil") {
		t.Fatalf("typed-nil factory model error = %v", err)
	}
	if ModelMatchesSpec(model, "openai:gpt-5") {
		t.Fatal("typed-nil model matched a specification")
	}
	if IsBedrockModel(model) {
		t.Fatal("typed-nil model matched Bedrock")
	}
}

func TestNilModelCoversEveryNilableKind(t *testing.T) {
	var pointer *int
	var mapping map[string]int
	var function func()
	var slice []int
	var channel chan int
	var interfaceValue any
	for name, value := range map[string]any{
		"pointer": pointer, "map": mapping, "function": function,
		"slice": slice, "channel": channel, "interface": interfaceValue,
	} {
		t.Run(name, func(t *testing.T) {
			if !nilModel(value) {
				t.Fatal("typed nil was not detected")
			}
		})
	}
}

func TestResolverAcceptsColonInProviderNativeIdentifier(t *testing.T) {
	called := false
	resolver := Resolver{
		Profiles: Profiles{"bedrock": {Options: map[string]any{"profile": true}}},
		Factories: map[string]Factory{"bedrock": func(spec string, options map[string]any) (damodel.Chat, error) {
			called = spec == "bedrock:amazon.nova-pro-v1:0" && options["profile"] == true
			return modeltest.New(damodel.Profile{Provider: "bedrock", Model: "amazon.nova-pro-v1:0"}), nil
		}},
	}
	if _, err := resolver.Resolve("bedrock:amazon.nova-pro-v1:0", nil); err != nil || !called {
		t.Fatalf("Resolve error = %v, factory called = %v", err, called)
	}
}

func TestModelMatchesSpec(t *testing.T) {
	model := modeltest.New(damodel.Profile{Provider: "openai-" + "co" + "dex", Model: "gpt-5.5"})
	for _, spec := range []string{"gpt-5.5", "OPENAI_" + "CO" + "DEX:gpt-5.5", "openai-" + "co" + "dex:gpt-5.5"} {
		if !ModelMatchesSpec(model, spec) {
			t.Fatalf("ModelMatchesSpec(%q) = false", spec)
		}
	}
	if ModelMatchesSpec(model, "openai:gpt-5.5") || ModelMatchesSpec(model, "gpt-5") {
		t.Fatal("ModelMatchesSpec accepted a mismatched spec")
	}
	uninspectable := modeltest.New(damodel.Profile{Model: "provider-model"})
	if !ModelMatchesSpec(uninspectable, "anything:provider-model") {
		t.Fatal("unadvertised provider did not fall back to identifier matching")
	}
}

func TestIsBedrockModel(t *testing.T) {
	for _, value := range []any{
		"bedrock:anthropic.claude-3-5-sonnet-20240620-v1:0",
		"us.amazon.nova-pro-v1:0",
		modeltest.New(damodel.Profile{Provider: "anthropic-bedrock", Model: "claude"}),
	} {
		if !IsBedrockModel(value) {
			t.Fatalf("IsBedrockModel(%v) = false", value)
		}
	}
	for _, value := range []any{"anthropic:claude-3-opus", "amazon.titan-text-express-v1:0", nil} {
		if IsBedrockModel(value) {
			t.Fatalf("IsBedrockModel(%v) = true", value)
		}
	}
}

func TestProviderProfilesMergeProviderExactDynamicAndCallerOptions(t *testing.T) {
	var order []string
	provider := "provider-profile-test"
	exact := provider + ":model-one"
	baseOptions := map[string]any{"base": true, "shared": "base"}
	profiles := Profiles{provider: Profile{
		Options: baseOptions,
		PreInit: func(spec string) error {
			order = append(order, "base-pre:"+spec)
			return nil
		},
		OptionsFactory: func() (map[string]any, error) {
			order = append(order, "base-factory")
			return map[string]any{"base_dynamic": true, "dynamic_shared": "base"}, nil
		},
	}}
	profiles[exact] = Profile{
		Options: map[string]any{"exact": true, "shared": "exact"},
		PreInit: func(spec string) error {
			order = append(order, "exact-pre:"+spec)
			return nil
		},
		OptionsFactory: func() (map[string]any, error) {
			order = append(order, "exact-factory")
			return map[string]any{"exact_dynamic": true, "dynamic_shared": "exact"}, nil
		},
	}

	caller := map[string]any{"shared": "caller"}
	got, err := profiles.ApplyWithPreInit(exact, caller)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"base": true, "exact": true, "shared": "caller",
		"base_dynamic": true, "exact_dynamic": true, "dynamic_shared": "exact",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
	wantOrder := []string{"base-pre:" + exact, "exact-pre:" + exact, "base-factory", "exact-factory"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %#v, want %#v", order, wantOrder)
	}
	got["shared"] = "mutated"
	if caller["shared"] != "caller" {
		t.Fatal("result aliases caller options")
	}
}

func TestProviderProfilesMergeExplicitly(t *testing.T) {
	key := "provider-additive-test"
	profiles := Profiles{key: Merge(
		Profile{Options: map[string]any{"one": 1, "shared": 1}},
		Profile{Options: map[string]any{"two": 2, "shared": 2}},
	)}
	got, err := profiles.Apply(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]any{"one": 1, "two": 2, "shared": 2}) {
		t.Fatalf("options = %#v", got)
	}
}

func TestProviderProfileStopsChainedHooksOnError(t *testing.T) {
	key := "provider-hook-error-test:model"
	called := false
	profiles := Profiles{key: Merge(
		Profile{PreInit: func(string) error { return errors.New("blocked") }},
		Profile{PreInit: func(string) error { called = true; return nil }},
	)}
	_, err := profiles.ApplyWithPreInit(key, nil)
	if err == nil || called {
		t.Fatalf("error = %v, override called = %v", err, called)
	}
}

func TestProviderProfileLookupRejectsMalformedSpecs(t *testing.T) {
	for _, spec := range []string{"", ":model", "provider:", " provider"} {
		if _, ok := (Profiles{}).Lookup(spec); ok {
			t.Fatalf("Lookup(%q) matched", spec)
		}
	}
	options := map[string]any{"caller": true}
	got, err := (Profiles{}).ApplyWithPreInit("unregistered-provider:model", options)
	if err != nil || !reflect.DeepEqual(got, options) {
		t.Fatalf("options = %#v, error = %v", got, err)
	}
}

func TestRegisteredProviderProfileLayersIntoBuiltinButNotExplicitSets(t *testing.T) {
	key := "openai:provider-plugin-overlay-test"
	if err := RegisterProviderProfile(key, Profile{Options: map[string]any{"plugin": true}}); err != nil {
		t.Fatal(err)
	}
	got, err := Builtin().Apply(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["use_responses_api"] != true || got["plugin"] != true {
		t.Fatalf("built-in and plugin options did not merge: %#v", got)
	}
	var resolvedOptions map[string]any
	resolver := Resolver{Factories: map[string]Factory{"openai": func(_ string, options map[string]any) (damodel.Chat, error) {
		resolvedOptions = options
		return modeltest.New(damodel.Profile{Provider: "openai", Model: "provider-plugin-overlay-test"}), nil
	}}}
	if _, err := resolver.Resolve(key, nil); err != nil {
		t.Fatal(err)
	}
	if resolvedOptions["use_responses_api"] != true || resolvedOptions["plugin"] != true {
		t.Fatalf("default resolver lookup did not merge profiles: %#v", resolvedOptions)
	}
	if _, exists := (Profiles{}).Lookup(key); exists {
		t.Fatal("registered profile leaked into an explicit profile set")
	}
	explicitOptions := map[string]any{}
	resolver.Profiles = Profiles{}
	resolver.Factories["openai"] = func(_ string, options map[string]any) (damodel.Chat, error) {
		explicitOptions = options
		return modeltest.New(damodel.Profile{Provider: "openai", Model: "provider-plugin-overlay-test"}), nil
	}
	if _, err := resolver.Resolve(key, nil); err != nil {
		t.Fatal(err)
	}
	if len(explicitOptions) != 0 {
		t.Fatalf("registered profile leaked into resolver's explicit set: %#v", explicitOptions)
	}
}

func TestRegisterProviderProfileIsConcurrentAndAdditive(t *testing.T) {
	key := "provider-plugin-concurrency-test:model"
	const count = 32
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			name := fmt.Sprintf("option_%d", index)
			if err := RegisterProviderProfile(key, Profile{Options: map[string]any{name: index}}); err != nil {
				t.Errorf("register %s: %v", name, err)
			}
		}()
	}
	wait.Wait()
	got, exists := Builtin().Lookup(key)
	if !exists || len(got.Options) != count {
		t.Fatalf("registered profile has %d options, exists = %v", len(got.Options), exists)
	}
}

func TestLoadProviderProfilePluginsIsolatesFailures(t *testing.T) {
	key := "provider-plugin-failure-test"
	failures := LoadProviderProfilePlugins(
		ProviderProfilePlugin{Name: "error", Register: func() error { return errors.New("unavailable") }},
		ProviderProfilePlugin{Name: "panic", Register: func() error { panic("broken") }},
		ProviderProfilePlugin{Name: "working", Register: func() error {
			return RegisterProviderProfile(key, Profile{Options: map[string]any{"loaded": true}})
		}},
	)
	if len(failures) != 2 || !strings.Contains(failures[0].Error(), "error") || !strings.Contains(failures[1].Error(), "panic") {
		t.Fatalf("failures = %#v", failures)
	}
	got, exists := Builtin().Lookup(key)
	if !exists || got.Options["loaded"] != true {
		t.Fatalf("later plugin was not loaded: %#v, exists = %v", got, exists)
	}
}
