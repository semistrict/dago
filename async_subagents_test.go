package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

type asyncRunnerStub struct {
	run           AsyncRun
	starts        int
	checks        int
	updates       int
	cancellations int
}

func (runner *asyncRunnerStub) Start(context.Context, AsyncStartRequest) (AsyncRun, error) {
	runner.starts++
	runner.run = AsyncRun{ThreadID: "task-1", RunID: "run-1", Status: "running"}
	return runner.run, nil
}

func (runner *asyncRunnerStub) Check(context.Context, AsyncCheckRequest) (AsyncRun, error) {
	runner.checks++
	return runner.run, nil
}

func (runner *asyncRunnerStub) Update(context.Context, AsyncUpdateRequest) (AsyncRun, error) {
	runner.updates++
	runner.run.RunID = "run-2"
	runner.run.Status = "running"
	return runner.run, nil
}

func (runner *asyncRunnerStub) Cancel(context.Context, AsyncCancelRequest) error {
	runner.cancellations++
	runner.run.Status = "cancelled"
	return nil
}

func TestAsyncSubagentTaskStatePersistsAcrossAgentTurns(t *testing.T) {
	runner := &asyncRunnerStub{}
	script := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[0].Role != damessage.RoleSystem || !strings.Contains(request.Messages[0].TextContent(), "background guidance") || !strings.Contains(request.Messages[0].TextContent(), "researcher") {
				return errors.New("async subagent prompt missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "start-1", Name: "start_async_task", Arguments: json.RawMessage(`{"description":"research","subagent_type":"researcher"}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("started")}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "check-1", Name: "check_async_task", Arguments: json.RawMessage(`{"task_id":"task-1"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), `"result":"report"`) {
				return errors.New("async result missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("checked")}},
	)
	compiled := New(
		script, Options{
			Saver: dacheckpoint.NewMemorySaver(), DisableSubagents: true, DisableSummary: true, Filesystem: Filesystem{Tools: []string{}},
			AsyncSubagents:      []AsyncSubagent{{Name: "researcher", Description: "Researches topics", GraphID: "research", Runner: runner}},
			AsyncSubagentPrompt: "background guidance",
		})

	config := dacheckpoint.Config{ThreadID: "async-tasks"}
	first, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("start")}})
	if err != nil {
		t.Fatal(err)
	}
	if decodeAsyncTasks(first.State[AsyncTasksKey])["task-1"].RunID != "run-1" {
		t.Fatalf("tasks = %#v", first.State[AsyncTasksKey])
	}
	runner.run.Status, runner.run.Outcome = "success", AsyncSuccess{Value: "report"}
	second, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("status")}})
	if err != nil {
		t.Fatal(err)
	}
	if runner.starts != 1 || runner.checks != 1 || second.Messages[len(second.Messages)-1].TextContent() != "checked" {
		t.Fatalf("runner = %#v, messages = %#v", runner, second.Messages)
	}
}

func TestAsyncSubagentManagementTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &asyncRunnerStub{}
		middleware := AsyncSubagentsWithOptions(
			AsyncSubagentsOptions{},
			AsyncSubagent{Name: "worker", Description: "Works", GraphID: "worker-graph", Runner: runner},
		)
		byName := map[string]datool.Tool{}
		for _, value := range middleware.Tools {
			byName[value.Definition().Name] = value
		}
		tasks := map[string]any{}
		execute := func(name, arguments string) datool.Result {
			t.Helper()
			result, err := byName[name].Execute(t.Context(), json.RawMessage(arguments), datool.Runtime{CallID: name, State: dastate.Values{AsyncTasksKey: tasks}})
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
		if task := decodeAsyncTasks(tasks)["task-1"]; task.CreatedAt != "2000-01-01T00:00:00Z" || task.LastCheckedAt != task.CreatedAt || task.LastUpdatedAt != task.CreatedAt {
			t.Fatalf("started task = %#v", task)
		}
		time.Sleep(2 * time.Second)
		if text := execute("check_async_task", `{"task_id":"task-1"}`).Content[0].Text; !strings.Contains(text, `"status":"running"`) {
			t.Fatal(text)
		}
		if task := decodeAsyncTasks(tasks)["task-1"]; task.LastCheckedAt != "2000-01-01T00:00:02Z" || task.LastUpdatedAt != "2000-01-01T00:00:00Z" {
			t.Fatalf("checked task = %#v", task)
		}
		time.Sleep(3 * time.Second)
		if text := execute("update_async_task", `{"task_id":"task-1","message":"more"}`).Content[0].Text; !strings.Contains(text, "Updated") {
			t.Fatal(text)
		}
		if task := decodeAsyncTasks(tasks)["task-1"]; task.LastCheckedAt != "2000-01-01T00:00:02Z" || task.LastUpdatedAt != "2000-01-01T00:00:05Z" {
			t.Fatalf("updated task = %#v", task)
		}
		time.Sleep(4 * time.Second)
		if text := execute("list_async_tasks", `{}`).Content[0].Text; !strings.Contains(text, "run") || !strings.Contains(text, "task-1") {
			t.Fatal(text)
		}
		if task := decodeAsyncTasks(tasks)["task-1"]; task.LastCheckedAt != "2000-01-01T00:00:09Z" || task.LastUpdatedAt != "2000-01-01T00:00:05Z" {
			t.Fatalf("listed task = %#v", task)
		}
		time.Sleep(time.Second)
		if text := execute("cancel_async_task", `{"task_id":"task-1"}`).Content[0].Text; !strings.Contains(text, "Cancelled") {
			t.Fatal(text)
		}
		task := decodeAsyncTasks(tasks)["task-1"]
		if task.Status != "cancelled" || task.RunID != "run-2" ||
			task.CreatedAt != "2000-01-01T00:00:00Z" ||
			task.LastCheckedAt != "2000-01-01T00:00:10Z" ||
			task.LastUpdatedAt != "2000-01-01T00:00:10Z" {
			t.Fatalf("task = %#v", task)
		}
		if runner.starts != 1 || runner.updates != 1 || runner.cancellations != 1 {
			t.Fatalf("runner = %#v", runner)
		}
	})
}

func TestAsyncCheckPreservesAnIntentionallyEmptyFinalMessage(t *testing.T) {
	runner := &asyncRunnerStub{run: AsyncRun{ThreadID: "task-1", RunID: "run-1", Status: "success", Outcome: AsyncSuccess{Value: ""}}}
	middleware := AsyncSubagents(AsyncSubagent{
		Name: "worker", Description: "Works", GraphID: "worker-graph", Runner: runner,
	})
	var check datool.Tool
	for _, candidate := range middleware.Tools {
		if candidate.Definition().Name == "check_async_task" {
			check = candidate
		}
	}
	tasks := map[string]any{"task-1": asyncTaskMap(AsyncTask{TaskID: "task-1", AgentName: "worker", ThreadID: "task-1", RunID: "run-1", Status: "running"})}
	result, err := check.Execute(context.Background(), json.RawMessage(`{"task_id":"task-1"}`), datool.Runtime{State: dastate.Values{AsyncTasksKey: tasks}})
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].Text; !strings.Contains(text, `"result":""`) || strings.Contains(text, "no output") {
		t.Fatalf("result = %q", text)
	}
}
