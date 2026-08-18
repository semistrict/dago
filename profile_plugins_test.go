package dago

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestRegisteredHarnessProfilesMergeProviderExactAndExplicit(t *testing.T) {
	provider := "harness-plugin-merge-test"
	exact := provider + ":model"
	providerSuffix := "provider suffix"
	exactSuffix := "exact suffix"
	explicitSuffix := "explicit suffix"
	if err := RegisterHarnessProfile(provider, Profile{
		SystemPrompt:       "provider prompt",
		SystemPromptSuffix: &providerSuffix,
		ToolDescriptions:   map[string]string{"provider_tool": "provider"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterHarnessProfile(exact, Profile{
		SystemPrompt:       "exact prompt",
		SystemPromptSuffix: &exactSuffix,
		ToolDescriptions:   map[string]string{"exact_tool": "exact"},
	}); err != nil {
		t.Fatal(err)
	}

	model := modeltest.New(damodel.Profile{Provider: provider, Model: "model"})
	got, err := resolveProfiles(model, []Profile{{
		SystemPromptSuffix: &explicitSuffix,
		ToolDescriptions:   map[string]string{"explicit_tool": "explicit"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.SystemPrompt != "provider prompt\n\nexact prompt" {
		t.Fatalf("system prompt = %q", got.SystemPrompt)
	}
	if got.SystemPromptSuffix == nil || *got.SystemPromptSuffix != explicitSuffix {
		t.Fatalf("suffix = %#v", got.SystemPromptSuffix)
	}
	for name, want := range map[string]string{
		"provider_tool": "provider",
		"exact_tool":    "exact",
		"explicit_tool": "explicit",
	} {
		if got.ToolDescriptions[name] != want {
			t.Fatalf("tool description %q = %q, want %q", name, got.ToolDescriptions[name], want)
		}
	}
}

func TestRegisterHarnessProfileIsConcurrentAndAdditive(t *testing.T) {
	key := "harness-plugin-concurrency-test:model"
	const count = 32
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			name := fmt.Sprintf("tool_%d", index)
			if err := RegisterHarnessProfile(key, Profile{ToolDescriptions: map[string]string{name: name}}); err != nil {
				t.Errorf("register %s: %v", name, err)
			}
		}()
	}
	wait.Wait()
	got, exists := registeredHarnessProfile("harness-plugin-concurrency-test", "model")
	if !exists || len(got.ToolDescriptions) != count {
		t.Fatalf("registered profile has %d tool descriptions, exists = %v", len(got.ToolDescriptions), exists)
	}
}

func TestLoadHarnessProfilePluginsIsolatesFailures(t *testing.T) {
	key := "harness-plugin-failure-test"
	failures := LoadHarnessProfilePlugins(
		HarnessProfilePlugin{Name: "error", Register: func() error { return errors.New("unavailable") }},
		HarnessProfilePlugin{Name: "panic", Register: func() error { panic("broken") }},
		HarnessProfilePlugin{Name: "working", Register: func() error {
			return RegisterHarnessProfile(key, Profile{SystemPrompt: "loaded"})
		}},
	)
	if len(failures) != 2 || !strings.Contains(failures[0].Error(), "error") || !strings.Contains(failures[1].Error(), "panic") {
		t.Fatalf("failures = %#v", failures)
	}
	got, exists := registeredHarnessProfile(key, "")
	if !exists || got.SystemPrompt != "loaded" {
		t.Fatalf("later plugin was not loaded: %#v, exists = %v", got, exists)
	}
}
