// Package anthropic adapts Anthropic's Messages API to damodel.Chat.
package anthropic

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
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/internal/optionvalue"
)

const (
	defaultEndpoint      = "https://api.anthropic.com/v1/messages"
	defaultMaxOutput     = 32_000
	rawBlockMetadataKey  = "anthropic.content_block.v1"
	maximumResponseBytes = 64 << 20
)

var hostedToolBetas = map[string]string{
	"web_fetch_20250910":              "web-fetch-2025-09-10",
	"code_execution_20250522":         "code-execution-2025-05-22",
	"code_execution_20250825":         "code-execution-2025-08-25",
	"mcp_toolset":                     "mcp-client-2025-11-20",
	"memory_20250818":                 "context-management-2025-06-27",
	"computer_20250124":               "computer-use-2025-01-24",
	"computer_20251124":               "computer-use-2025-11-24",
	"tool_search_tool_regex_20251119": "advanced-tool-use-2025-11-20",
	"tool_search_tool_bm25_20251119":  "advanced-tool-use-2025-11-20",
}

// Options configures a direct Anthropic Messages API model. HostedTools and
// Parameters are forward-compatible raw JSON boundaries for Anthropic features
// such as web fetch, code execution, computer use, memory, tool search,
// context management, task budgets, containers, and user profiles.
type Options struct {
	BaseURL         string
	HTTPClient      *http.Client
	Headers         http.Header
	MaxOutputTokens int
	ContextWindow   int
	WebSearch       bool
	HostedTools     []json.RawMessage
	MCPServers      []json.RawMessage
	Betas           []string
	Parameters      map[string]json.RawMessage
	// RetryBackoff controls retries for transport failures, rate limits, and
	// server errors. Nil selects conservative defaults; empty disables retries.
	RetryBackoff []time.Duration
}

// Client is a direct Anthropic Messages API client.
type Client struct {
	apiKey  string
	model   string
	options Options
}

// New constructs a direct Anthropic client. Construction performs no I/O.
func New(apiKey, model string, optionValues ...Options) *Client {
	options := optionvalue.Resolve("Anthropic client", optionValues)
	apiKey = strings.TrimSpace(apiKey)
	model = strings.TrimSpace(model)
	if apiKey == "" || model == "" {
		panic("anthropic: API key and model are required")
	}
	if options.MaxOutputTokens < 0 || options.ContextWindow < 0 {
		panic("anthropic: token limits cannot be negative")
	}
	if options.BaseURL == "" {
		options.BaseURL = defaultEndpoint
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.MaxOutputTokens == 0 {
		options.MaxOutputTokens = defaultMaxOutput
	}
	options.Headers = options.Headers.Clone()
	options.HostedTools = cloneRawSlice(options.HostedTools)
	options.MCPServers = cloneRawSlice(options.MCPServers)
	options.Betas = append([]string(nil), options.Betas...)
	options.Parameters = cloneRawMap(options.Parameters)
	if options.RetryBackoff == nil {
		options.RetryBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	} else {
		options.RetryBackoff = append([]time.Duration(nil), options.RetryBackoff...)
	}
	for _, delay := range options.RetryBackoff {
		if delay < 0 {
			panic("anthropic: retry delays cannot be negative")
		}
	}
	for name, value := range options.Parameters {
		if !json.Valid(value) {
			panic(fmt.Sprintf("anthropic: parameter %q is not valid JSON", name))
		}
		switch name {
		case "model", "max_tokens", "messages", "system", "tools", "tool_choice", "stream":
			panic(fmt.Sprintf("anthropic: parameter %q shadows a request field", name))
		}
	}
	for index, tool := range options.HostedTools {
		if !json.Valid(tool) {
			panic(fmt.Sprintf("anthropic: hosted tool %d is not valid JSON", index))
		}
	}
	for index, server := range options.MCPServers {
		if !json.Valid(server) {
			panic(fmt.Sprintf("anthropic: MCP server %d is not valid JSON", index))
		}
	}
	return &Client{apiKey: apiKey, model: model, options: options}
}

func (client *Client) Profile() damodel.Profile {
	return damodel.Profile{
		Provider: "anthropic", Model: client.model, ContextWindow: client.options.ContextWindow,
		MaxOutputTokens: client.options.MaxOutputTokens, ToolCalling: true, ParallelToolCalls: true,
		StructuredOutput: true, NativeStreaming: true, SupportsPromptCaching: true, SupportsSeparateSystemMessage: true,
		SupportsReasoning: true, ReasoningLevels: []string{"low", "medium", "high", "xhigh", "max"},
		SupportsImages: true, SupportsPDF: true, SupportsFiles: true, SupportsWebSearch: client.hasWebSearch(),
	}
}

func (client *Client) hasWebSearch() bool {
	if client.options.WebSearch {
		return true
	}
	for _, raw := range client.options.HostedTools {
		var tool struct{ Name, Type string }
		_ = json.Unmarshal(raw, &tool)
		if tool.Name == "web_search" || strings.HasPrefix(tool.Type, "web_search_") {
			return true
		}
	}
	return false
}

func (client *Client) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	payload, betas, err := client.payload(request)
	if err != nil {
		return damodel.Response{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return damodel.Response{}, fmt.Errorf("anthropic: encode request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		response, err := client.invokeOnce(ctx, body, betas, request.ResponseFormat)
		if err == nil || attempt >= len(client.options.RetryBackoff) || !retryable(err) {
			return response, err
		}
		timer := time.NewTimer(client.options.RetryBackoff[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return damodel.Response{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (client *Client) invokeOnce(ctx context.Context, body []byte, betas []string, format *damodel.ResponseFormat) (damodel.Response, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.options.BaseURL, bytes.NewReader(body))
	if err != nil {
		return damodel.Response{}, fmt.Errorf("anthropic: create request: %w", err)
	}
	client.setHeaders(httpRequest, betas)
	httpResponse, err := client.options.HTTPClient.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return damodel.Response{}, ctx.Err()
		}
		return damodel.Response{}, &transientError{fmt.Errorf("anthropic: transport: %w", err)}
	}
	defer httpResponse.Body.Close()
	limited := io.LimitReader(httpResponse.Body, maximumResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return damodel.Response{}, &transientError{fmt.Errorf("anthropic: read response: %w", err)}
	}
	if len(responseBody) > maximumResponseBytes {
		return damodel.Response{}, errors.New("anthropic: response exceeds 64 MiB")
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return damodel.Response{}, responseError(httpResponse.StatusCode, responseBody)
	}
	return normalizeResponse(responseBody, format)
}

func (client *Client) setHeaders(request *http.Request, betas []string) {
	for name, values := range client.options.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", client.apiKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	if len(betas) > 0 {
		request.Header.Set("anthropic-beta", strings.Join(betas, ","))
	}
}

func (client *Client) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	payload, betas, err := client.payload(request)
	if err != nil {
		return nil, err
	}
	payload["stream"] = true
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode streaming request: %w", err)
	}
	httpResponse, err := client.openStream(ctx, body, betas)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(httpResponse.Body)
	scanner.Buffer(make([]byte, 64<<10), maximumResponseBytes)
	return &messageStream{
		ctx: ctx, body: httpResponse.Body, scanner: scanner, blocks: map[int]*streamingBlock{},
		format: request.ResponseFormat,
	}, nil
}

func (client *Client) openStream(ctx context.Context, body []byte, betas []string) (*http.Response, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, client.options.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create streaming request: %w", err)
	}
	client.setHeaders(httpRequest, betas)
	httpResponse, err := client.options.HTTPClient.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("anthropic: streaming transport: %w", err)
	}
	if httpResponse.StatusCode >= 200 && httpResponse.StatusCode < 300 {
		return httpResponse, nil
	}
	defer httpResponse.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, maximumResponseBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("anthropic: read streaming error: %w", readErr)
	}
	return nil, responseError(httpResponse.StatusCode, responseBody)
}

type streamingBlock struct {
	value       map[string]any
	partialJSON strings.Builder
}

type messageStream struct {
	ctx       context.Context
	body      io.ReadCloser
	scanner   *bufio.Scanner
	blocks    map[int]*streamingBlock
	format    *damodel.ResponseFormat
	id        string
	model     string
	usage     damessage.Usage
	text      strings.Builder
	bytesRead int
	closed    bool
}

func (stream *messageStream) Next(ctx context.Context) (damodel.Chunk, error) {
	if stream.closed {
		return damodel.Chunk{}, io.EOF
	}
	for stream.scanner.Scan() {
		line := stream.scanner.Bytes()
		stream.bytesRead += len(line)
		if stream.bytesRead > maximumResponseBytes {
			_ = stream.Close()
			return damodel.Chunk{}, errors.New("anthropic: stream exceeds 64 MiB")
		}
		if err := ctx.Err(); err != nil {
			_ = stream.Close()
			return damodel.Chunk{}, err
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 {
			continue
		}
		chunk, emit, err := stream.consumeEvent(data)
		if err != nil {
			_ = stream.Close()
			return damodel.Chunk{}, err
		}
		if emit {
			return chunk, nil
		}
	}
	if err := stream.scanner.Err(); err != nil {
		_ = stream.Close()
		return damodel.Chunk{}, fmt.Errorf("anthropic: read stream: %w", err)
	}
	_ = stream.Close()
	return damodel.Chunk{}, io.EOF
}

func (stream *messageStream) consumeEvent(data []byte) (damodel.Chunk, bool, error) {
	var event struct {
		Type    string          `json:"type"`
		Index   int             `json:"index"`
		Message json.RawMessage `json:"message"`
		Block   json.RawMessage `json:"content_block"`
		Delta   struct {
			Type        string          `json:"type"`
			Text        string          `json:"text"`
			Thinking    string          `json:"thinking"`
			Signature   string          `json:"signature"`
			PartialJSON string          `json:"partial_json"`
			StopReason  string          `json:"stop_reason"`
			Citation    json.RawMessage `json:"citation"`
		} `json:"delta"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Error struct{ Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return damodel.Chunk{}, false, fmt.Errorf("anthropic: decode stream event: %w", err)
	}
	switch event.Type {
	case "message_start":
		var message struct {
			ID, Model string
			Usage     struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(event.Message, &message); err != nil {
			return damodel.Chunk{}, false, fmt.Errorf("anthropic: decode message start: %w", err)
		}
		stream.id, stream.model = message.ID, message.Model
		stream.setUsage(message.Usage.InputTokens, message.Usage.OutputTokens, message.Usage.CacheCreationInputTokens, message.Usage.CacheReadInputTokens)
	case "content_block_start":
		var value map[string]any
		if err := json.Unmarshal(event.Block, &value); err != nil {
			return damodel.Chunk{}, false, fmt.Errorf("anthropic: decode block start: %w", err)
		}
		stream.blocks[event.Index] = &streamingBlock{value: value}
	case "content_block_delta":
		return stream.consumeDelta(event.Index, event.Delta.Type, event.Delta.Text, event.Delta.Thinking, event.Delta.Signature, event.Delta.PartialJSON, event.Delta.Citation)
	case "content_block_stop":
		return stream.finishBlock(event.Index)
	case "message_delta":
		stream.setUsage(event.Usage.InputTokens, event.Usage.OutputTokens, event.Usage.CacheCreationInputTokens, event.Usage.CacheReadInputTokens)
		message := damessage.Message{ID: stream.id, Role: damessage.RoleAssistant, Usage: cloneUsage(&stream.usage)}
		reason := finishReason(event.Delta.StopReason)
		damodel.SetOutcome(&message, reason, nil)
		return damodel.Chunk{MessageDelta: message}, true, nil
	case "message_stop":
		chunk := damodel.Chunk{MessageDelta: damessage.Message{ID: stream.id, Role: damessage.RoleAssistant}, Done: true}
		if stream.format != nil {
			structured := strings.TrimSpace(stream.text.String())
			if !json.Valid([]byte(structured)) {
				return damodel.Chunk{}, false, errors.New("anthropic: structured response is not valid JSON")
			}
			chunk.Structured = json.RawMessage(structured)
		}
		_ = stream.Close()
		return chunk, true, nil
	case "error":
		return damodel.Chunk{}, false, fmt.Errorf("anthropic: stream %s: %s", event.Error.Type, event.Error.Message)
	}
	return damodel.Chunk{}, false, nil
}

func (stream *messageStream) consumeDelta(index int, deltaType, text, thinking, signature, partialJSON string, citationRaw json.RawMessage) (damodel.Chunk, bool, error) {
	block := stream.blocks[index]
	if block == nil {
		return damodel.Chunk{}, false, fmt.Errorf("anthropic: stream delta for unknown block %d", index)
	}
	blockType, _ := block.value["type"].(string)
	blockIndex := index
	switch deltaType {
	case "text_delta":
		block.value["text"] = stringValue(block.value["text"]) + text
		stream.text.WriteString(text)
		return damodel.Chunk{MessageDelta: damessage.Message{ID: stream.id, Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: text, Index: &blockIndex}}}}, text != "", nil
	case "citations_delta":
		citation, err := normalizeCitation(citationRaw)
		if err != nil {
			return damodel.Chunk{}, false, err
		}
		citations, _ := block.value["citations"].([]any)
		var decoded any
		_ = json.Unmarshal(citationRaw, &decoded)
		block.value["citations"] = append(citations, decoded)
		return damodel.Chunk{MessageDelta: damessage.Message{ID: stream.id, Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Index: &blockIndex, Citations: []damessage.Citation{citation}}}}}, true, nil
	case "thinking_delta":
		block.value["thinking"] = stringValue(block.value["thinking"]) + thinking
		return damodel.Chunk{MessageDelta: damessage.Message{ID: stream.id, Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{Type: damessage.BlockReasoning, Reasoning: thinking, Index: &blockIndex}}}}, thinking != "", nil
	case "signature_delta":
		block.value["signature"] = stringValue(block.value["signature"]) + signature
	case "input_json_delta":
		block.partialJSON.WriteString(partialJSON)
	case "compaction_delta":
		block.value["delta"] = map[string]any{"type": deltaType, "text": text}
	default:
		if blockType == "tool_use" && partialJSON != "" {
			block.partialJSON.WriteString(partialJSON)
		}
	}
	return damodel.Chunk{}, false, nil
}

func (stream *messageStream) finishBlock(index int) (damodel.Chunk, bool, error) {
	block := stream.blocks[index]
	if block == nil {
		return damodel.Chunk{}, false, fmt.Errorf("anthropic: stop for unknown block %d", index)
	}
	delete(stream.blocks, index)
	if block.partialJSON.Len() > 0 {
		var input any
		if err := json.Unmarshal([]byte(block.partialJSON.String()), &input); err != nil {
			return damodel.Chunk{}, false, fmt.Errorf("anthropic: streamed tool input: %w", err)
		}
		block.value["input"] = input
	}
	raw, err := json.Marshal(block.value)
	if err != nil {
		return damodel.Chunk{}, false, fmt.Errorf("anthropic: encode streamed block: %w", err)
	}
	content, toolCalls, err := normalizeContentBlock(raw)
	if err != nil {
		return damodel.Chunk{}, false, err
	}
	blockType, _ := block.value["type"].(string)
	if blockType == "text" || blockType == "thinking" {
		for itemIndex := range content {
			content[itemIndex].Text = ""
			content[itemIndex].Reasoning = ""
			content[itemIndex].Citations = nil
		}
	}
	return damodel.Chunk{MessageDelta: damessage.Message{ID: stream.id, Role: damessage.RoleAssistant, Content: content, ToolCalls: toolCalls}}, len(content) > 0 || len(toolCalls) > 0, nil
}

func (stream *messageStream) setUsage(input, output, cacheCreation, cacheRead int) {
	if input != 0 || cacheCreation != 0 || cacheRead != 0 {
		stream.usage.InputTokens = input + cacheCreation + cacheRead
		stream.usage.InputDetails = map[string]int{"cache_creation": cacheCreation, "cache_read": cacheRead}
	}
	if output != 0 {
		stream.usage.OutputTokens = output
	}
	stream.usage.TotalTokens = stream.usage.InputTokens + stream.usage.OutputTokens
	stream.usage.Provider, stream.usage.Model = "anthropic", stream.model
}

func (stream *messageStream) Close() error {
	if stream.closed {
		return nil
	}
	stream.closed = true
	return stream.body.Close()
}
func (stream *messageStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (client *Client) payload(request damodel.Request) (map[string]any, []string, error) {
	result := make(map[string]any, len(client.options.Parameters)+8)
	for name, value := range client.options.Parameters {
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, nil, fmt.Errorf("anthropic: decode parameter %q: %w", name, err)
		}
		result[name] = decoded
	}
	result["model"] = client.model
	result["max_tokens"] = client.options.MaxOutputTokens
	system, messages, err := formatMessages(request)
	if err != nil {
		return nil, nil, err
	}
	if len(system) > 0 {
		result["system"] = system
	}
	result["messages"] = messages
	tools := make([]any, 0, len(request.Tools)+len(client.options.HostedTools)+1)
	for _, definition := range request.Tools {
		if err := definition.Validate(); err != nil {
			return nil, nil, fmt.Errorf("anthropic: %w", err)
		}
		tool := map[string]any{"name": definition.Name, "description": definition.Description, "input_schema": json.RawMessage(definition.InputSchema)}
		if definition.Strict {
			tool["strict"] = true
		}
		for name, raw := range definition.Extra {
			if _, exists := tool[name]; exists {
				continue
			}
			var value any
			if json.Unmarshal(raw, &value) == nil {
				tool[name] = value
			}
		}
		tools = append(tools, tool)
	}
	if client.options.WebSearch {
		tools = append(tools, map[string]any{"type": "web_search_20250305", "name": "web_search"})
	}
	betas := append([]string(nil), client.options.Betas...)
	for _, raw := range client.options.HostedTools {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, nil, fmt.Errorf("anthropic: decode hosted tool: %w", err)
		}
		tools = append(tools, value)
		if toolType, _ := value["type"].(string); hostedToolBetas[toolType] != "" {
			betas = appendUnique(betas, hostedToolBetas[toolType])
		}
		if examples, exists := value["input_examples"]; exists && examples != nil {
			betas = appendUnique(betas, "advanced-tool-use-2025-11-20")
		}
	}
	if len(tools) > 0 {
		result["tools"] = tools
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case "auto", "":
			result["tool_choice"] = map[string]any{"type": "auto"}
		case "none":
			delete(result, "tools")
		case "required", "any":
			result["tool_choice"] = map[string]any{"type": "any"}
		case "tool":
			result["tool_choice"] = map[string]any{"type": "tool", "name": request.ToolChoice.Name}
		default:
			return nil, nil, fmt.Errorf("anthropic: unsupported tool choice %q", request.ToolChoice.Mode)
		}
	}
	if request.ResponseFormat != nil {
		if !json.Valid(request.ResponseFormat.Schema) {
			return nil, nil, errors.New("anthropic: response schema is not valid JSON")
		}
		output, _ := result["output_config"].(map[string]any)
		if output == nil {
			output = map[string]any{}
		}
		output["format"] = map[string]any{"type": "json_schema", "schema": json.RawMessage(request.ResponseFormat.Schema)}
		result["output_config"] = output
	}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		output, _ := result["output_config"].(map[string]any)
		if output == nil {
			output = map[string]any{}
		}
		output["effort"] = request.Reasoning.Effort
		result["output_config"] = output
		if _, exists := result["thinking"]; !exists && supportsAdaptiveThinking(client.model) {
			result["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		}
	}
	if len(client.options.MCPServers) > 0 {
		servers := make([]any, 0, len(client.options.MCPServers))
		for _, raw := range client.options.MCPServers {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, nil, fmt.Errorf("anthropic: decode MCP server: %w", err)
			}
			servers = append(servers, value)
		}
		result["mcp_servers"] = servers
		betas = appendUnique(betas, "mcp-client-2025-11-20")
	}
	if output, ok := result["output_config"].(map[string]any); ok && output["task_budget"] != nil {
		betas = appendUnique(betas, "task-budgets-2026-03-13")
	}
	if result["user_profile_id"] != nil {
		betas = appendUnique(betas, "user-profiles-2026-03-24")
	}
	return result, betas, nil
}

func formatMessages(request damodel.Request) ([]any, []map[string]any, error) {
	var system []any
	if request.SystemMessage != nil {
		system = append(system, contentToWire(request.SystemMessage.Content, false)...)
	}
	var messages []map[string]any
	for _, message := range request.Messages {
		if message.Role == damessage.RoleRemove {
			continue
		}
		if message.Role == damessage.RoleSystem {
			system = append(system, contentToWire(message.Content, false)...)
			continue
		}
		role := "user"
		var content []any
		switch message.Role {
		case damessage.RoleHuman:
			content = contentToWire(message.Content, false)
		case damessage.RoleAssistant:
			role = "assistant"
			content = contentToWire(message.Content, true)
			for _, call := range message.ToolCalls {
				var input any
				if err := json.Unmarshal(call.Arguments, &input); err != nil {
					return nil, nil, fmt.Errorf("anthropic: tool call %q arguments: %w", call.ID, err)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": input})
			}
		case damessage.RoleTool:
			block := map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": contentToWire(message.Content, false)}
			if message.ToolStatus == damessage.ToolStatusError {
				block["is_error"] = true
			}
			content = []any{block}
		default:
			return nil, nil, fmt.Errorf("anthropic: unsupported message role %q", message.Role)
		}
		if len(content) == 0 {
			content = []any{map[string]any{"type": "text", "text": " "}}
		}
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			messages[len(messages)-1]["content"] = append(messages[len(messages)-1]["content"].([]any), content...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	return system, messages, nil
}

func contentToWire(blocks []damessage.ContentBlock, replay bool) []any {
	result := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if raw := block.Extra[rawBlockMetadataKey]; replay && len(raw) > 0 && json.Valid(raw) {
			result = append(result, json.RawMessage(raw))
			continue
		}
		switch block.Type {
		case damessage.BlockText:
			if block.Text != "" {
				result = append(result, applyBlockExtras(map[string]any{"type": "text", "text": block.Text}, block, "cache_control"))
			}
		case damessage.BlockReasoning:
			value := map[string]any{"type": "thinking", "thinking": block.Reasoning}
			if signature := block.Extra["signature"]; len(signature) > 0 {
				value["signature"] = json.RawMessage(signature)
			}
			result = append(result, applyBlockExtras(value, block, "cache_control"))
		case damessage.BlockImage:
			result = append(result, applyBlockExtras(map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": block.MIMEType, "data": block.Data}}, block, "cache_control"))
		case damessage.BlockFile:
			result = append(result, applyBlockExtras(map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": block.MIMEType, "data": block.Data}}, block, "cache_control"))
		case damessage.BlockNonStandard:
			if json.Valid(block.NonStandard) {
				result = append(result, json.RawMessage(block.NonStandard))
			}
		}
	}
	return result
}

func applyBlockExtras(value map[string]any, block damessage.ContentBlock, names ...string) map[string]any {
	for _, name := range names {
		raw := block.Extra[name]
		if len(raw) == 0 {
			continue
		}
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			value[name] = decoded
		}
	}
	return value
}

func normalizeContentBlock(raw json.RawMessage) ([]damessage.ContentBlock, []damessage.ToolCall, error) {
	var header struct {
		Type, ID, Name, Text, Thinking string
		Input                          json.RawMessage
		Signature                      json.RawMessage
		Citations                      []json.RawMessage `json:"citations"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, nil, fmt.Errorf("anthropic: decode content block: %w", err)
	}
	extra := map[string]json.RawMessage{rawBlockMetadataKey: cloneRaw(raw)}
	switch header.Type {
	case "text":
		citations := make([]damessage.Citation, 0, len(header.Citations))
		for _, citationRaw := range header.Citations {
			citation, err := normalizeCitation(citationRaw)
			if err != nil {
				return nil, nil, err
			}
			citations = append(citations, citation)
		}
		return []damessage.ContentBlock{{Type: damessage.BlockText, Text: header.Text, Citations: citations, Extra: extra}}, nil, nil
	case "thinking":
		if len(header.Signature) > 0 {
			extra["signature"] = cloneRaw(header.Signature)
		}
		return []damessage.ContentBlock{{Type: damessage.BlockReasoning, Reasoning: header.Thinking, Extra: extra}}, nil, nil
	case "tool_use":
		input := header.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		return nil, []damessage.ToolCall{{ID: header.ID, Name: header.Name, Arguments: cloneRaw(input)}}, nil
	case "server_tool_use", "mcp_tool_use":
		return []damessage.ContentBlock{{Type: damessage.BlockServerTool, ID: header.ID, Name: header.Name, Extra: extra}}, nil, nil
	case "web_search_tool_result", "web_fetch_tool_result":
		return []damessage.ContentBlock{{Type: damessage.BlockSearchResult, ID: header.ID, Name: header.Type, Extra: extra}}, nil, nil
	default:
		return []damessage.ContentBlock{{Type: damessage.BlockNonStandard, NonStandard: cloneRaw(raw), Extra: extra}}, nil, nil
	}
}

func normalizeCitation(raw json.RawMessage) (damessage.Citation, error) {
	var value struct {
		URL            string `json:"url"`
		Title          string `json:"title"`
		CitedText      string `json:"cited_text"`
		StartCharIndex *int   `json:"start_char_index"`
		EndCharIndex   *int   `json:"end_char_index"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return damessage.Citation{}, fmt.Errorf("anthropic: decode citation: %w", err)
	}
	return damessage.Citation{URL: value.URL, Title: value.Title, CitedText: value.CitedText, StartIndex: value.StartCharIndex, EndIndex: value.EndCharIndex}, nil
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func cloneUsage(value *damessage.Usage) *damessage.Usage {
	if value == nil {
		return nil
	}
	result := *value
	if value.InputDetails != nil {
		result.InputDetails = make(map[string]int, len(value.InputDetails))
		for name, count := range value.InputDetails {
			result.InputDetails[name] = count
		}
	}
	return &result
}

func finishReason(stopReason string) damodel.FinishReason {
	switch stopReason {
	case "tool_use", "pause_turn":
		return damodel.FinishReasonToolCalls
	case "max_tokens", "model_context_window_exceeded":
		return damodel.FinishReasonMaxTokens
	case "refusal":
		return damodel.FinishReasonRefusal
	default:
		return damodel.FinishReasonStop
	}
}

func normalizeResponse(body []byte, format *damodel.ResponseFormat) (damodel.Response, error) {
	var payload struct {
		ID         string            `json:"id"`
		Model      string            `json:"model"`
		StopReason string            `json:"stop_reason"`
		Content    []json.RawMessage `json:"content"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return damodel.Response{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	message := damessage.Message{ID: payload.ID, Role: damessage.RoleAssistant}
	for _, raw := range payload.Content {
		var header struct {
			Type, ID, Name, Text, Thinking string
			Input                          json.RawMessage
			Signature                      json.RawMessage
			Citations                      []struct {
				URL, Title, CitedText string
				StartCharIndex        *int `json:"start_char_index"`
				EndCharIndex          *int `json:"end_char_index"`
			} `json:"citations"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return damodel.Response{}, fmt.Errorf("anthropic: decode content block: %w", err)
		}
		extra := map[string]json.RawMessage{rawBlockMetadataKey: cloneRaw(raw)}
		switch header.Type {
		case "text":
			citations := make([]damessage.Citation, len(header.Citations))
			for index, citation := range header.Citations {
				citations[index] = damessage.Citation{URL: citation.URL, Title: citation.Title, CitedText: citation.CitedText, StartIndex: citation.StartCharIndex, EndIndex: citation.EndCharIndex}
			}
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: header.Text, Citations: citations, Extra: extra})
		case "thinking":
			if len(header.Signature) > 0 {
				extra["signature"] = cloneRaw(header.Signature)
			}
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockReasoning, Reasoning: header.Thinking, Extra: extra})
		case "tool_use":
			input := header.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			message.ToolCalls = append(message.ToolCalls, damessage.ToolCall{ID: header.ID, Name: header.Name, Arguments: cloneRaw(input)})
		case "server_tool_use", "mcp_tool_use":
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockServerTool, ID: header.ID, Name: header.Name, Extra: extra})
		case "web_search_tool_result", "web_fetch_tool_result":
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockSearchResult, ID: header.ID, Name: header.Type, Extra: extra})
		default:
			message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockNonStandard, NonStandard: cloneRaw(raw), Extra: extra})
		}
	}
	input := payload.Usage.InputTokens + payload.Usage.CacheCreationInputTokens + payload.Usage.CacheReadInputTokens
	message.Usage = &damessage.Usage{
		InputTokens: input, OutputTokens: payload.Usage.OutputTokens, TotalTokens: input + payload.Usage.OutputTokens,
		Provider: "anthropic", Model: payload.Model,
		InputDetails: map[string]int{"cache_creation": payload.Usage.CacheCreationInputTokens, "cache_read": payload.Usage.CacheReadInputTokens},
	}
	reason := finishReason(payload.StopReason)
	damodel.SetOutcome(&message, reason, nil)
	response := damodel.Response{Message: message}
	if format != nil {
		text := strings.TrimSpace(message.TextContent())
		if !json.Valid([]byte(text)) {
			return damodel.Response{}, errors.New("anthropic: structured response is not valid JSON")
		}
		response.Structured = json.RawMessage(text)
	}
	return response, nil
}

type apiError struct {
	status   int
	typeName string
	message  string
}

type transientError struct{ error }

func (err *apiError) Error() string {
	return fmt.Sprintf("anthropic: HTTP %d %s: %s", err.status, err.typeName, err.message)
}

func responseError(status int, body []byte) error {
	var payload struct {
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(body, &payload)
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = http.StatusText(status)
	}
	err := &apiError{status: status, typeName: payload.Error.Type, message: message}
	if status == http.StatusBadRequest && (strings.Contains(strings.ToLower(message), "context") && strings.Contains(strings.ToLower(message), "token")) {
		return errors.Join(damodel.ErrContextOverflow, err)
	}
	return err
}

func retryable(err error) bool {
	var api *apiError
	if errors.As(err, &api) {
		return api.status == http.StatusRequestTimeout || api.status == http.StatusConflict || api.status == http.StatusTooManyRequests || api.status >= 500
	}
	var transient *transientError
	return errors.As(err, &transient)
}

func supportsAdaptiveThinking(model string) bool {
	return strings.HasPrefix(model, "claude-opus-5") || strings.HasPrefix(model, "claude-sonnet-5") || strings.Contains(model, "opus-4-7") || strings.Contains(model, "opus-4-8")
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }
func cloneRawSlice(values []json.RawMessage) []json.RawMessage {
	result := make([]json.RawMessage, len(values))
	for index := range values {
		result[index] = cloneRaw(values[index])
	}
	return result
}
func cloneRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		result[name] = cloneRaw(value)
	}
	return result
}

var _ damodel.Chat = (*Client)(nil)
