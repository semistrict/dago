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

	dago "github.com/semistrict/dago"
	dbackend "github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	dagent "github.com/semistrict/dago/dagent"
	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/gitstate"
	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/llm/llmhttp"
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
	Model            damodel.Chat
	ModelID          string
	History          []llm.Message
	Tools            []datool.Tool
	RecordMessage    MessageRecordFunc
	RecordWarning    WarningRecordFunc
	Logger           *slog.Logger
	System           []llm.SystemContent
	WorkingDir       string // working directory for tools
	OnGitStateChange GitStateChangeFunc
	// ThinkingLevel, when non-default, is sent on every native model request the loop
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
	// Saver is the durable dago checkpoint store for this conversation. When
	// nil, the loop uses an isolated in-memory saver.
	Saver dacheckpoint.Saver
	// ThreadID is the stable dago checkpoint thread id. The server uses the
	// conversation id; direct callers receive an isolated per-loop id.
	ThreadID string
	// Namespace isolates independent conversation generations under one thread.
	Namespace string
	// FilesystemTools is the canonical dago filesystem surface selected for
	// this conversation.
	FilesystemTools []string
	// SkillCatalog is an application-resolved catalog consumed by dago's
	// progressive-disclosure middleware.
	SkillCatalog    []dago.Skill
	SkillActivation func(dago.Skill) string
	Memory          []string
	MemoryContents  map[string]string
	MemoryPrompt    *string
}

// Loop manages a conversation turn with an LLM including tool execution and message recording.
// Notably, when the turn ends, the "Loop" is over. TODO: maybe rename to Turn?
type Loop struct {
	model            damodel.Chat
	modelID          string
	tools            []datool.Tool
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
	saver            dacheckpoint.Saver
	threadID         string
	namespace        string
	filesystemTools  []string
	skillCatalog     []dago.Skill
	skillActivation  func(dago.Skill) string
	memory           []string
	memoryContents   map[string]string
	memoryPrompt     *string
	runtimeSeeded    bool
	pendingInput     []llm.Message
	executionMu      sync.Mutex
	runtime          *dago.DeepAgent
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
		saver = dacheckpoint.NewMemorySaver()
	}
	loop := &Loop{
		model:            config.Model,
		modelID:          config.ModelID,
		history:          config.History,
		tools:            append([]datool.Tool(nil), config.Tools...),
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
		filesystemTools:  cloneFilesystemTools(config.FilesystemTools),
		skillCatalog:     cloneSkillCatalog(config.SkillCatalog),
		skillActivation:  config.SkillActivation,
		memory:           append([]string(nil), config.Memory...),
		memoryContents:   cloneStringMap(config.MemoryContents),
		memoryPrompt:     cloneStringPointer(config.MemoryPrompt),
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
	if l.model == nil {
		return fmt.Errorf("no chat model configured")
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
	if l.model == nil {
		return fmt.Errorf("no chat model configured")
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

// processLLMRequest runs one complete turn through dago's compiled agent graph.
// Shelley remains a UI/database projection layer; model routing, tool scheduling,
// cancellation propagation, state reduction, and checkpoint persistence all occur
// inside dago.
func (l *Loop) processLLMRequest(ctx context.Context) error {
	l.executionMu.Lock()
	defer l.executionMu.Unlock()
	inputMessages, err := l.runtimeInput(ctx)
	if err != nil {
		return err
	}

	dagoMessages, err := messagesToNative(inputMessages)
	if err != nil {
		return err
	}

	if l.runtime == nil {
		l.mu.Lock()
		tools := append([]datool.Tool(nil), l.tools...)
		model := l.model
		system := append([]llm.SystemContent(nil), l.system...)
		l.mu.Unlock()

		dagoTools, validateErr := validateTools(tools)
		if validateErr != nil {
			return validateErr
		}
		harnessBackend, backendErr := dbackend.NewLocalShell(dbackend.LocalShellOptions{
			Filesystem: dbackend.FilesystemOptions{Root: l.currentWorkingDir(), GetRoot: l.currentWorkingDir, AllowHostPaths: true},
		})
		if backendErr != nil {
			return fmt.Errorf("create Shelley harness backend: %w", backendErr)
		}
		if l.filesystemTools != nil && isPredictableModel(model) && !hasToolNamed(dagoTools, "bash") {
			aliases, aliasErr := predictableFilesystemAliases(harnessBackend, l.filesystemTools)
			if aliasErr != nil {
				return aliasErr
			}
			dagoTools = append(dagoTools, aliases...)
		}
		options := dago.Options{
			Name: "Shelley", Model: model,
			Tools: dagoTools, SystemPrompt: runtimeSystemPrompt(system), Middleware: []dagent.Middleware{l.runtimeMiddleware(harnessBackend)},
			Backend: harnessBackend,
			Saver:   l.saver, RetainThreadState: true, MaxConcurrency: 1, FailOnToolError: false,
			StateFields: map[string]dagent.StateField{
				"shelley.run": {Kind: dagent.FieldEphemeral, Contract: "shelley.run.v1", Clone: func(value any) any { return value }},
			},
		}
		if l.filesystemTools == nil {
			// A nil selection is the embedding form used by direct callers that
			// provide their complete tool surface. It still runs through dago, but
			// does not opt into dago's default harness tools or prompts.
			options.FilesystemTools = []string{}
			options.DisableTodo = true
			options.DisableSubagents = true
			options.DisableSummary = true
		} else {
			options.FilesystemTools = cloneFilesystemTools(l.filesystemTools)
			options.SkillCatalog = cloneSkillCatalog(l.skillCatalog)
			options.SkillActivation = l.skillActivation
			options.Memory = append([]string(nil), l.memory...)
			options.MemoryContents = cloneStringMap(l.memoryContents)
			options.MemorySystemPrompt = cloneStringPointer(l.memoryPrompt)
			// Shelley uses dago's conversation-subagent tool so child runs retain
			// their application-level UI and persistence contracts.
			options.DisableSubagents = true
		}
		runtime, runtimeErr := dago.New(options)
		if runtimeErr != nil {
			return fmt.Errorf("create dago Shelley agent: %w", runtimeErr)
		}
		l.runtime = runtime
	}
	runtime := l.runtime

	requestCtx, trace := llmhttp.WithRequestTrace(ctx)
	stream := runtime.Stream(requestCtx, dagent.Input{
		Config: l.checkpointConfig(), Messages: dagoMessages,
		State: dastate.Values{"shelley.run": true}, SkipValueEvents: true, DiscardResultState: true,
	}, 64)
	defer stream.Close()
	for {
		event, nextErr := stream.Next(requestCtx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			l.flushStream()
			return l.recordRuntimeError(ctx, nextErr, trace)
		}
		if event.Mode == dagent.EventToken && event.Chunk != nil {
			l.emitToken(*event.Chunk)
			continue
		}
		if event.Mode == dagent.EventToolProgress && event.ToolProgress != nil {
			if l.onToolProgress != nil {
				l.onToolProgress(llm.ToolProgress{ToolUseID: event.ToolProgress.CallID, ToolName: event.ToolProgress.Name, Output: event.ToolProgress.Output})
			}
			continue
		}
		if event.Mode != dagent.EventUpdate {
			continue
		}
		if err := l.projectRuntimeUpdate(ctx, event.Node, event.Update); err != nil {
			return err
		}
	}
	result, err := stream.Result(requestCtx)
	if err != nil {
		l.flushStream()
		return l.recordRuntimeError(ctx, err, trace)
	}
	l.mu.Lock()
	l.runtimeSeeded = true
	l.mu.Unlock()
	if len(result.Interrupts) > 0 {
		return fmt.Errorf("dago Shelley agent paused with %d unresolved interrupt(s)", len(result.Interrupts))
	}
	l.checkGitStateChange(ctx)
	return nil
}

func hasToolNamed(tools []datool.Tool, name string) bool {
	for _, item := range tools {
		if item.Definition().Name == name {
			return true
		}
	}
	return false
}

func cloneFilesystemTools(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func cloneSkillCatalog(values []dago.Skill) []dago.Skill {
	if values == nil {
		return nil
	}
	result := make([]dago.Skill, len(values))
	for index, item := range values {
		result[index] = item
		result[index].AllowedTools = append([]string(nil), item.AllowedTools...)
		if item.Metadata != nil {
			result[index].Metadata = make(map[string]string, len(item.Metadata))
			for key, value := range item.Metadata {
				result[index].Metadata[key] = value
			}
		}
	}
	return result
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func isPredictableModel(chat damodel.Chat) bool {
	profile := chat.Profile()
	return profile.Provider == "builtin" && profile.Model == "predictable-v1"
}

func predictableFilesystemAliases(files dbackend.Backend, selected []string) ([]datool.Tool, error) {
	selectedNames := make(map[string]bool, len(selected))
	for _, name := range selected {
		selectedNames[name] = true
	}
	enabled := func(name string) bool { return selected == nil || selectedNames[name] }
	wanted := make([]string, 0, 3)
	for _, name := range []string{"execute", "write_file", "edit_file"} {
		if enabled(name) {
			wanted = append(wanted, name)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	middleware, err := dago.FilesystemMiddleware(dago.FilesystemOptions{Backend: files, Tools: wanted})
	if err != nil {
		return nil, fmt.Errorf("create predictable filesystem alias: %w", err)
	}
	byName := make(map[string]datool.Tool, len(middleware.Tools))
	for _, item := range middleware.Tools {
		byName[item.Definition().Name] = item
	}
	result := make([]datool.Tool, 0, 2)
	if execute := byName["execute"]; execute != nil {
		alias, aliasErr := datool.Alias(execute, "bash")
		if aliasErr != nil {
			return nil, fmt.Errorf("create predictable filesystem alias: %w", aliasErr)
		}
		// The deterministic fixture predates execute's explicit exit-status footer.
		// Keep its historical exact-output assertions while still executing through
		// the canonical dago backend and schema.
		type input struct {
			Command string `json:"command"`
			Timeout *int   `json:"timeout,omitempty"`
		}
		definition := alias.Definition()
		result = append(result, datool.MustNew(definition.Name, definition.Description, func(ctx context.Context, input input) (datool.Result, error) {
			runtime, _ := datool.RuntimeFromContext(ctx)
			raw, err := json.Marshal(input)
			if err != nil {
				return datool.Result{}, err
			}
			output, executeErr := alias.Execute(ctx, raw, runtime)
			if executeErr != nil {
				return output, executeErr
			}
			const successFooter = "\n[Command succeeded with exit code 0]"
			for index := range output.Content {
				if output.Content[index].Type == dmessage.BlockText && strings.HasSuffix(output.Content[index].Text, successFooter) {
					output.Content[index].Text = strings.TrimSuffix(strings.TrimSuffix(output.Content[index].Text, successFooter), "\n")
				}
			}
			return output, nil
		}, datool.WithTransformSchema(func(json.RawMessage) (json.RawMessage, error) {
			return definition.InputSchema, nil
		})))
	}
	if write := byName["write_file"]; write != nil {
		result = append(result, predictablePatchAlias(files, write, byName["edit_file"]))
	}
	return result, nil
}

// predictablePatchAlias translates Shelley's historical deterministic fixture
// into dago's canonical write_file/edit_file calls. It is not part of the
// production model tool surface: real models receive only the canonical tools.
type predictablePatchInput struct {
	Path    string                      `json:"path"`
	Patches []predictablePatchOperation `json:"patches"`
}

type predictablePatchOperation struct {
	Operation string `json:"operation"`
	OldText   string `json:"oldText,omitempty"`
	NewText   string `json:"newText,omitempty"`
}

func predictablePatchAlias(files dbackend.Backend, write, edit datool.Tool) datool.Tool {
	return datool.MustNew("patch", "Compatibility adapter for deterministic Shelley fixtures.", func(ctx context.Context, input predictablePatchInput) (datool.Result, error) {
		runtime, _ := datool.RuntimeFromContext(ctx)
		if input.Path == "" || len(input.Patches) == 0 {
			return datool.Result{}, fmt.Errorf("patch requires path and at least one operation")
		}
		before := predictableFileContents(ctx, files, input.Path)
		var content []dmessage.ContentBlock
		for _, patch := range input.Patches {
			var executable datool.Tool
			var arguments []byte
			switch patch.Operation {
			case "overwrite":
				executable = write
				arguments, _ = json.Marshal(map[string]any{"file_path": input.Path, "content": patch.NewText})
			case "replace":
				if edit == nil {
					return datool.Result{}, fmt.Errorf("patch replace requires edit_file")
				}
				executable = edit
				arguments, _ = json.Marshal(map[string]any{"file_path": input.Path, "old_string": patch.OldText, "new_string": patch.NewText, "replace_all": false})
			default:
				return datool.Result{}, fmt.Errorf("unsupported patch operation %q", patch.Operation)
			}
			output, err := executable.Execute(ctx, arguments, runtime)
			if err != nil {
				return datool.Result{}, err
			}
			content = append(content, output.Content...)
		}
		after, err := readPredictableFileContents(ctx, files, input.Path)
		if err != nil {
			return datool.Result{}, err
		}
		artifact, err := json.Marshal(map[string]any{"path": input.Path, "oldContent": before, "newContent": after})
		if err != nil {
			return datool.Result{}, err
		}
		return datool.Result{Content: content, Artifact: artifact}, nil
	})
}

func predictableFileContents(ctx context.Context, files dbackend.Backend, path string) string {
	content, _ := readPredictableFileContents(ctx, files, path)
	return content
}

func readPredictableFileContents(ctx context.Context, files dbackend.Backend, path string) (string, error) {
	result, err := files.Read(ctx, path, 0, 1_000_000)
	if err != nil {
		return "", err
	}
	if result.Data == nil {
		return "", fmt.Errorf("read patched file %q returned no data", path)
	}
	if result.Data.Encoding != dbackend.EncodingUTF8 {
		return "", fmt.Errorf("patched file %q is not UTF-8 text", path)
	}
	return result.Data.Content, nil
}

// ResolveCancellation durably appends cancellation messages and clears any
// pending model/tool tasks in the dago graph before a later turn resumes.
func (l *Loop) ResolveCancellation(ctx context.Context, messages ...llm.Message) error {
	l.executionMu.Lock()
	defer l.executionMu.Unlock()
	if l.runtime == nil {
		return nil
	}
	converted, err := messagesToNative(messages)
	if err != nil {
		return err
	}
	if _, err := l.runtime.Cancel(ctx, dagent.Input{
		Config: l.checkpointConfig(), Messages: converted,
	}); err != nil {
		return fmt.Errorf("resolve dago Shelley cancellation: %w", err)
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
	var history []llm.Message
	if !seeded {
		history = append([]llm.Message(nil), l.history...)
	}
	l.mu.Unlock()
	if !seeded {
		tuple, err := l.saver.GetTuple(ctx, l.checkpointConfig())
		if err != nil {
			return nil, fmt.Errorf("load dago Shelley checkpoint: %w", err)
		}
		if tuple != nil {
			l.mu.Lock()
			l.runtimeSeeded = true
			l.mu.Unlock()
			return pending, nil
		}
		// dago's PatchToolCallsMiddleware owns canonical dangling-call repair.
		return history, nil
	}
	return pending, nil
}

func (l *Loop) checkpointConfig() dacheckpoint.Config {
	return dacheckpoint.Config{ThreadID: l.threadID, Namespace: l.namespace}
}

func (l *Loop) runtimeMiddleware(files dbackend.Backend) dagent.Middleware {
	return dagent.Middleware{
		Name: "shelley_runtime",
		BeforeModel: func(ctx context.Context, _ dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			var additions []llm.Message
			if l.injectMessages != nil {
				additions = append(additions, l.injectMessages(ctx)...)
			}
			l.mu.Lock()
			if len(l.messageQueue) > 0 {
				additions = append(additions, l.messageQueue...)
				l.messageQueue = nil
				l.logger.Info("processing user interruption during dago tool execution")
			}
			l.history = append(l.history, additions...)
			l.mu.Unlock()
			if len(additions) == 0 {
				return nil, nil
			}
			converted, err := messagesToNative(additions)
			if err != nil {
				return nil, err
			}
			return dastate.Values{dagent.MessagesKey: converted}, nil
		},
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			level := l.getThinkingLevel()
			if request.Model.Profile().SupportsReasoning && level != llm.ThinkingLevelDefault {
				request.Reasoning = &damodel.Reasoning{}
				if level != llm.ThinkingLevelOff {
					request.Reasoning.Effort = level.ThinkingEffort()
					request.Reasoning.Summary = "auto"
				}
			}
			requestCtx, cancel := context.WithTimeout(ctx, maxTurnDuration)
			defer cancel()
			if l.recordWarning != nil {
				requestCtx = damodel.WithRetryObserver(requestCtx, func(observerCtx context.Context, event damodel.RetryEvent) {
					warning := llm.RetryEvent{
						Attempt: event.Attempt, Sleep: event.Delay, Err: event.Err, Status: event.Status,
						Provider: event.Provider, Model: event.Model,
					}
					if warningErr := l.recordWarning(observerCtx, llm.FormatRetryEvent(warning)); warningErr != nil {
						l.logger.Error("failed to record retry warning", "error", warningErr)
					}
				})
			}
			return next(requestCtx, request)
		},
		AfterModel: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			messages, _ := values[dagent.MessagesKey].([]dmessage.Message)
			if len(messages) == 0 {
				return nil, nil
			}
			last := messages[len(messages)-1]
			response, ok, err := responseFromNative(last)
			if err != nil || !ok {
				return nil, err
			}
			if response.StopReason == llm.StopReasonMaxTokens || response.StopReason == llm.StopReasonRefusal {
				return dastate.Values{dagent.MessagesKey: []dmessage.Message{dmessage.Remove(last.ID)}}, nil
			}
			return nil, nil
		},
		WrapToolCall: func(ctx context.Context, request dagent.ToolCallRequest, next dagent.ToolHandler) (dagent.ToolCallResponse, error) {
			toolCtx := ctx
			toolCtx = llm.WithModelProfile(toolCtx, l.model.Profile())
			var usage llmhttp.UsageAccumulator
			toolCtx = llmhttp.WithUsageCollector(toolCtx, usage.Collect)
			changePath := dagoFileChangePath(request.Call)
			before := ""
			if changePath != "" {
				before = predictableFileContents(toolCtx, files, changePath)
			}
			started := time.Now()
			response, err := next(toolCtx, request)
			finished := time.Now()
			if err != nil {
				return response, err
			}
			var existing toolArtifactEnvelope
			if len(response.Result.Artifact) > 0 && json.Unmarshal(response.Result.Artifact, &existing) == nil && existing.Kind == shelleyToolArtifact {
				return response, nil
			}
			var display any
			if changePath != "" {
				after := ""
				if request.Call.Name != "delete" {
					after, err = readPredictableFileContents(toolCtx, files, changePath)
					if err != nil {
						return response, fmt.Errorf("read %s display data: %w", request.Call.Name, err)
					}
				}
				display = map[string]any{"path": changePath, "oldContent": before, "newContent": after}
			} else if len(response.Result.Artifact) > 0 {
				display = append(json.RawMessage(nil), response.Result.Artifact...)
			}
			exact := llm.Content{
				Type: llm.ContentTypeToolResult, ToolUseID: request.Call.ID,
				ToolResult:       contentFromNative(response.Result.Content),
				ToolUseStartTime: &started, ToolUseEndTime: &finished, Display: display,
			}
			artifact, encodeErr := json.Marshal(toolArtifactEnvelope{
				Version: 1, Kind: shelleyToolArtifact, Content: exact,
				OtherUsage: append(purposedUsageFromNative(response.Result.OtherUsage), usage.Take()...),
			})
			if encodeErr != nil {
				return dagent.ToolCallResponse{}, encodeErr
			}
			response.Result.Artifact = artifact
			return response, nil
		},
	}
}

func dagoFileChangePath(call dmessage.ToolCall) string {
	if call.Name != "write_file" && call.Name != "edit_file" && call.Name != "delete" {
		return ""
	}
	var input struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(call.Arguments, &input) != nil {
		return ""
	}
	return input.FilePath
}

func (l *Loop) projectRuntimeUpdate(ctx context.Context, node string, update dastate.Values) error {
	if node != "model" && node != "tools" {
		return nil
	}
	value, ok := update[dagent.MessagesKey]
	if !ok {
		return nil
	}
	messages, ok := value.([]dmessage.Message)
	if !ok {
		return fmt.Errorf("dago Shelley message update has type %T", value)
	}
	if node == "model" {
		l.flushStream()
		for _, item := range messages {
			response, found, err := responseFromNative(item)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if response.Model == "" && l.modelID != "" {
				response.Model = l.modelID
				response.Usage.Model = response.Model
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
	toolMessage := llm.Message{Role: llm.MessageRoleUser}
	var otherUsage []llm.PurposedUsage
	for _, item := range messages {
		if item.Role != dmessage.RoleTool {
			continue
		}
		content, usage, err := toolResultFromNative(item)
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

func runtimeSystemPrompt(items []llm.SystemContent) string {
	return strings.TrimSpace(joinSystem(items))
}

func (l *Loop) emitToken(chunk damodel.Chunk) {
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

func (l *Loop) recordRuntimeError(ctx context.Context, err error, trace *llmhttp.RequestTrace) error {
	displayErr := err
	if text := strings.TrimPrefix(err.Error(), `execute node "model": `); text != err.Error() {
		displayErr = errors.New(text)
	}
	message := llm.Message{
		Role: llm.MessageRoleAssistant, Content: llm.TextContent(userFacingLLMError(displayErr, trace)),
		EndOfTurn: true, ErrorType: llm.ErrorTypeLLMRequest, ErrorRetryable: IsRetryableLLMError(err),
	}
	if recordErr := l.recordMessage(ctx, message, llm.Usage{}, nil); recordErr != nil {
		l.logger.Error("failed to record dago runtime error", "error", recordErr)
	}
	return fmt.Errorf("LLM request failed: %w", err)
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
	var reporter damodel.RetryReporter
	if errors.As(err, &reporter) {
		return reporter.RetryEvent(0, 0).Retryable
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
