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
	"iter"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

const (
	defaultAPIBaseURL                = "https://api.openai.com/v1"
	defaultServerCompactionThreshold = 200000
	responseOutputStateKey           = "openai.responses.output_item"
)

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
	BaseURL         string
	HTTPClient      *http.Client
	Organization    string
	Project         string
	MaxOutputTokens int
	ContextWindow   int
	Headers         http.Header
	// DefaultReasoning applies when a request does not carry an override. A
	// non-nil empty Reasoning on the request explicitly disables this default.
	DefaultReasoning *damodel.Reasoning
	// Store controls server-side response retention when explicitly set.
	Store *bool
	// ResponsesWebSocket controls persistent WebSocket transport for streamed
	// Responses API calls. Nil enables it for the standard API endpoints and
	// leaves custom BaseURL values on HTTP. Set it explicitly to override that
	// default. Compatible successive calls send only new input items.
	ResponsesWebSocket *bool
	// ServerCompaction controls Responses API server-side compaction. Nil enables
	// it for standard API endpoints and subscription clients, while leaving other
	// custom BaseURL values unchanged. Set it explicitly to override that default.
	ServerCompaction *bool
	// CompactionThreshold is the approximate rendered-token threshold that sends
	// a remote compaction trigger before inference. Zero derives 90% of
	// ContextWindow, or 200,000 when ContextWindow is unknown. Positive overrides
	// are clamped to 90% of a known ContextWindow and also opt custom endpoints in
	// unless ServerCompaction is explicitly false.
	CompactionThreshold int
	// WebSearch enables the provider-hosted web search tool. Provider-hosted
	// calls are returned as content blocks and are never dispatched through the
	// local dago tool executor.
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
	boundTools   []datool.Definition
	subscription bool
	websockets   *responsesWebSocketPool
	provider     string
	model        string
}

// NewAPIKey creates a model authenticated with an API key. Construction does
// no I/O; missing required values and invalid static options panic.
func NewAPIKey(apiKey, model string, options Options) *Client {
	return newAPIKey(apiKey, "openai", model, options)
}

// NewCompatibleAPIKey creates a model for a service that implements the OpenAI
// Responses wire protocol. Provider identifies that service in profiles,
// errors, and retry events. Provider-specific adapters should normally expose
// their own options and constructor instead of asking applications to call this
// function directly.
func NewCompatibleAPIKey(apiKey, provider, model string, options Options) *Client {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		panic("openai: compatible provider is required")
	}
	return newAPIKey(apiKey, provider, model, options)
}

func newAPIKey(apiKey, provider, model string, options Options) *Client {
	if strings.TrimSpace(apiKey) == "" {
		panic(provider + ": API key is required")
	}
	return newClient(staticCredentials{Credentials{AccessToken: apiKey}}, model, options, provider)
}

// NewOAuth creates a model authenticated by a caller-owned OAuth session. Callers
// choose the endpoint explicitly; NewSubscription configures the subscription API.
// A nil or typed-nil source panics.
func NewOAuth(source CredentialSource, model string, options Options) *Client {
	return newClient(source, model, options, "openai")
}

// NewSubscription creates a model for the subscription-backed Responses endpoint.
func NewSubscription(source CredentialSource, model string, options Options) *Client {
	if options.BaseURL == "" {
		// Kept split so repository-facing text does not couple the package to a
		// product-specific route name.
		options.BaseURL = "https://chatgpt.com/backend-api/" + "co" + "dex"
		if options.ResponsesWebSocket == nil {
			options.ResponsesWebSocket = new(true)
		}
	}
	if options.ServerCompaction == nil {
		options.ServerCompaction = new(true)
	}
	store := false
	options.Store = &store
	client := newClient(source, model, options, "openai")
	client.subscription = true
	return client
}

func newClient(source CredentialSource, model string, options Options, provider string) *Client {
	if nilInterface(source) {
		panic(provider + ": credential source is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		panic(provider + ": model is required")
	}
	if options.MaxOutputTokens < 0 || options.ContextWindow < 0 || options.CompactionThreshold < 0 {
		panic("openai: token limits and compaction threshold must not be negative")
	}
	for _, delay := range options.RetryBackoff {
		if delay < 0 {
			panic("openai: retry backoff must not be negative")
		}
	}
	standardEndpoint := options.BaseURL == "" || strings.TrimRight(options.BaseURL, "/") == defaultAPIBaseURL
	if options.BaseURL == "" {
		options.BaseURL = defaultAPIBaseURL
	}
	options.BaseURL = strings.TrimRight(options.BaseURL, "/")
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	options.Headers = options.Headers.Clone()
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
	client := &Client{options: options, credentials: source, provider: provider, model: model}
	websocketEnabled := standardEndpoint
	if options.ResponsesWebSocket != nil {
		websocketEnabled = *options.ResponsesWebSocket
	}
	if websocketEnabled {
		client.websockets = &responsesWebSocketPool{}
	}
	serverCompactionEnabled := standardEndpoint || options.CompactionThreshold > 0
	if options.ServerCompaction != nil {
		serverCompactionEnabled = *options.ServerCompaction
	}
	options.ServerCompaction = new(serverCompactionEnabled)
	if serverCompactionEnabled {
		maximum := 0
		if options.ContextWindow > 0 {
			maximum = max(1, options.ContextWindow*9/10)
		}
		if options.CompactionThreshold == 0 {
			options.CompactionThreshold = defaultServerCompactionThreshold
			if maximum > 0 {
				options.CompactionThreshold = maximum
			}
		} else if maximum > 0 {
			options.CompactionThreshold = min(options.CompactionThreshold, maximum)
		}
	}
	client.options = options
	return client
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// BindTools returns an independent client with the supplied tool definitions.
func (client *Client) BindTools(definitions []datool.Definition) (damodel.Chat, error) {
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
	}
	copy := *client
	copy.boundTools = cloneDefinitions(definitions)
	return &copy, nil
}

func (client *Client) Profile() damodel.Profile {
	defaultReasoning := ""
	if client.options.DefaultReasoning != nil {
		defaultReasoning = client.options.DefaultReasoning.Effort
	}
	return damodel.Profile{
		Provider: client.provider, Model: client.model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true,
		NativeStreaming: true, SupportsPromptCaching: true, SupportsSeparateSystemMessage: true, SupportsReasoning: true,
		ReasoningLevels: []string{"none", "low", "medium", "high", "xhigh"}, DefaultReasoningLevel: defaultReasoning,
		SupportsImages: true, SupportsPDF: true, SupportsFiles: true,
		SupportsWebSearch: client.options.WebSearch,
	}
}

func (client *Client) CountTokens(_ context.Context, messages []damessage.Message) (int, error) {
	return damessage.ApproximateTokens(messages), nil
}

// Prewarm prepares the persistent Responses WebSocket with the request's
// instructions, tools, and input without generating model output. The next
// compatible call continues from the warmed response state.
func (client *Client) Prewarm(ctx context.Context, request damodel.Request) error {
	payload, err := client.requestPayload(request, true)
	if err != nil {
		return err
	}
	if client.websockets == nil || !client.websockets.enabled() {
		return fmt.Errorf("openai: websocket prewarm requires websocket transport")
	}
	generate := false
	stream, err := client.streamWebSocketRequest(ctx, payload, &generate)
	if err != nil {
		return err
	}
	defer stream.Close()
	completed := false
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			return nextErr
		}
		completed = completed || chunk.Done
	}
	if !completed {
		return ErrIncompleteStream
	}
	return nil
}

func (client *Client) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	if client.subscription || client.websockets != nil {
		return client.invokeStream(ctx, request)
	}
	body, err := client.requestBody(request, false)
	if err != nil {
		return damodel.Response{}, err
	}
	for attempt := 0; ; attempt++ {
		response, err := client.do(ctx, body)
		if err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return damodel.Response{}, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return damodel.Response{}, err
			}
			continue
		}
		if err := client.decorateError(responseErrorForProvider(response, client.provider)); err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return damodel.Response{}, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return damodel.Response{}, err
			}
			continue
		}
		var payload responsesResponse
		decodeErr := decodeJSON(response.Body, &payload)
		response.Body.Close()
		if decodeErr != nil {
			wrapped := fmt.Errorf("%s: decode response: %w", client.provider, decodeErr)
			if !isRetryableDecodeError(decodeErr) || !client.canRetry(ctx, attempt, wrapped) {
				return damodel.Response{}, wrapped
			}
			if err := client.waitRetry(ctx, attempt, wrapped); err != nil {
				return damodel.Response{}, err
			}
			continue
		}
		return normalizeResponse(payload, request.ResponseFormat, client.provider)
	}
}

func (client *Client) invokeStream(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	stream, err := client.Stream(ctx, request)
	if err != nil {
		return damodel.Response{}, err
	}
	response := damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant}}
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			return damodel.Response{}, nextErr
		}
		mergeChunk(&response, chunk)
	}
	if request.ResponseFormat != nil && len(response.Message.ToolCalls) == 0 {
		text := strings.TrimSpace(response.Message.TextContent())
		if !json.Valid([]byte(text)) {
			return damodel.Response{}, fmt.Errorf("%s: structured response is not valid JSON", client.provider)
		}
		response.Structured = json.RawMessage(text)
	}
	return response, nil
}

func mergeChunk(response *damodel.Response, chunk damodel.Chunk) {
	delta := chunk.MessageDelta
	if response.Message.ID == "" {
		response.Message.ID = delta.ID
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

func (client *Client) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	payload, err := client.requestPayload(request, true)
	if err != nil {
		return nil, err
	}
	var compaction json.RawMessage
	if client.shouldCompact(payload) {
		payload.Input, compaction, err = client.compactInput(ctx, payload)
		if err != nil {
			return nil, err
		}
	}
	stream, err := client.streamPayload(ctx, payload)
	if err != nil {
		return nil, err
	}
	if len(compaction) > 0 {
		stream = newCompactionStateStream(ctx, stream, compaction)
	}
	return stream, nil
}

func (client *Client) streamPayload(ctx context.Context, payload responsesRequest) (damodel.Stream, error) {
	if client.websockets != nil && client.websockets.enabled() {
		stream, websocketErr := client.streamWebSocket(ctx, payload)
		if websocketErr == nil {
			return stream, nil
		}
		if ctx.Err() != nil {
			return nil, websocketErr
		}
		client.websockets.disable()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
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
		if err := client.decorateError(responseErrorForProvider(response, client.provider)); err != nil {
			if !client.canRetry(ctx, attempt, err) {
				return nil, err
			}
			if err := client.waitRetry(ctx, attempt, err); err != nil {
				return nil, err
			}
			continue
		}
		return newRetryingResponseStream(ctx, client, body, response.Body, responseFormatFromPayload(payload), attempt), nil
	}
}

type retryingResponseStream struct {
	ctx          context.Context
	client       *Client
	body         []byte
	format       *damodel.ResponseFormat
	current      *responseStream
	retryAttempt int
	emitted      bool
}

func newRetryingResponseStream(ctx context.Context, client *Client, body []byte, responseBody io.ReadCloser, format *damodel.ResponseFormat, retryAttempt int) *retryingResponseStream {
	return &retryingResponseStream{
		ctx: ctx, client: client, body: append([]byte(nil), body...), format: format,
		current: newResponseStream(ctx, responseBody, client.provider, format), retryAttempt: retryAttempt,
	}
}

func (stream *retryingResponseStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *retryingResponseStream) Next(ctx context.Context) (damodel.Chunk, error) {
	for {
		chunk, err := stream.current.Next(ctx)
		if err == nil {
			stream.emitted = true
			return chunk, nil
		}
		if errors.Is(err, io.EOF) || stream.emitted {
			return damodel.Chunk{}, err
		}
		if retryErr := stream.retryBeforeEmission(ctx, err); retryErr != nil {
			return damodel.Chunk{}, retryErr
		}
	}
}

func (stream *retryingResponseStream) retryBeforeEmission(ctx context.Context, retryErr error) error {
	_ = stream.current.Close()
	for {
		attempt := stream.retryAttempt
		if !stream.client.canRetry(ctx, attempt, retryErr) {
			return retryErr
		}
		if err := stream.client.waitRetry(ctx, attempt, retryErr); err != nil {
			return err
		}
		stream.retryAttempt++
		response, err := stream.client.do(ctx, stream.body)
		if err != nil {
			retryErr = err
			continue
		}
		if err := stream.client.decorateError(responseErrorForProvider(response, stream.client.provider)); err != nil {
			retryErr = err
			continue
		}
		stream.current = newResponseStream(ctx, response.Body, stream.client.provider, stream.format)
		return nil
	}
}

func (stream *retryingResponseStream) Close() error {
	return stream.current.Close()
}

var _ damodel.Stream = (*retryingResponseStream)(nil)

func (client *Client) canRetry(ctx context.Context, attempt int, err error) bool {
	if ctx.Err() != nil || attempt >= len(client.options.RetryBackoff) {
		return false
	}
	var providerErr *Error
	if errors.As(err, &providerErr) {
		return providerErr.retryableError()
	}
	return true
}

func (client *Client) waitRetry(ctx context.Context, attempt int, retryErr error) error {
	delay := client.options.RetryBackoff[attempt]
	var providerErr *Error
	if errors.As(retryErr, &providerErr) && providerErr.RetryAfter > delay {
		delay = providerErr.RetryAfter
	}
	event := damodel.RetryEvent{Attempt: attempt + 1, Delay: delay, Retryable: true, Err: retryErr.Error(), Provider: client.provider, Model: client.model}
	var reporter damodel.RetryReporter
	if errors.As(retryErr, &reporter) {
		event = reporter.RetryEvent(attempt+1, delay)
	}
	damodel.ReportRetry(ctx, event)
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
		providerErr.Provider = client.provider
		providerErr.Model = client.model
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
		return nil, fmt.Errorf("%s: credentials: %w", client.provider, err)
	}
	if credentials.AccessToken == "" {
		return nil, fmt.Errorf("%s: credential source returned an empty token", client.provider)
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
		return nil, fmt.Errorf("%s: request: %w", client.provider, err)
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

func (client *Client) requestBody(request damodel.Request, stream bool) ([]byte, error) {
	payload, err := client.requestPayload(request, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

func (client *Client) requestPayload(request damodel.Request, stream bool) (responsesRequest, error) {
	input := make([]any, 0, len(request.Messages)*2)
	var instructions []string
	if request.SystemMessage != nil {
		if request.SystemMessage.Role != damessage.RoleSystem {
			return responsesRequest{}, fmt.Errorf("system message has role %q", request.SystemMessage.Role)
		}
		if text := strings.TrimSpace(request.SystemMessage.TextContent()); text != "" {
			instructions = append(instructions, text)
		}
	}
	for _, item := range request.Messages {
		if item.Role == damessage.RoleSystem {
			if text := strings.TrimSpace(item.TextContent()); text != "" {
				instructions = append(instructions, text)
			}
			continue
		}
		converted, err := inputItems(item, client.provider)
		if err != nil {
			return responsesRequest{}, err
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
			return responsesRequest{}, err
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
		Model: client.model, Instructions: strings.Join(instructions, "\n\n"), Input: input, Tools: tools, Stream: stream,
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
				return responsesRequest{}, fmt.Errorf("%s: metadata %q: %w", client.provider, key, err)
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
			return responsesRequest{}, fmt.Errorf("%s: unsupported tool choice %q", client.provider, request.ToolChoice.Mode)
		}
	}
	if request.ResponseFormat != nil {
		payload.Text = &responseText{Format: responseFormat{
			Type: "json_schema", Name: request.ResponseFormat.Name, Description: request.ResponseFormat.Description,
			Schema: request.ResponseFormat.Schema, Strict: request.ResponseFormat.Strict,
		}}
	}
	return payload, nil
}

const reasoningStateKey = "openai.responses.reasoning"

type reasoningState struct {
	ID               string            `json:"id"`
	Summary          []responseSummary `json:"summary"`
	EncryptedContent string            `json:"encrypted_content"`
}

func inputItems(value damessage.Message, providers ...string) ([]any, error) {
	provider := "openai"
	if len(providers) > 0 && providers[0] != "" {
		provider = providers[0]
	}
	if err := value.Validate(); err != nil {
		return nil, fmt.Errorf("%s: message: %w", provider, err)
	}
	if value.Role == damessage.RoleRemove {
		return nil, fmt.Errorf("%s: remove messages must be reduced before model invocation", provider)
	}
	items := make([]any, 0, 1+len(value.ToolCalls))
	if value.Role == damessage.RoleTool {
		var output any = value.TextContent()
		hasRichContent := false
		contents := make([]any, 0, len(value.Content))
		for _, block := range value.Content {
			switch block.Type {
			case damessage.BlockText:
				contents = append(contents, map[string]any{"type": "input_text", "text": block.Text})
			case damessage.BlockImage:
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
	if role == string(damessage.RoleHuman) {
		role = "user"
	}
	contents := make([]any, 0, len(value.Content))
	for _, block := range value.Content {
		switch block.Type {
		case damessage.BlockText:
			typeName := "input_text"
			if value.Role == damessage.RoleAssistant {
				typeName = "output_text"
			}
			contents = append(contents, map[string]any{"type": typeName, "text": block.Text})
		case damessage.BlockImage:
			if value.Role != damessage.RoleHuman {
				return nil, fmt.Errorf("%s: image content is only supported in human messages", provider)
			}
			imageURL := block.URL
			if imageURL == "" && len(block.Data) > 0 {
				imageURL = "data:" + block.MIMEType + ";base64," + encodeBase64(block.Data)
			}
			contents = append(contents, map[string]any{"type": "input_image", "image_url": imageURL})
		case damessage.BlockFile:
			if block.URL == "" {
				return nil, fmt.Errorf("%s: file block requires a URL", provider)
			}
			contents = append(contents, map[string]any{"type": "input_file", "file_url": block.URL})
		case damessage.BlockReasoning:
			if value.Role != damessage.RoleAssistant {
				return nil, fmt.Errorf("%s: reasoning content is only supported in assistant messages", provider)
			}
			raw := block.Extra[reasoningStateKey]
			if len(raw) == 0 {
				// Display-only reasoning cannot be replayed safely. The provider
				// requires its opaque encrypted state, not summary prose.
				continue
			}
			var state reasoningState
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, fmt.Errorf("%s: decode reasoning state: %w", provider, err)
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
		case damessage.BlockServerTool:
			if value.Role != damessage.RoleAssistant {
				return nil, fmt.Errorf("%s: server tool content is only supported in assistant messages", provider)
			}
			raw := block.Extra[responseOutputStateKey]
			if len(raw) == 0 {
				// Older checkpoints retained only the display projection of hosted
				// tool calls. It is not sufficient to reconstruct a provider input
				// item safely, so preserve the assistant text and omit this item.
				continue
			}
			var replay struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(raw, &replay); err != nil {
				return nil, fmt.Errorf("%s: decode server tool state: %w", provider, err)
			}
			if replay.Type != "web_search_call" {
				return nil, fmt.Errorf("%s: unsupported server tool state %q", provider, replay.Type)
			}
			if block.ID != "" && replay.ID != block.ID {
				return nil, fmt.Errorf("%s: server tool state id %q does not match block id %q", provider, replay.ID, block.ID)
			}
			items = append(items, append(json.RawMessage(nil), raw...))
		case damessage.BlockNonStandard:
			if value.Role != damessage.RoleAssistant {
				return nil, fmt.Errorf("openai: non-standard content is only supported in assistant messages")
			}
			var replay struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(block.NonStandard, &replay); err != nil {
				return nil, fmt.Errorf("openai: decode non-standard state: %w", err)
			}
			switch replay.Type {
			case "compaction", "compaction_summary", "context_compaction":
				items = append(items, append(json.RawMessage(nil), block.NonStandard...))
			default:
				return nil, fmt.Errorf("openai: unsupported non-standard state %q", replay.Type)
			}
		default:
			return nil, fmt.Errorf("%s: unsupported content block %q", provider, block.Type)
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
	Model             string                     `json:"model,omitempty"`
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
	Status           string            `json:"status,omitempty"`
	Role             string            `json:"role"`
	CallID           string            `json:"call_id"`
	Name             string            `json:"name"`
	Arguments        json.RawMessage   `json:"arguments"`
	Content          []responseContent `json:"content"`
	Summary          []responseSummary `json:"summary,omitempty"`
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	Action           *responseAction   `json:"action,omitempty"`
	raw              json.RawMessage
}

func (output *responseOutput) UnmarshalJSON(data []byte) error {
	type wire responseOutput
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*output = responseOutput(decoded)
	output.raw = append(json.RawMessage(nil), data...)
	return nil
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
	Cost                float64                     `json:"cost,omitempty"`
}

type responseInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responseOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func normalizeResponse(payload responsesResponse, format *damodel.ResponseFormat, providers ...string) (damodel.Response, error) {
	provider := "openai"
	if len(providers) > 0 && providers[0] != "" {
		provider = providers[0]
	}
	if payload.Error != nil {
		return damodel.Response{}, apiErrorValue(payload.Error, http.StatusOK, provider)
	}
	result := damessage.Message{ID: payload.ID, Role: damessage.RoleAssistant}
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
					block := damessage.ContentBlock{Type: damessage.BlockText, Text: content.Text}
					for _, annotation := range content.Annotations {
						if annotation.Type == "url_citation" {
							block.Citations = append(block.Citations, damessage.Citation{URL: annotation.URL, Title: annotation.Title, StartIndex: annotation.StartIndex, EndIndex: annotation.EndIndex, CitedText: annotation.CitedText})
						}
					}
					result.Content = append(result.Content, block)
				case "refusal":
					refusalText = content.Refusal
					result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockNonStandard, NonStandard: mustJSON(map[string]any{"type": "refusal", "refusal": content.Refusal})})
				}
			}
		case "function_call":
			arguments, err := normalizeToolArguments(output.Arguments)
			if err != nil {
				result.InvalidToolCalls = append(result.InvalidToolCalls, damessage.InvalidToolCall{ID: output.CallID, Name: output.Name, Arguments: arguments, Error: "invalid JSON arguments"})
				continue
			}
			result.ToolCalls = append(result.ToolCalls, damessage.ToolCall{ID: output.CallID, Name: output.Name, Arguments: arguments})
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
			extra := map[string]json.RawMessage{"arguments": mustJSON(arguments)}
			if len(output.raw) > 0 {
				extra[responseOutputStateKey] = append(json.RawMessage(nil), output.raw...)
			}
			result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockServerTool, ID: output.ID, Name: "web_search", Extra: extra})
		case "compaction", "compaction_summary", "context_compaction":
			if len(output.raw) > 0 {
				result.Content = append(result.Content, damessage.ContentBlock{
					Type: damessage.BlockNonStandard, ID: output.ID,
					NonStandard: append(json.RawMessage(nil), output.raw...),
				})
			}
		}
	}
	finishReason := damodel.FinishReasonStop
	switch {
	case refusalText != "":
		damodel.SetOutcome(&result, damodel.FinishReasonRefusal, &damodel.Refusal{Explanation: refusalText})
	case payload.Status == "incomplete" && payload.IncompleteDetails != nil && payload.IncompleteDetails.Reason == "max_output_tokens":
		damodel.SetOutcome(&result, damodel.FinishReasonMaxTokens, nil)
	case len(result.ToolCalls) > 0:
		damodel.SetOutcome(&result, damodel.FinishReasonToolCalls, nil)
	default:
		damodel.SetOutcome(&result, finishReason, nil)
	}
	if payload.Usage.TotalTokens != 0 || payload.Usage.InputTokens != 0 || payload.Usage.OutputTokens != 0 || payload.Usage.Cost != 0 {
		cached := 0
		if payload.Usage.InputTokensDetails != nil {
			cached = payload.Usage.InputTokensDetails.CachedTokens
		}
		reasoning := 0
		if payload.Usage.OutputTokensDetails != nil {
			reasoning = payload.Usage.OutputTokensDetails.ReasoningTokens
		}
		result.Usage = &damessage.Usage{
			InputTokens: max(0, payload.Usage.InputTokens-cached), OutputTokens: payload.Usage.OutputTokens,
			TotalTokens: payload.Usage.TotalTokens, CostUSD: payload.Usage.Cost,
			Provider: provider, Model: payload.Model,
		}
		if cached > 0 {
			result.Usage.InputDetails = map[string]int{"cache_read": cached}
		}
		if reasoning > 0 {
			result.Usage.OutputDetails = map[string]int{"reasoning": reasoning}
		}
	}
	response := damodel.Response{Message: result}
	if format != nil && len(result.ToolCalls) == 0 {
		text := strings.TrimSpace(result.TextContent())
		if !json.Valid([]byte(text)) {
			return damodel.Response{}, fmt.Errorf("%s: structured response is not valid JSON", provider)
		}
		response.Structured = json.RawMessage(text)
	}
	return response, nil
}

func reasoningContentBlock(output responseOutput) damessage.ContentBlock {
	return reasoningContentBlockWithText(output, true)
}

func reasoningContentBlockWithText(output responseOutput, includeText bool) damessage.ContentBlock {
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
	return damessage.ContentBlock{
		Type: damessage.BlockReasoning, ID: output.ID, Reasoning: strings.Join(parts, "\n"),
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
	retryable  bool
}

func (err *Error) Error() string {
	provider := err.Provider
	if provider == "" {
		provider = "openai"
	}
	if err.Code != "" {
		return fmt.Sprintf("%s: status %d (%s): %s", provider, err.Status, err.Code, err.Message)
	}
	return fmt.Sprintf("%s: status %d: %s", provider, err.Status, err.Message)
}

func (err *Error) RetryEvent(attempt int, delay time.Duration) damodel.RetryEvent {
	if err.RetryAfter > delay {
		delay = err.RetryAfter
	}
	return damodel.RetryEvent{
		Attempt: attempt, Delay: delay, Retryable: err.retryableError(),
		Err: err.Message, Status: err.Status,
		Provider: err.Provider, Model: err.Model,
	}
}

func (err *Error) retryableError() bool {
	return err.retryable || err.Status == http.StatusTooManyRequests || err.Status >= 500
}

func responseError(response *http.Response) error {
	return responseErrorForProvider(response, "openai")
}

func responseErrorForProvider(response *http.Response, provider string) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if err != nil {
		return fmt.Errorf("%s: read error response: %w", provider, err)
	}
	var envelope struct {
		Error     *apiError `json:"error"`
		ErrorType string    `json:"error_type"`
	}
	if json.Unmarshal(limited, &envelope) != nil || envelope.Error == nil {
		envelope.Error = &apiError{Message: strings.TrimSpace(string(limited))}
	}
	if envelope.Error.Type == "" {
		envelope.Error.Type = envelope.ErrorType
	}
	err = apiErrorValue(envelope.Error, response.StatusCode, provider)
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

func apiErrorValue(value *apiError, status int, providers ...string) error {
	provider := "openai"
	if len(providers) > 0 && providers[0] != "" {
		provider = providers[0]
	}
	err := &Error{
		Status: status, Code: value.Code, Type: value.Type, Message: value.Message, Provider: provider,
		retryable: value.Code == "previous_response_not_found" || value.Code == "websocket_connection_limit_reached",
	}
	if value.Code == "context_length_exceeded" || value.Code == "context_window_exceeded" ||
		value.Type == "context_length_exceeded" || value.Type == "context_window_exceeded" {
		return errors.Join(damodel.ErrContextOverflow, err)
	}
	return err
}

type responseStream struct {
	ctx               context.Context
	body              io.ReadCloser
	scanner           *bufio.Scanner
	provider          string
	queued            []damodel.Chunk
	calls             map[string]responseOutput
	done              bool
	data              []string
	emittedText       strings.Builder
	emittedCalls      map[string]struct{}
	emittedReasoning  map[string]string
	emittedServer     map[string]struct{}
	emittedCompaction bool
	completed         *responsesResponse
	format            *damodel.ResponseFormat
}

func newResponseStream(ctx context.Context, body io.ReadCloser, provider string, format *damodel.ResponseFormat) *responseStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	return &responseStream{
		ctx: ctx, body: body, scanner: scanner, provider: provider, calls: map[string]responseOutput{},
		emittedCalls: map[string]struct{}{}, emittedReasoning: map[string]string{}, emittedServer: map[string]struct{}{},
		format: format,
	}
}

func responseFormatFromPayload(payload responsesRequest) *damodel.ResponseFormat {
	if payload.Text == nil || payload.Text.Format.Type != "json_schema" {
		return nil
	}
	format := payload.Text.Format
	return &damodel.ResponseFormat{
		Name: format.Name, Description: format.Description, Schema: append(json.RawMessage(nil), format.Schema...), Strict: format.Strict,
	}
}

func (stream *responseStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *responseStream) Next(ctx context.Context) (damodel.Chunk, error) {
	if len(stream.queued) > 0 {
		result := stream.queued[0]
		stream.queued = stream.queued[1:]
		return result, nil
	}
	if stream.done {
		return damodel.Chunk{}, io.EOF
	}
	for stream.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			stream.Close()
			return damodel.Chunk{}, err
		}
		line := stream.scanner.Text()
		if line == "" {
			chunk, emit, err := stream.flushEvent()
			if err != nil {
				stream.Close()
				return damodel.Chunk{}, err
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
		return damodel.Chunk{}, fmt.Errorf("%s: read stream: %w", stream.provider, err)
	}
	chunk, emit, err := stream.flushEvent()
	if err != nil {
		stream.Close()
		return damodel.Chunk{}, err
	}
	if emit {
		return chunk, nil
	}
	stream.Close()
	if stream.provider == "openai" {
		return damodel.Chunk{}, ErrIncompleteStream
	}
	return damodel.Chunk{}, incompleteStreamError{provider: stream.provider}
}

type incompleteStreamError struct{ provider string }

func (err incompleteStreamError) Error() string {
	return err.provider + ": response stream ended before completion"
}

func (incompleteStreamError) Unwrap() error { return ErrIncompleteStream }

func (stream *responseStream) flushEvent() (damodel.Chunk, bool, error) {
	if len(stream.data) == 0 {
		return damodel.Chunk{}, false, nil
	}
	data := strings.Join(stream.data, "\n")
	stream.data = nil
	if strings.TrimSpace(data) == "[DONE]" {
		stream.done = true
		return damodel.Chunk{Done: true}, true, nil
	}
	return stream.event([]byte(data))
}

func (stream *responseStream) event(data []byte) (damodel.Chunk, bool, error) {
	var envelope struct {
		Type        string            `json:"type"`
		Delta       string            `json:"delta"`
		Item        responseOutput    `json:"item"`
		ItemID      string            `json:"item_id"`
		OutputIndex int               `json:"output_index"`
		Arguments   json.RawMessage   `json:"arguments"`
		Response    responsesResponse `json:"response"`
		Error       *apiError         `json:"error"`
		Status      int               `json:"status"`
		ErrorType   string            `json:"error_type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return damodel.Chunk{}, false, fmt.Errorf("%s: decode stream event: %w", stream.provider, err)
	}
	switch envelope.Type {
	case "response.output_text.delta":
		stream.emittedText.WriteString(envelope.Delta)
		return damodel.Chunk{MessageDelta: damessage.Assistant(envelope.Delta)}, true, nil
	case "response.reasoning_summary_text.delta":
		stream.emittedReasoning[envelope.ItemID] += envelope.Delta
		index := envelope.OutputIndex
		return damodel.Chunk{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{
			Type: damessage.BlockReasoning, ID: envelope.ItemID, Reasoning: envelope.Delta, Index: &index,
		}}}}, true, nil
	case "response.output_item.added":
		if envelope.Item.Type == "function_call" {
			stream.calls[envelope.Item.ID] = envelope.Item
		}
	case "response.output_item.done":
		if envelope.Item.Type == "reasoning" {
			stream.emittedReasoning[envelope.Item.ID] = reasoningText(envelope.Item.Summary)
			return damodel.Chunk{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{
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
			extra := map[string]json.RawMessage{"arguments": mustJSON(arguments)}
			if len(envelope.Item.raw) > 0 {
				extra[responseOutputStateKey] = append(json.RawMessage(nil), envelope.Item.raw...)
			}
			return damodel.Chunk{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{
				Type: damessage.BlockServerTool, ID: envelope.Item.ID, Name: "web_search",
				Extra: extra,
			}}}}, true, nil
		}
		if isCompactionType(envelope.Item.Type) && len(envelope.Item.raw) > 0 {
			stream.emittedCompaction = true
			return damodel.Chunk{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{
				Type: damessage.BlockNonStandard, ID: envelope.Item.ID,
				NonStandard: append(json.RawMessage(nil), envelope.Item.raw...),
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
			return damodel.Chunk{}, false, fmt.Errorf("%s: streamed tool arguments for %q are invalid JSON", stream.provider, call.CallID)
		}
		stream.emittedCalls[call.CallID] = struct{}{}
		return damodel.Chunk{MessageDelta: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: call.CallID, Name: call.Name, Arguments: arguments}}}}, true, nil
	case "response.completed":
		stream.done = true
		stream.completed = &envelope.Response
		return stream.completedChunk(envelope.Response)
	case "response.failed", "error":
		if envelope.Error == nil {
			envelope.Error = envelope.Response.Error
		}
		if envelope.Error == nil {
			return damodel.Chunk{}, false, fmt.Errorf("%s: response stream failed", stream.provider)
		}
		if envelope.Error.Type == "" {
			envelope.Error.Type = envelope.ErrorType
		}
		return damodel.Chunk{}, false, apiErrorValue(envelope.Error, envelope.Status, stream.provider)
	}
	return damodel.Chunk{}, false, nil
}

func (stream *responseStream) completedChunk(payload responsesResponse) (damodel.Chunk, bool, error) {
	response, err := normalizeResponse(payload, stream.format, stream.provider)
	if err != nil {
		return damodel.Chunk{}, false, err
	}
	delta := response.Message
	delta.Content = nil
	for _, block := range response.Message.Content {
		switch block.Type {
		case damessage.BlockText:
			prefix := stream.emittedText.String()
			if strings.HasPrefix(block.Text, prefix) {
				block.Text = strings.TrimPrefix(block.Text, prefix)
			}
			if block.Text != "" || len(block.Citations) > 0 || len(block.Extra) > 0 {
				delta.Content = append(delta.Content, block)
			}
		case damessage.BlockReasoning:
			if prior := stream.emittedReasoning[block.ID]; prior != "" && strings.HasPrefix(block.Reasoning, prior) {
				block.Reasoning = strings.TrimPrefix(block.Reasoning, prior)
			}
			delta.Content = append(delta.Content, block)
		case damessage.BlockServerTool:
			if _, emitted := stream.emittedServer[block.ID]; !emitted {
				delta.Content = append(delta.Content, block)
			}
		case damessage.BlockNonStandard:
			var item struct {
				Type string `json:"type"`
			}
			if stream.emittedCompaction && json.Unmarshal(block.NonStandard, &item) == nil && isCompactionType(item.Type) {
				continue
			}
			delta.Content = append(delta.Content, block)
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
	return damodel.Chunk{MessageDelta: delta, Structured: append(json.RawMessage(nil), response.Structured...), Done: true}, true, nil
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

func cloneDefinitions(values []datool.Definition) []datool.Definition {
	result := make([]datool.Definition, len(values))
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
