// Package model defines provider-neutral chat model contracts.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/tool"
)

var ErrContextOverflow = errors.New("model context window exceeded")

// RetryEvent is provider-neutral metadata for a transient model failure.
// Applications may project it into logs or user-visible retry notices.
type RetryEvent struct {
	Attempt   int
	Delay     time.Duration
	Retryable bool
	Err       string
	Status    int
	Provider  string
	Model     string
}

// RetryReporter is implemented by model errors that carry structured retry
// metadata. It does not decide retry policy; the invoking runtime owns that.
type RetryReporter interface {
	RetryEvent(attempt int, delay time.Duration) RetryEvent
}

// ResponseMetadataKey marks assistant messages that were produced by a model
// invocation. Agent middleware and projections use it to distinguish model
// output from caller-supplied assistant history.
const ResponseMetadataKey = "dago.model.response.v1"

const (
	FinishReasonMetadataKey = "dago.model.finish_reason.v1"
	RefusalMetadataKey      = "dago.model.refusal.v1"
)

// FinishReason is the provider-neutral terminal condition for a model turn.
type FinishReason string

const (
	FinishReasonStop      FinishReason = "stop"
	FinishReasonToolCalls FinishReason = "tool_calls"
	FinishReasonMaxTokens FinishReason = "max_tokens"
	FinishReasonRefusal   FinishReason = "refusal"
)

// Refusal carries provider-supplied refusal details without coupling callers
// to a provider response schema.
type Refusal struct {
	Category    string `json:"category,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

// SetOutcome writes a normalized model outcome onto an assistant message so
// it survives streaming, checkpoints, and application projections.
func SetOutcome(value *message.Message, reason FinishReason, refusal *Refusal) {
	if value.ResponseMetadata == nil {
		value.ResponseMetadata = map[string]json.RawMessage{}
	}
	if reason != "" {
		raw, _ := json.Marshal(reason)
		value.ResponseMetadata[FinishReasonMetadataKey] = raw
	}
	if refusal != nil {
		raw, _ := json.Marshal(refusal)
		value.ResponseMetadata[RefusalMetadataKey] = raw
	}
}

// Outcome reads normalized model outcome metadata from a message.
func Outcome(value message.Message) (FinishReason, *Refusal) {
	var reason FinishReason
	if raw := value.ResponseMetadata[FinishReasonMetadataKey]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &reason)
	}
	var refusal *Refusal
	if raw := value.ResponseMetadata[RefusalMetadataKey]; len(raw) > 0 {
		var decoded Refusal
		if json.Unmarshal(raw, &decoded) == nil {
			refusal = &decoded
		}
	}
	return reason, refusal
}

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

// Reasoning requests provider-native reasoning behavior. Effort is deliberately
// a string because providers expose different, evolving level vocabularies.
// Summary asks the provider to return a displayable reasoning summary when it
// supports one.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Request is one model invocation.
type Request struct {
	Messages       []message.Message          `json:"messages"`
	Tools          []tool.Definition          `json:"tools,omitempty"`
	ToolChoice     *ToolChoice                `json:"tool_choice,omitempty"`
	ResponseFormat *ResponseFormat            `json:"response_format,omitempty"`
	PromptCache    *PromptCache               `json:"prompt_cache,omitempty"`
	Reasoning      *Reasoning                 `json:"reasoning,omitempty"`
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
	Provider              string   `json:"provider,omitempty"`
	Model                 string   `json:"model,omitempty"`
	ContextWindow         int      `json:"context_window,omitempty"`
	MaxOutputTokens       int      `json:"max_output_tokens,omitempty"`
	ToolCalling           bool     `json:"tool_calling,omitempty"`
	ParallelToolCalls     bool     `json:"parallel_tool_calls,omitempty"`
	StructuredOutput      bool     `json:"structured_output,omitempty"`
	NativeStreaming       bool     `json:"native_streaming,omitempty"`
	SupportsPromptCaching bool     `json:"supports_prompt_caching,omitempty"`
	SupportsReasoning     bool     `json:"supports_reasoning,omitempty"`
	ReasoningLevels       []string `json:"reasoning_levels,omitempty"`
	DefaultReasoningLevel string   `json:"default_reasoning_level,omitempty"`
	SupportsImages        bool     `json:"supports_images,omitempty"`
	SupportsWebSearch     bool     `json:"supports_web_search,omitempty"`
	UseSimplifiedPatch    bool     `json:"use_simplified_patch,omitempty"`
	MaxImageDimension     int      `json:"max_image_dimension,omitempty"`
	MaxImageBytes         int      `json:"max_image_bytes,omitempty"`
}

// EmptyStream is a stream that immediately terminates.
type EmptyStream struct{}

func (EmptyStream) Next(context.Context) (Chunk, error) { return Chunk{}, io.EOF }
func (EmptyStream) Close() error                        { return nil }
