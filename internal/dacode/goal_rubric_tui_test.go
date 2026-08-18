package dacode

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

func TestParseCLIGoalAndRubricFlags(t *testing.T) {
	goal, err := parseCLI([]string{"--goal", "ship safely"}, io.Discard)
	if err != nil || goal.goal != "ship safely" {
		t.Fatalf("goal options = %#v, %v", goal, err)
	}
	rubric, err := parseCLI([]string{"-n", "run checks", "--rubric", "tests pass", "--rubric-model", "fixture:grader", "--rubric-max-iterations", "7"}, io.Discard)
	if err != nil || rubric.rubric != "tests pass" || rubric.rubricModel != "fixture:grader" || rubric.rubricMax != 7 {
		t.Fatalf("rubric options = %#v, %v", rubric, err)
	}
	for _, arguments := range [][]string{
		{"--rubric-max-iterations", "0"},
		{"--rubric-max-iterations", "invalid"},
	} {
		if _, err := parseCLI(arguments, io.Discard); err == nil {
			t.Fatalf("parseCLI(%v) succeeded", arguments)
		}
	}
}

func TestRubricCLIOptionsRequireHeadlessExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, arguments := range [][]string{
		{"--rubric", "tests pass"},
		{"--rubric-model", "fixture:grader"},
		{"--rubric-max-iterations", "2"},
	} {
		err := Run(context.Background(), arguments, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "requires non-interactive mode") {
			t.Fatalf("Run(%v) error = %v", arguments, err)
		}
	}
}

func TestNonInteractiveRubricIsAppliedToInitialTurn(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{result: dagent.Result{Messages: []damessage.Message{damessage.Assistant("done")}}}}}
	if err := runNonInteractive(t.Context(), runner, "/work", "thread-1", "run checks", nonInteractiveOptions{
		Quiet: true, Rubric: "- Tests pass.\n- No unrelated changes.",
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(runner.inputs) != 1 || runner.inputs[0].State[dago.RubricKey] != "- Tests pass.\n- No unrelated changes." {
		t.Fatalf("headless rubric input = %#v", runner.inputs)
	}
}

func TestTUIRubricCommandsPersistAliasAndApplyNextTurnOnce(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")

	command, handled := model.slashCommand("/criteria set tests pass; no unrelated changes")
	if !handled || command == nil {
		t.Fatalf("criteria alias handled=%t command=%v", handled, command)
	}
	if _, next := model.Update(command()); next != nil || runner.rubric.Criteria != "tests pass; no unrelated changes" {
		t.Fatalf("persistent rubric = %#v, next=%v", runner.rubric, next)
	}

	command, handled = model.slashCommand("/rubric next browser restart passes")
	if !handled || command != nil || model.nextRubric != "browser restart passes" {
		t.Fatalf("next rubric handled=%t command=%v value=%q", handled, command, model.nextRubric)
	}
	model.submitPrompt("run the check")
	if len(runner.inputs) != 1 || runner.inputs[0].State[dago.RubricKey] != "browser restart passes" || !model.oneShotRubric {
		t.Fatalf("next-turn input = %#v", runner.inputs)
	}
	_, clear := model.finishStream(streamDoneMsg{result: dagent.Result{State: dastate.Values{
		dago.RubricKey: "browser restart passes", dago.RubricStatusKey: string(dago.RubricSatisfied),
	}}})
	if clear == nil {
		t.Fatal("one-turn rubric was not scheduled for clearing")
	}
	if _, next := model.Update(clear()); next != nil || model.oneShotRubric || model.rubric.Criteria != "tests pass; no unrelated changes" {
		t.Fatalf("one-turn rubric remained active: next=%v model=%#v", next, model.rubric)
	}
	if runner.rubric.Criteria != "tests pass; no unrelated changes" {
		t.Fatalf("sticky rubric was not restored: %#v", runner.rubric)
	}
}

func TestTUIGoalGraderPrefixesRemainValidMultiwordObjectives(t *testing.T) {
	for _, objective := range []string{"model parser migration", "max-iterations for the parser"} {
		model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
		command := model.goalCommand(objective)
		if command == nil || model.status != "Drafting goal criteria" {
			t.Fatalf("goal %q was intercepted as a settings command: status=%q command=%v", objective, model.status, command)
		}
	}
}

func TestTUIRestoresGoalRubricAndRendersPersistentPanel(t *testing.T) {
	goal := &dagoal.Goal{ID: "goal-1", Objective: "Ship safely", Criteria: "- tests pass", Status: dagoal.StatusActive}
	runner := &fakeRunner{goal: goal, rubric: dago.RubricSnapshot{Criteria: goal.Criteria, Status: dago.RubricSatisfied}, streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", false, true, "")
	message := loadGoal(t.Context(), runner, "thread-1")().(goalLoadedMsg)
	model.Update(message)
	if model.goal == nil || model.rubric.Criteria != "- tests pass" {
		t.Fatalf("restored goal=%#v rubric=%#v", model.goal, model.rubric)
	}
	model.resize(100, 30)
	view := model.View()
	for _, want := range []string{"Goal • active", "Ship safely", "rubric"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %s", want, view)
		}
	}
}

func TestTUIRubricEventsRenderPerCriterionResults(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	model.rubric.Criteria = "- tests pass"
	payload, err := json.Marshal(map[string]any{
		"type": "rubric_evaluation_end", "grading_run_id": "run-1", "iteration": 0,
		"result": "needs_revision", "explanation": "browser gap",
		"criteria": []map[string]any{{"name": "unit", "passed": true}, {"name": "browser", "passed": false, "gap": "restart missing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	model.applyEvent(dagent.Event{Mode: dagent.EventCustom, Custom: payload})
	if len(model.items) != 1 || !strings.Contains(model.items[0].text, "✗ browser: restart missing") || model.rubric.Status != dago.RubricNeedsRevision {
		t.Fatalf("items=%#v rubric=%#v", model.items, model.rubric)
	}
}
