// Package model defines provider-neutral chat model contracts.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/tool"
)

var ErrContextOverflow = errors.New("model context window exceeded")

// ToolChoice controls whether and how a model may call tools.
type ToolChoice struct {
	Mode string `json:"mode,omitempty"`
	Name string `json:"name,omitempty"`
}

// ResponseFormat requests provider-native or tool-based structured output.
type ResponseFormat struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict,omitempty"`
}

// PromptCache is a provider-neutral cache hint. Adapters that do not advertise
// SupportsPromptCaching ignore it.
type PromptCache struct {
	Key       string `json:"key,omitempty"`
	Retention string `json:"retention,omitempty"`
}

// Request is one model invocation.
type Request struct {
	Messages       []message.Message          `json:"messages"`
	Tools          []tool.Definition          `json:"tools,omitempty"`
	ToolChoice     *ToolChoice                `json:"tool_choice,omitempty"`
	ResponseFormat *ResponseFormat            `json:"response_format,omitempty"`
	PromptCache    *PromptCache               `json:"prompt_cache,omitempty"`
	Metadata       map[string]json.RawMessage `json:"metadata,omitempty"`
	Tags           []string                   `json:"tags,omitempty"`
	Stop           []string                   `json:"stop,omitempty"`
}

// Response is one complete model result.
type Response struct {
	Message    message.Message `json:"message"`
	Structured json.RawMessage `json:"structured,omitempty"`
}

// Chunk is an ordered model stream fragment. MessageDelta uses the same explicit
// content records as complete messages; provider adapters own chunk coalescing.
type Chunk struct {
	MessageDelta message.Message `json:"message_delta"`
	Structured   json.RawMessage `json:"structured,omitempty"`
	Done         bool            `json:"done,omitempty"`
}

// Stream yields chunks until io.EOF. Close must stop producer work promptly.
type Stream interface {
	Next(ctx context.Context) (Chunk, error)
	Close() error
}

// Chat is the minimal model contract consumed by the agent factory.
type Chat interface {
	Invoke(ctx context.Context, request Request) (Response, error)
	Stream(ctx context.Context, request Request) (Stream, error)
	Profile() Profile
}

// Binder is implemented by models that require explicit tool binding before an
// invocation. Models may instead consume Request.Tools directly.
type Binder interface {
	BindTools(tools []tool.Definition) (Chat, error)
}

// TokenCounter supplies model-specific token counts.
type TokenCounter interface {
	CountTokens(ctx context.Context, messages []message.Message) (int, error)
}

// Profile records capabilities used for agent routing and middleware decisions.
type Profile struct {
	Provider              string `json:"provider,omitempty"`
	Model                 string `json:"model,omitempty"`
	ContextWindow         int    `json:"context_window,omitempty"`
	MaxOutputTokens       int    `json:"max_output_tokens,omitempty"`
	ToolCalling           bool   `json:"tool_calling,omitempty"`
	ParallelToolCalls     bool   `json:"parallel_tool_calls,omitempty"`
	StructuredOutput      bool   `json:"structured_output,omitempty"`
	NativeStreaming       bool   `json:"native_streaming,omitempty"`
	SupportsPromptCaching bool   `json:"supports_prompt_caching,omitempty"`
}

// EmptyStream is a stream that immediately terminates.
type EmptyStream struct{}

func (EmptyStream) Next(context.Context) (Chunk, error) { return Chunk{}, io.EOF }
func (EmptyStream) Close() error                        { return nil }
