package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

// ChatOptions configures an OpenAI-compatible Chat Completions endpoint.
type ChatOptions struct {
	Model             string
	BaseURL           string
	HTTPClient        *http.Client
	Headers           http.Header
	Provider          string
	ContextWindow     int
	MaxOutputTokens   int
	SupportsImages    bool
	SupportsReasoning bool
	ParallelToolCalls bool
	DefaultReasoning  *model.Reasoning
}

// ChatCompletionsClient implements the Dago model contract against the
// OpenAI-compatible Chat Completions protocol.
type ChatCompletionsClient struct {
	apiKey     string
	options    ChatOptions
	boundTools []tool.Definition
}

// NewChatCompletions creates an OpenAI-compatible chat model.
func NewChatCompletions(apiKey string, options ChatOptions) (*ChatCompletionsClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("openai chat: API key is required")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, fmt.Errorf("openai chat: model is required")
	}
	if options.BaseURL == "" {
		options.BaseURL = defaultAPIBaseURL
	}
	options.BaseURL = chatAPIBaseURL(options.BaseURL)
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Provider == "" {
		options.Provider = "openai"
	}
	if options.DefaultReasoning != nil {
		reasoning := *options.DefaultReasoning
		options.DefaultReasoning = &reasoning
	}
	return &ChatCompletionsClient{apiKey: apiKey, options: options}, nil
}

func chatAPIBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	value = strings.TrimSuffix(value, "/chat/completions")
	return strings.TrimRight(value, "/")
}

func (client *ChatCompletionsClient) BindTools(definitions []tool.Definition) (model.Chat, error) {
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
	}
	copy := *client
	copy.boundTools = cloneDefinitions(definitions)
	return &copy, nil
}

func (client *ChatCompletionsClient) Profile() model.Profile {
	return model.Profile{
		Provider: client.options.Provider, Model: client.options.Model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: client.options.ParallelToolCalls,
		StructuredOutput: true, NativeStreaming: true,
		SupportsReasoning: client.options.SupportsReasoning,
	}
}

func (client *ChatCompletionsClient) CountTokens(_ context.Context, messages []message.Message) (int, error) {
	return message.ApproximateTokens(messages), nil
}

func (client *ChatCompletionsClient) Invoke(ctx context.Context, request model.Request) (model.Response, error) {
	body, err := client.requestBody(request, false)
	if err != nil {
		return model.Response{}, err
	}
	response, err := client.do(ctx, body)
	if err != nil {
		return model.Response{}, err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return model.Response{}, err
	}
	var payload chatResponse
	if err := decodeJSON(response.Body, &payload); err != nil {
		return model.Response{}, fmt.Errorf("openai chat: decode response: %w", err)
	}
	return normalizeChatResponse(payload, request.ResponseFormat)
}

func (client *ChatCompletionsClient) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	body, err := client.requestBody(request, true)
	if err != nil {
		return nil, err
	}
	response, err := client.do(ctx, body)
	if err != nil {
		return nil, err
	}
	if err := responseError(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return newChatStream(response.Body, request.ResponseFormat), nil
}

type chatRequest struct {
	Model             string             `json:"model"`
	Messages          []chatMessage      `json:"messages"`
	Tools             []chatTool         `json:"tools,omitempty"`
	ToolChoice        any                `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool              `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    any                `json:"response_format,omitempty"`
	ReasoningEffort   string             `json:"reasoning_effort,omitempty"`
	MaxTokens         int                `json:"max_tokens,omitempty"`
	Stop              []string           `json:"stop,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	StreamOptions     *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function chatCallFunction `json:"function"`
}

type chatCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (client *ChatCompletionsClient) requestBody(request model.Request, stream bool) ([]byte, error) {
	messages, err := toChatMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	definitions := request.Tools
	if len(definitions) == 0 {
		definitions = client.boundTools
	}
	tools := make([]chatTool, 0, len(definitions))
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
		tools = append(tools, chatTool{Type: "function", Function: chatFunction{
			Name: definition.Name, Description: definition.Description,
			Parameters: append(json.RawMessage(nil), definition.InputSchema...),
		}})
	}
	payload := chatRequest{
		Model: client.options.Model, Messages: messages, Tools: tools,
		MaxTokens: client.options.MaxOutputTokens, Stop: append([]string(nil), request.Stop...), Stream: stream,
	}
	if stream {
		payload.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	}
	if len(tools) > 0 && client.options.ParallelToolCalls {
		parallel := true
		payload.ParallelToolCalls = &parallel
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case "", "auto", "none", "required":
			payload.ToolChoice = request.ToolChoice.Mode
		case "tool", "function", "name":
			payload.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": request.ToolChoice.Name}}
		default:
			return nil, fmt.Errorf("openai chat: unsupported tool choice %q", request.ToolChoice.Mode)
		}
	}
	if request.ResponseFormat != nil {
		payload.ResponseFormat = map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": request.ResponseFormat.Name, "description": request.ResponseFormat.Description,
			"schema": json.RawMessage(request.ResponseFormat.Schema), "strict": request.ResponseFormat.Strict,
		}}
	}
	reasoning := request.Reasoning
	if reasoning == nil {
		reasoning = client.options.DefaultReasoning
	}
	if reasoning != nil {
		payload.ReasoningEffort = reasoning.Effort
	}
	return json.Marshal(payload)
}

func toChatMessages(messages []message.Message) ([]chatMessage, error) {
	result := make([]chatMessage, 0, len(messages))
	for _, source := range messages {
		if source.Role == message.RoleRemove {
			continue
		}
		target := chatMessage{Name: source.Name, ToolCallID: source.ToolCallID}
		switch source.Role {
		case message.RoleSystem:
			target.Role = "system"
		case message.RoleHuman:
			target.Role = "user"
		case message.RoleAssistant:
			target.Role = "assistant"
		case message.RoleTool:
			target.Role = "tool"
		default:
			return nil, fmt.Errorf("openai chat: unsupported message role %q", source.Role)
		}
		parts := make([]chatContentPart, 0, len(source.Content))
		var reasoning strings.Builder
		for _, block := range source.Content {
			switch block.Type {
			case message.BlockText:
				parts = append(parts, chatContentPart{Type: "text", Text: block.Text})
			case message.BlockReasoning:
				reasoning.WriteString(block.Reasoning)
			case message.BlockImage:
				url := block.URL
				if url == "" && len(block.Data) > 0 {
					url = "data:" + block.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(block.Data)
				}
				if url != "" {
					parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
				}
			}
		}
		target.ReasoningContent = reasoning.String()
		if len(parts) == 1 && parts[0].Type == "text" {
			target.Content = parts[0].Text
		} else if len(parts) > 0 {
			target.Content = parts
		}
		for _, call := range source.ToolCalls {
			arguments, err := normalizeToolArguments(call.Arguments)
			if err != nil {
				return nil, fmt.Errorf("openai chat: tool call %q: %w", call.ID, err)
			}
			target.ToolCalls = append(target.ToolCalls, chatToolCall{ID: call.ID, Type: "function", Function: chatCallFunction{Name: call.Name, Arguments: string(arguments)}})
		}
		result = append(result, target)
	}
	return result, nil
}

func (client *ChatCompletionsClient) do(ctx context.Context, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.options.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai chat: create request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+client.apiKey)
	request.Header.Set("Content-Type", "application/json")
	for key, values := range client.options.Headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.options.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("openai chat: request: %w", err)
	}
	return response, nil
}

type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
	Error   *apiError    `json:"error,omitempty"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
	Delta   chatMessage `json:"delta"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func normalizeChatResponse(payload chatResponse, format *model.ResponseFormat) (model.Response, error) {
	if payload.Error != nil {
		return model.Response{}, apiErrorValue(payload.Error, http.StatusOK)
	}
	if len(payload.Choices) == 0 {
		return model.Response{}, fmt.Errorf("openai chat: response contained no choices")
	}
	result, err := fromChatMessage(payload.Choices[0].Message)
	if err != nil {
		return model.Response{}, err
	}
	result.ID = payload.ID
	result.Usage = &message.Usage{InputTokens: payload.Usage.PromptTokens, OutputTokens: payload.Usage.CompletionTokens, TotalTokens: payload.Usage.TotalTokens}
	response := model.Response{Message: result}
	if format != nil {
		content := result.TextContent()
		if !json.Valid([]byte(content)) {
			return model.Response{}, fmt.Errorf("openai chat: structured response is not valid JSON")
		}
		response.Structured = json.RawMessage(content)
	}
	return response, nil
}

func fromChatMessage(source chatMessage) (message.Message, error) {
	result := message.Message{Role: message.RoleAssistant, Name: source.Name}
	switch content := source.Content.(type) {
	case string:
		if content != "" {
			result.Content = append(result.Content, message.ContentBlock{Type: message.BlockText, Text: content})
		}
	case nil:
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return message.Message{}, fmt.Errorf("openai chat: decode message content: %w", err)
		}
		var parts []chatContentPart
		if err := json.Unmarshal(encoded, &parts); err != nil {
			return message.Message{}, fmt.Errorf("openai chat: decode message content: %w", err)
		}
		for _, part := range parts {
			if part.Type == "text" {
				result.Content = append(result.Content, message.ContentBlock{Type: message.BlockText, Text: part.Text})
			}
		}
	}
	if source.ReasoningContent != "" {
		result.Content = append([]message.ContentBlock{{Type: message.BlockReasoning, Reasoning: source.ReasoningContent}}, result.Content...)
	}
	for _, call := range source.ToolCalls {
		arguments, err := normalizeToolArguments(json.RawMessage(call.Function.Arguments))
		if err != nil {
			return message.Message{}, fmt.Errorf("openai chat: tool arguments for %q are invalid JSON", call.ID)
		}
		result.ToolCalls = append(result.ToolCalls, message.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	return result, nil
}

type chatStream struct {
	body       io.ReadCloser
	scanner    *bufio.Scanner
	format     *model.ResponseFormat
	calls      map[int]chatToolCall
	structured strings.Builder
	done       bool
}

func newChatStream(body io.ReadCloser, format *model.ResponseFormat) *chatStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &chatStream{body: body, scanner: scanner, format: format, calls: map[int]chatToolCall{}}
}

func (stream *chatStream) Next(ctx context.Context) (model.Chunk, error) {
	if stream.done {
		return model.Chunk{}, io.EOF
	}
	for stream.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			stream.Close()
			return model.Chunk{}, err
		}
		line := stream.scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			stream.done = true
			chunk := model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant}, Done: true}
			calls, err := stream.finishToolCalls()
			if err != nil {
				return model.Chunk{}, err
			}
			chunk.MessageDelta.ToolCalls = calls
			if stream.format != nil && json.Valid([]byte(stream.structured.String())) {
				chunk.Structured = json.RawMessage(stream.structured.String())
			}
			return chunk, nil
		}
		var payload chatResponse
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			stream.Close()
			return model.Chunk{}, fmt.Errorf("openai chat: decode stream event: %w", err)
		}
		if payload.Error != nil {
			stream.Close()
			return model.Chunk{}, apiErrorValue(payload.Error, http.StatusOK)
		}
		chunk := model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant}}
		if payload.Usage.TotalTokens != 0 || payload.Usage.PromptTokens != 0 || payload.Usage.CompletionTokens != 0 {
			chunk.MessageDelta.Usage = &message.Usage{InputTokens: payload.Usage.PromptTokens, OutputTokens: payload.Usage.CompletionTokens, TotalTokens: payload.Usage.TotalTokens}
		}
		if len(payload.Choices) > 0 {
			delta := payload.Choices[0].Delta
			if content, ok := delta.Content.(string); ok && content != "" {
				chunk.MessageDelta.Content = append(chunk.MessageDelta.Content, message.ContentBlock{Type: message.BlockText, Text: content})
				stream.structured.WriteString(content)
			}
			if delta.ReasoningContent != "" {
				chunk.MessageDelta.Content = append(chunk.MessageDelta.Content, message.ContentBlock{Type: message.BlockReasoning, Reasoning: delta.ReasoningContent})
			}
			for _, fragment := range delta.ToolCalls {
				call := stream.calls[fragment.Index]
				if fragment.ID != "" {
					call.ID = fragment.ID
				}
				if fragment.Function.Name != "" {
					call.Function.Name += fragment.Function.Name
				}
				call.Function.Arguments += fragment.Function.Arguments
				stream.calls[fragment.Index] = call
			}
		}
		if len(chunk.MessageDelta.Content) > 0 || chunk.MessageDelta.Usage != nil {
			return chunk, nil
		}
	}
	if err := stream.scanner.Err(); err != nil {
		stream.Close()
		return model.Chunk{}, fmt.Errorf("openai chat: read stream: %w", err)
	}
	stream.done = true
	if len(stream.calls) > 0 {
		chunk := model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant}}
		calls, err := stream.finishToolCalls()
		if err != nil {
			return model.Chunk{}, err
		}
		chunk.MessageDelta.ToolCalls = calls
		return chunk, nil
	}
	return model.Chunk{}, io.EOF
}

func (stream *chatStream) finishToolCalls() ([]message.ToolCall, error) {
	result := make([]message.ToolCall, 0, len(stream.calls))
	for index := 0; index < len(stream.calls); index++ {
		call, ok := stream.calls[index]
		if !ok {
			continue
		}
		arguments, err := normalizeToolArguments(json.RawMessage(call.Function.Arguments))
		if err != nil {
			return nil, fmt.Errorf("openai chat: streamed tool arguments for %q are invalid JSON", call.ID)
		}
		result = append(result, message.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	stream.calls = map[int]chatToolCall{}
	return result, nil
}

func (stream *chatStream) Close() error {
	stream.done = true
	return stream.body.Close()
}
