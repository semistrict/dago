package dagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

// ToolRetryOptions configures whole-tool execution retries.
type ToolRetryOptions struct {
	Name      string
	Attempts  int
	Backoff   time.Duration
	Retryable func(error) bool
}

// ToolRetry returns middleware that retries whole tool executions. Attempts includes
// the initial call. Context cancellation and deadline errors are never retried.
func ToolRetry(options ToolRetryOptions) Middleware {
	if options.Attempts < 0 || options.Backoff < 0 {
		panic("tool retry attempts and backoff cannot be negative")
	}
	if options.Name == "" {
		options.Name = "tool_retry"
	}
	if options.Attempts == 0 {
		options.Attempts = 1
	}
	return Middleware{Name: options.Name, WrapToolCall: func(ctx context.Context, request ToolCallRequest, next ToolHandler) (ToolCallResponse, error) {
		var last error
		for attempt := 0; attempt < options.Attempts; attempt++ {
			response, err := next(ctx, request)
			if err == nil {
				return response, nil
			}
			last = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				(options.Retryable != nil && !options.Retryable(err)) || attempt+1 >= options.Attempts {
				break
			}
			if options.Backoff > 0 {
				timer := time.NewTimer(options.Backoff)
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
	Status  string `json:"status" jsonschema:"enum=pending|in_progress|completed"`
}

type todoConfig struct {
	systemPrompt    string
	toolDescription string
}

// TodoOption configures the todo-list middleware.
type TodoOption interface{ applyTodo(*todoConfig) }

type todoOptionFunc func(*todoConfig)

func (option todoOptionFunc) applyTodo(config *todoConfig) { option(config) }

// WithTodoPrompt replaces the todo-list model instructions.
func WithTodoPrompt(prompt string) TodoOption {
	return todoOptionFunc(func(config *todoConfig) { config.systemPrompt = prompt })
}

// WithTodoDescription replaces the write_todos tool description.
func WithTodoDescription(description string) TodoOption {
	return todoOptionFunc(func(config *todoConfig) { config.toolDescription = description })
}

const todoSystemPrompt = `## write_todos

Use write_todos to create and manage a structured task list for complex objectives. Skip it for simple work that takes only a few steps.

- Mark work in_progress before beginning it and completed immediately after it is actually finished.
- Revise the list as new information appears; remove irrelevant items.
- Unless all work is complete, keep at least one applicable item in_progress.
- Never call write_todos multiple times in parallel.
- The final answer must be a later assistant message, not the write_todos tool call itself.`

const todoToolDescription = `Create or replace the structured task list for the current work session. Use this for non-trivial work with at least three distinct steps, when the user asks for a plan, or when tracking will materially help. Keep statuses current as work proceeds. Do not call this tool multiple times in one model response.`

const parallelTodoError = "Error: The `write_todos` tool should never be called multiple times in parallel. Please call it only once per model invocation to update the todo list."

// TodoList adds the write_todos tool and a checkpointed todos state field.
func TodoList(options ...TodoOption) Middleware {
	config := todoConfig{systemPrompt: todoSystemPrompt, toolDescription: todoToolDescription}
	for index, option := range options {
		if option == nil {
			panic(fmt.Sprintf("todo option %d is nil", index))
		}
		option.applyTodo(&config)
	}
	type input struct {
		Todos []Todo `json:"todos"`
	}
	write := datool.MustNew(
		"write_todos", config.toolDescription,
		func(_ context.Context, arguments input) (datool.Result, error) {
			for index, todo := range arguments.Todos {
				if todo.Content == "" || (todo.Status != "pending" && todo.Status != "in_progress" && todo.Status != "completed") {
					return datool.Result{}, fmt.Errorf("%w: invalid todo %d", datool.ErrInvalidArguments, index)
				}
			}
			return datool.Result{
				Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "Updated todo list."}},
				Update:  map[string]any{"todos": todosToState(arguments.Todos)},
			}, nil
		},
	)
	return Middleware{
		Name: "todo_list", SerializedName: "TodoListMiddleware", Tools: []datool.Tool{write},
		Fields: map[string]StateField{"todos": Field(FieldSpec[todoState]{
			Kind: FieldLast, Contract: "dago.todos.v1", Clone: cloneTodoRecords,
		})},
		WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
			if config.systemPrompt != "" {
				appendModelSystem(&request, config.systemPrompt)
			}
			return next(ctx, request)
		},
		AfterModel: func(_ context.Context, values dastate.Values, _ Runtime) (dastate.Values, error) {
			messages, err := messagesView(values[MessagesKey])
			if err != nil {
				return nil, err
			}
			var latest *damessage.Message
			for index := len(messages) - 1; index >= 0; index-- {
				if messages[index].Role == damessage.RoleAssistant {
					latest = &messages[index]
					break
				}
			}
			if latest == nil {
				return nil, nil
			}
			var calls []damessage.ToolCall
			for _, call := range latest.ToolCalls {
				if call.Name == "write_todos" {
					calls = append(calls, call)
				}
			}
			if len(calls) <= 1 {
				return nil, nil
			}
			errors := make([]damessage.Message, len(calls))
			for index, call := range calls {
				errors[index] = damessage.Tool(call.ID, parallelTodoError)
				errors[index].Name = call.Name
				errors[index].ToolStatus = damessage.ToolStatusError
			}
			return dastate.Values{MessagesKey: errors, structuredRetryKey: true}, nil
		},
	}
}

type todoState []map[string]any

func todosToState(values []Todo) []map[string]any {
	result := make([]map[string]any, len(values))
	for index, item := range values {
		result[index] = map[string]any{"content": item.Content, "status": item.Status}
	}
	return result
}

func todosFromState(value any) []Todo {
	if values, ok := value.([]Todo); ok {
		return values
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Slice {
		return nil
	}
	result := make([]Todo, 0, reflected.Len())
	for index := 0; index < reflected.Len(); index++ {
		record, ok := reflected.Index(index).Interface().(map[string]any)
		if !ok {
			continue
		}
		content, _ := record["content"].(string)
		status, _ := record["status"].(string)
		result = append(result, Todo{Content: content, Status: status})
	}
	return result
}

func cloneTodoRecords(value todoState) todoState {
	return todoState(todosToState(todosFromState(value)))
}

func appendModelSystem(request *ModelRequest, text string) {
	copy := request.SystemMessage.Clone()
	copy.Content = append(copy.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + text})
	request.SystemMessage = &copy
}

// ApprovalRequest describes one tool call awaiting a human decision.
type ApprovalRequest struct {
	Call             damessage.ToolCall `json:"call"`
	Description      string             `json:"description,omitempty"`
	AllowedDecisions []ApprovalDecision `json:"allowed_decisions,omitempty"`
}

type ApprovalDecision string

const (
	ApprovalApprove ApprovalDecision = "approve"
	ApprovalEdit    ApprovalDecision = "edit"
	ApprovalReject  ApprovalDecision = "reject"
	ApprovalRespond ApprovalDecision = "respond"
)

// ApprovalResponse is supplied as Input.Resume. Every gated call in the interrupt
// must have exactly one decision.
type ApprovalResponse struct {
	Decisions map[string]ApprovalChoice `json:"decisions"`
}

type ApprovalChoice struct {
	Decision ApprovalDecision    `json:"decision"`
	Call     *damessage.ToolCall `json:"call,omitempty"`
	Reason   string              `json:"reason,omitempty"`
	Message  string              `json:"message,omitempty"`
}

// ApprovalRule gates tool names using path.Match syntax. Rules are evaluated in
// order and the first match wins.
type ApprovalRule struct {
	Pattern          string
	Description      string
	AllowedDecisions []ApprovalDecision
	When             func(ToolCallRequest) bool
}

func (rule ApprovalRule) MatchesName(name string) (bool, error) {
	return path.Match(rule.Pattern, name)
}

func (rule ApprovalRule) Applies(request ToolCallRequest) (bool, error) {
	matched, err := rule.MatchesName(request.Call.Name)
	if err != nil || !matched {
		return matched, err
	}
	return rule.When == nil || rule.When(request), nil
}

func (rule ApprovalRule) decisions() []ApprovalDecision {
	if len(rule.AllowedDecisions) == 0 {
		return []ApprovalDecision{ApprovalApprove, ApprovalEdit, ApprovalReject, ApprovalRespond}
	}
	return append([]ApprovalDecision(nil), rule.AllowedDecisions...)
}

func (rule ApprovalRule) allows(decision ApprovalDecision) bool {
	for _, allowed := range rule.decisions() {
		if allowed == decision {
			return true
		}
	}
	return false
}

func ValidateApprovalRules(rules []ApprovalRule) error {
	for index, rule := range rules {
		if rule.Pattern == "" {
			return fmt.Errorf("approval rule %d requires a tool pattern", index)
		}
		if _, err := path.Match(rule.Pattern, "probe"); err != nil {
			return fmt.Errorf("approval rule %d pattern %q: %w", index, rule.Pattern, err)
		}
		seen := map[ApprovalDecision]bool{}
		for _, decision := range rule.AllowedDecisions {
			if decision != ApprovalApprove && decision != ApprovalEdit && decision != ApprovalReject && decision != ApprovalRespond {
				return fmt.Errorf("approval rule %d has invalid decision %q", index, decision)
			}
			if seen[decision] {
				return fmt.Errorf("approval rule %d repeats decision %q", index, decision)
			}
			seen[decision] = true
		}
	}
	return nil
}

// HumanApproval pauses before any matching tool executes and supports approve,
// edit, reject, and respond decisions, including several pending calls in one interrupt.
func HumanApproval(rules []ApprovalRule) Middleware {
	if err := ValidateApprovalRules(rules); err != nil {
		panic(err)
	}
	rules = append([]ApprovalRule(nil), rules...)
	return Middleware{Name: "human_approval", SerializedName: "HumanInTheLoopMiddleware", BeforeTools: func(_ context.Context, request ToolBatchRequest) (ToolBatchResponse, error) {
		var pending []ApprovalRequest
		gated := map[string]bool{}
		matchedRules := map[string]ApprovalRule{}
		for _, call := range request.Calls {
			for _, rule := range rules {
				matched, err := rule.Applies(ToolCallRequest{Call: call, Tool: request.Tools[call.Name], State: request.State, Runtime: request.Runtime})
				if err != nil {
					return ToolBatchResponse{}, fmt.Errorf("approval pattern %q: %w", rule.Pattern, err)
				}
				if matched {
					pending = append(pending, ApprovalRequest{Call: call, Description: rule.Description, AllowedDecisions: rule.decisions()})
					gated[call.ID] = true
					matchedRules[call.ID] = rule
				}
				if nameMatched, _ := rule.MatchesName(call.Name); nameMatched {
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
				record["allowed_decisions"] = item.AllowedDecisions
				value = append(value, record)
			}
			return ToolBatchResponse{Interrupt: &Interrupt{ID: "human_approval", Value: value}}, nil
		}
		resume, ok := ResumeAs[ApprovalResponse](request.Runtime)
		if !ok {
			return ToolBatchResponse{}, fmt.Errorf("human approval resume cannot decode from type %T", request.Runtime.Resume)
		}
		calls := make([]damessage.ToolCall, 0, len(request.Calls))
		var rejected []damessage.Message
		for _, call := range request.Calls {
			if !gated[call.ID] {
				calls = append(calls, call)
				continue
			}
			choice, exists := resume.Decisions[call.ID]
			if !exists {
				return ToolBatchResponse{}, fmt.Errorf("human approval decision missing for call %q", call.ID)
			}
			rule := matchedRules[call.ID]
			if !rule.allows(choice.Decision) {
				return ToolBatchResponse{}, fmt.Errorf("decision %q is not allowed for tool %q", choice.Decision, call.Name)
			}
			switch choice.Decision {
			case ApprovalApprove:
				calls = append(calls, call)
			case ApprovalEdit:
				if choice.Call == nil || choice.Call.Name == "" || !json.Valid(choice.Call.Arguments) || (choice.Call.ID != "" && choice.Call.ID != call.ID) {
					return ToolBatchResponse{}, fmt.Errorf("human approval edit for call %q is invalid", call.ID)
				}
				edited := *choice.Call
				edited.ID = call.ID
				calls = append(calls, edited)
			case ApprovalReject:
				text := choice.Message
				if text == "" {
					text = choice.Reason
				}
				if text == "" {
					text = fmt.Sprintf("User rejected the tool call for `%s` with id %s. The tool was not executed. Do not retry this tool call unless the user explicitly requests it.", call.Name, call.ID)
				}
				result := damessage.Tool(call.ID, text)
				result.Name = call.Name
				result.ToolStatus = damessage.ToolStatusError
				rejected = append(rejected, result)
			case ApprovalRespond:
				if choice.Message == "" {
					return ToolBatchResponse{}, fmt.Errorf("human response for call %q requires a message", call.ID)
				}
				result := damessage.Tool(call.ID, choice.Message)
				result.Name = call.Name
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
func JumpUpdate(destination string) dastate.Values {
	return dastate.Values{jumpToKey: destination}
}

// PromptCaching adds a cache hint only when the selected model advertises prompt
// caching. Anthropic requests also receive the provider's explicit cache
// breakpoints on the final system content block and final tool definition.
func PromptCaching(retention string, key func(ModelRequest) string) Middleware {
	return Middleware{Name: "prompt_caching", WrapModelCall: func(ctx context.Context, request ModelRequest, next ModelHandler) (ModelResponse, error) {
		if request.Model.Profile().SupportsPromptCaching {
			cacheKey := ""
			if key != nil {
				cacheKey = key(request.Clone())
			}
			request.PromptCache = &damodel.PromptCache{Key: cacheKey, Retention: retention}
			if strings.EqualFold(request.Model.Profile().Provider, "anthropic") {
				request = addAnthropicCacheBreakpoints(request, retention)
			}
		}
		return next(ctx, request)
	}}
}

type definitionOverrideTool struct {
	datool.Tool
	definition datool.Definition
}

func (wrapped definitionOverrideTool) Definition() datool.Definition {
	definition := wrapped.definition
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	definition.Extra = cloneRawMap(definition.Extra)
	return definition
}

func addAnthropicCacheBreakpoints(request ModelRequest, retention string) ModelRequest {
	cacheControl := map[string]string{"type": "ephemeral", "ttl": "5m"}
	if retention == "1h" {
		cacheControl["ttl"] = "1h"
	}
	raw, _ := json.Marshal(cacheControl)
	if len(request.SystemMessage.Content) > 0 {
		system := request.SystemMessage.Clone()
		last := len(system.Content) - 1
		if system.Content[last].Extra == nil {
			system.Content[last].Extra = map[string]json.RawMessage{}
		}
		system.Content[last].Extra["cache_control"] = append(json.RawMessage(nil), raw...)
		request.SystemMessage = &system
	}
	if len(request.Tools) > 0 {
		request.Tools = append([]datool.Tool(nil), request.Tools...)
		last := len(request.Tools) - 1
		definition := request.Tools[last].Definition()
		if definition.Extra == nil {
			definition.Extra = map[string]json.RawMessage{}
		}
		definition.Extra["cache_control"] = append(json.RawMessage(nil), raw...)
		request.Tools[last] = definitionOverrideTool{Tool: request.Tools[last], definition: definition}
	}
	return request
}
