// Package modelconfig resolves provider-qualified and bare model names into
// caller-owned model factories. It owns configuration precedence, not provider
// SDKs or network discovery.
package modelconfig

import (
	"fmt"
	"sort"
	"strings"
)

// Authentication describes how a provider obtains authority to make calls.
type Authentication string

const (
	AuthenticationRequired Authentication = "required"
	AuthenticationOptional Authentication = "optional"
	AuthenticationAmbient  Authentication = "ambient"
	AuthenticationOAuth    Authentication = "oauth"
)

// Model contains per-model construction and runtime-profile overrides.
type Model struct {
	Parameters       map[string]any
	ProfileOverrides map[string]any
}

// Provider is a static provider declaration. Parameters and profile overrides
// are provider-wide defaults; a matching Models entry wins over them.
type Provider struct {
	Name                  string
	Authentication        Authentication
	CredentialEnvironment string
	BaseURLEnvironments   []string
	BaseURL               string
	RetryParameter        string
	Parameters            map[string]any
	ProfileOverrides      map[string]any
	Models                map[string]Model
}

// Providers returns the pinned provider registry in name order. The returned
// values are defensive copies and include SDK-free providers such as Bedrock
// and Ollama because callers may supply factories for them.
func Providers() []Provider {
	providers := builtinProviders()
	result := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		result = append(result, cloneProvider(provider))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func builtinProviders() map[string]Provider {
	baseURLs := map[string][]string{
		"anthropic":    {"ANTHROPIC_BASE_URL", "ANTHROPIC_API_URL"},
		"azure_openai": {"AZURE_OPENAI_ENDPOINT"},
		"baseten":      {"BASETEN_BASE_URL", "BASETEN_API_BASE"},
		"cohere":       {"CO_API_URL"},
		"deepseek":     {"DEEPSEEK_API_BASE"},
		"fireworks":    {"FIREWORKS_BASE_URL", "FIREWORKS_API_BASE"},
		"google_genai": {"GOOGLE_GEMINI_BASE_URL"},
		"groq":         {"GROQ_BASE_URL", "GROQ_API_BASE"},
		"huggingface":  {"HF_INFERENCE_ENDPOINT"},
		"ibm":          {"WATSONX_URL"},
		"meta":         {"MODEL_API_BASE"},
		"mistralai":    {"MISTRAL_BASE_URL"},
		"nvidia":       {"NVIDIA_BASE_URL"},
		"openai":       {"OPENAI_BASE_URL", "OPENAI_API_BASE"},
		"openrouter":   {"OPENROUTER_API_BASE"},
		"perplexity":   {"PERPLEXITY_BASE_URL"},
		"together":     {"TOGETHER_API_BASE"},
		"xai":          {"XAI_API_BASE"},
	}
	retry := map[string]bool{
		"anthropic": true, "azure_openai": true, "baseten": true,
		"bedrock": true, "deepseek": true, "fireworks": true,
		"google_genai": true, "google_vertexai": true, "groq": true,
		"litellm": true, "meta": true, "mistralai": true, "openai": true,
		"openrouter": true, "perplexity": true, "together": true, "xai": true,
	}
	environments := map[string]string{
		"anthropic": "ANTHROPIC_API_KEY", "azure_openai": "AZURE_OPENAI_API_KEY",
		"baseten": "BASETEN_API_KEY", "cohere": "COHERE_API_KEY",
		"deepseek": "DEEPSEEK_API_KEY", "fireworks": "FIREWORKS_API_KEY",
		"google_genai": "GOOGLE_API_KEY", "google_vertexai": "GOOGLE_CLOUD_PROJECT",
		"groq": "GROQ_API_KEY", "huggingface": "HUGGINGFACEHUB_API_TOKEN",
		"ibm": "WATSONX_APIKEY", "litellm": "LITELLM_API_KEY",
		"meta": "MODEL_API_KEY", "mistralai": "MISTRAL_API_KEY",
		"nvidia": "NVIDIA_API_KEY", "openai": "OPENAI_API_KEY",
		"openrouter": "OPENROUTER_API_KEY", "perplexity": "PPLX_API_KEY",
		"together": "TOGETHER_API_KEY", "xai": "XAI_API_KEY",
	}
	names := []string{
		"anthropic", "azure_openai", "baseten", "bedrock", "claude_agent", "cohere", "deepseek",
		"fireworks", "google_genai", "google_vertexai", "groq", "huggingface",
		"ibm", "litellm", "meta", "mistralai", "nvidia", "ollama", "openai",
		"openai_oauth", "openrouter", "perplexity", "together", "xai",
	}
	result := make(map[string]Provider, len(names))
	for _, name := range names {
		authentication := AuthenticationRequired
		switch name {
		case "bedrock", "claude_agent", "google_vertexai":
			authentication = AuthenticationAmbient
		case "ollama":
			authentication = AuthenticationOptional
		case "openai_oauth":
			authentication = AuthenticationOAuth
		}
		provider := Provider{
			Name: name, Authentication: authentication,
			CredentialEnvironment: environments[name],
			BaseURLEnvironments:   append([]string(nil), baseURLs[name]...),
		}
		if retry[name] {
			provider.RetryParameter = "max_retries"
		}
		result[name] = provider
	}
	return result
}

func compileProviders(overrides []Provider) map[string]Provider {
	result := builtinProviders()
	for _, override := range overrides {
		validateProviderDeclaration(override)
		name := normalizeProvider(override.Name)
		override.Name = name
		result[name] = override
	}
	return result
}

func validateProviderDeclaration(provider Provider) {
	name := normalizeProvider(provider.Name)
	if name == "" || len(name) > 128 || name != provider.Name || !providerIdentifier(name) {
		panic(fmt.Sprintf("modelconfig: invalid provider declaration %q", provider.Name))
	}
	switch provider.Authentication {
	case "", AuthenticationRequired, AuthenticationOptional, AuthenticationAmbient, AuthenticationOAuth:
	default:
		panic(fmt.Sprintf("modelconfig: invalid authentication for %q", provider.Name))
	}
	if provider.Authentication == "" {
		provider.Authentication = AuthenticationRequired
	}
	validateEnvironment(provider.CredentialEnvironment)
	for _, environment := range provider.BaseURLEnvironments {
		validateEnvironment(environment)
	}
	if provider.RetryParameter != "" && !identifier(provider.RetryParameter) {
		panic(fmt.Sprintf("modelconfig: invalid retry parameter for %q", provider.Name))
	}
	for model := range provider.Models {
		if strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) || len(model) > 512 {
			panic(fmt.Sprintf("modelconfig: invalid model declaration for %q", provider.Name))
		}
	}
}

func validateEnvironment(value string) {
	if value == "" {
		return
	}
	if len(value) > 128 || value != strings.ToUpper(value) || !identifier(value) {
		panic(fmt.Sprintf("modelconfig: invalid environment declaration %q", value))
	}
}

func identifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || index > 0 && (character == '_' || character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func providerIdentifier(value string) bool {
	for index, character := range value {
		if character >= 'a' && character <= 'z' || index > 0 && (character == '_' || character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return value != ""
}

func normalizeProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "azure":
		return "azure_openai"
	case "mistral":
		return "mistralai"
	case "amazon_bedrock", "anthropic_bedrock", "aws", "bedrock_converse":
		return "bedrock"
	default:
		return value
	}
}

func cloneProvider(provider Provider) Provider {
	provider.BaseURLEnvironments = append([]string(nil), provider.BaseURLEnvironments...)
	provider.Parameters = cloneMap(provider.Parameters)
	provider.ProfileOverrides = cloneMap(provider.ProfileOverrides)
	provider.Models = cloneModels(provider.Models)
	return provider
}

func cloneModels(models map[string]Model) map[string]Model {
	if models == nil {
		return nil
	}
	result := make(map[string]Model, len(models))
	for name, model := range models {
		model.Parameters = cloneMap(model.Parameters)
		model.ProfileOverrides = cloneMap(model.ProfileOverrides)
		result[name] = model
	}
	return result
}
