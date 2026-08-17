package dagoal

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func fixedOptions() Options {
	return Options{
		Clock: func() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) },
		NewID: func() (string, error) { return "goal-1", nil },
	}
}

func TestGoalToolsPersistAccountAndComplete(t *testing.T) {
	options := fixedOptions()
	first := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{
			Role:      damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{ID: "create-1", Name: "create_goal", Arguments: json.RawMessage(`{"objective":"Finish the migration","token_budget":100}`)}},
			Usage:     &damessage.Usage{TotalTokens: 10, StartedAt: options.Clock(), FinishedAt: options.Clock().Add(2 * time.Second)},
		}}},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				context := ""
				if request.SystemMessage != nil {
					context += request.SystemMessage.TextContent()
				}
				for _, message := range request.Messages {
					if message.Role == damessage.RoleSystem {
						context += message.TextContent()
					}
				}
				if !strings.Contains(context, "Finish the migration") {
					return errors.New("active goal context missing")
				}
				return nil
			},
			Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, Content: damessage.Assistant("working").Content, Usage: &damessage.Usage{TotalTokens: 5}}},
		},
	)
	saver := dacheckpoint.NewMemorySaver()
	agent := dagent.New(first, dagent.Options{Middleware: []dagent.Middleware{Middleware(options)}, Saver: saver})
	config := dacheckpoint.Config{ThreadID: "goal-tools"}
	result, err := agent.Invoke(t.Context(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("Create a goal")}})
	if err != nil {
		t.Fatal(err)
	}
	goal, present := FromState(result.State)
	if !present || goal == nil || goal.Status != StatusActive || goal.TokensUsed != 15 || goal.TimeUsedSeconds != 2 {
		t.Fatalf("goal = %#v, present = %t", goal, present)
	}

	second := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{
			Role:      damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{ID: "complete-1", Name: "update_goal", Arguments: json.RawMessage(`{"status":"complete"}`)}},
			Usage:     &damessage.Usage{TotalTokens: 4},
		}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, Content: damessage.Assistant("done").Content, Usage: &damessage.Usage{TotalTokens: 1}}}},
	)
	agent = dagent.New(second, dagent.Options{Middleware: []dagent.Middleware{Middleware(options)}, Saver: saver})
	result, err = agent.Invoke(t.Context(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("Continue")}})
	if err != nil {
		t.Fatal(err)
	}
	goal, _ = FromState(result.State)
	if goal == nil || goal.Status != StatusComplete || goal.TokensUsed != 19 {
		t.Fatalf("completed goal = %#v", goal)
	}
	foundReport := false
	for _, message := range result.Messages {
		foundReport = foundReport || strings.Contains(message.TextContent(), "using 19 of 100 tokens")
	}
	if !foundReport {
		t.Fatal("completion tool result omitted final budget usage")
	}
}

func TestGoalServiceControlsLifecycleAndClear(t *testing.T) {
	options := fixedOptions()
	agent := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{
		Middleware: []dagent.Middleware{Middleware(options)}, Saver: dacheckpoint.NewMemorySaver(),
	})
	service := NewService(agent, options)
	config := dacheckpoint.Config{ThreadID: "goal-service"}
	objective := "Ship the release"
	goal, err := service.Set(t.Context(), config, SetRequest{Objective: &objective, Budget: SetBudget(50)})
	if err != nil {
		t.Fatal(err)
	}
	if goal.ID != "goal-1" || goal.Status != StatusActive || goal.TokenBudget == nil || *goal.TokenBudget != 50 {
		t.Fatalf("created goal = %#v", goal)
	}
	paused := StatusPaused
	goal, err = service.Set(t.Context(), config, SetRequest{Status: &paused})
	if err != nil || goal.Status != StatusPaused {
		t.Fatalf("paused goal = %#v, %v", goal, err)
	}
	active := StatusActive
	goal, err = service.Set(t.Context(), config, SetRequest{Status: &active, Budget: ClearBudget()})
	if err != nil || goal.Status != StatusActive || goal.TokenBudget != nil {
		t.Fatalf("resumed goal = %#v, %v", goal, err)
	}
	cleared, err := service.Clear(t.Context(), config)
	if err != nil || !cleared {
		t.Fatalf("Clear() = %t, %v", cleared, err)
	}
	goal, err = service.Get(t.Context(), config)
	if err != nil || goal != nil {
		t.Fatalf("Get() after clear = %#v, %v", goal, err)
	}
}

func TestGoalStateRestartsFromSQLite(t *testing.T) {
	options := fixedOptions()
	path := filepath.Join(t.TempDir(), "threads.db")
	firstSaver, err := checkpointsqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstAgent := dagent.New(modeltest.New(damodel.Profile{}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("working")},
	}), dagent.Options{
		Middleware: []dagent.Middleware{Middleware(options)}, Saver: firstSaver,
	})
	config := dacheckpoint.Config{ThreadID: "goal-sqlite"}
	objective := "Survive restart"
	if _, err := NewService(firstAgent, options).Set(t.Context(), config, SetRequest{Objective: &objective, Budget: SetBudget(500)}); err != nil {
		t.Fatal(err)
	}
	if _, err := firstAgent.Invoke(t.Context(), dagent.Input{
		Config: config, Messages: []damessage.Message{damessage.Human("Continue")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := firstSaver.Close(); err != nil {
		t.Fatal(err)
	}

	secondSaver, err := checkpointsqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSaver.Close()
	secondAgent := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{
		Middleware: []dagent.Middleware{Middleware(options)}, Saver: secondSaver,
	})
	goal, err := NewService(secondAgent, options).Get(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	if goal == nil || goal.Objective != objective || goal.TokenBudget == nil || *goal.TokenBudget != 500 {
		t.Fatalf("restored goal = %#v", goal)
	}
}

func TestGoalAccountingStopsAtBudget(t *testing.T) {
	options := fixedOptions()
	model := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{
		Role: damessage.RoleAssistant, Content: damessage.Assistant("worked").Content, Usage: &damessage.Usage{TotalTokens: 5},
	}}})
	agent := dagent.New(model, dagent.Options{Middleware: []dagent.Middleware{Middleware(options)}, Saver: dacheckpoint.NewMemorySaver()})
	service := NewService(agent, options)
	config := dacheckpoint.Config{ThreadID: "goal-budget"}
	objective := "Use the budget"
	if _, err := service.Set(t.Context(), config, SetRequest{Objective: &objective, Budget: SetBudget(5)}); err != nil {
		t.Fatal(err)
	}
	result, err := agent.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("go")}})
	if err != nil {
		t.Fatal(err)
	}
	goal, _ := FromState(result.State)
	if goal == nil || goal.Status != StatusBudgetLimited || goal.TokensUsed != 5 || goal.Actionable() {
		t.Fatalf("budget-limited goal = %#v", goal)
	}
}

func TestGoalMiddlewareAccountsToolUsageAndWholeRunTime(t *testing.T) {
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var clockCalls atomic.Int64
	options := fixedOptions()
	options.Clock = func() time.Time { return base.Add(time.Duration(clockCalls.Add(1)-1) * time.Second) }
	middleware := Middleware(options)
	goal := &Goal{ID: "goal-1", Objective: "Account everything", Status: StatusActive, CreatedAt: base, UpdatedAt: base}
	values := dastate.Values{StateKey: goalToState(goal)}
	runUpdate, err := middleware.BeforeAgent(t.Context(), values, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	toolResult := damessage.Tool("tool-1", "done")
	toolResult.OtherUsage = []damessage.PurposedUsage{{Purpose: "helper", Usage: damessage.Usage{TotalTokens: 3}}}
	system := damessage.System("You are a helpful assistant.")
	response, err := middleware.WrapModelCall(t.Context(), dagent.ModelRequest{
		SystemMessage: &system, State: values, Messages: []damessage.Message{damessage.Human("go"), damessage.Assistant("calling"), toolResult},
	}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		return dagent.ModelResponse{Messages: []damessage.Message{{
			Role: damessage.RoleAssistant, Content: damessage.Assistant("worked").Content, Usage: &damessage.Usage{TotalTokens: 2},
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	values[StateKey] = response.Update[StateKey]
	values[goalRunKey] = runUpdate[goalRunKey]
	after, err := middleware.AfterAgent(t.Context(), values, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	values[StateKey] = after[StateKey]
	accounted, _ := FromState(values)
	if accounted == nil || accounted.TokensUsed != 5 || accounted.TimeUsedSeconds != 2 {
		t.Fatalf("accounted goal = %#v", accounted)
	}
}

func TestCreateGoalRejectsUnfinishedGoal(t *testing.T) {
	options := fixedOptions()
	middleware := Middleware(options)
	var createToolIndex int
	for index, tool := range middleware.Tools {
		if tool.Definition().Name == "create_goal" {
			createToolIndex = index
			break
		}
	}
	existing := &Goal{ID: "existing", Objective: "Existing", Status: StatusPaused}
	_, err := middleware.Tools[createToolIndex].Execute(t.Context(), json.RawMessage(`{"objective":"Replacement"}`), datoolRuntime(existing))
	if !errors.Is(err, ErrUnfinishedGoal) {
		t.Fatalf("create_goal error = %v", err)
	}
}

func datoolRuntime(goal *Goal) datool.Runtime {
	return datool.Runtime{State: dastate.Values{StateKey: goal}}
}
