package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

// ToolRetry returns middleware that retries whole tool executions. Attempts includes
// the initial call. Context cancellation and deadline errors are never retried.
func ToolRetry(name string, attempts int, backoff time.Duration, retryable func(error) bool) Middleware {
	if name == "" {
		name = "tool_retry"
	}
	if attempts <= 0 {
		attempts = 1
	}
	return Middleware{Name: name, WrapToolCall: func(ctx context.Context, request ToolCallRequest, next ToolHandler) (ToolCallResponse, error) {
		var last error
		for attempt := 0; attempt < attempts; attempt++ {
			response, err := next(ctx, request)
			if err == nil {
				return response, nil
			}
			last = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				(retryable != nil && !retryable(err)) || attempt+1 >= attempts {
				break
			}
			if backoff > 0 {
				timer := time.NewTimer(backoff)
				select {
				case <-timer.C:
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return ToolCallResponse{}, ctx.Err()
				}
			}
		}
		return ToolCallResponse{}, last
	}}
}

// Todo is one model-managed planning item.
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// TodoList adds the write_todos tool and a checkpointed todos state field.
func TodoList() Middleware {
	schema := json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["content","status"],"additionalProperties":false}}},"required":["todos"],"additionalProperties":false}`)
	write := tool.Func{
		Spec: tool.Definition{Name: "write_todos", Description: "Create or replace the task list for the current run.", InputSchema: schema},
		Run: func(_ context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
			var arguments struct {
				Todos []Todo `json:"todos"`
			}
			if err := json.Unmarshal(raw, &arguments); err != nil {
				return tool.Result{}, fmt.Errorf("%w: %v", tool.ErrInvalidArguments, err)
			}
			for index, todo := range arguments.Todos {
				if todo.Content == "" || (todo.Status != "pending" && todo.Status != "in_progress" && todo.Status != "completed") {
					return tool.Result{}, fmt.Errorf("%w: invalid todo %d", tool.ErrInvalidArguments, index)
				}
			}
			return tool.Result{
				Content: []message.ContentBlock{{Type: message.BlockText, Text: "Updated todo list."}},
				Update:  map[string]any{"todos": append([]Todo(nil), arguments.Todos...)},
			}, nil
		},
	}
	return Middleware{
		Name: "todo_list", Tools: []tool.Tool{write},
		Fields: map[string]StateField{"todos": {Kind: FieldLast, Contract: "dago.todos.v1", Clone: func(value any) any {
			if todos, ok := value.([]Todo); ok {
				return append([]Todo(nil), todos...)
			}
			return value
		}}},
	}
}

// ApprovalRequest describes one tool call awaiting a human decision.
type ApprovalRequest struct {
	Call        message.ToolCall `json:"call"`
	Description string           `json:"description,omitempty"`
}

type ApprovalDecision string

const (
	ApprovalApprove ApprovalDecision = "approve"
	ApprovalEdit    ApprovalDecision = "edit"
	ApprovalReject  ApprovalDecision = "reject"
)

// ApprovalResponse is supplied as Input.Resume. Every gated call in the interrupt
// must have exactly one decision.
type ApprovalResponse struct {
	Decisions map[string]ApprovalChoice `json:"decisions"`
}

type ApprovalChoice struct {
	Decision ApprovalDecision  `json:"decision"`
	Call     *message.ToolCall `json:"call,omitempty"`
	Reason   string            `json:"reason,omitempty"`
}

// ApprovalRule gates tool names using path.Match syntax. Rules are evaluated in
// order and the first match wins.
type ApprovalRule struct {
	Pattern     string
	Description string
}

// HumanApproval pauses before any matching tool executes and supports approve,
// edit, and reject decisions, including several pending calls in one interrupt.
func HumanApproval(rules []ApprovalRule) Middleware {
	return Middleware{Name: "human_approval", BeforeTools: func(_ context.Context, request ToolBatchRequest) (ToolBatchResponse, error) {
		var pending []ApprovalRequest
		gated := map[string]bool{}
		for _, call := range request.Calls {
			for _, rule := range rules {
				matched, err := path.Match(rule.Pattern, call.Name)
				if err != nil {
					return ToolBatchResponse{}, fmt.Errorf("approval pattern %q: %w", rule.Pattern, err)
				}
				if matched {
					pending = append(pending, ApprovalRequest{Call: call, Description: rule.Description})
					gated[call.ID] = true
					break
				}
			}
		}
		if len(pending) == 0 {
			return ToolBatchResponse{}, nil
		}
		if request.Runtime.Resume == nil {
			value := make([]any, 0, len(pending))
			for _, item := range pending {
				var arguments any
				if err := json.Unmarshal(item.Call.Arguments, &arguments); err != nil {
					return ToolBatchResponse{}, fmt.Errorf("encode human approval call %q: %w", item.Call.ID, err)
				}
				call := map[string]any{"id": item.Call.ID, "name": item.Call.Name, "arguments": arguments}
				record := map[string]any{"call": call}
				if item.Description != "" {
					record["description"] = item.Description
				}
				value = append(value, record)
			}
			return ToolBatchResponse{Interrupt: &Interrupt{ID: "human_approval", Value: value}}, nil
		}
		resume, ok := request.Runtime.Resume.(ApprovalResponse)
		if !ok {
			return ToolBatchResponse{}, fmt.Errorf("human approval resume has type %T", request.Runtime.Resume)
		}
		calls := make([]message.ToolCall, 0, len(request.Calls))
		var rejected []message.Message
		for _, call := range request.Calls {
			if !gated[call.ID] {
				calls = append(calls, call)
				continue
			}
			choice, exists := resume.Decisions[call.ID]
			if !exists {
				return ToolBatchResponse{}, fmt.Errorf("human approval decision missing for call %q", call.ID)
			}
			switch choice.Decision {
			case ApprovalApprove:
				calls = append(calls, call)
			case ApprovalEdit:
				if choice.Call == nil || choice.Call.ID != call.ID || choice.Call.Name == "" || !json.Valid(choice.Call.Arguments) {
					return ToolBatchResponse{}, fmt.Errorf("human approval edit for call %q is invalid", call.ID)
				}
				calls = append(calls, *choice.Call)
			case ApprovalReject:
				text := choice.Reason
				if text == "" {
					text = "Tool call rejected by reviewer."
				}
				result := message.Tool(call.ID, text)
				result.Name = call.Name
				result.ToolStatus = message.ToolStatusError
				rejected = append(rejected, result)
			default:
				return ToolBatchResponse{}, fmt.Errorf("human approval decision %q is invalid", choice.Decision)
			}
		}
		return ToolBatchResponse{Calls: calls, Messages: rejected, ResumeConsumed: true}, nil
	}}
}

// JumpUpdate is reserved for middleware that records an explicit loop decision in
// state without exposing internal graph types.
func JumpUpdate(destination string) state.Values {
	return state.Values{"jump_to": destination}
}

// PromptCaching adds a cache hint only when the selected model advertises prompt
// caching. The adapter remains responsible for translating it to provider fields.
func PromptCaching(name, retention string, key func(ModelRequest) string) Middleware {
	if name == "" {
		name = "prompt_caching"
	}
	return Middleware{Name: name, WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
		if request.Model.Profile().SupportsPromptCaching {
			cacheKey := ""
			if key != nil {
				cacheKey = key(request.Clone())
			}
			request.PromptCache = &model.PromptCache{Key: cacheKey, Retention: retention}
		}
		return next(ctx, request)
	}}
}
