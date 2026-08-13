// Package damessage defines the provider-neutral messages exchanged by models, tools,
// agents, and checkpoints.
package damessage

import (
	"encoding/json"
	"fmt"
	"time"
)

// Role identifies the source or control purpose of a message.
type Role string

const (
	RoleHuman     Role = "human"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
	RoleRemove    Role = "remove"
)

// BlockType identifies a standard message content block.
type BlockType string

const (
	BlockText         BlockType = "text"
	BlockReasoning    BlockType = "reasoning"
	BlockImage        BlockType = "image"
	BlockFile         BlockType = "file"
	BlockAudio        BlockType = "audio"
	BlockVideo        BlockType = "video"
	BlockCitation     BlockType = "citation"
	BlockServerTool   BlockType = "server_tool_use"
	BlockSearchResult BlockType = "web_search_result"
	BlockNonStandard  BlockType = "non_standard"
)

// ContentBlock is a language-neutral standard content record. Extra values must be
// valid JSON and are preserved for provider adapters.
type ContentBlock struct {
	Type        BlockType                  `json:"type"`
	ID          string                     `json:"id,omitempty"`
	Text        string                     `json:"text,omitempty"`
	Reasoning   string                     `json:"reasoning,omitempty"`
	URL         string                     `json:"url,omitempty"`
	Data        []byte                     `json:"data,omitempty"`
	MIMEType    string                     `json:"mime_type,omitempty"`
	Name        string                     `json:"name,omitempty"`
	Index       *int                       `json:"index,omitempty"`
	Citations   []Citation                 `json:"citations,omitempty"`
	Extra       map[string]json.RawMessage `json:"extra,omitempty"`
	NonStandard json.RawMessage            `json:"value,omitempty"`
}

// Citation identifies a source span associated with generated text.
type Citation struct {
	ID         string `json:"id,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex *int   `json:"start_index,omitempty"`
	EndIndex   *int   `json:"end_index,omitempty"`
	CitedText  string `json:"cited_text,omitempty"`
}

// ToolCall is a normalized model request to invoke a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// InvalidToolCall retains a provider tool call that could not be normalized.
type InvalidToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Error     string          `json:"error"`
}

// Usage contains normalized token accounting and optional provider breakdowns.
type Usage struct {
	InputTokens   int            `json:"input_tokens"`
	OutputTokens  int            `json:"output_tokens"`
	TotalTokens   int            `json:"total_tokens"`
	InputDetails  map[string]int `json:"input_token_details,omitempty"`
	OutputDetails map[string]int `json:"output_token_details,omitempty"`
	Provider      string         `json:"provider,omitempty"`
	Model         string         `json:"model,omitempty"`
	URL           string         `json:"url,omitempty"`
	CostUSD       float64        `json:"cost_usd,omitempty"`
	StartedAt     time.Time      `json:"started_at,omitzero"`
	FinishedAt    time.Time      `json:"finished_at,omitzero"`
}

// PurposedUsage accounts for a nested model call made while producing another
// message, such as summarization or a model-backed tool.
type PurposedUsage struct {
	Purpose string `json:"purpose"`
	Usage
}

// ToolStatus describes whether a tool call completed successfully.
type ToolStatus string

const (
	ToolStatusSuccess ToolStatus = "success"
	ToolStatusError   ToolStatus = "error"
)

// Message is the canonical message representation. Metadata and artifact values are
// raw JSON so they remain safe to checkpoint across languages.
type Message struct {
	ID               string                     `json:"id,omitempty"`
	Role             Role                       `json:"role"`
	Name             string                     `json:"name,omitempty"`
	Content          []ContentBlock             `json:"content,omitempty"`
	ToolCalls        []ToolCall                 `json:"tool_calls,omitempty"`
	InvalidToolCalls []InvalidToolCall          `json:"invalid_tool_calls,omitempty"`
	ToolCallID       string                     `json:"tool_call_id,omitempty"`
	ToolStatus       ToolStatus                 `json:"tool_status,omitempty"`
	Artifact         json.RawMessage            `json:"artifact,omitempty"`
	Metadata         map[string]json.RawMessage `json:"metadata,omitempty"`
	ResponseMetadata map[string]json.RawMessage `json:"response_metadata,omitempty"`
	Usage            *Usage                     `json:"usage,omitempty"`
	OtherUsage       []PurposedUsage            `json:"other_usage,omitempty"`
}

// Text creates a one-block text message.
func Text(role Role, text string) Message {
	return Message{Role: role, Content: []ContentBlock{{Type: BlockText, Text: text}}}
}

// Human creates a human text message.
func Human(text string) Message {
	return Text(RoleHuman, text)
}

// Assistant creates an assistant text message.
func Assistant(text string) Message {
	return Text(RoleAssistant, text)
}

// System creates a system text message.
func System(text string) Message {
	return Text(RoleSystem, text)
}

// Tool creates a successful tool-result message.
func Tool(callID, text string) Message {
	message := Text(RoleTool, text)
	message.ToolCallID = callID
	message.ToolStatus = ToolStatusSuccess
	return message
}

// MessageFrom normalizes a user-supplied message value. Message values pass
// through unchanged, strings become human text, and all other values are
// JSON-encoded as human text.
func MessageFrom(value any) (Message, error) {
	if message, ok := value.(Message); ok {
		return message, nil
	}
	if message, ok := value.(*Message); ok {
		if message == nil {
			return Human("null"), nil
		}
		return *message, nil
	}
	if text, ok := value.(string); ok {
		return Human(text), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return Message{}, fmt.Errorf("convert message to JSON: %w", err)
	}
	return Human(string(encoded)), nil
}

// Remove creates a message tombstone for an existing ID.
func Remove(id string) Message {
	return Message{ID: id, Role: RoleRemove}
}

// Clone returns a deep copy suitable for reducer, model, and checkpoint boundaries.
func (message Message) Clone() Message {
	copy := message
	copy.Content = cloneSlice(message.Content)
	for index := range copy.Content {
		copy.Content[index].Data = cloneSlice(message.Content[index].Data)
		copy.Content[index].Citations = cloneSlice(message.Content[index].Citations)
		copy.Content[index].Extra = cloneRawMap(message.Content[index].Extra)
		copy.Content[index].NonStandard = cloneRaw(message.Content[index].NonStandard)
	}
	copy.ToolCalls = cloneSlice(message.ToolCalls)
	for index := range copy.ToolCalls {
		copy.ToolCalls[index].Arguments = cloneRaw(message.ToolCalls[index].Arguments)
	}
	copy.InvalidToolCalls = cloneSlice(message.InvalidToolCalls)
	for index := range copy.InvalidToolCalls {
		copy.InvalidToolCalls[index].Arguments = cloneRaw(message.InvalidToolCalls[index].Arguments)
	}
	copy.Artifact = cloneRaw(message.Artifact)
	copy.Metadata = cloneRawMap(message.Metadata)
	copy.ResponseMetadata = cloneRawMap(message.ResponseMetadata)
	if message.Usage != nil {
		usage := *message.Usage
		usage.InputDetails = cloneMap(message.Usage.InputDetails)
		usage.OutputDetails = cloneMap(message.Usage.OutputDetails)
		copy.Usage = &usage
	}
	copy.OtherUsage = cloneSlice(message.OtherUsage)
	for index := range copy.OtherUsage {
		copy.OtherUsage[index].InputDetails = cloneMap(message.OtherUsage[index].InputDetails)
		copy.OtherUsage[index].OutputDetails = cloneMap(message.OtherUsage[index].OutputDetails)
	}
	return copy
}

// TextContent concatenates the text blocks in display order.
func (message Message) TextContent() string {
	var result string
	for _, block := range message.Content {
		if block.Type == BlockText {
			result += block.Text
		}
	}
	return result
}

// Validate checks language-neutral message invariants without applying
// provider-specific restrictions.
func (message Message) Validate() error {
	switch message.Role {
	case RoleHuman, RoleAssistant, RoleSystem, RoleTool, RoleRemove:
	default:
		return fmt.Errorf("message role %q is unsupported", message.Role)
	}
	if message.Role == RoleRemove {
		if message.ID == "" {
			return fmt.Errorf("remove message requires an id")
		}
		return nil
	}
	if message.Role == RoleTool && message.ToolCallID == "" {
		return fmt.Errorf("tool message requires a tool call id")
	}
	for index, call := range message.ToolCalls {
		if call.ID == "" || call.Name == "" {
			return fmt.Errorf("tool call %d requires id and name", index)
		}
		if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
			return fmt.Errorf("tool call %q has invalid JSON arguments", call.ID)
		}
	}
	for index, block := range message.Content {
		if block.Type == "" {
			return fmt.Errorf("content block %d requires a type", index)
		}
		if block.Type == BlockNonStandard && !json.Valid(block.NonStandard) {
			return fmt.Errorf("content block %d has invalid non-standard JSON", index)
		}
	}
	return nil
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return cloneSlice(value)
}

func cloneRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		result[key] = cloneRaw(value)
	}
	return result
}

func cloneMap[K comparable, V any](values map[K]V) map[K]V {
	if values == nil {
		return nil
	}
	result := make(map[K]V, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append([]T{}, values...)
}
