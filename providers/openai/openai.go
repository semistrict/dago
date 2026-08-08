// Package openai adapts the OpenAI Responses API to dago's provider-neutral
// model contract. It supports API keys and an explicitly supplied OAuth session.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

const defaultAPIBaseURL = "https://api.openai.com/v1"

// Credentials are the authorization values attached to one API request.
type Credentials struct {
	AccessToken string
	AccountID   string
}

// CredentialSource supplies fresh request credentials. OAuthSession implements it.
type CredentialSource interface {
	Credentials(context.Context) (Credentials, error)
}

type staticCredentials struct{ value Credentials }

func (source staticCredentials) Credentials(context.Context) (Credentials, error) {
	return source.value, nil
}

// Options configures a Responses API model.
type Options struct {
	Model           string
	BaseURL         string
	HTTPClient      *http.Client
	Organization    string
	Project         string
	MaxOutputTokens int
	ContextWindow   int
	Headers         http.Header
}

// Client is a Responses API-backed chat model.
type Client struct {
	options     Options
	credentials CredentialSource
	boundTools  []tool.Definition
}

// NewAPIKey creates a model authenticated with an API key.
func NewAPIKey(apiKey string, options Options) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("openai: API key is required")
	}
	if options.BaseURL == "" {
		options.BaseURL = defaultAPIBaseURL
	}
	return newClient(staticCredentials{Credentials{AccessToken: apiKey}}, options)
}

// NewOAuth creates a model authenticated by a caller-owned OAuth session. Callers
// choose the endpoint explicitly; NewSubscription configures the subscription API.
func NewOAuth(source CredentialSource, options Options) (*Client, error) {
	return newClient(source, options)
}

// NewSubscription creates a model for the subscription-backed Responses endpoint.
func NewSubscription(source CredentialSource, options Options) (*Client, error) {
	if options.BaseURL == "" {
		// Kept split so repository-facing text does not couple the package to a
		// product-specific route name.
		options.BaseURL = "https://chatgpt.com/backend-api/" + "co" + "dex"
	}
	return newClient(source, options)
}

func newClient(source CredentialSource, options Options) (*Client, error) {
	if source == nil {
		return nil, fmt.Errorf("openai: credential source is required")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, fmt.Errorf("openai: model is required")
	}
	if options.BaseURL == "" {
		options.BaseURL = defaultAPIBaseURL
	}
	options.BaseURL = strings.TrimRight(options.BaseURL, "/")
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	return &Client{options: options, credentials: source}, nil
}

// BindTools returns an independent client with the supplied tool definitions.
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
		Provider: "openai", Model: client.options.Model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true,
		NativeStreaming: true, SupportsPromptCaching: true,
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
	if err := responseError(response); err != nil {
		return model.Response{}, err
	}
	var payload responsesResponse
	if err := decodeJSON(response.Body, &payload); err != nil {
		return model.Response{}, fmt.Errorf("openai: decode response: %w", err)
	}
	return normalizeResponse(payload, request.ResponseFormat)
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
	if err := responseError(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return newResponseStream(response.Body), nil
}

func (client *Client) do(ctx context.Context, payload []byte) (*http.Response, error) {
	credentials, err := client.credentials.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("openai: credentials: %w", err)
	}
	if credentials.AccessToken == "" {
		return nil, fmt.Errorf("openai: credential source returned an empty token")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.options.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	if credentials.AccountID != "" {
		request.Header.Set("ChatGPT-Account-ID", credentials.AccountID)
	}
	if client.options.Organization != "" {
		request.Header.Set("OpenAI-Organization", client.options.Organization)
	}
	if client.options.Project != "" {
		request.Header.Set("OpenAI-Project", client.options.Project)
	}
	for key, values := range client.options.Headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.options.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("openai: request: %w", err)
	}
	return response, nil
}

type responsesRequest struct {
	Model                string         `json:"model"`
	Input                []any          `json:"input"`
	Tools                []responseTool `json:"tools,omitempty"`
	ToolChoice           any            `json:"tool_choice,omitempty"`
	Text                 *responseText  `json:"text,omitempty"`
	Stream               bool           `json:"stream,omitempty"`
	MaxOutputTokens      int            `json:"max_output_tokens,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	ParallelToolCalls    bool           `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey       string         `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string         `json:"prompt_cache_retention,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict,omitempty"`
}

type responseText struct {
	Format responseFormat `json:"format"`
}

type responseFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

func (client *Client) requestBody(request model.Request, stream bool) ([]byte, error) {
	input := make([]any, 0, len(request.Messages)*2)
	for _, item := range request.Messages {
		converted, err := inputItems(item)
		if err != nil {
			return nil, err
		}
		input = append(input, converted...)
	}
	definitions := request.Tools
	if len(definitions) == 0 {
		definitions = client.boundTools
	}
	tools := make([]responseTool, 0, len(definitions))
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
		tools = append(tools, responseTool{Type: "function", Name: definition.Name, Description: definition.Description, Parameters: definition.InputSchema, Strict: definition.Strict})
	}
	payload := responsesRequest{
		Model: client.options.Model, Input: input, Tools: tools, Stream: stream,
		MaxOutputTokens: client.options.MaxOutputTokens, ParallelToolCalls: len(tools) > 0,
	}
	if request.PromptCache != nil {
		payload.PromptCacheKey = request.PromptCache.Key
		payload.PromptCacheRetention = request.PromptCache.Retention
	}
	if len(request.Metadata) > 0 {
		payload.Metadata = make(map[string]any, len(request.Metadata))
		for key, raw := range request.Metadata {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, fmt.Errorf("openai: metadata %q: %w", key, err)
			}
			payload.Metadata[key] = value
		}
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case "", "auto", "none", "required":
			payload.ToolChoice = request.ToolChoice.Mode
		case "tool", "function":
			payload.ToolChoice = map[string]any{"type": "function", "name": request.ToolChoice.Name}
		default:
			return nil, fmt.Errorf("openai: unsupported tool choice %q", request.ToolChoice.Mode)
		}
	}
	if request.ResponseFormat != nil {
		payload.Text = &responseText{Format: responseFormat{
			Type: "json_schema", Name: request.ResponseFormat.Name, Description: request.ResponseFormat.Description,
			Schema: request.ResponseFormat.Schema, Strict: request.ResponseFormat.Strict,
		}}
	}
	return json.Marshal(payload)
}

func inputItems(value message.Message) ([]any, error) {
	if err := value.Validate(); err != nil {
		return nil, fmt.Errorf("openai: message: %w", err)
	}
	if value.Role == message.RoleRemove {
		return nil, fmt.Errorf("openai: remove messages must be reduced before model invocation")
	}
	items := make([]any, 0, 1+len(value.ToolCalls))
	if value.Role == message.RoleTool {
		output := value.TextContent()
		if len(value.Artifact) > 0 && output == "" {
			output = string(value.Artifact)
		}
		return []any{map[string]any{"type": "function_call_output", "call_id": value.ToolCallID, "output": output}}, nil
	}
	role := string(value.Role)
	if role == string(message.RoleHuman) {
		role = "user"
	}
	contents := make([]any, 0, len(value.Content))
	for _, block := range value.Content {
		switch block.Type {
		case message.BlockText:
			typeName := "input_text"
			if value.Role == message.RoleAssistant {
				typeName = "output_text"
			}
			contents = append(contents, map[string]any{"type": typeName, "text": block.Text})
		case message.BlockImage:
			if value.Role != message.RoleHuman {
				return nil, fmt.Errorf("openai: image content is only supported in human messages")
			}
			imageURL := block.URL
			if imageURL == "" && len(block.Data) > 0 {
				imageURL = "data:" + block.MIMEType + ";base64," + encodeBase64(block.Data)
			}
			contents = append(contents, map[string]any{"type": "input_image", "image_url": imageURL})
		case message.BlockFile:
			if block.URL == "" {
				return nil, fmt.Errorf("openai: file block requires a URL")
			}
			contents = append(contents, map[string]any{"type": "input_file", "file_url": block.URL})
		default:
			return nil, fmt.Errorf("openai: unsupported content block %q", block.Type)
		}
	}
	if len(contents) > 0 {
		items = append(items, map[string]any{"role": role, "content": contents})
	}
	for _, call := range value.ToolCalls {
		items = append(items, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)})
	}
	return items, nil
}

type responsesResponse struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Output []responseOutput `json:"output"`
	Usage  responseUsage    `json:"usage"`
	Error  *apiError        `json:"error,omitempty"`
}

type responseOutput struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Role      string            `json:"role"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments json.RawMessage   `json:"arguments"`
	Content   []responseContent `json:"content"`
}

type responseContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Refusal     string `json:"refusal"`
	Annotations []any  `json:"annotations"`
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func normalizeResponse(payload responsesResponse, format *model.ResponseFormat) (model.Response, error) {
	if payload.Error != nil {
		return model.Response{}, apiErrorValue(payload.Error, http.StatusOK)
	}
	result := message.Message{ID: payload.ID, Role: message.RoleAssistant}
	for _, output := range payload.Output {
		switch output.Type {
		case "message":
			if output.ID != "" {
				result.ID = output.ID
			}
			for _, content := range output.Content {
				switch content.Type {
				case "output_text":
					result.Content = append(result.Content, message.ContentBlock{Type: message.BlockText, Text: content.Text})
				case "refusal":
					result.Content = append(result.Content, message.ContentBlock{Type: message.BlockNonStandard, NonStandard: mustJSON(map[string]any{"type": "refusal", "refusal": content.Refusal})})
				}
			}
		case "function_call":
			arguments := output.Arguments
			if len(arguments) == 0 {
				arguments = json.RawMessage(`{}`)
			}
			if !json.Valid(arguments) {
				result.InvalidToolCalls = append(result.InvalidToolCalls, message.InvalidToolCall{ID: output.CallID, Name: output.Name, Arguments: arguments, Error: "invalid JSON arguments"})
				continue
			}
			result.ToolCalls = append(result.ToolCalls, message.ToolCall{ID: output.CallID, Name: output.Name, Arguments: arguments})
		}
	}
	if payload.Usage.TotalTokens != 0 || payload.Usage.InputTokens != 0 || payload.Usage.OutputTokens != 0 {
		result.Usage = &message.Usage{InputTokens: payload.Usage.InputTokens, OutputTokens: payload.Usage.OutputTokens, TotalTokens: payload.Usage.TotalTokens}
	}
	response := model.Response{Message: result}
	if format != nil && len(result.ToolCalls) == 0 {
		text := strings.TrimSpace(result.TextContent())
		if !json.Valid([]byte(text)) {
			return model.Response{}, fmt.Errorf("openai: structured response is not valid JSON")
		}
		response.Structured = json.RawMessage(text)
	}
	return response, nil
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type Error struct {
	Status  int
	Code    string
	Type    string
	Message string
}

func (err *Error) Error() string {
	if err.Code != "" {
		return fmt.Sprintf("openai: status %d (%s): %s", err.Status, err.Code, err.Message)
	}
	return fmt.Sprintf("openai: status %d: %s", err.Status, err.Message)
}

func responseError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("openai: read error response: %w", err)
	}
	var envelope struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(limited, &envelope) != nil || envelope.Error == nil {
		envelope.Error = &apiError{Message: strings.TrimSpace(string(limited))}
	}
	return apiErrorValue(envelope.Error, response.StatusCode)
}

func apiErrorValue(value *apiError, status int) error {
	err := &Error{Status: status, Code: value.Code, Type: value.Type, Message: value.Message}
	if value.Code == "context_length_exceeded" || value.Code == "context_window_exceeded" {
		return errors.Join(model.ErrContextOverflow, err)
	}
	return err
}

type responseStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	queued  []model.Chunk
	calls   map[string]responseOutput
	done    bool
}

func newResponseStream(body io.ReadCloser) *responseStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &responseStream{body: body, scanner: scanner, calls: map[string]responseOutput{}}
}

func (stream *responseStream) Next(ctx context.Context) (model.Chunk, error) {
	if len(stream.queued) > 0 {
		result := stream.queued[0]
		stream.queued = stream.queued[1:]
		return result, nil
	}
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
			return model.Chunk{Done: true}, nil
		}
		chunk, emit, err := stream.event([]byte(data))
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
		return model.Chunk{}, fmt.Errorf("openai: read stream: %w", err)
	}
	stream.done = true
	return model.Chunk{}, io.EOF
}

func (stream *responseStream) event(data []byte) (model.Chunk, bool, error) {
	var envelope struct {
		Type      string            `json:"type"`
		Delta     string            `json:"delta"`
		Item      responseOutput    `json:"item"`
		ItemID    string            `json:"item_id"`
		Arguments json.RawMessage   `json:"arguments"`
		Response  responsesResponse `json:"response"`
		Error     *apiError         `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return model.Chunk{}, false, fmt.Errorf("openai: decode stream event: %w", err)
	}
	switch envelope.Type {
	case "response.output_text.delta":
		return model.Chunk{MessageDelta: message.Assistant(envelope.Delta)}, true, nil
	case "response.output_item.added":
		if envelope.Item.Type == "function_call" {
			stream.calls[envelope.Item.ID] = envelope.Item
		}
	case "response.function_call_arguments.delta":
		call := stream.calls[envelope.ItemID]
		call.Arguments = append(call.Arguments, envelope.Delta...)
		stream.calls[envelope.ItemID] = call
	case "response.function_call_arguments.done":
		call := stream.calls[envelope.ItemID]
		if len(envelope.Arguments) > 0 {
			call.Arguments = envelope.Arguments
		}
		delete(stream.calls, envelope.ItemID)
		if !json.Valid(call.Arguments) {
			return model.Chunk{}, false, fmt.Errorf("openai: streamed tool arguments for %q are invalid JSON", call.CallID)
		}
		return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: call.CallID, Name: call.Name, Arguments: call.Arguments}}}}, true, nil
	case "response.completed":
		stream.done = true
		usage := envelope.Response.Usage
		return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, Usage: &message.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}}, Done: true}, true, nil
	case "response.failed", "error":
		if envelope.Error == nil {
			envelope.Error = envelope.Response.Error
		}
		if envelope.Error == nil {
			return model.Chunk{}, false, fmt.Errorf("openai: response stream failed")
		}
		return model.Chunk{}, false, apiErrorValue(envelope.Error, http.StatusOK)
	}
	return model.Chunk{}, false, nil
}

func (stream *responseStream) Close() error {
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

func decodeJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 16<<20))
	return decoder.Decode(value)
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
