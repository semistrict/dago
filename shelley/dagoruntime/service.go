// Package dagoruntime adapts Dago's provider-neutral model contract to the
// Shelley server's established LLM boundary.
package dagoruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/llm"
)

// Service exposes a Dago chat model through Shelley's LLM interface.
type Service struct {
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

// DagoChat returns the native model used by this compatibility service.
// Agent runtimes can use it without crossing the compatibility boundary twice.
func (service *Service) DagoChat() dmodel.Chat { return service.chat }

func NewService(chat dmodel.Chat) (*Service, error) {
	return NewServiceWithOptions(chat, ServiceOptions{})
}

type ServiceOptions struct {
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

func Unavailable(err error) llm.Service {
	if err == nil {
		panic("dagoruntime.Unavailable called with nil error")
	}
	return unavailableService{err: err}
}

func (service unavailableService) Do(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, service.err
}
func (unavailableService) Provider() string        { return "dago" }
func (unavailableService) TokenContextWindow() int { return 0 }
func (unavailableService) MaxImageDimension() int  { return 0 }
func (unavailableService) MaxImageBytes() int      { return 0 }
func (unavailableService) SupportsImages() bool    { return false }

func NewServiceWithOptions(chat dmodel.Chat, options ServiceOptions) (*Service, error) {
	if chat == nil {
		return nil, fmt.Errorf("dago runtime: chat model is required")
	}
	return &Service{
		chat: chat, supportsImages: options.SupportsImages,
		useSimplifiedPatch: options.UseSimplifiedPatch,
		maxImageDimension:  options.MaxImageDimension, maxImageBytes: options.MaxImageBytes,
		provider: options.Provider, modelID: options.ModelID, baseURL: options.BaseURL,
		supportsReasoning: options.SupportsReasoning,
	}, nil
}

func (service *Service) Provider() string {
	if service.provider != "" {
		return service.provider
	}
	return service.chat.Profile().Provider
}
func (service *Service) TokenContextWindow() int       { return service.chat.Profile().ContextWindow }
func (service *Service) MaxImageDimension() int        { return service.maxImageDimension }
func (service *Service) MaxImageBytes() int            { return service.maxImageBytes }
func (service *Service) SupportsImages() bool          { return service.supportsImages }
func (service *Service) SupportsReasoning() bool       { return service.supportsReasoning }
func (service *Service) UseSimplifiedPatch() bool      { return service.useSimplifiedPatch }
func (service *Service) DefaultReasoningLevel() string { return "" }
func (service *Service) ModelID() string {
	if service.modelID != "" {
		return service.modelID
	}
	return service.chat.Profile().Model
}
func (service *Service) BaseURL() string { return service.baseURL }

func (service *Service) Do(ctx context.Context, request *llm.Request) (*llm.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("dago runtime: request is required")
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

func (service *Service) stream(ctx context.Context, request dmodel.Request, emit func(llm.StreamDelta)) (dmodel.Response, error) {
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
				emit(llm.StreamDelta{Type: "text", Text: block.Text, Index: index})
			case dmessage.BlockReasoning:
				emit(llm.StreamDelta{Type: "thinking", Text: block.Reasoning, Index: index})
			}
			nextIndex = max(nextIndex, index+1)
		}
		for _, call := range chunk.MessageDelta.ToolCalls {
			emit(llm.StreamDelta{Type: "tool_input", Text: string(call.Arguments), Index: nextIndex})
			nextIndex++
		}
		mergeChunk(&response, chunk)
	}
}

func requestToDago(request *llm.Request) (dmodel.Request, error) {
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
			return dmodel.Request{}, fmt.Errorf("dago runtime: tool %q: %w", item.Name, err)
		}
		definitions = append(definitions, definition)
	}
	converted := dmodel.Request{Messages: messages, Tools: definitions}
	if request.ToolChoice != nil {
		choice := &dmodel.ToolChoice{}
		switch request.ToolChoice.Type {
		case llm.ToolChoiceTypeAny:
			choice.Mode = "required"
		case llm.ToolChoiceTypeNone:
			choice.Mode = "none"
		case llm.ToolChoiceTypeTool:
			choice.Mode, choice.Name = "tool", request.ToolChoice.Name
		default:
			choice.Mode = "auto"
		}
		converted.ToolChoice = choice
	}
	return converted, nil
}

func messagesToDago(item llm.Message) ([]dmessage.Message, error) {
	role := dmessage.RoleHuman
	if item.Role == llm.MessageRoleAssistant {
		role = dmessage.RoleAssistant
	}
	base := dmessage.Message{Role: role}
	result := make([]dmessage.Message, 0, 1)
	for _, block := range item.Content {
		switch block.Type {
		case llm.ContentTypeText:
			base.Content = append(base.Content, dmessage.ContentBlock{Type: dmessage.BlockText, Text: block.Text})
		case llm.ContentTypeThinking, llm.ContentTypeRedactedThinking:
			base.Content = append(base.Content, dmessage.ContentBlock{Type: dmessage.BlockReasoning, ID: block.ID, Reasoning: block.Thinking})
		case llm.ContentTypeToolUse:
			base.ToolCalls = append(base.ToolCalls, dmessage.ToolCall{ID: block.ID, Name: block.ToolName, Arguments: append(json.RawMessage(nil), block.ToolInput...)})
		case llm.ContentTypeToolResult:
			if len(base.Content) > 0 || len(base.ToolCalls) > 0 {
				result = append(result, base)
				base = dmessage.Message{Role: role}
			}
			toolMessage := dmessage.Message{Role: dmessage.RoleTool, ToolCallID: block.ToolUseID, ToolStatus: dmessage.ToolStatusSuccess}
			if block.ToolError {
				toolMessage.ToolStatus = dmessage.ToolStatusError
			}
			for _, toolBlock := range block.ToolResult {
				if toolBlock.Type == llm.ContentTypeText {
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

func responseFromDago(response dmodel.Response, profile dmodel.Profile, started, finished time.Time) *llm.Response {
	result := &llm.Response{ID: response.Message.ID, Role: llm.MessageRoleAssistant, Model: profile.Model, StopReason: llm.StopReasonEndTurn, StartTime: &started, EndTime: &finished}
	for _, block := range response.Message.Content {
		switch block.Type {
		case dmessage.BlockText:
			result.Content = append(result.Content, llm.Content{ID: block.ID, Type: llm.ContentTypeText, Text: block.Text})
		case dmessage.BlockReasoning:
			result.Content = append(result.Content, llm.Content{ID: block.ID, Type: llm.ContentTypeThinking, Thinking: block.Reasoning})
		case dmessage.BlockImage:
			result.Content = append(result.Content, llm.Content{ID: block.ID, Type: llm.ContentTypeText, Text: block.URL})
		}
	}
	for _, call := range response.Message.ToolCalls {
		result.Content = append(result.Content, llm.Content{ID: call.ID, Type: llm.ContentTypeToolUse, ToolName: call.Name, ToolInput: append(json.RawMessage(nil), call.Arguments...)})
	}
	if len(response.Message.ToolCalls) > 0 {
		result.StopReason = llm.StopReasonToolUse
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
