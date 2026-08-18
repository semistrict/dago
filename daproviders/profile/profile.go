// Package profile applies explicit provider-construction profiles.
package profile

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/semistrict/dago/damodel"
)

var (
	// ErrInvalidModelSpec marks a model specification that does not use the
	// provider:model form required for string resolution.
	ErrInvalidModelSpec = errors.New("invalid model specification")
	// ErrProviderUnavailable marks a syntactically valid model specification
	// whose provider has no configured construction factory.
	ErrProviderUnavailable = errors.New("model provider unavailable")
)

// Factory constructs a chat model from a provider:model specification and the
// fully merged provider options. Factories should treat options as read-only.
type Factory func(modelSpec string, options map[string]any) (damodel.Chat, error)

// Resolver turns provider:model strings into chat models. Profiles are applied
// before the selected factory is called, and caller options take precedence.
// Factories and profiles are explicit so applications retain ownership of
// credentials and optional provider dependencies.
type Resolver struct {
	// Profiles selects construction profiles. Nil uses Builtin, including
	// registered plugin overlays; a non-nil set is used exactly as supplied.
	Profiles  Profiles
	Factories map[string]Factory
}

// Resolve constructs the model named by modelSpec. Known spelling aliases are
// normalized only for factory selection; the original specification is passed
// to profiles, hooks, and the factory.
func (resolver Resolver) Resolve(modelSpec string, callerOptions map[string]any) (damodel.Chat, error) {
	provider, _, valid := splitProviderSpec(modelSpec)
	if !valid || !strings.Contains(modelSpec, ":") {
		return nil, fmt.Errorf("%w %q: expected provider:model", ErrInvalidModelSpec, modelSpec)
	}
	factory := resolver.Factories[normalizeProvider(provider)]
	if factory == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderUnavailable, provider)
	}
	profiles := resolver.Profiles
	if profiles == nil {
		profiles = Builtin()
	}
	options, err := profiles.ApplyWithPreInit(modelSpec, callerOptions)
	if err != nil {
		return nil, err
	}
	model, err := factory(modelSpec, options)
	if err != nil {
		return nil, fmt.Errorf("construct model %q: %w", modelSpec, err)
	}
	if nilModel(model) {
		return nil, fmt.Errorf("construct model %q: factory returned nil", modelSpec)
	}
	return model, nil
}

// ModelMatchesSpec reports whether model advertises the provider and identifier
// named by spec. A bare spec compares only identifiers. When a model does not
// advertise its provider, a provider-prefixed spec falls back to identifier
// matching for compatibility with custom model implementations.
func ModelMatchesSpec(model damodel.Chat, spec string) bool {
	if nilModel(model) {
		return false
	}
	current := model.Profile()
	if current.Model == "" {
		return false
	}
	if spec == current.Model {
		return true
	}
	provider, identifier, found := strings.Cut(spec, ":")
	if !found || provider == "" || identifier != current.Model {
		return false
	}
	return current.Provider == "" || normalizeProvider(provider) == normalizeProvider(current.Provider)
}

// IsBedrockModel reports whether a provider:model string or chat model targets
// AWS Bedrock. Bare Amazon Nova identifiers, including regional inference
// profile identifiers, are recognized as well.
func IsBedrockModel(model any) bool {
	switch value := model.(type) {
	case string:
		if isBedrockNovaIdentifier(value) {
			return true
		}
		provider, _, found := strings.Cut(value, ":")
		return found && isBedrockProvider(provider)
	case damodel.Chat:
		if nilModel(value) {
			return false
		}
		profile := value.Profile()
		if isBedrockProvider(profile.Provider) || isBedrockNovaIdentifier(profile.Model) {
			return true
		}
		name := reflect.Indirect(reflect.ValueOf(value)).Type().Name()
		switch name {
		case "ChatAnthropicBedrock", "ChatBedrock", "ChatBedrockConverse", "ChatBedrockNovaSonic":
			return true
		}
		return false
	default:
		return false
	}
}

func nilModel(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func normalizeProvider(provider string) string {
	value := strings.ReplaceAll(strings.ToLower(provider), "-", "_")
	switch value {
	case "azure_openai":
		return "azure"
	case "mistralai":
		return "mistral"
	default:
		return value
	}
}

func isBedrockProvider(provider string) bool {
	switch normalizeProvider(provider) {
	case "amazon_bedrock", "anthropic_bedrock", "aws", "bedrock", "bedrock_converse":
		return true
	default:
		return false
	}
}

func isBedrockNovaIdentifier(identifier string) bool {
	for _, prefix := range []string{"apac.", "amer.", "au.", "eu.", "global.", "jp.", "sa.", "us.", "us-gov."} {
		if strings.HasPrefix(identifier, prefix) {
			identifier = strings.TrimPrefix(identifier, prefix)
			break
		}
	}
	return strings.HasPrefix(identifier, "amazon.nova-")
}

// Profile supplies defaults and hooks to a caller that constructs a model.
type Profile struct {
	Options        map[string]any
	PreInit        func(modelSpec string) error
	OptionsFactory func() (map[string]any, error)
}

var providerProfileRegistry = struct {
	sync.RWMutex
	profiles Profiles
}{profiles: Profiles{}}

// ProviderProfilePlugin identifies an explicitly imported provider-profile
// plugin. Register may call RegisterProviderProfile one or more times.
//
// Go does not provide a portable equivalent of Python package entry points,
// so applications must import plugin packages and pass their registration
// functions to LoadProviderProfilePlugins (or rely on an imported package's
// init function calling RegisterProviderProfile).
type ProviderProfilePlugin struct {
	Name     string
	Register func() error
}

// RegisterProviderProfile additively registers a provider or provider:model
// construction profile. Existing hooks and factories run first; incoming
// options and factory results win on conflicts. Registration is safe to call
// concurrently and defensively copies the profile's static options.
//
// Registered profiles are included by Builtin. They are deliberately not
// injected into arbitrary Profiles values, so callers that construct an
// explicit set retain complete ownership of that set.
func RegisterProviderProfile(key string, profile Profile) error {
	if err := validateProfileKey(key); err != nil {
		return err
	}
	profile = cloneProfile(profile)
	providerProfileRegistry.Lock()
	defer providerProfileRegistry.Unlock()
	if existing, ok := providerProfileRegistry.profiles[key]; ok {
		profile = Merge(existing, profile)
	}
	providerProfileRegistry.profiles[key] = profile
	return nil
}

// LoadProviderProfilePlugins invokes explicitly supplied plugins in order.
// A returned error or panic is captured for that plugin and does not prevent
// later plugins from loading. Registrations completed before a failure remain
// registered, matching the additive plugin contract.
func LoadProviderProfilePlugins(plugins ...ProviderProfilePlugin) []error {
	var failures []error
	for index, plugin := range plugins {
		if err := loadProviderProfilePlugin(plugin, index); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

func loadProviderProfilePlugin(plugin ProviderProfilePlugin, index int) (err error) {
	label := plugin.Name
	if label == "" {
		label = fmt.Sprintf("plugin %d", index)
	}
	if plugin.Register == nil {
		return fmt.Errorf("provider profile plugin %q: registration function is nil", label)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("provider profile plugin %q panicked: %v", label, recovered)
		}
	}()
	if err := plugin.Register(); err != nil {
		return fmt.Errorf("provider profile plugin %q: %w", label, err)
	}
	return nil
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
	if name == "" || name != strings.TrimSpace(name) {
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

// Builtin returns the standard provider-construction defaults plus registered
// third-party overlays as a fresh set. Built-ins are applied first.
func Builtin() Profiles {
	result := Profiles{
		"openai": {Options: map[string]any{"use_responses_api": true}},
		"nvidia": {OptionsFactory: func() (map[string]any, error) {
			return map[string]any{"default_headers": map[string]string{"X-BILLING-INVOKE-ORIGIN": "DeepAgents"}}, nil
		}},
		"openrouter": {OptionsFactory: openRouterDefaults},
	}
	providerProfileRegistry.RLock()
	defer providerProfileRegistry.RUnlock()
	for key, profile := range providerProfileRegistry.profiles {
		if existing, ok := result[key]; ok {
			result[key] = Merge(existing, profile)
		} else {
			result[key] = cloneProfile(profile)
		}
	}
	return result
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
