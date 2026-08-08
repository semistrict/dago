// Package anthropic adapts the Anthropic Messages API to Dago's model contract.
package anthropic

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

const defaultMessagesURL = "https://api.anthropic.com/v1/messages"

type Options struct {
	Model            string
	BaseURL          string
	HTTPClient       *http.Client
	Headers          http.Header
	ContextWindow    int
	MaxOutputTokens  int
	SupportsImages   bool
	AdaptiveThinking bool
	DefaultReasoning *model.Reasoning
}

type Client struct {
	apiKey     string
	options    Options
	boundTools []tool.Definition
}

func New(apiKey string, options Options) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("anthropic: API key is required")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, fmt.Errorf("anthropic: model is required")
	}
	if options.BaseURL == "" {
		options.BaseURL = defaultMessagesURL
	} else {
		options.BaseURL = messagesURL(options.BaseURL)
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.MaxOutputTokens == 0 {
		options.MaxOutputTokens = 64000
	}
	if options.ContextWindow == 0 {
		options.ContextWindow = 200000
	}
	if options.DefaultReasoning != nil {
		reasoning := *options.DefaultReasoning
		options.DefaultReasoning = &reasoning
	}
	return &Client{apiKey: apiKey, options: options}, nil
}

func messagesURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if strings.HasSuffix(value, "/messages") {
		return value
	}
	if strings.HasSuffix(value, "/v1") {
		return value + "/messages"
	}
	return value + "/v1/messages"
}

func (client *Client) BindTools(definitions []tool.Definition) (model.Chat, error) {
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
	}
	copy := *client
	copy.boundTools = cloneDefinitions(definitions)
	return &copy, nil
}

func (client *Client) Profile() model.Profile {
	return model.Profile{
		Provider: "anthropic", Model: client.options.Model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: true, NativeStreaming: true,
		SupportsPromptCaching: true, SupportsReasoning: true,
	}
}

func (client *Client) CountTokens(_ context.Context, messages []message.Message) (int, error) {
	return message.ApproximateTokens(messages), nil
}

func (client *Client) Invoke(ctx context.Context, request model.Request) (model.Response, error) {
	body, err := client.requestBody(request, false)
	if err != nil {
		return model.Response{}, err
	}
	response, err := client.do(ctx, body)
	if err != nil {
		return model.Response{}, err
	}
	defer response.Body.Close()
	if err := anthropicResponseError(response); err != nil {
		return model.Response{}, err
	}
	var payload responsePayload
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return model.Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	return normalizeResponse(payload)
}

func (client *Client) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	body, err := client.requestBody(request, true)
	if err != nil {
		return nil, err
	}
	response, err := client.do(ctx, body)
	if err != nil {
		return nil, err
	}
	if err := anthropicResponseError(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return newMessageStream(response.Body), nil
}

type requestPayload struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []wireMessage   `json:"messages"`
	System        []wireContent   `json:"system,omitempty"`
	Tools         []wireTool      `json:"tools,omitempty"`
	ToolChoice    *wireToolChoice `json:"tool_choice,omitempty"`
	Thinking      *wireThinking   `json:"thinking,omitempty"`
	OutputConfig  *outputConfig   `json:"output_config,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
}

type wireMessage struct {
	Role    string        `json:"role"`
	Content []wireContent `json:"content"`
}

type wireContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []wireContent   `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Citations []wireCitation  `json:"citations,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type wireCitation struct {
	Type       string `json:"type,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex *int   `json:"start_index,omitempty"`
	EndIndex   *int   `json:"end_index,omitempty"`
	CitedText  string `json:"cited_text,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type wireThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

func (client *Client) requestBody(request model.Request, stream bool) ([]byte, error) {
	messages, system, err := toWireMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	definitions := request.Tools
	if len(definitions) == 0 {
		definitions = client.boundTools
	}
	tools := make([]wireTool, 0, len(definitions))
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
		tools = append(tools, wireTool{Name: definition.Name, Description: definition.Description, InputSchema: append(json.RawMessage(nil), definition.InputSchema...)})
	}
	payload := requestPayload{
		Model: client.options.Model, MaxTokens: client.options.MaxOutputTokens,
		Messages: messages, System: system, Tools: tools,
		StopSequences: append([]string(nil), request.Stop...), Stream: stream,
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case "", "auto":
			payload.ToolChoice = &wireToolChoice{Type: "auto"}
		case "required":
			payload.ToolChoice = &wireToolChoice{Type: "any"}
		case "none":
			payload.ToolChoice = &wireToolChoice{Type: "none"}
		case "tool", "function", "name":
			payload.ToolChoice = &wireToolChoice{Type: "tool", Name: request.ToolChoice.Name}
		default:
			return nil, fmt.Errorf("anthropic: unsupported tool choice %q", request.ToolChoice.Mode)
		}
	}
	reasoning := request.Reasoning
	if reasoning == nil {
		reasoning = client.options.DefaultReasoning
	}
	if reasoning != nil && reasoning.Effort != "" && reasoning.Effort != "off" && reasoning.Effort != "none" {
		if client.options.AdaptiveThinking {
			payload.Thinking = &wireThinking{Type: "adaptive"}
			if reasoning.Summary != "" {
				payload.Thinking.Display = "summarized"
			}
			payload.OutputConfig = &outputConfig{Effort: normalizeAdaptiveEffort(reasoning.Effort)}
		} else {
			budget := reasoningBudget(reasoning.Effort)
			payload.Thinking = &wireThinking{Type: "enabled", BudgetTokens: budget}
			if payload.MaxTokens <= budget {
				payload.MaxTokens = budget + 1024
			}
		}
	}
	return json.Marshal(payload)
}

func normalizeAdaptiveEffort(effort string) string {
	if effort == "minimal" {
		return "low"
	}
	return effort
}

func reasoningBudget(effort string) int {
	switch effort {
	case "minimal":
		return 1024
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 16384
	case "xhigh", "max":
		return 32768
	default:
		return 8192
	}
}

func toWireMessages(messages []message.Message) ([]wireMessage, []wireContent, error) {
	var result []wireMessage
	var system []wireContent
	appendMessage := func(role string, content []wireContent) {
		if len(content) == 0 {
			return
		}
		if len(result) > 0 && result[len(result)-1].Role == role {
			result[len(result)-1].Content = append(result[len(result)-1].Content, content...)
			return
		}
		result = append(result, wireMessage{Role: role, Content: content})
	}
	for _, source := range messages {
		if source.Role == message.RoleRemove {
			continue
		}
		if source.Role == message.RoleSystem {
			if text := source.TextContent(); text != "" {
				system = append(system, wireContent{Type: "text", Text: text})
			}
			continue
		}
		if source.Role == message.RoleTool {
			blocks, err := toWireToolResult(source)
			if err != nil {
				return nil, nil, err
			}
			appendMessage("user", blocks)
			continue
		}
		role := "user"
		if source.Role == message.RoleAssistant {
			role = "assistant"
		} else if source.Role != message.RoleHuman {
			return nil, nil, fmt.Errorf("anthropic: unsupported message role %q", source.Role)
		}
		content, err := toWireContent(source.Content)
		if err != nil {
			return nil, nil, err
		}
		for _, call := range source.ToolCalls {
			if !json.Valid(call.Arguments) {
				return nil, nil, fmt.Errorf("anthropic: tool call %q has invalid JSON arguments", call.ID)
			}
			content = append(content, wireContent{Type: "tool_use", ID: call.ID, Name: call.Name, Input: append(json.RawMessage(nil), call.Arguments...)})
		}
		appendMessage(role, content)
	}
	return result, system, nil
}

func toWireContent(blocks []message.ContentBlock) ([]wireContent, error) {
	result := make([]wireContent, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case message.BlockText:
			result = append(result, wireContent{Type: "text", Text: block.Text})
		case message.BlockReasoning:
			item := wireContent{Type: "thinking", Thinking: block.Reasoning}
			if raw := block.Extra["signature"]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &item.Signature)
			}
			result = append(result, item)
		case message.BlockImage:
			source := &imageSource{}
			if block.URL != "" {
				source.Type, source.URL = "url", block.URL
			} else if len(block.Data) > 0 {
				source.Type, source.MediaType, source.Data = "base64", block.MIMEType, base64.StdEncoding.EncodeToString(block.Data)
			} else {
				return nil, fmt.Errorf("anthropic: image block requires URL or data")
			}
			result = append(result, wireContent{Type: "image", Source: source})
		case message.BlockNonStandard:
			var item wireContent
			if err := json.Unmarshal(block.NonStandard, &item); err != nil {
				return nil, fmt.Errorf("anthropic: decode non-standard block: %w", err)
			}
			result = append(result, item)
		}
	}
	return result, nil
}

func toWireToolResult(source message.Message) ([]wireContent, error) {
	content, err := toWireContent(source.Content)
	if err != nil {
		return nil, err
	}
	return []wireContent{{Type: "tool_result", ToolUseID: source.ToolCallID, Content: content, IsError: source.ToolStatus == message.ToolStatusError}}, nil
}

func (client *Client) do(ctx context.Context, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.options.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create request: %w", err)
	}
	request.Header.Set("x-api-key", client.apiKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	for key, values := range client.options.Headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.options.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request: %w", err)
	}
	return response, nil
}

func anthropicResponseError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &payload)
	detail := strings.TrimSpace(payload.Error.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(data))
	}
	return fmt.Errorf("anthropic: status %d: %s", response.StatusCode, detail)
}

type responsePayload struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Content    []wireContent `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      wireUsage     `json:"usage"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func normalizeResponse(payload responsePayload) (model.Response, error) {
	if payload.Error != nil {
		return model.Response{}, fmt.Errorf("anthropic: %s", payload.Error.Message)
	}
	result := message.Message{ID: payload.ID, Role: message.RoleAssistant}
	for _, item := range payload.Content {
		switch item.Type {
		case "text":
			block := message.ContentBlock{Type: message.BlockText, Text: item.Text}
			for _, citation := range item.Citations {
				block.Citations = append(block.Citations, message.Citation{URL: citation.URL, Title: citation.Title, StartIndex: citation.StartIndex, EndIndex: citation.EndIndex, CitedText: citation.CitedText})
			}
			result.Content = append(result.Content, block)
		case "thinking":
			block := message.ContentBlock{Type: message.BlockReasoning, Reasoning: item.Thinking}
			if item.Signature != "" {
				block.Extra = map[string]json.RawMessage{"signature": mustJSON(item.Signature)}
			}
			result.Content = append(result.Content, block)
		case "redacted_thinking":
			encoded, _ := json.Marshal(item)
			result.Content = append(result.Content, message.ContentBlock{Type: message.BlockNonStandard, NonStandard: encoded})
		case "tool_use", "server_tool_use":
			result.ToolCalls = append(result.ToolCalls, message.ToolCall{ID: item.ID, Name: item.Name, Arguments: normalizeArguments(item.Input)})
		default:
			encoded, _ := json.Marshal(item)
			result.Content = append(result.Content, message.ContentBlock{Type: message.BlockNonStandard, NonStandard: encoded})
		}
	}
	result.Usage = usageMessage(payload.Usage)
	result.ResponseMetadata = map[string]json.RawMessage{"anthropic.stop_reason": mustJSON(payload.StopReason)}
	return model.Response{Message: result}, nil
}

func normalizeArguments(value json.RawMessage) json.RawMessage {
	if len(value) == 0 || !json.Valid(value) {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), value...)
}

func usageMessage(usage wireUsage) *message.Usage {
	input := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	return &message.Usage{
		InputTokens: input, OutputTokens: usage.OutputTokens, TotalTokens: input + usage.OutputTokens,
		InputDetails: map[string]int{"cache_creation": usage.CacheCreationInputTokens, "cache_read": usage.CacheReadInputTokens},
	}
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

type messageStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	blocks  map[int]*wireContent
	usage   wireUsage
	done    bool
}

func newMessageStream(body io.ReadCloser) *messageStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &messageStream{body: body, scanner: scanner, blocks: map[int]*wireContent{}}
}

func (stream *messageStream) Next(ctx context.Context) (model.Chunk, error) {
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
		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			stream.Close()
			return model.Chunk{}, fmt.Errorf("anthropic: decode stream event: %w", err)
		}
		chunk, emit, err := stream.event(event)
		if err != nil {
			stream.Close()
			return model.Chunk{}, err
		}
		if emit {
			return chunk, nil
		}
	}
	if err := stream.scanner.Err(); err != nil {
		stream.Close()
		return model.Chunk{}, fmt.Errorf("anthropic: read stream: %w", err)
	}
	stream.done = true
	return model.Chunk{}, io.EOF
}

type streamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      responsePayload `json:"message"`
	ContentBlock wireContent     `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (stream *messageStream) event(event streamEvent) (model.Chunk, bool, error) {
	switch event.Type {
	case "message_start":
		stream.usage = event.Message.Usage
	case "content_block_start":
		block := event.ContentBlock
		stream.blocks[event.Index] = &block
		if block.Type == "text" && block.Text != "" {
			return model.Chunk{MessageDelta: message.Assistant(block.Text)}, true, nil
		}
	case "content_block_delta":
		block := stream.blocks[event.Index]
		if block == nil {
			block = &wireContent{}
			stream.blocks[event.Index] = block
		}
		switch event.Delta.Type {
		case "text_delta":
			block.Text += event.Delta.Text
			return model.Chunk{MessageDelta: message.Assistant(event.Delta.Text)}, true, nil
		case "thinking_delta":
			block.Thinking += event.Delta.Thinking
			index := event.Index
			return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: message.BlockReasoning, Reasoning: event.Delta.Thinking, Index: &index}}}}, true, nil
		case "input_json_delta":
			block.Input = append(block.Input, event.Delta.PartialJSON...)
		case "signature_delta":
			block.Signature += event.Delta.Signature
		}
	case "content_block_stop":
		block := stream.blocks[event.Index]
		if block != nil && (block.Type == "tool_use" || block.Type == "server_tool_use") {
			delete(stream.blocks, event.Index)
			return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: block.ID, Name: block.Name, Arguments: normalizeArguments(block.Input)}}}}, true, nil
		}
	case "message_delta":
		stream.usage.OutputTokens = event.Usage.OutputTokens
	case "message_stop":
		stream.done = true
		return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, Usage: usageMessage(stream.usage)}, Done: true}, true, nil
	case "error":
		if event.Error == nil {
			return model.Chunk{}, false, fmt.Errorf("anthropic: stream failed")
		}
		return model.Chunk{}, false, fmt.Errorf("anthropic: %s", event.Error.Message)
	}
	return model.Chunk{}, false, nil
}

func (stream *messageStream) Close() error {
	stream.done = true
	return stream.body.Close()
}

func cloneDefinitions(values []tool.Definition) []tool.Definition {
	result := make([]tool.Definition, len(values))
	for index, value := range values {
		result[index] = value
		result[index].InputSchema = append(json.RawMessage(nil), value.InputSchema...)
	}
	return result
}
