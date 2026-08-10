package dago

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/tool"
)

const AsyncTasksKey = "async_tasks"

// AsyncRun is the provider-neutral status returned by a background-agent runner.
type AsyncRun struct {
	ThreadID string
	RunID    string
	Status   string
	Result   string
	// ResultValue preserves structured final message content when a runner can
	// return it. Result remains the convenient text representation.
	ResultValue any
	// HasResult distinguishes an empty final message from a completed thread
	// with no output messages.
	HasResult bool
	Error     string
}

// AsyncSubagentRunner adapts a hosted or local background-agent service.
type AsyncSubagentRunner interface {
	Start(context.Context, string, string) (AsyncRun, error)
	Check(context.Context, string, string) (AsyncRun, error)
	Update(context.Context, string, string, string) (AsyncRun, error)
	Cancel(context.Context, string, string) error
}

type AsyncSubagent struct {
	Name        string
	Description string
	GraphID     string
	URL         string
	APIKey      string
	Headers     map[string]string
	HTTPClient  *http.Client
	Runner      AsyncSubagentRunner
}

type AsyncTask struct {
	TaskID        string `json:"task_id"`
	AgentName     string `json:"agent_name"`
	ThreadID      string `json:"thread_id"`
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	LastCheckedAt string `json:"last_checked_at"`
	LastUpdatedAt string `json:"last_updated_at"`
}

type AsyncSubagentOptions struct {
	Subagents    []AsyncSubagent
	SystemPrompt string
	Now          func() time.Time
}

// AsyncSubagentMiddleware adds tools for starting and managing durable
// background-agent tasks.
func AsyncSubagentMiddleware(options AsyncSubagentOptions) (agent.Middleware, error) {
	if len(options.Subagents) == 0 {
		return agent.Middleware{}, fmt.Errorf("at least one async subagent is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	byName := make(map[string]AsyncSubagent, len(options.Subagents))
	for _, value := range options.Subagents {
		if strings.TrimSpace(value.Name) == "" || strings.TrimSpace(value.Description) == "" || strings.TrimSpace(value.GraphID) == "" {
			return agent.Middleware{}, fmt.Errorf("async subagent name, description, and graph id are required")
		}
		if value.Runner == nil {
			runner, err := NewAgentProtocolRunner(AgentProtocolOptions{URL: value.URL, APIKey: value.APIKey, Headers: value.Headers, HTTPClient: value.HTTPClient})
			if err != nil {
				return agent.Middleware{}, fmt.Errorf("async subagent %q: %w", value.Name, err)
			}
			value.Runner = runner
		}
		if _, exists := byName[value.Name]; exists {
			return agent.Middleware{}, fmt.Errorf("duplicate async subagent %q", value.Name)
		}
		byName[value.Name] = value
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, "- "+name+": "+byName[name].Description)
	}
	available := strings.Join(lines, "\n")
	tools := []tool.Tool{
		asyncStartTool(byName, names, available, options.Now),
		asyncCheckTool(byName, options.Now),
		asyncUpdateTool(byName, options.Now),
		asyncCancelTool(byName, options.Now),
		asyncListTool(byName, options.Now),
	}
	middleware := agent.Middleware{
		Name: "async_subagents", SerializedName: "AsyncSubAgentMiddleware",
		Fields: map[string]agent.StateField{AsyncTasksKey: {
			Kind: agent.FieldDelta, Contract: "dago.async_tasks.delta.v1", SnapshotFrequency: 100,
			Initial: func() any { return map[string]any{} }, Reduce: reduceAsyncTasks, Clone: cloneAsyncTasks,
		}},
		Tools: tools,
	}
	if options.SystemPrompt != "" {
		fragment := options.SystemPrompt + "\n\nAvailable async subagent types:\n\n" + available
		middleware.WrapModelCall = func(ctx context.Context, request agent.ModelRequest, next agent.ModelHandler) (agent.ModelResponse, error) {
			appendSystem(&request, fragment)
			return next(ctx, request)
		}
	}
	return middleware, nil
}

func asyncStartTool(byName map[string]AsyncSubagent, names []string, available string, now func() time.Time) tool.Tool {
	schema, _ := json.Marshal(map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"description":   map[string]any{"type": "string", "description": "A detailed description of the task for the async subagent to perform."},
			"subagent_type": map[string]any{"type": "string", "enum": names},
		},
		"required": []string{"description", "subagent_type"},
	})
	description := `Start an async subagent on a remote server. The subagent runs in the background and returns a task ID immediately.

Available async agent types:
` + available + `

## Usage notes:
1. This tool launches a background task and returns immediately with a task ID. Report the task ID to the user and stop — do NOT immediately check status.
2. Use check_async_task only when the user asks for a status update or result.
3. Use update_async_task to send new instructions to a running task.
4. Multiple async subagents can run concurrently — launch several and let them run in the background.
5. The subagent runs on a remote server, so it has its own tools and capabilities.`
	return tool.Func{Spec: tool.Definition{Name: "start_async_task", Description: description, InputSchema: schema}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			Description string `json:"description"`
			Type        string `json:"subagent_type"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		spec, exists := byName[input.Type]
		if !exists {
			quoted := make([]string, len(names))
			for index, name := range names {
				quoted[index] = "`" + name + "`"
			}
			return tool.TextResult("Unknown async subagent type `" + input.Type + "`. Available types: " + strings.Join(quoted, ", ")), nil
		}
		run, err := spec.Runner.Start(ctx, spec.GraphID, input.Description)
		if err != nil {
			return tool.TextResult("Failed to launch async subagent '" + input.Type + "': " + err.Error()), nil
		}
		if run.ThreadID == "" || run.RunID == "" {
			return tool.Result{}, fmt.Errorf("async subagent %q returned an empty thread or run id", input.Type)
		}
		stamp := asyncTimestamp(now())
		task := AsyncTask{TaskID: run.ThreadID, AgentName: input.Type, ThreadID: run.ThreadID, RunID: run.RunID, Status: "running", CreatedAt: stamp, LastCheckedAt: stamp, LastUpdatedAt: stamp}
		return asyncTaskResult("Launched async subagent. task_id: "+task.TaskID, task), nil
	}}
}

func asyncCheckTool(byName map[string]AsyncSubagent, now func() time.Time) tool.Tool {
	return tool.Func{Spec: tool.Definition{Name: "check_async_task", Description: "Check the status of an async subagent task. Returns the current status and, if complete, the result. Statuses shown earlier in the conversation are always stale, so call this to get the current status rather than reporting a status from a previous tool result.", InputSchema: asyncTaskIDSchema()}, Run: func(ctx context.Context, raw json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
		task, result, err := resolveAsyncTask(raw, runtime)
		if err != nil || result != nil {
			return resultOrError(result, err)
		}
		spec, exists := byName[task.AgentName]
		if !exists {
			return tool.TextResult("No async subagent configuration found for tracked agent: " + task.AgentName), nil
		}
		run, err := spec.Runner.Check(ctx, task.ThreadID, task.RunID)
		if err != nil {
			return tool.TextResult("Failed to get run status: " + err.Error()), nil
		}
		stamp := asyncTimestamp(now())
		if run.Status != "" && run.Status != task.Status {
			task.Status = run.Status
			task.LastUpdatedAt = stamp
		}
		task.LastCheckedAt = stamp
		payload := map[string]any{"status": task.Status, "thread_id": task.ThreadID}
		if task.Status == "success" {
			var resultValue any
			switch {
			case run.HasResult:
				resultValue = run.ResultValue
				if resultValue == nil && run.Result != "" {
					resultValue = run.Result
				}
			case run.ResultValue != nil:
				resultValue = run.ResultValue
			case run.Result != "":
				resultValue = run.Result
			default:
				run.Result = "(completed with no output messages)"
				resultValue = run.Result
			}
			payload["result"] = resultValue
		} else if task.Status == "error" {
			if run.Error == "" {
				run.Error = "The async subagent encountered an error."
			}
			payload["error"] = run.Error
		}
		encoded, _ := json.Marshal(payload)
		return asyncTaskResult(string(encoded), task), nil
	}}
}

func asyncUpdateTool(byName map[string]AsyncSubagent, now func() time.Time) tool.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"},"message":{"type":"string"}},"required":["task_id","message"],"additionalProperties":false}`)
	return tool.Func{Spec: tool.Definition{Name: "update_async_task", Description: "Send updated instructions to an async subagent. Interrupts the current run and starts a new one on the same thread, so the subagent sees the full conversation history plus your new message. The task_id remains the same.", InputSchema: schema}, Run: func(ctx context.Context, raw json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
		var input struct {
			TaskID  string `json:"task_id"`
			Message string `json:"message"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		task, result := asyncTaskFromRuntime(strings.TrimSpace(input.TaskID), runtime)
		if result != nil {
			return *result, nil
		}
		spec, exists := byName[task.AgentName]
		if !exists {
			return tool.TextResult("No async subagent configuration found for tracked agent: " + task.AgentName), nil
		}
		run, err := spec.Runner.Update(ctx, spec.GraphID, task.ThreadID, input.Message)
		if err != nil {
			return tool.TextResult("Failed to update async subagent: " + err.Error()), nil
		}
		if run.RunID == "" {
			return tool.Result{}, fmt.Errorf("async subagent %q returned an empty updated run id", task.AgentName)
		}
		task.RunID = run.RunID
		task.Status = "running"
		task.LastUpdatedAt = asyncTimestamp(now())
		return asyncTaskResult("Updated async subagent. task_id: "+task.TaskID, task), nil
	}}
}

func asyncCancelTool(byName map[string]AsyncSubagent, now func() time.Time) tool.Tool {
	return tool.Func{Spec: tool.Definition{Name: "cancel_async_task", Description: "Cancel a running async subagent task. Use this to stop a task that is no longer needed.", InputSchema: asyncTaskIDSchema()}, Run: func(ctx context.Context, raw json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
		task, result, err := resolveAsyncTask(raw, runtime)
		if err != nil || result != nil {
			return resultOrError(result, err)
		}
		spec, exists := byName[task.AgentName]
		if !exists {
			return tool.TextResult("No async subagent configuration found for tracked agent: " + task.AgentName), nil
		}
		if err := spec.Runner.Cancel(ctx, task.ThreadID, task.RunID); err != nil {
			return tool.TextResult("Failed to cancel run: " + err.Error()), nil
		}
		stamp := asyncTimestamp(now())
		task.Status, task.LastCheckedAt, task.LastUpdatedAt = "cancelled", stamp, stamp
		return asyncTaskResult("Cancelled async subagent task: "+task.TaskID, task), nil
	}}
}

func asyncListTool(byName map[string]AsyncSubagent, now func() time.Time) tool.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{"status_filter":{"type":"string","enum":["running","success","error","cancelled","all"]}},"additionalProperties":false}`)
	return tool.Func{Spec: tool.Definition{Name: "list_async_tasks", Description: "List tracked async subagent tasks with their current live statuses. By default shows all tasks. Use status_filter to narrow by status. Use check_async_task to get the full result of a specific completed task. Statuses shown earlier in the conversation are always stale, so call this to read current statuses rather than reporting one from a previous tool result.", InputSchema: schema}, Run: func(ctx context.Context, raw json.RawMessage, runtime tool.Runtime) (tool.Result, error) {
		var input struct {
			Status string `json:"status_filter"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		if input.Status != "" && input.Status != "all" && input.Status != "running" && input.Status != "success" && input.Status != "error" && input.Status != "cancelled" {
			return tool.Result{}, fmt.Errorf("invalid async task status filter %q", input.Status)
		}
		tasks := asyncTasksFromRuntime(runtime)
		ids := make([]string, 0, len(tasks))
		for id, task := range tasks {
			if input.Status == "" || input.Status == "all" || task.Status == input.Status {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			return tool.TextResult("No async subagent tasks tracked."), nil
		}
		stamp := asyncTimestamp(now())
		updates := map[string]any{}
		lines := make([]string, 0, len(ids))
		for _, id := range ids {
			task := tasks[id]
			if spec, exists := byName[task.AgentName]; exists && !asyncTerminalStatus(task.Status) {
				if run, err := spec.Runner.Check(ctx, task.ThreadID, task.RunID); err == nil {
					if run.Status != "" && run.Status != task.Status {
						task.Status, task.LastUpdatedAt = run.Status, stamp
					}
				}
			}
			task.LastCheckedAt = stamp
			updates[id] = asyncTaskMap(task)
			lines = append(lines, fmt.Sprintf("- task_id: %s  agent: %s  status: %s", task.TaskID, task.AgentName, task.Status))
		}
		return tool.Result{Content: tool.TextResult(fmt.Sprintf("%d tracked task(s):\n%s", len(lines), strings.Join(lines, "\n"))).Content, Update: map[string]any{AsyncTasksKey: updates}}, nil
	}}
}

func asyncTaskIDSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`)
}

func resolveAsyncTask(raw json.RawMessage, runtime tool.Runtime) (AsyncTask, *tool.Result, error) {
	var input struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeArguments(raw, &input); err != nil {
		return AsyncTask{}, nil, err
	}
	task, result := asyncTaskFromRuntime(strings.TrimSpace(input.TaskID), runtime)
	return task, result, nil
}

func asyncTaskFromRuntime(taskID string, runtime tool.Runtime) (AsyncTask, *tool.Result) {
	task, exists := asyncTasksFromRuntime(runtime)[taskID]
	if !exists {
		result := tool.TextResult(fmt.Sprintf("No tracked task found for task_id: %q", taskID))
		return AsyncTask{}, &result
	}
	return task, nil
}

func asyncTasksFromRuntime(runtime tool.Runtime) map[string]AsyncTask {
	if runtime.State == nil {
		return map[string]AsyncTask{}
	}
	value, _ := runtime.State.Get(AsyncTasksKey)
	return decodeAsyncTasks(value)
}

func asyncTaskResult(text string, task AsyncTask) tool.Result {
	return tool.Result{Content: tool.TextResult(text).Content, Update: map[string]any{AsyncTasksKey: map[string]any{task.TaskID: asyncTaskMap(task)}}}
}

func resultOrError(result *tool.Result, err error) (tool.Result, error) {
	if err != nil {
		return tool.Result{}, err
	}
	return *result, nil
}

func asyncTaskMap(task AsyncTask) map[string]any {
	return map[string]any{
		"task_id": task.TaskID, "agent_name": task.AgentName, "thread_id": task.ThreadID, "run_id": task.RunID,
		"status": task.Status, "created_at": task.CreatedAt, "last_checked_at": task.LastCheckedAt, "last_updated_at": task.LastUpdatedAt,
	}
}

func decodeAsyncTasks(value any) map[string]AsyncTask {
	result := map[string]AsyncTask{}
	values, _ := value.(map[string]any)
	for id, raw := range values {
		entry, _ := raw.(map[string]any)
		result[id] = AsyncTask{
			TaskID: stringValue(entry["task_id"]), AgentName: stringValue(entry["agent_name"]), ThreadID: stringValue(entry["thread_id"]), RunID: stringValue(entry["run_id"]),
			Status: stringValue(entry["status"]), CreatedAt: stringValue(entry["created_at"]), LastCheckedAt: stringValue(entry["last_checked_at"]), LastUpdatedAt: stringValue(entry["last_updated_at"]),
		}
	}
	return result
}

func reduceAsyncTasks(current any, updates []any) (any, error) {
	result, _ := cloneAsyncTasks(current).(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	for _, update := range updates {
		values, ok := update.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("async task update has type %T", update)
		}
		for id, value := range values {
			entry, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("async task %q has type %T", id, value)
			}
			result[id] = cloneAnyMap(entry)
		}
	}
	return result, nil
}

func cloneAsyncTasks(value any) any {
	values, ok := value.(map[string]any)
	if !ok {
		return value
	}
	result := make(map[string]any, len(values))
	for id, raw := range values {
		if entry, ok := raw.(map[string]any); ok {
			result[id] = cloneAnyMap(entry)
		} else {
			result[id] = raw
		}
	}
	return result
}

func cloneAnyMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func asyncTimestamp(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05Z") }

func asyncTerminalStatus(value string) bool {
	switch value {
	case "cancelled", "success", "error", "timeout", "interrupted":
		return true
	default:
		return false
	}
}
