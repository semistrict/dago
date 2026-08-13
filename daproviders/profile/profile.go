// Package profile applies explicit provider-construction profiles.
package profile

import (
	"fmt"
	"os"
	"strings"
)

// Profile supplies defaults and hooks to a caller that constructs a model.
type Profile struct {
	Options        map[string]any
	PreInit        func(modelSpec string) error
	OptionsFactory func() (map[string]any, error)
}

// Profiles is an explicit set of provider and provider:model construction profiles.
type Profiles map[string]Profile

// Lookup resolves an exact provider:model profile over its provider defaults.
func (profiles Profiles) Lookup(modelSpec string) (Profile, bool) {
	provider, exact, valid := splitProviderSpec(modelSpec)
	if !valid {
		return Profile{}, false
	}
	exactProfile, hasExact := profiles[exact]
	baseProfile, hasBase := profiles[provider]
	if exact == provider {
		if !hasExact {
			return Profile{}, false
		}
		return cloneProfile(exactProfile), true
	}
	switch {
	case hasBase && hasExact:
		return Merge(baseProfile, exactProfile), true
	case hasExact:
		return cloneProfile(exactProfile), true
	case hasBase:
		return cloneProfile(baseProfile), true
	default:
		return Profile{}, false
	}
}

// Apply merges static, dynamic, and caller options without running pre-initialization hooks.
// Caller options have highest precedence. The returned map never aliases an input map.
func (profiles Profiles) Apply(modelSpec string, callerOptions map[string]any) (map[string]any, error) {
	return profiles.apply(modelSpec, callerOptions, false)
}

// ApplyWithPreInit runs the resolved pre-initialization hook before merging options.
func (profiles Profiles) ApplyWithPreInit(modelSpec string, callerOptions map[string]any) (map[string]any, error) {
	return profiles.apply(modelSpec, callerOptions, true)
}

func (profiles Profiles) apply(modelSpec string, callerOptions map[string]any, runPreInit bool) (map[string]any, error) {
	result := cloneMap(callerOptions)
	profile, exists := profiles.Lookup(modelSpec)
	if !exists {
		return result, nil
	}
	if runPreInit && profile.PreInit != nil {
		if err := profile.PreInit(modelSpec); err != nil {
			return nil, fmt.Errorf("provider profile pre-initialization for %q: %w", modelSpec, err)
		}
	}
	merged := cloneMap(profile.Options)
	if profile.OptionsFactory != nil {
		dynamic, err := profile.OptionsFactory()
		if err != nil {
			return nil, fmt.Errorf("provider profile options for %q: %w", modelSpec, err)
		}
		for key, value := range dynamic {
			merged[key] = value
		}
	}
	for key, value := range result {
		merged[key] = value
	}
	return merged, nil
}

// Merge layers override on base. Hooks and factories run base first.
func Merge(base, override Profile) Profile {
	result := Profile{Options: cloneMap(base.Options)}
	for key, value := range override.Options {
		result.Options[key] = value
	}
	result.PreInit = chainProviderPreInit(base.PreInit, override.PreInit)
	result.OptionsFactory = chainProviderFactories(base.OptionsFactory, override.OptionsFactory)
	return result
}

func chainProviderPreInit(base, override func(string) error) func(string) error {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}
	return func(modelSpec string) error {
		if err := base(modelSpec); err != nil {
			return err
		}
		return override(modelSpec)
	}
}

func chainProviderFactories(base, override func() (map[string]any, error)) func() (map[string]any, error) {
	if base == nil {
		return override
	}
	if override == nil {
		return base
	}
	return func() (map[string]any, error) {
		result, err := base()
		if err != nil {
			return nil, err
		}
		result = cloneMap(result)
		values, err := override()
		if err != nil {
			return nil, err
		}
		for key, value := range values {
			result[key] = value
		}
		return result, nil
	}
}

func splitProviderSpec(value string) (provider, exact string, valid bool) {
	if err := validateProfileKey(value); err != nil {
		return "", "", false
	}
	provider = value
	for index, char := range value {
		if char == ':' {
			provider = value[:index]
			break
		}
	}
	return provider, value, true
}

func cloneProfile(profile Profile) Profile {
	profile.Options = cloneMap(profile.Options)
	return profile
}

func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func validateProfileKey(name string) error {
	if name == "" || name != strings.TrimSpace(name) || strings.Count(name, ":") > 1 {
		return fmt.Errorf("invalid provider profile key %q", name)
	}
	if provider, model, found := strings.Cut(name, ":"); found && (provider == "" || model == "" || provider != strings.TrimSpace(provider) || model != strings.TrimSpace(model)) {
		return fmt.Errorf("invalid provider profile key %q", name)
	}
	return nil
}

const (
	openRouterAppURL   = "https://github.com/langchain-ai/deepagents"
	openRouterAppTitle = "Deep Agents"
)

// Builtin returns the standard provider-construction defaults as a fresh set.
func Builtin() Profiles {
	return Profiles{
		"openai": {Options: map[string]any{"use_responses_api": true}},
		"nvidia": {OptionsFactory: func() (map[string]any, error) {
			return map[string]any{"default_headers": map[string]string{"X-BILLING-INVOKE-ORIGIN": "DeepAgents"}}, nil
		}},
		"openrouter": {OptionsFactory: openRouterDefaults},
	}
}

func openRouterDefaults() (map[string]any, error) {
	result := map[string]any{}
	if _, exists := os.LookupEnv("OPENROUTER_APP_URL"); !exists {
		result["app_url"] = openRouterAppURL
	}
	if _, exists := os.LookupEnv("OPENROUTER_APP_TITLE"); !exists {
		result["app_title"] = openRouterAppTitle
	}
	if !environmentTruthy("DEEPAGENTS_OPENROUTER_ALLOW_AZURE") {
		result["openrouter_provider"] = map[string]any{"ignore": []string{"azure"}}
	}
	return result, nil
}

func environmentTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
