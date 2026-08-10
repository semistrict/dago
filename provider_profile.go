package dago

import (
	"fmt"
	"sync"
)

// ProviderProfile supplies defaults and hooks to a caller that constructs a model.
// It is intentionally separate from Profile, which configures the agent harness.
type ProviderProfile struct {
	Options        map[string]any
	PreInit        func(modelSpec string) error
	OptionsFactory func() (map[string]any, error)
}

var providerProfileRegistry = struct {
	sync.RWMutex
	values map[string]ProviderProfile
}{values: map[string]ProviderProfile{}}

// RegisterProviderProfile installs or additively extends a provider or provider:model profile.
func RegisterProviderProfile(key string, profile ProviderProfile) error {
	if err := validateProfileKey(key); err != nil {
		return err
	}
	profile = cloneProviderProfile(profile)
	providerProfileRegistry.Lock()
	defer providerProfileRegistry.Unlock()
	if existing, exists := providerProfileRegistry.values[key]; exists {
		profile = MergeProviderProfiles(existing, profile)
	}
	providerProfileRegistry.values[key] = profile
	return nil
}

// LookupProviderProfile resolves an exact provider:model profile over its provider defaults.
func LookupProviderProfile(modelSpec string) (ProviderProfile, bool) {
	provider, exact, valid := splitProviderSpec(modelSpec)
	if !valid {
		return ProviderProfile{}, false
	}
	providerProfileRegistry.RLock()
	exactProfile, hasExact := providerProfileRegistry.values[exact]
	baseProfile, hasBase := providerProfileRegistry.values[provider]
	providerProfileRegistry.RUnlock()
	if exact == provider {
		if !hasExact {
			return ProviderProfile{}, false
		}
		return cloneProviderProfile(exactProfile), true
	}
	switch {
	case hasBase && hasExact:
		return MergeProviderProfiles(baseProfile, exactProfile), true
	case hasExact:
		return cloneProviderProfile(exactProfile), true
	case hasBase:
		return cloneProviderProfile(baseProfile), true
	default:
		return ProviderProfile{}, false
	}
}

// ApplyProviderProfile runs the resolved hook and merges static, dynamic, and caller options.
// Caller options have highest precedence. The returned map never aliases an input map.
func ApplyProviderProfile(modelSpec string, callerOptions map[string]any, runPreInit bool) (map[string]any, error) {
	result := cloneAnyMap(callerOptions)
	profile, exists := LookupProviderProfile(modelSpec)
	if !exists {
		return result, nil
	}
	if runPreInit && profile.PreInit != nil {
		if err := profile.PreInit(modelSpec); err != nil {
			return nil, fmt.Errorf("provider profile pre-initialization for %q: %w", modelSpec, err)
		}
	}
	merged := cloneAnyMap(profile.Options)
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

// MergeProviderProfiles layers override on base. Hooks and factories run base first.
func MergeProviderProfiles(base, override ProviderProfile) ProviderProfile {
	result := ProviderProfile{Options: cloneAnyMap(base.Options)}
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
		result = cloneAnyMap(result)
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

func cloneProviderProfile(profile ProviderProfile) ProviderProfile {
	profile.Options = cloneAnyMap(profile.Options)
	return profile
}
