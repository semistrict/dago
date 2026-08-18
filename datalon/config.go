// Package datalon provides an experimental local host for long-running agents.
package datalon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAssistantID     = "default"
	defaultRecursion       = 500
	defaultMaxMessage      = 1 << 20
	defaultSendTimeout     = 30 * time.Second
	defaultStopTimeout     = 10 * time.Second
	defaultApprovalTimeout = 5 * time.Minute
	defaultApprovalActions = 32
	defaultApprovalPrompt  = 16 << 10
	assistantIDEnv         = "DEEPAGENTS_TALON_ASSISTANT_ID"
	legacyAssistantIDEnv   = "AGENT_ASSISTANT_ID"
	homeEnv                = "DEEPAGENTS_TALON_HOME"
	workspaceEnv           = "DEEPAGENTS_TALON_WORKSPACE"
	recursionEnv           = "DEEPAGENTS_TALON_RECURSION_LIMIT"
)

var assistantIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

var (
	ErrInvalidConfig   = errors.New("invalid long-running host configuration")
	ErrInvalidMessage  = errors.New("invalid channel message")
	ErrHostNotRunning  = errors.New("long-running host is not running")
	ErrMessageTooLarge = errors.New("channel message exceeds the configured limit")
	ErrSendFailed      = errors.New("channel send failed")
)

// Config controls one assistant host. Its zero value selects useful, bounded
// defaults: assistant ID "default", state below ~/.deepagents/default, the
// current working directory as workspace, recursion limit 500, 1 MiB inbound
// messages, a 30-second send deadline, a 10-second shutdown deadline, and
// approval prompts bounded to 32 actions, 16 KiB, and five minutes.
type Config struct {
	AssistantID            string
	StateRoot              string
	Workspace              string
	RecursionLimit         int
	MaxMessageBytes        int
	SendTimeout            time.Duration
	StopTimeout            time.Duration
	ApprovalTimeout        time.Duration
	MaxApprovalActions     int
	MaxApprovalPromptBytes int
}

// DefaultConfig returns the normalized environment-independent defaults. Home
// and working-directory lookup remain deferred until the host starts.
func DefaultConfig() Config {
	return Config{
		AssistantID: defaultAssistantID, RecursionLimit: defaultRecursion,
		MaxMessageBytes: defaultMaxMessage, SendTimeout: defaultSendTimeout,
		StopTimeout: defaultStopTimeout, ApprovalTimeout: defaultApprovalTimeout,
		MaxApprovalActions: defaultApprovalActions, MaxApprovalPromptBytes: defaultApprovalPrompt,
	}
}

// ConfigFromEnv parses supported host environment values. External values can
// be malformed, so unlike static constructors this loader returns an error.
// A nil map reads the process environment.
func ConfigFromEnv(env map[string]string) (Config, error) {
	if env == nil {
		env = processEnvironment()
	}
	config := DefaultConfig()
	config.AssistantID = firstNonEmpty(env[assistantIDEnv], env[legacyAssistantIDEnv], defaultAssistantID)
	config.StateRoot = strings.TrimSpace(env[homeEnv])
	config.Workspace = strings.TrimSpace(env[workspaceEnv])
	if raw := strings.TrimSpace(env[recursionEnv]); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return Config{}, fmt.Errorf("%w: recursion limit must be a positive integer", ErrInvalidConfig)
		}
		config.RecursionLimit = value
	}
	if err := validateAssistantID(config.AssistantID); err != nil {
		return Config{}, err
	}
	return config, nil
}

// StateDir returns the per-assistant state directory. It does not create it.
func (config Config) StateDir() string {
	config = config.withDefaults()
	if validateAssistantID(config.AssistantID) != nil {
		return ""
	}
	root := config.StateRoot
	if root == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			root = filepath.Join(home, ".deepagents")
		}
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, config.AssistantID)
}

func (config Config) withDefaults() Config {
	defaults := DefaultConfig()
	if config.AssistantID == "" {
		config.AssistantID = defaults.AssistantID
	}
	if config.RecursionLimit <= 0 {
		config.RecursionLimit = defaults.RecursionLimit
	}
	if config.MaxMessageBytes <= 0 {
		config.MaxMessageBytes = defaults.MaxMessageBytes
	}
	if config.SendTimeout <= 0 {
		config.SendTimeout = defaults.SendTimeout
	}
	if config.StopTimeout <= 0 {
		config.StopTimeout = defaults.StopTimeout
	}
	if config.ApprovalTimeout <= 0 {
		config.ApprovalTimeout = defaults.ApprovalTimeout
	}
	if config.MaxApprovalActions <= 0 {
		config.MaxApprovalActions = defaults.MaxApprovalActions
	}
	if config.MaxApprovalPromptBytes <= 0 {
		config.MaxApprovalPromptBytes = defaults.MaxApprovalPromptBytes
	}
	return config
}

func (config Config) validateStaticBounds() {
	switch {
	case config.RecursionLimit < 0:
		panic("datalon: negative recursion limit")
	case config.MaxMessageBytes < 0:
		panic("datalon: negative maximum message size")
	case config.SendTimeout < 0:
		panic("datalon: negative send timeout")
	case config.StopTimeout < 0:
		panic("datalon: negative stop timeout")
	case config.ApprovalTimeout < 0:
		panic("datalon: negative approval timeout")
	case config.MaxApprovalActions < 0:
		panic("datalon: negative maximum approval actions")
	case config.MaxApprovalPromptBytes < 0:
		panic("datalon: negative maximum approval prompt size")
	}
}

func (config Config) prepare() (Config, error) {
	config = config.withDefaults()
	if err := validateAssistantID(config.AssistantID); err != nil {
		return Config{}, err
	}
	stateDir := config.StateDir()
	if stateDir == "" {
		return Config{}, fmt.Errorf("%w: user home is unavailable and state root is empty", ErrInvalidConfig)
	}
	workspace := config.Workspace
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return Config{}, fmt.Errorf("%w: determine workspace: %v", ErrInvalidConfig, err)
		}
	}
	absoluteWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return Config{}, fmt.Errorf("%w: resolve workspace: %v", ErrInvalidConfig, err)
	}
	info, err := os.Stat(absoluteWorkspace)
	if err != nil {
		return Config{}, fmt.Errorf("%w: inspect workspace: %v", ErrInvalidConfig, err)
	}
	if !info.IsDir() {
		return Config{}, fmt.Errorf("%w: workspace is not a directory", ErrInvalidConfig)
	}
	config.Workspace = absoluteWorkspace
	if err := ensureStateDirectories(stateDir); err != nil {
		return Config{}, err
	}
	return config, nil
}

func ensureStateDirectories(stateDir string) error {
	paths := []string{
		stateDir,
		filepath.Join(stateDir, "agents"),
		filepath.Join(stateDir, "cron"),
		filepath.Join(stateDir, "channels"),
		filepath.Join(stateDir, "media", "inbound"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("prepare assistant state: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure assistant state: %w", err)
		}
	}
	return nil
}

func validateAssistantID(id string) error {
	if id == "." || id == ".." || !assistantIDPattern.MatchString(id) {
		return fmt.Errorf("%w: assistant ID must contain 1-128 letters, numbers, dots, underscores, or hyphens", ErrInvalidConfig)
	}
	return nil
}

func processEnvironment() map[string]string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
