package dacredential

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProviderRegistryAndResolutionPrecedence(t *testing.T) {
	store := testStore(t)
	if err := store.SetAPIKey(t.Context(), "openai", "stored-secret", "https://stored.example/v1", ""); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"DEEPAGENTS_CODE_OPENAI_API_KEY": "prefixed-secret",
		"OPENAI_API_KEY":                 "canonical-secret",
		"ANTHROPIC_API_KEY":              "anthropic-secret",
	}
	lookup := func(name string) (string, bool) { value, ok := environment[name]; return value, ok }
	resolution, err := store.Resolve(t.Context(), "openai", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Source != StoredSource || resolution.Credential.APIKey == nil || resolution.Credential.APIKey.Key != "stored-secret" {
		t.Fatalf("stored resolution = %#v", resolution)
	}
	resolution, err = store.Resolve(t.Context(), "anthropic", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Source != EnvironmentSource || resolution.Environment != "ANTHROPIC_API_KEY" || resolution.Credential.APIKey.Key != "anthropic-secret" {
		t.Fatalf("environment resolution = %#v", resolution)
	}
	environment["DEEPAGENTS_CODE_ANTHROPIC_API_KEY"] = ""
	resolution, err = store.Resolve(t.Context(), "anthropic", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Source != MissingSource || resolution.Configured || resolution.Environment != "DEEPAGENTS_CODE_ANTHROPIC_API_KEY" {
		t.Fatalf("empty prefixed shadow = %#v", resolution)
	}
	for _, rendered := range []string{fmt.Sprint(resolution), fmt.Sprintf("%#v", resolution)} {
		for _, secret := range []string{"stored-secret", "prefixed-secret", "canonical-secret", "anthropic-secret"} {
			if strings.Contains(rendered, secret) {
				t.Fatalf("resolution formatting leaked %q: %s", secret, rendered)
			}
		}
	}
}

func TestRegistryIncludesPinnedProvidersAndServices(t *testing.T) {
	providers := Providers()
	if len(providers) != 23 {
		t.Fatalf("provider count = %d", len(providers))
	}
	for _, expected := range []string{"anthropic", "google_vertexai", "openai", "openai_oauth", "langsmith", "tavily", "xai"} {
		provider, ok := ProviderByName(expected)
		if !ok || provider.Name != expected {
			t.Fatalf("missing provider %q", expected)
		}
	}
	if provider, _ := ProviderByName("openai_oauth"); !provider.OAuthOnly || provider.Environment != "" {
		t.Fatalf("openai_oauth = %#v", provider)
	}
	if provider, _ := ProviderByName("tavily"); !provider.Service || provider.Environment != "TAVILY_API_KEY" {
		t.Fatalf("tavily = %#v", provider)
	}
	if provider, _ := ProviderByName("langsmith"); !provider.Service || provider.Environment != "LANGSMITH_API_KEY" {
		t.Fatalf("langsmith = %#v", provider)
	}
}

func TestServiceResolutionUsesStoredPrefixedAndCanonicalPrecedence(t *testing.T) {
	store := testStore(t)
	environment := map[string]string{
		"DEEPAGENTS_CODE_TAVILY_API_KEY": "prefixed-tavily",
		"TAVILY_API_KEY":                 "canonical-tavily",
		"LANGSMITH_API_KEY":              "canonical-langsmith",
	}
	lookup := func(name string) (string, bool) { value, ok := environment[name]; return value, ok }
	resolution, err := store.Resolve(t.Context(), "tavily", lookup)
	if err != nil || resolution.Source != EnvironmentSource || resolution.Environment != "DEEPAGENTS_CODE_TAVILY_API_KEY" || resolution.Credential.APIKey.Key != "prefixed-tavily" {
		t.Fatalf("prefixed Tavily resolution = %#v, %v", resolution, err)
	}
	resolution, err = store.Resolve(t.Context(), "langsmith", lookup)
	if err != nil || resolution.Source != EnvironmentSource || resolution.Environment != "LANGSMITH_API_KEY" || resolution.Credential.APIKey.Key != "canonical-langsmith" {
		t.Fatalf("canonical LangSmith resolution = %#v, %v", resolution, err)
	}
	if err := store.SetAPIKey(t.Context(), "tavily", "stored-tavily", "", ""); err != nil {
		t.Fatal(err)
	}
	resolution, err = store.Resolve(t.Context(), "tavily", lookup)
	if err != nil || resolution.Source != StoredSource || resolution.Credential.APIKey.Key != "stored-tavily" {
		t.Fatalf("stored Tavily resolution = %#v, %v", resolution, err)
	}
	delete(environment, "DEEPAGENTS_CODE_TAVILY_API_KEY")
	environment["TAVILY_API_KEY"] = strings.Repeat("x", DefaultOptions().MaxSecretBytes+1)
	if _, err := store.Resolve(t.Context(), "langsmith", func(name string) (string, bool) {
		if name == "LANGSMITH_API_KEY" {
			return "unsafe\r\nvalue", true
		}
		return "", false
	}); !errors.Is(err, ErrInvalidCredential) || strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("control-bearing environment error = %v", err)
	}
	if _, err := store.Remove(t.Context(), "tavily"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resolve(t.Context(), "tavily", lookup); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("oversized environment error = %v", err)
	}
}

func TestDefaultPathAndResolverStaticInputs(t *testing.T) {
	if got := DefaultPath("/home/operator"); got != "/home/operator/.deepagents/.state/auth.json" {
		t.Fatalf("DefaultPath = %q", got)
	}
	store := NewStore("auth.json", time.Now, Options{})
	defer func() {
		if recover() == nil {
			t.Fatal("nil lookup did not panic")
		}
	}()
	_, _ = store.Resolve(t.Context(), "openai", nil)
}
