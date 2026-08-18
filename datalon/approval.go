package datalon

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	// ErrInvalidToolApproval reports an unsafe or malformed approval request.
	ErrInvalidToolApproval = errors.New("invalid channel tool approval")
	// ErrToolApprovalPending reports an ambiguous second approval request for
	// the same active channel conversation.
	ErrToolApprovalPending = errors.New("channel tool approval already pending")
	// ErrToolApprovalTimeout reports a pending approval that reached its finite
	// host deadline. The corresponding decision is always reject.
	ErrToolApprovalTimeout = errors.New("channel tool approval timed out")
)

// ToolApprovalDecision is the portable approve/reject result returned by a
// channel operator. Channel approval is experimental convenience, not a
// complete authorization or isolation boundary.
type ToolApprovalDecision string

const (
	ToolApprovalApprove ToolApprovalDecision = "approve"
	ToolApprovalReject  ToolApprovalDecision = "reject"
)

// ToolApprovalAction describes one gated runtime tool call. Arguments must be
// valid JSON and are shown, with a finite bound, to the channel operator.
type ToolApprovalAction struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolApprovalRequest is one batch of tool calls awaiting a single channel
// decision. The host binds ConversationID to the active channel request.
type ToolApprovalRequest struct {
	ConversationID string               `json:"conversation_id"`
	InterruptID    string               `json:"interrupt_id"`
	Actions        []ToolApprovalAction `json:"actions"`
}

// ToolApprovalHandler asks the originating channel operator for one decision.
// Any returned error must be treated as rejection by the runtime.
type ToolApprovalHandler func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error)
