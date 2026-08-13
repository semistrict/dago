package dagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
	graph "github.com/semistrict/dago/internal/graph"
)

// Options configures a generic tool-calling agent.
type Options struct {
	Name             string
	Model            damodel.Chat
	Tools            []datool.Tool
	SystemPrompt     string
	SystemMessage    *damessage.Message
	Middleware       []Middleware
	StateFields      map[string]StateField
	StructuredOutput *StructuredOutput
	Saver            dacheckpoint.Saver
	// RetainThreadState keeps active thread state in memory between invocations
	// while continuing to persist checkpoints. Disable it when another process or
	// agent instance may update the same thread concurrently.
	RetainThreadState bool
	Store             dastore.Store
	Cache             dacache.Cache
	Deps              any
	RecursionLimit    int
	MaxConcurrency    int
	FailOnToolError   bool
	Metadata          map[string]json.RawMessage
	Tags              []string
	Debug             bool
}

// Agent is a compiled provider-neutral model/tool graph.
type Agent struct {
	graph   *graph.Compiled
	saver   dacheckpoint.Saver
	private map[string]bool
}

// Input starts or resumes one agent thread.
type Input struct {
	Config   dacheckpoint.Config
	Messages []damessage.Message
	State    dastate.Values
	Resume   any
	// Deps overrides construction-time dependencies for this invocation and is
	// inherited by inline subagents. A nil value uses Options.Deps.
	Deps any
	// Configurable contains immutable, runtime-only application settings. It is
	// available to middleware and tools but is never persisted.
	Configurable       map[string]any
	SkipValueEvents    bool
	DiscardResultState bool
}

// Result is the complete visible agent state.
type Result struct {
	Config     dacheckpoint.Config
	Messages   []damessage.Message
	Structured json.RawMessage
	State      dastate.Values
	Interrupts []Interrupt
	Handoff    *datool.Handoff
	Steps      int
}

// Interrupt is a language-neutral pause request.
type Interrupt struct {
	ID    string
	Value any
}

// New validates options and compiles the model/tool graph.
func New(options Options) (*Agent, error) {
	if options.Model == nil {
		return nil, fmt.Errorf("create agent: model is required")
	}
	if options.SystemPrompt != "" && options.SystemMessage != nil {
		return nil, fmt.Errorf("create agent: system prompt and system message are mutually exclusive")
	}
	if options.SystemMessage != nil {
		if options.SystemMessage.Role != damessage.RoleSystem {
			return nil, fmt.Errorf("create agent: system message role must be system")
		}
		copy := options.SystemMessage.Clone()
		options.SystemMessage = &copy
	}
	options.Metadata = cloneRawMap(options.Metadata)
	options.Tags = append([]string(nil), options.Tags...)
	structuredOutput, err := prepareStructuredOutput(options.StructuredOutput)
	if err != nil {
		return nil, err
	}
	options.StructuredOutput = structuredOutput
	if options.RecursionLimit <= 0 {
		options.RecursionLimit = 9999
	}
	if options.MaxConcurrency <= 0 {
		options.MaxConcurrency = 16
	}
	fields, err := resolveFields(options.StateFields, options.Middleware)
	if err != nil {
		return nil, err
	}
	tools, err := resolveTools(options.Tools, options.Middleware)
	if err != nil {
		return nil, err
	}
	names := map[string]struct{}{}
	for _, middleware := range options.Middleware {
		if middleware.Name == "" {
			return nil, fmt.Errorf("agent middleware name is required")
		}
		if _, duplicate := names[middleware.Name]; duplicate {
			return nil, fmt.Errorf("%w %q", ErrDuplicateMiddleware, middleware.Name)
		}
		names[middleware.Name] = struct{}{}
	}

	schema := graph.Schema{Fields: map[string]graph.Field{}, AllowUnknown: true}
	for name, field := range fields {
		switch field.Kind {
		case FieldLast:
			schema.Fields[name] = graph.LastValue(field.Clone)
		case FieldAggregate:
			schema.Fields[name] = graph.Aggregate(field.Initial, field.Reduce, field.Clone)
		case FieldDelta:
			reducer := field.Reduce
			if options.RetainThreadState && name == MessagesKey {
				reducer = reduceMessagesOwned
			}
			graphField := graph.Delta(field.Initial, reducer, field.Clone, field.SnapshotFrequency)
			if options.RetainThreadState && name == MessagesKey {
				graphField = graphField.WithReadView(identityClone).WithOwnedReducerInput()
			}
			schema.Fields[name] = graphField
		case FieldEphemeral:
			schema.Fields[name] = graph.Ephemeral(field.Clone)
		}
	}
	schema.Fields[toolDirectKey] = graph.Ephemeral(identityClone)
	schema.Fields[structuredRetryKey] = graph.Ephemeral(identityClone)

	compiled := &compiler{options: options, tools: tools, fields: fields}
	builder := graph.NewBuilder(schema)
	if err := compiled.addNodes(builder); err != nil {
		return nil, err
	}
	runtimeGraph, err := builder.Compile(graph.CompileOptions{
		Saver: options.Saver, Store: options.Store, Cache: options.Cache, Deps: options.Deps,
		RetainThreadState: options.RetainThreadState,
		RecursionLimit:    options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
		Writer: debugGraphWriter(options.Debug),
	})
	if err != nil {
		return nil, err
	}
	private := map[string]bool{}
	for name, field := range fields {
		if field.Private {
			private[name] = true
		}
	}
	return &Agent{graph: runtimeGraph, saver: options.Saver, private: private}, nil
}

type debugEventWriter struct{}

func debugGraphWriter(enabled bool) graph.EventWriter {
	if !enabled {
		return nil
	}
	return debugEventWriter{}
}

func (debugEventWriter) Write(ctx context.Context, event graph.Event) error {
	slog.DebugContext(ctx, "agent graph event", "mode", event.Mode, "step", event.Step, "node", event.Node, "task_id", event.TaskID)
	return nil
}

func (agent *Agent) Invoke(ctx context.Context, input Input) (Result, error) {
	if input.Config.ThreadID == "" {
		input.Config.ThreadID = "default"
	}
	values := input.State.Clone()
	if values == nil {
		values = dastate.Values{}
	}
	if len(input.Messages) > 0 {
		values[MessagesKey] = damessage.EnsureIDs(input.Messages)
	}
	ensureMessageIDsInValues(values)
	execution, err := agent.graph.Invoke(ctx, graph.Invocation{
		Config: input.Config, State: values, Resume: input.Resume, Deps: input.Deps,
		Configurable:    cloneConfigurable(input.Configurable),
		SkipValueEvents: input.SkipValueEvents,
	})
	if err != nil {
		return Result{}, err
	}
	return resultFromExecution(execution, agent.private, input.DiscardResultState)
}

// Cancel durably appends final messages/state and clears pending graph tasks.
// It must be called after the active invocation has observed cancellation.
func (agent *Agent) Cancel(ctx context.Context, input Input) (Result, error) {
	if input.Config.ThreadID == "" {
		input.Config.ThreadID = "default"
	}
	values := input.State.Clone()
	if values == nil {
		values = dastate.Values{}
	}
	if len(input.Messages) > 0 {
		values[MessagesKey] = damessage.EnsureIDs(input.Messages)
	}
	ensureMessageIDsInValues(values)
	execution, err := agent.graph.Cancel(ctx, graph.Invocation{
		Config: input.Config, State: values, Deps: input.Deps, Configurable: cloneConfigurable(input.Configurable),
	})
	if err != nil {
		return Result{}, err
	}
	return resultFromExecution(execution, agent.private, false)
}

func resultFromExecution(execution graph.Execution, private map[string]bool, discardState bool) (Result, error) {
	if discardState {
		result := Result{Config: execution.Config, Steps: execution.Steps}
		for _, interrupt := range execution.Interrupts {
			result.Interrupts = append(result.Interrupts, Interrupt{ID: interrupt.ID, Value: interrupt.Value})
		}
		return result, nil
	}
	messages, err := messagesFrom(execution.State[MessagesKey])
	if err != nil {
		return Result{}, err
	}
	result := Result{Config: execution.Config, Messages: messages, State: publicState(execution.State, private), Steps: execution.Steps}
	if raw, ok := execution.State[StructuredResponseKey].(json.RawMessage); ok {
		result.Structured = append(json.RawMessage(nil), raw...)
	} else if value, exists := execution.State[StructuredResponseKey]; exists {
		result.Structured, err = json.Marshal(value)
		if err != nil {
			return Result{}, err
		}
	}
	for _, interrupt := range execution.Interrupts {
		result.Interrupts = append(result.Interrupts, Interrupt{ID: interrupt.ID, Value: interrupt.Value})
	}
	result.Handoff = terminalHandoff(messages)
	return result, nil
}

const handoffMetadataKey = "dago_handoff"

func terminalHandoff(messages []damessage.Message) *datool.Handoff {
	for index := len(messages) - 1; index >= 0; index-- {
		item := messages[index]
		if item.Role == damessage.RoleHuman {
			return nil
		}
		if item.Role != damessage.RoleTool || len(item.ResponseMetadata[handoffMetadataKey]) == 0 {
			continue
		}
		var handoff datool.Handoff
		if json.Unmarshal(item.ResponseMetadata[handoffMetadataKey], &handoff) == nil && handoff.Destination != "" {
			return &handoff
		}
	}
	return nil
}

func publicState(values dastate.Values, private map[string]bool) dastate.Values {
	result := values.Clone()
	for key := range private {
		delete(result, key)
	}
	return result
}

type compiler struct {
	options Options
	tools   map[string]datool.Tool
	fields  map[string]StateField
}

func (compiler *compiler) addNodes(builder *graph.Builder) error {
	if err := builder.AddNode("before_agent", compiler.beforeAgent); err != nil {
		return err
	}
	if err := builder.AddNode("model", compiler.model); err != nil {
		return err
	}
	if err := builder.AddNode("tools", compiler.executeTools); err != nil {
		return err
	}
	if err := builder.AddNode("after_agent", compiler.afterAgent); err != nil {
		return err
	}
	for _, edge := range [][2]string{{graph.Start, "before_agent"}, {"before_agent", "model"}, {"after_agent", graph.End}} {
		if err := builder.AddEdge(edge[0], edge[1]); err != nil {
			return err
		}
	}
	if err := builder.AddConditional("model", compiler.routeModel); err != nil {
		return err
	}
	return builder.AddConditional("tools", compiler.routeTools)
}

func (compiler *compiler) beforeAgent(ctx context.Context, values dastate.Values, runtime graph.Runtime) (graph.Command, error) {
	update := dastate.Values{}
	current := values.Clone()
	for _, middleware := range compiler.options.Middleware {
		if middleware.BeforeAgent == nil {
			continue
		}
		result, err := middleware.BeforeAgent(ctx, current.Clone(), convertRuntime(runtime))
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q before agent: %w", middleware.Name, err)
		}
		compiler.mergeUpdate(current, update, result)
	}
	ensureMessageIDsInValues(update)
	return graph.Command{Update: update}, nil
}

func (compiler *compiler) afterAgent(ctx context.Context, values dastate.Values, runtime graph.Runtime) (graph.Command, error) {
	update := dastate.Values{}
	current := values.Clone()
	for index := len(compiler.options.Middleware) - 1; index >= 0; index-- {
		middleware := compiler.options.Middleware[index]
		if middleware.AfterAgent == nil {
			continue
		}
		result, err := middleware.AfterAgent(ctx, current.Clone(), convertRuntime(runtime))
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q after agent: %w", middleware.Name, err)
		}
		compiler.mergeUpdate(current, update, result)
	}
	command := graph.Command{Update: update}
	if destination, exists := update[jumpToKey]; exists {
		name, ok := destination.(string)
		if !ok || name != "model" {
			return graph.Command{}, fmt.Errorf("after-agent jump destination must be model, got %v", destination)
		}
		delete(update, jumpToKey)
		command.Goto = []string{name}
	}
	ensureMessageIDsInValues(update)
	return command, nil
}

func (compiler *compiler) model(ctx context.Context, values dastate.Values, runtime graph.Runtime) (graph.Command, error) {
	current := values.Clone()
	update := dastate.Values{}
	for _, middleware := range compiler.options.Middleware {
		if middleware.BeforeModel == nil {
			continue
		}
		result, err := middleware.BeforeModel(ctx, current.Clone(), convertRuntime(runtime))
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q before model: %w", middleware.Name, err)
		}
		compiler.mergeUpdate(current, update, result)
	}
	var messages []damessage.Message
	var err error
	if compiler.options.RetainThreadState {
		messages, err = messagesView(current[MessagesKey])
	} else {
		messages, err = messagesFrom(current[MessagesKey])
	}
	if err != nil {
		return graph.Command{}, err
	}
	request := ModelRequest{
		Model: compiler.options.Model, Messages: messages, Tools: toolsSlice(compiler.tools),
		State: current.Clone(), Runtime: convertRuntime(runtime),
		MessagesReadOnly:   compiler.options.RetainThreadState,
		InvocationMetadata: cloneRawMap(compiler.options.Metadata), InvocationTags: append([]string(nil), compiler.options.Tags...),
	}
	if compiler.options.SystemMessage != nil {
		system := compiler.options.SystemMessage.Clone()
		request.SystemMessage = &system
	} else if compiler.options.SystemPrompt != "" {
		system := damessage.System(compiler.options.SystemPrompt)
		request.SystemMessage = &system
	}
	configureStructuredRequest(&request, compiler.options.StructuredOutput)
	handler := compiler.modelHandler()
	for index := len(compiler.options.Middleware) - 1; index >= 0; index-- {
		wrapper := compiler.options.Middleware[index].WrapModelCall
		if wrapper == nil {
			continue
		}
		next := handler
		handler = func(ctx context.Context, request ModelRequest) (ModelResponse, error) {
			return wrapper(ctx, request, next)
		}
	}
	response, err := handler(ctx, request)
	if err != nil {
		return graph.Command{}, err
	}
	if len(response.Messages) == 0 {
		return graph.Command{}, fmt.Errorf("%w: model returned no messages", ErrInvalidModelOutput)
	}
	if compiler.hasAfterModelHook() {
		compiler.mergeUpdate(current, update, dastate.Values{MessagesKey: response.Messages})
		compiler.mergeUpdate(current, update, response.Update)
	} else {
		mergePendingUpdate(update, dastate.Values{MessagesKey: response.Messages})
		mergePendingUpdate(update, response.Update)
	}
	if len(response.Structured) > 0 {
		update[StructuredResponseKey] = append(json.RawMessage(nil), response.Structured...)
	}
	for index := len(compiler.options.Middleware) - 1; index >= 0; index-- {
		middleware := compiler.options.Middleware[index]
		if middleware.AfterModel == nil {
			continue
		}
		result, err := middleware.AfterModel(ctx, current.Clone(), convertRuntime(runtime))
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q after model: %w", middleware.Name, err)
		}
		compiler.mergeUpdate(current, update, result)
	}
	ensureMessageIDsInValues(update)
	return graph.Command{Update: update}, nil
}

func (compiler *compiler) hasAfterModelHook() bool {
	for _, middleware := range compiler.options.Middleware {
		if middleware.AfterModel != nil {
			return true
		}
	}
	return false
}

func (compiler *compiler) modelHandler() ModelHandler {
	return func(ctx context.Context, request ModelRequest) (ModelResponse, error) {
		definitions := make([]datool.Definition, 0, len(request.Tools))
		for _, executable := range request.Tools {
			definition := executable.Definition()
			if err := definition.Validate(); err != nil {
				return ModelResponse{}, err
			}
			definitions = append(definitions, definition)
		}
		messages := request.Messages
		if !compiler.options.RetainThreadState {
			messages = cloneMessages(request.Messages)
		}
		var providerSystem *damessage.Message
		if request.SystemMessage != nil && compiler.options.RetainThreadState && request.Model.Profile().SupportsSeparateSystemMessage {
			system := request.SystemMessage.Clone()
			providerSystem = &system
		} else if request.SystemMessage != nil {
			messages = append([]damessage.Message{request.SystemMessage.Clone()}, messages...)
		}
		chat := request.Model
		if binder, ok := chat.(damodel.Binder); ok && len(definitions) > 0 {
			bound, err := binder.BindTools(definitions)
			if err != nil {
				return ModelResponse{}, err
			}
			chat = bound
		}
		providerRequest := damodel.Request{
			Messages: messages, SystemMessage: providerSystem, Tools: definitions, ToolChoice: request.ToolChoice,
			ResponseFormat: request.ResponseFormat, PromptCache: request.PromptCache,
			Reasoning: request.Reasoning,
			Metadata:  cloneRawMap(request.Metadata), Tags: append([]string(nil), request.Tags...),
		}
		var response damodel.Response
		var err error
		if request.Runtime.Writer != nil && chat.Profile().NativeStreaming {
			response, err = invokeModelStream(ctx, chat, providerRequest, request.Runtime.Writer)
		} else {
			response, err = chat.Invoke(ctx, providerRequest)
		}
		if err != nil {
			var reporter damodel.RetryReporter
			if errors.As(err, &reporter) {
				event := reporter.RetryEvent(0, 0)
				if event.Retryable {
					damodel.ReportRetry(ctx, event)
				}
			}
			return ModelResponse{}, err
		}
		if response.Message.Role != damessage.RoleAssistant {
			return ModelResponse{}, fmt.Errorf("%w: response role is %q", ErrInvalidModelOutput, response.Message.Role)
		}
		if response.Message.ResponseMetadata == nil {
			response.Message.ResponseMetadata = map[string]json.RawMessage{}
		}
		response.Message.ResponseMetadata[damodel.ResponseMetadataKey] = json.RawMessage(`true`)
		if compiler.options.Name != "" && response.Message.Name == "" {
			response.Message.Name = compiler.options.Name
		}
		return handleStructuredResponse(response, compiler.options.StructuredOutput)
	}
}

func invokeModelStream(ctx context.Context, chat damodel.Chat, request damodel.Request, writer EventWriter) (damodel.Response, error) {
	stream, err := chat.Stream(ctx, request)
	if err != nil {
		return damodel.Response{}, err
	}
	response := damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant}}
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			return damodel.Response{}, nextErr
		}
		encoded, encodeErr := json.Marshal(streamEnvelope{Version: 1, Kind: "token", Chunk: chunk})
		if encodeErr != nil {
			return damodel.Response{}, encodeErr
		}
		if err := writer.Write(ctx, encoded); err != nil {
			return damodel.Response{}, err
		}
		mergeModelChunk(&response, chunk)
	}
	return response, nil
}

type streamEnvelope struct {
	Version      int              `json:"version"`
	Kind         string           `json:"kind"`
	Chunk        damodel.Chunk    `json:"chunk,omitempty"`
	Child        *ChildEvent      `json:"child,omitempty"`
	ToolProgress *datool.Progress `json:"tool_progress,omitempty"`
}

func mergeModelChunk(response *damodel.Response, chunk damodel.Chunk) {
	delta := chunk.MessageDelta
	if response.Message.ID == "" {
		response.Message.ID = delta.ID
	}
	if response.Message.Name == "" {
		response.Message.Name = delta.Name
	}
	if len(delta.Metadata) > 0 {
		if response.Message.Metadata == nil {
			response.Message.Metadata = map[string]json.RawMessage{}
		}
		for key, value := range delta.Metadata {
			response.Message.Metadata[key] = append(json.RawMessage(nil), value...)
		}
	}
	if len(delta.ResponseMetadata) > 0 {
		if response.Message.ResponseMetadata == nil {
			response.Message.ResponseMetadata = map[string]json.RawMessage{}
		}
		for key, value := range delta.ResponseMetadata {
			response.Message.ResponseMetadata[key] = append(json.RawMessage(nil), value...)
		}
	}
	if len(delta.Artifact) > 0 {
		response.Message.Artifact = append(json.RawMessage(nil), delta.Artifact...)
	}
	for _, block := range delta.Content {
		textTarget := -1
		if block.Type == damessage.BlockText && len(response.Message.Content) > 0 {
			if response.Message.Content[len(response.Message.Content)-1].Type == damessage.BlockText {
				textTarget = len(response.Message.Content) - 1
			} else if block.Text == "" && (len(block.Citations) > 0 || len(block.Extra) > 0) {
				for index := len(response.Message.Content) - 1; index >= 0; index-- {
					if response.Message.Content[index].Type == damessage.BlockText {
						textTarget = index
						break
					}
				}
			}
		}
		if textTarget >= 0 {
			current := &response.Message.Content[textTarget]
			current.Text += block.Text
			current.Citations = append(current.Citations, block.Citations...)
			if current.ID == "" {
				current.ID = block.ID
			}
			if len(block.Extra) > 0 {
				if current.Extra == nil {
					current.Extra = map[string]json.RawMessage{}
				}
				for key, value := range block.Extra {
					current.Extra[key] = append(json.RawMessage(nil), value...)
				}
			}
			continue
		}
		if block.Type == damessage.BlockReasoning {
			merged := false
			for index := len(response.Message.Content) - 1; index >= 0; index-- {
				current := &response.Message.Content[index]
				if current.Type != damessage.BlockReasoning || (block.ID != "" && current.ID != "" && current.ID != block.ID) {
					continue
				}
				current.Reasoning += block.Reasoning
				if current.ID == "" {
					current.ID = block.ID
				}
				if len(block.Extra) > 0 {
					if current.Extra == nil {
						current.Extra = map[string]json.RawMessage{}
					}
					for key, value := range block.Extra {
						current.Extra[key] = append(json.RawMessage(nil), value...)
					}
				}
				merged = true
				break
			}
			if merged {
				continue
			}
		}
		response.Message.Content = append(response.Message.Content, block)
	}
	response.Message.ToolCalls = append(response.Message.ToolCalls, delta.ToolCalls...)
	response.Message.InvalidToolCalls = append(response.Message.InvalidToolCalls, delta.InvalidToolCalls...)
	if delta.Usage != nil {
		usage := *delta.Usage
		response.Message.Usage = &usage
	}
	if len(chunk.Structured) > 0 {
		response.Structured = append(json.RawMessage(nil), chunk.Structured...)
	}
}

func (compiler *compiler) executeTools(ctx context.Context, values dastate.Values, runtime graph.Runtime) (graph.Command, error) {
	messages, err := messagesFrom(values[MessagesKey])
	if err != nil {
		return graph.Command{}, err
	}
	if len(messages) == 0 {
		return graph.Command{}, fmt.Errorf("execute tools: message history is empty")
	}
	assistantIndex := len(messages) - 1
	for assistantIndex >= 0 && (messages[assistantIndex].Role != damessage.RoleAssistant || len(messages[assistantIndex].ToolCalls) == 0) {
		assistantIndex--
	}
	if assistantIndex < 0 {
		return graph.Command{}, fmt.Errorf("execute tools: last message has no tool calls")
	}
	completed := map[string]bool{}
	for _, item := range messages[assistantIndex+1:] {
		if item.Role == damessage.RoleTool && item.ToolCallID != "" {
			completed[item.ToolCallID] = true
		}
	}
	calls := make([]damessage.ToolCall, 0, len(messages[assistantIndex].ToolCalls))
	for _, call := range messages[assistantIndex].ToolCalls {
		if !completed[call.ID] {
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return graph.Command{}, nil
	}
	prefilled := []damessage.Message{}
	resumeConsumed := false
	for _, middleware := range compiler.options.Middleware {
		if middleware.BeforeTools == nil {
			continue
		}
		response, err := middleware.BeforeTools(ctx, ToolBatchRequest{
			Calls: append([]damessage.ToolCall(nil), calls...), Tools: cloneToolMap(compiler.tools),
			State: values.Clone(), Runtime: convertRuntime(runtime),
		})
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q before tools: %w", middleware.Name, err)
		}
		if response.Interrupt != nil {
			return graph.Command{Interrupt: &graph.Interrupt{ID: response.Interrupt.ID, Value: response.Interrupt.Value}}, nil
		}
		if response.Calls != nil {
			calls = append([]damessage.ToolCall(nil), response.Calls...)
		}
		resumeConsumed = resumeConsumed || response.ResumeConsumed
		prefilled = append(prefilled, cloneMessages(response.Messages)...)
	}
	type outcome struct {
		message   damessage.Message
		update    dastate.Values
		direct    bool
		handoff   *datool.Handoff
		interrupt *Interrupt
		err       error
	}
	results := make([]outcome, len(calls))
	semaphore := make(chan struct{}, compiler.options.MaxConcurrency)
	var wait sync.WaitGroup
	for index, call := range calls {
		index, call := index, call
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index].err = ctx.Err()
				return
			}
			toolRuntime := runtime
			if resumeConsumed {
				toolRuntime.Resume = nil
			}
			results[index] = compiler.executeTool(ctx, call, values, toolRuntime)
		}()
	}
	wait.Wait()
	var pendingInterrupt *Interrupt
	for _, result := range results {
		if result.err != nil {
			return graph.Command{}, result.err
		}
		if result.interrupt == nil {
			continue
		}
		if pendingInterrupt == nil {
			interrupt := *result.interrupt
			pendingInterrupt = &interrupt
			continue
		}
		if pendingInterrupt.ID != result.interrupt.ID {
			return graph.Command{}, fmt.Errorf("tools interrupted with incompatible ids %q and %q", pendingInterrupt.ID, result.interrupt.ID)
		}
		current, currentOK := pendingInterrupt.Value.([]any)
		additional, additionalOK := result.interrupt.Value.([]any)
		if !currentOK || !additionalOK {
			return graph.Command{}, fmt.Errorf("multiple tool interrupts %q cannot be combined", pendingInterrupt.ID)
		}
		pendingInterrupt.Value = append(current, additional...)
	}
	update := dastate.Values{}
	var handoff *datool.Handoff
	byCallID := make(map[string]damessage.Message, len(prefilled)+len(results))
	var unassociated []damessage.Message
	for _, item := range messages[assistantIndex+1:] {
		if item.Role != damessage.RoleTool {
			continue
		}
		if item.ToolCallID == "" {
			unassociated = append(unassociated, item)
		} else {
			byCallID[item.ToolCallID] = item
		}
	}
	for _, item := range prefilled {
		if item.ToolCallID == "" {
			unassociated = append(unassociated, item)
		} else {
			byCallID[item.ToolCallID] = item
		}
	}
	for _, result := range results {
		for key, value := range result.update {
			if previous, exists := update[key]; exists {
				field, reducible := compiler.fields[key]
				if !reducible || (field.Kind != FieldAggregate && field.Kind != FieldDelta) {
					return graph.Command{}, fmt.Errorf("%w for field %q", ErrConflictingUpdate, key)
				}
				if batch, ok := previous.(dastate.Batch); ok {
					batch.Values = append(batch.Values, value)
					update[key] = batch
				} else {
					update[key] = dastate.Batch{Values: []any{previous, value}}
				}
				continue
			}
			update[key] = value
		}
		if result.interrupt != nil {
			continue
		}
		byCallID[result.message.ToolCallID] = result.message
		if result.direct {
			update[toolDirectKey] = true
		}
		if result.handoff != nil {
			if handoff != nil {
				return graph.Command{}, fmt.Errorf("multiple tools requested parent handoffs to %q and %q", handoff.Destination, result.handoff.Destination)
			}
			value := *result.handoff
			handoff = &value
			update[toolDirectKey] = true
		}
	}
	toolMessages := make([]damessage.Message, 0, len(byCallID)+len(unassociated))
	for _, call := range messages[assistantIndex].ToolCalls {
		if item, ok := byCallID[call.ID]; ok {
			toolMessages = append(toolMessages, item)
			delete(byCallID, call.ID)
		}
	}
	toolMessages = append(toolMessages, unassociated...)
	if len(byCallID) > 0 {
		ids := make([]string, 0, len(byCallID))
		for id := range byCallID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			toolMessages = append(toolMessages, byCallID[id])
		}
	}
	if len(toolMessages) > 0 {
		if assistantIndex < len(messages)-1 {
			replacement := append(cloneMessages(messages[:assistantIndex+1]), toolMessages...)
			update[MessagesKey] = dastate.Overwrite{Value: replacement}
		} else {
			update[MessagesKey] = toolMessages
		}
	}
	command := graph.Command{Update: update}
	if pendingInterrupt != nil {
		command.Interrupt = &graph.Interrupt{ID: pendingInterrupt.ID, Value: pendingInterrupt.Value}
	}
	ensureMessageIDsInValues(update)
	return command, nil
}

func (compiler *compiler) executeTool(ctx context.Context, call damessage.ToolCall, values dastate.Values, runtime graph.Runtime) (result struct {
	message   damessage.Message
	update    dastate.Values
	direct    bool
	handoff   *datool.Handoff
	interrupt *Interrupt
	err       error
}) {
	executable := compiler.tools[call.Name]
	if executable == nil {
		if compiler.options.FailOnToolError {
			result.err = fmt.Errorf("%w %q", ErrUnknownTool, call.Name)
			return
		}
		names := make([]string, 0, len(compiler.tools))
		for name := range compiler.tools {
			names = append(names, name)
		}
		sort.Strings(names)
		result.message = damessage.Tool(call.ID, fmt.Sprintf("Error: %s is not a valid tool, try one of [%s].", call.Name, strings.Join(names, ", ")))
		result.message.Name = call.Name
		result.message.ToolStatus = damessage.ToolStatusError
		return
	}
	request := ToolCallRequest{Call: call, Tool: executable, State: values.Clone(), Runtime: convertRuntime(runtime)}
	handler := func(ctx context.Context, request ToolCallRequest) (ToolCallResponse, error) {
		output, err := request.Tool.Execute(ctx, request.Call.Arguments, datool.Runtime{
			CallID: request.Call.ID, TaskID: runtime.TaskID, ThreadID: runtime.Config.ThreadID,
			Namespace:    runtime.Config.Namespace,
			CheckpointID: runtime.Config.CheckpointID,
			Resume:       request.Runtime.Resume,
			State:        request.State, Store: runtime.Store,
			Stream: toolWriter{writer: runtime.Writer}, Deps: request.Runtime.Deps,
			Configurable: request.Runtime.Configurable,
		})
		return ToolCallResponse{Result: output}, err
	}
	for index := len(compiler.options.Middleware) - 1; index >= 0; index-- {
		wrapper := compiler.options.Middleware[index].WrapToolCall
		if wrapper == nil {
			continue
		}
		next := handler
		handler = func(ctx context.Context, request ToolCallRequest) (ToolCallResponse, error) {
			return wrapper(ctx, request, next)
		}
	}
	response, err := handler(ctx, request)
	if err != nil {
		if compiler.options.FailOnToolError || propagatesToolError(err) {
			result.err = err
			return
		}
		result.message = damessage.Tool(call.ID, err.Error())
		result.message.Name = call.Name
		result.message.ToolStatus = damessage.ToolStatusError
		return
	}
	if response.Result.Interrupt != nil {
		result.interrupt = &Interrupt{ID: response.Result.Interrupt.ID, Value: response.Result.Interrupt.Value}
		result.update = dastate.Values(response.Result.Update)
		return
	}
	if response.Call != nil {
		call = *response.Call
	}
	status := response.Result.Status
	if status == "" {
		status = damessage.ToolStatusSuccess
	}
	if status != damessage.ToolStatusSuccess && status != damessage.ToolStatusError {
		result.err = fmt.Errorf("tool %q returned invalid status %q", call.Name, status)
		return
	}
	result.message = damessage.Message{
		Role: damessage.RoleTool, Name: call.Name, ToolCallID: call.ID,
		ToolStatus: status, Content: response.Result.Content,
		Artifact: response.Result.Artifact, OtherUsage: response.Result.OtherUsage,
	}
	result.update = dastate.Values(response.Result.Update)
	result.direct = executable.Definition().Direct
	if response.Result.Handoff != nil {
		if strings.TrimSpace(response.Result.Handoff.Destination) == "" {
			result.err = fmt.Errorf("tool %q returned a handoff with an empty destination", call.Name)
			return
		}
		value := *response.Result.Handoff
		result.handoff = &value
		encoded, err := json.Marshal(value)
		if err != nil {
			result.err = fmt.Errorf("encode tool %q handoff: %w", call.Name, err)
			return
		}
		result.message.ResponseMetadata = map[string]json.RawMessage{handoffMetadataKey: encoded}
	}
	return
}

type propagatingToolError interface {
	PropagateToolError()
}

func propagatesToolError(err error) bool {
	var marker propagatingToolError
	return errors.As(err, &marker)
}

func (compiler *compiler) routeModel(_ context.Context, values dastate.Values) ([]string, error) {
	if retry, _ := values[structuredRetryKey].(bool); retry {
		return []string{"model"}, nil
	}
	if _, done := values[StructuredResponseKey]; done {
		return []string{"after_agent"}, nil
	}
	messages, err := messagesView(values[MessagesKey])
	if err != nil {
		return nil, err
	}
	if len(messages) > 0 && len(messages[len(messages)-1].ToolCalls) > 0 {
		return []string{"tools"}, nil
	}
	return []string{"after_agent"}, nil
}

func (compiler *compiler) routeTools(_ context.Context, values dastate.Values) ([]string, error) {
	if direct, _ := values[toolDirectKey].(bool); direct {
		return []string{"after_agent"}, nil
	}
	return []string{"model"}, nil
}

func resolveFields(base map[string]StateField, middleware []Middleware) (map[string]StateField, error) {
	result := map[string]StateField{
		MessagesKey: {
			Kind: FieldDelta, Contract: "dago.messages.delta.v1", SnapshotFrequency: 100,
			Initial: func() any { return []damessage.Message{} }, Reduce: reduceMessages, Clone: cloneMessageValue,
		},
		StructuredResponseKey: {Kind: FieldLast, Contract: "dago.structured.v1", Clone: identityClone},
	}
	merge := func(name string, field StateField, explicit bool) error {
		if err := field.validate(name); err != nil {
			return err
		}
		if current, exists := result[name]; exists {
			if current.Contract != field.Contract && !explicit {
				return fmt.Errorf("agent state field %q has incompatible contracts %q and %q", name, current.Contract, field.Contract)
			}
			field.Private = field.Private || current.Private
		}
		result[name] = field
		return nil
	}
	for _, item := range middleware {
		for name, field := range item.Fields {
			if err := merge(name, field, false); err != nil {
				return nil, err
			}
		}
	}
	for name, field := range base {
		if err := merge(name, field, true); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func resolveTools(base []datool.Tool, middleware []Middleware) (map[string]datool.Tool, error) {
	result := map[string]datool.Tool{}
	all := append([]datool.Tool(nil), base...)
	for _, item := range middleware {
		all = append(all, item.Tools...)
	}
	for _, executable := range all {
		if executable == nil {
			return nil, fmt.Errorf("agent tool is nil")
		}
		definition := executable.Definition()
		if err := definition.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := result[definition.Name]; duplicate {
			return nil, fmt.Errorf("%w %q", ErrDuplicateTool, definition.Name)
		}
		result[definition.Name] = executable
	}
	return result, nil
}

func toolsSlice(values map[string]datool.Tool) []datool.Tool {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]datool.Tool, 0, len(names))
	for _, name := range names {
		result = append(result, values[name])
	}
	return result
}

func cloneToolMap(values map[string]datool.Tool) map[string]datool.Tool {
	result := make(map[string]datool.Tool, len(values))
	for name, executable := range values {
		result[name] = executable
	}
	return result
}

func reduceMessages(current any, writes []any) (any, error) {
	left, err := messagesView(current)
	if err != nil {
		return nil, err
	}
	right := make([][]damessage.Message, 0, len(writes))
	for _, write := range writes {
		messages, err := messagesView(write)
		if err != nil {
			return nil, err
		}
		right = append(right, messages)
	}
	return damessage.DeltaReduce(left, right)
}

func reduceMessagesOwned(current any, writes []any) (any, error) {
	left, err := messagesView(current)
	if err != nil {
		return nil, err
	}
	right := make([][]damessage.Message, 0, len(writes))
	for _, write := range writes {
		messages, err := messagesView(write)
		if err != nil {
			return nil, err
		}
		right = append(right, messages)
	}
	return damessage.DeltaReduceOwned(left, right)
}

func messagesFrom(value any) ([]damessage.Message, error) {
	if value == nil {
		return []damessage.Message{}, nil
	}
	switch typed := value.(type) {
	case []damessage.Message:
		return cloneMessages(typed), nil
	case []any:
		result := make([]damessage.Message, len(typed))
		for index, item := range typed {
			messageValue, ok := item.(damessage.Message)
			if !ok {
				return nil, fmt.Errorf("agent messages[%d] has type %T", index, item)
			}
			result[index] = messageValue.Clone()
		}
		return result, nil
	default:
		return nil, fmt.Errorf("agent messages have type %T", value)
	}
}

// messagesView avoids an extra deep copy when a caller only reads the slice or
// immediately passes it to a reducer that provides its own isolation.
func messagesView(value any) ([]damessage.Message, error) {
	if typed, ok := value.([]damessage.Message); ok {
		return typed, nil
	}
	return messagesFrom(value)
}

func cloneMessageValue(value any) any {
	messages, err := messagesFrom(value)
	if err != nil {
		return value
	}
	return messages
}

func mergeUpdate(current, combined, update dastate.Values) {
	for key, value := range update {
		if key == MessagesKey {
			if overwrite, ok := value.(dastate.Overwrite); ok {
				if replacement, err := messagesFrom(overwrite.Value); err == nil {
					replacement = damessage.EnsureIDs(replacement)
					current[key] = replacement
					combined[key] = dastate.Overwrite{Value: replacement}
					continue
				}
			}
			currentMessages, err := messagesView(current[key])
			if err == nil {
				incoming, incomingErr := messagesView(value)
				if incomingErr == nil {
					incoming = damessage.EnsureIDs(incoming)
					merged, mergeErr := damessage.DeltaReduce(currentMessages, [][]damessage.Message{incoming})
					if mergeErr == nil {
						current[key] = merged
					}
					if pendingOverwrite, ok := combined[key].(dastate.Overwrite); ok {
						pending, pendingErr := messagesView(pendingOverwrite.Value)
						if pendingErr == nil {
							pendingMerged, pendingMergeErr := damessage.DeltaReduce(pending, [][]damessage.Message{incoming})
							if pendingMergeErr == nil {
								combined[key] = dastate.Overwrite{Value: pendingMerged}
								continue
							}
						}
					}
					pending, _ := messagesView(combined[key])
					combined[key] = append(pending, incoming...)
					continue
				}
			}
		}
		current[key] = value
		combined[key] = value
	}
}

func (compiler *compiler) mergeUpdate(current, combined, update dastate.Values) {
	if !compiler.options.RetainThreadState {
		mergeUpdate(current, combined, update)
		return
	}
	for key, value := range update {
		if key != MessagesKey {
			current[key] = value
			combined[key] = value
			continue
		}
		if _, overwrite := value.(dastate.Overwrite); overwrite {
			mergeUpdate(current, combined, update)
			return
		}
		currentMessages, currentErr := messagesView(current[key])
		incoming, incomingErr := messagesView(value)
		if currentErr != nil || incomingErr != nil {
			mergeUpdate(current, combined, update)
			return
		}
		incoming = damessage.EnsureIDs(incoming)
		merged, err := damessage.DeltaReduceOwned(currentMessages, [][]damessage.Message{incoming})
		if err != nil {
			mergeUpdate(current, combined, update)
			return
		}
		current[key] = merged
		pending, _ := messagesView(combined[key])
		combined[key] = append(pending, incoming...)
	}
}

func mergePendingUpdate(combined, update dastate.Values) {
	for key, value := range update {
		if key != MessagesKey {
			combined[key] = value
			continue
		}
		incoming, err := messagesView(value)
		if err != nil {
			combined[key] = value
			continue
		}
		pending, _ := messagesView(combined[key])
		combined[key] = append(pending, incoming...)
	}
}

func ensureMessageIDsInValues(values dastate.Values) {
	if values == nil {
		return
	}
	raw, exists := values[MessagesKey]
	if !exists {
		return
	}
	if overwrite, ok := raw.(dastate.Overwrite); ok {
		if messages, err := messagesView(overwrite.Value); err == nil {
			overwrite.Value = damessage.EnsureIDs(messages)
			values[MessagesKey] = overwrite
		}
		return
	}
	if messages, err := messagesView(raw); err == nil {
		values[MessagesKey] = damessage.EnsureIDs(messages)
	}
}

func identityClone(value any) any { return value }

func cloneConfigurable(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneConfigurableValue(value)
	}
	return result
}

func cloneConfigurableValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneConfigurable(typed)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = cloneConfigurableValue(typed[index])
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(typed))
		for key, item := range typed {
			result[key] = item
		}
		return result
	case []string:
		return append([]string(nil), typed...)
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	default:
		return value
	}
}

func convertRuntime(runtime graph.Runtime) Runtime {
	var writer EventWriter
	if runtime.Writer != nil {
		writer = eventWriter{writer: runtime.Writer}
	}
	return Runtime{
		Deps: runtime.Deps, Config: runtime.Config, Configurable: datool.NewConfigurable(runtime.Configurable),
		Store: runtime.Store, Cache: runtime.Cache,
		Previous: runtime.Previous.Clone(), TaskID: runtime.TaskID, Resume: runtime.Resume,
		Writer: writer,
	}
}

type eventWriter struct{ writer graph.EventWriter }

func (writer eventWriter) Write(ctx context.Context, value json.RawMessage) error {
	if writer.writer == nil {
		return nil
	}
	return writer.writer.Write(ctx, graph.Event{Mode: graph.EventCustom, Custom: append(json.RawMessage(nil), value...)})
}

type toolWriter struct{ writer graph.EventWriter }

func (writer toolWriter) Write(ctx context.Context, value json.RawMessage) error {
	return eventWriter{writer: writer.writer}.Write(ctx, value)
}

func configureStructuredRequest(request *ModelRequest, output *StructuredOutput) {
	if output == nil {
		return
	}
	strategy := output.Strategy
	if strategy == "" || strategy == StructuredAuto {
		if request.Model.Profile().StructuredOutput {
			strategy = StructuredProvider
		} else {
			strategy = StructuredTool
		}
	}
	if strategy == StructuredProvider {
		request.ResponseFormat = &damodel.ResponseFormat{Name: output.Name, Description: output.Description, Schema: output.Schema, Strict: output.Strict}
		return
	}
	synthetic := datool.Func{Spec: datool.Definition{Name: output.Name, Description: output.Description, InputSchema: output.Schema}}
	request.Tools = append(request.Tools, synthetic)
}

func handleStructuredResponse(response damodel.Response, output *StructuredOutput) (ModelResponse, error) {
	if output == nil {
		return ModelResponse{Messages: []damessage.Message{response.Message}}, nil
	}
	strategy := output.Strategy
	if strategy == "" || strategy == StructuredAuto {
		if len(response.Structured) > 0 {
			strategy = StructuredProvider
		} else {
			strategy = StructuredTool
		}
	}
	if strategy == StructuredProvider {
		if len(response.Structured) == 0 || !json.Valid(response.Structured) {
			return structuredFailure(response.Message, nil, output, "provider structured response is missing or invalid")
		}
		if err := validateStructured(output, response.Structured); err != nil {
			return structuredFailure(response.Message, nil, output, err.Error())
		}
		return ModelResponse{Messages: []damessage.Message{response.Message}, Structured: append(json.RawMessage(nil), response.Structured...)}, nil
	}
	var matching []damessage.ToolCall
	for _, call := range response.Message.ToolCalls {
		if call.Name == output.Name {
			matching = append(matching, call)
		}
	}
	if len(matching) == 0 {
		return ModelResponse{Messages: []damessage.Message{response.Message}}, nil
	}
	if len(matching) > 1 {
		return ModelResponse{}, fmt.Errorf("%w: multiple structured output calls", ErrInvalidModelOutput)
	}
	if !json.Valid(matching[0].Arguments) {
		return structuredFailure(response.Message, &matching[0], output, "structured output arguments are invalid JSON")
	}
	if err := validateStructured(output, matching[0].Arguments); err != nil {
		return structuredFailure(response.Message, &matching[0], output, err.Error())
	}
	content := output.ToolMessageContent
	if content == "" {
		content = "Returning structured response"
	}
	toolMessage := damessage.Tool(matching[0].ID, content)
	toolMessage.Name = matching[0].Name
	return ModelResponse{
		Messages:   []damessage.Message{response.Message, toolMessage},
		Structured: append(json.RawMessage(nil), matching[0].Arguments...),
	}, nil
}

func validateStructured(output *StructuredOutput, raw json.RawMessage) error {
	if output == nil || output.compiled == nil {
		return fmt.Errorf("%w: schema is not compiled", ErrStructuredValidation)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%w: %v", ErrStructuredValidation, err)
	}
	if err := output.compiled.Validate(value); err != nil {
		return fmt.Errorf("%w: %v", ErrStructuredValidation, err)
	}
	return nil
}

func structuredFailure(assistant damessage.Message, call *damessage.ToolCall, output *StructuredOutput, detail string) (ModelResponse, error) {
	err := fmt.Errorf("%w: %s", ErrStructuredValidation, detail)
	if output == nil || !output.HandleErrors {
		return ModelResponse{}, err
	}
	messages := []damessage.Message{assistant}
	if call != nil {
		toolMessage := damessage.Tool(call.ID, err.Error()+". Return a corrected value matching the declared schema.")
		toolMessage.Name = call.Name
		toolMessage.ToolStatus = damessage.ToolStatusError
		messages = append(messages, toolMessage)
	} else {
		messages = append(messages, damessage.Human(err.Error()+". Return a corrected value matching the declared schema."))
	}
	return ModelResponse{Messages: messages, Update: dastate.Values{structuredRetryKey: true}}, nil
}
