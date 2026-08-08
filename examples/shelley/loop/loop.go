package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	dagent "github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/checkpoint"
	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/gitstate"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
)

// maxTurnDuration is an absolute backstop on a single LLM request (including
// its inner transport retries). The primary bound on a stuck stream is the
// transport idle/stall timeout (llmhttp.DefaultIdleTimeout); this ceiling only
// exists to stop a provider that keeps the connection warm with
// heartbeats/keepalives (which reset the idle timer) from hanging a turn
// indefinitely. It is deliberately far larger than the idle window so that
// genuinely long, steadily-streaming turns are unaffected.
const maxTurnDuration = 15 * time.Minute

// MessageRecordFunc is called to record new messages to persistent storage.
// otherUsage carries the usage of indirect LLM calls affiliated with the
// message (e.g. LLM-backed tools for a tool-result message); nil for most
// messages.
type MessageRecordFunc func(ctx context.Context, message llm.Message, usage llm.Usage, otherUsage []llm.PurposedUsage) error

// WarningRecordFunc is called to record user-visible warnings that are not sent to the LLM.
type WarningRecordFunc func(ctx context.Context, text string) error

// GitStateChangeFunc is called when the git state changes at the end of a turn.
// This is used to record user-visible notifications about git changes.
type GitStateChangeFunc func(ctx context.Context, state *gitstate.GitState)

// Config contains all configuration needed to create a Loop.
type Config struct {
	LLM     llm.Service
	History []llm.Message
	Tools   []*llm.Tool
	// NativeTools are production Dago executables keyed by definition name.
	// Legacy Tools remain the pinned test/provider facade and supply fallbacks
	// only when a native implementation has not been provided.
	NativeTools      []dtool.Tool
	RecordMessage    MessageRecordFunc
	RecordWarning    WarningRecordFunc
	Logger           *slog.Logger
	System           []llm.SystemContent
	WorkingDir       string // working directory for tools
	OnGitStateChange GitStateChangeFunc
	// ThinkingLevel, when non-default, is sent on every llm.Request the loop
	// issues. Per-conversation override; ThinkingLevelDefault means "use the
	// service default".
	ThinkingLevel llm.ThinkingLevel
	// GetWorkingDir returns the current working directory for tools.
	// If set, this is called at end of turn to check for git state changes.
	// If nil, Config.WorkingDir is used as a static value.
	GetWorkingDir func() string
	// OnToolProgress is called when a tool reports progress (partial output).
	OnToolProgress llm.ToolProgressFunc
	// OnStreamDelta is called when the LLM streams a partial content delta.
	OnStreamDelta func(llm.StreamDelta)
	// OnStreamDone is called when a streaming LLM response completes,
	// before the assistant message is recorded. Use this to flush any
	// buffered stream deltas so they reach the UI before the full message.
	OnStreamDone func()
	// InjectMessages, if set, is called between LLM rounds (immediately
	// before each request is built, including the first of a turn). Any
	// messages it returns are appended to history and included in that
	// request. Used to splice subagent completion notifications into an
	// in-flight turn as soon as possible instead of waiting for the turn to
	// end. The callback owns persistence: it must record the messages before
	// returning them, so the DB sequence order matches the in-memory splice
	// point.
	InjectMessages func(ctx context.Context) []llm.Message
	// Saver is the durable Dago checkpoint store for this conversation. When
	// nil, the loop uses an isolated in-memory saver.
	Saver checkpoint.Saver
	// ThreadID is the stable Dago checkpoint thread id. The server uses the
	// conversation id; direct callers receive an isolated per-loop id.
	ThreadID string
	// Namespace isolates independent conversation generations under one thread.
	Namespace string
}

// Loop manages a conversation turn with an LLM including tool execution and message recording.
// Notably, when the turn ends, the "Loop" is over. TODO: maybe rename to Turn?
type Loop struct {
	llm              llm.Service
	tools            []*llm.Tool
	nativeTools      []dtool.Tool
	recordMessage    MessageRecordFunc
	recordWarning    WarningRecordFunc
	history          []llm.Message
	messageQueue     []llm.Message
	totalUsage       llm.Usage
	mu               sync.Mutex
	logger           *slog.Logger
	system           []llm.SystemContent
	workingDir       string
	onGitStateChange GitStateChangeFunc
	getWorkingDir    func() string
	lastGitState     *gitstate.GitState
	onToolProgress   llm.ToolProgressFunc
	onStreamDelta    func(llm.StreamDelta)
	onStreamDone     func()
	injectMessages   func(ctx context.Context) []llm.Message
	thinkingLevel    llm.ThinkingLevel
	notify           chan struct{} // signaled when a message is queued or retry requested
	retryPending     bool          // set by Retry() to re-run processLLMRequest with current history
	saver            checkpoint.Saver
	threadID         string
	namespace        string
	runtimeSeeded    bool
	pendingInput     []llm.Message
	executionMu      sync.Mutex
	runtime          *dagent.Agent
}

// NewLoop creates a new Loop instance with the provided configuration
func NewLoop(config Config) *Loop {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Get initial git state
	workingDir := config.WorkingDir
	if config.GetWorkingDir != nil {
		workingDir = config.GetWorkingDir()
	}
	initialGitState := gitstate.GetGitState(workingDir)

	saver := config.Saver
	if saver == nil {
		saver = checkpoint.NewMemorySaver()
	}
	loop := &Loop{
		llm:              config.LLM,
		history:          config.History,
		tools:            config.Tools,
		nativeTools:      append([]dtool.Tool(nil), config.NativeTools...),
		recordMessage:    config.RecordMessage,
		recordWarning:    config.RecordWarning,
		messageQueue:     make([]llm.Message, 0),
		logger:           logger,
		system:           config.System,
		workingDir:       config.WorkingDir,
		onGitStateChange: config.OnGitStateChange,
		getWorkingDir:    config.GetWorkingDir,
		lastGitState:     initialGitState,
		onToolProgress:   config.OnToolProgress,
		onStreamDelta:    config.OnStreamDelta,
		onStreamDone:     config.OnStreamDone,
		injectMessages:   config.InjectMessages,
		thinkingLevel:    config.ThinkingLevel,
		notify:           make(chan struct{}, 1),
		saver:            saver,
		threadID:         config.ThreadID,
		namespace:        config.Namespace,
	}
	if loop.threadID == "" {
		loop.threadID = fmt.Sprintf("shelley-loop-%p", loop)
	}
	return loop
}

// Retry signals the loop to re-attempt the next LLM request without queueing
// a new user message. The loop's in-memory history is unchanged (failed
// requests don't append anything to history, and error messages are persisted
// to the DB but excluded from context on reload), so the request body sent
// will match the one that originally failed. Safe to call concurrently;
// Go() consumes the retryPending flag exactly once per outer iteration.
func (l *Loop) Retry() {
	l.mu.Lock()
	l.retryPending = true
	l.logger.Debug("retry requested", "history_len", len(l.history))
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// SetThinkingLevel updates the reasoning/thinking level sent on subsequent
// LLM requests. Safe to call concurrently; the new level applies to the next
// request the loop issues.
func (l *Loop) SetThinkingLevel(level llm.ThinkingLevel) {
	l.mu.Lock()
	l.thinkingLevel = level
	l.mu.Unlock()
}

// QueueUserMessage adds a user message to the queue to be processed
func (l *Loop) QueueUserMessage(message llm.Message) {
	l.QueueMessages(message)
}

// QueueMessages atomically appends one or more messages to the loop's queue
// in order, then wakes the loop. The messages can be of any role; this is
// useful for splicing in a synthetic tool_use / tool_result pair that must
// be appended together so the LLM sees a coherent history.
func (l *Loop) QueueMessages(messages ...llm.Message) {
	if len(messages) == 0 {
		return
	}
	l.mu.Lock()
	l.messageQueue = append(l.messageQueue, messages...)
	l.logger.Debug("queued messages", "count", len(messages))
	l.mu.Unlock()
	// Wake the run loop immediately.
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

// GetUsage returns the total usage accumulated by this loop
func (l *Loop) GetUsage() llm.Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalUsage
}

// GetHistory returns a copy of the current conversation history
func (l *Loop) GetHistory() []llm.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Deep copy the messages to prevent modifications
	historyCopy := make([]llm.Message, len(l.history))
	for i, msg := range l.history {
		// Copy the message
		historyCopy[i] = llm.Message{
			Role:    msg.Role,
			ToolUse: msg.ToolUse, // This is a pointer, but we won't modify it in tests
			Content: make([]llm.Content, len(msg.Content)),
		}
		// Copy content slice
		copy(historyCopy[i].Content, msg.Content)
	}
	return historyCopy
}

// Go runs the conversation loop until the context is canceled
func (l *Loop) Go(ctx context.Context) error {
	if l.llm == nil {
		return fmt.Errorf("no LLM service configured")
	}

	l.logger.Info("starting conversation loop", "tools", len(l.tools))

	for {
		select {
		case <-ctx.Done():
			l.logger.Info("conversation loop canceled")
			return ctx.Err()
		default:
		}

		// Process any queued messages
		l.mu.Lock()
		hasQueuedMessages := len(l.messageQueue) > 0
		if hasQueuedMessages {
			// Add queued messages to history (they are already recorded to DB by ConversationManager)
			for _, msg := range l.messageQueue {
				l.history = append(l.history, msg)
			}
			l.pendingInput = append(l.pendingInput, l.messageQueue...)
			l.messageQueue = l.messageQueue[:0] // Clear queue
		}
		retryPending := l.retryPending
		l.retryPending = false
		l.mu.Unlock()

		if hasQueuedMessages || retryPending {
			// Send request to LLM
			l.logger.Debug("processing queued messages", "count", 1)
			if err := l.processLLMRequest(ctx); err != nil {
				l.logger.Error("failed to process LLM request", "error", err)
				time.Sleep(time.Second) // Wait before retrying
				continue
			}
			l.logger.Debug("finished processing queued messages")
		} else {
			// No queued messages, wait for a signal or context cancellation.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-l.notify:
				// Continue loop
			}
		}
	}
}

// ProcessOneTurn processes queued messages through one complete turn (user message + assistant response)
// It stops after the assistant responds, regardless of whether tools were called
func (l *Loop) ProcessOneTurn(ctx context.Context) error {
	if l.llm == nil {
		return fmt.Errorf("no LLM service configured")
	}

	// Process any queued messages first
	l.mu.Lock()
	if len(l.messageQueue) > 0 {
		// Add queued messages to history (they are already recorded to DB by ConversationManager)
		for _, msg := range l.messageQueue {
			l.history = append(l.history, msg)
		}
		l.pendingInput = append(l.pendingInput, l.messageQueue...)
		l.messageQueue = nil
	}
	l.mu.Unlock()

	// Process one LLM request and response
	return l.processLLMRequest(ctx)
}

// processLLMRequest runs one complete turn through Dago's compiled agent graph.
// Shelley remains a UI/database projection layer; model routing, tool scheduling,
// cancellation propagation, state reduction, and checkpoint persistence all occur
// inside Dago.
func (l *Loop) processLLMRequest(ctx context.Context) error {
	l.executionMu.Lock()
	defer l.executionMu.Unlock()
	inputMessages, err := l.runtimeInput(ctx)
	if err != nil {
		return err
	}

	l.mu.Lock()
	tools := append([]*llm.Tool(nil), l.tools...)
	nativeOverrides := append([]dtool.Tool(nil), l.nativeTools...)
	service := l.llm
	system := append([]llm.SystemContent(nil), l.system...)
	l.mu.Unlock()

	dagoTools, err := resolveNativeTools(tools, nativeOverrides, nativeToolOptions{
		WorkingDir: l.currentWorkingDir,
		Progress:   l.onToolProgress,
		Service:    service,
	})
	if err != nil {
		return err
	}
	dagoMessages, err := messagesToDago(inputMessages)
	if err != nil {
		return err
	}

	middleware := []dagent.Middleware{l.runtimeMiddleware()}
	runtime, err := dagent.New(dagent.Options{
		Name: "Shelley", Model: nativeChat(service, nativeChatOptions{
			ThinkingLevel: l.getThinkingLevel,
			OnRetry:       l.recordRetryWarning(ctx),
		}),
		Tools: dagoTools, SystemPrompt: joinSystem(system), Middleware: middleware,
		Saver: l.saver, MaxConcurrency: 1, FailOnToolError: false,
		StateFields: map[string]dagent.StateField{
			"shelley.run": {Kind: dagent.FieldEphemeral, Contract: "shelley.run.v1", Clone: func(value any) any { return value }},
		},
	})
	if err != nil {
		return fmt.Errorf("create Dago Shelley agent: %w", err)
	}
	l.runtime = runtime

	stream := runtime.Stream(ctx, dagent.Input{
		Config: l.checkpointConfig(), Messages: dagoMessages,
		State: state.Values{"shelley.run": true},
	}, 64)
	defer stream.Close()
	for {
		event, nextErr := stream.Next(ctx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			l.flushStream()
			return l.recordRuntimeError(ctx, nextErr)
		}
		if event.Mode == dagent.EventToken && event.Chunk != nil {
			l.emitToken(*event.Chunk)
			continue
		}
		if event.Mode != dagent.EventUpdate {
			continue
		}
		if err := l.projectRuntimeUpdate(ctx, event.Node, event.Update); err != nil {
			return err
		}
	}
	result, err := stream.Result(ctx)
	if err != nil {
		l.flushStream()
		return l.recordRuntimeError(ctx, err)
	}
	l.mu.Lock()
	l.runtimeSeeded = true
	l.mu.Unlock()
	if len(result.Interrupts) > 0 {
		return fmt.Errorf("Dago Shelley agent paused with %d unresolved interrupt(s)", len(result.Interrupts))
	}
	l.checkGitStateChange(ctx)
	return nil
}

// ResolveCancellation durably appends cancellation messages and clears any
// pending model/tool tasks in the Dago graph before a later turn resumes.
func (l *Loop) ResolveCancellation(ctx context.Context, messages ...llm.Message) error {
	l.executionMu.Lock()
	defer l.executionMu.Unlock()
	if l.runtime == nil {
		return nil
	}
	converted, err := messagesToDago(messages)
	if err != nil {
		return err
	}
	if _, err := l.runtime.Cancel(ctx, dagent.Input{
		Config: l.checkpointConfig(), Messages: converted,
	}); err != nil {
		return fmt.Errorf("resolve Dago Shelley cancellation: %w", err)
	}
	l.mu.Lock()
	l.history = append(l.history, messages...)
	l.runtimeSeeded = true
	l.mu.Unlock()
	return nil
}

func (l *Loop) runtimeInput(ctx context.Context) ([]llm.Message, error) {
	l.mu.Lock()
	seeded := l.runtimeSeeded
	pending := append([]llm.Message(nil), l.pendingInput...)
	l.pendingInput = nil
	history := append([]llm.Message(nil), l.history...)
	l.mu.Unlock()
	if !seeded {
		tuple, err := l.saver.GetTuple(ctx, l.checkpointConfig())
		if err != nil {
			return nil, fmt.Errorf("load Dago Shelley checkpoint: %w", err)
		}
		if tuple != nil {
			l.mu.Lock()
			l.runtimeSeeded = true
			l.mu.Unlock()
			return pending, nil
		}
		request := &llm.Request{Messages: history}
		l.insertMissingToolResults(request)
		return request.Messages, nil
	}
	return pending, nil
}

func (l *Loop) checkpointConfig() checkpoint.Config {
	return checkpoint.Config{ThreadID: l.threadID, Namespace: l.namespace}
}

func (l *Loop) runtimeMiddleware() dagent.Middleware {
	return dagent.Middleware{
		Name: "shelley_runtime",
		BeforeModel: func(ctx context.Context, _ state.Values, _ dagent.Runtime) (state.Values, error) {
			var additions []llm.Message
			if l.injectMessages != nil {
				additions = append(additions, l.injectMessages(ctx)...)
			}
			l.mu.Lock()
			if len(l.messageQueue) > 0 {
				additions = append(additions, l.messageQueue...)
				l.messageQueue = nil
				l.logger.Info("processing user interruption during Dago tool execution")
			}
			l.history = append(l.history, additions...)
			l.mu.Unlock()
			if len(additions) == 0 {
				return nil, nil
			}
			converted, err := messagesToDago(additions)
			if err != nil {
				return nil, err
			}
			return state.Values{dagent.MessagesKey: converted}, nil
		},
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			level := l.getThinkingLevel()
			if request.Model.Profile().SupportsReasoning && level != llm.ThinkingLevelDefault {
				request.Reasoning = &dmodel.Reasoning{}
				if level != llm.ThinkingLevelOff {
					request.Reasoning.Effort = level.ThinkingEffort()
					request.Reasoning.Summary = "auto"
				}
			}
			requestCtx, cancel := context.WithTimeout(ctx, maxTurnDuration)
			defer cancel()
			const attempts = 2
			var response dagent.ModelResponse
			var err error
			for attempt := 1; attempt <= attempts; attempt++ {
				response, err = next(requestCtx, request)
				if err == nil || !isRetryableError(err) || attempt == attempts {
					return response, err
				}
				timer := time.NewTimer(time.Duration(attempt) * time.Second)
				select {
				case <-timer.C:
				case <-requestCtx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return dagent.ModelResponse{}, requestCtx.Err()
				}
			}
			return response, err
		},
		AfterModel: func(_ context.Context, values state.Values, _ dagent.Runtime) (state.Values, error) {
			messages, _ := values[dagent.MessagesKey].([]dmessage.Message)
			if len(messages) == 0 {
				return nil, nil
			}
			last := messages[len(messages)-1]
			response, ok, err := responseFromDago(last)
			if err != nil || !ok {
				return nil, err
			}
			if response.StopReason == llm.StopReasonMaxTokens || response.StopReason == llm.StopReasonRefusal {
				return state.Values{dagent.MessagesKey: []dmessage.Message{dmessage.Remove(last.ID)}}, nil
			}
			return nil, nil
		},
		BeforeTools: func(_ context.Context, request dagent.ToolBatchRequest) (dagent.ToolBatchResponse, error) {
			calls := make([]dmessage.ToolCall, 0, len(request.Calls))
			var missing []dmessage.Message
			for _, call := range request.Calls {
				if request.Tools[call.Name] != nil {
					calls = append(calls, call)
					continue
				}
				exact := llm.Content{
					Type: llm.ContentTypeToolResult, ToolUseID: call.ID, ToolError: true,
					ToolResult: llm.TextContent(fmt.Sprintf("Tool '%s' not found", call.Name)),
				}
				artifact, err := json.Marshal(map[string]any{
					"version": 1, "kind": "shelley.tool_result.v1", "content": exact,
				})
				if err != nil {
					return dagent.ToolBatchResponse{}, err
				}
				message := dmessage.Tool(call.ID, exact.ToolResult[0].Text)
				message.Name = call.Name
				message.ToolStatus = dmessage.ToolStatusError
				message.Artifact = artifact
				missing = append(missing, message)
			}
			if len(missing) == 0 {
				return dagent.ToolBatchResponse{}, nil
			}
			return dagent.ToolBatchResponse{Calls: calls, Messages: missing}, nil
		},
		WrapToolCall: func(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
			started := time.Now()
			response, err := next(ctx, request)
			finished := time.Now()
			if err != nil {
				return response, err
			}
			var existing toolArtifactEnvelope
			if len(response.Result.Artifact) > 0 && json.Unmarshal(response.Result.Artifact, &existing) == nil && existing.Kind == shelleyToolArtifact {
				return response, nil
			}
			var display any
			if len(response.Result.Artifact) > 0 {
				display = append(json.RawMessage(nil), response.Result.Artifact...)
			}
			exact := llm.Content{
				Type: llm.ContentTypeToolResult, ToolUseID: request.Call.ID,
				ToolResult:       contentFromDago(response.Result.Content),
				ToolUseStartTime: &started, ToolUseEndTime: &finished, Display: display,
			}
			artifact, encodeErr := json.Marshal(toolArtifactEnvelope{Version: 1, Kind: shelleyToolArtifact, Content: exact})
			if encodeErr != nil {
				return dagent.ToolCallResponse{}, encodeErr
			}
			response.Result.Artifact = artifact
			return response, nil
		},
	}
}

func (l *Loop) projectRuntimeUpdate(ctx context.Context, node string, update state.Values) error {
	value, ok := update[dagent.MessagesKey]
	if !ok {
		return nil
	}
	messages, ok := value.([]dmessage.Message)
	if !ok {
		return fmt.Errorf("Dago Shelley message update has type %T", value)
	}
	if node == "model" {
		l.flushStream()
		for _, item := range messages {
			response, found, err := responseFromDago(item)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if response.Model == "" {
				if identified, ok := l.llm.(interface{ ModelID() string }); ok {
					response.Model = identified.ModelID()
					response.Usage.Model = response.Model
				}
			}
			l.mu.Lock()
			l.totalUsage.Add(response.Usage)
			l.mu.Unlock()
			switch response.StopReason {
			case llm.StopReasonMaxTokens:
				return l.handleMaxTokensTruncation(ctx, response)
			case llm.StopReasonRefusal:
				return l.handleRefusal(ctx, response)
			}
			assistant := response.ToMessage()
			l.mu.Lock()
			l.history = append(l.history, assistant)
			l.mu.Unlock()
			if err := l.recordMessage(ctx, assistant, response.UsageWithMeta(), nil); err != nil {
				l.logger.Error("failed to record assistant message", "error", err)
			}
		}
		return nil
	}
	if node != "tools" {
		return nil
	}
	toolMessage := llm.Message{Role: llm.MessageRoleUser}
	var otherUsage []llm.PurposedUsage
	for _, item := range messages {
		if item.Role != dmessage.RoleTool {
			continue
		}
		content, usage, err := toolResultFromDago(item)
		if err != nil {
			return err
		}
		toolMessage.Content = append(toolMessage.Content, content)
		otherUsage = append(otherUsage, usage...)
	}
	if len(toolMessage.Content) == 0 {
		return nil
	}
	l.mu.Lock()
	l.history = append(l.history, toolMessage)
	l.mu.Unlock()
	if err := l.recordMessage(ctx, toolMessage, llm.Usage{}, otherUsage); err != nil {
		l.logger.Error("failed to record tool result message", "error", err)
	}
	return nil
}

func (l *Loop) currentWorkingDir() string {
	if l.getWorkingDir != nil {
		return l.getWorkingDir()
	}
	return l.workingDir
}

func (l *Loop) getThinkingLevel() llm.ThinkingLevel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.thinkingLevel
}

func joinSystem(items []llm.SystemContent) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.Text)
	}
	return strings.Join(parts, "\n\n")
}

func (l *Loop) emitToken(chunk dmodel.Chunk) {
	if l.onStreamDelta == nil {
		return
	}
	for index, block := range chunk.MessageDelta.Content {
		blockIndex := index
		if block.Index != nil {
			blockIndex = *block.Index
		}
		switch block.Type {
		case dmessage.BlockText:
			if block.Text != "" {
				l.onStreamDelta(llm.StreamDelta{Type: "text", Text: block.Text, Index: blockIndex})
			}
		case dmessage.BlockReasoning:
			if block.Reasoning != "" {
				l.onStreamDelta(llm.StreamDelta{Type: "thinking", Text: block.Reasoning, Index: blockIndex})
			}
		}
	}
}

func (l *Loop) flushStream() {
	if l.onStreamDone != nil {
		l.onStreamDone()
	}
}

func (l *Loop) recordRuntimeError(ctx context.Context, err error) error {
	displayErr := err
	if text := strings.TrimPrefix(err.Error(), `execute node "model": `); text != err.Error() {
		displayErr = errors.New(text)
	}
	message := llm.Message{
		Role: llm.MessageRoleAssistant, Content: llm.TextContent(userFacingLLMError(displayErr, nil)),
		EndOfTurn: true, ErrorType: llm.ErrorTypeLLMRequest, ErrorRetryable: IsRetryableLLMError(err),
	}
	if recordErr := l.recordMessage(ctx, message, llm.Usage{}, nil); recordErr != nil {
		l.logger.Error("failed to record Dago runtime error", "error", recordErr)
	}
	return fmt.Errorf("LLM request failed: %w", err)
}

func (l *Loop) recordRetryWarning(ctx context.Context) func(llm.RetryEvent) {
	if l.recordWarning == nil {
		return nil
	}
	return func(event llm.RetryEvent) {
		msg := llm.FormatRetryEvent(event)
		if err := l.recordWarning(ctx, msg); err != nil {
			l.logger.Error("failed to record retry warning", "error", err)
		}
	}
}

// checkGitStateChange checks if the git state has changed and calls the callback if so.
// This is called at the end of each turn.
func (l *Loop) checkGitStateChange(ctx context.Context) {
	if l.onGitStateChange == nil {
		return
	}

	// Get current working directory
	workingDir := l.workingDir
	if l.getWorkingDir != nil {
		workingDir = l.getWorkingDir()
	}

	// Get current git state
	currentState := gitstate.GetGitState(workingDir)

	// Compare with last known state
	l.mu.Lock()
	lastState := l.lastGitState
	l.mu.Unlock()

	// Check if state changed
	if !currentState.Equal(lastState) {
		l.mu.Lock()
		l.lastGitState = currentState
		l.mu.Unlock()

		if currentState.IsRepo {
			l.logger.Debug("git state changed",
				"worktree", currentState.Worktree,
				"branch", currentState.Branch,
				"commit", currentState.Commit)
			l.onGitStateChange(ctx, currentState)
		}
	}
}

// handleMaxTokensTruncation handles the case where the LLM response was truncated
// due to hitting the maximum output token limit. It records the truncated message
// for cost tracking (excluded from context) and an error message for the user.
func (l *Loop) handleMaxTokensTruncation(ctx context.Context, resp *llm.Response) error {
	// Record the truncated message for cost tracking, but mark it as excluded from context.
	// This preserves billing information without confusing the LLM on future turns.
	truncatedMessage := resp.ToMessage()
	truncatedMessage.ExcludedFromContext = true

	// Record the truncated message with usage metadata
	if err := l.recordMessage(ctx, truncatedMessage, resp.UsageWithMeta(), nil); err != nil {
		l.logger.Error("failed to record truncated message", "error", err)
	}

	// Record a truncation error message with EndOfTurn=true to properly signal end of turn.
	errorMessage := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{
			{
				Type: llm.ContentTypeText,
				Text: "[SYSTEM ERROR: Your previous response was truncated because it exceeded the maximum output token limit. " +
					"Any tool calls in that response were lost. Please retry with smaller, incremental changes. " +
					"For file operations, break large changes into multiple smaller patches. " +
					"The user can ask you to continue if needed.]",
			},
		},
		EndOfTurn: true,
		ErrorType: llm.ErrorTypeTruncation,
	}

	l.mu.Lock()
	l.history = append(l.history, errorMessage)
	l.mu.Unlock()

	// Record the truncation error message
	if err := l.recordMessage(ctx, errorMessage, llm.Usage{}, nil); err != nil {
		l.logger.Error("failed to record truncation error message", "error", err)
	}

	// End the turn - don't automatically continue
	l.checkGitStateChange(ctx)
	return nil
}

// handleRefusal handles a stop_reason=refusal response: the model declined to
// continue. Such responses usually carry no visible content (just a thinking
// block, or nothing), so recording them normally leaves a blank agent bubble
// and, because the empty response ends up in history, every follow-up
// "continue" replays the same context and refuses again. We instead record the
// raw response excluded from context (for cost tracking) and record a visible,
// non-retryable error message that ends the turn. Neither is added to the live
// context history, matching the cold-start rehydration path.
func (l *Loop) handleRefusal(ctx context.Context, resp *llm.Response) error {
	// Record the raw refusal for cost tracking, but keep it out of context so it
	// doesn't poison future turns (an empty/near-empty assistant turn biases the
	// model toward refusing again, and empty content blocks can wedge replay).
	rawMessage := resp.ToMessage()
	rawMessage.ExcludedFromContext = true

	if err := l.recordMessage(ctx, rawMessage, resp.UsageWithMeta(), nil); err != nil {
		l.logger.Error("failed to record refusal message", "error", err)
	}

	// Build the user-visible notice. Start with the standard guidance, then
	// append every field the provider gave us in the refusal reason (category
	// and full explanation), so nothing is hidden from the user.
	noticeText := "[The model declined to continue this request. Retrying the same " +
		"request will likely be declined again. Switch to Opus to continue, " +
		"or use /model to switch models. You can also try rephrasing or " +
		"clarifying the intent instead.]"
	var refusalCategory, refusalExplanation string
	if resp.RefusalDetails != nil {
		refusalCategory = strings.TrimSpace(resp.RefusalDetails.Category)
		refusalExplanation = strings.TrimSpace(resp.RefusalDetails.Explanation)
		if refusalCategory != "" {
			noticeText += "\n\nCategory: " + refusalCategory
		}
		if refusalExplanation != "" {
			noticeText += "\n\nReason: " + refusalExplanation
		}
	}

	// Record a visible refusal notice with EndOfTurn=true. Marked non-retryable:
	// re-running the identical request just refuses again, so the UI should not
	// offer a Retry button. Rephrasing the request is what actually helps.
	//
	// Deliberately NOT appended to l.history: like other error messages it is a
	// system-generated, user-visible artifact that must not be sent back to the
	// model. The cold-start path already excludes it from context
	// (partitionMessages skips MessageTypeError, ListMessagesForContext skips
	// excluded rows), so keeping it out of the live in-memory history too makes
	// active-session and rehydrated behavior identical. Otherwise a rephrase in
	// the same session would show the model an assistant turn narrating its own
	// refusal, biasing it toward refusing again. (Mirrors the llm_request error
	// path above, which also records without appending.)
	errorMessage := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{
			{
				Type: llm.ContentTypeText,
				Text: noticeText,
			},
		},
		EndOfTurn:          true,
		ErrorType:          llm.ErrorTypeRefusal,
		ErrorRetryable:     false,
		RefusalCategory:    refusalCategory,
		RefusalExplanation: refusalExplanation,
	}

	if err := l.recordMessage(ctx, errorMessage, llm.Usage{}, nil); err != nil {
		l.logger.Error("failed to record refusal error message", "error", err)
	}

	// End the turn - don't automatically continue.
	l.checkGitStateChange(ctx)
	return nil
}

// insertMissingToolResults fixes tool_result issues in the conversation history:
//  1. Adds error results for tool_uses that were requested but not included in the next message.
//     This can happen when a request is cancelled or fails after the LLM responds with tool_use
//     blocks but before the tools execute.
//  2. Removes orphan tool_results that reference tool_use IDs not present in the immediately
//     preceding assistant message. This can happen when a tool execution completes after
//     CancelConversation has already written cancellation messages.
//
// This prevents API errors like:
//   - "tool_use ids were found without tool_result blocks"
//   - "unexpected tool_use_id found in tool_result blocks ... Each tool_result block must have
//     a corresponding tool_use block in the previous message"
//
// Mutates the request's Messages slice.
func (l *Loop) insertMissingToolResults(req *llm.Request) {
	if len(req.Messages) < 1 {
		return
	}

	// Scan through all messages looking for assistant messages with tool_use
	// that are not immediately followed by a user message with corresponding tool_results.
	// We may need to insert synthetic user messages with tool_results or filter orphans.
	var newMessages []llm.Message
	totalInserted := 0
	totalRemoved := 0

	// Track the tool_use IDs from the most recent assistant message
	var prevAssistantToolUseIDs map[string]bool

	for i := 0; i < len(req.Messages); i++ {
		msg := req.Messages[i]

		if msg.Role == llm.MessageRoleAssistant {
			// Handle empty assistant messages - add placeholder content if not the last message
			// The API requires all messages to have non-empty content except for the optional
			// final assistant message. Empty content can happen when the model ends its turn
			// without producing any output.
			if len(msg.Content) == 0 && i < len(req.Messages)-1 {
				req.Messages[i].Content = []llm.Content{{Type: llm.ContentTypeText, Text: "(no response)"}}
				msg = req.Messages[i] // update local copy for subsequent processing
				l.logger.Debug("added placeholder content to empty assistant message", "index", i)
			}

			// Track all tool_use IDs in this assistant message
			prevAssistantToolUseIDs = make(map[string]bool)
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolUse {
					prevAssistantToolUseIDs[c.ID] = true
				}
			}
			newMessages = append(newMessages, msg)

			// Check if next message needs synthetic tool_results
			var toolUseContents []llm.Content
			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolUse {
					toolUseContents = append(toolUseContents, c)
				}
			}

			if len(toolUseContents) == 0 {
				continue
			}

			// Check if next message is a user message with corresponding tool_results
			var nextMsg *llm.Message
			if i+1 < len(req.Messages) {
				nextMsg = &req.Messages[i+1]
			}

			if nextMsg == nil || nextMsg.Role != llm.MessageRoleUser {
				// Next message is not a user message (or there is no next message).
				// Insert a synthetic user message with tool_results for all tool_uses.
				var toolResultContent []llm.Content
				for _, tu := range toolUseContents {
					toolResultContent = append(toolResultContent, llm.Content{
						Type:      llm.ContentTypeToolResult,
						ToolUseID: tu.ID,
						ToolError: true,
						ToolResult: []llm.Content{{
							Type: llm.ContentTypeText,
							Text: "not executed; retry possible",
						}},
					})
				}
				syntheticMsg := llm.Message{
					Role:    llm.MessageRoleUser,
					Content: toolResultContent,
				}
				newMessages = append(newMessages, syntheticMsg)
				totalInserted += len(toolResultContent)
			}
		} else if msg.Role == llm.MessageRoleUser {
			// Filter out orphan tool_results and add missing ones
			var filteredContent []llm.Content
			existingResultIDs := make(map[string]bool)

			for _, c := range msg.Content {
				if c.Type == llm.ContentTypeToolResult {
					// Only keep tool_results that match a tool_use in the previous assistant message
					if prevAssistantToolUseIDs != nil && prevAssistantToolUseIDs[c.ToolUseID] {
						filteredContent = append(filteredContent, c)
						existingResultIDs[c.ToolUseID] = true
					} else {
						// Orphan tool_result - skip it
						totalRemoved++
						l.logger.Debug("removing orphan tool_result", "tool_use_id", c.ToolUseID)
					}
				} else {
					// Keep non-tool_result content
					filteredContent = append(filteredContent, c)
				}
			}

			// Check if we need to add missing tool_results for this user message
			if prevAssistantToolUseIDs != nil {
				var prefix []llm.Content
				for toolUseID := range prevAssistantToolUseIDs {
					if !existingResultIDs[toolUseID] {
						prefix = append(prefix, llm.Content{
							Type:      llm.ContentTypeToolResult,
							ToolUseID: toolUseID,
							ToolError: true,
							ToolResult: []llm.Content{{
								Type: llm.ContentTypeText,
								Text: "not executed; retry possible",
							}},
						})
						totalInserted++
					}
				}
				if len(prefix) > 0 {
					filteredContent = append(prefix, filteredContent...)
				}
			}

			// Only add the message if it has content
			if len(filteredContent) > 0 {
				msg.Content = filteredContent
				newMessages = append(newMessages, msg)
			} else {
				// Message is now empty after filtering - skip it entirely
				l.logger.Debug("removing empty user message after filtering orphan tool_results")
			}

			// Reset for next iteration - user message "consumes" the previous tool_uses
			prevAssistantToolUseIDs = nil
		} else {
			newMessages = append(newMessages, msg)
		}
	}

	if totalInserted > 0 || totalRemoved > 0 {
		req.Messages = newMessages
		if totalInserted > 0 {
			l.logger.Debug("inserted missing tool results", "count", totalInserted)
		}
		if totalRemoved > 0 {
			l.logger.Debug("removed orphan tool results", "count", totalRemoved)
		}
	}
}

// userFacingLLMError renders an LLM request failure into a message shown in
// the conversation. For the idle/stall timeout it explains what happened and
// what to do, rather than surfacing an opaque "context deadline exceeded".
// Other errors fall through to their raw text. When trace carries any
// correlation ids (Shelley's own request id and/or an upstream provider id),
// they are appended so users can quote them to support.
func userFacingLLMError(err error, trace *llmhttp.RequestTrace) string {
	var msg string
	if errors.Is(err, llmhttp.ErrIdleTimeout) {
		msg = fmt.Sprintf(
			"LLM request timed out: the model stopped sending data for %s "+
				"(idle/stall timeout), so the request was aborted. This usually "+
				"means the provider or upstream connection stalled mid-response, "+
				"not that your turn was too long — a slow but steadily streaming "+
				"response is allowed to finish. Press Retry to try again.\n\n"+
				"Details: %v",
			llmhttp.DefaultIdleTimeout, err,
		)
	} else {
		msg = fmt.Sprintf("LLM request failed: %v", err)
	}
	if trace != nil {
		if ids := trace.String(); ids != "" {
			msg += "\n\n(" + ids + ")"
		}
	}
	return msg
}

// isRetryableError checks if an LLM request error should be retried by the
// loop's tight inner-retry loop (max 2 attempts, ~1s sleep). Keep this set
// narrow: this is for transport-level hiccups that have a good chance of
// succeeding immediately. Provider-level 5xx, rate limits, and scale-up
// hints are handled by the user-facing Retry button (IsRetryableLLMError),
// which has no short retry budget and won't hammer providers.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	// A stalled stream (no bytes within the idle window) is a transport-level
	// hiccup: a fresh attempt often succeeds immediately.
	if errors.Is(err, llmhttp.ErrIdleTimeout) {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, p := range []string{
		"eof",
		"connection reset",
		"connection refused",
		"no such host",
		"network is unreachable",
		"i/o timeout",
		"reset by peer",
		"broken pipe",
	} {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// IsRetryableLLMError reports whether an LLM request failure is transient and
// safe to retry by re-sending the same conversation state.
//
// Retryable: transport hiccups (EOF, resets, timeouts), upstream 5xx, gateway
// errors, Fireworks scale-up hints, rate limits. NOT retryable: auth,
// quota/credits, 400 validation errors, missing models.
//
// Note: a generic "context canceled" string CAN come from a user-initiated
// cancel as well as a server-side timeout. We classify it retryable here
// because the cancel path records its own "[Operation cancelled]" tool
// result (not an llm_request error message), so the only thing reaching this
// classifier is a non-user-initiated timeout/disconnect.
func IsRetryableLLMError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	// Idle/stall timeout: the stream went silent mid-flight. Re-sending the
	// same conversation state is safe and usually succeeds.
	if errors.Is(err, llmhttp.ErrIdleTimeout) {
		return true
	}
	lower := strings.ToLower(err.Error())

	// Hard non-retryable signals override anything else.
	nonRetryable := []string{
		"credits exhausted",
		"insufficient_quota",
		"invalid api key",
		"invalid_api_key",
		"unauthorized",
		"permission denied",
		"forbidden",
		"invalid_request_error", // 400 from providers
		"model_not_found",
		"does not exist or you do not have access",
	}
	for _, p := range nonRetryable {
		if strings.Contains(lower, p) {
			return false
		}
	}

	retryableSubstrings := []string{
		// Transport-layer
		"eof",
		"connection reset",
		"connection refused",
		"no such host",
		"network is unreachable",
		"i/o timeout",
		"context deadline exceeded",
		"context canceled",
		"context cancelled",
		"deadline exceeded",
		"broken pipe",
		"reset by peer",
		"tls handshake",
		// Provider/gateway 5xx (as words, not bare numerics)
		"internal server error",
		"bad gateway",
		"service unavailable",
		"gateway timeout",
		"gateway proxy error",
		"upstream connect error",
		"overloaded",
		"rate limit",
		"too many requests",
		"server had an error processing your request",
		// Fireworks scale-up hint
		"deployment_scaling_up",
		"scaling up",
		// Generic provider "please retry" hint
		"please retry",
	}
	for _, pattern := range retryableSubstrings {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	// HTTP status codes — match them in contexts where they look like a
	// status code rather than a random number in the body.
	if httpStatus5xxRE.MatchString(lower) {
		return true
	}
	return false
}

// httpStatus5xxRE matches 5xx HTTP status codes when they appear in
// status-like contexts (after "status", "http", "code", "returned", "error
// code", or as a bare number in a typical "error code: 503" line). Avoids
// matching numbers like 500 in token counts or other unrelated payloads.
var httpStatus5xxRE = regexp.MustCompile(`(?:status|http|code|returned|response)[ :=]+5(?:00|02|03|04)\b`)
