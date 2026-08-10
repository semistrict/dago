package dago

import (
	"errors"
	"reflect"
	"testing"
)

func TestProviderProfilesMergeProviderExactDynamicAndCallerOptions(t *testing.T) {
	var order []string
	provider := "provider-profile-test"
	exact := provider + ":model-one"
	baseOptions := map[string]any{"base": true, "shared": "base"}
	if err := RegisterProviderProfile(provider, ProviderProfile{
		Options: baseOptions,
		PreInit: func(spec string) error {
			order = append(order, "base-pre:"+spec)
			return nil
		},
		OptionsFactory: func() (map[string]any, error) {
			order = append(order, "base-factory")
			return map[string]any{"base_dynamic": true, "dynamic_shared": "base"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	baseOptions["base"] = false
	if err := RegisterProviderProfile(exact, ProviderProfile{
		Options: map[string]any{"exact": true, "shared": "exact"},
		PreInit: func(spec string) error {
			order = append(order, "exact-pre:"+spec)
			return nil
		},
		OptionsFactory: func() (map[string]any, error) {
			order = append(order, "exact-factory")
			return map[string]any{"exact_dynamic": true, "dynamic_shared": "exact"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	caller := map[string]any{"shared": "caller"}
	got, err := ApplyProviderProfile(exact, caller, true)
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

func TestProviderProfileRegistrationIsAdditive(t *testing.T) {
	key := "provider-additive-test"
	if err := RegisterProviderProfile(key, ProviderProfile{Options: map[string]any{"one": 1, "shared": 1}}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterProviderProfile(key, ProviderProfile{Options: map[string]any{"two": 2, "shared": 2}}); err != nil {
		t.Fatal(err)
	}
	got, err := ApplyProviderProfile(key, nil, false)
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
	if err := RegisterProviderProfile(key, ProviderProfile{PreInit: func(string) error { return errors.New("blocked") }}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterProviderProfile(key, ProviderProfile{PreInit: func(string) error { called = true; return nil }}); err != nil {
		t.Fatal(err)
	}
	_, err := ApplyProviderProfile(key, nil, true)
	if err == nil || called {
		t.Fatalf("error = %v, override called = %v", err, called)
	}
}

func TestProviderProfileLookupRejectsMalformedSpecs(t *testing.T) {
	for _, spec := range []string{"", ":model", "provider:", "a:b:c", " provider"} {
		if _, ok := LookupProviderProfile(spec); ok {
			t.Fatalf("LookupProviderProfile(%q) matched", spec)
		}
	}
	options := map[string]any{"caller": true}
	got, err := ApplyProviderProfile("unregistered-provider:model", options, true)
	if err != nil || !reflect.DeepEqual(got, options) {
		t.Fatalf("options = %#v, error = %v", got, err)
	}
}
