package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/semistrict/dago/datalon"
)

const (
	defaultManagedProject = "deepagents-code"
	defaultUSEndpoint     = "https://api.smith.langchain.com"
	defaultEUEndpoint     = "https://eu.api.smith.langchain.com"
	managementPrefix      = "DEEPAGENTS_CODE_"
	redactedValue         = "[REDACTED]"
	maxCredentialBytes    = 64 << 10
	maxProjectBytes       = 256
	maxReplicaProjects    = 8
)

var (
	ErrCredentialStore = errors.New("tracing credential store failed")
	ErrInvalidTracing  = errors.New("invalid tracing configuration")
	ErrSinkFactory     = errors.New("tracing sink factory failed")
)

// Credential is a non-serializable stored LangSmith credential. Use
// NewCredential so required secret material remains positional.
type Credential struct {
	apiKey   string
	endpoint string
	project  string
}

// NewCredential validates one stored tracing credential without I/O.
func NewCredential(apiKey, endpoint, project string) Credential {
	credential := Credential{apiKey: strings.TrimSpace(apiKey), endpoint: strings.TrimSpace(endpoint), project: strings.TrimSpace(project)}
	if credential.apiKey == "" || len(credential.apiKey) > maxCredentialBytes || strings.ContainsAny(credential.apiKey, "\x00\r\n") {
		panic("tracing API key is invalid")
	}
	if credential.endpoint != "" {
		credential.endpoint = normalizeEndpoint(credential.endpoint)
		if _, err := validateEndpoint(credential.endpoint); err != nil {
			panic(err)
		}
	}
	if !validProject(credential.project, true) {
		panic("tracing project is invalid")
	}
	return credential
}

func (Credential) String() string   { return "tracing.Credential{redacted}" }
func (Credential) GoString() string { return "tracing.Credential{redacted}" }

// CredentialStore loads an optional application-owned credential. A zero
// Credential means no stored credential.
type CredentialStore interface {
	LoadTracingCredential(context.Context) (Credential, error)
}

// SinkFactory constructs the provider sink after configuration resolution.
// The API key is passed only to this caller-owned trust boundary.
type SinkFactory interface {
	NewTracingSink(context.Context, string, string) (Sink, error)
}

// StaticCredentialStore is a network- and filesystem-free store for callers
// that already resolved credentials through another mechanism.
type StaticCredentialStore struct{ credential Credential }

// NewStaticCredentialStore constructs a store without I/O.
func NewStaticCredentialStore(credential Credential) *StaticCredentialStore {
	return &StaticCredentialStore{credential: credential}
}

func (store *StaticCredentialStore) LoadTracingCredential(ctx context.Context) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if store == nil {
		panic("initialized tracing credential store is required")
	}
	return store.credential, nil
}

// ManagementOptions controls non-secret defaults and finite parsing limits.
// Its zero value is useful.
type ManagementOptions struct {
	DefaultProject      string
	MaxEnvironmentKeys  int
	MaxEnvironmentBytes int
	MaxReplicaProjects  int
}

func (options ManagementOptions) withDefaults() ManagementOptions {
	if options.MaxEnvironmentKeys < 0 || options.MaxEnvironmentBytes < 0 || options.MaxReplicaProjects < 0 {
		panic("tracing management limits cannot be negative")
	}
	if strings.TrimSpace(options.DefaultProject) == "" {
		options.DefaultProject = defaultManagedProject
	}
	if options.MaxEnvironmentKeys <= 0 {
		options.MaxEnvironmentKeys = 4096
	}
	if options.MaxEnvironmentBytes <= 0 {
		options.MaxEnvironmentBytes = 4 << 20
	}
	if options.MaxReplicaProjects <= 0 {
		options.MaxReplicaProjects = maxReplicaProjects
	}
	if !validProject(options.DefaultProject, false) || options.MaxEnvironmentKeys > 1<<16 || options.MaxEnvironmentBytes > 64<<20 || options.MaxReplicaProjects > 64 {
		panic("tracing management options exceed hard limits")
	}
	return options
}

// Manager resolves an immutable tracing configuration from one credential
// source and one caller-supplied environment snapshot.
type Manager struct {
	store   CredentialStore
	options ManagementOptions
}

// NewManager constructs a manager without reading credentials or environment.
func NewManager(store CredentialStore, options ManagementOptions) *Manager {
	if nilValue(store) {
		panic("tracing credential store is required")
	}
	return &Manager{store: store, options: options.withDefaults()}
}

// Status is a secret-free, deterministic configuration report.
type Status struct {
	Enabled            bool
	ExplicitlyDisabled bool
	Orphaned           bool
	HasCredentials     bool
	Project            string
	Endpoint           string
	ReplicaProjects    []string
}

// Configuration is an immutable resolved configuration. It intentionally
// exposes only a redacted status and derived environments, not the API key.
type Configuration struct {
	status   Status
	agent    map[string]string
	original map[string]*string
	secrets  []string
	endpoint string
	apiKey   string
}

// ResolveSink constructs the enabled provider sink through a required factory.
// Disabled configurations return a no-op sink without calling the factory.
func (configuration Configuration) ResolveSink(ctx context.Context, factory SinkFactory) (Sink, error) {
	if nilValue(factory) {
		panic("tracing sink factory is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !configuration.status.Enabled {
		return discardSink{}, nil
	}
	sink, err := callSinkFactory(ctx, factory, configuration.endpoint, configuration.apiKey)
	if err != nil || nilValue(sink) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrSinkFactory
	}
	return sink, nil
}

func callSinkFactory(ctx context.Context, factory SinkFactory, endpoint, apiKey string) (sink Sink, err error) {
	defer func() {
		if recover() != nil {
			sink, err = nil, ErrSinkFactory
		}
	}()
	return factory.NewTracingSink(ctx, endpoint, apiKey)
}

type discardSink struct{}

func (discardSink) Begin(context.Context, Run) (Span, error) { return discardSpan{}, nil }

type discardSpan struct{}

func (discardSpan) End(context.Context, Completion) error { return nil }

// NewManaged constructs the fully managed tracing integration. Stored or
// environment credentials enable tracing unless explicitly disabled; orphaned
// tracing fails closed. Known credential values are redacted from every trace,
// and configured replica projects receive equivalent spans.
func NewManaged(runtime datalon.Runtime, sink Sink, assistantID string, configuration Configuration, options Options) *Runtime {
	status := configuration.Status()
	options.Project = status.Project
	managed := newRuntime(runtime, sink, assistantID, options, status.Enabled)
	managed.replicas = append([]string(nil), status.ReplicaProjects...)
	managed.redact = configuration.redact
	managed.safeErrors = true
	return managed
}

// Resolve loads the optional stored credential once and applies pinned
// prefixed/canonical precedence without mutating the process environment.
func (manager *Manager) Resolve(ctx context.Context, environment map[string]string) (Configuration, error) {
	if manager == nil || nilValue(manager.store) {
		panic("initialized tracing manager is required")
	}
	if environment == nil {
		environment = processEnvironment()
	}
	if err := validateEnvironment(environment, manager.options); err != nil {
		return Configuration{}, err
	}
	stored, err := loadCredential(ctx, manager.store)
	if err != nil {
		if ctx.Err() != nil {
			return Configuration{}, ctx.Err()
		}
		return Configuration{}, ErrCredentialStore
	}
	if err := validateLoadedCredential(stored); err != nil {
		return Configuration{}, err
	}
	working := cloneEnvironment(environment)
	original := snapshotOriginal(environment)

	apiKey, keySpecified := prefixedValue(environment, "LANGSMITH_API_KEY")
	keyConfigured := apiKey != ""
	if !keySpecified {
		apiKey = strings.TrimSpace(environment["LANGCHAIN_API_KEY"])
		keyConfigured = apiKey != ""
	}
	if !keySpecified && !keyConfigured && stored.apiKey != "" {
		apiKey = stored.apiKey
		keyConfigured = true
	}
	if _, prefixed := environment[managementPrefix+"LANGSMITH_API_KEY"]; prefixed {
		if apiKey == "" {
			delete(working, "LANGSMITH_API_KEY")
		} else {
			working["LANGSMITH_API_KEY"] = apiKey
		}
	}

	flag, flagSet := tracingFlag(environment)
	explicitlyDisabled := flagSet && !flag
	if !flagSet && stored.apiKey != "" {
		flag = true
	}

	endpoint := firstNonEmptyPrefixed(environment, "LANGSMITH_ENDPOINT", "LANGCHAIN_ENDPOINT")
	if endpoint == "" {
		endpoint = stored.endpoint
	}
	if endpoint != "" {
		endpoint = normalizeEndpoint(endpoint)
		if _, err := validateEndpoint(endpoint); err != nil {
			return Configuration{}, err
		}
	}
	project := firstNonEmptyPrefixed(environment, "LANGSMITH_PROJECT")
	if project == "" {
		project = stored.project
	}
	if project == "" {
		project = manager.options.DefaultProject
	}
	if !validProject(project, false) {
		return Configuration{}, ErrInvalidTracing
	}
	replicas, err := parseReplicas(environment[managementPrefix+"LANGSMITH_REPLICA_PROJECTS"], manager.options.MaxReplicaProjects)
	if err != nil {
		return Configuration{}, err
	}
	hasReplicaEndpoints := validRunsEndpoints(environment)
	if apiKey != "" {
		if strings.Contains(project, apiKey) || strings.Contains(endpoint, apiKey) {
			return Configuration{}, ErrInvalidTracing
		}
		for _, replica := range replicas {
			if strings.Contains(replica, apiKey) {
				return Configuration{}, ErrInvalidTracing
			}
		}
	}
	hasCustomEndpoint := endpoint != "" && !strings.EqualFold(strings.TrimRight(endpoint, "/"), defaultUSEndpoint)
	orphaned := flag && !keyConfigured && !hasCustomEndpoint && !hasReplicaEndpoints
	enabled := flag && !explicitlyDisabled && !orphaned

	if enabled {
		working["LANGSMITH_TRACING"] = "true"
		working["LANGSMITH_PROJECT"] = project
		if apiKey != "" {
			working["LANGSMITH_API_KEY"] = apiKey
		}
		if endpoint != "" {
			working["LANGSMITH_ENDPOINT"] = endpoint
		}
	} else {
		delete(working, "LANGSMITH_TRACING")
		delete(working, "LANGSMITH_TRACING_V2")
		delete(working, "LANGCHAIN_TRACING")
		delete(working, "LANGCHAIN_TRACING_V2")
	}
	status := Status{Enabled: enabled, ExplicitlyDisabled: explicitlyDisabled, Orphaned: orphaned, HasCredentials: keyConfigured, Project: project, Endpoint: publicEndpoint(endpoint), ReplicaProjects: append([]string(nil), replicas...)}
	secrets := []string{}
	if apiKey != "" {
		secrets = append(secrets, apiKey)
	}
	secrets = append(secrets, runsEndpointSecrets(environment)...)
	secrets = normalizeSecrets(secrets)
	return Configuration{status: status, agent: working, original: original, secrets: secrets, endpoint: endpoint, apiKey: apiKey}, nil
}

func loadCredential(ctx context.Context, store CredentialStore) (credential Credential, err error) {
	defer func() {
		if recover() != nil {
			credential, err = Credential{}, ErrCredentialStore
		}
	}()
	return store.LoadTracingCredential(ctx)
}

// Status returns a defensive copy containing no credential values.
func (configuration Configuration) Status() Status {
	status := configuration.status
	status.ReplicaProjects = append([]string(nil), status.ReplicaProjects...)
	return status
}

// AgentEnvironment returns the canonical environment for the agent trace
// runtime. It may contain credentials and must not be logged.
func (configuration Configuration) AgentEnvironment() map[string]string {
	return cloneEnvironment(configuration.agent)
}

// ShellEnvironment overlays the caller's environment with the original
// tracing values captured before agent-specific routing. It may contain
// credentials and must not be logged.
func (configuration Configuration) ShellEnvironment(base map[string]string) map[string]string {
	result := cloneEnvironment(base)
	for key, value := range configuration.original {
		if value == nil {
			delete(result, key)
		} else {
			result[key] = *value
		}
	}
	return result
}

func (configuration Configuration) redact(value string) string {
	for _, secret := range configuration.secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, redactedValue)
		}
	}
	return value
}

func validateEnvironment(environment map[string]string, options ManagementOptions) error {
	if len(environment) > options.MaxEnvironmentKeys {
		return ErrInvalidTracing
	}
	total := 0
	for key, value := range environment {
		total += len(key) + len(value)
		if len(key) > 1024 || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) || total > options.MaxEnvironmentBytes {
			return ErrInvalidTracing
		}
	}
	return nil
}

func validateLoadedCredential(credential Credential) error {
	if credential.apiKey == "" {
		if credential.endpoint != "" || credential.project != "" {
			return ErrInvalidTracing
		}
		return nil
	}
	if len(credential.apiKey) > maxCredentialBytes || strings.ContainsAny(credential.apiKey, "\x00\r\n") || !validProject(credential.project, true) {
		return ErrInvalidTracing
	}
	if credential.endpoint != "" {
		if _, err := validateEndpoint(credential.endpoint); err != nil {
			return err
		}
	}
	return nil
}

func tracingFlag(environment map[string]string) (bool, bool) {
	anyTrue, anyFalse := false, false
	bridged := map[string]struct{}{"LANGSMITH_TRACING": {}, "LANGCHAIN_TRACING_V2": {}}
	for _, name := range []string{"LANGSMITH_TRACING_V2", "LANGCHAIN_TRACING_V2", "LANGSMITH_TRACING", "LANGCHAIN_TRACING"} {
		raw, present := environment[name]
		if _, ok := bridged[name]; ok {
			if prefixed, exists := environment[managementPrefix+name]; exists {
				if strings.TrimSpace(prefixed) == "" {
					if value, valid := classifyBool(raw); present && valid && value {
						anyFalse = true
					}
					continue
				}
				raw, present = prefixed, true
			}
		}
		if !present {
			continue
		}
		if value, valid := classifyBool(raw); valid {
			if value {
				anyTrue = true
			} else {
				anyFalse = true
			}
		}
	}
	if anyFalse {
		return false, true
	}
	return anyTrue, anyTrue
}

func classifyBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func prefixedValue(environment map[string]string, canonical string) (string, bool) {
	if value, ok := environment[managementPrefix+canonical]; ok {
		return strings.TrimSpace(value), true
	}
	value, ok := environment[canonical]
	return strings.TrimSpace(value), ok && strings.TrimSpace(value) != ""
}

func firstNonEmptyPrefixed(environment map[string]string, names ...string) string {
	for _, name := range names {
		if value, ok := prefixedValue(environment, name); ok && value != "" {
			return value
		}
	}
	return ""
}

func normalizeEndpoint(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "us":
		return defaultUSEndpoint
	case "eu":
		return defaultEUEndpoint
	default:
		return strings.TrimRight(value, "/")
	}
}

func validateEndpoint(value string) (*url.URL, error) {
	if len(value) > 16<<10 || strings.ContainsAny(value, "\x00\r\n") {
		return nil, ErrInvalidTracing
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, ErrInvalidTracing
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, ErrInvalidTracing
	}
	return parsed, nil
}

func publicEndpoint(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(strings.Trim(host, "[]"))
	return address != nil && address.IsLoopback()
}

func validProject(project string, allowEmpty bool) bool {
	if project == "" {
		return allowEmpty
	}
	return len(project) <= maxProjectBytes && strings.TrimSpace(project) == project && strings.IndexFunc(project, unicode.IsControl) < 0
}

func parseReplicas(raw string, maximum int) ([]string, error) {
	seen := map[string]struct{}{}
	result := []string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !validProject(item, false) {
			return nil, ErrInvalidTracing
		}
		if _, exists := seen[item]; exists {
			continue
		}
		if len(result) == maximum {
			return nil, ErrInvalidTracing
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}

func validRunsEndpoints(environment map[string]string) bool {
	valid, _ := runsEndpointInfo(environment)
	return valid
}

func runsEndpointSecrets(environment map[string]string) []string {
	_, secrets := runsEndpointInfo(environment)
	return secrets
}

func runsEndpointInfo(environment map[string]string) (bool, []string) {
	valid := false
	result := []string{}
	for _, name := range []string{"LANGSMITH_RUNS_ENDPOINTS", "LANGCHAIN_RUNS_ENDPOINTS"} {
		raw := strings.TrimSpace(environment[name])
		if raw == "" || len(raw) > 1<<20 {
			continue
		}
		var value any
		if json.Unmarshal([]byte(raw), &value) != nil {
			continue
		}
		switch typed := value.(type) {
		case map[string]any:
			for endpoint, key := range typed {
				if secret, ok := key.(string); ok {
					if _, err := validateEndpoint(endpoint); err == nil {
						valid = true
						if secret != "" {
							result = append(result, secret)
						}
					}
				}
			}
		case []any:
			for _, item := range typed {
				entry, ok := item.(map[string]any)
				endpoint, endpointOK := entry["api_url"].(string)
				secret, keyOK := entry["api_key"].(string)
				if ok && endpointOK && keyOK {
					if _, err := validateEndpoint(endpoint); err == nil {
						valid = true
						if secret != "" {
							result = append(result, secret)
						}
					}
				}
			}
		}
	}
	return valid, result
}

func normalizeSecrets(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		if len(result[left]) == len(result[right]) {
			return result[left] < result[right]
		}
		return len(result[left]) > len(result[right])
	})
	return result
}

func snapshotOriginal(environment map[string]string) map[string]*string {
	result := map[string]*string{}
	keys := []string{"LANGSMITH_PROJECT", "LANGSMITH_TRACING", "LANGSMITH_TRACING_V2", "LANGCHAIN_TRACING", "LANGCHAIN_TRACING_V2", "LANGSMITH_API_KEY", "LANGCHAIN_API_KEY", "LANGSMITH_ENDPOINT", "LANGCHAIN_ENDPOINT"}
	sort.Strings(keys)
	for _, key := range keys {
		if value, ok := environment[key]; ok {
			copy := value
			result[key] = &copy
		} else {
			result[key] = nil
		}
	}
	return result
}

func cloneEnvironment(environment map[string]string) map[string]string {
	result := make(map[string]string, len(environment))
	for key, value := range environment {
		result[key] = value
	}
	return result
}

func (status Status) String() string {
	return fmt.Sprintf("tracing.Status{Enabled:%t, ExplicitlyDisabled:%t, Orphaned:%t, HasCredentials:%t, Project:%q, Endpoint:%q, ReplicaProjects:%q}", status.Enabled, status.ExplicitlyDisabled, status.Orphaned, status.HasCredentials, status.Project, status.Endpoint, status.ReplicaProjects)
}
