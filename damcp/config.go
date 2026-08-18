package damcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"
)

// ConfigScope identifies who supplied an MCP server definition.
type ConfigScope string

const (
	UserConfigScope     ConfigScope = "user"
	ProjectConfigScope  ConfigScope = "project"
	ExplicitConfigScope ConfigScope = "explicit"
)

// ConfigOptions contains optional discovery limits and an explicit config
// override. Its zero value discovers the standard bounded locations.
type ConfigOptions struct {
	ExplicitPath       string
	MaxConfigBytes     int64
	MaxServers         int
	MaxDefinitionBytes int
}

func (options ConfigOptions) withConfigDefaults() ConfigOptions {
	if options.MaxConfigBytes == 0 {
		options.MaxConfigBytes = 4 << 20
	}
	if options.MaxServers == 0 {
		options.MaxServers = 256
	}
	if options.MaxDefinitionBytes == 0 {
		options.MaxDefinitionBytes = 1 << 20
	}
	if options.MaxConfigBytes < 1 || options.MaxConfigBytes > 16<<20 ||
		options.MaxServers < 1 || options.MaxServers > 4096 ||
		options.MaxDefinitionBytes < 2 || options.MaxDefinitionBytes > 4<<20 {
		panic("damcp: invalid config discovery limits")
	}
	if len(options.ExplicitPath) > 4096 || strings.ContainsRune(options.ExplicitPath, 0) {
		panic("damcp: invalid explicit config path")
	}
	return options
}

// ConfigSource is one candidate source in low-to-high precedence order.
type ConfigSource struct {
	Path   string
	Scope  ConfigScope
	Exists bool
}

// ConfiguredServer is one winning raw server definition. Definition remains
// uninterpolated so project trust can bind the exact declared value.
type ConfiguredServer struct {
	Name       string
	Definition json.RawMessage
	Source     string
	Scope      ConfigScope
}

// ConfigDiagnostic identifies a skipped source or server without including
// its definition, command, URL, headers, or environment values.
type ConfigDiagnostic struct {
	Path   string
	Server string
	Reason string
}

// ConfigReport contains deterministic merged discovery output. A later source
// shadows a same-name earlier source even when its server definition is later
// rejected during activation.
type ConfigReport struct {
	Sources     []ConfigSource
	Servers     []ConfiguredServer
	Diagnostics []ConfigDiagnostic
}

// DiscoverConfigs loads the standard user and project .mcp.json files in
// ascending precedence. Empty home or project paths omit that layer. An
// explicit path bypasses standard discovery and must load successfully.
func DiscoverConfigs(ctx context.Context, homeDirectory, projectRoot string, options ConfigOptions) (ConfigReport, error) {
	if ctx == nil {
		panic("damcp: config discovery context is required")
	}
	options = options.withConfigDefaults()
	if err := ctx.Err(); err != nil {
		return ConfigReport{}, err
	}
	for _, value := range []string{homeDirectory, projectRoot} {
		if value != "" && (!filepath.IsAbs(value) || len(value) > 4096 || strings.ContainsRune(value, 0)) {
			panic("damcp: config roots must be bounded absolute paths")
		}
	}
	candidates := standardConfigSources(homeDirectory, projectRoot)
	explicit := strings.TrimSpace(options.ExplicitPath)
	if explicit != "" {
		if !filepath.IsAbs(explicit) {
			if projectRoot == "" {
				panic("damcp: relative explicit config requires a project root")
			}
			explicit = filepath.Join(projectRoot, filepath.FromSlash(explicit))
		}
		candidates = []ConfigSource{{Path: filepath.Clean(explicit), Scope: ExplicitConfigScope}}
	}
	report := ConfigReport{Sources: append([]ConfigSource(nil), candidates...)}
	winning := map[string]ConfiguredServer{}
	for index := range report.Sources {
		if err := ctx.Err(); err != nil {
			return ConfigReport{}, err
		}
		definitions, exists, err := readConfigSource(report.Sources[index].Path, options)
		report.Sources[index].Exists = exists
		if explicit != "" && !exists && err == nil {
			return ConfigReport{}, fmt.Errorf("open explicit MCP config: %w", os.ErrNotExist)
		}
		if err != nil {
			if explicit != "" {
				return ConfigReport{}, err
			}
			report.Diagnostics = append(report.Diagnostics, ConfigDiagnostic{Path: report.Sources[index].Path, Reason: publicConfigReason(err)})
			continue
		}
		for name, definition := range definitions {
			winning[name] = ConfiguredServer{
				Name: name, Definition: append(json.RawMessage(nil), definition...),
				Source: report.Sources[index].Path, Scope: report.Sources[index].Scope,
			}
		}
		if len(winning) > options.MaxServers {
			return ConfigReport{}, fmt.Errorf("MCP config exceeds %d merged servers", options.MaxServers)
		}
	}
	names := make([]string, 0, len(winning))
	for name := range winning {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		report.Servers = append(report.Servers, winning[name])
	}
	sort.Slice(report.Diagnostics, func(i, j int) bool {
		if report.Diagnostics[i].Path == report.Diagnostics[j].Path {
			return report.Diagnostics[i].Server < report.Diagnostics[j].Server
		}
		return report.Diagnostics[i].Path < report.Diagnostics[j].Path
	})
	return report, nil
}

func standardConfigSources(homeDirectory, projectRoot string) []ConfigSource {
	var sources []ConfigSource
	if homeDirectory != "" {
		sources = append(sources, ConfigSource{Path: filepath.Join(homeDirectory, ".deepagents", ".mcp.json"), Scope: UserConfigScope})
	}
	if projectRoot != "" {
		sources = append(sources,
			ConfigSource{Path: filepath.Join(projectRoot, ".deepagents", ".mcp.json"), Scope: ProjectConfigScope},
			ConfigSource{Path: filepath.Join(projectRoot, ".mcp.json"), Scope: ProjectConfigScope},
		)
	}
	return sources
}

func readConfigSource(filePath string, options ConfigOptions) (map[string]json.RawMessage, bool, error) {
	info, err := os.Lstat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect config: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > options.MaxConfigBytes {
		return nil, true, errors.New("config must be a bounded regular non-symlink file")
	}
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return nil, true, fmt.Errorf("open config directory: %w", err)
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(filePath))
	if err != nil {
		return nil, true, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, true, errors.New("config changed while opening")
	}
	limited := &io.LimitedReader{R: file, N: options.MaxConfigBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, true, fmt.Errorf("read config: %w", err)
	}
	if limited.N == 0 || !utf8.Valid(data) {
		return nil, true, errors.New("config is oversized or not UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var top map[string]json.RawMessage
	if err := decoder.Decode(&top); err != nil {
		return nil, true, errors.New("config is not valid JSON")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, true, err
	}
	serversRaw, exists := top["mcpServers"]
	if !exists {
		return nil, true, errors.New("config is missing mcpServers")
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil || servers == nil {
		return nil, true, errors.New("mcpServers must be an object")
	}
	if len(servers) > options.MaxServers {
		return nil, true, fmt.Errorf("config exceeds %d servers", options.MaxServers)
	}
	if len(servers) == 0 {
		return nil, true, errors.New("mcpServers must not be empty")
	}
	for name, definition := range servers {
		if !validMCPServerName(name) || len(definition) > options.MaxDefinitionBytes || len(definition) == 0 || definition[0] != '{' {
			return nil, true, fmt.Errorf("server %q has an invalid name or definition", boundedServerName(name))
		}
	}
	return servers, true, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errors.New("config contains trailing JSON")
}

func publicConfigReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func boundedServerName(name string) string {
	if len(name) > 128 {
		return "<oversized>"
	}
	return name
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validMCPServerName(name string) bool {
	return len(name) <= 128 && mcpServerName.MatchString(name)
}

// Connection is one validated and environment-resolved MCP connection. It
// contains credentials when its source definition references credential
// environment variables and must not be logged or persisted.
type Connection struct {
	Name          string
	Transport     string
	URL           string
	Command       string
	CWD           string
	Args          []string
	Env           map[string]string
	Headers       map[string]string
	Auth          string
	AllowedTools  []string
	DisabledTools []string
}

type rawConnection struct {
	Type          string            `json:"type"`
	Transport     string            `json:"transport"`
	URL           string            `json:"url"`
	Command       string            `json:"command"`
	CWD           string            `json:"cwd"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	Headers       map[string]string `json:"headers"`
	Auth          string            `json:"auth"`
	AllowedTools  []string          `json:"allowedTools"`
	DisabledTools []string          `json:"disabledTools"`
}

// ResolveConnection validates and interpolates one discovered server. Lookup
// is required and should read the already-approved process environment.
func ResolveConnection(server ConfiguredServer, lookup LookupEnv) (Connection, error) {
	if lookup == nil {
		panic("damcp: MCP environment lookup is required")
	}
	if !validMCPServerName(server.Name) || len(server.Definition) == 0 || len(server.Definition) > 4<<20 {
		return Connection{}, errors.New("invalid MCP server definition")
	}
	decoder := json.NewDecoder(bytes.NewReader(server.Definition))
	var raw rawConnection
	if err := decoder.Decode(&raw); err != nil {
		return Connection{}, fmt.Errorf("MCP server %q has an invalid definition", server.Name)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Connection{}, fmt.Errorf("MCP server %q has an invalid definition", server.Name)
	}
	resolved, err := resolveRawConnection(server.Name, raw, lookup)
	if err != nil {
		return Connection{}, err
	}
	return resolved, nil
}

func resolveRawConnection(name string, raw rawConnection, lookup LookupEnv) (Connection, error) {
	kind := strings.ToLower(strings.TrimSpace(raw.Type))
	transport := strings.ToLower(strings.TrimSpace(raw.Transport))
	if kind != "" && transport != "" && normalizeTransport(kind) != normalizeTransport(transport) {
		return Connection{}, fmt.Errorf("MCP server %q declares conflicting transports", name)
	}
	if kind == "" {
		kind = transport
	}
	if kind == "" {
		if raw.URL != "" {
			kind = "http"
		} else {
			kind = "stdio"
		}
	}
	kind = normalizeTransport(kind)
	connection := Connection{Name: name, Transport: kind, Auth: strings.TrimSpace(raw.Auth)}
	var err error
	connection.URL, err = interpolateMCPValue(raw.URL, lookup)
	if err != nil {
		return Connection{}, fmt.Errorf("MCP server %q URL environment is unresolved", name)
	}
	connection.Command, err = interpolateMCPValue(raw.Command, lookup)
	if err != nil {
		return Connection{}, fmt.Errorf("MCP server %q command environment is unresolved", name)
	}
	connection.CWD, err = interpolateMCPValue(raw.CWD, lookup)
	if err != nil {
		return Connection{}, fmt.Errorf("MCP server %q working directory environment is unresolved", name)
	}
	connection.Args, err = interpolateMCPSlice(raw.Args, lookup, 256)
	if err != nil {
		return Connection{}, fmt.Errorf("MCP server %q argument environment is unresolved", name)
	}
	connection.Env, err = interpolateMCPMap(raw.Env, lookup, 256, false)
	if err != nil {
		return Connection{}, fmt.Errorf("MCP server %q environment is unresolved", name)
	}
	connection.Headers, err = interpolateMCPMap(raw.Headers, lookup, 128, true)
	if err != nil {
		return Connection{}, fmt.Errorf("MCP server %q header environment is unresolved", name)
	}
	connection.AllowedTools = append([]string(nil), raw.AllowedTools...)
	connection.DisabledTools = append([]string(nil), raw.DisabledTools...)
	if err := validateConnection(connection); err != nil {
		return Connection{}, err
	}
	return connection, nil
}

func normalizeTransport(value string) string {
	switch value {
	case "streamable_http", "streamable-http":
		return "http"
	default:
		return value
	}
}

func validateConnection(connection Connection) error {
	switch connection.Transport {
	case "stdio":
		if strings.TrimSpace(connection.Command) == "" || connection.URL != "" || len(connection.Command) > 16<<10 || strings.ContainsRune(connection.Command, 0) {
			return fmt.Errorf("MCP server %q has an invalid stdio command", connection.Name)
		}
		if connection.CWD != "" && (!filepath.IsAbs(connection.CWD) || len(connection.CWD) > 4096 || strings.ContainsRune(connection.CWD, 0)) {
			return fmt.Errorf("MCP server %q has an invalid working directory", connection.Name)
		}
		if connection.Auth != "" {
			return fmt.Errorf("MCP server %q cannot use authentication with stdio", connection.Name)
		}
	case "http", "sse":
		if connection.Command != "" || connection.CWD != "" {
			return fmt.Errorf("MCP server %q remote transport cannot set a command", connection.Name)
		}
		parsed, err := url.Parse(connection.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return fmt.Errorf("MCP server %q has an invalid remote URL", connection.Name)
		}
		if connection.Auth != "" && connection.Auth != "oauth" {
			return fmt.Errorf("MCP server %q has unsupported authentication", connection.Name)
		}
		if connection.Auth == "oauth" {
			if parsed.Scheme != "https" {
				return fmt.Errorf("MCP server %q OAuth endpoint must use HTTPS", connection.Name)
			}
			for header := range connection.Headers {
				if strings.EqualFold(header, "Authorization") {
					return fmt.Errorf("MCP server %q cannot combine OAuth with an Authorization header", connection.Name)
				}
			}
		}
	default:
		return fmt.Errorf("MCP server %q has unsupported transport", connection.Name)
	}
	if len(connection.AllowedTools) > 0 && len(connection.DisabledTools) > 0 {
		return fmt.Errorf("MCP server %q cannot set both tool filters", connection.Name)
	}
	for _, argument := range connection.Args {
		if len(argument) > 64<<10 || strings.ContainsRune(argument, 0) {
			return fmt.Errorf("MCP server %q has an invalid argument", connection.Name)
		}
	}
	for key, value := range connection.Env {
		if key == "" || len(key) > 1024 || strings.ContainsAny(key, "=\x00") || len(value) > 64<<10 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("MCP server %q has an invalid environment entry", connection.Name)
		}
	}
	for key, value := range connection.Headers {
		if len(key) > 256 || len(value) > 16<<10 || !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("MCP server %q has an invalid header", connection.Name)
		}
	}
	for _, patterns := range [][]string{connection.AllowedTools, connection.DisabledTools} {
		if len(patterns) > 512 {
			return fmt.Errorf("MCP server %q has too many tool filters", connection.Name)
		}
		for _, patternValue := range patterns {
			if patternValue == "" || len(patternValue) > 1024 {
				return fmt.Errorf("MCP server %q has an invalid tool filter", connection.Name)
			}
			if _, err := path.Match(patternValue, "tool"); err != nil {
				return fmt.Errorf("MCP server %q has an invalid tool filter", connection.Name)
			}
		}
	}
	return nil
}

var mcpEnvReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^{}]*))?\}`)

func interpolateMCPValue(value string, lookup LookupEnv) (string, error) {
	spans := mcpEnvReference.FindAllStringIndex(value, -1)
	for offset := 0; ; {
		index := strings.Index(value[offset:], "${")
		if index < 0 {
			break
		}
		position := offset + index
		valid := false
		for _, span := range spans {
			if span[0] == position {
				valid = true
				break
			}
		}
		if !valid {
			return "", errors.New("malformed environment reference")
		}
		offset = position + 2
	}
	var interpolationError error
	resolved := mcpEnvReference.ReplaceAllStringFunc(value, func(reference string) string {
		match := mcpEnvReference.FindStringSubmatch(reference)
		name, fallback := match[1], match[2]
		if current, exists := lookup(name); exists && current != "" {
			return current
		} else if len(match) > 2 && strings.Contains(reference, ":-") {
			return fallback
		} else if exists {
			return ""
		}
		interpolationError = errors.New("unset environment variable")
		return ""
	})
	if interpolationError != nil {
		return "", interpolationError
	}
	if len(resolved) > 64<<10 || strings.ContainsRune(resolved, 0) {
		return "", errors.New("interpolated value exceeds bounds")
	}
	return resolved, nil
}

func interpolateMCPSlice(values []string, lookup LookupEnv, limit int) ([]string, error) {
	if len(values) > limit {
		return nil, errors.New("list exceeds bounds")
	}
	result := make([]string, len(values))
	for index, value := range values {
		resolved, err := interpolateMCPValue(value, lookup)
		if err != nil {
			return nil, err
		}
		result[index] = resolved
	}
	return result, nil
}

func interpolateMCPMap(values map[string]string, lookup LookupEnv, limit int, header bool) (map[string]string, error) {
	if len(values) > limit {
		return nil, errors.New("mapping exceeds bounds")
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		resolved, err := interpolateMCPValue(value, lookup)
		if err != nil {
			return nil, err
		}
		if header && !httpguts.ValidHeaderFieldName(key) {
			return nil, errors.New("invalid header name")
		}
		result[key] = resolved
	}
	return result, nil
}

// MatchTool reports whether a discovered tool survives the connection's
// allow/deny filters. Both bare and server-prefixed names are matched.
func (connection Connection) MatchTool(toolName string) bool {
	prefixed := connection.Name + "_" + toolName
	match := func(patterns []string) bool {
		for _, patternValue := range patterns {
			bare, _ := path.Match(patternValue, toolName)
			qualified, _ := path.Match(patternValue, prefixed)
			if bare || qualified {
				return true
			}
		}
		return false
	}
	if len(connection.AllowedTools) > 0 {
		return match(connection.AllowedTools)
	}
	return !match(connection.DisabledTools)
}

// HTTPHeaders returns a detached header map for transport construction.
func (connection Connection) HTTPHeaders() http.Header {
	headers := make(http.Header, len(connection.Headers))
	for key, value := range connection.Headers {
		headers.Set(key, value)
	}
	return headers
}
