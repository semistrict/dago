// Package gemini adapts the Gemini generateContent API to Dago's model contract.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

type Options struct {
	Model             string
	BaseURL           string
	HTTPClient        *http.Client
	Headers           http.Header
	ContextWindow     int
	MaxOutputTokens   int
	SupportsImages    bool
	SupportsReasoning bool
	DefaultReasoning  *model.Reasoning
}

type Client struct {
	apiKey     string
	options    Options
	boundTools []tool.Definition
}

func New(apiKey string, options Options) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("gemini: API key is required")
	}
	if strings.TrimSpace(options.Model) == "" {
		return nil, fmt.Errorf("gemini: model is required")
	}
	if options.BaseURL == "" {
		options.BaseURL = defaultBaseURL
	}
	options.BaseURL = strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.ContextWindow == 0 {
		options.ContextWindow = 1_000_000
	}
	if options.DefaultReasoning != nil {
		reasoning := *options.DefaultReasoning
		options.DefaultReasoning = &reasoning
	}
	return &Client{apiKey: apiKey, options: options}, nil
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
		Provider: "gemini", Model: client.options.Model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true,
		NativeStreaming: false, SupportsReasoning: client.options.SupportsReasoning,
	}
}

func (client *Client) CountTokens(_ context.Context, messages []message.Message) (int, error) {
	return message.ApproximateTokens(messages), nil
}

func (client *Client) Invoke(ctx context.Context, request model.Request) (model.Response, error) {
	body, err := client.requestBody(request)
	if err != nil {
		return model.Response{}, err
	}
	endpoint := client.options.BaseURL + "/models/" + url.PathEscape(client.options.Model) + ":generateContent?key=" + url.QueryEscape(client.apiKey)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return model.Response{}, fmt.Errorf("gemini: create request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	for key, values := range client.options.Headers {
		for _, value := range values {
			httpRequest.Header.Add(key, value)
		}
	}
	httpResponse, err := client.options.HTTPClient.Do(httpRequest)
	if err != nil {
		return model.Response{}, fmt.Errorf("gemini: request: %w", err)
	}
	defer httpResponse.Body.Close()
	data, err := io.ReadAll(io.LimitReader(httpResponse.Body, 8<<20))
	if err != nil {
		return model.Response{}, fmt.Errorf("gemini: read response: %w", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return model.Response{}, geminiError(httpResponse.StatusCode, data)
	}
	var payload responsePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return model.Response{}, fmt.Errorf("gemini: decode response: %w", err)
	}
	return normalizeResponse(payload, request.ResponseFormat)
}

func (client *Client) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	response, err := client.Invoke(ctx, request)
	if err != nil {
		return nil, err
	}
	return &singleResponseStream{response: response}, nil
}

type requestPayload struct {
	SystemInstruction *wireContent      `json:"systemInstruction,omitempty"`
	Contents          []wireContent     `json:"contents"`
	Tools             []wireTool        `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig   `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *wireBlob         `json:"inlineData,omitempty"`
	FileData         *wireFile         `json:"fileData,omitempty"`
	FunctionCall     *wireFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResp `json:"functionResponse,omitempty"`
}

type wireBlob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFile struct {
	MIMEType string `json:"mimeType,omitempty"`
	URI      string `json:"fileUri"`
}

type wireFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type wireFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type wireTool struct {
	FunctionDeclarations []wireFunctionDeclaration `json:"functionDeclarations"`
}

type wireFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type wireToolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type generationConfig struct {
	MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   map[string]any  `json:"responseSchema,omitempty"`
	ThinkingConfig   *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
}

func (client *Client) requestBody(request model.Request) ([]byte, error) {
	contents, system, err := toWireMessages(request.Messages)
	if err != nil {
		return nil, err
	}
	definitions := request.Tools
	if len(definitions) == 0 {
		definitions = client.boundTools
	}
	declarations := make([]wireFunctionDeclaration, 0, len(definitions))
	for _, definition := range definitions {
		if err := definition.Validate(); err != nil {
			return nil, err
		}
		var schema map[string]any
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("gemini: decode schema for %q: %w", definition.Name, err)
		}
		declarations = append(declarations, wireFunctionDeclaration{Name: definition.Name, Description: definition.Description, Parameters: normalizeSchema(schema)})
	}
	payload := requestPayload{Contents: contents, SystemInstruction: system}
	if len(declarations) > 0 {
		payload.Tools = []wireTool{{FunctionDeclarations: declarations}}
	}
	if request.ToolChoice != nil {
		config := &wireToolConfig{}
		switch request.ToolChoice.Mode {
		case "", "auto":
			config.FunctionCallingConfig.Mode = "AUTO"
		case "none":
			config.FunctionCallingConfig.Mode = "NONE"
		case "required":
			config.FunctionCallingConfig.Mode = "ANY"
		case "tool", "function", "name":
			config.FunctionCallingConfig.Mode = "ANY"
			config.FunctionCallingConfig.AllowedFunctionNames = []string{request.ToolChoice.Name}
		default:
			return nil, fmt.Errorf("gemini: unsupported tool choice %q", request.ToolChoice.Mode)
		}
		payload.ToolConfig = config
	}
	generation := &generationConfig{MaxOutputTokens: client.options.MaxOutputTokens, StopSequences: append([]string(nil), request.Stop...)}
	if request.ResponseFormat != nil {
		var schema map[string]any
		if err := json.Unmarshal(request.ResponseFormat.Schema, &schema); err != nil {
			return nil, fmt.Errorf("gemini: decode response schema: %w", err)
		}
		generation.ResponseMIMEType = "application/json"
		generation.ResponseSchema = normalizeSchema(schema)
	}
	reasoning := request.Reasoning
	if reasoning == nil {
		reasoning = client.options.DefaultReasoning
	}
	if reasoning != nil && reasoning.Effort != "" {
		generation.ThinkingConfig = geminiThinking(client.options.Model, reasoning)
	}
	if generation.MaxOutputTokens != 0 || len(generation.StopSequences) > 0 || generation.ResponseMIMEType != "" || generation.ThinkingConfig != nil {
		payload.GenerationConfig = generation
	}
	return json.Marshal(payload)
}

func geminiThinking(modelID string, reasoning *model.Reasoning) *thinkingConfig {
	include := reasoning.Summary != ""
	if strings.HasPrefix(strings.ToLower(modelID), "gemini-3") {
		effort := reasoning.Effort
		if effort == "off" || effort == "none" {
			effort = "low"
		}
		if effort == "xhigh" || effort == "max" {
			effort = "high"
		}
		return &thinkingConfig{ThinkingLevel: effort, IncludeThoughts: include}
	}
	budget := reasoningBudget(reasoning.Effort)
	return &thinkingConfig{ThinkingBudget: &budget, IncludeThoughts: include && budget != 0}
}

func reasoningBudget(effort string) int {
	switch effort {
	case "off", "none":
		return 0
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
		return -1
	}
}

func toWireMessages(messages []message.Message) ([]wireContent, *wireContent, error) {
	callNames := map[string]string{}
	for _, item := range messages {
		for _, call := range item.ToolCalls {
			callNames[call.ID] = call.Name
		}
	}
	var contents []wireContent
	var systemParts []wirePart
	appendContent := func(role string, parts []wirePart) {
		if len(parts) == 0 {
			return
		}
		if len(contents) > 0 && contents[len(contents)-1].Role == role {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
			return
		}
		contents = append(contents, wireContent{Role: role, Parts: parts})
	}
	for _, item := range messages {
		if item.Role == message.RoleRemove {
			continue
		}
		if item.Role == message.RoleSystem {
			if text := item.TextContent(); text != "" {
				systemParts = append(systemParts, wirePart{Text: text})
			}
			continue
		}
		role := "user"
		if item.Role == message.RoleAssistant {
			role = "model"
		} else if item.Role != message.RoleHuman && item.Role != message.RoleTool {
			return nil, nil, fmt.Errorf("gemini: unsupported message role %q", item.Role)
		}
		if item.Role == message.RoleTool {
			name := callNames[item.ToolCallID]
			if name == "" {
				return nil, nil, fmt.Errorf("gemini: no function name for tool call %q", item.ToolCallID)
			}
			response := map[string]any{"result": item.TextContent(), "error": item.ToolStatus == message.ToolStatusError}
			appendContent("user", []wirePart{{FunctionResponse: &wireFunctionResp{Name: name, Response: response}}})
			continue
		}
		parts, err := toWireParts(item.Content)
		if err != nil {
			return nil, nil, err
		}
		for _, call := range item.ToolCalls {
			var arguments map[string]any
			if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
				return nil, nil, fmt.Errorf("gemini: decode tool call %q: %w", call.ID, err)
			}
			part := wirePart{FunctionCall: &wireFunctionCall{Name: call.Name, Args: arguments}}
			if raw := item.ResponseMetadata["gemini.thought_signature."+call.ID]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &part.ThoughtSignature)
			}
			parts = append(parts, part)
		}
		appendContent(role, parts)
	}
	var system *wireContent
	if len(systemParts) > 0 {
		system = &wireContent{Parts: systemParts}
	}
	return contents, system, nil
}

func toWireParts(blocks []message.ContentBlock) ([]wirePart, error) {
	result := make([]wirePart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case message.BlockText:
			part := wirePart{Text: block.Text}
			if raw := block.Extra["thought_signature"]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &part.ThoughtSignature)
			}
			result = append(result, part)
		case message.BlockReasoning:
			part := wirePart{Text: block.Reasoning, Thought: true}
			if raw := block.Extra["thought_signature"]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &part.ThoughtSignature)
			}
			result = append(result, part)
		case message.BlockImage:
			if len(block.Data) > 0 {
				result = append(result, wirePart{InlineData: &wireBlob{MIMEType: block.MIMEType, Data: base64.StdEncoding.EncodeToString(block.Data)}})
			} else if block.URL != "" {
				result = append(result, wirePart{FileData: &wireFile{MIMEType: block.MIMEType, URI: block.URL}})
			} else {
				return nil, fmt.Errorf("gemini: image block requires URL or data")
			}
		}
	}
	return result, nil
}

func normalizeSchema(schema map[string]any) map[string]any {
	result := make(map[string]any, len(schema))
	for key, value := range schema {
		switch key {
		case "$schema", "$id", "additionalProperties", "default", "examples", "title":
			continue
		case "type":
			if text, ok := value.(string); ok {
				result[key] = strings.ToUpper(text)
				continue
			}
		case "properties":
			if properties, ok := value.(map[string]any); ok {
				normalized := make(map[string]any, len(properties))
				for name, property := range properties {
					if object, ok := property.(map[string]any); ok {
						normalized[name] = normalizeSchema(object)
					}
				}
				result[key] = normalized
				continue
			}
		case "items":
			if items, ok := value.(map[string]any); ok {
				result[key] = normalizeSchema(items)
				continue
			}
		}
		result[key] = value
	}
	return result
}

type responsePayload struct {
	Candidates    []candidate   `json:"candidates"`
	UsageMetadata usageMetadata `json:"usageMetadata"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

type candidate struct {
	Content      wireContent `json:"content"`
	FinishReason string      `json:"finishReason"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

func normalizeResponse(payload responsePayload, format *model.ResponseFormat) (model.Response, error) {
	if payload.Error != nil {
		return model.Response{}, fmt.Errorf("gemini: %s", payload.Error.Message)
	}
	if len(payload.Candidates) == 0 {
		return model.Response{}, fmt.Errorf("gemini: response contained no candidates")
	}
	result := message.Message{Role: message.RoleAssistant, ResponseMetadata: map[string]json.RawMessage{}}
	for index, part := range payload.Candidates[0].Content.Parts {
		if part.FunctionCall != nil {
			arguments, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return model.Response{}, fmt.Errorf("gemini: encode tool arguments: %w", err)
			}
			id := fmt.Sprintf("gemini_tool_%s_%d", part.FunctionCall.Name, index)
			result.ToolCalls = append(result.ToolCalls, message.ToolCall{ID: id, Name: part.FunctionCall.Name, Arguments: arguments})
			if part.ThoughtSignature != "" {
				result.ResponseMetadata["gemini.thought_signature."+id] = mustJSON(part.ThoughtSignature)
			}
			continue
		}
		if part.Text == "" {
			continue
		}
		block := message.ContentBlock{Type: message.BlockText, Text: part.Text}
		if part.Thought {
			block.Type, block.Text, block.Reasoning = message.BlockReasoning, "", part.Text
		}
		if part.ThoughtSignature != "" {
			block.Extra = map[string]json.RawMessage{"thought_signature": mustJSON(part.ThoughtSignature)}
		}
		result.Content = append(result.Content, block)
	}
	if len(result.ResponseMetadata) == 0 {
		result.ResponseMetadata = nil
	}
	result.Usage = &message.Usage{InputTokens: payload.UsageMetadata.PromptTokenCount, OutputTokens: payload.UsageMetadata.CandidatesTokenCount, TotalTokens: payload.UsageMetadata.TotalTokenCount}
	response := model.Response{Message: result}
	if format != nil {
		text := result.TextContent()
		if !json.Valid([]byte(text)) {
			return model.Response{}, fmt.Errorf("gemini: structured response is not valid JSON")
		}
		response.Structured = json.RawMessage(text)
	}
	return response, nil
}

func geminiError(status int, data []byte) error {
	var payload responsePayload
	_ = json.Unmarshal(data, &payload)
	detail := strings.TrimSpace(string(data))
	if payload.Error != nil && payload.Error.Message != "" {
		detail = payload.Error.Message
	}
	return fmt.Errorf("gemini: status %d: %s", status, detail)
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

type singleResponseStream struct {
	response model.Response
	done     bool
}

func (stream *singleResponseStream) Next(context.Context) (model.Chunk, error) {
	if stream.done {
		return model.Chunk{}, io.EOF
	}
	stream.done = true
	return model.Chunk{MessageDelta: stream.response.Message, Structured: stream.response.Structured, Done: true}, nil
}

func (stream *singleResponseStream) Close() error {
	stream.done = true
	return nil
}

func cloneDefinitions(values []tool.Definition) []tool.Definition {
	result := make([]tool.Definition, len(values))
	for index, value := range values {
		result[index] = value
		result[index].InputSchema = append(json.RawMessage(nil), value.InputSchema...)
	}
	return result
}
