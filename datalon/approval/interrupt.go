package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/datalon"
)

var ErrInvalidInterrupt = errors.New("invalid Talon approval interrupt")

// ResolveInterrupt asks the request's channel handler for one decision and
// expands it into the provider-neutral resume value expected by HumanApproval.
// It handles both live and checkpoint-restored interrupts. Runtimes invoke it
// for every human_approval interrupt, whether the rule came from Policy or the
// agent's base configuration.
func ResolveInterrupt(
	ctx context.Context,
	request datalon.Request,
	interrupt dagent.Interrupt,
) (dagent.ApprovalResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return dagent.ApprovalResponse{}, err
	}
	if interrupt.ID != "human_approval" {
		return dagent.ApprovalResponse{}, fmt.Errorf("%w: unsupported interrupt ID", ErrInvalidInterrupt)
	}
	pending, ok := dagent.InterruptAs[[]dagent.ApprovalRequest](interrupt)
	if !ok || len(pending) == 0 {
		return dagent.ApprovalResponse{}, fmt.Errorf("%w: approval actions are required", ErrInvalidInterrupt)
	}
	actions := make([]datalon.ToolApprovalAction, 0, len(pending))
	seen := make(map[string]struct{}, len(pending))
	for index, item := range pending {
		call := item.Call
		if call.ID == "" || call.Name == "" {
			return dagent.ApprovalResponse{}, fmt.Errorf("%w: action %d requires an ID and name", ErrInvalidInterrupt, index)
		}
		if _, exists := seen[call.ID]; exists {
			return dagent.ApprovalResponse{}, fmt.Errorf("%w: duplicate action ID", ErrInvalidInterrupt)
		}
		if len(call.Arguments) > 0 && !json.Valid(call.Arguments) {
			return dagent.ApprovalResponse{}, fmt.Errorf("%w: action %d arguments are not valid JSON", ErrInvalidInterrupt, index)
		}
		seen[call.ID] = struct{}{}
		actions = append(actions, datalon.ToolApprovalAction{
			ID: call.ID, Name: call.Name, Arguments: append([]byte(nil), call.Arguments...),
		})
	}
	if request.ApprovalHandler == nil {
		return dagent.ApprovalResponse{}, ErrApprovalUnavailable
	}
	decision, err := request.ApprovalHandler(ctx, datalon.ToolApprovalRequest{
		ConversationID: request.ConversationID,
		InterruptID:    interrupt.ID,
		Actions:        actions,
	})
	if err != nil {
		return dagent.ApprovalResponse{}, errors.Join(ErrToolRejected, err)
	}
	resumeDecision := dagent.ApprovalReject
	switch decision {
	case datalon.ToolApprovalApprove:
		resumeDecision = dagent.ApprovalApprove
	case datalon.ToolApprovalReject:
	default:
		return dagent.ApprovalResponse{}, fmt.Errorf("%w: invalid approval decision", ErrToolRejected)
	}
	decisions := make(map[string]dagent.ApprovalChoice, len(pending))
	for _, item := range pending {
		if !decisionAllowed(item.AllowedDecisions, resumeDecision) {
			return dagent.ApprovalResponse{}, fmt.Errorf("%w: decision is not allowed for the requested tool", ErrToolRejected)
		}
		choice := dagent.ApprovalChoice{Decision: resumeDecision}
		if resumeDecision == dagent.ApprovalReject {
			choice.Reason = "Denied by operator."
		}
		decisions[item.Call.ID] = choice
	}
	return dagent.ApprovalResponse{Decisions: decisions}, nil
}

func decisionAllowed(allowed []dagent.ApprovalDecision, decision dagent.ApprovalDecision) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate == decision {
			return true
		}
	}
	return false
}
