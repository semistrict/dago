package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

type asyncRunnerStub struct {
	run           AsyncRun
	starts        int
	checks        int
	updates       int
	cancellations int
}

func (runner *asyncRunnerStub) Start(context.Context, string, string) (AsyncRun, error) {
	runner.starts++
	runner.run = AsyncRun{ThreadID: "task-1", RunID: "run-1", Status: "running"}
	return runner.run, nil
}

func (runner *asyncRunnerStub) Check(context.Context, string, string) (AsyncRun, error) {
	runner.checks++
	return runner.run, nil
}

func (runner *asyncRunnerStub) Update(context.Context, string, string, string) (AsyncRun, error) {
	runner.updates++
	runner.run.RunID = "run-2"
	runner.run.Status = "running"
	return runner.run, nil
}

func (runner *asyncRunnerStub) Cancel(context.Context, string, string) error {
	runner.cancellations++
	runner.run.Status = "cancelled"
	return nil
}

func TestAsyncSubagentTaskStatePersistsAcrossAgentTurns(t *testing.T) {
	runner := &asyncRunnerStub{}
	script := modeltest.New(model.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[0].Role != message.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "background guidance") || !strings.Contains(request.Messages[0].TextContent(), "researcher") {
				return errors.New("async subagent prompt missing")
			}
			return nil
		}, Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "start-1", Name: "start_async_task", Arguments: json.RawMessage(`{"description":"research","subagent_type":"researcher"}`)}}}}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("started")}},
		modeltest.Step{Response: model.Response{Message: message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "check-1", Name: "check_async_task", Arguments: json.RawMessage(`{"task_id":"task-1"}`)}}}}},
		modeltest.Step{Check: func(request model.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), `"result":"report"`) {
				return errors.New("async result missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("checked")}},
	)
	compiled, err := New(Options{
		Model: script, Saver: checkpoint.NewMemorySaver(), DisableSubagents: true, DisableSummary: true, FilesystemTools: []string{},
		AsyncSubagents:      []AsyncSubagent{{Name: "researcher", Description: "Researches topics", GraphID: "research", Runner: runner}},
		AsyncSubagentPrompt: "background guidance",
	})
	if err != nil {
		t.Fatal(err)
	}
	config := checkpoint.Config{ThreadID: "async-tasks"}
	first, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("start")}})
	if err != nil {
		t.Fatal(err)
	}
	if decodeAsyncTasks(first.State[AsyncTasksKey])["task-1"].RunID != "run-1" {
		t.Fatalf("tasks = %#v", first.State[AsyncTasksKey])
	}
	runner.run.Status, runner.run.Result = "success", "report"
	second, err := compiled.Invoke(context.Background(), agent.Input{Config: config, Messages: []message.Message{message.Human("status")}})
	if err != nil {
		t.Fatal(err)
	}
	if runner.starts != 1 || runner.checks != 1 || second.Messages[len(second.Messages)-1].TextContent() != "checked" {
		t.Fatalf("runner = %#v, messages = %#v", runner, second.Messages)
	}
}

func TestAsyncSubagentManagementTools(t *testing.T) {
	runner := &asyncRunnerStub{}
	clock := func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }
	middleware, err := AsyncSubagentMiddleware(AsyncSubagentOptions{
		Subagents: []AsyncSubagent{{Name: "worker", Description: "Works", GraphID: "worker-graph", Runner: runner}}, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]tool.Tool{}
	for _, value := range middleware.Tools {
		byName[value.Definition().Name] = value
	}
	tasks := map[string]any{}
	execute := func(name, arguments string) tool.Result {
		t.Helper()
		result, err := byName[name].Execute(context.Background(), json.RawMessage(arguments), tool.Runtime{CallID: name, State: state.Values{AsyncTasksKey: tasks}})
		if err != nil {
			t.Fatal(err)
		}
		if update, exists := result.Update[AsyncTasksKey]; exists {
			merged, err := reduceAsyncTasks(tasks, []any{update})
			if err != nil {
				t.Fatal(err)
			}
			tasks = merged.(map[string]any)
		}
		return result
	}
	if text := execute("start_async_task", `{"description":"work","subagent_type":"worker"}`).Content[0].Text; !strings.Contains(text, "task-1") {
		t.Fatal(text)
	}
	if text := execute("check_async_task", `{"task_id":"task-1"}`).Content[0].Text; !strings.Contains(text, `"status":"running"`) {
		t.Fatal(text)
	}
	if text := execute("update_async_task", `{"task_id":"task-1","message":"more"}`).Content[0].Text; !strings.Contains(text, "Updated") {
		t.Fatal(text)
	}
	if text := execute("list_async_tasks", `{}`).Content[0].Text; !strings.Contains(text, "run") || !strings.Contains(text, "task-1") {
		t.Fatal(text)
	}
	if text := execute("cancel_async_task", `{"task_id":"task-1"}`).Content[0].Text; !strings.Contains(text, "Cancelled") {
		t.Fatal(text)
	}
	if task := decodeAsyncTasks(tasks)["task-1"]; task.Status != "cancelled" || task.RunID != "run-2" {
		t.Fatalf("task = %#v", task)
	}
	if runner.starts != 1 || runner.updates != 1 || runner.cancellations != 1 {
		t.Fatalf("runner = %#v", runner)
	}
}
