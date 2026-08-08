package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
)

const (
	shelleyMessageMetadata  = "shelley.message.v1"
	shelleyResponseMetadata = "shelley.response.v1"
	shelleyToolArtifact     = "shelley.tool_result.v1"
	openAIReasoningStateKey = "openai.responses.reasoning"
)

type openAIReasoningState struct {
	ID               string                                `json:"id"`
	Summary          []llm.OpenAIResponsesReasoningSummary `json:"summary"`
	EncryptedContent string                                `json:"encrypted_content"`
}

// nativeChatOptions supplies the request-scoped controls that are not part of
// Dago's provider-neutral request shape yet.
type nativeChatOptions struct {
	ThinkingLevel func() llm.ThinkingLevel
	OnRetry       func(llm.RetryEvent)
}

// nativeChat exposes any Shelley model implementation through Dago's model
// contract. The Dago agent graph remains the sole owner of the model/tool loop.
func nativeChat(service llm.Service, options nativeChatOptions) dmodel.Chat {
	if native, ok := service.(interface{ DagoChat() dmodel.Chat }); ok {
		if chat := native.DagoChat(); chat != nil {
			return chat
		}
	}
	return &chatAdapter{service: service, options: options}
}

type chatAdapter struct {
	service llm.Service
	options nativeChatOptions
}

func (adapter *chatAdapter) Profile() dmodel.Profile {
	if adapter == nil || adapter.service == nil {
		return dmodel.Profile{}
	}
	profile := dmodel.Profile{
		Provider: adapter.service.Provider(), ContextWindow: adapter.service.TokenContextWindow(),
		ToolCalling: true, ParallelToolCalls: false, NativeStreaming: true,
	}
	if identified, ok := adapter.service.(interface{ ModelID() string }); ok {
		profile.Model = identified.ModelID()
	}
	return profile
}

func (adapter *chatAdapter) Invoke(ctx context.Context, request dmodel.Request) (dmodel.Response, error) {
	converted, err := adapter.requestFromDago(request, nil)
	if err != nil {
		return dmodel.Response{}, err
	}
	response, err := adapter.do(ctx, converted)
	if err != nil {
		return dmodel.Response{}, err
	}
	return responseToDago(response)
}

func (adapter *chatAdapter) Stream(ctx context.Context, request dmodel.Request) (dmodel.Stream, error) {
	if adapter == nil || adapter.service == nil {
		return nil, fmt.Errorf("dago runtime: Shelley service is required")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	result := &shelleyStream{cancel: cancel, chunks: make(chan streamItem, 64)}
	converted, err := adapter.requestFromDago(request, func(delta llm.StreamDelta) {
		messageDelta := dmessage.Message{Role: dmessage.RoleAssistant}
		index := delta.Index
		switch delta.Type {
		case "text":
			messageDelta.Content = []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: delta.Text, Index: &index}}
		case "thinking":
			messageDelta.Content = []dmessage.ContentBlock{{Type: dmessage.BlockReasoning, Reasoning: delta.Text, Index: &index}}
		default:
			// Tool input deltas do not carry a stable call id/name in Shelley's
			// callback. The complete tool call is emitted with the final response.
			return
		}
		result.emit(streamCtx, streamItem{chunk: dmodel.Chunk{MessageDelta: messageDelta}})
		result.mu.Lock()
		result.sawContent = true
		result.mu.Unlock()
	})
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer close(result.chunks)
		response, runErr := adapter.do(streamCtx, converted)
		if runErr != nil {
			result.emit(streamCtx, streamItem{err: runErr})
			return
		}
		convertedResponse, convertErr := responseToDago(response)
		if convertErr != nil {
			result.emit(streamCtx, streamItem{err: convertErr})
			return
		}
		result.mu.Lock()
		sawContent := result.sawContent
		result.mu.Unlock()
		terminal := convertedResponse.Message
		if sawContent {
			terminal.Content = nil
		}
		result.emit(streamCtx, streamItem{chunk: dmodel.Chunk{MessageDelta: terminal, Done: true}})
	}()
	return result, nil
}

func (adapter *chatAdapter) do(ctx context.Context, request *llm.Request) (*llm.Response, error) {
	response, err := adapter.service.Do(ctx, request)
	if err != nil || response == nil || response.StopReason != llm.StopReasonPause {
		return response, err
	}
	merged := append([]llm.Content(nil), response.Content...)
	usage := response.Usage
	started := response.StartTime
	for continuation := 0; response.StopReason == llm.StopReasonPause; continuation++ {
		if continuation >= 16 {
			break
		}
		nextRequest := *request
		nextRequest.Messages = append(append([]llm.Message(nil), request.Messages...), llm.Message{
			Role: llm.MessageRoleAssistant, Content: append([]llm.Content(nil), merged...),
		})
		response, err = adapter.service.Do(ctx, &nextRequest)
		if err != nil {
			return nil, err
		}
		usage.Add(response.Usage)
		merged = append(merged, response.Content...)
	}
	resolved := *response
	resolved.Content = merged
	resolved.Usage = usage
	resolved.StartTime = started
	return &resolved, nil
}

func (adapter *chatAdapter) requestFromDago(request dmodel.Request, stream func(llm.StreamDelta)) (*llm.Request, error) {
	if adapter == nil || adapter.service == nil {
		return nil, fmt.Errorf("dago runtime: Shelley service is required")
	}
	converted := &llm.Request{OnStream: stream, OnRetry: adapter.options.OnRetry}
	if request.Reasoning != nil {
		converted.ReasoningEffort = request.Reasoning.Effort
	}
	if adapter.options.ThinkingLevel != nil {
		converted.ThinkingLevel = adapter.options.ThinkingLevel()
	}
	for _, item := range request.Messages {
		if item.Role == dmessage.RoleSystem {
			converted.System = append(converted.System, llm.SystemContent{Text: item.TextContent()})
			continue
		}
		messages, err := messagesFromDago([]dmessage.Message{item})
		if err != nil {
			return nil, err
		}
		converted.Messages = append(converted.Messages, messages...)
	}
	converted.Messages = combineToolResultMessages(converted.Messages)
	for _, definition := range request.Tools {
		converted.Tools = append(converted.Tools, &llm.Tool{
			Name: definition.Name, Description: definition.Description,
			InputSchema: append(json.RawMessage(nil), definition.InputSchema...), EndsTurn: definition.Direct,
		})
	}
	if request.ToolChoice != nil {
		choice := &llm.ToolChoice{Name: request.ToolChoice.Name}
		switch request.ToolChoice.Mode {
		case "required":
			choice.Type = llm.ToolChoiceTypeAny
		case "none":
			choice.Type = llm.ToolChoiceTypeNone
		case "tool", "function":
			choice.Type = llm.ToolChoiceTypeTool
		default:
			choice.Type = llm.ToolChoiceTypeAuto
		}
		converted.ToolChoice = choice
	}
	if request.PromptCache != nil {
		if len(converted.Tools) > 0 {
			converted.Tools[len(converted.Tools)-1].Cache = true
		}
		for index := len(converted.Messages) - 1; index >= 0; index-- {
			if converted.Messages[index].Role == llm.MessageRoleUser && len(converted.Messages[index].Content) > 0 {
				converted.Messages[index].Content[len(converted.Messages[index].Content)-1].Cache = true
				break
			}
		}
	}
	return converted, nil
}

type streamItem struct {
	chunk dmodel.Chunk
	err   error
}

type shelleyStream struct {
	cancel     context.CancelFunc
	chunks     chan streamItem
	mu         sync.Mutex
	sawContent bool
	closeOnce  sync.Once
}

func (stream *shelleyStream) emit(ctx context.Context, item streamItem) {
	select {
	case stream.chunks <- item:
	case <-ctx.Done():
	}
}

func (stream *shelleyStream) Next(ctx context.Context) (dmodel.Chunk, error) {
	select {
	case <-ctx.Done():
		return dmodel.Chunk{}, ctx.Err()
	case item, ok := <-stream.chunks:
		if !ok {
			return dmodel.Chunk{}, io.EOF
		}
		return item.chunk, item.err
	}
}

func (stream *shelleyStream) Close() error {
	stream.closeOnce.Do(stream.cancel)
	return nil
}

// nativeToolOptions supplies the Shelley context values expected by existing tools.
type nativeToolOptions struct {
	WorkingDir    func() string
	Progress      llm.ToolProgressFunc
	Service       llm.Service
	RequireNative bool
}

type toolArtifactEnvelope struct {
	Version    int                 `json:"version"`
	Kind       string              `json:"kind"`
	Content    llm.Content         `json:"content"`
	OtherUsage []llm.PurposedUsage `json:"other_usage,omitempty"`
}

// resolveNativeTools selects production-native executables by name and adapts
// only the remaining pinned Shelley test facades. Dago validates, schedules,
// cancels, and checkpoints every returned tool.
func resolveNativeTools(items []*llm.Tool, overrides []dtool.Tool, options nativeToolOptions) ([]dtool.Tool, error) {
	nativeByName := make(map[string]dtool.Tool, len(overrides))
	for _, item := range overrides {
		if item == nil {
			return nil, fmt.Errorf("native tool is nil")
		}
		definition := item.Definition()
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("native tool %q: %w", definition.Name, err)
		}
		if nativeByName[definition.Name] != nil {
			return nil, fmt.Errorf("duplicate native tool %q", definition.Name)
		}
		nativeByName[definition.Name] = item
	}
	result := make([]dtool.Tool, 0, len(items))
	for _, item := range items {
		if item == nil || item.ServerSide {
			continue
		}
		if native := nativeByName[item.Name]; native != nil {
			result = append(result, native)
			delete(nativeByName, item.Name)
			continue
		}
		if options.RequireNative {
			return nil, fmt.Errorf("dago runtime: tool %q has no native implementation", item.Name)
		}
		if item.Run == nil {
			return nil, fmt.Errorf("dago runtime: tool %q has no implementation", item.Name)
		}
		definition := dtool.Definition{
			Name: item.Name, Description: item.Description,
			InputSchema: append(json.RawMessage(nil), item.InputSchema...), Direct: item.EndsTurn,
		}
		if err := definition.Validate(); err != nil {
			return nil, fmt.Errorf("dago runtime: tool %q: %w", item.Name, err)
		}
		shelleyTool := item
		result = append(result, dtool.Func{Spec: definition, Run: func(ctx context.Context, input json.RawMessage, runtime dtool.Runtime) (dtool.Result, error) {
			toolCtx := ctx
			if options.WorkingDir != nil {
				if workingDir := options.WorkingDir(); workingDir != "" {
					toolCtx = llm.WithWorkingDir(toolCtx, workingDir)
				}
			}
			if options.Progress != nil {
				toolCtx = llm.WithToolProgress(toolCtx, options.Progress)
			}
			toolCtx = llm.WithToolUseID(toolCtx, runtime.CallID)
			if options.Service != nil {
				toolCtx = llm.WithLLMService(toolCtx, options.Service)
			}
			var usage llmhttp.UsageAccumulator
			toolCtx = llmhttp.WithUsageCollector(toolCtx, usage.Collect)
			started := time.Now()
			output := shelleyTool.Run(toolCtx, input)
			finished := time.Now()
			content := output.LLMContent
			if output.Error != nil {
				content = llm.TextContent(output.Error.Error())
			}
			exact := llm.Content{
				Type: llm.ContentTypeToolResult, ToolUseID: runtime.CallID,
				ToolError: output.Error != nil, ToolResult: content,
				ToolUseStartTime: &started, ToolUseEndTime: &finished, Display: output.Display,
			}
			artifact, err := json.Marshal(toolArtifactEnvelope{
				Version: 1, Kind: shelleyToolArtifact, Content: exact, OtherUsage: usage.Take(),
			})
			if err != nil {
				return dtool.Result{}, fmt.Errorf("encode Shelley tool result: %w", err)
			}
			blocks := contentToDago(content)
			if len(blocks) == 0 {
				blocks = []dmessage.ContentBlock{{Type: dmessage.BlockText, Text: ""}}
			}
			return dtool.Result{Content: blocks, Artifact: artifact}, nil
		}})
	}
	if len(nativeByName) > 0 {
		return nil, fmt.Errorf("native tool %q has no Shelley facade", firstToolName(nativeByName))
	}
	return result, nil
}

func firstToolName(items map[string]dtool.Tool) string {
	for name := range items {
		return name
	}
	return ""
}

// messagesToDago converts persisted Shelley context into Dago checkpoint state.
func messagesToDago(items []llm.Message) ([]dmessage.Message, error) {
	var result []dmessage.Message
	for _, item := range items {
		if item.ExcludedFromContext || item.ErrorType != llm.ErrorTypeNone {
			continue
		}
		role := dmessage.RoleHuman
		if item.Role == llm.MessageRoleAssistant {
			role = dmessage.RoleAssistant
		}
		base := dmessage.Message{Role: role}
		var toolMessages []dmessage.Message
		for _, content := range item.Content {
			switch content.Type {
			case llm.ContentTypeToolUse:
				base.ToolCalls = append(base.ToolCalls, dmessage.ToolCall{
					ID: content.ID, Name: content.ToolName,
					Arguments: append(json.RawMessage(nil), content.ToolInput...),
				})
			case llm.ContentTypeToolResult:
				artifact, err := json.Marshal(toolArtifactEnvelope{Version: 1, Kind: shelleyToolArtifact, Content: content})
				if err != nil {
					return nil, err
				}
				status := dmessage.ToolStatusSuccess
				if content.ToolError {
					status = dmessage.ToolStatusError
				}
				toolMessages = append(toolMessages, dmessage.Message{
					Role: dmessage.RoleTool, ToolCallID: content.ToolUseID,
					ToolStatus: status, Content: contentToDago(content.ToolResult), Artifact: artifact,
				})
			default:
				base.Content = append(base.Content, contentToDago([]llm.Content{content})...)
			}
		}
		if len(base.Content) > 0 || len(base.ToolCalls) > 0 || len(toolMessages) == 0 {
			fragment := item
			fragment.Content = removeToolResults(item.Content)
			encoded, err := json.Marshal(fragment)
			if err != nil {
				return nil, err
			}
			base.Metadata = map[string]json.RawMessage{shelleyMessageMetadata: encoded}
			result = append(result, base)
		}
		result = append(result, toolMessages...)
	}
	return result, nil
}

// messagesFromDago returns the Shelley representation used by the existing UI
// projection and provider adapters.
func messagesFromDago(items []dmessage.Message) ([]llm.Message, error) {
	result := make([]llm.Message, 0, len(items))
	for _, item := range items {
		if raw := item.Metadata[shelleyMessageMetadata]; len(raw) > 0 {
			var exact llm.Message
			if err := json.Unmarshal(raw, &exact); err != nil {
				return nil, fmt.Errorf("decode Shelley message metadata: %w", err)
			}
			result = append(result, exact)
			continue
		}
		if item.Role == dmessage.RoleSystem || item.Role == dmessage.RoleRemove {
			continue
		}
		if item.Role == dmessage.RoleTool {
			content, _, err := toolResultFromDago(item)
			if err != nil {
				return nil, err
			}
			result = append(result, llm.Message{Role: llm.MessageRoleUser, Content: []llm.Content{content}})
			continue
		}
		converted := llm.Message{Role: llm.MessageRoleUser}
		if item.Role == dmessage.RoleAssistant {
			converted.Role = llm.MessageRoleAssistant
			converted.EndOfTurn = len(item.ToolCalls) == 0
		}
		converted.Content = contentFromDago(item.Content)
		for _, call := range item.ToolCalls {
			converted.Content = append(converted.Content, llm.Content{
				ID: call.ID, Type: llm.ContentTypeToolUse, ToolName: call.Name,
				ToolInput: append(json.RawMessage(nil), call.Arguments...),
			})
		}
		result = append(result, converted)
	}
	return combineToolResultMessages(result), nil
}

// responseFromDago recovers exact provider response metadata when available and
// otherwise projects a native assistant message into Shelley's UI record.
func responseFromDago(item dmessage.Message) (*llm.Response, bool, error) {
	raw := item.ResponseMetadata[shelleyResponseMetadata]
	if len(raw) > 0 {
		var response llm.Response
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, false, fmt.Errorf("decode Shelley response metadata: %w", err)
		}
		return &response, true, nil
	}
	if item.Role != dmessage.RoleAssistant {
		return nil, false, nil
	}
	if len(item.ResponseMetadata[dmodel.ResponseMetadataKey]) == 0 {
		return nil, false, nil
	}
	response := llm.Response{
		ID: item.ID, Role: llm.MessageRoleAssistant,
		Content: contentFromDago(item.Content), StopReason: llm.StopReasonEndTurn,
	}
	for _, call := range item.ToolCalls {
		response.Content = append(response.Content, llm.Content{
			ID: call.ID, Type: llm.ContentTypeToolUse, ToolName: call.Name,
			ToolInput: append(json.RawMessage(nil), call.Arguments...),
		})
	}
	if len(item.ToolCalls) > 0 {
		response.StopReason = llm.StopReasonToolUse
	}
	if item.Usage != nil {
		response.Usage.InputTokens = uint64(max(0, item.Usage.InputTokens))
		response.Usage.OutputTokens = uint64(max(0, item.Usage.OutputTokens))
		response.Usage.CacheCreationInputTokens = uint64(max(0, item.Usage.InputDetails["cache_creation"]))
		response.Usage.CacheReadInputTokens = uint64(max(0, item.Usage.InputDetails["cache_read"]))
	}
	return &response, true, nil
}

// toolResultFromDago recovers one exact tool result and its indirect usage.
func toolResultFromDago(item dmessage.Message) (llm.Content, []llm.PurposedUsage, error) {
	if len(item.Artifact) > 0 {
		var artifact toolArtifactEnvelope
		if err := json.Unmarshal(item.Artifact, &artifact); err == nil && artifact.Kind == shelleyToolArtifact {
			return artifact.Content, artifact.OtherUsage, nil
		}
	}
	content := llm.Content{
		Type: llm.ContentTypeToolResult, ToolUseID: item.ToolCallID,
		ToolError:  item.ToolStatus == dmessage.ToolStatusError,
		ToolResult: contentFromDago(item.Content),
	}
	return content, nil, nil
}

func responseToDago(response *llm.Response) (dmodel.Response, error) {
	if response == nil {
		return dmodel.Response{}, errors.New("dago runtime: Shelley service returned a nil response")
	}
	exact, err := json.Marshal(response)
	if err != nil {
		return dmodel.Response{}, fmt.Errorf("encode Shelley response metadata: %w", err)
	}
	message := dmessage.Message{
		ID: response.ID, Role: dmessage.RoleAssistant,
		ResponseMetadata: map[string]json.RawMessage{shelleyResponseMetadata: exact},
		Content:          contentToDago(response.Content),
	}
	if message.ID == "" {
		message.ID = uuid.NewString()
	}
	for _, content := range response.Content {
		if content.Type == llm.ContentTypeToolUse {
			message.ToolCalls = append(message.ToolCalls, dmessage.ToolCall{
				ID: content.ID, Name: content.ToolName,
				Arguments: append(json.RawMessage(nil), content.ToolInput...),
			})
		}
	}
	if response.StopReason == llm.StopReasonMaxTokens || response.StopReason == llm.StopReasonRefusal {
		message.ToolCalls = nil
	}
	message.Usage = &dmessage.Usage{
		InputTokens: int(response.Usage.TotalInputTokens()), OutputTokens: int(response.Usage.OutputTokens),
		TotalTokens: int(response.Usage.TotalInputTokens() + response.Usage.OutputTokens),
		InputDetails: map[string]int{
			"uncached": int(response.Usage.InputTokens), "cache_creation": int(response.Usage.CacheCreationInputTokens),
			"cache_read": int(response.Usage.CacheReadInputTokens),
		},
	}
	return dmodel.Response{Message: message}, nil
}

func contentToDago(items []llm.Content) []dmessage.ContentBlock {
	result := make([]dmessage.ContentBlock, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case llm.ContentTypeText:
			result = append(result, dmessage.ContentBlock{Type: dmessage.BlockText, ID: item.ID, Text: item.Text})
		case llm.ContentTypeThinking, llm.ContentTypeRedactedThinking:
			block := dmessage.ContentBlock{Type: dmessage.BlockReasoning, ID: item.ID, Reasoning: item.Thinking}
			if item.OpenAIResponsesReasoning != nil {
				state := openAIReasoningState{
					ID: item.OpenAIResponsesReasoning.ID, Summary: append([]llm.OpenAIResponsesReasoningSummary(nil), item.OpenAIResponsesReasoning.Summary...),
					EncryptedContent: item.OpenAIResponsesReasoning.EncryptedContent,
				}
				if raw, err := json.Marshal(state); err == nil {
					block.Extra = map[string]json.RawMessage{openAIReasoningStateKey: raw}
				}
			}
			result = append(result, block)
		case llm.ContentTypeToolUse, llm.ContentTypeToolResult:
			// Calls/results use the dedicated Dago fields.
		default:
			raw, err := json.Marshal(item)
			if err == nil {
				result = append(result, dmessage.ContentBlock{Type: dmessage.BlockNonStandard, ID: item.ID, NonStandard: raw})
			}
		}
	}
	return result
}

func contentFromDago(items []dmessage.ContentBlock) []llm.Content {
	result := make([]llm.Content, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case dmessage.BlockText:
			result = append(result, llm.Content{ID: item.ID, Type: llm.ContentTypeText, Text: item.Text})
		case dmessage.BlockReasoning:
			content := llm.Content{ID: item.ID, Type: llm.ContentTypeThinking, Thinking: item.Reasoning}
			if raw := item.Extra[openAIReasoningStateKey]; len(raw) > 0 {
				var state openAIReasoningState
				if json.Unmarshal(raw, &state) == nil {
					content.OpenAIResponsesReasoning = &llm.OpenAIResponsesReasoningMetadata{
						ID: state.ID, EncryptedContent: state.EncryptedContent,
						Summary: append([]llm.OpenAIResponsesReasoningSummary(nil), state.Summary...),
					}
				}
			}
			result = append(result, content)
		case dmessage.BlockImage:
			result = append(result, llm.Content{ID: item.ID, Type: llm.ContentTypeText, Text: item.URL, MediaType: item.MIMEType, Data: string(item.Data)})
		case dmessage.BlockNonStandard:
			var exact llm.Content
			if json.Unmarshal(item.NonStandard, &exact) == nil {
				result = append(result, exact)
			}
		}
	}
	return result
}

func removeToolResults(items []llm.Content) []llm.Content {
	result := make([]llm.Content, 0, len(items))
	for _, item := range items {
		if item.Type != llm.ContentTypeToolResult {
			result = append(result, item)
		}
	}
	return result
}

func combineToolResultMessages(items []llm.Message) []llm.Message {
	result := make([]llm.Message, 0, len(items))
	for _, item := range items {
		if len(result) > 0 && isToolResultOnly(result[len(result)-1]) && isToolResultOnly(item) {
			result[len(result)-1].Content = append(result[len(result)-1].Content, item.Content...)
			continue
		}
		result = append(result, item)
	}
	return result
}

func isToolResultOnly(item llm.Message) bool {
	if item.Role != llm.MessageRoleUser || len(item.Content) == 0 {
		return false
	}
	for _, content := range item.Content {
		if content.Type != llm.ContentTypeToolResult {
			return false
		}
	}
	return true
}
