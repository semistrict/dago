package daeval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/semistrict/dago/daeval"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func TestDeterministicAgentBehavior(t *testing.T) {
	echo := datool.Func{Spec: datool.Definition{
		Name:        "echo",
		Description: "Echo text",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}, Run: func(_ context.Context, arguments json.RawMessage, _ datool.Runtime) (datool.Result, error) {
		var input struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(arguments, &input); err != nil {
			return datool.Result{}, err
		}
		return datool.TextResult(input.Text), nil
	}}
	model := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{
			Role: damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{
				ID: "echo-1", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`),
			}},
		}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("Echoed hello")}},
	)
	agent := dagent.New(model, dagent.Options{Tools: []datool.Tool{echo}})

	evaluation := daeval.NewEvaluation(daeval.Invoke(agent, dagent.Prompt("echo hello")))
	evaluation.Name = "direct echo"
	evaluation.Category = "tool_use"
	evaluation.Correctness = []daeval.Check{
		daeval.ToolCalled("echo").AtStep(1).WithArguments(map[string]any{"text": "hello"}),
		daeval.ToolNotCalled("write_file"),
		daeval.FinalTextContainsFold("ECHOED HELLO"),
	}
	evaluation.Expectations = []daeval.Check{daeval.StepCount(2), daeval.ToolCallCount(1)}
	report := (daeval.Harness{}).Evaluate(context.Background(), evaluation)

	if report.Passed != 1 || report.Failed != 0 || report.Errors != 0 || report.Correctness != 1 {
		t.Fatalf("report = %#v", report)
	}
	if report.StepRatio == nil || *report.StepRatio != 1 || report.ToolCallRatio == nil || *report.ToolCallRatio != 1 {
		t.Fatalf("efficiency ratios = steps %v, calls %v", report.StepRatio, report.ToolCallRatio)
	}
	if report.CategoryScores["tool_use"] != 1 {
		t.Fatalf("category scores = %#v", report.CategoryScores)
	}
}

func TestTrajectoryConstructionOwnsResultAndFiles(t *testing.T) {
	assistant := damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
		ID: "read-1", Name: "read_file", Arguments: json.RawMessage(`{ "file_path": "/note.md" }`),
	}}}
	tool := damessage.Tool("read-1", "hello")
	result := dagent.Result{Messages: []damessage.Message{
		damessage.Human("read it"), assistant, tool, damessage.Assistant("line one\nline two"),
	}}
	files := map[string]string{"/note.md": "hello"}
	trajectory := daeval.TrajectoryFromResult(result, files)
	result.Messages[1].ToolCalls[0].Arguments[0] = '['
	files["/note.md"] = "changed"

	if len(trajectory.Steps) != 2 || len(trajectory.Steps[0].Observations) != 1 {
		t.Fatalf("trajectory = %#v", trajectory)
	}
	if string(trajectory.Steps[0].Action.ToolCalls[0].Arguments) != `{ "file_path": "/note.md" }` || trajectory.Files["/note.md"] != "hello" {
		t.Fatal("trajectory aliases source data")
	}
	if trajectory.Answer() != "line one\nline two" || trajectory.ToolCallCount() != 1 {
		t.Fatalf("answer/count = %q/%d", trajectory.Answer(), trajectory.ToolCallCount())
	}
	pretty := trajectory.String()
	if !strings.Contains(pretty, `read_file {"file_path":"/note.md"}`) || !strings.Contains(pretty, `line one\nline two`) {
		t.Fatalf("pretty trajectory = %q", pretty)
	}
}

func TestCorrectnessFailsWhileExpectationsStayDiagnostic(t *testing.T) {
	trajectory := daeval.Trajectory{
		Steps: []daeval.Step{{Index: 1, Action: damessage.Assistant("D\u200bONE")}},
		Files: map[string]string{"/result.txt": "safe output"},
	}
	report := (daeval.Harness{}).Evaluate(context.Background(),
		evaluation(fixed(trajectory), daeval.Evaluation{
			Name:     "correct with inefficient shape",
			Category: "files",
			Correctness: []daeval.Check{
				daeval.FinalTextContains("DONE"),
				daeval.FinalTextContainsAny("DONE", "complete"),
				daeval.FinalTextExcludesFold("error"),
				daeval.FinalTextMinLength(4),
				daeval.FileContains("/result.txt", "safe"),
				daeval.FileExcludes("/result.txt", "secret"),
				daeval.FileAbsent("/secret.txt"),
			},
			Expectations: []daeval.Check{daeval.StepCount(2), daeval.ToolCallCount(1), daeval.MaxToolCallCount(0)},
		}),
		evaluation(fixed(trajectory), daeval.Evaluation{
			Name:        "wrong answer",
			Category:    "files",
			Correctness: []daeval.Check{daeval.FileEquals("/result.txt", "different")},
		}),
	)

	if report.Passed != 1 || report.Failed != 1 || report.Correctness != 0.5 || report.CategoryScores["files"] != 0.5 {
		t.Fatalf("report = %#v", report)
	}
	if got := report.Results[0]; got.Status != daeval.StatusPassed || len(got.Expectations) != 2 || len(got.Failures) != 0 {
		t.Fatalf("soft expectations changed status: %#v", got)
	}
	if got := report.Results[1]; got.Status != daeval.StatusFailed || len(got.Failures) != 1 || !strings.Contains(got.Failures[0].Message, "trajectory:") {
		t.Fatalf("failure = %#v", got)
	}
}

func TestToolSelectorsMatchStepAndArguments(t *testing.T) {
	trajectory := daeval.Trajectory{Steps: []daeval.Step{
		{Index: 1, Action: toolAction("write_file", `{"file_path":"/keep.md","mode":"w"}`)},
		{Index: 2, Action: toolAction("write_file", `{"file_path":"/other.md","reason":null}`)},
	}}
	checks := []struct {
		check daeval.Check
		want  bool
	}{
		{daeval.ToolCalled("write_file").AtStep(1).WithArguments(map[string]any{"file_path": "/keep.md"}), true},
		{daeval.ToolCalled("write_file").AtStep(1).WithExactArguments(map[string]any{"file_path": "/keep.md"}), false},
		{daeval.ToolNotCalled("write_file").WithArguments(map[string]any{"file_path": "/secret.md"}), true},
		{daeval.ToolCalled("write_file").WithArguments(map[string]any{"reason": nil}), true},
		{daeval.ToolNotCalled("write_file").AtStep(3), false},
	}
	for index, test := range checks {
		got, _ := test.check.Evaluate(trajectory)
		if got != test.want {
			t.Errorf("check %d = %v, want %v", index, got, test.want)
		}
	}

	arguments := map[string]any{"file_path": "/keep.md"}
	check := daeval.ToolCalled("write_file").WithArguments(arguments)
	arguments["file_path"] = "/changed.md"
	if passed, _ := check.Evaluate(trajectory); !passed {
		t.Fatal("tool selector aliases caller arguments")
	}
	var zero daeval.ToolCheck
	if passed, _ := zero.Evaluate(trajectory); passed {
		t.Fatal("zero tool selector passed vacuously")
	}
}

func TestExecutionErrorsSkipsCancellationAndDefaults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := (daeval.Harness{}).Evaluate(ctx,
		daeval.Evaluation{Skip: "not applicable"},
		evaluation(func(context.Context) (daeval.Trajectory, error) {
			return daeval.Trajectory{}, errors.New("must not run")
		}, daeval.Evaluation{}),
		daeval.Evaluation{},
	)

	if report.Skipped != 1 || report.Errors != 2 || report.Failed != 0 || report.Correctness != 0 {
		t.Fatalf("report = %#v", report)
	}
	if report.Results[0].Name != "evaluation_1" || report.Results[0].Category != "uncategorized" {
		t.Fatalf("defaults = %#v", report.Results[0])
	}
	if report.Results[1].Error != context.Canceled.Error() || report.Results[2].Error != "evaluation run is required" {
		t.Fatalf("errors = %#v", report.Results)
	}
}

func TestFailureBoundAndStableJSON(t *testing.T) {
	long := strings.Repeat("界", 100)
	report := (daeval.Harness{MaxFailureBytes: 80}).Evaluate(context.Background(), evaluation(
		fixed(daeval.Trajectory{Steps: []daeval.Step{{Index: 1, Action: damessage.Assistant(long)}}}), daeval.Evaluation{
			Correctness: []daeval.Check{daeval.FinalTextContains("absent")},
		}))
	message := report.Results[0].Failures[0].Message
	if len(message) > 80 || !strings.HasSuffix(message, "... [truncated]") || !utf8.ValidString(message) {
		t.Fatalf("bounded failure = %q (%d bytes)", message, len(message))
	}
	first, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(report)
	if err != nil || string(first) != string(second) || !strings.Contains(string(first), `"version":1`) {
		t.Fatalf("JSON is not stable: %s / %s / %v", first, second, err)
	}
}

func TestHarnessRejectsNegativeFailureBound(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Harness.Evaluate did not panic")
		}
	}()
	(daeval.Harness{MaxFailureBytes: -1}).Evaluate(context.Background())
}

func TestProgrammerErrorsPanicAtConstruction(t *testing.T) {
	for name, construct := range map[string]func(){
		"nil agent":       func() { daeval.Invoke(nil, dagent.Prompt("input")) },
		"nil run":         func() { daeval.NewEvaluation(nil) },
		"empty tool":      func() { daeval.ToolCalled(" ") },
		"empty file path": func() { daeval.FileAbsent(" ") },
		"bad step":        func() { daeval.ToolCalled("read").AtStep(0) },
		"bad text length": func() { daeval.FinalTextMinLength(-1) },
		"no text choices": func() { daeval.FinalTextContainsAny() },
		"empty text":      func() { daeval.FinalTextContains("") },
		"bad step count":  func() { daeval.StepCount(-1) },
		"bad args": func() {
			daeval.ToolCalled("read").WithArguments(map[string]any{"channel": make(chan int)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("did not panic")
				}
			}()
			construct()
		})
	}
}

type customCheck struct{}

func (customCheck) Evaluate(trajectory daeval.Trajectory) (bool, string) {
	return trajectory.Answer() == "custom", "want custom answer"
}

func TestCustomCheckAndZeroHarness(t *testing.T) {
	report := (daeval.Harness{}).Evaluate(context.Background(), evaluation(
		fixed(daeval.Trajectory{Steps: []daeval.Step{{Action: damessage.Assistant("custom")}}}), daeval.Evaluation{
			Correctness: []daeval.Check{customCheck{}},
		}))
	if report.Passed != 1 || report.Version != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func fixed(trajectory daeval.Trajectory) daeval.Run {
	return func(context.Context) (daeval.Trajectory, error) { return trajectory, nil }
}

func evaluation(run daeval.Run, config daeval.Evaluation) daeval.Evaluation {
	result := daeval.NewEvaluation(run)
	result.Name = config.Name
	result.Category = config.Category
	result.Correctness = config.Correctness
	result.Expectations = config.Expectations
	result.Skip = config.Skip
	return result
}

func toolAction(name, arguments string) damessage.Message {
	return damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
		ID: name, Name: name, Arguments: json.RawMessage(arguments),
	}}}
}
