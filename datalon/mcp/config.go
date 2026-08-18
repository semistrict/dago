// Package mcp loads Model Context Protocol tools for a long-running assistant.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const (
	talonConfigEnvironment = "DEEPAGENTS_TALON_MCP_CONFIG"
	configEnvironment      = "MCP_CONFIG"
	maxConfigBytes         = 4 << 20
	maxServers             = 64
)

var ErrInvalidConfig = errors.New("invalid MCP configuration")

// Config is the portable .mcp.json document used by the host and Fleet imports.
type Config struct {
	Servers map[string]Server `json:"mcpServers"`
}

// Server describes one trusted local or remote MCP server.
type Server struct {
	Type          string            `json:"type,omitempty"`
	URL           string            `json:"url,omitempty"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Auth          string            `json:"auth,omitempty"`
	AllowedTools  []string          `json:"allowedTools,omitempty"`
	DisabledTools []string          `json:"disabledTools,omitempty"`
}

// Candidate describes one configuration source in highest-to-lowest priority.
type Candidate struct {
	Path     string
	Source   string
	Exists   bool
	Selected bool
}

// Discover returns the supported configuration paths in resolution order. A
// nil environment reads the process environment. Home may be empty to use the
// current user's home directory.
func Discover(environment map[string]string, home string) []Candidate {
	if environment == nil {
		environment = processEnvironment()
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	candidates := make([]Candidate, 0, 3)
	for _, item := range []struct{ value, source string }{
		{environment[talonConfigEnvironment], talonConfigEnvironment},
		{environment[configEnvironment], configEnvironment},
	} {
		if path := strings.TrimSpace(item.value); path != "" {
			candidates = append(candidates, Candidate{Path: expandHome(path, home), Source: item.source})
		}
	}
	if home != "" {
		candidates = append(candidates, Candidate{Path: filepath.Join(home, ".deepagents", ".mcp.json"), Source: "user default"})
	}
	selected := false
	for index := range candidates {
		info, err := os.Stat(candidates[index].Path)
		candidates[index].Exists = err == nil && info.Mode().IsRegular()
		if !selected && candidates[index].Exists {
			candidates[index].Selected = true
			selected = true
		}
	}
	return candidates
}

// Resolve returns the highest-priority existing configuration path. An empty
// path and nil error mean no configuration is present.
func Resolve(environment map[string]string, home string) (string, error) {
	candidates := Discover(environment, home)
	for _, candidate := range candidates {
		if candidate.Source == talonConfigEnvironment || candidate.Source == configEnvironment {
			return candidate.Path, nil
		}
	}
	for _, candidate := range candidates {
		if candidate.Selected {
			return candidate.Path, nil
		}
	}
	return "", nil
}

// LoadConfig parses and validates one bounded external configuration file.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open MCP config: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("inspect MCP config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("%w: config must be a regular file", ErrInvalidConfig)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read MCP config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: config exceeds %d bytes", ErrInvalidConfig, maxConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks static server declarations without performing I/O.
func (config Config) Validate() error {
	if len(config.Servers) > maxServers {
		return fmt.Errorf("%w: at most %d servers are allowed", ErrInvalidConfig, maxServers)
	}
	names := make([]string, 0, len(config.Servers))
	for name := range config.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateServer(name, config.Servers[name]); err != nil {
			return err
		}
	}
	return nil
}

func validateServer(name string, server Server) error {
	if strings.TrimSpace(name) == "" || len(name) > 128 || strings.ContainsAny(name, "\x00\r\n") {
		return fmt.Errorf("%w: server name is invalid", ErrInvalidConfig)
	}
	serverType := strings.ToLower(strings.TrimSpace(server.Type))
	if serverType == "" {
		if server.Command != "" {
			serverType = "stdio"
		} else {
			serverType = "http"
		}
	}
	switch serverType {
	case "http", "streamable_http", "sse":
		if strings.TrimSpace(server.URL) == "" || strings.TrimSpace(server.Command) != "" {
			return fmt.Errorf("%w: server %q requires url and cannot set command", ErrInvalidConfig, name)
		}
		if _, err := validateRemoteURL(server.URL); err != nil {
			return fmt.Errorf("%w: server %q: %v", ErrInvalidConfig, name, err)
		}
	case "stdio":
		if strings.TrimSpace(server.Command) == "" || strings.TrimSpace(server.URL) != "" {
			return fmt.Errorf("%w: server %q requires command and cannot set url", ErrInvalidConfig, name)
		}
		if len(server.Command) > 16<<10 || strings.ContainsRune(server.Command, 0) {
			return fmt.Errorf("%w: server %q has an invalid command", ErrInvalidConfig, name)
		}
	default:
		return fmt.Errorf("%w: server %q has unsupported type %q", ErrInvalidConfig, name, server.Type)
	}
	if server.Auth != "" && server.Auth != "oauth" {
		return fmt.Errorf("%w: server %q has unsupported authentication", ErrInvalidConfig, name)
	}
	if server.Auth == "oauth" {
		if serverType == "stdio" {
			return fmt.Errorf("%w: server %q cannot use OAuth with stdio", ErrInvalidConfig, name)
		}
		for key := range server.Headers {
			if strings.EqualFold(key, "Authorization") {
				return fmt.Errorf("%w: server %q cannot combine OAuth and an Authorization header", ErrInvalidConfig, name)
			}
		}
	}
	if len(server.AllowedTools) > 0 && len(server.DisabledTools) > 0 {
		return fmt.Errorf("%w: server %q cannot set both allowedTools and disabledTools", ErrInvalidConfig, name)
	}
	if len(server.Args) > 256 || len(server.Env) > 256 || len(server.Headers) > 128 || len(server.AllowedTools) > 512 || len(server.DisabledTools) > 512 {
		return fmt.Errorf("%w: server %q exceeds configuration limits", ErrInvalidConfig, name)
	}
	for key, value := range server.Headers {
		if !validHeader(key, value) {
			return fmt.Errorf("%w: server %q has an invalid header", ErrInvalidConfig, name)
		}
	}
	for key, value := range server.Env {
		if key == "" || len(key) > 1024 || len(value) > 64<<10 || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%w: server %q has an invalid environment entry", ErrInvalidConfig, name)
		}
	}
	for _, argument := range server.Args {
		if len(argument) > 64<<10 || strings.ContainsRune(argument, 0) {
			return fmt.Errorf("%w: server %q has an invalid argument", ErrInvalidConfig, name)
		}
	}
	for _, pattern := range append(append([]string(nil), server.AllowedTools...), server.DisabledTools...) {
		if pattern == "" || len(pattern) > 1024 {
			return fmt.Errorf("%w: server %q has an invalid tool pattern", ErrInvalidConfig, name)
		}
		if _, err := pathMatch(pattern, "tool"); err != nil {
			return fmt.Errorf("%w: server %q has an invalid tool pattern", ErrInvalidConfig, name)
		}
	}
	return nil
}

func validHeader(name, value string) bool {
	return len(name) <= 256 && len(value) <= 16<<10 && httpguts.ValidHeaderFieldName(name) && httpguts.ValidHeaderFieldValue(value)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: trailing JSON value", ErrInvalidConfig)
		}
		return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return nil
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") && home != "" {
		return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(path, "~/")))
	}
	return path
}

func processEnvironment() map[string]string {
	result := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			result[name] = value
		}
	}
	return result
}
