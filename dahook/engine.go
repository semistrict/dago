package dahook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	defaultTimeout     = 10 * time.Minute
	promptTimeout      = 30 * time.Second
	defaultOutputLimit = 100_000
	processWaitDelay   = 250 * time.Millisecond
)

// EngineOptions bounds command execution. Zero values select useful defaults.
type EngineOptions struct {
	DefaultTimeout time.Duration
	MaxOutputBytes int
	Environment    map[string]string
	OnProgress     func(Progress)
}

// Progress describes one hook handler entering or leaving execution. Callbacks
// may be concurrent and must return promptly; panics are isolated from hook
// execution. Message is bounded to one control-free display line.
type Progress struct {
	OperationID string
	HandlerID   string
	Event       Event
	Active      bool
	Message     string
}

// Engine executes an immutable Snapshot.
type Engine struct {
	snapshot Snapshot
	options  EngineOptions
	sequence atomic.Uint64
}

// NewEngine constructs an engine. An invalid snapshot is static configuration
// and panics; command failures are reported at Run time.
func NewEngine(snapshot Snapshot, options EngineOptions) *Engine {
	if snapshot.Handlers == nil {
		panic("dahook: snapshot handlers are required")
	}
	if options.DefaultTimeout < 0 || options.MaxOutputBytes < 0 {
		panic("dahook: engine bounds cannot be negative")
	}
	if options.DefaultTimeout == 0 {
		options.DefaultTimeout = defaultTimeout
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = defaultOutputLimit
	}
	frozen := Snapshot{ID: snapshot.ID, Handlers: make(map[Event][]Handler, len(snapshot.Handlers)), Diagnostics: append([]Diagnostic(nil), snapshot.Diagnostics...)}
	for event, handlers := range snapshot.Handlers {
		if _, ok := allEvents[event]; !ok {
			panic("dahook: snapshot contains an invalid event")
		}
		copied := append([]Handler(nil), handlers...)
		for index := range copied {
			if copied[index].Event != event || copied[index].ID == "" || (strings.TrimSpace(copied[index].Command) == "" && len(copied[index].Argv) == 0) || copied[index].Timeout < 0 {
				panic("dahook: snapshot contains an invalid handler")
			}
			if _, err := matches(copied[index].Matcher, ""); err != nil {
				panic("dahook: snapshot contains an invalid matcher")
			}
			copied[index].Argv = append([]string(nil), copied[index].Argv...)
			copied[index].Environment = cloneStringMap(copied[index].Environment)
		}
		frozen.Handlers[event] = copied
	}
	options.Environment = cloneStringMap(options.Environment)
	return &Engine{snapshot: frozen, options: options}
}

// SnapshotID returns the stable configuration identity.
func (engine *Engine) SnapshotID() string { return engine.snapshot.ID }

type handlerResult struct {
	handler     Handler
	output      wireOutput
	plain       string
	diagnostics []Diagnostic
}

type wireOutput struct {
	Continue                 *bool              `json:"continue"`
	StopReason               string             `json:"stopReason"`
	SuppressOutput           bool               `json:"suppressOutput"`
	SystemMessage            string             `json:"systemMessage"`
	Decision                 string             `json:"decision"`
	Reason                   string             `json:"reason"`
	PermissionDecision       PermissionDecision `json:"permissionDecision"`
	PermissionDecisionReason string             `json:"permissionDecisionReason"`
	AdditionalContext        string             `json:"additionalContext"`
	HookSpecificOutput       map[string]any     `json:"hookSpecificOutput"`
}

// Run executes every matching handler concurrently, then reduces results in
// project/user/plugin declaration order.
func (engine *Engine) Run(ctx context.Context, invocation Invocation) (Decision, error) {
	if err := invocation.validate(); err != nil {
		return Decision{}, err
	}
	payload, err := json.Marshal(invocation)
	if err != nil {
		return Decision{}, fmt.Errorf("encode hook invocation: %w", err)
	}
	target := matcherTarget(invocation)
	var handlers []Handler
	for _, handler := range engine.snapshot.Handlers[invocation.Event] {
		matched, matchErr := matches(handler.Matcher, target)
		if matchErr != nil {
			continue
		}
		if matched {
			handlers = append(handlers, handler)
		}
	}
	results := make([]handlerResult, len(handlers))
	var wait sync.WaitGroup
	for index, handler := range handlers {
		index, handler := index, handler
		wait.Add(1)
		go func() {
			defer wait.Done()
			operationID := fmt.Sprintf("%d:%s", engine.sequence.Add(1), handler.ID)
			engine.reportProgress(Progress{OperationID: operationID, HandlerID: handler.ID, Event: invocation.Event, Active: true, Message: normalizeStatusMessage(handler.StatusMessage)})
			defer engine.reportProgress(Progress{OperationID: operationID, HandlerID: handler.ID, Event: invocation.Event, Message: normalizeStatusMessage(handler.StatusMessage)})
			results[index] = engine.runHandler(ctx, invocation.Event, invocation.CWD, handler, payload)
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].handler.order < results[j].handler.order })
	decision := Decision{Continue: true, Diagnostics: append([]Diagnostic(nil), engine.snapshot.Diagnostics...)}
	for _, result := range results {
		reduceResult(invocation, &decision, result)
	}
	return decision, nil
}

func (engine *Engine) reportProgress(progress Progress) {
	if engine.options.OnProgress == nil {
		return
	}
	defer func() { _ = recover() }()
	engine.options.OnProgress(progress)
}

func normalizeStatusMessage(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	fields := strings.Fields(value)
	value = strings.Join(fields, " ")
	runes := []rune(value)
	if len(runes) > 256 {
		value = string(runes[:255]) + "…"
	}
	return value
}

func (engine *Engine) runHandler(ctx context.Context, event Event, cwd string, handler Handler, payload []byte) handlerResult {
	result := handlerResult{handler: handler}
	if handler.LegacyEvent != "" {
		var envelope map[string]any
		_ = json.Unmarshal(payload, &envelope)
		legacy := map[string]any{"event": handler.LegacyEvent}
		if handler.LegacyEvent == "session.start" || handler.LegacyEvent == "task.complete" || handler.LegacyEvent == "session.end" {
			legacy["thread_id"] = envelope["session_id"]
		}
		payload, _ = json.Marshal(legacy)
	}
	timeout := handler.Timeout
	if timeout <= 0 {
		timeout = engine.options.DefaultTimeout
		if event == UserPromptSubmit {
			timeout = promptTimeout
		}
	}
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var command *exec.Cmd
	if len(handler.Argv) > 0 {
		command = exec.CommandContext(runContext, handler.Argv[0], handler.Argv[1:]...)
	} else if runtime.GOOS == "windows" {
		command = exec.CommandContext(runContext, "cmd.exe", "/C", handler.Command)
	} else {
		command = exec.CommandContext(runContext, "/bin/sh", "-c", handler.Command)
	}
	command.Dir = cwd
	command.Env = sanitizedEnvironment(engine.options.Environment, handler.Environment)
	command.Stdin = bytes.NewReader(payload)
	stdout := &boundedBuffer{limit: engine.options.MaxOutputBytes}
	stderr := &boundedBuffer{limit: engine.options.MaxOutputBytes}
	command.Stdout, command.Stderr = stdout, stderr
	process, err := configureProcess(command)
	if err != nil {
		result.diagnostics = append(result.diagnostics, diagnostic(handler.ID, "process_setup_failed", "hook process isolation failed"))
		return result
	}
	defer process.close()
	if err = command.Start(); err == nil {
		err = process.started(command)
		if err == nil {
			err = command.Wait()
		} else {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		result.diagnostics = append(result.diagnostics, diagnostic(handler.ID, "timeout", "hook command exceeded its timeout"))
		return result
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 2 {
			result.output.Decision = "block"
			result.output.Reason = strings.TrimSpace(stderr.String())
			if result.output.Reason == "" {
				result.output.Reason = "Hook blocked the operation"
			}
			return result
		}
		result.diagnostics = append(result.diagnostics, diagnostic(handler.ID, "command_failed", "hook command failed"))
		return result
	}
	if stdout.truncated {
		result.diagnostics = append(result.diagnostics, diagnostic(handler.ID, "stdout_truncated", "hook stdout exceeded configured limit"))
	}
	if stderr.truncated {
		result.diagnostics = append(result.diagnostics, diagnostic(handler.ID, "stderr_truncated", "hook stderr exceeded configured limit"))
	}
	plain := strings.TrimSpace(stdout.String())
	if plain == "" {
		return result
	}
	if err := json.Unmarshal(stdout.Bytes(), &result.output); err != nil {
		result.plain = plain
	}
	return result
}

func reduceResult(invocation Invocation, decision *Decision, result handlerResult) {
	event := invocation.Event
	decision.Diagnostics = append(decision.Diagnostics, result.diagnostics...)
	if result.plain != "" {
		if event == SessionStart || event == UserPromptSubmit {
			decision.AdditionalContext = append(decision.AdditionalContext, result.plain)
		} else {
			decision.Diagnostics = append(decision.Diagnostics, diagnostic(result.handler.ID, "malformed_json", "hook output is not valid JSON"))
		}
	}
	output := result.output
	if output.Continue != nil && !*output.Continue {
		decision.Continue = false
		if decision.StopReason == "" {
			decision.StopReason = output.StopReason
		}
	}
	if output.SystemMessage != "" && !output.SuppressOutput {
		decision.SystemMessages = append(decision.SystemMessages, output.SystemMessage)
	}
	additional := output.AdditionalContext
	if specific := output.HookSpecificOutput; specific != nil {
		if named, ok := specific["hookEventName"].(string); ok && named != string(event) {
			decision.Diagnostics = append(decision.Diagnostics, diagnostic(result.handler.ID, "mismatched_output", "hook-specific output does not match the invoked event"))
			return
		}
		if value, ok := specific["additionalContext"].(string); ok {
			additional = value
		}
		if value, ok := specific["permissionDecision"].(string); ok {
			output.PermissionDecision = PermissionDecision(value)
		}
		if value, ok := specific["permissionDecisionReason"].(string); ok {
			output.PermissionDecisionReason = value
		}
		if value, ok := specific["suppressOriginalPrompt"].(bool); ok {
			decision.SuppressOriginalPrompt = decision.SuppressOriginalPrompt || value
		}
		if event == PermissionRequest {
			if permission, ok := specific["decision"].(map[string]any); ok {
				if behavior, _ := permission["behavior"].(string); behavior == "allow" {
					output.PermissionDecision = PermissionAllow
				} else if behavior == "deny" {
					output.PermissionDecision = PermissionDeny
					output.PermissionDecisionReason, _ = permission["message"].(string)
				}
			}
		}
	}
	if additional != "" {
		if event != Stop || stopContinuationAllowed(invocation) {
			decision.AdditionalContext = append(decision.AdditionalContext, additional)
		}
		if event == Stop && stopContinuationAllowed(invocation) {
			decision.ContinueLoop = true
		} else if event == Stop {
			decision.Diagnostics = append(decision.Diagnostics, diagnostic(result.handler.ID, "continuation_cap", "ignored Stop continuation after 8 attempts"))
		}
	}
	if output.PermissionDecision != PermissionNone && !validPermission(output.PermissionDecision) {
		decision.Diagnostics = append(decision.Diagnostics, diagnostic(result.handler.ID, "invalid_permission", "hook returned an invalid permission decision"))
	} else if output.PermissionDecision != PermissionNone && permissionRank(output.PermissionDecision) > permissionRank(decision.Permission) {
		decision.Permission = output.PermissionDecision
		decision.PermissionReason = output.PermissionDecisionReason
	}
	if output.Decision == "block" {
		reason := output.Reason
		if reason == "" {
			reason = "Blocked by hook"
		}
		switch event {
		case PreToolUse, PermissionRequest:
			decision.Permission, decision.PermissionReason = PermissionDeny, reason
		case Stop:
			if !stopContinuationAllowed(invocation) {
				decision.Diagnostics = append(decision.Diagnostics, diagnostic(result.handler.ID, "continuation_cap", "ignored Stop continuation after 8 attempts"))
			} else {
				decision.ContinueLoop = true
				decision.AdditionalContext = append(decision.AdditionalContext, reason)
			}
		case PostToolUse, PostToolUseFailure:
			decision.AdditionalContext = append(decision.AdditionalContext, reason)
		case UserPromptSubmit:
			decision.Continue = false
			if decision.StopReason == "" {
				decision.StopReason = reason
			}
		case SubagentStop:
			decision.AdditionalContext = append(decision.AdditionalContext, reason)
		case SessionStart, SessionEnd, Notification:
			decision.Diagnostics = append(decision.Diagnostics, diagnostic(result.handler.ID, "unsupported_block", "blocking is not supported for this event"))
		}
	}
}

func permissionRank(value PermissionDecision) int {
	switch value {
	case PermissionDeny:
		return 3
	case PermissionAsk:
		return 2
	case PermissionAllow:
		return 1
	default:
		return 0
	}
}

func validPermission(value PermissionDecision) bool {
	return value == PermissionAllow || value == PermissionDeny || value == PermissionAsk || value == PermissionDefer
}

func stopContinuationAllowed(invocation Invocation) bool {
	value, ok := invocation.Data["continuation_count"]
	if !ok {
		return true
	}
	switch typed := value.(type) {
	case int:
		return typed < 8
	case float64:
		return typed < 8
	default:
		return false
	}
}
func diagnostic(id, code, message string) Diagnostic {
	return Diagnostic{HandlerID: id, Code: code, Message: message}
}

type boundedBuffer struct {
	value     bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := buffer.limit - buffer.value.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = buffer.value.Write(value)
	}
	if original > remaining {
		buffer.truncated = true
	}
	return original, nil
}
func (buffer *boundedBuffer) Bytes() []byte  { return buffer.value.Bytes() }
func (buffer *boundedBuffer) String() string { return buffer.value.String() }

func sanitizedEnvironment(base, overlay map[string]string) []string {
	values := base
	if values == nil {
		values = map[string]string{}
		for _, item := range os.Environ() {
			key, value, ok := strings.Cut(item, "=")
			if ok {
				values[key] = value
			}
		}
	}
	result := map[string]string{}
	for key, value := range values {
		if !secretEnvironmentName(key) {
			result[key] = value
		}
	}
	allowed := map[string]bool{"CLAUDE_PLUGIN_ROOT": true, "PLUGIN_ROOT": true, "CLAUDE_PLUGIN_DATA": true, "PLUGIN_DATA": true, "CLAUDE_PROJECT_DIR": true}
	for key, value := range overlay {
		if allowed[key] {
			result[key] = value
		}
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+result[key])
	}
	return environment
}

func secretEnvironmentName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

var _ io.Writer = (*boundedBuffer)(nil)
