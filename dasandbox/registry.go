package dasandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/dabackend"
)

const (
	defaultSetupBytes   = 1 << 20
	defaultSetupTimeout = 10 * time.Minute
	defaultCloseTimeout = 30 * time.Second
	maxProviders        = 128
	maxParameters       = 64
	maxParameterBytes   = 16 << 10
)

var (
	ErrUnknownProvider     = errors.New("unknown sandbox provider")
	ErrProviderUnavailable = errors.New("sandbox provider is unavailable")
	ErrProviderFailed      = errors.New("sandbox provider operation failed")
	ErrUnsupportedAttach   = errors.New("sandbox provider does not support attach")
	ErrUnsupportedSnapshot = errors.New("sandbox provider does not support snapshots")
	ErrInvalidRequest      = errors.New("invalid sandbox request")
	ErrSetupFailed         = errors.New("sandbox setup failed")

	providerNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// Metadata describes a provider without constructing it.
type Metadata struct {
	Name                 string `json:"name"`
	WorkingDir           string `json:"working_dir"`
	BuiltIn              bool   `json:"built_in,omitempty"`
	SupportsSandboxID    bool   `json:"supports_sandbox_id"`
	SupportsSnapshotName bool   `json:"supports_snapshot_name"`
}

// BuiltinMetadata returns defensive copies of the six pinned built-in
// provider descriptions.
func BuiltinMetadata() map[string]Metadata {
	return map[string]Metadata{
		"agentcore": {Name: "agentcore", WorkingDir: "/tmp", BuiltIn: true, SupportsSandboxID: false},
		"daytona":   {Name: "daytona", WorkingDir: "/home/daytona", BuiltIn: true, SupportsSandboxID: true},
		"langsmith": {Name: "langsmith", WorkingDir: "/root", BuiltIn: true, SupportsSandboxID: true, SupportsSnapshotName: true},
		"modal":     {Name: "modal", WorkingDir: "/workspace", BuiltIn: true, SupportsSandboxID: true},
		"runloop":   {Name: "runloop", WorkingDir: "/home/user", BuiltIn: true, SupportsSandboxID: true, SupportsSnapshotName: true},
		"vercel":    {Name: "vercel", WorkingDir: "/vercel/sandbox", BuiltIn: true, SupportsSandboxID: true},
	}
}

// ProviderRequest selects attach or fresh creation. SandboxID and Snapshot are
// mutually exclusive. Params are immutable copies of registry and request
// configuration, with request values taking precedence.
type ProviderRequest struct {
	SandboxID string
	Snapshot  string
	Params    map[string]string
}

// Provider is the common lifecycle implemented by built-in and extension
// adapters. GetOrCreate must return an explicitly executable sandbox.
type Provider interface {
	GetOrCreate(context.Context, ProviderRequest) (dabackend.Sandbox, error)
	Delete(context.Context, string) error
}

// ProviderFuncs adapts functions to Provider without another constructor.
type ProviderFuncs struct {
	GetOrCreateFunc func(context.Context, ProviderRequest) (dabackend.Sandbox, error)
	DeleteFunc      func(context.Context, string) error
}

func (provider ProviderFuncs) GetOrCreate(ctx context.Context, request ProviderRequest) (dabackend.Sandbox, error) {
	if provider.GetOrCreateFunc == nil {
		return nil, ErrProviderUnavailable
	}
	return provider.GetOrCreateFunc(ctx, request)
}

func (provider ProviderFuncs) Delete(ctx context.Context, sandboxID string) error {
	if provider.DeleteFunc == nil {
		return ErrProviderUnavailable
	}
	return provider.DeleteFunc(ctx, sandboxID)
}

// Factory receives only bounded, copied application configuration. It may
// validate external credentials but should defer remote I/O to Provider.
type Factory func(context.Context, ProviderConfig) (Provider, error)

// ProviderConfig identifies the resolved registration source and parameters.
type ProviderConfig struct {
	Name   string
	Params map[string]string
}

// Definition binds metadata, a factory, and default parameters. Metadata may
// be nil to inherit built-in metadata or use extension defaults.
type Definition struct {
	Metadata *Metadata
	Factory  Factory
	Params   map[string]string
}

// RegistryOptions configures precedence. Configuration overrides Extensions,
// which override the positional built-in factories.
type RegistryOptions struct {
	Default       string
	Extensions    map[string]Definition
	Configuration map[string]Definition
	MaxSetupBytes int
	SetupTimeout  time.Duration
	CloseTimeout  time.Duration
}

type resolvedDefinition struct {
	metadata Metadata
	factory  Factory
	params   map[string]string
	source   string
}

// Registry is an immutable provider catalog.
type Registry struct {
	providers     map[string]resolvedDefinition
	defaultName   string
	maxSetupBytes int
	setupTimeout  time.Duration
	closeTimeout  time.Duration
}

// NewRegistry compiles a registry. builtins is positional because applications
// explicitly choose which authenticated built-in integrations are linked. A
// nil map still exposes the six metadata entries and fails closed if selected.
func NewRegistry(builtins map[string]Definition, options RegistryOptions) *Registry {
	if len(builtins)+len(options.Extensions)+len(options.Configuration) > maxProviders {
		panic("dasandbox: too many provider registrations")
	}
	applyRegistryDefaults(&options)
	providers := make(map[string]resolvedDefinition)
	for name, metadata := range BuiltinMetadata() {
		definition, exists := builtins[name]
		if !exists {
			providers[name] = resolvedDefinition{metadata: metadata, source: "builtin"}
			continue
		}
		providers[name] = compileDefinition(name, definition, metadata, "builtin")
	}
	for name := range builtins {
		if _, exists := providers[name]; !exists {
			panic("dasandbox: positional builtins may contain only curated built-in names")
		}
	}
	for name, definition := range options.Extensions {
		base := defaultMetadata(name)
		previous, exists := providers[name]
		if exists {
			base = previous.metadata
		}
		resolved := compileDefinition(name, definition, base, "extension")
		inheritDefinition(&resolved, definition, previous, exists)
		providers[name] = resolved
	}
	for name, definition := range options.Configuration {
		base := defaultMetadata(name)
		previous, exists := providers[name]
		if exists {
			base = previous.metadata
		}
		resolved := compileDefinition(name, definition, base, "configuration")
		inheritDefinition(&resolved, definition, previous, exists)
		providers[name] = resolved
	}
	defaultName := strings.TrimSpace(options.Default)
	if defaultName != "" {
		if !providerNamePattern.MatchString(defaultName) {
			panic("dasandbox: invalid default provider name")
		}
		if _, exists := providers[defaultName]; !exists {
			panic("dasandbox: default provider is not registered")
		}
	}
	return &Registry{
		providers: providers, defaultName: defaultName, maxSetupBytes: options.MaxSetupBytes,
		setupTimeout: options.SetupTimeout, closeTimeout: options.CloseTimeout,
	}
}

func inheritDefinition(result *resolvedDefinition, definition Definition, previous resolvedDefinition, exists bool) {
	if !exists {
		return
	}
	if definition.Factory == nil {
		result.factory = previous.factory
	}
	if definition.Params == nil {
		result.params = cloneParams(previous.params)
	}
}

func applyRegistryDefaults(options *RegistryOptions) {
	if options.MaxSetupBytes < 0 || options.SetupTimeout < 0 || options.CloseTimeout < 0 {
		panic("dasandbox: registry limits cannot be negative")
	}
	if options.MaxSetupBytes == 0 {
		options.MaxSetupBytes = defaultSetupBytes
	}
	if options.MaxSetupBytes > 16<<20 {
		panic("dasandbox: setup byte limit exceeds maximum")
	}
	if options.SetupTimeout == 0 {
		options.SetupTimeout = defaultSetupTimeout
	}
	if options.CloseTimeout == 0 {
		options.CloseTimeout = defaultCloseTimeout
	}
}

func compileDefinition(name string, definition Definition, base Metadata, source string) resolvedDefinition {
	if !providerNamePattern.MatchString(name) {
		panic("dasandbox: invalid provider name")
	}
	metadata := base
	if definition.Metadata != nil {
		metadata = *definition.Metadata
	}
	metadata.Name = name
	if !validWorkingDir(metadata.WorkingDir) {
		panic("dasandbox: provider working directory must be a bounded absolute POSIX path")
	}
	metadata.BuiltIn = base.BuiltIn
	return resolvedDefinition{
		metadata: metadata, factory: definition.Factory,
		params: validateParams(definition.Params), source: source,
	}
}

func defaultMetadata(name string) Metadata {
	return Metadata{Name: name, WorkingDir: "/workspace", SupportsSandboxID: true}
}

func validWorkingDir(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}

func validateParams(params map[string]string) map[string]string {
	if len(params) > maxParameters {
		panic("dasandbox: too many provider parameters")
	}
	copyParams := make(map[string]string, len(params))
	for key, value := range params {
		if key == "" || len(key) > 128 || strings.ContainsAny(key, "\x00\r\n") || len(value) > maxParameterBytes || strings.ContainsRune(value, 0) {
			panic("dasandbox: invalid provider parameter")
		}
		copyParams[key] = value
	}
	return copyParams
}

// Default returns the configured default. A default never opens a sandbox by
// itself; callers must explicitly call Open with an empty provider name.
func (registry *Registry) Default() string { return registry.defaultName }

// Available returns all known names in deterministic order, including curated
// providers whose factories are not linked.
func (registry *Registry) Available() []string {
	names := make([]string, 0, len(registry.providers))
	for name := range registry.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Metadata returns a defensive provider description.
func (registry *Registry) Metadata(name string) (Metadata, bool) {
	definition, exists := registry.providers[strings.TrimSpace(name)]
	return definition.metadata, exists
}

// Source reports which precedence layer owns name.
func (registry *Registry) Source(name string) (string, bool) {
	definition, exists := registry.providers[strings.TrimSpace(name)]
	return definition.source, exists
}

// OpenRequest controls one registry-owned session. SetupScript is literal
// script content; it is never expanded against the host environment.
type OpenRequest struct {
	SandboxID   string
	Snapshot    string
	Params      map[string]string
	SetupScript []byte
}

// Open resolves providerName, validates its capabilities, creates or attaches,
// and runs the bounded setup script. An empty providerName explicitly requests
// the configured default.
func (registry *Registry) Open(ctx context.Context, providerName string, request OpenRequest) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		providerName = registry.defaultName
		if providerName == "" {
			return nil, fmt.Errorf("%w: no default is configured", ErrInvalidRequest)
		}
	}
	definition, exists := registry.providers[providerName]
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, providerName)
	}
	if definition.factory == nil {
		return nil, fmt.Errorf("%w: %q is not linked into this application", ErrProviderUnavailable, providerName)
	}
	sandboxID := strings.TrimSpace(request.SandboxID)
	snapshot := strings.TrimSpace(request.Snapshot)
	if sandboxID != "" && snapshot != "" {
		return nil, fmt.Errorf("%w: sandbox id and snapshot are mutually exclusive", ErrInvalidRequest)
	}
	if sandboxID != "" && !definition.metadata.SupportsSandboxID {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAttach, providerName)
	}
	if snapshot != "" && !definition.metadata.SupportsSnapshotName {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedSnapshot, providerName)
	}
	if len(request.SetupScript) > registry.maxSetupBytes {
		return nil, fmt.Errorf("%w: setup script exceeds %d bytes", ErrInvalidRequest, registry.maxSetupBytes)
	}
	requestParams, err := validateRequestParams(request.Params)
	if err != nil {
		return nil, err
	}
	params := cloneParams(definition.params)
	for key, value := range requestParams {
		params[key] = value
	}
	provider, err := callFactory(ctx, definition.factory, ProviderConfig{Name: providerName, Params: cloneParams(params)})
	if err != nil {
		return nil, err
	}
	providerRequest := ProviderRequest{SandboxID: sandboxID, Snapshot: snapshot, Params: cloneParams(params)}
	sandbox, err := callGetOrCreate(ctx, provider, providerRequest)
	if err != nil {
		return nil, err
	}
	if isNil(sandbox) || strings.TrimSpace(sandbox.ID()) == "" {
		return nil, fmt.Errorf("%w: provider returned an invalid sandbox", ErrProviderFailed)
	}
	session := &Session{Sandbox: sandbox, provider: provider, owned: sandboxID == "", workingDir: definition.metadata.WorkingDir, closeTimeout: registry.closeTimeout}
	if request.SetupScript != nil {
		if err := registry.runSetup(ctx, session, request.SetupScript); err != nil {
			if session.owned {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), registry.closeTimeout)
				cleanupErr := session.Close(cleanupCtx)
				cancel()
				if cleanupErr != nil {
					return nil, errors.Join(err, cleanupErr)
				}
			}
			return nil, err
		}
	}
	return session, nil
}

func cloneParams(params map[string]string) map[string]string {
	copyParams := make(map[string]string, len(params))
	for key, value := range params {
		copyParams[key] = value
	}
	return copyParams
}

func validateRequestParams(params map[string]string) (result map[string]string, err error) {
	defer func() {
		if recover() != nil {
			result, err = nil, ErrInvalidRequest
		}
	}()
	return validateParams(params), nil
}

func callFactory(ctx context.Context, factory Factory, config ProviderConfig) (provider Provider, err error) {
	defer func() {
		if recover() != nil {
			provider, err = nil, ErrProviderFailed
		}
	}()
	provider, err = factory(ctx, config)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("%w: construct provider", ErrProviderFailed)
	}
	if isNil(provider) {
		return nil, fmt.Errorf("%w: factory returned nil", ErrProviderFailed)
	}
	return provider, nil
}

func callGetOrCreate(ctx context.Context, provider Provider, request ProviderRequest) (sandbox dabackend.Sandbox, err error) {
	defer func() {
		if recover() != nil {
			sandbox, err = nil, ErrProviderFailed
		}
	}()
	sandbox, err = provider.GetOrCreate(ctx, request)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("%w: open sandbox", ErrProviderFailed)
	}
	return sandbox, nil
}

func isNil(value any) bool {
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

// Session is one attached or registry-owned sandbox. Attached sessions are
// never deleted by Close.
type Session struct {
	dabackend.Sandbox
	provider     Provider
	owned        bool
	workingDir   string
	closeTimeout time.Duration
	closeMu      sync.Mutex
	closed       bool
}

// Owned reports whether this registry created the remote resource.
func (session *Session) Owned() bool { return session != nil && session.owned }

// WorkingDir returns the provider's absolute remote workspace directory.
func (session *Session) WorkingDir() string {
	if session == nil {
		return ""
	}
	return session.workingDir
}

// Close deletes a registry-created sandbox exactly once. Attached sandboxes are
// left running. Provider failures are bounded to a stable classification.
func (session *Session) Close(ctx context.Context) error {
	if session == nil || !session.owned {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session.closeMu.Lock()
	defer session.closeMu.Unlock()
	if session.closed {
		return nil
	}
	deletionCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		deletionCtx, cancel = context.WithTimeout(ctx, session.closeTimeout)
	}
	defer cancel()
	if err := callDelete(deletionCtx, session.provider, session.ID()); err != nil {
		return err
	}
	session.closed = true
	return nil
}

func callDelete(ctx context.Context, provider Provider, sandboxID string) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrProviderFailed
		}
	}()
	err = provider.Delete(ctx, sandboxID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err != nil {
		return fmt.Errorf("%w: delete sandbox", ErrProviderFailed)
	}
	return nil
}
