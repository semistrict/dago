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
	"time"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

const defaultAPIBaseURL = "https://api.openai.com/v1"

var ErrIncompleteStream = errors.New("openai: response stream ended before completion")

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
	// DefaultReasoning applies when a request does not carry an override. A
	// non-nil empty Reasoning on the request explicitly disables this default.
	DefaultReasoning *model.Reasoning
	// Store controls server-side response retention when explicitly set.
	Store *bool
	// WebSearch enables the provider-hosted web search tool. Provider-hosted
	// calls are returned as content blocks and are never dispatched through the
	// local Dago tool executor.
	WebSearch bool
	// RetryBackoff controls retries for transport failures, rate limits,
	// server errors, and incomplete JSON responses. Nil selects conservative
	// defaults; an explicitly empty slice disables provider-level retries.
	RetryBackoff []time.Duration
}

// Client is a Responses API-backed chat model.
type Client struct {
	options      Options
	credentials  CredentialSource
	boundTools   []tool.Definition
	subscription bool
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
	store := false
	options.Store = &store
	client, err := newClient(source, options)
	if err != nil {
		return nil, err
	}
	client.subscription = true
	return client, nil
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
	if options.Store == nil {
		store := false
		options.Store = &store
	}
	if options.DefaultReasoning != nil {
		reasoning := *options.DefaultReasoning
		options.DefaultReasoning = &reasoning
	}
	if options.RetryBackoff == nil {
		options.RetryBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	} else {
		options.RetryBackoff = append([]time.Duration(nil), options.RetryBackoff...)
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
	defaultReasoning := ""
	if client.options.DefaultReasoning != nil {
		defaultReasoning = client.options.DefaultReasoning.Effort
	}
	return model.Profile{
		Provider: "openai", Model: client.options.Model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true,
		NativeStreaming: true, SupportsPromptCaching: true, SupportsReasoning: true,
		ReasoningLevels: []string{"none", "low", "medium", "high", "xhigh"}, DefaultReasoningLevel: defaultReasoning,
		SupportsImages: true, SupportsPDF: true, SupportsFiles: true,
		SupportsWebSearch: client.options.WebSearch,
	}
}

func (client *Client) CountTokens(_ context.Context, messages []message.Message) (int, error) {
	return message.ApproximateTokens(messages), nil
}

func (client *Client) Invoke(ctx context.Context, request model.Request) (model.Response, error) {
	if client.subscription {
		return client.invokeSubscription(ctx, request)
	}
	body, err := client.requestBody(request, false)
	if err != nil {
		return model.Response{}, err
	}
	for attempt := 0; ; attempt++ {
		response, err := client.do(ctx, body)
		if err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return model.Response{}, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return model.Response{}, err
			}
			continue
		}
		if err := client.decorateError(responseError(response)); err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return model.Response{}, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return model.Response{}, err
			}
			continue
		}
		var payload responsesResponse
		decodeErr := decodeJSON(response.Body, &payload)
		response.Body.Close()
		if decodeErr != nil {
			wrapped := fmt.Errorf("openai: decode response: %w", decodeErr)
			if !isRetryableDecodeError(decodeErr) || !client.canRetry(ctx, attempt, wrapped) {
				return model.Response{}, wrapped
			}
			if err := client.waitRetry(ctx, attempt, wrapped); err != nil {
				return model.Response{}, err
			}
			continue
		}
		return normalizeResponse(payload, request.ResponseFormat)
	}
}

func (client *Client) invokeSubscription(ctx context.Context, request model.Request) (model.Response, error) {
	stream, err := client.Stream(ctx, request)
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
		mergeChunk(&response, chunk)
	}
	if request.ResponseFormat != nil && len(response.Message.ToolCalls) == 0 {
		text := strings.TrimSpace(response.Message.TextContent())
		if !json.Valid([]byte(text)) {
			return model.Response{}, fmt.Errorf("openai: structured response is not valid JSON")
		}
		response.Structured = json.RawMessage(text)
	}
	return response, nil
}

func mergeChunk(response *model.Response, chunk model.Chunk) {
	delta := chunk.MessageDelta
	if response.Message.ID == "" {
		response.Message.ID = delta.ID
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

func (client *Client) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	body, err := client.requestBody(request, true)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		response, err := client.do(ctx, body)
		if err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return nil, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return nil, err
			}
			continue
		}
		if err := client.decorateError(responseError(response)); err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return nil, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return nil, err
			}
			continue
		}
		return newResponseStream(response.Body), nil
	}
}

func (client *Client) canRetry(ctx context.Context, attempt int, err error) bool {
	if ctx.Err() != nil || attempt >= len(client.options.RetryBackoff) {
		return false
	}
	var providerErr *Error
	if errors.As(err, &providerErr) {
		return providerErr.Status == http.StatusTooManyRequests || providerErr.Status >= 500
	}
	return true
}

func (client *Client) waitRetry(ctx context.Context, attempt int, retryErr error) error {
	delay := client.options.RetryBackoff[attempt]
	var providerErr *Error
	if errors.As(retryErr, &providerErr) && providerErr.RetryAfter > delay {
		delay = providerErr.RetryAfter
	}
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (client *Client) decorateError(err error) error {
	var providerErr *Error
	if errors.As(err, &providerErr) {
		providerErr.Provider = "openai"
		providerErr.Model = client.options.Model
		providerErr.URL = client.options.BaseURL + "/responses"
	}
	return err
}

func isRetryableDecodeError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
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
	Instructions         string         `json:"instructions,omitempty"`
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
	Reasoning            *reasoning     `json:"reasoning,omitempty"`
	Include              []string       `json:"include,omitempty"`
	Store                *bool          `json:"store,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
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
	var instructions []string
	for _, item := range request.Messages {
		if item.Role == message.RoleSystem {
			if text := strings.TrimSpace(item.TextContent()); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
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
	if client.options.WebSearch {
		tools = append(tools, responseTool{Type: "web_search"})
	}
	maxOutputTokens := client.options.MaxOutputTokens
	if client.subscription {
		maxOutputTokens = 0
	}
	payload := responsesRequest{
		Model: client.options.Model, Instructions: strings.Join(instructions, "\n\n"), Input: input, Tools: tools, Stream: stream,
		MaxOutputTokens: maxOutputTokens, ParallelToolCalls: len(tools) > 0,
		Store: client.options.Store,
	}
	reasoningRequest := request.Reasoning
	if reasoningRequest == nil {
		reasoningRequest = client.options.DefaultReasoning
	}
	if reasoningRequest != nil && (reasoningRequest.Effort != "" || reasoningRequest.Summary != "") {
		payload.Reasoning = &reasoning{Effort: reasoningRequest.Effort, Summary: reasoningRequest.Summary}
	}
	if client.subscription || (reasoningRequest != nil && reasoningRequest.Summary != "") {
		payload.Include = []string{"reasoning.encrypted_content"}
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

const reasoningStateKey = "openai.responses.reasoning"

type reasoningState struct {
	ID               string            `json:"id"`
	Summary          []responseSummary `json:"summary"`
	EncryptedContent string            `json:"encrypted_content"`
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
		var output any = value.TextContent()
		hasRichContent := false
		contents := make([]any, 0, len(value.Content))
		for _, block := range value.Content {
			switch block.Type {
			case message.BlockText:
				contents = append(contents, map[string]any{"type": "input_text", "text": block.Text})
			case message.BlockImage:
				hasRichContent = true
				imageURL := block.URL
				if imageURL == "" && len(block.Data) > 0 {
					imageURL = "data:" + block.MIMEType + ";base64," + encodeBase64(block.Data)
				}
				contents = append(contents, map[string]any{"type": "input_image", "image_url": imageURL})
			}
		}
		if hasRichContent {
			output = contents
		} else if len(value.Artifact) > 0 && output == "" {
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
		case message.BlockReasoning:
			if value.Role != message.RoleAssistant {
				return nil, fmt.Errorf("openai: reasoning content is only supported in assistant messages")
			}
			raw := block.Extra[reasoningStateKey]
			if len(raw) == 0 {
				// Display-only reasoning cannot be replayed safely. The provider
				// requires its opaque encrypted state, not summary prose.
				continue
			}
			var state reasoningState
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, fmt.Errorf("openai: decode reasoning state: %w", err)
			}
			if state.EncryptedContent == "" {
				continue
			}
			summary := state.Summary
			if summary == nil {
				// Responses requires reasoning-item summaries to be arrays. Older
				// persisted turns (and valid responses with no visible summary)
				// decode to a nil slice; encoding that value as null makes replay
				// fail with invalid_type.
				summary = []responseSummary{}
			}
			items = append(items, map[string]any{
				"type": "reasoning", "id": state.ID, "summary": summary,
				"encrypted_content": state.EncryptedContent,
			})
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
	ID                string                     `json:"id"`
	Status            string                     `json:"status"`
	IncompleteDetails *responseIncompleteDetails `json:"incomplete_details,omitempty"`
	Output            []responseOutput           `json:"output"`
	Usage             responseUsage              `json:"usage"`
	Error             *apiError                  `json:"error,omitempty"`
}

type responseIncompleteDetails struct {
	Reason string `json:"reason"`
}

type responseOutput struct {
	Type             string            `json:"type"`
	ID               string            `json:"id"`
	Role             string            `json:"role"`
	CallID           string            `json:"call_id"`
	Name             string            `json:"name"`
	Arguments        json.RawMessage   `json:"arguments"`
	Content          []responseContent `json:"content"`
	Summary          []responseSummary `json:"summary,omitempty"`
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	Action           *responseAction   `json:"action,omitempty"`
}

type responseAction struct {
	Type    string   `json:"type,omitempty"`
	Queries []string `json:"queries,omitempty"`
}

type responseSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseContent struct {
	Type        string               `json:"type"`
	Text        string               `json:"text"`
	Refusal     string               `json:"refusal"`
	Annotations []responseAnnotation `json:"annotations"`
}

type responseAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex *int   `json:"start_index,omitempty"`
	EndIndex   *int   `json:"end_index,omitempty"`
	CitedText  string `json:"cited_text,omitempty"`
}

type responseUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  *responseInputTokenDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails *responseOutputTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                         `json:"total_tokens"`
}

type responseInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responseOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func normalizeResponse(payload responsesResponse, format *model.ResponseFormat) (model.Response, error) {
	if payload.Error != nil {
		return model.Response{}, apiErrorValue(payload.Error, http.StatusOK)
	}
	result := message.Message{ID: payload.ID, Role: message.RoleAssistant}
	var refusalText string
	for _, output := range payload.Output {
		switch output.Type {
		case "reasoning":
			result.Content = append(result.Content, reasoningContentBlock(output))
		case "message":
			if output.ID != "" {
				result.ID = output.ID
			}
			for _, content := range output.Content {
				switch content.Type {
				case "output_text":
					block := message.ContentBlock{Type: message.BlockText, Text: content.Text}
					for _, annotation := range content.Annotations {
						if annotation.Type == "url_citation" {
							block.Citations = append(block.Citations, message.Citation{URL: annotation.URL, Title: annotation.Title, StartIndex: annotation.StartIndex, EndIndex: annotation.EndIndex, CitedText: annotation.CitedText})
						}
					}
					result.Content = append(result.Content, block)
				case "refusal":
					refusalText = content.Refusal
					result.Content = append(result.Content, message.ContentBlock{Type: message.BlockNonStandard, NonStandard: mustJSON(map[string]any{"type": "refusal", "refusal": content.Refusal})})
				}
			}
		case "function_call":
			arguments, err := normalizeToolArguments(output.Arguments)
			if err != nil {
				result.InvalidToolCalls = append(result.InvalidToolCalls, message.InvalidToolCall{ID: output.CallID, Name: output.Name, Arguments: arguments, Error: "invalid JSON arguments"})
				continue
			}
			result.ToolCalls = append(result.ToolCalls, message.ToolCall{ID: output.CallID, Name: output.Name, Arguments: arguments})
		case "web_search_call":
			arguments := map[string]any{}
			if output.Action != nil {
				switch len(output.Action.Queries) {
				case 1:
					arguments["query"] = output.Action.Queries[0]
				default:
					if len(output.Action.Queries) > 1 {
						arguments["queries"] = output.Action.Queries
					}
				}
			}
			result.Content = append(result.Content, message.ContentBlock{Type: message.BlockServerTool, ID: output.ID, Name: "web_search", Extra: map[string]json.RawMessage{"arguments": mustJSON(arguments)}})
		}
	}
	finishReason := model.FinishReasonStop
	switch {
	case refusalText != "":
		model.SetOutcome(&result, model.FinishReasonRefusal, &model.Refusal{Explanation: refusalText})
	case payload.Status == "incomplete" && payload.IncompleteDetails != nil && payload.IncompleteDetails.Reason == "max_output_tokens":
		model.SetOutcome(&result, model.FinishReasonMaxTokens, nil)
	case len(result.ToolCalls) > 0:
		model.SetOutcome(&result, model.FinishReasonToolCalls, nil)
	default:
		model.SetOutcome(&result, finishReason, nil)
	}
	if payload.Usage.TotalTokens != 0 || payload.Usage.InputTokens != 0 || payload.Usage.OutputTokens != 0 {
		cached := 0
		if payload.Usage.InputTokensDetails != nil {
			cached = payload.Usage.InputTokensDetails.CachedTokens
		}
		reasoning := 0
		if payload.Usage.OutputTokensDetails != nil {
			reasoning = payload.Usage.OutputTokensDetails.ReasoningTokens
		}
		result.Usage = &message.Usage{
			InputTokens: max(0, payload.Usage.InputTokens-cached), OutputTokens: payload.Usage.OutputTokens,
			TotalTokens: payload.Usage.TotalTokens,
		}
		if cached > 0 {
			result.Usage.InputDetails = map[string]int{"cache_read": cached}
		}
		if reasoning > 0 {
			result.Usage.OutputDetails = map[string]int{"reasoning": reasoning}
		}
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

func reasoningContentBlock(output responseOutput) message.ContentBlock {
	return reasoningContentBlockWithText(output, true)
}

func reasoningContentBlockWithText(output responseOutput, includeText bool) message.ContentBlock {
	parts := make([]string, 0, len(output.Summary))
	if includeText {
		for _, summary := range output.Summary {
			if summary.Text != "" {
				parts = append(parts, summary.Text)
			}
		}
	}
	state := reasoningState{ID: output.ID, Summary: append([]responseSummary(nil), output.Summary...), EncryptedContent: output.EncryptedContent}
	raw, _ := json.Marshal(state)
	return message.ContentBlock{
		Type: message.BlockReasoning, ID: output.ID, Reasoning: strings.Join(parts, "\n"),
		Extra: map[string]json.RawMessage{reasoningStateKey: raw},
	}
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type Error struct {
	Status     int
	Code       string
	Type       string
	Message    string
	Provider   string
	Model      string
	URL        string
	RetryAfter time.Duration
}

func (err *Error) Error() string {
	if err.Code != "" {
		return fmt.Sprintf("openai: status %d (%s): %s", err.Status, err.Code, err.Message)
	}
	return fmt.Sprintf("openai: status %d: %s", err.Status, err.Message)
}

func (err *Error) RetryEvent(attempt int, delay time.Duration) model.RetryEvent {
	if err.RetryAfter > delay {
		delay = err.RetryAfter
	}
	return model.RetryEvent{
		Attempt: attempt, Delay: delay, Retryable: err.Status == http.StatusTooManyRequests || err.Status >= 500,
		Err: err.Message, Status: err.Status,
		Provider: err.Provider, Model: err.Model,
	}
}

func responseError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if err != nil {
		return fmt.Errorf("openai: read error response: %w", err)
	}
	var envelope struct {
		Error *apiError `json:"error"`
	}
	if json.Unmarshal(limited, &envelope) != nil || envelope.Error == nil {
		envelope.Error = &apiError{Message: strings.TrimSpace(string(limited))}
	}
	err = apiErrorValue(envelope.Error, response.StatusCode)
	var providerErr *Error
	if errors.As(err, &providerErr) {
		providerErr.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	}
	return err
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
		return seconds
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

func apiErrorValue(value *apiError, status int) error {
	err := &Error{Status: status, Code: value.Code, Type: value.Type, Message: value.Message}
	if value.Code == "context_length_exceeded" || value.Code == "context_window_exceeded" {
		return errors.Join(model.ErrContextOverflow, err)
	}
	return err
}

type responseStream struct {
	body             io.ReadCloser
	scanner          *bufio.Scanner
	queued           []model.Chunk
	calls            map[string]responseOutput
	done             bool
	data             []string
	emittedText      strings.Builder
	emittedCalls     map[string]struct{}
	emittedReasoning map[string]string
	emittedServer    map[string]struct{}
}

func newResponseStream(body io.ReadCloser) *responseStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &responseStream{
		body: body, scanner: scanner, calls: map[string]responseOutput{},
		emittedCalls: map[string]struct{}{}, emittedReasoning: map[string]string{}, emittedServer: map[string]struct{}{},
	}
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
		if line == "" {
			chunk, emit, err := stream.flushEvent()
			if err != nil {
				stream.Close()
				return model.Chunk{}, err
			}
			if emit {
				return chunk, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			stream.data = append(stream.data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := stream.scanner.Err(); err != nil {
		stream.Close()
		return model.Chunk{}, fmt.Errorf("openai: read stream: %w", err)
	}
	chunk, emit, err := stream.flushEvent()
	if err != nil {
		stream.Close()
		return model.Chunk{}, err
	}
	if emit {
		return chunk, nil
	}
	stream.Close()
	return model.Chunk{}, ErrIncompleteStream
}

func (stream *responseStream) flushEvent() (model.Chunk, bool, error) {
	if len(stream.data) == 0 {
		return model.Chunk{}, false, nil
	}
	data := strings.Join(stream.data, "\n")
	stream.data = nil
	if strings.TrimSpace(data) == "[DONE]" {
		stream.done = true
		return model.Chunk{Done: true}, true, nil
	}
	return stream.event([]byte(data))
}

func (stream *responseStream) event(data []byte) (model.Chunk, bool, error) {
	var envelope struct {
		Type        string            `json:"type"`
		Delta       string            `json:"delta"`
		Item        responseOutput    `json:"item"`
		ItemID      string            `json:"item_id"`
		OutputIndex int               `json:"output_index"`
		Arguments   json.RawMessage   `json:"arguments"`
		Response    responsesResponse `json:"response"`
		Error       *apiError         `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return model.Chunk{}, false, fmt.Errorf("openai: decode stream event: %w", err)
	}
	switch envelope.Type {
	case "response.output_text.delta":
		stream.emittedText.WriteString(envelope.Delta)
		return model.Chunk{MessageDelta: message.Assistant(envelope.Delta)}, true, nil
	case "response.reasoning_summary_text.delta":
		stream.emittedReasoning[envelope.ItemID] += envelope.Delta
		index := envelope.OutputIndex
		return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{
			Type: message.BlockReasoning, ID: envelope.ItemID, Reasoning: envelope.Delta, Index: &index,
		}}}}, true, nil
	case "response.output_item.added":
		if envelope.Item.Type == "function_call" {
			stream.calls[envelope.Item.ID] = envelope.Item
		}
	case "response.output_item.done":
		if envelope.Item.Type == "reasoning" {
			stream.emittedReasoning[envelope.Item.ID] = reasoningText(envelope.Item.Summary)
			return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{
				reasoningContentBlockWithText(envelope.Item, false),
			}}}, true, nil
		}
		if envelope.Item.Type == "web_search_call" {
			stream.emittedServer[envelope.Item.ID] = struct{}{}
			arguments := map[string]any{}
			if envelope.Item.Action != nil {
				if len(envelope.Item.Action.Queries) == 1 {
					arguments["query"] = envelope.Item.Action.Queries[0]
				} else if len(envelope.Item.Action.Queries) > 1 {
					arguments["queries"] = envelope.Item.Action.Queries
				}
			}
			return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{{
				Type: message.BlockServerTool, ID: envelope.Item.ID, Name: "web_search",
				Extra: map[string]json.RawMessage{"arguments": mustJSON(arguments)},
			}}}}, true, nil
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
		arguments, err := normalizeToolArguments(call.Arguments)
		if err != nil {
			return model.Chunk{}, false, fmt.Errorf("openai: streamed tool arguments for %q are invalid JSON", call.CallID)
		}
		stream.emittedCalls[call.CallID] = struct{}{}
		return model.Chunk{MessageDelta: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: call.CallID, Name: call.Name, Arguments: arguments}}}}, true, nil
	case "response.completed":
		stream.done = true
		return stream.completedChunk(envelope.Response)
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

func (stream *responseStream) completedChunk(payload responsesResponse) (model.Chunk, bool, error) {
	response, err := normalizeResponse(payload, nil)
	if err != nil {
		return model.Chunk{}, false, err
	}
	delta := response.Message
	delta.Content = nil
	for _, block := range response.Message.Content {
		switch block.Type {
		case message.BlockText:
			prefix := stream.emittedText.String()
			if strings.HasPrefix(block.Text, prefix) {
				block.Text = strings.TrimPrefix(block.Text, prefix)
			}
			if block.Text != "" || len(block.Citations) > 0 || len(block.Extra) > 0 {
				delta.Content = append(delta.Content, block)
			}
		case message.BlockReasoning:
			if prior := stream.emittedReasoning[block.ID]; prior != "" && strings.HasPrefix(block.Reasoning, prior) {
				block.Reasoning = strings.TrimPrefix(block.Reasoning, prior)
			}
			delta.Content = append(delta.Content, block)
		case message.BlockServerTool:
			if _, emitted := stream.emittedServer[block.ID]; !emitted {
				delta.Content = append(delta.Content, block)
			}
		default:
			delta.Content = append(delta.Content, block)
		}
	}
	delta.ToolCalls = nil
	for _, call := range response.Message.ToolCalls {
		if _, emitted := stream.emittedCalls[call.ID]; !emitted {
			delta.ToolCalls = append(delta.ToolCalls, call)
		}
	}
	return model.Chunk{MessageDelta: delta, Done: true}, true, nil
}

func reasoningText(items []responseSummary) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n")
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
		result[index].Extra = cloneRawMap(value.Extra)
	}
	return result
}

func cloneRawMap(value map[string]json.RawMessage) map[string]json.RawMessage {
	if value == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(value))
	for key, item := range value {
		result[key] = append(json.RawMessage(nil), item...)
	}
	return result
}

func normalizeToolArguments(value json.RawMessage) (json.RawMessage, error) {
	if len(value) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if value[0] == '"' {
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return value, err
		}
		value = json.RawMessage(decoded)
	}
	if !json.Valid(value) {
		return value, fmt.Errorf("invalid JSON")
	}
	return append(json.RawMessage(nil), value...), nil
}

func decodeJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 16<<20))
	return decoder.Decode(value)
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
