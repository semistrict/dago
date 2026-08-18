package modelconfig

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/damodel"
	providerprofile "github.com/semistrict/dago/daproviders/profile"
)

var (
	ErrInvalidSpec         = errors.New("invalid model specification")
	ErrUnknownProvider     = errors.New("unknown model provider")
	ErrAmbiguousProvider   = errors.New("ambiguous model provider")
	ErrMissingCredential   = errors.New("missing model credential")
	ErrProviderUnavailable = errors.New("model provider unavailable")
)

// Spec is one normalized provider and provider-local model identifier.
type Spec struct {
	Provider string
	Model    string
}

func (spec Spec) String() string { return spec.Provider + ":" + spec.Model }

// Construction is the complete, defensive configuration supplied to a
// caller-owned factory. Credential material remains in the separate positional
// Resolution argument.
type Construction struct {
	BaseURL          string
	Parameters       map[string]any
	ProfileOverrides map[string]any
	RetryParameter   string
	MaxRetries       int
	HasMaxRetries    bool
}

func (construction Construction) String() string {
	return fmt.Sprintf("Construction(base_url_set=%t,parameters=%d,profile_overrides=%d,retry_parameter=%q,max_retries=%d,has_max_retries=%t)", construction.BaseURL != "", len(construction.Parameters), len(construction.ProfileOverrides), construction.RetryParameter, construction.MaxRetries, construction.HasMaxRetries)
}

func (construction Construction) GoString() string { return construction.String() }

// Factory constructs a model without transferring dependency or transport
// ownership to this package.
type Factory func(context.Context, Spec, dacredential.Resolution, Construction) (damodel.Chat, error)

// Options contains static declarations and finite resolver limits. A zero value
// selects the pinned registry, built-in profiles, and conservative bounds.
type Options struct {
	Providers         []Provider
	Profiles          providerprofile.Profiles
	DefaultMaxRetries int
	MaxParameters     int
	MaxNesting        int
	MaxParameterBytes int
}

// ResolveOptions are request-level overrides. A non-nil BaseURL may be empty to
// deliberately suppress stored and environment endpoints.
type ResolveOptions struct {
	Parameters       map[string]any
	ProfileOverrides map[string]any
	BaseURL          *string
	MaxRetries       *int
}

// Resolution is a secret-free description of a constructed model.
type Resolution struct {
	Spec                  Spec
	CredentialSource      dacredential.Source
	CredentialEnvironment string
	Construction          Construction
	Model                 damodel.Chat
}

func (resolution Resolution) String() string {
	return fmt.Sprintf("Resolution(spec=%s,credential_source=%s,credential_environment=%s)", resolution.Spec, resolution.CredentialSource, resolution.CredentialEnvironment)
}

func (resolution Resolution) GoString() string { return resolution.String() }

// Status is one provider's uniform, secret-free authentication and factory
// availability view.
type Status struct {
	Provider              string
	Authentication        Authentication
	CredentialEnvironment string
	CredentialSource      dacredential.Source
	Configured            bool
	FactoryAvailable      bool
}

func (status Status) String() string {
	return fmt.Sprintf("Status(provider=%s,authentication=%s,configured=%t,credential_source=%s,factory_available=%t)", status.Provider, status.Authentication, status.Configured, status.CredentialSource, status.FactoryAvailable)
}

func (status Status) GoString() string { return status.String() }

// Resolver combines the pinned provider catalog, caller-owned credentials,
// environment snapshot, and explicitly available factories.
type Resolver struct {
	credentials       *dacredential.Store
	lookup            dacredential.EnvironmentLookup
	factories         map[string]Factory
	providers         map[string]Provider
	profiles          providerprofile.Profiles
	limits            valueLimits
	defaultMaxRetries int
}

// NewResolver constructs a resolver without I/O. Credential store, environment
// lookup, and factory catalog are required positional dependencies.
func NewResolver(credentials *dacredential.Store, lookup dacredential.EnvironmentLookup, factories map[string]Factory, options Options) *Resolver {
	if credentials == nil {
		panic("modelconfig: credential store is required")
	}
	if lookup == nil {
		panic("modelconfig: environment lookup is required")
	}
	if factories == nil {
		panic("modelconfig: factory catalog is required")
	}
	if options.MaxParameters == 0 {
		options.MaxParameters = 256
	}
	if options.MaxNesting == 0 {
		options.MaxNesting = 8
	}
	if options.MaxParameterBytes == 0 {
		options.MaxParameterBytes = 64 << 10
	}
	if options.DefaultMaxRetries == 0 {
		options.DefaultMaxRetries = 6
	}
	if options.MaxParameters < 1 || options.MaxParameters > 4096 || options.MaxNesting < 1 || options.MaxNesting > 32 || options.MaxParameterBytes < 1 || options.MaxParameterBytes > 1<<20 || options.DefaultMaxRetries < 0 || options.DefaultMaxRetries > 100 {
		panic("modelconfig: resolver limits are outside their finite ranges")
	}
	providers := compileProviders(options.Providers)
	limits := valueLimits{maxEntries: options.MaxParameters, maxDepth: options.MaxNesting, maxBytes: options.MaxParameterBytes}
	for name, provider := range providers {
		parameters, err := validateAndCloneMap(provider.Parameters, limits)
		if err != nil {
			panic(fmt.Sprintf("modelconfig: provider %q parameters: %v", name, err))
		}
		profileOverrides, err := validateAndCloneMap(provider.ProfileOverrides, limits)
		if err != nil {
			panic(fmt.Sprintf("modelconfig: provider %q profile: %v", name, err))
		}
		provider.Parameters = parameters
		provider.ProfileOverrides = profileOverrides
		provider.BaseURLEnvironments = append([]string(nil), provider.BaseURLEnvironments...)
		models := make(map[string]Model, len(provider.Models))
		for modelName, model := range provider.Models {
			modelParameters, err := validateAndCloneMap(model.Parameters, limits)
			if err != nil {
				panic(fmt.Sprintf("modelconfig: provider %q model %q parameters: %v", name, modelName, err))
			}
			modelProfile, err := validateAndCloneMap(model.ProfileOverrides, limits)
			if err != nil {
				panic(fmt.Sprintf("modelconfig: provider %q model %q profile: %v", name, modelName, err))
			}
			models[modelName] = Model{Parameters: modelParameters, ProfileOverrides: modelProfile}
		}
		provider.Models = models
		if provider.BaseURL != "" {
			if _, err := validateBaseURL(provider.BaseURL); err != nil {
				panic(fmt.Sprintf("modelconfig: provider %q base URL: %v", name, err))
			}
		}
		providers[name] = provider
	}
	compiledFactories := make(map[string]Factory, len(factories))
	for name, factory := range factories {
		normalized := normalizeProvider(name)
		if normalized == "" || !providerIdentifier(normalized) || factory == nil {
			panic("modelconfig: factory declarations require a provider and function")
		}
		if _, duplicate := compiledFactories[normalized]; duplicate {
			panic(fmt.Sprintf("modelconfig: duplicate factory for %q", normalized))
		}
		compiledFactories[normalized] = factory
	}
	profiles := options.Profiles
	if profiles == nil {
		profiles = providerprofile.Builtin()
	}
	return &Resolver{credentials: credentials, lookup: lookup, factories: compiledFactories, providers: providers, profiles: profiles, limits: limits, defaultMaxRetries: options.DefaultMaxRetries}
}

// Statuses reports all declared providers in stable name order without calling
// factories or probing provider endpoints.
func (resolver *Resolver) Statuses(ctx context.Context) ([]Status, error) {
	if ctx == nil {
		panic("modelconfig: nil context")
	}
	names := make([]string, 0, len(resolver.providers))
	for name := range resolver.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]Status, 0, len(names))
	for _, name := range names {
		provider := resolver.providers[name]
		authentication := provider.Authentication
		if authentication == "" {
			authentication = AuthenticationRequired
		}
		credential, err := resolver.resolveCredential(ctx, provider)
		if err != nil {
			return nil, err
		}
		environment := credential.Environment
		if environment == "" {
			environment = provider.CredentialEnvironment
		}
		result = append(result, Status{
			Provider: name, Authentication: authentication,
			CredentialEnvironment: environment, CredentialSource: credential.Source,
			Configured:       credential.Configured || authentication == AuthenticationAmbient || authentication == AuthenticationOptional,
			FactoryAvailable: resolver.factories[name] != nil,
		})
	}
	return result, nil
}

// Resolve validates configuration, resolves credentials and endpoint metadata,
// then invokes exactly one explicitly registered factory.
func (resolver *Resolver) Resolve(ctx context.Context, modelSpec string, options ResolveOptions) (Resolution, error) {
	if ctx == nil {
		panic("modelconfig: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	spec, err := resolver.Parse(ctx, modelSpec)
	if err != nil {
		return Resolution{}, err
	}
	provider := resolver.providers[spec.Provider]
	factory := resolver.factories[spec.Provider]
	if factory == nil {
		return Resolution{}, fmt.Errorf("%w: %s", ErrProviderUnavailable, spec.Provider)
	}
	requestParameters, err := validateAndCloneMap(options.Parameters, resolver.limits)
	if err != nil {
		return Resolution{}, err
	}
	requestProfile, err := validateAndCloneMap(options.ProfileOverrides, resolver.limits)
	if err != nil {
		return Resolution{}, err
	}
	credential, err := resolver.resolveCredential(ctx, provider)
	if err != nil {
		return Resolution{}, err
	}
	if provider.Authentication == "" {
		provider.Authentication = AuthenticationRequired
	}
	if (provider.Authentication == AuthenticationRequired || provider.Authentication == AuthenticationOAuth) && !credential.Configured {
		return Resolution{}, fmt.Errorf("%w: provider %s expects %s", ErrMissingCredential, provider.Name, provider.CredentialEnvironment)
	}
	construction, err := resolver.construction(spec, provider, credential, options, requestParameters, requestProfile)
	if err != nil {
		return Resolution{}, err
	}
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	model, err := invokeFactory(ctx, factory, spec, credential, construction)
	if err != nil {
		return Resolution{}, err
	}
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	model, err = applyRuntimeProfile(model, construction.ProfileOverrides)
	if err != nil {
		return Resolution{}, err
	}
	model, err = bindResolvedIdentity(model, spec)
	if err != nil {
		return Resolution{}, err
	}
	environment := credential.Environment
	if environment == "" {
		environment = provider.CredentialEnvironment
	}
	return Resolution{Spec: spec, CredentialSource: credential.Source, CredentialEnvironment: environment, Construction: cloneConstruction(construction), Model: model}, nil
}

// Parse resolves provider:model input, exact custom model declarations, and the
// pinned family heuristics. It performs no network I/O and does not call a model
// factory.
func (resolver *Resolver) Parse(ctx context.Context, modelSpec string) (Spec, error) {
	if ctx == nil {
		panic("modelconfig: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Spec{}, err
	}
	value := strings.TrimSpace(modelSpec)
	if value == "" || value != modelSpec || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
		return Spec{}, fmt.Errorf("%w: model name is empty or malformed", ErrInvalidSpec)
	}
	if isBedrockIdentifier(value) {
		return Spec{Provider: "bedrock", Model: value}, nil
	}
	if providerName, model, found := strings.Cut(value, ":"); found {
		providerName = normalizeProvider(providerName)
		if providerName == "" || strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) {
			return Spec{}, fmt.Errorf("%w: expected provider:model", ErrInvalidSpec)
		}
		if _, exists := resolver.providers[providerName]; !exists {
			return Spec{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
		}
		return Spec{Provider: providerName, Model: model}, nil
	}
	exact := make([]string, 0, 1)
	for name, provider := range resolver.providers {
		if _, exists := provider.Models[value]; exists {
			exact = append(exact, name)
		}
	}
	if len(exact) > 1 {
		sort.Strings(exact)
		return Spec{}, fmt.Errorf("%w: model %q is declared by %s", ErrAmbiguousProvider, value, strings.Join(exact, ", "))
	}
	if len(exact) == 1 {
		return Spec{Provider: exact[0], Model: value}, nil
	}
	provider := resolver.detectProvider(value)
	if provider == "" {
		return Spec{}, fmt.Errorf("%w: cannot infer provider for %q", ErrUnknownProvider, value)
	}
	return Spec{Provider: provider, Model: value}, nil
}

func (resolver *Resolver) detectProvider(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lower, "gpt-"), strings.HasPrefix(lower, "o1"), strings.HasPrefix(lower, "o3"), strings.HasPrefix(lower, "o4"), strings.HasPrefix(lower, "chatgpt"), strings.HasPrefix(lower, "text-davinci"):
		return "openai"
	case strings.HasPrefix(lower, "command"):
		return "cohere"
	case strings.HasPrefix(lower, "mistral"), strings.HasPrefix(lower, "mixtral"):
		return "mistralai"
	case strings.HasPrefix(lower, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(lower, "grok"):
		return "xai"
	case strings.HasPrefix(lower, "sonar"):
		return "perplexity"
	case strings.HasPrefix(lower, "claude"):
		if resolver.onlyVertexCredentials() {
			return "google_vertexai"
		}
		return "anthropic"
	case strings.HasPrefix(lower, "gemini"):
		if resolver.onlyVertexCredentials() {
			return "google_vertexai"
		}
		return "google_genai"
	case strings.HasPrefix(lower, "nemotron"), strings.HasPrefix(lower, "nvidia/"):
		return "nvidia"
	case strings.HasPrefix(lower, "accounts/fireworks/models/"):
		return "fireworks"
	default:
		return ""
	}
}

func (resolver *Resolver) onlyVertexCredentials() bool {
	vertex := environmentPresent(resolver.lookup, "GOOGLE_CLOUD_PROJECT")
	genai := environmentPresent(resolver.lookup, "GOOGLE_API_KEY")
	return vertex && !genai
}

func environmentPresent(lookup dacredential.EnvironmentLookup, canonical string) bool {
	if value, present := lookup("DEEPAGENTS_CODE_" + canonical); present {
		return strings.TrimSpace(value) != ""
	}
	value, present := lookup(canonical)
	return present && strings.TrimSpace(value) != ""
}

func isBedrockIdentifier(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"apac.", "amer.", "au.", "eu.", "global.", "jp.", "sa.", "us.", "us-gov."} {
		lower = strings.TrimPrefix(lower, prefix)
	}
	if strings.HasPrefix(lower, "amazon.nova-") {
		return true
	}
	return strings.Contains(lower, ".") && (strings.HasPrefix(lower, "anthropic.") || strings.HasPrefix(lower, "amazon.") || strings.HasPrefix(lower, "cohere.") || strings.HasPrefix(lower, "meta.") || strings.HasPrefix(lower, "mistral."))
}

func (resolver *Resolver) resolveCredential(ctx context.Context, provider Provider) (dacredential.Resolution, error) {
	resolution, err := resolver.credentials.Resolve(ctx, provider.Name, resolver.lookup)
	if err != nil || resolution.Configured || provider.CredentialEnvironment == "" {
		return resolution, err
	}
	value, environment, present := resolveEnvironment(resolver.lookup, provider.CredentialEnvironment)
	if !present || strings.TrimSpace(value) == "" {
		return dacredential.Resolution{Provider: provider.Name, Source: dacredential.MissingSource, Environment: environment}, nil
	}
	value = strings.TrimSpace(value)
	if len(value) > 64<<10 || strings.ContainsAny(value, "\x00\r\n") {
		return dacredential.Resolution{}, fmt.Errorf("%w: %s contains invalid credential data", dacredential.ErrInvalidCredential, environment)
	}
	return dacredential.Resolution{Provider: provider.Name, Source: dacredential.EnvironmentSource, Environment: environment, Configured: true, Credential: dacredential.Credential{Type: dacredential.APIKeyType, APIKey: &dacredential.APIKeyCredential{Key: value}}}, nil
}

func resolveEnvironment(lookup dacredential.EnvironmentLookup, canonical string) (string, string, bool) {
	prefixed := "DEEPAGENTS_CODE_" + canonical
	if value, present := lookup(prefixed); present {
		return value, prefixed, true
	}
	value, present := lookup(canonical)
	return value, canonical, present
}

func (resolver *Resolver) construction(spec Spec, provider Provider, credential dacredential.Resolution, options ResolveOptions, requestParameters, requestProfile map[string]any) (Construction, error) {
	modelConfig := provider.Models[spec.Model]
	parameters := mergeMaps(provider.Parameters, modelConfig.Parameters)
	profileOptions, err := applyProviderProfile(resolver.profiles, spec.String(), parameters)
	if err != nil {
		return Construction{}, err
	}
	profileOptions, err = validateAndCloneMap(profileOptions, resolver.limits)
	if err != nil {
		return Construction{}, err
	}
	parameters = mergeMaps(profileOptions, requestParameters)
	profileOverrides := mergeMaps(provider.ProfileOverrides, modelConfig.ProfileOverrides, requestProfile)
	if _, err := validateAndCloneMap(parameters, resolver.limits); err != nil {
		return Construction{}, err
	}
	if _, err := validateAndCloneMap(profileOverrides, resolver.limits); err != nil {
		return Construction{}, err
	}
	if err := validateRuntimeProfileOverrides(profileOverrides); err != nil {
		return Construction{}, err
	}
	baseURL, err := resolver.resolveBaseURL(provider, credential, options.BaseURL)
	if err != nil {
		return Construction{}, err
	}
	construction := Construction{BaseURL: baseURL, Parameters: parameters, ProfileOverrides: profileOverrides, RetryParameter: provider.RetryParameter}
	if provider.RetryParameter != "" {
		construction.MaxRetries, construction.HasMaxRetries = resolver.defaultMaxRetries, true
		if value, exists := parameters[provider.RetryParameter]; exists {
			configured, ok := intParameter(value)
			if !ok {
				return Construction{}, fmt.Errorf("%w: %s must be an integer from 0 through 100", ErrInvalidOptions, provider.RetryParameter)
			}
			construction.MaxRetries = configured
		}
	}
	if options.MaxRetries != nil {
		if *options.MaxRetries < 0 || *options.MaxRetries > 100 {
			return Construction{}, fmt.Errorf("%w: max retries must be between 0 and 100", ErrInvalidOptions)
		}
		construction.MaxRetries, construction.HasMaxRetries = *options.MaxRetries, true
		if construction.RetryParameter == "" {
			construction.RetryParameter = "max_retries"
		}
	}
	if construction.HasMaxRetries {
		construction.Parameters[construction.RetryParameter] = construction.MaxRetries
	}
	return construction, nil
}

func intParameter(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, value >= 0 && value <= 100
	case int64:
		return int(value), value >= 0 && value <= 100
	case float64:
		return int(value), value >= 0 && value <= 100 && value == float64(int(value))
	default:
		return 0, false
	}
}

func (resolver *Resolver) resolveBaseURL(provider Provider, credential dacredential.Resolution, requested *string) (string, error) {
	if requested != nil {
		return validateBaseURL(*requested)
	}
	if provider.BaseURL != "" {
		return validateBaseURL(provider.BaseURL)
	}
	if credential.Source == dacredential.StoredSource {
		if credential.Credential.APIKey != nil {
			return validateBaseURL(credential.Credential.APIKey.BaseURL)
		}
		return "", nil
	}
	for _, environment := range provider.BaseURLEnvironments {
		value, _, present := resolveEnvironment(resolver.lookup, environment)
		if present && strings.TrimSpace(value) != "" {
			return validateBaseURL(value)
		}
	}
	return "", nil
}

func validateBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "", nil
	}
	if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: invalid base URL", ErrInvalidOptions)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: base URL must be an HTTP(S) origin or path without credentials, query, or fragment", ErrInvalidOptions)
	}
	return value, nil
}

func invokeFactory(ctx context.Context, factory Factory, spec Spec, credential dacredential.Resolution, construction Construction) (model damodel.Chat, err error) {
	defer func() {
		if recover() != nil {
			model = nil
			err = fmt.Errorf("modelconfig: provider factory for %s panicked", spec)
		}
	}()
	model, err = factory(ctx, spec, credential, cloneConstruction(construction))
	if err != nil {
		return nil, &factoryError{message: redactFactoryError(err.Error(), credential, construction), cause: err}
	}
	if model == nil || nilValue(model) {
		return nil, fmt.Errorf("modelconfig: provider factory for %s returned nil", spec)
	}
	return model, nil
}

func nilValue(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func bindResolvedIdentity(model damodel.Chat, spec Spec) (damodel.Chat, error) {
	advertised := model.Profile()
	if advertised.Provider != "" && normalizeProvider(advertised.Provider) != spec.Provider {
		return nil, fmt.Errorf("modelconfig: provider factory returned provider %q for requested provider %q", advertised.Provider, spec.Provider)
	}
	if advertised.Model != "" && advertised.Model != spec.Model {
		return nil, fmt.Errorf("modelconfig: provider factory returned model %q for requested model %q", advertised.Model, spec.Model)
	}
	return damodel.WithProfile(model, func(profile *damodel.Profile) {
		profile.Provider = spec.Provider
		profile.Model = spec.Model
	}), nil
}

type factoryError struct {
	message string
	cause   error
}

func (err *factoryError) Error() string {
	return "modelconfig: provider factory failed: " + err.message
}
func (err *factoryError) Unwrap() error { return err.cause }

type configurationError struct{ cause error }

func (err *configurationError) Error() string {
	return "modelconfig: provider profile configuration failed"
}
func (err *configurationError) Unwrap() error { return err.cause }

func redactFactoryError(message string, credential dacredential.Resolution, construction Construction) string {
	secrets := []string{}
	if credential.Credential.APIKey != nil {
		secrets = append(secrets, credential.Credential.APIKey.Key)
	}
	if credential.Credential.OAuth != nil {
		secrets = append(secrets, credential.Credential.OAuth.AccessToken, credential.Credential.OAuth.RefreshToken)
	}
	collectSensitiveValues(construction.Parameters, &secrets)
	collectSensitiveValues(construction.ProfileOverrides, &secrets)
	for _, secret := range secrets {
		if len(secret) >= 4 {
			message = strings.ReplaceAll(message, secret, "<redacted>")
		}
	}
	if len(message) > 4096 {
		message = message[:4096] + "..."
	}
	return message
}

// collectSensitiveValues follows only the bounded JSON-compatible shapes
// accepted by validateAndCloneMap, including maps nested inside arrays.
func collectSensitiveValues(values map[string]any, secrets *[]string) {
	collectSensitiveValue(values, false, secrets)
}

func collectSensitiveValue(value any, inheritedSensitive bool, secrets *[]string) {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			lower := strings.ToLower(key)
			sensitive := inheritedSensitive || strings.Contains(lower, "key") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie")
			collectSensitiveValue(item, sensitive, secrets)
		}
	case []any:
		for _, item := range value {
			collectSensitiveValue(item, inheritedSensitive, secrets)
		}
	case string:
		if inheritedSensitive {
			*secrets = append(*secrets, value)
		}
	}
}

func applyProviderProfile(profiles providerprofile.Profiles, modelSpec string, parameters map[string]any) (options map[string]any, err error) {
	defer func() {
		if recover() != nil {
			options = nil
			err = &configurationError{}
		}
	}()
	options, err = profiles.ApplyWithPreInit(modelSpec, parameters)
	if err != nil {
		return nil, &configurationError{cause: err}
	}
	return options, nil
}

func cloneConstruction(construction Construction) Construction {
	construction.Parameters = cloneMap(construction.Parameters)
	construction.ProfileOverrides = cloneMap(construction.ProfileOverrides)
	return construction
}
