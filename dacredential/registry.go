package dacredential

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const environmentPrefix = "DEEPAGENTS_CODE_"

// EnvironmentLookup reads one caller-owned environment snapshot.
type EnvironmentLookup func(string) (string, bool)

// Provider describes one pinned model-provider or service credential slot.
type Provider struct {
	Name        string `json:"name"`
	Environment string `json:"environment,omitempty"`
	Service     bool   `json:"service,omitempty"`
	OAuthOnly   bool   `json:"oauth_only,omitempty"`
}

var pinnedProviders = []Provider{
	{Name: "anthropic", Environment: "ANTHROPIC_API_KEY"},
	{Name: "azure_openai", Environment: "AZURE_OPENAI_API_KEY"},
	{Name: "baseten", Environment: "BASETEN_API_KEY"},
	{Name: "cohere", Environment: "COHERE_API_KEY"},
	{Name: "deepseek", Environment: "DEEPSEEK_API_KEY"},
	{Name: "fireworks", Environment: "FIREWORKS_API_KEY"},
	{Name: "google_genai", Environment: "GOOGLE_API_KEY"},
	{Name: "google_vertexai", Environment: "GOOGLE_CLOUD_PROJECT"},
	{Name: "groq", Environment: "GROQ_API_KEY"},
	{Name: "huggingface", Environment: "HUGGINGFACEHUB_API_TOKEN"},
	{Name: "ibm", Environment: "WATSONX_APIKEY"},
	{Name: "langsmith", Environment: "LANGSMITH_API_KEY", Service: true},
	{Name: "litellm", Environment: "LITELLM_API_KEY"},
	{Name: "meta", Environment: "MODEL_API_KEY"},
	{Name: "mistralai", Environment: "MISTRAL_API_KEY"},
	{Name: "nvidia", Environment: "NVIDIA_API_KEY"},
	{Name: "openai", Environment: "OPENAI_API_KEY"},
	{Name: "openai_oauth", OAuthOnly: true},
	{Name: "openrouter", Environment: "OPENROUTER_API_KEY"},
	{Name: "perplexity", Environment: "PPLX_API_KEY"},
	{Name: "tavily", Environment: "TAVILY_API_KEY", Service: true},
	{Name: "together", Environment: "TOGETHER_API_KEY"},
	{Name: "xai", Environment: "XAI_API_KEY"},
}

// Providers returns a defensive, name-sorted copy of the pinned registry.
func Providers() []Provider {
	providers := append([]Provider(nil), pinnedProviders...)
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	return providers
}

// ProviderByName returns pinned metadata for a normalized exact name.
func ProviderByName(name string) (Provider, bool) {
	for _, provider := range pinnedProviders {
		if provider.Name == name {
			return provider, true
		}
	}
	return Provider{}, false
}

// Source identifies where a credential resolved.
type Source string

const (
	StoredSource      Source = "stored"
	EnvironmentSource Source = "environment"
	MissingSource     Source = "missing"
)

// Resolution is one provider's resolved credential and source. Formatting is
// deliberately secret-free.
type Resolution struct {
	Provider    string
	Source      Source
	Environment string
	Credential  Credential
	Configured  bool
}

func (resolution Resolution) String() string {
	return fmt.Sprintf("Resolution(provider=%s,source=%s,configured=%t,<redacted>)", resolution.Provider, resolution.Source, resolution.Configured)
}

func (resolution Resolution) GoString() string { return resolution.String() }

// Resolve applies the pinned precedence: stored credentials first, then a
// present DEEPAGENTS_CODE_ override, then the canonical environment variable.
// A present empty prefixed value intentionally shadows the canonical value.
func (store *Store) Resolve(ctx context.Context, provider string, lookup EnvironmentLookup) (Resolution, error) {
	if lookup == nil {
		panic("dacredential: environment lookup is required")
	}
	provider, err := validateProvider(provider)
	if err != nil {
		return Resolution{}, err
	}
	snapshot, err := store.Load(ctx)
	if err != nil {
		return Resolution{}, err
	}
	if credential, ok := snapshot.Credential(provider); ok {
		return Resolution{Provider: provider, Source: StoredSource, Credential: credential, Configured: true}, nil
	}
	metadata, known := ProviderByName(provider)
	if !known || metadata.Environment == "" {
		return Resolution{Provider: provider, Source: MissingSource}, nil
	}
	prefixed := environmentPrefix + metadata.Environment
	if value, present := lookup(prefixed); present {
		return store.environmentResolution(provider, prefixed, value)
	}
	value, present := lookup(metadata.Environment)
	if !present {
		return Resolution{Provider: provider, Source: MissingSource, Environment: metadata.Environment}, nil
	}
	return store.environmentResolution(provider, metadata.Environment, value)
}

func (store *Store) environmentResolution(provider, environment, value string) (Resolution, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Resolution{Provider: provider, Source: MissingSource, Environment: environment}, nil
	}
	if err := validateSecret(value, store.options.MaxSecretBytes); err != nil {
		return Resolution{}, fmt.Errorf("%w: %s contains an invalid credential", ErrInvalidCredential, environment)
	}
	return Resolution{
		Provider: provider, Source: EnvironmentSource, Environment: environment, Configured: true,
		Credential: Credential{Type: APIKeyType, APIKey: &APIKeyCredential{Key: value}},
	}, nil
}
