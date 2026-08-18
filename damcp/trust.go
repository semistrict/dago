// Package damcp implements host-neutral policy for project-supplied MCP servers.
package damcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/semistrict/dago/dagit"
)

const (
	DangerouslyEnableProjectServersEnv = "DEEPAGENTS_CODE_DANGEROUSLY_ENABLE_PROJECT_MCP_SERVERS"
	DisabledProjectServersEnv          = "DEEPAGENTS_CODE_DISABLED_PROJECT_MCP_SERVERS"
	LegacyEnabledProjectServersEnv     = "DEEPAGENTS_CODE_ENABLED_PROJECT_MCP_SERVERS"
)

var (
	ErrInvalidTrustConfig = errors.New("invalid MCP trust configuration")
	ErrUnknownServer      = errors.New("unknown MCP server")
	ErrTrustLimit         = errors.New("MCP trust limit exceeded")
)

// LookupEnv reads an operator-controlled process environment. A project .env
// loader must not be supplied here because that would let a project trust itself.
type LookupEnv func(string) (string, bool)

// Options contains optional finite policy and definition limits.
type Options struct {
	MaxConfigBytes     int64
	MaxServers         int
	MaxDefinitionBytes int
	MaxEnvBytes        int
}

// DefaultOptions returns finite production limits.
func DefaultOptions() Options {
	return Options{
		MaxConfigBytes:     1 << 20,
		MaxServers:         256,
		MaxDefinitionBytes: 1 << 20,
		MaxEnvBytes:        16 << 10,
	}
}

// Store owns one user-controlled TOML policy file. Construction performs no I/O.
type Store struct {
	path    string
	lookup  LookupEnv
	options Options
	lock    chan struct{}
}

// NewStore constructs a trust-policy store. The user-controlled config path and
// process environment lookup are required positional inputs. Invalid static
// configuration panics; a zero Options value selects useful finite defaults.
func NewStore(configPath string, lookup LookupEnv, options Options) *Store {
	if configPath == "" || strings.ContainsRune(configPath, 0) || len(configPath) > 4096 {
		panic("damcp: bounded config path is required")
	}
	if lookup == nil {
		panic("damcp: environment lookup is required")
	}
	defaults := DefaultOptions()
	if options.MaxConfigBytes == 0 {
		options.MaxConfigBytes = defaults.MaxConfigBytes
	}
	if options.MaxServers == 0 {
		options.MaxServers = defaults.MaxServers
	}
	if options.MaxDefinitionBytes == 0 {
		options.MaxDefinitionBytes = defaults.MaxDefinitionBytes
	}
	if options.MaxEnvBytes == 0 {
		options.MaxEnvBytes = defaults.MaxEnvBytes
	}
	if options.MaxConfigBytes < 1 || options.MaxConfigBytes > 16<<20 ||
		options.MaxServers < 1 || options.MaxServers > 4096 ||
		options.MaxDefinitionBytes < 2 || options.MaxDefinitionBytes > 16<<20 ||
		options.MaxEnvBytes < 1 || options.MaxEnvBytes > 1<<20 {
		panic("damcp: trust limits are outside their finite ranges")
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return &Store{path: filepath.Clean(configPath), lookup: lookup, options: options, lock: lock}
}

// Path returns the configured user policy path without filesystem access.
func (store *Store) Path() string { return store.path }

// Server is one project-supplied MCP server definition. Definition is the raw
// JSON value from the project's MCP configuration.
type Server struct {
	Name       string
	Definition json.RawMessage
}

// Approval is a definition-bound persisted grant. GitCommonDir identifies
// fixed remote servers whose grants can be shared across validated worktrees.
type Approval struct {
	ProjectRoot  string
	Name         string
	Fingerprint  string
	GitCommonDir bool
}

// Diagnostics describes ignored legacy or malformed policy without exposing
// policy values or server definitions.
type Diagnostics struct {
	ReadError            string
	LegacyIgnored        []string
	LegacyEnvIgnored     bool
	MalformedApprovals   int
	EnvironmentTruncated bool
}

// Policy is a loaded user policy. Slices are sorted and contain unique names.
// ReadError means TOML grants and whole-project trust must fail closed; explicit
// process environment grants and denies remain effective.
type Policy struct {
	Enabled     []string
	Disabled    []string
	Approvals   []Approval
	Diagnostics Diagnostics
	maxServers  int
	maxDefBytes int
}

// LoadFailed reports whether the user TOML policy could not be fully enforced.
func (policy Policy) LoadFailed() bool { return policy.Diagnostics.ReadError != "" }

// Resolution partitions configured project servers without executing or
// connecting to them. Prompt contains servers that still need a host decision.
type Resolution struct {
	Allowed  []Server
	Prompt   []Server
	Disabled []Server
}

// Load reads the user-level trust policy. Missing files produce a useful empty
// policy. A malformed or unreadable file is reported in Policy.Diagnostics and
// fails closed rather than being returned as an operational error; cancellation
// is returned as an error so callers can abandon startup promptly.
func (store *Store) Load(ctx context.Context) (Policy, error) {
	if ctx == nil {
		panic("damcp: nil context")
	}
	if err := store.acquire(ctx); err != nil {
		return Policy{}, err
	}
	defer store.release()
	return store.loadLocked(ctx)
}

func (store *Store) loadLocked(ctx context.Context) (Policy, error) {
	if err := ctx.Err(); err != nil {
		return Policy{}, err
	}
	enabled, enabledTruncated := store.envNames(DangerouslyEnableProjectServersEnv)
	disabled, disabledTruncated := store.envNames(DisabledProjectServersEnv)
	_, legacyEnv := store.lookup(LegacyEnabledProjectServersEnv)
	policy := Policy{maxServers: store.options.MaxServers, maxDefBytes: store.options.MaxDefinitionBytes, Diagnostics: Diagnostics{
		LegacyEnvIgnored:     legacyEnv,
		EnvironmentTruncated: enabledTruncated || disabledTruncated,
	}}
	if disabledTruncated {
		// A truncated deny list cannot be enforced. Discard process grants too:
		// the omitted tail could reject one of them, and rejection must win.
		enabled = map[string]struct{}{}
		policy.Diagnostics.ReadError = "disabled project MCP server environment exceeds its configured bound"
	}

	data, err := store.readConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			policy.Enabled = subtractSorted(enabled, disabled)
			policy.Disabled = sortedKeys(disabled)
			return policy, nil
		}
		policy.Diagnostics.ReadError = err.Error()
		policy.Enabled = subtractSorted(enabled, disabled)
		policy.Disabled = sortedKeys(disabled)
		return policy, nil
	}
	if err := ctx.Err(); err != nil {
		return Policy{}, err
	}

	sectionValue, found := data["mcp"]
	section := map[string]any{}
	if found {
		var ok bool
		section, ok = sectionValue.(map[string]any)
		if !ok {
			policy.Diagnostics.ReadError = "[mcp] must be a table"
			policy.Enabled = subtractSorted(enabled, disabled)
			policy.Disabled = sortedKeys(disabled)
			return policy, nil
		}
	}

	tomlDisabled, malformed := parseNames(section["disabled_project_servers"], store.options.MaxServers)
	if malformed {
		policy.Diagnostics.ReadError = "[mcp].disabled_project_servers must be a string or list of strings"
		policy.Enabled = subtractSorted(enabled, disabled)
		policy.Disabled = sortedKeys(disabled)
		return policy, nil
	}
	for _, name := range tomlDisabled {
		disabled[name] = struct{}{}
	}
	legacy, _ := parseNames(section["enabled_project_servers"], store.options.MaxServers)
	policy.Diagnostics.LegacyIgnored = sortedUnique(legacy)
	approvals, dropped := parseApprovals(section["enabled_project_server_approvals"], store.options.MaxServers)
	policy.Diagnostics.MalformedApprovals = dropped

	policy.Enabled = subtractSorted(enabled, disabled)
	policy.Disabled = sortedKeys(disabled)
	for _, approval := range approvals {
		if _, denied := disabled[approval.Name]; !denied {
			policy.Approvals = append(policy.Approvals, approval)
		}
	}
	sortApprovals(policy.Approvals)
	return policy, nil
}

// Resolve applies reject precedence, explicit process grants, remembered
// definition-bound approvals, and optional whole-project session trust. A
// failed policy load prevents whole-project trust but retains process grants.
func (policy Policy) Resolve(projectRoot string, servers []Server, trustProject bool) (Resolution, error) {
	if strings.TrimSpace(projectRoot) == "" || strings.ContainsRune(projectRoot, 0) {
		return Resolution{}, fmt.Errorf("%w: project root is required", ErrInvalidTrustConfig)
	}
	maxServers, maxDefinitionBytes := policy.maxServers, policy.maxDefBytes
	if maxServers == 0 {
		maxServers = DefaultOptions().MaxServers
	}
	if maxDefinitionBytes == 0 {
		maxDefinitionBytes = DefaultOptions().MaxDefinitionBytes
	}
	if len(servers) > maxServers {
		return Resolution{}, fmt.Errorf("%w: too many server definitions", ErrTrustLimit)
	}
	enabled := stringSet(policy.Enabled)
	disabled := stringSet(policy.Disabled)
	resolution := Resolution{}
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		if name == "" || len(name) > 256 || len(server.Definition) > maxDefinitionBytes {
			return Resolution{}, fmt.Errorf("%w: server name or definition is invalid", ErrInvalidTrustConfig)
		}
		if _, duplicate := seen[name]; duplicate {
			return Resolution{}, fmt.Errorf("%w: duplicate server name", ErrInvalidTrustConfig)
		}
		seen[name] = struct{}{}
		copyServer := cloneServer(server, name)
		if _, denied := disabled[name]; denied {
			resolution.Disabled = append(resolution.Disabled, copyServer)
			continue
		}
		if _, allowed := enabled[name]; allowed || (trustProject && !policy.LoadFailed()) {
			resolution.Allowed = append(resolution.Allowed, copyServer)
			continue
		}
		approved, err := policy.approved(projectRoot, copyServer)
		if err != nil {
			return Resolution{}, err
		}
		if approved {
			resolution.Allowed = append(resolution.Allowed, copyServer)
		} else {
			resolution.Prompt = append(resolution.Prompt, copyServer)
		}
	}
	return resolution, nil
}

func (policy Policy) approved(projectRoot string, server Server) (bool, error) {
	if policy.LoadFailed() {
		return false, nil
	}
	approval, err := approvalFor(projectRoot, server)
	if err != nil {
		return false, err
	}
	for _, candidate := range policy.Approvals {
		if candidate == approval {
			return true, nil
		}
	}
	if !approval.GitCommonDir {
		return false, nil
	}
	legacy, err := approvalForExactRoot(projectRoot, server)
	if err != nil {
		return false, err
	}
	for _, candidate := range policy.Approvals {
		if candidate == legacy {
			return true, nil
		}
	}
	return false, nil
}

// Remember atomically persists approvals for the named servers. Unknown names,
// malformed definitions, unreadable existing TOML, cancellation, and exceeded
// limits fail without replacing the policy file. An empty name list is a no-op.
func (store *Store) Remember(ctx context.Context, projectRoot string, servers []Server, names ...string) error {
	if ctx == nil {
		panic("damcp: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(servers) > store.options.MaxServers || len(names) > store.options.MaxServers {
		return fmt.Errorf("%w: too many servers", ErrTrustLimit)
	}
	byName := make(map[string]Server, len(servers))
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		if name == "" || len(name) > 256 || len(server.Definition) > store.options.MaxDefinitionBytes {
			return fmt.Errorf("%w: server name or definition is invalid", ErrInvalidTrustConfig)
		}
		if _, duplicate := byName[name]; duplicate {
			return fmt.Errorf("%w: duplicate server name", ErrInvalidTrustConfig)
		}
		byName[name] = cloneServer(server, name)
	}
	cleanNames := sortedUnique(names)
	if len(cleanNames) == 0 {
		return nil
	}
	toAdd := make([]Approval, 0, len(cleanNames))
	for _, name := range cleanNames {
		server, ok := byName[name]
		if !ok {
			return fmt.Errorf("%w: %q", ErrUnknownServer, name)
		}
		approval, err := approvalFor(projectRoot, server)
		if err != nil {
			return err
		}
		toAdd = append(toAdd, approval)
	}
	if err := store.acquire(ctx); err != nil {
		return err
	}
	defer store.release()
	return store.rememberLocked(ctx, cleanNames, toAdd)
}

func (store *Store) rememberLocked(ctx context.Context, names []string, toAdd []Approval) error {
	data, err := store.readConfig()
	if errors.Is(err, os.ErrNotExist) {
		data = map[string]any{}
	} else if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	section := map[string]any{}
	if raw, found := data["mcp"]; found {
		var ok bool
		section, ok = raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: [mcp] must be a table", ErrInvalidTrustConfig)
		}
	}
	existing, _ := parseApprovals(section["enabled_project_server_approvals"], store.options.MaxServers)
	merged := make(map[Approval]struct{}, len(existing)+len(toAdd))
	for _, approval := range existing {
		merged[approval] = struct{}{}
	}
	for _, approval := range toAdd {
		merged[approval] = struct{}{}
	}
	if len(merged) > store.options.MaxServers {
		return fmt.Errorf("%w: too many persisted approvals", ErrTrustLimit)
	}
	approvals := make([]Approval, 0, len(merged))
	for approval := range merged {
		approvals = append(approvals, approval)
	}
	sortApprovals(approvals)
	rows := make([]map[string]any, 0, len(approvals))
	for _, approval := range approvals {
		row := map[string]any{
			"project_root": approval.ProjectRoot,
			"name":         approval.Name,
			"fingerprint":  approval.Fingerprint,
		}
		if approval.GitCommonDir {
			row["git_common_dir"] = true
		}
		rows = append(rows, row)
	}
	section["enabled_project_server_approvals"] = rows
	if legacy, malformed := parseNames(section["enabled_project_servers"], store.options.MaxServers); !malformed && len(legacy) > 0 {
		remove := stringSet(names)
		remaining := make([]string, 0, len(legacy))
		for _, name := range legacy {
			if _, selected := remove[name]; !selected {
				remaining = append(remaining, name)
			}
		}
		if len(remaining) == 0 {
			delete(section, "enabled_project_servers")
		} else {
			section["enabled_project_servers"] = remaining
		}
	}
	data["mcp"] = section
	return store.writeConfig(ctx, data)
}

func (store *Store) readConfig() (map[string]any, error) {
	info, err := os.Lstat(store.path)
	if err != nil {
		return nil, fmt.Errorf("read MCP trust policy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > store.options.MaxConfigBytes {
		return nil, fmt.Errorf("%w: policy must be a bounded regular file", ErrInvalidTrustConfig)
	}
	file, err := os.Open(store.path)
	if err != nil {
		return nil, fmt.Errorf("read MCP trust policy: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() > store.options.MaxConfigBytes || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%w: policy changed before it was read", ErrInvalidTrustConfig)
	}
	payload, err := io.ReadAll(io.LimitReader(file, store.options.MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read MCP trust policy: %w", err)
	}
	if int64(len(payload)) > store.options.MaxConfigBytes {
		return nil, fmt.Errorf("%w: policy exceeds %d bytes", ErrTrustLimit, store.options.MaxConfigBytes)
	}
	final, err := file.Stat()
	if err != nil || !final.Mode().IsRegular() || final.Size() > store.options.MaxConfigBytes || !os.SameFile(opened, final) {
		return nil, fmt.Errorf("%w: policy changed while it was read", ErrInvalidTrustConfig)
	}
	var data map[string]any
	if _, err := toml.NewDecoder(bytes.NewReader(payload)).Decode(&data); err != nil {
		return nil, fmt.Errorf("%w: decode policy", ErrInvalidTrustConfig)
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

func (store *Store) writeConfig(ctx context.Context, data map[string]any) error {
	var payload bytes.Buffer
	if err := toml.NewEncoder(&payload).Encode(data); err != nil {
		return fmt.Errorf("%w: encode policy", ErrInvalidTrustConfig)
	}
	if int64(payload.Len()) > store.options.MaxConfigBytes {
		return fmt.Errorf("%w: encoded policy exceeds %d bytes", ErrTrustLimit, store.options.MaxConfigBytes)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create MCP trust directory: %w", err)
	}
	if info, err := os.Lstat(store.path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: policy target is not a regular file", ErrInvalidTrustConfig)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect MCP trust policy: %w", err)
	}
	file, err := os.CreateTemp(parent, ".mcp-trust-*")
	if err != nil {
		return fmt.Errorf("create MCP trust temporary file: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure MCP trust temporary file: %w", err)
	}
	if _, err := file.Write(payload.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("write MCP trust policy: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync MCP trust policy: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close MCP trust policy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporary, store.path); err != nil {
		return fmt.Errorf("replace MCP trust policy: %w", err)
	}
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (store *Store) envNames(key string) (map[string]struct{}, bool) {
	raw, ok := store.lookup(key)
	if !ok {
		return map[string]struct{}{}, false
	}
	if len(raw) > store.options.MaxEnvBytes {
		return map[string]struct{}{}, true
	}
	return stringSet(strings.Split(raw, ",")), false
}

func approvalFor(projectRoot string, server Server) (Approval, error) {
	root, err := normalizedRoot(projectRoot)
	if err != nil {
		return Approval{}, err
	}
	remote, err := fixedRemote(server.Definition)
	if err != nil {
		return Approval{}, err
	}
	common := ""
	if remote {
		common = dagit.FindCommonDir(root)
	}
	if common != "" {
		root = common
	}
	fingerprint, err := Fingerprint(server.Definition)
	if err != nil {
		return Approval{}, err
	}
	return Approval{ProjectRoot: root, Name: strings.TrimSpace(server.Name), Fingerprint: fingerprint, GitCommonDir: common != ""}, nil
}

func approvalForExactRoot(projectRoot string, server Server) (Approval, error) {
	root, err := normalizedRoot(projectRoot)
	if err != nil {
		return Approval{}, err
	}
	fingerprint, err := Fingerprint(server.Definition)
	if err != nil {
		return Approval{}, err
	}
	return Approval{ProjectRoot: root, Name: strings.TrimSpace(server.Name), Fingerprint: fingerprint}, nil
}

// Fingerprint returns the pinned stable SHA-256 fingerprint for one JSON MCP
// server definition. Object key order and insignificant whitespace do not alter it.
func Fingerprint(definition json.RawMessage) (string, error) {
	if len(definition) == 0 || len(definition) > 16<<20 {
		return "", fmt.Errorf("%w: server definition is empty or oversized", ErrInvalidTrustConfig)
	}
	decoder := json.NewDecoder(bytes.NewReader(definition))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("%w: decode server definition", ErrInvalidTrustConfig)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return "", fmt.Errorf("%w: trailing server definition data", ErrInvalidTrustConfig)
	}
	var canonical bytes.Buffer
	if err := writeCanonicalJSON(&canonical, value); err != nil {
		return "", fmt.Errorf("%w: encode server definition", ErrInvalidTrustConfig)
	}
	digest := sha256.Sum256(canonical.Bytes())
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func fixedRemote(definition json.RawMessage) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(definition))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return false, fmt.Errorf("%w: decode server definition", ErrInvalidTrustConfig)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return false, fmt.Errorf("%w: trailing server definition data", ErrInvalidTrustConfig)
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return false, nil
	}
	if _, command := object["command"]; command {
		return false, nil
	}
	url, ok := object["url"].(string)
	if !ok || strings.Contains(url, "${") {
		return false, nil
	}
	transport := object["type"]
	if transport == nil {
		transport = object["transport"]
	}
	if transport == nil {
		return true, nil
	}
	name, ok := transport.(string)
	if !ok {
		return false, nil
	}
	switch name {
	case "http", "sse", "streamable_http", "streamable-http":
		return true, nil
	default:
		return false, nil
	}
}

func normalizedRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" || strings.ContainsRune(root, 0) || len(root) > 4096 {
		return "", fmt.Errorf("%w: project root is required and bounded", ErrInvalidTrustConfig)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("%w: normalize project root", ErrInvalidTrustConfig)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func parseApprovals(value any, limit int) ([]Approval, int) {
	if value == nil {
		return nil, 0
	}
	rows, ok := tableArray(value)
	if !ok {
		return nil, 1
	}
	approvals := make([]Approval, 0, min(len(rows), limit))
	dropped := 0
	for i, row := range rows {
		if i >= limit {
			dropped++
			continue
		}
		projectRoot, rootOK := row["project_root"].(string)
		name, nameOK := row["name"].(string)
		fingerprint, fingerprintOK := row["fingerprint"].(string)
		marker, markerOK := row["git_common_dir"]
		gitCommon := false
		if markerOK {
			gitCommon, markerOK = marker.(bool)
		}
		if !rootOK || !nameOK || !fingerprintOK || !markerOK && row["git_common_dir"] != nil ||
			strings.TrimSpace(projectRoot) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(fingerprint) == "" {
			dropped++
			continue
		}
		if gitCommon {
			if !filepath.IsAbs(projectRoot) || strings.ContainsRune(projectRoot, 0) {
				dropped++
				continue
			}
			projectRoot = filepath.Clean(projectRoot)
		} else {
			var err error
			projectRoot, err = normalizedRoot(projectRoot)
			if err != nil {
				dropped++
				continue
			}
		}
		approvals = append(approvals, Approval{
			ProjectRoot: projectRoot, Name: strings.TrimSpace(name),
			Fingerprint: strings.TrimSpace(fingerprint), GitCommonDir: gitCommon,
		})
	}
	return approvals, dropped
}

func tableArray(value any) ([]map[string]any, bool) {
	switch rows := value.(type) {
	case []map[string]any:
		return rows, true
	case []any:
		result := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			table, ok := row.(map[string]any)
			if !ok {
				return nil, false
			}
			result = append(result, table)
		}
		return result, true
	default:
		return nil, false
	}
}

func parseNames(value any, limit int) ([]string, bool) {
	if value == nil {
		return nil, false
	}
	var values []string
	switch typed := value.(type) {
	case string:
		values = strings.Split(typed, ",")
	case []string:
		values = typed
	case []any:
		for _, item := range typed {
			name, ok := item.(string)
			if ok {
				values = append(values, name)
			}
		}
	default:
		return nil, true
	}
	values = sortedUnique(values)
	if len(values) > limit {
		values = values[:limit]
	}
	return values, false
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("extra JSON value")
	}
	return nil
}

func writeCanonicalJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		buffer.WriteString(strconv.FormatBool(typed))
	case json.Number:
		buffer.WriteString(typed.String())
	case string:
		writeJSONString(buffer, typed)
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeCanonicalJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buffer.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				buffer.WriteByte(',')
			}
			writeJSONString(buffer, key)
			buffer.WriteByte(':')
			if err := writeCanonicalJSON(buffer, typed[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	default:
		return fmt.Errorf("unsupported JSON value %T", value)
	}
	return nil
}

func writeJSONString(buffer *bytes.Buffer, value string) {
	buffer.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"', '\\':
			buffer.WriteByte('\\')
			buffer.WriteRune(char)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		default:
			if char < 0x20 {
				buffer.WriteString(`\u00`)
				const hexadecimal = "0123456789abcdef"
				buffer.WriteByte(hexadecimal[byte(char)>>4])
				buffer.WriteByte(hexadecimal[byte(char)&0x0f])
			} else if char == utf8.RuneError {
				buffer.WriteRune(utf8.RuneError)
			} else {
				buffer.WriteRune(char)
			}
		}
	}
	buffer.WriteByte('"')
}

func cloneServer(server Server, normalizedName string) Server {
	return Server{Name: normalizedName, Definition: append(json.RawMessage(nil), server.Definition...)}
}

func (store *Store) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.lock:
		return nil
	}
}

func (store *Store) release() { store.lock <- struct{}{} }

func sortedUnique(values []string) []string {
	set := stringSet(values)
	return sortedKeys(set)
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 256 {
			set[value] = struct{}{}
		}
	}
	return set
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func subtractSorted(values, remove map[string]struct{}) []string {
	result := make(map[string]struct{}, len(values))
	for value := range values {
		if _, denied := remove[value]; !denied {
			result[value] = struct{}{}
		}
	}
	return sortedKeys(result)
}

func sortApprovals(approvals []Approval) {
	sort.Slice(approvals, func(i, j int) bool {
		left, right := approvals[i], approvals[j]
		if left.ProjectRoot != right.ProjectRoot {
			return left.ProjectRoot < right.ProjectRoot
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Fingerprint != right.Fingerprint {
			return left.Fingerprint < right.Fingerprint
		}
		return !left.GitCommonDir && right.GitCommonDir
	})
}
