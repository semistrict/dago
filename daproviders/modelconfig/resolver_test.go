package modelconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/damodel"
	providerprofile "github.com/semistrict/dago/daproviders/profile"
)

type testChat struct{ profile damodel.Profile }

func (chat *testChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{}, nil
}
func (chat *testChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return nil, nil
}
func (chat *testChat) Profile() damodel.Profile { return chat.profile }

func newTestResolver(t *testing.T, environment map[string]string, factories map[string]Factory, options Options) (*Resolver, *dacredential.Store) {
	t.Helper()
	store := dacredential.NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, dacredential.Options{})
	lookup := func(name string) (string, bool) { value, ok := environment[name]; return value, ok }
	return NewResolver(store, lookup, factories, options), store
}

func TestPinnedProviderRegistry(t *testing.T) {
	want := []string{"anthropic", "azure_openai", "baseten", "bedrock", "cohere", "deepseek", "fireworks", "google_genai", "google_vertexai", "groq", "huggingface", "ibm", "litellm", "meta", "mistralai", "nvidia", "ollama", "openai", "openai_oauth", "openrouter", "perplexity", "together", "xai"}
	providers := Providers()
	got := make([]string, len(providers))
	for index, provider := range providers {
		got[index] = provider.Name
	}
	if !slices.Equal(got, want) {
		t.Fatalf("providers = %v, want %v", got, want)
	}
	providers[0].BaseURLEnvironments = append(providers[0].BaseURLEnvironments, "MUTATED")
	if slices.Contains(Providers()[0].BaseURLEnvironments, "MUTATED") {
		t.Fatal("Providers returned aliased declarations")
	}
	wantEnvironments := map[string]string{
		"anthropic": "ANTHROPIC_API_KEY", "azure_openai": "AZURE_OPENAI_API_KEY",
		"baseten": "BASETEN_API_KEY", "cohere": "COHERE_API_KEY", "deepseek": "DEEPSEEK_API_KEY",
		"fireworks": "FIREWORKS_API_KEY", "google_genai": "GOOGLE_API_KEY", "google_vertexai": "GOOGLE_CLOUD_PROJECT",
		"groq": "GROQ_API_KEY", "huggingface": "HUGGINGFACEHUB_API_TOKEN", "ibm": "WATSONX_APIKEY",
		"litellm": "LITELLM_API_KEY", "meta": "MODEL_API_KEY", "mistralai": "MISTRAL_API_KEY",
		"nvidia": "NVIDIA_API_KEY", "openai": "OPENAI_API_KEY", "openrouter": "OPENROUTER_API_KEY",
		"perplexity": "PPLX_API_KEY", "together": "TOGETHER_API_KEY", "xai": "XAI_API_KEY",
	}
	for _, provider := range Providers() {
		if want, exists := wantEnvironments[provider.Name]; exists && provider.CredentialEnvironment != want {
			t.Errorf("%s credential environment = %q, want %q", provider.Name, provider.CredentialEnvironment, want)
		}
	}
}

func TestResolverStaticInputsFailAtConstruction(t *testing.T) {
	credentials := dacredential.NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, dacredential.Options{})
	lookup := func(string) (string, bool) { return "", false }
	for name, construct := range map[string]func(){
		"nil credentials": func() { NewResolver(nil, lookup, map[string]Factory{}, Options{}) },
		"nil lookup":      func() { NewResolver(credentials, nil, map[string]Factory{}, Options{}) },
		"nil factories":   func() { NewResolver(credentials, lookup, nil, Options{}) },
		"invalid provider": func() {
			NewResolver(credentials, lookup, map[string]Factory{}, Options{Providers: []Provider{{Name: "invalid provider"}}})
		},
		"duplicate alias factory": func() {
			factory := func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
				return &testChat{}, nil
			}
			NewResolver(credentials, lookup, map[string]Factory{"azure": factory, "azure_openai": factory}, Options{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("construction did not panic")
				}
			}()
			construct()
		})
	}
}

func TestParsePinnedFamiliesAndExplicitProviders(t *testing.T) {
	resolver, _ := newTestResolver(t, nil, map[string]Factory{}, Options{})
	tests := map[string]Spec{
		"gpt-5.6-terra":                    {Provider: "openai", Model: "gpt-5.6-terra"},
		"command-r-plus":                   {Provider: "cohere", Model: "command-r-plus"},
		"mistral-large":                    {Provider: "mistralai", Model: "mistral-large"},
		"deepseek-chat":                    {Provider: "deepseek", Model: "deepseek-chat"},
		"grok-4":                           {Provider: "xai", Model: "grok-4"},
		"sonar-pro":                        {Provider: "perplexity", Model: "sonar-pro"},
		"claude-opus-5":                    {Provider: "anthropic", Model: "claude-opus-5"},
		"gemini-3.1-pro-preview":           {Provider: "google_genai", Model: "gemini-3.1-pro-preview"},
		"nemotron-3-super":                 {Provider: "nvidia", Model: "nemotron-3-super"},
		"nvidia/llama":                     {Provider: "nvidia", Model: "nvidia/llama"},
		"accounts/fireworks/models/kimi":   {Provider: "fireworks", Model: "accounts/fireworks/models/kimi"},
		"us.anthropic.claude-sonnet-4-5:0": {Provider: "bedrock", Model: "us.anthropic.claude-sonnet-4-5:0"},
		"azure:model":                      {Provider: "azure_openai", Model: "model"},
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := resolver.Parse(t.Context(), input)
			if err != nil || got != want {
				t.Fatalf("Parse() = %#v, %v, want %#v", got, err, want)
			}
		})
	}
	for _, input := range []string{"", " provider:model", "unknown:model", "provider:", "mystery"} {
		if _, err := resolver.Parse(t.Context(), input); err == nil {
			t.Fatalf("Parse(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseUsesVertexOnlyWhenItsCredentialIsExclusive(t *testing.T) {
	resolver, _ := newTestResolver(t, map[string]string{"GOOGLE_CLOUD_PROJECT": "project"}, map[string]Factory{}, Options{})
	for _, model := range []string{"claude-sonnet", "gemini-pro"} {
		spec, err := resolver.Parse(t.Context(), model)
		if err != nil || spec.Provider != "google_vertexai" {
			t.Fatalf("Parse(%q) = %#v, %v", model, spec, err)
		}
	}
	resolver, _ = newTestResolver(t, map[string]string{"GOOGLE_CLOUD_PROJECT": "project", "GOOGLE_API_KEY": "key"}, map[string]Factory{}, Options{})
	spec, _ := resolver.Parse(t.Context(), "gemini-pro")
	if spec.Provider != "google_genai" {
		t.Fatalf("provider = %q, want google_genai", spec.Provider)
	}
}

func TestCustomModelsResolveExactlyAndRejectAmbiguity(t *testing.T) {
	providers := []Provider{
		{Name: "first", Authentication: AuthenticationOptional, Models: map[string]Model{"exact": {}}},
		{Name: "second", Authentication: AuthenticationOptional, Models: map[string]Model{"other": {}}},
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{}, Options{Providers: providers})
	spec, err := resolver.Parse(t.Context(), "exact")
	if err != nil || spec.Provider != "first" {
		t.Fatalf("Parse = %#v, %v", spec, err)
	}
	providers[1].Models["exact"] = Model{}
	resolver, _ = newTestResolver(t, nil, map[string]Factory{}, Options{Providers: providers})
	if _, err := resolver.Parse(t.Context(), "exact"); !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("error = %v, want ErrAmbiguousProvider", err)
	}
}

func TestResolveAppliesCredentialEndpointAndOverridePrecedence(t *testing.T) {
	var capturedCredential dacredential.Resolution
	var captured Construction
	factory := func(_ context.Context, spec Spec, credential dacredential.Resolution, construction Construction) (damodel.Chat, error) {
		capturedCredential, captured = credential, construction
		construction.Parameters["factory_mutation"] = true
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	profiles := providerprofile.Profiles{"openai": {Options: map[string]any{"profile_default": "yes", "order": "profile"}}}
	resolver, store := newTestResolver(t, map[string]string{"OPENAI_BASE_URL": "https://environment.example/v1"}, map[string]Factory{"openai": factory}, Options{
		Profiles: profiles,
		Providers: []Provider{{
			Name: "openai", CredentialEnvironment: "OPENAI_API_KEY", RetryParameter: "max_retries",
			BaseURLEnvironments: []string{"OPENAI_BASE_URL"},
			Parameters:          map[string]any{"order": "provider", "provider": true, "max_retries": 2},
			ProfileOverrides:    map[string]any{"context_window": 100},
			Models:              map[string]Model{"model": {Parameters: map[string]any{"order": "model"}, ProfileOverrides: map[string]any{"context_window": 200}}},
		}},
	})
	secret := "fixture-" + "credential-value"
	if err := store.SetAPIKey(t.Context(), "openai", secret, "https://stored.example/v1/", ""); err != nil {
		t.Fatal(err)
	}
	baseURL := "https://request.example/v1/"
	retries := 0
	result, err := resolver.Resolve(t.Context(), "openai:model", ResolveOptions{
		Parameters:       map[string]any{"order": "request", "temperature": 0.2},
		ProfileOverrides: map[string]any{"context_window": 300}, BaseURL: &baseURL, MaxRetries: &retries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if capturedCredential.Source != dacredential.StoredSource || capturedCredential.Credential.APIKey.Key != secret {
		t.Fatalf("credential = %#v", capturedCredential)
	}
	if captured.BaseURL != "https://request.example/v1" || captured.Parameters["order"] != "request" || captured.Parameters["profile_default"] != "yes" || captured.Parameters["provider"] != true || captured.Parameters["max_retries"] != 0 || captured.ProfileOverrides["context_window"] != 300 {
		t.Fatalf("construction = %#v", captured)
	}
	if _, exists := result.Construction.Parameters["factory_mutation"]; exists {
		t.Fatal("factory mutated returned construction")
	}
	if result.Model.Profile().ContextWindow != 300 {
		t.Fatalf("runtime profile context window = %d", result.Model.Profile().ContextWindow)
	}
}

func TestStoredCredentialEndpointPairSuppressesEnvironment(t *testing.T) {
	var got string
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, construction Construction) (damodel.Chat, error) {
		got = construction.BaseURL
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	resolver, store := newTestResolver(t, map[string]string{"OPENAI_BASE_URL": "https://gateway.example/v1"}, map[string]Factory{"openai": factory}, Options{})
	if err := store.SetAPIKey(t.Context(), "openai", "stored-fixture-key", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), "openai:model", ResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("base URL = %q, stored key without endpoint must suppress inherited gateway", got)
	}
}

func TestEnvironmentPrefixWinsForCredentialAndBaseURL(t *testing.T) {
	var gotCredential, gotBaseURL string
	factory := func(_ context.Context, spec Spec, credential dacredential.Resolution, construction Construction) (damodel.Chat, error) {
		gotCredential = credential.Credential.APIKey.Key
		gotBaseURL = construction.BaseURL
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	environment := map[string]string{
		"OPENAI_API_KEY": "canonical-key", "DEEPAGENTS_CODE_OPENAI_API_KEY": "prefixed-key",
		"OPENAI_BASE_URL": "https://canonical.example", "DEEPAGENTS_CODE_OPENAI_BASE_URL": "https://prefixed.example",
	}
	resolver, _ := newTestResolver(t, environment, map[string]Factory{"openai": factory}, Options{})
	if _, err := resolver.Resolve(t.Context(), "openai:model", ResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	if gotCredential != "prefixed-key" || gotBaseURL != "https://prefixed.example" {
		t.Fatalf("credential/base URL = %q, %q", gotCredential, gotBaseURL)
	}
}

func TestStatusesAreUniformSecretFreeAndDoNotCallFactories(t *testing.T) {
	secret := "status-fixture-secret"
	var calls atomic.Int32
	resolver, _ := newTestResolver(t, map[string]string{"OPENAI_API_KEY": secret}, map[string]Factory{
		"openai": func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
			calls.Add(1)
			return nil, nil
		},
	}, Options{})
	statuses, err := resolver.Statuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var openAI, bedrock Status
	for _, status := range statuses {
		if strings.Contains(status.String(), secret) {
			t.Fatalf("status leaked credential: %s", status)
		}
		switch status.Provider {
		case "openai":
			openAI = status
		case "bedrock":
			bedrock = status
		}
	}
	if !openAI.Configured || !openAI.FactoryAvailable || openAI.CredentialSource != dacredential.EnvironmentSource {
		t.Fatalf("openai status = %#v", openAI)
	}
	if !bedrock.Configured || bedrock.Authentication != AuthenticationAmbient {
		t.Fatalf("bedrock status = %#v", bedrock)
	}
	if calls.Load() != 0 {
		t.Fatalf("Statuses called factory %d times", calls.Load())
	}
}

func TestResolveRequiresCredentialsOnlyForRequiredProviders(t *testing.T) {
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, _ Construction) (damodel.Chat, error) {
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{"openai": factory, "ollama": factory, "bedrock": factory}, Options{})
	if _, err := resolver.Resolve(t.Context(), "openai:model", ResolveOptions{}); !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("openai error = %v", err)
	}
	for _, spec := range []string{"ollama:model", "bedrock:model"} {
		if _, err := resolver.Resolve(t.Context(), spec, ResolveOptions{}); err != nil {
			t.Fatalf("Resolve(%q): %v", spec, err)
		}
	}
}

func TestResolveIsBoundedCancellableAndRejectsUnsafeEndpoints(t *testing.T) {
	var calls atomic.Int32
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, _ Construction) (damodel.Chat, error) {
		calls.Add(1)
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": factory}, Options{})
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := resolver.Resolve(cancelled, "ollama:model", ResolveOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	credentialURL := "https://user:" + "password@example.test"
	for _, endpoint := range []string{"file:///tmp/socket", credentialURL, "https://example.test/#fragment"} {
		if _, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{BaseURL: &endpoint}); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("endpoint %q error = %v", endpoint, err)
		}
	}
	deep := map[string]any{"value": true}
	for range 10 {
		deep = map[string]any{"nested": deep}
	}
	if _, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{Parameters: deep}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("deep options error = %v", err)
	}
	if _, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{ProfileOverrides: map[string]any{"unknown_capability": true}}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("unknown profile error = %v", err)
	}
	if _, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{ProfileOverrides: map[string]any{"provider": "spoofed"}}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("profile identity error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("factory called %d times for rejected inputs", calls.Load())
	}
}

func TestFactoryFailuresAreBoundedRedactedAndPreserveIdentity(t *testing.T) {
	sentinel := errors.New("transport failed")
	secret := "factory-fixture-secret"
	factory := func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
		return nil, fmt.Errorf("credential %s: %w", secret, sentinel)
	}
	resolver, store := newTestResolver(t, nil, map[string]Factory{"openai": factory}, Options{})
	if err := store.SetAPIKey(t.Context(), "openai", secret, "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := resolver.Resolve(t.Context(), "openai:model", ResolveOptions{})
	if !errors.Is(err, sentinel) || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error = %v", err)
	}
	panicResolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
		panic("panic-secret-must-not-escape")
	}}, Options{})
	if _, err := panicResolver.Resolve(t.Context(), "ollama:model", ResolveOptions{}); err == nil || strings.Contains(err.Error(), "panic-secret") {
		t.Fatalf("panic error = %v", err)
	}
	var typedNil *testChat
	nilResolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
		return typedNil, nil
	}}, Options{})
	if _, err := nilResolver.Resolve(t.Context(), "ollama:model", ResolveOptions{}); err == nil {
		t.Fatal("typed nil factory result accepted")
	}
	mismatchResolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
		return &testChat{profile: damodel.Profile{Provider: "openai", Model: "other"}}, nil
	}}, Options{})
	if _, err := mismatchResolver.Resolve(t.Context(), "ollama:model", ResolveOptions{}); err == nil {
		t.Fatal("mismatched factory identity accepted")
	}
}

func TestProviderProfileFailureAndPanicAreContainedBeforeFactory(t *testing.T) {
	sentinel := errors.New("profile unavailable")
	var calls atomic.Int32
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, _ Construction) (damodel.Chat, error) {
		calls.Add(1)
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	for name, profile := range map[string]providerprofile.Profile{
		"error": {PreInit: func(string) error { return sentinel }},
		"panic": {PreInit: func(string) error { panic("profile panic secret") }},
	} {
		t.Run(name, func(t *testing.T) {
			resolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": factory}, Options{Profiles: providerprofile.Profiles{"ollama": profile}})
			_, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{})
			if err == nil || strings.Contains(err.Error(), "panic secret") {
				t.Fatalf("error = %v", err)
			}
			if name == "error" && !errors.Is(err, sentinel) {
				t.Fatalf("error identity lost: %v", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("factory called %d times", calls.Load())
	}
}

func TestFactoryErrorRedactsSecretShapedRequestParameters(t *testing.T) {
	secret := "request-option-fixture-secret"
	factory := func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error) {
		return nil, fmt.Errorf("provider repeated %s", secret)
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": factory}, Options{})
	_, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{Parameters: map[string]any{"api_token": secret}})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error = %v", err)
	}
	construction := Construction{BaseURL: "https://internal.example/tenant-name", Parameters: map[string]any{"api_token": secret}}
	if formatted := fmt.Sprintf("%#v", construction); strings.Contains(formatted, secret) || strings.Contains(formatted, "tenant-name") {
		t.Fatalf("construction formatting leaked secret: %s", formatted)
	}
}

func TestResolverDefensivelyCopiesStaticAndRequestOptions(t *testing.T) {
	providerParams := map[string]any{"nested": map[string]any{"value": "original"}}
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, construction Construction) (damodel.Chat, error) {
		if construction.Parameters["nested"].(map[string]any)["value"] != "original" {
			return nil, errors.New("static options were aliased")
		}
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{"custom": factory}, Options{Providers: []Provider{{Name: "custom", Authentication: AuthenticationOptional, Parameters: providerParams}}})
	providerParams["nested"].(map[string]any)["value"] = "mutated"
	if _, err := resolver.Resolve(t.Context(), "custom:model", ResolveOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentResolveHasNoSharedMutableConstruction(t *testing.T) {
	var seen sync.Map
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, construction Construction) (damodel.Chat, error) {
		identifier := construction.Parameters["identifier"].(int)
		construction.Parameters["identifier"] = -1
		seen.Store(identifier, true)
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": factory}, Options{})
	var group sync.WaitGroup
	for index := range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := resolver.Resolve(t.Context(), "ollama:model", ResolveOptions{Parameters: map[string]any{"identifier": index}}); err != nil {
				t.Errorf("Resolve: %v", err)
			}
		}()
	}
	group.Wait()
	count := 0
	seen.Range(func(_, _ any) bool { count++; return true })
	if count != 32 {
		t.Fatalf("saw %d distinct constructions", count)
	}
}

func TestPreferencesPersistClearAndSelectDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	preferences := NewPreferenceStore(daconfig.NewStore(PreferenceManifest(), path, daconfig.StoreOptions{}))
	if err := preferences.SetDefault(t.Context(), "ollama:local"); err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetRecent(t.Context(), "openai:recent"); err != nil {
		t.Fatal(err)
	}
	if value, err := preferences.Default(t.Context()); err != nil || value != "ollama:local" {
		t.Fatalf("Default = %q, %v", value, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config permissions = %v, %v", info.Mode(), err)
	}
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, _ Construction) (damodel.Chat, error) {
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	resolver, _ := newTestResolver(t, nil, map[string]Factory{"ollama": factory, "openai": factory}, Options{})
	resolution, err := resolver.ResolvePreferred(t.Context(), preferences, ResolveOptions{})
	if err != nil || resolution.Spec.String() != "ollama:local" {
		t.Fatalf("ResolvePreferred = %v, %v", resolution.Spec, err)
	}
	removed, err := preferences.ClearDefault(t.Context())
	if err != nil || !removed {
		t.Fatalf("ClearDefault = %t, %v", removed, err)
	}
	if _, err := resolver.ResolvePreferred(t.Context(), preferences, ResolveOptions{}); !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("recent missing credential error = %v", err)
	}
	if err := preferences.SetDefault(t.Context(), "bare-model"); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("invalid default error = %v", err)
	}
}

func TestResolvePreferredUsesCredentialOrderWithoutDiscovery(t *testing.T) {
	var factoryCalls atomic.Int32
	factory := func(_ context.Context, spec Spec, _ dacredential.Resolution, _ Construction) (damodel.Chat, error) {
		factoryCalls.Add(1)
		return &testChat{profile: damodel.Profile{Provider: spec.Provider, Model: spec.Model}}, nil
	}
	environment := map[string]string{"ANTHROPIC_API_KEY": "anthropic-fixture"}
	resolver, _ := newTestResolver(t, environment, map[string]Factory{"anthropic": factory, "ollama": factory}, Options{})
	preferences := NewPreferenceStore(daconfig.NewStore(PreferenceManifest(), filepath.Join(t.TempDir(), "config.json"), daconfig.StoreOptions{}))
	resolution, err := resolver.ResolvePreferred(t.Context(), preferences, ResolveOptions{})
	if err != nil || resolution.Spec.String() != "anthropic:claude-opus-5" || factoryCalls.Load() != 1 {
		t.Fatalf("ResolvePreferred = %v, %v, calls=%d", resolution.Spec, err, factoryCalls.Load())
	}
	if _, err := resolver.Parse(t.Context(), "ollama:local"); err != nil || factoryCalls.Load() != 1 {
		t.Fatalf("Parse performed discovery: err=%v calls=%d", err, factoryCalls.Load())
	}
}

func TestResolvePreferredWithoutConfiguredCredentialsFailsDistinctly(t *testing.T) {
	resolver, _ := newTestResolver(t, nil, map[string]Factory{}, Options{})
	preferences := NewPreferenceStore(daconfig.NewStore(PreferenceManifest(), filepath.Join(t.TempDir(), "config.json"), daconfig.StoreOptions{}))
	if _, err := resolver.ResolvePreferred(t.Context(), preferences, ResolveOptions{}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("error = %v, want ErrNoCredentials", err)
	}
}
