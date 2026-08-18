package datalon

import "context"

// Message is one inbound channel message.
type Message struct {
	ConversationID string         `json:"conversation_id"`
	Text           string         `json:"text"`
	SenderID       string         `json:"sender_id,omitempty"`
	MessageID      string         `json:"message_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// Request is one serialized agent invocation.
type Request struct {
	ConversationID string         `json:"conversation_id"`
	Text           string         `json:"text"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Workspace      string         `json:"workspace"`
	RecursionLimit int            `json:"recursion_limit"`
	// ApprovalHandler is present only for channel-originated runs and is never
	// serialized. Scheduled and other handlerless runs must reject gated tools.
	ApprovalHandler ToolApprovalHandler `json:"-"`
}

// Result is the agent output to deliver to a channel.
type Result struct {
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SendResult describes a channel delivery attempt.
type SendResult struct {
	Success   bool   `json:"success"`
	MessageID string `json:"message_id,omitempty"`
	Error     string `json:"error,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

// Handler receives inbound messages from a Channel.
type Handler func(context.Context, Message) error

// Channel is a transport integration managed by Host. ID must be stable and
// unique within a host because it namespaces channel conversation identifiers.
type Channel interface {
	ID() string
	Start(context.Context, Handler) error
	Stop(context.Context) error
	Send(context.Context, string, string) SendResult
}

// ScheduledJob is one scheduler-originated agent request.
type ScheduledJob struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversation_id"`
	Prompt         string         `json:"prompt"`
	ChannelID      string         `json:"channel_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// ScheduledHandler runs a job and returns text for scheduler-owned delivery.
type ScheduledHandler func(context.Context, ScheduledJob) (string, error)

// Scheduler is an optional persistent job source managed by Host.
type Scheduler interface {
	Start(context.Context, ScheduledHandler) error
	Stop(context.Context) error
}

// Runtime is the agent implementation managed and invoked by Host.
type Runtime interface {
	Start(context.Context) error
	Stop(context.Context) error
	Invoke(context.Context, Request) (Result, error)
}
