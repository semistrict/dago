package daconfig

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ServerPrefix namespaces the bounded child-process configuration payload.
const ServerPrefix = "DEEPAGENTS_CODE_SERVER_"

// ServerConfig is the credential-free configuration passed to a child server.
// Its zero value enables useful interactive defaults.
type ServerConfig struct {
	Model            string   `json:"model,omitempty"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	StateDirectory   string   `json:"state_directory,omitempty"`
	RecursionLimit   int      `json:"recursion_limit"`
	MemoryReadOnly   bool     `json:"memory_read_only,omitempty"`
	NonInteractive   bool     `json:"non_interactive,omitempty"`
	ShellAllowList   []string `json:"shell_allow_list,omitempty"`
}

// DefaultServerConfig returns the normalized useful zero-value behavior.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{RecursionLimit: 2000}
}

func (config ServerConfig) normalized() ServerConfig {
	if config.RecursionLimit == 0 {
		config.RecursionLimit = 2000
	}
	config.ShellAllowList = append([]string(nil), config.ShellAllowList...)
	return config
}

// Environment serializes the non-credential server payload. Static invalid
// values panic; the returned map is safe to merge into a child process env.
func (config ServerConfig) Environment() map[string]string {
	config = config.normalized()
	if err := config.validate(); err != nil {
		panic(err)
	}
	result := map[string]string{
		ServerPrefix + "RECURSION_LIMIT":  strconv.Itoa(config.RecursionLimit),
		ServerPrefix + "MEMORY_AUTO_SAVE": strconv.FormatBool(!config.MemoryReadOnly),
		ServerPrefix + "INTERACTIVE":      strconv.FormatBool(!config.NonInteractive),
	}
	if config.Model != "" {
		result[ServerPrefix+"MODEL"] = config.Model
	}
	if config.WorkingDirectory != "" {
		result[ServerPrefix+"WORKING_DIRECTORY"] = config.WorkingDirectory
	}
	if config.StateDirectory != "" {
		result[ServerPrefix+"STATE_DIRECTORY"] = config.StateDirectory
	}
	if config.ShellAllowList != nil {
		payload, _ := json.Marshal(config.ShellAllowList)
		result[ServerPrefix+"SHELL_ALLOW_LIST"] = string(payload)
	}
	return result
}

// ServerConfigFromEnvironment parses an untrusted process environment and
// fails closed on malformed values or values outside finite bounds.
func ServerConfigFromEnvironment(lookup LookupEnv) (ServerConfig, error) {
	if lookup == nil {
		panic("daconfig: server environment lookup is nil")
	}
	config := DefaultServerConfig()
	read := func(suffix string) (string, bool) { return lookup(ServerPrefix + suffix) }
	if value, ok := read("MODEL"); ok {
		config.Model = value
	}
	if value, ok := read("WORKING_DIRECTORY"); ok {
		config.WorkingDirectory = value
	}
	if value, ok := read("STATE_DIRECTORY"); ok {
		config.StateDirectory = value
	}
	if value, ok := read("RECURSION_LIMIT"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return ServerConfig{}, fmt.Errorf("%w: %sRECURSION_LIMIT must be an integer", ErrInvalidConfig, ServerPrefix)
		}
		config.RecursionLimit = parsed
	}
	if value, ok := read("MEMORY_AUTO_SAVE"); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return ServerConfig{}, fmt.Errorf("%w: %sMEMORY_AUTO_SAVE must be true or false", ErrInvalidConfig, ServerPrefix)
		}
		config.MemoryReadOnly = !parsed
	}
	if value, ok := read("INTERACTIVE"); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return ServerConfig{}, fmt.Errorf("%w: %sINTERACTIVE must be true or false", ErrInvalidConfig, ServerPrefix)
		}
		config.NonInteractive = !parsed
	}
	if value, ok := read("SHELL_ALLOW_LIST"); ok {
		if len(value) > 64<<10 || json.Unmarshal([]byte(value), &config.ShellAllowList) != nil || config.ShellAllowList == nil {
			return ServerConfig{}, fmt.Errorf("%w: %sSHELL_ALLOW_LIST must be a JSON array", ErrInvalidConfig, ServerPrefix)
		}
	}
	if err := config.validate(); err != nil {
		return ServerConfig{}, err
	}
	return config.normalized(), nil
}

func (config ServerConfig) validate() error {
	if len(config.Model) > 512 || strings.ContainsAny(config.Model, "\x00\r\n") {
		return fmt.Errorf("%w: server model is invalid", ErrInvalidConfig)
	}
	for _, path := range []string{config.WorkingDirectory, config.StateDirectory} {
		if len(path) > 4096 || strings.ContainsRune(path, 0) || path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("%w: server paths must be bounded absolute paths", ErrInvalidConfig)
		}
	}
	if config.RecursionLimit < 1 || config.RecursionLimit > 100_000 {
		return fmt.Errorf("%w: server recursion limit must be between 1 and 100000", ErrInvalidConfig)
	}
	if (config.ShellAllowList != nil && len(config.ShellAllowList) == 0) || len(config.ShellAllowList) > 128 {
		return fmt.Errorf("%w: server shell allow-list must contain 1-128 entries when set", ErrInvalidConfig)
	}
	for _, command := range config.ShellAllowList {
		if command == "" || len(command) > 256 || command != strings.TrimSpace(command) || strings.ContainsAny(command, "\x00\r\n") {
			return fmt.Errorf("%w: server shell allow-list contains an invalid command", ErrInvalidConfig)
		}
	}
	return nil
}
