package dahook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxConfigBytes = 4 << 20

// Scope fixes decision precedence. Lower values have higher authority.
type Scope int

const (
	ProjectScope Scope = iota
	UserScope
	PluginScope
)

// Handler describes one trusted command hook. Argv uses direct execution;
// Command uses the platform shell for compatibility with hooks.json.
type Handler struct {
	ID            string
	Event         Event
	Matcher       string
	Command       string
	Argv          []string
	Timeout       time.Duration
	StatusMessage string
	Scope         Scope
	Environment   map[string]string
	LegacyEvent   string
	order         int
}

type commandSpec struct {
	Type          string   `json:"type"`
	Command       string   `json:"command"`
	Argv          []string `json:"argv"`
	Timeout       float64  `json:"timeout"`
	StatusMessage string   `json:"statusMessage"`
	Async         bool     `json:"async"`
}

type groupSpec struct {
	Matcher string        `json:"matcher"`
	Hooks   []commandSpec `json:"hooks"`
}

type configDocument struct {
	Hooks json.RawMessage `json:"hooks"`
}

// Plugin contributes an enabled plugin hooks.json document. Environment must
// contain only the plugin/project variables the host intends to expose.
type Plugin struct {
	ID          string
	Path        string
	Inline      json.RawMessage
	Enabled     bool
	Environment map[string]string
}

// LoadOptions controls config sources. TrustProject must be explicit in
// headless programs; persisted trust is never consulted when Headless is true.
type LoadOptions struct {
	UserConfigDir string
	TrustStore    string
	TrustProject  bool
	Headless      bool
	Plugins       []Plugin
}

// Snapshot is an immutable, precedence-ordered hook configuration.
type Snapshot struct {
	ID          string
	Handlers    map[Event][]Handler
	Diagnostics []Diagnostic
}

// Load reads project, user, and enabled plugin config in decision precedence.
// projectRoot is positional because workspace identity is security-critical.
func Load(ctx context.Context, projectRoot string, options LoadOptions) (Snapshot, error) {
	if projectRoot == "" {
		panic("dahook: project root is required")
	}
	root, err := canonicalProject(projectRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve project root: %w", err)
	}
	userDir := options.UserConfigDir
	if userDir == "" {
		userDir, err = os.UserHomeDir()
		if err != nil {
			return Snapshot{}, fmt.Errorf("resolve user home: %w", err)
		}
		userDir = filepath.Join(userDir, ".deepagents")
	}
	trustStore := options.TrustStore
	if trustStore == "" {
		trustStore = filepath.Join(userDir, ".state", "hooks_trust.json")
	}
	trusted := options.TrustProject
	if !options.Headless && !trusted {
		trusted, err = IsTrusted(ctx, trustStore, root)
		if err != nil {
			return Snapshot{}, err
		}
	}
	type source struct {
		path   string
		inline json.RawMessage
		scope  Scope
		env    map[string]string
		plugin bool
	}
	var sources []source
	if trusted {
		sources = append(sources, source{path: filepath.Join(root, ".deepagents", "hooks.json"), scope: ProjectScope})
	}
	sources = append(sources, source{path: filepath.Join(userDir, "hooks.json"), scope: UserScope})
	for _, plugin := range options.Plugins {
		if plugin.Enabled {
			if (plugin.Path == "") == (len(plugin.Inline) == 0) {
				return Snapshot{}, errors.New("plugin hook source requires exactly one path or inline document")
			}
			sources = append(sources, source{path: plugin.Path, inline: append(json.RawMessage(nil), plugin.Inline...), scope: PluginScope, env: pluginEnvironment(plugin.Environment), plugin: true})
		}
	}
	result := Snapshot{Handlers: map[Event][]Handler{}}
	hash := sha256.New()
	order := 0
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		raw := source.inline
		if len(raw) == 0 {
			raw, err = readBounded(source.path, maxConfigBytes)
			if errors.Is(err, os.ErrNotExist) {
				if source.plugin {
					result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "plugin_hooks_failed", Message: "plugin hook configuration was skipped"})
				}
				continue
			}
			if err != nil {
				if source.plugin {
					result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "plugin_hooks_failed", Message: "plugin hook configuration was skipped"})
					continue
				}
				return Snapshot{}, fmt.Errorf("read hooks config %q: %w", source.path, err)
			}
		} else if len(raw) > maxConfigBytes {
			if source.plugin {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "plugin_hooks_failed", Message: "plugin hook configuration was skipped"})
				continue
			}
			return Snapshot{}, fmt.Errorf("read inline hooks config: document exceeds %d bytes", maxConfigBytes)
		}
		_, _ = fmt.Fprintf(hash, "%d\x00%s\x00%d\x00", source.scope, source.path, len(raw))
		_, _ = hash.Write(raw)
		for _, key := range sortedStringKeys(source.env) {
			_, _ = fmt.Fprintf(hash, "\x00%s=%s", key, source.env[key])
		}
		handlers, diagnostics, err := parseDocument(raw, source.scope, source.env, &order)
		if err != nil {
			if source.plugin {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "plugin_hooks_failed", Message: "plugin hook configuration was skipped"})
				continue
			}
			return Snapshot{}, fmt.Errorf("parse hooks config %q: %w", source.path, err)
		}
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		for _, handler := range handlers {
			result.Handlers[handler.Event] = append(result.Handlers[handler.Event], handler)
		}
	}
	result.ID = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

func sortedStringKeys(input map[string]string) []string {
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func canonicalProject(projectRoot string) (string, error) {
	absolute, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func pluginEnvironment(input map[string]string) map[string]string {
	allowed := map[string]bool{"CLAUDE_PLUGIN_ROOT": true, "PLUGIN_ROOT": true, "CLAUDE_PLUGIN_DATA": true, "PLUGIN_DATA": true, "CLAUDE_PROJECT_DIR": true}
	result := map[string]string{}
	for key, value := range input {
		if allowed[key] {
			result[key] = value
		}
	}
	return result
}

func readBounded(path string, limit int64) ([]byte, error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.Mode().IsRegular() {
		return nil, fmt.Errorf("config is not a regular unlinked file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(entry, info) || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("config is not a regular file within %d bytes", limit)
	}
	value := make([]byte, info.Size())
	_, err = io.ReadFull(file, value)
	return value, err
}

func parseDocument(raw []byte, scope Scope, env map[string]string, order *int) ([]Handler, []Diagnostic, error) {
	var document configDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, nil, err
	}
	var events map[Event][]groupSpec
	if err := json.Unmarshal(document.Hooks, &events); err != nil {
		return migrateLegacy(document.Hooks, scope, env, order)
	}
	var result []Handler
	var diagnostics []Diagnostic
	for event, groups := range events {
		if _, ok := allEvents[event]; !ok {
			return nil, nil, fmt.Errorf("unknown event %q", event)
		}
		for groupIndex, group := range groups {
			matcherSupported := event == PreToolUse || event == PostToolUse || event == PostToolUseFailure || event == PermissionRequest || event == SessionStart || event == SessionEnd || event == Notification || event == PreCompact || event == SubagentStart || event == SubagentStop
			if !matcherSupported && group.Matcher != "" && group.Matcher != "*" {
				diagnostics = append(diagnostics, Diagnostic{Code: "unsupported_matcher", Message: fmt.Sprintf("%s does not support matchers", event)})
				continue
			}
			if _, err := matches(group.Matcher, ""); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Code: "invalid_matcher", Message: "hook matcher is invalid"})
				continue
			}
			for handlerIndex, spec := range group.Hooks {
				if spec.Async || spec.Type != "command" || (strings.TrimSpace(spec.Command) == "" && len(spec.Argv) == 0) {
					return nil, nil, fmt.Errorf("invalid command handler for %s", event)
				}
				if len(spec.Argv) > 0 && strings.TrimSpace(spec.Argv[0]) == "" {
					return nil, nil, fmt.Errorf("empty argv executable for %s", event)
				}
				timeout := time.Duration(spec.Timeout * float64(time.Second))
				if spec.Timeout < 0 || spec.Timeout > float64(math.MaxInt64)/float64(time.Second) {
					return nil, nil, fmt.Errorf("negative timeout for %s", event)
				}
				result = append(result, Handler{ID: fmt.Sprintf("%s:%d:%d", event, groupIndex, handlerIndex), Event: event, Matcher: group.Matcher, Command: spec.Command, Argv: expandPluginArgs(spec.Argv, env), Timeout: timeout, StatusMessage: spec.StatusMessage, Scope: scope, Environment: cloneStringMap(env), order: *order})
				*order++
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].order < result[j].order })
	return result, diagnostics, nil
}

type legacySpec struct {
	Command []string `json:"command"`
	Events  []string `json:"events"`
}

func migrateLegacy(raw json.RawMessage, scope Scope, env map[string]string, order *int) ([]Handler, []Diagnostic, error) {
	var legacy []legacySpec
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, nil, err
	}
	mapping := map[string]struct {
		event   Event
		matcher string
	}{
		"session.start": {UserPromptSubmit, ""}, "user.prompt": {UserPromptSubmit, ""},
		"task.complete": {Notification, "agent_completed"}, "session.end": {SessionEnd, ""},
		"context.offload": {PreCompact, "manual"}, "context.compact": {PreCompact, "manual"},
		"input.required": {Notification, "agent_needs_input"},
	}
	var result []Handler
	for index, item := range legacy {
		if len(item.Command) == 0 {
			continue
		}
		events := item.Events
		if len(events) == 0 {
			events = []string{"session.start", "user.prompt", "task.complete", "session.end", "context.offload", "context.compact", "input.required"}
		}
		seen := map[string]bool{}
		for _, name := range events {
			mapped, ok := mapping[name]
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			result = append(result, Handler{ID: fmt.Sprintf("legacy:%d:%s", index, name), Event: mapped.event, Matcher: mapped.matcher, Argv: expandPluginArgs(item.Command, env), Timeout: 6 * time.Second, Scope: scope, Environment: cloneStringMap(env), LegacyEvent: name, order: *order})
			*order++
		}
	}
	return result, []Diagnostic{{Code: "legacy_config", Message: "migrated compatible list-shaped hooks.json entries"}}, nil
}

func expandPlugin(value string, env map[string]string) string {
	for key, replacement := range env {
		value = strings.ReplaceAll(value, "${"+key+"}", replacement)
	}
	return value
}
func expandPluginArgs(values []string, env map[string]string) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = expandPlugin(value, env)
	}
	return result
}
func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := map[string]string{}
	for key, value := range input {
		result[key] = value
	}
	return result
}

func matcherTarget(invocation Invocation) string {
	switch invocation.Event {
	case PreToolUse, PostToolUse, PostToolUseFailure, PermissionRequest:
		value, _ := invocation.Data["tool_name"].(string)
		return value
	case SessionStart, SessionEnd:
		value, _ := invocation.Data["source"].(string)
		if value == "" {
			value, _ = invocation.Data["reason"].(string)
		}
		return value
	case Notification:
		value, _ := invocation.Data["notification_type"].(string)
		return value
	case PreCompact:
		value, _ := invocation.Data["trigger"].(string)
		return value
	case SubagentStart, SubagentStop:
		value, _ := invocation.Data["agent_name"].(string)
		return value
	default:
		return ""
	}
}

func matches(pattern, target string) (bool, error) {
	if pattern == "" || pattern == "*" {
		return true, nil
	}
	if regexp.MustCompile(`^[\w\s,|\-]+$`).MatchString(pattern) {
		for _, part := range regexp.MustCompile(`[|,]`).Split(pattern, -1) {
			if strings.TrimSpace(part) == target {
				return true, nil
			}
		}
		return false, nil
	}
	return regexp.MatchString(pattern, target)
}
