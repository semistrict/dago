package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/semistrict/dago/cache"
	"github.com/semistrict/dago/checkpoint"
	graph "github.com/semistrict/dago/internal/graph"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/store"
	"github.com/semistrict/dago/tool"
)

// Options configures a generic tool-calling agent.
type Options struct {
	Name             string
	Model            model.Chat
	Tools            []tool.Tool
	SystemPrompt     string
	SystemMessage    *message.Message
	Middleware       []Middleware
	StateFields      map[string]StateField
	StructuredOutput *StructuredOutput
	Saver            checkpoint.Saver
	Store            store.Store
	Cache            cache.Cache
	Context          any
	RecursionLimit   int
	MaxConcurrency   int
	FailOnToolError  bool
}

// Agent is a compiled provider-neutral model/tool graph.
type Agent struct {
	graph   *graph.Compiled
	saver   checkpoint.Saver
	private map[string]bool
}

// Input starts or resumes one agent thread.
type Input struct {
	Config   checkpoint.Config
	Messages []message.Message
	State    state.Values
	Resume   any
}

// Result is the complete visible agent state.
type Result struct {
	Config     checkpoint.Config
	Messages   []message.Message
	Structured json.RawMessage
	State      state.Values
	Interrupts []Interrupt
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
		if options.SystemMessage.Role != message.RoleSystem {
			return nil, fmt.Errorf("create agent: system message role must be system")
		}
		copy := options.SystemMessage.Clone()
		options.SystemMessage = &copy
	}
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
			schema.Fields[name] = graph.Delta(field.Initial, field.Reduce, field.Clone, field.SnapshotFrequency)
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
		Saver: options.Saver, Store: options.Store, Cache: options.Cache, Context: options.Context,
		RecursionLimit: options.RecursionLimit, MaxConcurrency: options.MaxConcurrency,
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

func (agent *Agent) Invoke(ctx context.Context, input Input) (Result, error) {
	if input.Config.ThreadID == "" {
		input.Config.ThreadID = "default"
	}
	values := input.State.Clone()
	if values == nil {
		values = state.Values{}
	}
	if len(input.Messages) > 0 {
		values[MessagesKey] = message.EnsureIDs(input.Messages)
	}
	ensureMessageIDsInValues(values)
	execution, err := agent.graph.Invoke(ctx, graph.Invocation{Config: input.Config, State: values, Resume: input.Resume})
	if err != nil {
		return Result{}, err
	}
	return resultFromExecution(execution, agent.private)
}

// Cancel durably appends final messages/state and clears pending graph tasks.
// It must be called after the active invocation has observed cancellation.
func (agent *Agent) Cancel(ctx context.Context, input Input) (Result, error) {
	if input.Config.ThreadID == "" {
		input.Config.ThreadID = "default"
	}
	values := input.State.Clone()
	if values == nil {
		values = state.Values{}
	}
	if len(input.Messages) > 0 {
		values[MessagesKey] = message.EnsureIDs(input.Messages)
	}
	ensureMessageIDsInValues(values)
	execution, err := agent.graph.Cancel(ctx, graph.Invocation{Config: input.Config, State: values})
	if err != nil {
		return Result{}, err
	}
	return resultFromExecution(execution, agent.private)
}

func resultFromExecution(execution graph.Execution, private map[string]bool) (Result, error) {
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
	return result, nil
}

func publicState(values state.Values, private map[string]bool) state.Values {
	result := values.Clone()
	for key := range private {
		delete(result, key)
	}
	return result
}

type compiler struct {
	options Options
	tools   map[string]tool.Tool
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

func (compiler *compiler) beforeAgent(ctx context.Context, values state.Values, runtime graph.Runtime) (graph.Command, error) {
	update := state.Values{}
	current := values.Clone()
	for _, middleware := range compiler.options.Middleware {
		if middleware.BeforeAgent == nil {
			continue
		}
		result, err := middleware.BeforeAgent(ctx, current.Clone(), convertRuntime(runtime))
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q before agent: %w", middleware.Name, err)
		}
		mergeUpdate(current, update, result)
	}
	ensureMessageIDsInValues(update)
	return graph.Command{Update: update}, nil
}

func (compiler *compiler) afterAgent(ctx context.Context, values state.Values, runtime graph.Runtime) (graph.Command, error) {
	update := state.Values{}
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
		mergeUpdate(current, update, result)
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

func (compiler *compiler) model(ctx context.Context, values state.Values, runtime graph.Runtime) (graph.Command, error) {
	current := values.Clone()
	update := state.Values{}
	for _, middleware := range compiler.options.Middleware {
		if middleware.BeforeModel == nil {
			continue
		}
		result, err := middleware.BeforeModel(ctx, current.Clone(), convertRuntime(runtime))
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q before model: %w", middleware.Name, err)
		}
		mergeUpdate(current, update, result)
	}
	messages, err := messagesFrom(current[MessagesKey])
	if err != nil {
		return graph.Command{}, err
	}
	request := ModelRequest{
		Model: compiler.options.Model, Messages: messages, Tools: toolsSlice(compiler.tools),
		State: current.Clone(), Runtime: convertRuntime(runtime),
	}
	if compiler.options.SystemMessage != nil {
		system := compiler.options.SystemMessage.Clone()
		request.SystemMessage = &system
	} else if compiler.options.SystemPrompt != "" {
		system := message.System(compiler.options.SystemPrompt)
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
	mergeUpdate(current, update, state.Values{MessagesKey: response.Messages})
	mergeUpdate(current, update, response.Update)
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
		mergeUpdate(current, update, result)
	}
	ensureMessageIDsInValues(update)
	return graph.Command{Update: update}, nil
}

func (compiler *compiler) modelHandler() ModelHandler {
	return func(ctx context.Context, request ModelRequest) (ModelResponse, error) {
		definitions := make([]tool.Definition, 0, len(request.Tools))
		for _, executable := range request.Tools {
			definition := executable.Definition()
			if err := definition.Validate(); err != nil {
				return ModelResponse{}, err
			}
			definitions = append(definitions, definition)
		}
		messages := cloneMessages(request.Messages)
		if request.SystemMessage != nil {
			messages = append([]message.Message{request.SystemMessage.Clone()}, messages...)
		}
		chat := request.Model
		if binder, ok := chat.(model.Binder); ok && len(definitions) > 0 {
			bound, err := binder.BindTools(definitions)
			if err != nil {
				return ModelResponse{}, err
			}
			chat = bound
		}
		providerRequest := model.Request{
			Messages: messages, Tools: definitions, ToolChoice: request.ToolChoice,
			ResponseFormat: request.ResponseFormat, PromptCache: request.PromptCache,
			Reasoning: request.Reasoning,
			Metadata:  cloneRawMap(request.Metadata), Tags: append([]string(nil), request.Tags...),
		}
		var response model.Response
		var err error
		if request.Runtime.Writer != nil && chat.Profile().NativeStreaming {
			response, err = invokeModelStream(ctx, chat, providerRequest, request.Runtime.Writer)
		} else {
			response, err = chat.Invoke(ctx, providerRequest)
		}
		if err != nil {
			return ModelResponse{}, err
		}
		if response.Message.Role != message.RoleAssistant {
			return ModelResponse{}, fmt.Errorf("%w: response role is %q", ErrInvalidModelOutput, response.Message.Role)
		}
		if response.Message.ResponseMetadata == nil {
			response.Message.ResponseMetadata = map[string]json.RawMessage{}
		}
		response.Message.ResponseMetadata[model.ResponseMetadataKey] = json.RawMessage(`true`)
		if compiler.options.Name != "" && response.Message.Name == "" {
			response.Message.Name = compiler.options.Name
		}
		return handleStructuredResponse(response, compiler.options.StructuredOutput)
	}
}

func invokeModelStream(ctx context.Context, chat model.Chat, request model.Request, writer EventWriter) (model.Response, error) {
	stream, err := chat.Stream(ctx, request)
	if err != nil {
		return model.Response{}, err
	}
	defer stream.Close()
	response := model.Response{Message: message.Message{Role: message.RoleAssistant}}
	for {
		chunk, nextErr := stream.Next(ctx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return model.Response{}, nextErr
		}
		encoded, encodeErr := json.Marshal(streamEnvelope{Version: 1, Kind: "token", Chunk: chunk})
		if encodeErr != nil {
			return model.Response{}, encodeErr
		}
		if err := writer.Write(ctx, encoded); err != nil {
			return model.Response{}, err
		}
		mergeModelChunk(&response, chunk)
	}
	return response, nil
}

type streamEnvelope struct {
	Version int         `json:"version"`
	Kind    string      `json:"kind"`
	Chunk   model.Chunk `json:"chunk,omitempty"`
	Child   *ChildEvent `json:"child,omitempty"`
}

func mergeModelChunk(response *model.Response, chunk model.Chunk) {
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
		if block.Type == message.BlockText && len(response.Message.Content) > 0 {
			if response.Message.Content[len(response.Message.Content)-1].Type == message.BlockText {
				textTarget = len(response.Message.Content) - 1
			} else if block.Text == "" && (len(block.Citations) > 0 || len(block.Extra) > 0) {
				for index := len(response.Message.Content) - 1; index >= 0; index-- {
					if response.Message.Content[index].Type == message.BlockText {
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
		if block.Type == message.BlockReasoning {
			merged := false
			for index := len(response.Message.Content) - 1; index >= 0; index-- {
				current := &response.Message.Content[index]
				if current.Type != message.BlockReasoning || (block.ID != "" && current.ID != "" && current.ID != block.ID) {
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

func (compiler *compiler) executeTools(ctx context.Context, values state.Values, runtime graph.Runtime) (graph.Command, error) {
	messages, err := messagesFrom(values[MessagesKey])
	if err != nil {
		return graph.Command{}, err
	}
	if len(messages) == 0 {
		return graph.Command{}, fmt.Errorf("execute tools: message history is empty")
	}
	assistantIndex := len(messages) - 1
	for assistantIndex >= 0 && (messages[assistantIndex].Role != message.RoleAssistant || len(messages[assistantIndex].ToolCalls) == 0) {
		assistantIndex--
	}
	if assistantIndex < 0 {
		return graph.Command{}, fmt.Errorf("execute tools: last message has no tool calls")
	}
	completed := map[string]bool{}
	for _, item := range messages[assistantIndex+1:] {
		if item.Role == message.RoleTool && item.ToolCallID != "" {
			completed[item.ToolCallID] = true
		}
	}
	calls := make([]message.ToolCall, 0, len(messages[assistantIndex].ToolCalls))
	for _, call := range messages[assistantIndex].ToolCalls {
		if !completed[call.ID] {
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return graph.Command{}, nil
	}
	prefilled := []message.Message{}
	resumeConsumed := false
	for _, middleware := range compiler.options.Middleware {
		if middleware.BeforeTools == nil {
			continue
		}
		response, err := middleware.BeforeTools(ctx, ToolBatchRequest{
			Calls: append([]message.ToolCall(nil), calls...), Tools: cloneToolMap(compiler.tools),
			State: values.Clone(), Runtime: convertRuntime(runtime),
		})
		if err != nil {
			return graph.Command{}, fmt.Errorf("middleware %q before tools: %w", middleware.Name, err)
		}
		if response.Interrupt != nil {
			return graph.Command{Interrupt: &graph.Interrupt{ID: response.Interrupt.ID, Value: response.Interrupt.Value}}, nil
		}
		if response.Calls != nil {
			calls = append([]message.ToolCall(nil), response.Calls...)
		}
		resumeConsumed = resumeConsumed || response.ResumeConsumed
		prefilled = append(prefilled, cloneMessages(response.Messages)...)
	}
	type outcome struct {
		message   message.Message
		update    state.Values
		direct    bool
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
	update := state.Values{}
	byCallID := make(map[string]message.Message, len(prefilled)+len(results))
	var unassociated []message.Message
	for _, item := range messages[assistantIndex+1:] {
		if item.Role != message.RoleTool {
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
		if result.interrupt != nil {
			continue
		}
		byCallID[result.message.ToolCallID] = result.message
		if result.direct {
			update[toolDirectKey] = true
		}
		for key, value := range result.update {
			if previous, exists := update[key]; exists {
				field, reducible := compiler.fields[key]
				if !reducible || (field.Kind != FieldAggregate && field.Kind != FieldDelta) {
					return graph.Command{}, fmt.Errorf("%w for field %q", ErrConflictingUpdate, key)
				}
				if batch, ok := previous.(state.Batch); ok {
					batch.Values = append(batch.Values, value)
					update[key] = batch
				} else {
					update[key] = state.Batch{Values: []any{previous, value}}
				}
				continue
			}
			update[key] = value
		}
	}
	toolMessages := make([]message.Message, 0, len(byCallID)+len(unassociated))
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
			update[MessagesKey] = state.Overwrite{Value: replacement}
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

func (compiler *compiler) executeTool(ctx context.Context, call message.ToolCall, values state.Values, runtime graph.Runtime) (result struct {
	message   message.Message
	update    state.Values
	direct    bool
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
		result.message = message.Tool(call.ID, fmt.Sprintf("Error: %s is not a valid tool, try one of [%s].", call.Name, strings.Join(names, ", ")))
		result.message.Name = call.Name
		result.message.ToolStatus = message.ToolStatusError
		return
	}
	request := ToolCallRequest{Call: call, Tool: executable, State: values.Clone(), Runtime: convertRuntime(runtime)}
	handler := func(ctx context.Context, request ToolCallRequest) (ToolCallResponse, error) {
		output, err := request.Tool.Execute(ctx, request.Call.Arguments, tool.Runtime{
			CallID: request.Call.ID, TaskID: runtime.TaskID, ThreadID: runtime.Config.ThreadID,
			Namespace:    runtime.Config.Namespace,
			CheckpointID: runtime.Config.CheckpointID,
			Resume:       request.Runtime.Resume,
			State:        request.State, Store: runtime.Store,
			Stream: toolWriter{writer: runtime.Writer}, Context: runtime.Context,
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
		if compiler.options.FailOnToolError {
			result.err = err
			return
		}
		result.message = message.Tool(call.ID, err.Error())
		result.message.Name = call.Name
		result.message.ToolStatus = message.ToolStatusError
		return
	}
	if response.Result.Interrupt != nil {
		result.interrupt = &Interrupt{ID: response.Result.Interrupt.ID, Value: response.Result.Interrupt.Value}
		return
	}
	if response.Call != nil {
		call = *response.Call
	}
	result.message = message.Message{
		Role: message.RoleTool, Name: call.Name, ToolCallID: call.ID,
		ToolStatus: message.ToolStatusSuccess, Content: response.Result.Content,
		Artifact: response.Result.Artifact, OtherUsage: response.Result.OtherUsage,
	}
	result.update = state.Values(response.Result.Update)
	result.direct = executable.Definition().Direct
	return
}

func (compiler *compiler) routeModel(_ context.Context, values state.Values) ([]string, error) {
	if retry, _ := values[structuredRetryKey].(bool); retry {
		return []string{"model"}, nil
	}
	if _, done := values[StructuredResponseKey]; done {
		return []string{"after_agent"}, nil
	}
	messages, err := messagesFrom(values[MessagesKey])
	if err != nil {
		return nil, err
	}
	if len(messages) > 0 && len(messages[len(messages)-1].ToolCalls) > 0 {
		return []string{"tools"}, nil
	}
	return []string{"after_agent"}, nil
}

func (compiler *compiler) routeTools(_ context.Context, values state.Values) ([]string, error) {
	if direct, _ := values[toolDirectKey].(bool); direct {
		return []string{"after_agent"}, nil
	}
	return []string{"model"}, nil
}

func resolveFields(base map[string]StateField, middleware []Middleware) (map[string]StateField, error) {
	result := map[string]StateField{
		MessagesKey: {
			Kind: FieldDelta, Contract: "dago.messages.delta.v1", SnapshotFrequency: 100,
			Initial: func() any { return []message.Message{} }, Reduce: reduceMessages, Clone: cloneMessageValue,
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

func resolveTools(base []tool.Tool, middleware []Middleware) (map[string]tool.Tool, error) {
	result := map[string]tool.Tool{}
	all := append([]tool.Tool(nil), base...)
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

func toolsSlice(values map[string]tool.Tool) []tool.Tool {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		result = append(result, values[name])
	}
	return result
}

func cloneToolMap(values map[string]tool.Tool) map[string]tool.Tool {
	result := make(map[string]tool.Tool, len(values))
	for name, executable := range values {
		result[name] = executable
	}
	return result
}

func reduceMessages(current any, writes []any) (any, error) {
	left, err := messagesFrom(current)
	if err != nil {
		return nil, err
	}
	right := make([][]message.Message, 0, len(writes))
	for _, write := range writes {
		messages, err := messagesFrom(write)
		if err != nil {
			return nil, err
		}
		right = append(right, messages)
	}
	return message.DeltaReduce(left, right)
}

func messagesFrom(value any) ([]message.Message, error) {
	if value == nil {
		return []message.Message{}, nil
	}
	switch typed := value.(type) {
	case []message.Message:
		return cloneMessages(typed), nil
	case []any:
		result := make([]message.Message, len(typed))
		for index, item := range typed {
			messageValue, ok := item.(message.Message)
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

func cloneMessageValue(value any) any {
	messages, err := messagesFrom(value)
	if err != nil {
		return value
	}
	return messages
}

func mergeUpdate(current, combined, update state.Values) {
	for key, value := range update {
		if key == MessagesKey {
			if overwrite, ok := value.(state.Overwrite); ok {
				if replacement, err := messagesFrom(overwrite.Value); err == nil {
					replacement = message.EnsureIDs(replacement)
					current[key] = replacement
					combined[key] = state.Overwrite{Value: replacement}
					continue
				}
			}
			currentMessages, err := messagesFrom(current[key])
			if err == nil {
				incoming, incomingErr := messagesFrom(value)
				if incomingErr == nil {
					incoming = message.EnsureIDs(incoming)
					merged, mergeErr := message.DeltaReduce(currentMessages, [][]message.Message{incoming})
					if mergeErr == nil {
						current[key] = merged
					}
					if pendingOverwrite, ok := combined[key].(state.Overwrite); ok {
						pending, pendingErr := messagesFrom(pendingOverwrite.Value)
						if pendingErr == nil {
							pendingMerged, pendingMergeErr := message.DeltaReduce(pending, [][]message.Message{incoming})
							if pendingMergeErr == nil {
								combined[key] = state.Overwrite{Value: pendingMerged}
								continue
							}
						}
					}
					pending, _ := messagesFrom(combined[key])
					combined[key] = append(pending, incoming...)
					continue
				}
			}
		}
		current[key] = value
		combined[key] = value
	}
}

func ensureMessageIDsInValues(values state.Values) {
	if values == nil {
		return
	}
	raw, exists := values[MessagesKey]
	if !exists {
		return
	}
	if overwrite, ok := raw.(state.Overwrite); ok {
		if messages, err := messagesFrom(overwrite.Value); err == nil {
			overwrite.Value = message.EnsureIDs(messages)
			values[MessagesKey] = overwrite
		}
		return
	}
	if messages, err := messagesFrom(raw); err == nil {
		values[MessagesKey] = message.EnsureIDs(messages)
	}
}

func identityClone(value any) any { return value }

func convertRuntime(runtime graph.Runtime) Runtime {
	var writer EventWriter
	if runtime.Writer != nil {
		writer = eventWriter{writer: runtime.Writer}
	}
	return Runtime{
		Context: runtime.Context, Config: runtime.Config,
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
		request.ResponseFormat = &model.ResponseFormat{Name: output.Name, Description: output.Description, Schema: output.Schema, Strict: output.Strict}
		return
	}
	synthetic := tool.Func{Spec: tool.Definition{Name: output.Name, Description: output.Description, InputSchema: output.Schema}}
	request.Tools = append(request.Tools, synthetic)
}

func handleStructuredResponse(response model.Response, output *StructuredOutput) (ModelResponse, error) {
	if output == nil {
		return ModelResponse{Messages: []message.Message{response.Message}}, nil
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
		return ModelResponse{Messages: []message.Message{response.Message}, Structured: append(json.RawMessage(nil), response.Structured...)}, nil
	}
	var matching []message.ToolCall
	for _, call := range response.Message.ToolCalls {
		if call.Name == output.Name {
			matching = append(matching, call)
		}
	}
	if len(matching) == 0 {
		return ModelResponse{Messages: []message.Message{response.Message}}, nil
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
	toolMessage := message.Tool(matching[0].ID, content)
	toolMessage.Name = matching[0].Name
	return ModelResponse{
		Messages:   []message.Message{response.Message, toolMessage},
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

func structuredFailure(assistant message.Message, call *message.ToolCall, output *StructuredOutput, detail string) (ModelResponse, error) {
	err := fmt.Errorf("%w: %s", ErrStructuredValidation, detail)
	if output == nil || !output.HandleErrors {
		return ModelResponse{}, err
	}
	messages := []message.Message{assistant}
	if call != nil {
		toolMessage := message.Tool(call.ID, err.Error()+". Return a corrected value matching the declared schema.")
		toolMessage.Name = call.Name
		toolMessage.ToolStatus = message.ToolStatusError
		messages = append(messages, toolMessage)
	} else {
		messages = append(messages, message.Human(err.Error()+". Return a corrected value matching the declared schema."))
	}
	return ModelResponse{Messages: messages, Update: state.Values{structuredRetryKey: true}}, nil
}
