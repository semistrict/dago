package approval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datalon"
)

func TestResolveInterruptExpandsOneChannelDecision(t *testing.T) {
	t.Parallel()
	request := datalon.Request{
		ConversationID: "chat:room",
		ApprovalHandler: func(_ context.Context, approval datalon.ToolApprovalRequest) (datalon.ToolApprovalDecision, error) {
			if approval.InterruptID != "human_approval" || len(approval.Actions) != 2 || approval.Actions[1].Name != "mcp/delete" {
				t.Fatalf("approval = %+v", approval)
			}
			return datalon.ToolApprovalReject, nil
		},
	}
	interrupt := dagent.Interrupt{ID: "human_approval", Value: []dagent.ApprovalRequest{
		{Call: damessage.ToolCall{ID: "one", Name: "local/write", Arguments: json.RawMessage(`{"path":"one"}`)}},
		{Call: damessage.ToolCall{ID: "two", Name: "mcp/delete", Arguments: json.RawMessage(`{}`)}},
	}}
	response, err := ResolveInterrupt(t.Context(), request, interrupt)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		choice, ok := response.Decisions[id]
		if !ok || choice.Decision != dagent.ApprovalReject || choice.Reason == "" {
			t.Fatalf("decision %q = %+v, %v", id, choice, ok)
		}
	}
}

func TestResolveInterruptDecodesRestoredValue(t *testing.T) {
	t.Parallel()
	request := requestWithDecision(datalon.ToolApprovalApprove, nil)
	interrupt := dagent.Interrupt{ID: "human_approval", Value: []any{map[string]any{
		"call":              map[string]any{"id": "one", "name": "local/write", "arguments": map[string]any{"path": "one"}},
		"allowed_decisions": []any{"approve", "reject"},
	}}}
	response, err := ResolveInterrupt(t.Context(), request, interrupt)
	if err != nil {
		t.Fatal(err)
	}
	if response.Decisions["one"].Decision != dagent.ApprovalApprove {
		t.Fatalf("response = %+v", response)
	}
}

func TestResolveInterruptFailsClosedForAmbiguousOrUnsupportedInput(t *testing.T) {
	t.Parallel()
	call := damessage.ToolCall{ID: "same", Name: "tool", Arguments: json.RawMessage(`{}`)}
	tests := []struct {
		name      string
		request   datalon.Request
		interrupt dagent.Interrupt
		want      error
	}{
		{name: "no handler", interrupt: dagent.Interrupt{ID: "human_approval", Value: []dagent.ApprovalRequest{{Call: call}}}, want: ErrApprovalUnavailable},
		{name: "unknown interrupt", request: requestWithDecision(datalon.ToolApprovalApprove, nil), interrupt: dagent.Interrupt{ID: "ask_user", Value: []dagent.ApprovalRequest{{Call: call}}}, want: ErrInvalidInterrupt},
		{name: "duplicate ID", request: requestWithDecision(datalon.ToolApprovalApprove, nil), interrupt: dagent.Interrupt{ID: "human_approval", Value: []dagent.ApprovalRequest{{Call: call}, {Call: call}}}, want: ErrInvalidInterrupt},
		{name: "invalid arguments", request: requestWithDecision(datalon.ToolApprovalApprove, nil), interrupt: dagent.Interrupt{ID: "human_approval", Value: []dagent.ApprovalRequest{{Call: damessage.ToolCall{ID: "one", Name: "tool", Arguments: json.RawMessage(`{bad`)}}}}, want: ErrInvalidInterrupt},
		{name: "unsupported approve", request: requestWithDecision(datalon.ToolApprovalApprove, nil), interrupt: dagent.Interrupt{ID: "human_approval", Value: []dagent.ApprovalRequest{{Call: call, AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalReject}}}}, want: ErrToolRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveInterrupt(t.Context(), test.request, test.interrupt)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}
