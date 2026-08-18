package scorecard_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/daeval/harbor"
	"github.com/semistrict/dago/daeval/scorecard"
)

func TestScorecardComparesModelsCategoriesAndStatistics(t *testing.T) {
	t.Parallel()

	strong := scorecard.NewModel("provider-a/strong")
	strong.Provider = "provider-a"
	weak := scorecard.NewModel("provider-b/weak")
	weak.Provider = "provider-b"
	var order []string
	runner := scorecard.RunnerFunc(func(_ context.Context, model scorecard.Model, task harbor.Task) (harbor.Trial, error) {
		order = append(order, model.ID+":"+task.Name)
		reward := 1.0
		if model.ID == weak.ID && task.Category == "research" {
			reward = 0
		}
		return harbor.Trial{Reward: &reward}, nil
	})
	contextTask, err := harbor.NewContextBenchRecord("cb-cloud-1", "retrieve").Adapt()
	if err != nil {
		t.Fatal(err)
	}
	researchTask, err := harbor.NewDRBenchRecord("DR0001", "research", harbor.DRBenchEmail).Adapt()
	if err != nil {
		t.Fatal(err)
	}

	report := scorecard.New(runner, strong, weak).Evaluate(context.Background(), contextTask, researchTask)
	if report.Error != "" || report.Runs != 4 || report.RequestedModels != 2 || report.RequestedTasks != 2 {
		t.Fatalf("report header = %#v", report)
	}
	wantOrder := []string{
		"provider-a/strong:cb-cloud-1",
		"provider-a/strong:DR0001",
		"provider-b/weak:cb-cloud-1",
		"provider-b/weak:DR0001",
	}
	if strings.Join(order, "|") != strings.Join(wantOrder, "|") {
		t.Fatalf("runner order = %v, want %v", order, wantOrder)
	}
	if report.Overall.Total != 4 || report.Overall.Passed != 3 || report.Overall.Failed != 1 || report.Overall.Correctness != 0.75 {
		t.Fatalf("overall = %#v", report.Overall)
	}
	if report.Overall.Confidence95.Lower <= 0 || report.Overall.Confidence95.Upper >= 1 ||
		report.Overall.MinimumDetectableEffect <= 0 {
		t.Fatalf("statistics = %#v", report.Overall)
	}
	if len(report.Models) != 2 ||
		report.Models[0].Report.Evaluation.Correctness != 1 ||
		report.Models[1].Report.Evaluation.Correctness != 0.5 {
		t.Fatalf("model reports = %#v", report.Models)
	}
	if len(report.Categories) != 2 ||
		report.Categories[0].Category != "context" ||
		report.Categories[0].MeanCorrectness != 1 ||
		report.Categories[1].Category != "research" ||
		report.Categories[1].MeanCorrectness != 0.5 {
		t.Fatalf("category scores = %#v", report.Categories)
	}
	if len(report.Leaderboard) != 2 ||
		report.Leaderboard[0].Rank != 1 ||
		report.Leaderboard[0].ModelID != strong.ID ||
		report.Leaderboard[1].Rank != 2 ||
		report.Leaderboard[1].ModelID != weak.ID {
		t.Fatalf("leaderboard = %#v", report.Leaderboard)
	}
}

func TestScorecardReportIsDeterministicAndContainsNoRunnerCredentials(t *testing.T) {
	t.Parallel()

	const secret = "private-runner-token"
	model := scorecard.NewModel("model/stable")
	task := harbor.NewTask("task", "work")
	runner := scorecard.RunnerFunc(func(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
		if secret == "" {
			t.Fatal("unreachable")
		}
		reward := 1.0
		return harbor.Trial{Reward: &reward}, nil
	})
	card := scorecard.New(runner, model)
	first := card.Evaluate(context.Background(), task)
	second := card.Evaluate(context.Background(), task)
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("reports differ:\n%s\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), secret) {
		t.Fatalf("runner credential leaked: %s", firstJSON)
	}
}

func TestScorecardRejectsUnfairOrExcessiveWorkBeforeExecution(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	runner := scorecard.RunnerFunc(func(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
		calls.Add(1)
		return harbor.Trial{}, nil
	})
	modelA := scorecard.NewModel("model-a")
	modelB := scorecard.NewModel("model-b")
	taskA := harbor.NewTask("task-a", "work")
	taskB := harbor.NewTask("task-b", "work")
	tests := []struct {
		name      string
		configure func(*scorecard.Scorecard)
		want      string
	}{
		{name: "models", configure: func(card *scorecard.Scorecard) { card.MaxModels = 1 }, want: "2 models"},
		{name: "tasks", configure: func(card *scorecard.Scorecard) { card.MaxTasks = 1 }, want: "2 tasks"},
		{name: "runs", configure: func(card *scorecard.Scorecard) { card.MaxRuns = 3 }, want: "4 runs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			card := scorecard.New(runner, modelA, modelB)
			test.configure(&card)
			report := card.Evaluate(context.Background(), taskA, taskB)
			if !strings.Contains(report.Error, test.want) || report.Runs != 0 || len(report.Models) != 0 {
				t.Fatalf("bounded report = %#v", report)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("runner called %d times for rejected work", calls.Load())
	}
}

func TestScorecardPropagatesCancellationAndContainsRunnerPanic(t *testing.T) {
	t.Parallel()

	model := scorecard.NewModel("model")
	task := harbor.NewTask("task", "work")
	timeoutRunner := scorecard.RunnerFunc(func(ctx context.Context, _ scorecard.Model, _ harbor.Task) (harbor.Trial, error) {
		<-ctx.Done()
		return harbor.Trial{}, ctx.Err()
	})
	card := scorecard.New(timeoutRunner, model)
	card.TrialTimeout = 5 * time.Millisecond
	card.ModelTimeout = time.Second
	card.Timeout = time.Second
	report := card.Evaluate(context.Background(), task)
	if report.Overall.Errors != 1 ||
		report.Models[0].Report.Trials[0].FailureCategory != harbor.FailureInfraTimeout {
		t.Fatalf("timeout report = %#v", report)
	}

	panicRunner := scorecard.RunnerFunc(func(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
		panic("private panic detail")
	})
	report = scorecard.New(panicRunner, model).Evaluate(context.Background(), task)
	if report.Overall.Errors != 1 ||
		report.Models[0].Report.Trials[0].FailureCategory != harbor.FailureUnknown {
		t.Fatalf("panic report = %#v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private panic detail") {
		t.Fatalf("panic detail leaked: %s", encoded)
	}
}

func TestScorecardCanceledContextDoesNotInvokeRunner(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	runner := scorecard.RunnerFunc(func(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
		calls.Add(1)
		return harbor.Trial{}, errors.New("unexpected")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := scorecard.New(runner, scorecard.NewModel("model")).Evaluate(ctx, harbor.NewTask("task", "work"))
	if calls.Load() != 0 || report.Overall.Errors != 1 {
		t.Fatalf("canceled report = %#v, calls = %d", report, calls.Load())
	}
}

func TestScorecardEmptyTaskSetHasUsefulStableShape(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	runner := scorecard.RunnerFunc(func(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
		calls.Add(1)
		return harbor.Trial{}, nil
	})
	report := scorecard.New(
		runner,
		scorecard.NewModel("model-b"),
		scorecard.NewModel("model-a"),
	).Evaluate(context.Background())
	if calls.Load() != 0 || report.Runs != 0 || len(report.Models) != 2 ||
		len(report.Categories) != 0 || len(report.Leaderboard) != 2 {
		t.Fatalf("empty report = %#v", report)
	}
	if report.Leaderboard[0].ModelID != "model-a" || report.Overall.MinimumDetectableEffect != 1 {
		t.Fatalf("empty defaults = %#v", report)
	}
}

func TestScorecardRequiredInputsAndStaticConfiguration(t *testing.T) {
	t.Parallel()

	model := scorecard.NewModel("provider:model")
	runner := scorecard.RunnerFunc(func(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
		return harbor.Trial{}, nil
	})
	assertPanics(t, func() { scorecard.New(nil, model) })
	var typedNil *fakeRunner
	assertPanics(t, func() { scorecard.New(typedNil, model) })
	assertPanics(t, func() { scorecard.NewModel("../unsafe") })
	assertPanics(t, func() { scorecard.New(runner, model, model) })
	invalidProvider := model
	invalidProvider.Provider = "bad/provider"
	assertPanics(t, func() { scorecard.New(runner, invalidProvider) })
	assertPanics(t, func() {
		card := scorecard.New(runner, model)
		card.MaxRuns = -1
		card.Evaluate(context.Background())
	})
}

type fakeRunner struct{}

func (*fakeRunner) Run(context.Context, scorecard.Model, harbor.Task) (harbor.Trial, error) {
	return harbor.Trial{}, nil
}

func assertPanics(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("function did not panic")
		}
	}()
	call()
}
