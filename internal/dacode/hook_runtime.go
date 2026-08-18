package dacode

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dahook"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

const hookContextStateKey = "__dacode_hook_context"

type dacodeHookRuntime struct {
	engine     *dahook.Engine
	fulfiller  *dahook.Fulfiller
	capability dahook.Capability
	cwd        string

	mu       sync.Mutex
	sessions map[string]struct{}
}

type dacodeHookRuntimeOptions struct {
	Headless      bool
	UserConfigDir string
	TrustStore    string
	OnProgress    func(dahook.Progress)
}

func newDacodeHookRuntime(ctx context.Context, projectRoot string, plugins []dahook.Plugin, options dacodeHookRuntimeOptions) (*dacodeHookRuntime, error) {
	snapshot, err := dahook.Load(ctx, projectRoot, dahook.LoadOptions{
		Headless: options.Headless, UserConfigDir: options.UserConfigDir, TrustStore: options.TrustStore, Plugins: plugins,
	})
	if err != nil {
		return nil, fmt.Errorf("load lifecycle hooks: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("initialize lifecycle hook capability: %w", err)
	}
	capability := dahook.NewCapability(key)
	engine := dahook.NewEngine(snapshot, dahook.EngineOptions{OnProgress: options.OnProgress})
	return &dacodeHookRuntime{
		engine: engine, fulfiller: dahook.NewFulfiller(engine, capability, 0), capability: capability,
		cwd: projectRoot, sessions: map[string]struct{}{},
	}, nil
}

func (runtime *dacodeHookRuntime) Middleware() dagent.Middleware {
	if runtime == nil || runtime.engine == nil {
		panic("dacode: hook runtime is required")
	}
	return dagent.Middleware{
		Name: "lifecycle_hooks",
		Fields: map[string]dagent.StateField{
			hookContextStateKey: {Kind: dagent.FieldLast, Contract: "dacode.hooks.context.v1", Private: true, Clone: cloneHookContext},
		},
		BeforeAgent: runtime.beforeAgent,
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			contextValues, _ := request.State[hookContextStateKey].([]string)
			if len(contextValues) > 0 {
				fragment := strings.Join(contextValues, "\n\n")
				if request.SystemMessage == nil {
					message := damessage.System(fragment)
					request.SystemMessage = &message
				} else {
					message := request.SystemMessage.Clone()
					message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: fragment})
					request.SystemMessage = &message
				}
			}
			return next(ctx, request)
		},
		WrapToolCall: runtime.wrapToolCall,
		AfterAgent:   runtime.afterAgent,
	}
}

func (runtime *dacodeHookRuntime) beforeAgent(ctx context.Context, values dastate.Values, graphRuntime dagent.Runtime) (dastate.Values, error) {
	sessionID := hookSessionID(graphRuntime)
	if sessionID == "" {
		return nil, errors.New("lifecycle hooks require a session id")
	}
	var contexts []string
	if runtime.markSession(sessionID) {
		decision, err := runtime.run(ctx, dahook.Invocation{
			Event: dahook.SessionStart, SessionID: sessionID, CWD: runtime.cwd,
			Data: map[string]any{"source": "startup"},
		})
		if err != nil {
			return nil, err
		}
		if err := enforceHookDecision(decision); err != nil {
			return nil, err
		}
		contexts = appendHookContext(contexts, decision)
	}
	messages, err := decodeSessionMessages(values[dagent.MessagesKey])
	if err != nil {
		return nil, err
	}
	prompt := lastMessageText(messages, damessage.RoleHuman)
	if prompt != "" {
		decision, runErr := runtime.run(ctx, dahook.Invocation{
			Event: dahook.UserPromptSubmit, SessionID: sessionID, CWD: runtime.cwd,
			Data: map[string]any{"prompt": prompt},
		})
		if runErr != nil {
			return nil, runErr
		}
		if err := enforceHookDecision(decision); err != nil {
			return nil, err
		}
		contexts = appendHookContext(contexts, decision)
	}
	return dastate.Values{hookContextStateKey: contexts}, nil
}

func (runtime *dacodeHookRuntime) wrapToolCall(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
	sessionID := hookSessionID(request.Runtime)
	toolUse := buildHookToolUsePayload(request.Call)
	pre, err := runtime.run(ctx, dahook.Invocation{
		Event: dahook.PreToolUse, SessionID: sessionID, CWD: runtime.cwd,
		Data: map[string]any{
			"tool_name": toolUse.ToolName, "tool_input": toolUse.ToolArgs,
			"tool_id": toolUse.ToolID, "tool_args": toolUse.ToolArgs,
		},
	})
	if err != nil {
		return dagent.ToolCallResponse{}, propagatingHookError{err}
	}
	if err := enforceHookDecision(pre); err != nil {
		return dagent.ToolCallResponse{}, propagatingHookError{err}
	}
	response, callErr := next(ctx, request)
	toolResult := buildHookToolResultPayload(request.Call, response.Result, callErr)
	event := dahook.PostToolUse
	data := map[string]any{
		"tool_name": toolResult.ToolName, "tool_response": toolResult.ToolOutput,
		"tool_id": toolResult.ToolID, "tool_args": toolResult.ToolArgs,
		"tool_status": toolResult.ToolStatus, "tool_output": toolResult.ToolOutput,
	}
	if toolResult.ToolStatus == string(damessage.ToolStatusError) {
		event = dahook.PostToolUseFailure
		data["error"] = toolResult.ToolOutput
		data["tool_names"] = buildHookToolErrorPayload(toolResult.ToolName).ToolNames
	}
	post, hookErr := runtime.run(ctx, dahook.Invocation{Event: event, SessionID: sessionID, CWD: runtime.cwd, Data: data})
	if hookErr != nil {
		return dagent.ToolCallResponse{}, propagatingHookError{hookErr}
	}
	if err := enforceHookDecision(post); err != nil {
		return dagent.ToolCallResponse{}, propagatingHookError{err}
	}
	if callErr == nil {
		contexts, _ := request.State[hookContextStateKey].([]string)
		contexts = append([]string(nil), contexts...)
		contexts = appendHookContext(contexts, pre)
		contexts = appendHookContext(contexts, post)
		if len(contexts) > 0 {
			if response.Result.Update == nil {
				response.Result.Update = map[string]any{}
			}
			response.Result.Update[hookContextStateKey] = contexts
		}
	}
	return response, callErr
}

func (runtime *dacodeHookRuntime) afterAgent(ctx context.Context, values dastate.Values, graphRuntime dagent.Runtime) (dastate.Values, error) {
	messages, err := decodeSessionMessages(values[dagent.MessagesKey])
	if err != nil {
		return nil, err
	}
	decision, err := runtime.run(ctx, dahook.Invocation{
		Event: dahook.Stop, SessionID: hookSessionID(graphRuntime), CWD: runtime.cwd,
		Data: map[string]any{"last_assistant_message": lastMessageText(messages, damessage.RoleAssistant)},
	})
	if err != nil {
		return nil, err
	}
	return nil, enforceHookDecision(decision)
}

func (runtime *dacodeHookRuntime) Close() error {
	if runtime == nil || runtime.engine == nil {
		return nil
	}
	runtime.mu.Lock()
	sessions := make([]string, 0, len(runtime.sessions))
	for id := range runtime.sessions {
		sessions = append(sessions, id)
	}
	runtime.sessions = map[string]struct{}{}
	runtime.mu.Unlock()
	closeContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var result error
	for _, sessionID := range sessions {
		decision, err := runtime.run(closeContext, dahook.Invocation{
			Event: dahook.SessionEnd, SessionID: sessionID, CWD: runtime.cwd,
			Data: map[string]any{"reason": "other"},
		})
		if err == nil {
			err = enforceHookDecision(decision)
		}
		if err != nil && result == nil {
			result = err
		}
	}
	return result
}

// run preserves the lifecycle protocol ownership boundary even though the
// terminal host and hook client live in one process. Server-owned events are
// authenticated, replay-protected interrupts before the client engine can run
// any side effect; client-owned events execute directly on that client engine.
func (runtime *dacodeHookRuntime) run(ctx context.Context, invocation dahook.Invocation) (dahook.Decision, error) {
	if dahook.EventOwner(invocation.Event) == dahook.ClientOwner {
		return runtime.engine.Run(ctx, invocation)
	}
	deadline := time.Now().Add(10 * time.Minute)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	// A failed or cancelled client fulfillment has no authenticated response to
	// consume. Keep that abandoned pending request scoped to this exchange so a
	// long-running host cannot exhaust a session-wide server ledger with failed
	// hook commands; the fulfiller still retains bounded cross-request replay
	// protection before any client side effect.
	server := dahook.NewServer(runtime.engine.SnapshotID(), dahook.NewLedger(1), runtime.capability)
	interrupt, err := server.Interrupt(invocation.SessionID, invocation, deadline)
	if err != nil {
		return dahook.Decision{}, err
	}
	response, err := runtime.fulfiller.Fulfill(ctx, interrupt.Request)
	if err != nil {
		return dahook.Decision{}, err
	}
	return server.Resume(response)
}

func (runtime *dacodeHookRuntime) markSession(id string) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, exists := runtime.sessions[id]; exists {
		return false
	}
	runtime.sessions[id] = struct{}{}
	return true
}

func hookSessionID(runtime dagent.Runtime) string {
	if runtime.Config.ThreadID != "" {
		return runtime.Config.ThreadID
	}
	return runtime.TaskID
}

func lastMessageText(messages []damessage.Message, role damessage.Role) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == role {
			return messages[index].TextContent()
		}
	}
	return ""
}

func appendHookContext(current []string, decision dahook.Decision) []string {
	current = append(current, decision.AdditionalContext...)
	return append(current, decision.SystemMessages...)
}

func enforceHookDecision(decision dahook.Decision) error {
	if decision.Continue && decision.Permission != dahook.PermissionDeny {
		return nil
	}
	reason := strings.TrimSpace(decision.PermissionReason)
	if reason == "" {
		reason = strings.TrimSpace(decision.StopReason)
	}
	if reason == "" {
		reason = "lifecycle hook blocked the operation"
	}
	return errors.New(boundedHookText(reason, 4096))
}

func boundedHookText(value string, limit int) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) > limit {
		return value[:limit] + "..."
	}
	return value
}

func cloneHookContext(value any) any {
	values, _ := value.([]string)
	return append([]string(nil), values...)
}

type propagatingHookError struct{ error }

func (propagatingHookError) PropagateToolError() {}
