package dacode

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/daproviders/modelconfig"
)

func TestParseCLIModelConfigurationFlags(t *testing.T) {
	options, err := parseCLI([]string{
		"--model", "openai:test-model",
		"--model-params", `{"context_window":4096,"web_search":true}`,
		"--profile-override", `{"max_input_tokens":2048}`,
		"--max-retries", "0",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if options.model != "openai:test-model" || options.modelParameters["web_search"] != true || options.profileOverride["max_input_tokens"] == nil || options.maxRetries == nil || *options.maxRetries != 0 {
		t.Fatalf("options = %#v", options)
	}
	for _, arguments := range [][]string{
		{"--model-params", "[]"}, {"--model-params", "{bad"},
		{"--profile-override", "null"}, {"--max-retries", "-1"}, {"--max-retries", "101"},
	} {
		if _, err := parseCLI(arguments, &bytes.Buffer{}); err == nil {
			t.Fatalf("parseCLI(%v) unexpectedly succeeded", arguments)
		}
	}
}

func TestDefaultModelCommandPersistsShowsAndClearsWithoutAuthentication(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(t.TempDir(), "config.json")
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"--config", configPath, "--default-model", "claude-opus-5"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "anthropic:claude-opus-5") {
		t.Fatalf("set output = %q", output.String())
	}
	output.Reset()
	if err := Run(t.Context(), []string{"--config", configPath, "--default-model"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Default model: anthropic:claude-opus-5\n" {
		t.Fatalf("show output = %q", output.String())
	}
	output.Reset()
	if err := Run(t.Context(), []string{"--config", configPath, "--clear-default-model"}, strings.NewReader(""), &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Default model cleared.\n" {
		t.Fatalf("clear output = %q", output.String())
	}
	info, err := os.Stat(configPath)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config mode = %v, %v", info.Mode(), err)
	}
}

func TestConfiguredModelAuthenticationUsesOfflineFactoriesAndNoProviderDiscovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "router-fixture-key")
	authentication, err := resolveConfiguredModelAuthentication(t.Context(), "openrouter:test-model", "", t.TempDir(), nil, modelconfig.ResolveOptions{
		Parameters:       map[string]any{"context_window": 4096},
		ProfileOverrides: map[string]any{"max_input_tokens": 2048},
		MaxRetries:       new(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := authentication.resolveModel(t.Context(), "openrouter:test-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if profile := chat.Profile(); profile.Provider != "openrouter" || profile.Model != "test-model" || profile.ContextWindow != 2048 {
		t.Fatalf("profile = %#v", profile)
	}
	if _, err := resolveConfiguredModelAuthentication(t.Context(), "anthropic:claude-opus-5", "", t.TempDir(), nil, modelconfig.ResolveOptions{}); err != nil {
		t.Fatalf("configuration should not authenticate or probe unavailable providers: %v", err)
	}
}

func TestConfiguredModelAuthenticationRejectsUnavailableProviderAtConstruction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-fixture-key")
	authentication, err := resolveConfiguredModelAuthentication(t.Context(), "anthropic:claude-opus-5", "", t.TempDir(), nil, modelconfig.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authentication.resolveModel(t.Context(), "anthropic:claude-opus-5", "")
	if !errors.Is(err, modelconfig.ErrProviderUnavailable) {
		t.Fatalf("error = %v, want ErrProviderUnavailable", err)
	}
}

func TestOpenAIModelsEnableHostedWebSearchByDefault(t *testing.T) {
	credential := dacredential.Resolution{Credential: dacredential.Credential{
		Type: dacredential.APIKeyType, APIKey: &dacredential.APIKeyCredential{Key: "fixture-key"},
	}}
	chat, err := openAIModelFactory(t.Context(), modelconfig.Spec{Provider: "openai", Model: "gpt-test"}, credential, modelconfig.Construction{})
	if err != nil {
		t.Fatal(err)
	}
	if !chat.Profile().SupportsWebSearch {
		t.Fatal("OpenAI hosted web search is disabled by default")
	}

	chat, err = openAIModelFactory(t.Context(), modelconfig.Spec{Provider: "openai", Model: "gpt-test"}, credential, modelconfig.Construction{
		Parameters: map[string]any{"web_search": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if chat.Profile().SupportsWebSearch {
		t.Fatal("explicit web_search=false was ignored")
	}
}
