package harbor_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/daeval"
	"github.com/semistrict/dago/daeval/harbor"
	"github.com/semistrict/dago/damessage"
)

func TestBenchmarkEvaluatesRewardsChecksAndSkipsDeterministically(t *testing.T) {
	t.Parallel()

	runs := 0
	runner := harbor.RunnerFunc(func(_ context.Context, task harbor.Task) (harbor.Trial, error) {
		runs++
		if task.Correctness != nil || task.Expectations != nil {
			t.Fatal("host-only checks crossed the runner boundary")
		}
		if task.Metadata["suite"] != "terminal" {
			t.Fatalf("metadata = %#v", task.Metadata)
		}
		task.Metadata["suite"] = "mutated"
		reward := 1.0
		if task.Name == "wrong" {
			reward = 0.25
		}
		return harbor.Trial{
			Reward: &reward,
			Trajectory: daeval.Trajectory{Steps: []daeval.Step{{
				Index:  1,
				Action: damessage.Assistant("finished safely"),
			}}},
		}, nil
	})
	benchmark := harbor.NewBenchmark(runner)
	passing := harbor.NewTask("right", "finish the task")
	passing.Category = "terminal"
	passing.Metadata = map[string]string{"suite": "terminal"}
	passing.Correctness = []daeval.Check{daeval.FinalTextContains("safely")}
	failing := harbor.NewTask("wrong", "finish another task")
	failing.Category = "terminal"
	failing.Metadata = map[string]string{"suite": "terminal"}
	skipped := harbor.NewTask("unsupported", "unused")
	skipped.Skip = "requires hardware"

	report := benchmark.Evaluate(context.Background(), passing, failing, skipped)
	if runs != 2 {
		t.Fatalf("runner calls = %d, want 2", runs)
	}
	if passing.Metadata["suite"] != "terminal" {
		t.Fatalf("runner mutated caller metadata: %#v", passing.Metadata)
	}
	if report.Evaluation.Passed != 1 || report.Evaluation.Failed != 1 || report.Evaluation.Skipped != 1 {
		t.Fatalf("counts = %#v", report.Evaluation)
	}
	if report.CapabilityFailures != 1 || report.InfrastructureFailures != 0 || report.UnknownFailures != 0 {
		t.Fatalf("failure counts = %#v", report)
	}
	if got := report.Trials[1]; got.Reward != 0.25 || !got.RewardPresent || got.FailureCategory != harbor.FailureCapability {
		t.Fatalf("failed trial = %#v", got)
	}
	if report.Trials[2].RewardPresent {
		t.Fatalf("skipped trial unexpectedly has reward: %#v", report.Trials[2])
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportAgain := benchmark.Evaluate(context.Background(), passing, failing, skipped)
	encodedAgain, err := json.Marshal(reportAgain)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != string(encodedAgain) {
		t.Fatalf("reports differ:\n%s\n%s", encoded, encodedAgain)
	}
}

func TestBenchmarkClassifiesInfrastructureWithoutScanningAssistantText(t *testing.T) {
	t.Parallel()

	reward := 1.0
	tests := []struct {
		name     string
		trial    harbor.Trial
		err      error
		category harbor.FailureCategory
		infra    bool
	}{
		{
			name: "structured OOM exit",
			trial: harbor.Trial{Reward: &reward, Trajectory: trajectoryWithObservation(
				"I will explain exit code 124 but succeeded", `{"exit_code": 137}`,
			)},
			category: harbor.FailureInfraOOM,
			infra:    true,
		},
		{
			name:     "timeout exception",
			trial:    harbor.Trial{Exception: "operation deadline exceeded"},
			category: harbor.FailureInfraTimeout,
			infra:    true,
		},
		{
			name:     "sandbox exception",
			trial:    harbor.Trial{Exception: "connection refused by worker"},
			category: harbor.FailureInfraSandbox,
			infra:    true,
		},
		{
			name:     "unrecognized runner error",
			err:      errors.New("provider returned a malformed envelope"),
			category: harbor.FailureUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := harbor.RunnerFunc(func(context.Context, harbor.Task) (harbor.Trial, error) {
				return test.trial, test.err
			})
			report := harbor.NewBenchmark(runner).Evaluate(context.Background(), harbor.NewTask("task", "work"))
			if report.Evaluation.Errors != 1 {
				t.Fatalf("errors = %d, want 1", report.Evaluation.Errors)
			}
			if got := report.Trials[0]; got.FailureCategory != test.category || got.Infrastructure != test.infra {
				t.Fatalf("trial = %#v, want category %q infra %v", got, test.category, test.infra)
			}
		})
	}
}

func TestExtractExitCodesUsesObservationsOnly(t *testing.T) {
	t.Parallel()

	trajectory := trajectoryWithObservation(
		"The phrase exit code 124 in assistant output is not evidence.",
		"exit-code 0\nexit code: 1\nexit_code\": 137\nexit.code: 124",
	)
	got := harbor.ExtractExitCodes(trajectory)
	want := []int{1, 137}
	if len(got) != len(want) {
		t.Fatalf("exit codes = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("exit codes = %v, want %v", got, want)
		}
	}
}

func TestBenchmarkEnforcesInputAndResultBoundsBeforeFurtherWork(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	reward := 1.0
	runner := harbor.RunnerFunc(func(_ context.Context, task harbor.Task) (harbor.Trial, error) {
		calls.Add(1)
		if task.Name == "large-result" {
			return harbor.Trial{Reward: &reward, Trajectory: daeval.Trajectory{Steps: []daeval.Step{{
				Index: 1, Action: damessage.Assistant(strings.Repeat("r", 65)),
			}}}}, nil
		}
		return harbor.Trial{Reward: &reward}, nil
	})
	benchmark := harbor.NewBenchmark(runner)
	benchmark.MaxInstructionBytes = 32
	benchmark.MaxResultBytes = 64
	benchmark.MaxTasks = 2
	tooLarge := harbor.NewTask("large-input", strings.Repeat("i", 33))
	largeResult := harbor.NewTask("large-result", "run")
	omitted := harbor.NewTask("omitted", "run")

	report := benchmark.Evaluate(context.Background(), tooLarge, largeResult, omitted)
	if calls.Load() != 1 {
		t.Fatalf("runner calls = %d, want 1", calls.Load())
	}
	if report.OmittedTasks != 1 || report.Evaluation.Total != 2 || report.Evaluation.Errors != 2 {
		t.Fatalf("bounded report = %#v", report)
	}
	if !strings.Contains(report.Evaluation.Results[0].Error, "task text exceeds") {
		t.Fatalf("input error = %q", report.Evaluation.Results[0].Error)
	}
	if !strings.Contains(report.Evaluation.Results[1].Error, "result exceeds") {
		t.Fatalf("result error = %q", report.Evaluation.Results[1].Error)
	}
}

func TestBenchmarkPropagatesTrialDeadline(t *testing.T) {
	t.Parallel()

	runner := harbor.RunnerFunc(func(ctx context.Context, _ harbor.Task) (harbor.Trial, error) {
		<-ctx.Done()
		return harbor.Trial{}, ctx.Err()
	})
	benchmark := harbor.NewBenchmark(runner)
	benchmark.TrialTimeout = 5 * time.Millisecond
	benchmark.Timeout = time.Second
	report := benchmark.Evaluate(context.Background(), harbor.NewTask("slow", "wait"))
	if report.Evaluation.Errors != 1 || report.Trials[0].FailureCategory != harbor.FailureInfraTimeout {
		t.Fatalf("timeout report = %#v", report)
	}
}

func TestBenchmarkRejectsNonFiniteReward(t *testing.T) {
	t.Parallel()

	notANumber := math.NaN()
	runner := harbor.RunnerFunc(func(context.Context, harbor.Task) (harbor.Trial, error) {
		return harbor.Trial{Reward: &notANumber}, nil
	})
	report := harbor.NewBenchmark(runner).Evaluate(context.Background(), harbor.NewTask("bad reward", "run"))
	if report.Evaluation.Errors != 1 || report.Trials[0].FailureCategory != harbor.FailureUnknown {
		t.Fatalf("report = %#v", report)
	}
	if _, err := json.Marshal(report); err != nil {
		t.Fatalf("invalid reward made report non-JSON: %v", err)
	}
}

func TestBenchmarkBoundsEmptyStructuralItemsAndContainsRunnerPanics(t *testing.T) {
	t.Parallel()

	structural := harbor.NewBenchmark(harbor.RunnerFunc(func(context.Context, harbor.Task) (harbor.Trial, error) {
		return harbor.Trial{Trajectory: daeval.Trajectory{Steps: []daeval.Step{{
			Action: damessage.Message{Content: make([]damessage.ContentBlock, 4)},
		}}}}, nil
	}))
	structural.MaxResultItems = 3
	result := structural.Evaluate(context.Background(), harbor.NewTask("items", "run"))
	if result.Evaluation.Errors != 1 || !strings.Contains(result.Evaluation.Results[0].Error, "structural items") {
		t.Fatalf("structural bound report = %#v", result)
	}

	panicking := harbor.NewBenchmark(harbor.RunnerFunc(func(context.Context, harbor.Task) (harbor.Trial, error) {
		panic("secret panic value")
	}))
	result = panicking.Evaluate(context.Background(), harbor.NewTask("panic", "run"))
	if result.Evaluation.Errors != 1 || result.Trials[0].FailureCategory != harbor.FailureUnknown {
		t.Fatalf("panic report = %#v", result)
	}
	if strings.Contains(result.Evaluation.Results[0].Error, "secret") {
		t.Fatalf("panic value leaked: %q", result.Evaluation.Results[0].Error)
	}
}

func TestDeterministicExampleIDsAndStatistics(t *testing.T) {
	t.Parallel()

	if got := harbor.ExampleID("  hello\n"); got != "150d5cb4-493c-88be-e674-c39eadaad07b" {
		t.Fatalf("example ID = %q", got)
	}
	if harbor.ExampleIDWithSeed("hello", 43) == harbor.ExampleID("hello") {
		t.Fatal("different seeds produced the same ID")
	}
	interval := harbor.WilsonInterval(72, 90)
	if math.Abs(interval.Lower-0.706) > 0.001 || math.Abs(interval.Upper-0.8696) > 0.001 {
		t.Fatalf("Wilson interval = %#v", interval)
	}
	effect := harbor.MinimumDetectableEffect(90)
	if math.Abs(effect-0.1461) > 0.001 {
		t.Fatalf("minimum detectable effect = %g", effect)
	}
	if got := harbor.MinimumDetectableEffect(0); got != 1 {
		t.Fatalf("empty MDE = %g", got)
	}
}

func TestRequiredConstructorsRejectMissingInputs(t *testing.T) {
	t.Parallel()

	assertPanics(t, func() { harbor.NewBenchmark(nil) })
	var typedNil *nilRunner
	assertPanics(t, func() { harbor.NewBenchmark(typedNil) })
	assertPanics(t, func() { harbor.NewTask("", "instruction") })
	assertPanics(t, func() { harbor.NewTask("name", " \n") })
	assertPanics(t, func() {
		benchmark := harbor.NewBenchmark(harbor.RunnerFunc(func(context.Context, harbor.Task) (harbor.Trial, error) {
			return harbor.Trial{}, nil
		}))
		benchmark.MaxTasks = -1
		benchmark.Evaluate(context.Background())
	})
}

type nilRunner struct{}

func (*nilRunner) Run(context.Context, harbor.Task) (harbor.Trial, error) {
	return harbor.Trial{}, nil
}

func trajectoryWithObservation(assistant, observation string) daeval.Trajectory {
	return daeval.Trajectory{Steps: []daeval.Step{{
		Index:        1,
		Action:       damessage.Assistant(assistant),
		Observations: []damessage.Message{damessage.Tool("call", observation)},
	}}}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("function did not panic")
		}
	}()
	fn()
}
