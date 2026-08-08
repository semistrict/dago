// NativeService exposes Dago's provider-neutral model contract through the
// Shelley model facade used by product features outside the agent loop.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"
)

const nativeOpenAIReasoningStateKey = "openai.responses.reasoning"

type nativeOpenAIReasoningState struct {
	ID               string                            `json:"id"`
	Summary          []OpenAIResponsesReasoningSummary `json:"summary"`
	EncryptedContent string                            `json:"encrypted_content"`
}

// NativeService exposes a Dago chat model through Shelley's LLM interface.
type NativeService struct {
	chat               dmodel.Chat
	supportsImages     bool
	useSimplifiedPatch bool
	maxImageDimension  int
	maxImageBytes      int
	provider           string
	modelID            string
	baseURL            string
	supportsReasoning  bool
}

// DagoChat returns the native model with catalog capability overrides applied.
// Agent runtimes can use it without crossing the Shelley request boundary.
func (service *NativeService) DagoChat() dmodel.Chat {
	return profiledChat{Chat: service.chat, profile: service.Profile()}
}

// Profile returns the native model profile with Shelley catalog capability
// overrides applied.
func (service *NativeService) Profile() dmodel.Profile {
	profile := service.chat.Profile()
	profile.SupportsReasoning = service.supportsReasoning
	return profile
}

type profiledChat struct {
	dmodel.Chat
	profile dmodel.Profile
}

func (chat profiledChat) Profile() dmodel.Profile { return chat.profile }

func NewNativeService(chat dmodel.Chat) (*NativeService, error) {
	return NewNativeServiceWithOptions(chat, NativeServiceOptions{})
}

type NativeServiceOptions struct {
	SupportsImages     bool
	UseSimplifiedPatch bool
	MaxImageDimension  int
	MaxImageBytes      int
	Provider           string
	ModelID            string
	BaseURL            string
	SupportsReasoning  bool
}

type unavailableService struct{ err error }

func UnavailableNativeService(err error) Service {
	if err == nil {
		panic("llm.UnavailableNativeService called with nil error")
	}
	return unavailableService{err: err}
}

func (service unavailableService) Do(context.Context, *Request) (*Response, error) {
	return nil, service.err
}
func (unavailableService) Provider() string        { return "dago" }
func (unavailableService) TokenContextWindow() int { return 0 }
func (unavailableService) MaxImageDimension() int  { return 0 }
func (unavailableService) MaxImageBytes() int      { return 0 }
func (unavailableService) SupportsImages() bool    { return false }

func NewNativeServiceWithOptions(chat dmodel.Chat, options NativeServiceOptions) (*NativeService, error) {
	if chat == nil {
		return nil, fmt.Errorf("native model: chat model is required")
	}
	return &NativeService{
		chat: chat, supportsImages: options.SupportsImages,
		useSimplifiedPatch: options.UseSimplifiedPatch,
		maxImageDimension:  options.MaxImageDimension, maxImageBytes: options.MaxImageBytes,
		provider: options.Provider, modelID: options.ModelID, baseURL: options.BaseURL,
		supportsReasoning: options.SupportsReasoning,
	}, nil
}

func (service *NativeService) Provider() string {
	if service.provider != "" {
		return service.provider
	}
	return service.chat.Profile().Provider
}
func (service *NativeService) TokenContextWindow() int       { return service.chat.Profile().ContextWindow }
func (service *NativeService) MaxImageDimension() int        { return service.maxImageDimension }
func (service *NativeService) MaxImageBytes() int            { return service.maxImageBytes }
func (service *NativeService) SupportsImages() bool          { return service.supportsImages }
func (service *NativeService) SupportsReasoning() bool       { return service.supportsReasoning }
func (service *NativeService) UseSimplifiedPatch() bool      { return service.useSimplifiedPatch }
func (service *NativeService) DefaultReasoningLevel() string { return "" }
func (service *NativeService) ModelID() string {
	if service.modelID != "" {
		return service.modelID
	}
	return service.chat.Profile().Model
}
func (service *NativeService) BaseURL() string { return service.baseURL }

func (service *NativeService) Do(ctx context.Context, request *Request) (*Response, error) {
	if request == nil {
		return nil, fmt.Errorf("native model: request is required")
	}
	converted, err := requestToDago(request)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	var response dmodel.Response
	if request.OnStream == nil {
		response, err = service.chat.Invoke(ctx, converted)
	} else {
		response, err = service.stream(ctx, converted, request.OnStream)
	}
	if err != nil {
		return nil, err
	}
	finished := time.Now()
	return responseFromDago(response, service.chat.Profile(), started, finished), nil
}

func (service *NativeService) stream(ctx context.Context, request dmodel.Request, emit func(StreamDelta)) (dmodel.Response, error) {
	stream, err := service.chat.Stream(ctx, request)
	if err != nil {
		return dmodel.Response{}, err
	}
	defer stream.Close()
	response := dmodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant}}
	nextIndex := 0
	for {
		chunk, nextErr := stream.Next(ctx)
		if nextErr == io.EOF {
			return response, nil
		}
		if nextErr != nil {
			return dmodel.Response{}, nextErr
		}
		for _, block := range chunk.MessageDelta.Content {
			index := nextIndex
			if block.Index != nil {
				index = *block.Index
			}
			switch block.Type {
			case dmessage.BlockText:
				emit(StreamDelta{Type: "text", Text: block.Text, Index: index})
			case dmessage.BlockReasoning:
				emit(StreamDelta{Type: "thinking", Text: block.Reasoning, Index: index})
			}
			nextIndex = max(nextIndex, index+1)
		}
		for _, call := range chunk.MessageDelta.ToolCalls {
			emit(StreamDelta{Type: "tool_input", Text: string(call.Arguments), Index: nextIndex})
			nextIndex++
		}
		mergeChunk(&response, chunk)
	}
}

func requestToDago(request *Request) (dmodel.Request, error) {
	messages := make([]dmessage.Message, 0, len(request.Messages)+1)
	if len(request.System) > 0 {
		var text string
		for index, item := range request.System {
			if index > 0 {
				text += "\n\n"
			}
			text += item.Text
		}
		messages = append(messages, dmessage.System(text))
	}
	for _, item := range request.Messages {
		converted, err := messagesToDago(item)
		if err != nil {
			return dmodel.Request{}, err
		}
		messages = append(messages, converted...)
	}
	definitions := make([]dtool.Definition, 0, len(request.Tools))
	for _, item := range request.Tools {
		if item == nil || item.ServerSide {
			continue
		}
		definition := dtool.Definition{Name: item.Name, Description: item.Description, InputSchema: append(json.RawMessage(nil), item.InputSchema...), Direct: item.EndsTurn}
		if err := definition.Validate(); err != nil {
			return dmodel.Request{}, fmt.Errorf("native model: tool %q: %w", item.Name, err)
		}
		definitions = append(definitions, definition)
	}
	converted := dmodel.Request{Messages: messages, Tools: definitions}
	switch {
	case request.ReasoningEffort != "":
		converted.Reasoning = &dmodel.Reasoning{Effort: request.ReasoningEffort, Summary: "auto"}
	case request.ThinkingLevel == ThinkingLevelOff:
		converted.Reasoning = &dmodel.Reasoning{}
	case request.ThinkingLevel != ThinkingLevelDefault:
		converted.Reasoning = &dmodel.Reasoning{Effort: request.ThinkingLevel.ThinkingEffort(), Summary: "auto"}
	}
	if request.ToolChoice != nil {
		choice := &dmodel.ToolChoice{}
		switch request.ToolChoice.Type {
		case ToolChoiceTypeAny:
			choice.Mode = "required"
		case ToolChoiceTypeNone:
			choice.Mode = "none"
		case ToolChoiceTypeTool:
			choice.Mode, choice.Name = "tool", request.ToolChoice.Name
		default:
			choice.Mode = "auto"
		}
		converted.ToolChoice = choice
	}
	return converted, nil
}

func messagesToDago(item Message) ([]dmessage.Message, error) {
	role := dmessage.RoleHuman
	if item.Role == MessageRoleAssistant {
		role = dmessage.RoleAssistant
	}
	base := dmessage.Message{Role: role}
	result := make([]dmessage.Message, 0, 1)
	for _, block := range item.Content {
		switch block.Type {
		case ContentTypeText:
			base.Content = append(base.Content, dmessage.ContentBlock{Type: dmessage.BlockText, Text: block.Text})
		case ContentTypeThinking, ContentTypeRedactedThinking:
			converted := dmessage.ContentBlock{Type: dmessage.BlockReasoning, ID: block.ID, Reasoning: block.Thinking}
			if block.OpenAIResponsesReasoning != nil {
				state := nativeOpenAIReasoningState{
					ID:               block.OpenAIResponsesReasoning.ID,
					Summary:          append([]OpenAIResponsesReasoningSummary(nil), block.OpenAIResponsesReasoning.Summary...),
					EncryptedContent: block.OpenAIResponsesReasoning.EncryptedContent,
				}
				if raw, err := json.Marshal(state); err == nil {
					converted.Extra = map[string]json.RawMessage{nativeOpenAIReasoningStateKey: raw}
				}
			}
			base.Content = append(base.Content, converted)
		case ContentTypeToolUse:
			base.ToolCalls = append(base.ToolCalls, dmessage.ToolCall{ID: block.ID, Name: block.ToolName, Arguments: append(json.RawMessage(nil), block.ToolInput...)})
		case ContentTypeToolResult:
			if len(base.Content) > 0 || len(base.ToolCalls) > 0 {
				result = append(result, base)
				base = dmessage.Message{Role: role}
			}
			toolMessage := dmessage.Message{Role: dmessage.RoleTool, ToolCallID: block.ToolUseID, ToolStatus: dmessage.ToolStatusSuccess}
			if block.ToolError {
				toolMessage.ToolStatus = dmessage.ToolStatusError
			}
			for _, toolBlock := range block.ToolResult {
				if toolBlock.Type == ContentTypeText {
					toolMessage.Content = append(toolMessage.Content, dmessage.ContentBlock{Type: dmessage.BlockText, Text: toolBlock.Text})
				}
			}
			result = append(result, toolMessage)
		}
	}
	if len(base.Content) > 0 || len(base.ToolCalls) > 0 || len(result) == 0 {
		result = append(result, base)
	}
	return result, nil
}

func responseFromDago(response dmodel.Response, profile dmodel.Profile, started, finished time.Time) *Response {
	result := &Response{ID: response.Message.ID, Role: MessageRoleAssistant, Model: profile.Model, StopReason: StopReasonEndTurn, StartTime: &started, EndTime: &finished}
	for _, block := range response.Message.Content {
		switch block.Type {
		case dmessage.BlockText:
			result.Content = append(result.Content, Content{ID: block.ID, Type: ContentTypeText, Text: block.Text})
		case dmessage.BlockReasoning:
			content := Content{ID: block.ID, Type: ContentTypeThinking, Thinking: block.Reasoning}
			if raw := block.Extra[nativeOpenAIReasoningStateKey]; len(raw) > 0 {
				var state nativeOpenAIReasoningState
				if json.Unmarshal(raw, &state) == nil {
					content.OpenAIResponsesReasoning = &OpenAIResponsesReasoningMetadata{
						ID: state.ID, EncryptedContent: state.EncryptedContent,
						Summary: append([]OpenAIResponsesReasoningSummary(nil), state.Summary...),
					}
				}
			}
			result.Content = append(result.Content, content)
		case dmessage.BlockImage:
			result.Content = append(result.Content, Content{ID: block.ID, Type: ContentTypeText, Text: block.URL})
		}
	}
	for _, call := range response.Message.ToolCalls {
		result.Content = append(result.Content, Content{ID: call.ID, Type: ContentTypeToolUse, ToolName: call.Name, ToolInput: append(json.RawMessage(nil), call.Arguments...)})
	}
	if len(response.Message.ToolCalls) > 0 {
		result.StopReason = StopReasonToolUse
	}
	if usage := response.Message.Usage; usage != nil {
		result.Usage.InputTokens = uint64(max(0, usage.InputTokens))
		result.Usage.OutputTokens = uint64(max(0, usage.OutputTokens))
		result.Usage.Model = profile.Model
	}
	return result
}

func mergeChunk(response *dmodel.Response, chunk dmodel.Chunk) {
	delta := chunk.MessageDelta
	if response.Message.ID == "" {
		response.Message.ID = delta.ID
	}
	for _, block := range delta.Content {
		if block.Type == dmessage.BlockText && len(response.Message.Content) > 0 && response.Message.Content[len(response.Message.Content)-1].Type == dmessage.BlockText {
			response.Message.Content[len(response.Message.Content)-1].Text += block.Text
			continue
		}
		response.Message.Content = append(response.Message.Content, block)
	}
	response.Message.ToolCalls = append(response.Message.ToolCalls, delta.ToolCalls...)
	if delta.Usage != nil {
		usage := *delta.Usage
		response.Message.Usage = &usage
	}
	if len(chunk.Structured) > 0 {
		response.Structured = append(json.RawMessage(nil), chunk.Structured...)
	}
}
