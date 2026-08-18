// Package langsmithgateway resolves provider:model specifications through the
// LangSmith LLM Gateway without owning provider SDKs or credentials discovery.
package langsmithgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/semistrict/dago/damodel"
)

// DefaultEndpoint is the managed LangSmith LLM Gateway origin.
const DefaultEndpoint = "https://gateway.smith.langchain.com"

var (
	// ErrInvalidConfig identifies invalid static gateway configuration.
	ErrInvalidConfig = errors.New("invalid LangSmith gateway configuration")
	// ErrInvalidModelSpec identifies malformed provider:model input.
	ErrInvalidModelSpec = errors.New("invalid LangSmith gateway model specification")
	// ErrUnsupportedProvider identifies a syntactically valid provider that has
	// no configured gateway route.
	ErrUnsupportedProvider = errors.New("LangSmith gateway provider unsupported")
	// ErrFactory identifies a caller-owned factory failure. The underlying error
	// text is deliberately not returned because it may contain credentials.
	ErrFactory = errors.New("LangSmith gateway model factory failed")
)

// Factory constructs a provider model using the routed endpoint, gateway API
// key, and original provider:model specification. All mandatory values are
// positional. Implementations own SDK selection and network behavior.
type Factory interface {
	NewGatewayModel(context.Context, string, string, string) (damodel.Chat, error)
}

// FactoryFunc adapts a function to Factory.
type FactoryFunc func(context.Context, string, string, string) (damodel.Chat, error)

// NewGatewayModel invokes the adapted function.
func (factory FactoryFunc) NewGatewayModel(ctx context.Context, endpoint, apiKey, modelSpec string) (damodel.Chat, error) {
	return factory(ctx, endpoint, apiKey, modelSpec)
}

// Options controls the supported provider paths and model-spec bound. A nil
// ProviderPaths map selects the pinned built-in routes; a non-nil map is used
// exactly as supplied.
type Options struct {
	ProviderPaths     map[string]string
	MaxModelSpecRunes int
}

// DefaultOptions returns finite production defaults.
func DefaultOptions() Options {
	return Options{ProviderPaths: DefaultProviderPaths(), MaxModelSpecRunes: 512}
}

// DefaultProviderPaths returns the pinned gateway-aware provider routes.
func DefaultProviderPaths() map[string]string {
	return map[string]string{
		"anthropic":    "anthropic",
		"baseten":      "baseten",
		"fireworks":    "fireworks",
		"google_genai": "gemini",
		"openai":       "openai/v1",
	}
}

// Resolver turns provider:model specifications into caller-owned chat models.
// It is immutable and safe for concurrent use.
type Resolver struct {
	factory      Factory
	endpoint     string
	apiKey       string
	providerPath map[string]string
	maxSpecRunes int
}

// String returns a credential-free description suitable for diagnostics.
func (resolver *Resolver) String() string {
	return fmt.Sprintf("langsmithgateway.Resolver{providers:%d}", len(resolver.providerPath))
}

// GoString returns a credential-free Go-syntax description.
func (resolver *Resolver) GoString() string { return resolver.String() }

// NewResolver compiles a gateway resolver without performing I/O. The factory,
// endpoint, and API key are positional; an empty endpoint selects
// DefaultEndpoint. Invalid static inputs panic.
func NewResolver(factory Factory, endpoint, apiKey string, options Options) *Resolver {
	if nilInterface(factory) {
		panic("langsmithgateway: factory is nil")
	}
	normalizedEndpoint, err := normalizeEndpoint(endpoint)
	if err != nil {
		panic(err)
	}
	if apiKey == "" || len(apiKey) > 64<<10 || apiKey != strings.TrimSpace(apiKey) || containsControl(apiKey) {
		panic("langsmithgateway: API key is empty, padded, unsafe, or too long")
	}
	defaults := DefaultOptions()
	if options.ProviderPaths == nil {
		options.ProviderPaths = defaults.ProviderPaths
	}
	if options.MaxModelSpecRunes == 0 {
		options.MaxModelSpecRunes = defaults.MaxModelSpecRunes
	}
	if options.MaxModelSpecRunes < 1 || options.MaxModelSpecRunes > 4096 || len(options.ProviderPaths) < 1 || len(options.ProviderPaths) > 128 {
		panic("langsmithgateway: options are outside their finite bounds")
	}
	paths := make(map[string]string, len(options.ProviderPaths))
	for provider, path := range options.ProviderPaths {
		normalized := normalizeProvider(provider)
		if normalized != provider || !providerPattern.MatchString(provider) || !validProviderPath(path) {
			panic("langsmithgateway: provider paths contain an invalid declaration")
		}
		paths[provider] = path
	}
	return &Resolver{
		factory: factory, endpoint: normalizedEndpoint, apiKey: apiKey,
		providerPath: paths, maxSpecRunes: options.MaxModelSpecRunes,
	}
}

// Providers returns configured providers in deterministic order.
func (resolver *Resolver) Providers() []string {
	providers := make([]string, 0, len(resolver.providerPath))
	for provider := range resolver.providerPath {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

// EndpointFor returns the credential-free routed endpoint for modelSpec.
func (resolver *Resolver) EndpointFor(modelSpec string) (string, error) {
	provider, _, err := resolver.parseModelSpec(modelSpec)
	if err != nil {
		return "", err
	}
	path, ok := resolver.providerPath[provider]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnsupportedProvider, provider)
	}
	return resolver.endpoint + "/" + path, nil
}

// ResolveModel constructs modelSpec through its configured gateway route.
// Factory error and panic values are sanitized so credentials cannot escape
// through this boundary; context cancellation remains identifiable.
func (resolver *Resolver) ResolveModel(ctx context.Context, modelSpec string) (damodel.Chat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endpoint, err := resolver.EndpointFor(modelSpec)
	if err != nil {
		return nil, err
	}
	model, err := callFactory(ctx, resolver.factory, endpoint, resolver.apiKey, modelSpec)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if errors.Is(err, context.Canceled) {
				return nil, context.Canceled
			}
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("%w for %q", ErrFactory, modelSpec)
	}
	if nilInterface(model) {
		return nil, fmt.Errorf("%w for %q: returned nil", ErrFactory, modelSpec)
	}
	return model, nil
}

func (resolver *Resolver) parseModelSpec(modelSpec string) (string, string, error) {
	if modelSpec == "" || len([]rune(modelSpec)) > resolver.maxSpecRunes || modelSpec != strings.TrimSpace(modelSpec) || containsControl(modelSpec) {
		return "", "", fmt.Errorf("%w: specification is empty, padded, unsafe, or too long", ErrInvalidModelSpec)
	}
	provider, model, found := strings.Cut(modelSpec, ":")
	provider = normalizeProvider(provider)
	if !found || !providerPattern.MatchString(provider) || model == "" || model != strings.TrimSpace(model) {
		return "", "", fmt.Errorf("%w: expected provider:model", ErrInvalidModelSpec)
	}
	return provider, model, nil
}

var (
	providerPattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	endpointPathPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

func normalizeProvider(provider string) string {
	return strings.ReplaceAll(strings.ToLower(provider), "-", "_")
}

func validProviderPath(path string) bool {
	if path == "" || len(path) > 256 || path != strings.Trim(path, "/") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if !providerPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

func normalizeEndpoint(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultEndpoint
	}
	if len(value) > 4096 || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("%w: endpoint is too long", ErrInvalidConfig)
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: endpoint must be an absolute origin or base path", ErrInvalidConfig)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		host := parsed.Hostname()
		address := net.ParseIP(host)
		if host != "localhost" && (address == nil || !address.IsLoopback()) {
			return "", fmt.Errorf("%w: plain HTTP is restricted to loopback", ErrInvalidConfig)
		}
		if host == "localhost" {
			port := parsed.Port()
			parsed.Host = "127.0.0.1"
			if port != "" {
				parsed.Host += ":" + port
			}
		}
	default:
		return "", fmt.Errorf("%w: endpoint scheme must be HTTPS or loopback HTTP", ErrInvalidConfig)
	}
	if path := strings.Trim(parsed.EscapedPath(), "/"); path != "" {
		for _, segment := range strings.Split(path, "/") {
			decoded, err := url.PathUnescape(segment)
			if err != nil || decoded == "." || decoded == ".." || !endpointPathPattern.MatchString(decoded) {
				return "", fmt.Errorf("%w: endpoint path is invalid", ErrInvalidConfig)
			}
		}
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func callFactory(ctx context.Context, factory Factory, endpoint, apiKey, modelSpec string) (model damodel.Chat, err error) {
	defer func() {
		if recover() != nil {
			model, err = nil, ErrFactory
		}
	}()
	return factory.NewGatewayModel(ctx, endpoint, apiKey, modelSpec)
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func nilInterface(value any) bool {
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
