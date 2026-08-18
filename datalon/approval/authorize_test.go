package approval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/semistrict/dago/datalon"
)

func TestAuthorizeGatesOnlyForcedActionsAndKeepsDuplicates(t *testing.T) {
	t.Parallel()
	policy := NewPolicy("local/write", "mcp/delete")
	var received datalon.ToolApprovalRequest
	request := datalon.Request{
		ConversationID: "chat:room",
		ApprovalHandler: func(_ context.Context, approval datalon.ToolApprovalRequest) (datalon.ToolApprovalDecision, error) {
			received = approval
			return datalon.ToolApprovalApprove, nil
		},
	}
	actions := []datalon.ToolApprovalAction{
		{ID: "1", Name: "local/write", Arguments: json.RawMessage(`{"path":"one"}`)},
		{ID: "2", Name: "unknown", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "local/write", Arguments: json.RawMessage(`{"path":"two"}`)},
	}
	if err := policy.Authorize(t.Context(), request, "interrupt", actions...); err != nil {
		t.Fatal(err)
	}
	if received.ConversationID != "chat:room" || received.InterruptID != "interrupt" {
		t.Fatalf("request scope = %+v", received)
	}
	if len(received.Actions) != 2 || received.Actions[0].ID != "1" || received.Actions[1].ID != "3" {
		t.Fatalf("gated actions = %+v", received.Actions)
	}
	actions[0].Arguments[0] = '['
	if received.Actions[0].Arguments[0] != '{' {
		t.Fatal("handler arguments alias caller memory")
	}
}

func TestAuthorizeFailClosed(t *testing.T) {
	t.Parallel()
	policy := NewPolicy("danger")
	action := datalon.ToolApprovalAction{Name: "danger", Arguments: json.RawMessage(`{}`)}
	tests := []struct {
		name    string
		request datalon.Request
		want    error
	}{
		{name: "scheduled without handler", request: datalon.Request{}, want: ErrApprovalUnavailable},
		{name: "operator rejects", request: requestWithDecision(datalon.ToolApprovalReject, nil), want: ErrToolRejected},
		{name: "invalid decision", request: requestWithDecision("maybe", nil), want: ErrToolRejected},
		{name: "handler fails", request: requestWithDecision("", errors.New("transport stopped")), want: ErrToolRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := policy.Authorize(t.Context(), test.request, "interrupt", action); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAuthorizeSkipsUnconfiguredToolsWithoutHandler(t *testing.T) {
	t.Parallel()
	if err := NewPolicy("danger").Authorize(t.Context(), datalon.Request{}, "interrupt",
		datalon.ToolApprovalAction{Name: "unknown"}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizeHonorsCancellationBeforeHandler(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	request := datalon.Request{ApprovalHandler: func(context.Context, datalon.ToolApprovalRequest) (datalon.ToolApprovalDecision, error) {
		called = true
		return datalon.ToolApprovalApprove, nil
	}}
	err := NewPolicy("danger").Authorize(ctx, request, "interrupt", datalon.ToolApprovalAction{Name: "danger"})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("Authorize() = %v, handler called = %v", err, called)
	}
}

func requestWithDecision(decision datalon.ToolApprovalDecision, err error) datalon.Request {
	return datalon.Request{ApprovalHandler: func(context.Context, datalon.ToolApprovalRequest) (datalon.ToolApprovalDecision, error) {
		return decision, err
	}}
}
