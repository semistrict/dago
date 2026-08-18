package approval

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/dago/datalon"
)

var (
	// ErrApprovalUnavailable reports a forced tool in a scheduled or channel run
	// whose runtime request has no approval handler.
	ErrApprovalUnavailable = errors.New("Talon channel tool approval is unavailable")
	// ErrToolRejected reports an operator rejection or invalid handler decision.
	ErrToolRejected = errors.New("Talon tool call rejected")
)

// Authorize asks for one channel decision when any action is forced by policy.
// Runtimes must call it before executing local or MCP actions. Handler absence,
// handler failure, cancellation, timeout, rejection, and invalid decisions all
// fail closed. Duplicate actions remain visible in the approval batch.
func (policy Policy) Authorize(
	ctx context.Context,
	request datalon.Request,
	interruptID string,
	actions ...datalon.ToolApprovalAction,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	gated := make([]datalon.ToolApprovalAction, 0, len(actions))
	for _, action := range actions {
		if policy.Requires(action.Name) {
			copy := action
			copy.Arguments = append([]byte(nil), action.Arguments...)
			gated = append(gated, copy)
		}
	}
	if len(gated) == 0 {
		return nil
	}
	if request.ApprovalHandler == nil {
		return ErrApprovalUnavailable
	}
	decision, err := request.ApprovalHandler(ctx, datalon.ToolApprovalRequest{
		ConversationID: request.ConversationID,
		InterruptID:    interruptID,
		Actions:        gated,
	})
	if err != nil {
		return errors.Join(ErrToolRejected, err)
	}
	switch decision {
	case datalon.ToolApprovalApprove:
		return nil
	case datalon.ToolApprovalReject:
		return ErrToolRejected
	default:
		return fmt.Errorf("%w: invalid approval decision", ErrToolRejected)
	}
}
