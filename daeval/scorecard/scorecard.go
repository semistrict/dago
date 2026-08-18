// Package scorecard compares provider-neutral evaluation results across models.
//
// Model execution is entirely caller-supplied. The package performs no network,
// credential lookup, hosted reporting, or provider SDK initialization.
package scorecard

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/dago/daeval"
	"github.com/semistrict/dago/daeval/harbor"
)

const (
	defaultMaxModels    = 16
	defaultMaxTasks     = 1_000
	defaultMaxRuns      = 10_000
	defaultTimeout      = 4 * time.Hour
	defaultModelTimeout = time.Hour
	defaultTrialTimeout = 10 * time.Minute
)

var (
	modelIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	modelProviderPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{0,128}$`)
)

// Model is a public, non-secret scorecard identity. Runner implementations
// resolve credentials and provider configuration out of band using ID.
type Model struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
}

// NewModel constructs a model from its required stable ID. Invalid IDs panic.
func NewModel(id string) Model {
	if !modelIDPattern.MatchString(id) {
		panic("scorecard model ID has an unsafe form")
	}
	return Model{ID: id}
}

// Runner executes one already-adapted Harbor task for one model. Implementations
// must honor ctx and keep credentials outside Model, Task, Trial, and reports.
type Runner interface {
	Run(context.Context, Model, harbor.Task) (harbor.Trial, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(context.Context, Model, harbor.Task) (harbor.Trial, error)

// Run implements Runner.
func (run RunnerFunc) Run(ctx context.Context, model Model, task harbor.Task) (harbor.Trial, error) {
	return run(ctx, model, task)
}

// Scorecard compares the same ordered task set across an ordered model set. Use
// New to provide the required runner and at least one model. Zero-valued limits
// receive bounded defaults during Evaluate.
type Scorecard struct {
	runner Runner
	models []Model

	MaxModels     int
	MaxTasks      int
	MaxRuns       int
	Timeout       time.Duration
	ModelTimeout  time.Duration
	TrialTimeout  time.Duration
	MinimumReward float64
}

// New constructs a scorecard with a required runner and first model. Additional
// models are optional. Invalid or duplicate static model identities panic.
func New(runner Runner, model Model, additionalModels ...Model) Scorecard {
	if isNil(runner) {
		panic("scorecard runner is required")
	}
	models := append([]Model{model}, additionalModels...)
	validateModels(models)
	return Scorecard{runner: runner, models: append([]Model(nil), models...)}
}

// Summary is one deterministic scored aggregate.
type Summary struct {
	Total                   int             `json:"total"`
	Passed                  int             `json:"passed"`
	Failed                  int             `json:"failed"`
	Skipped                 int             `json:"skipped"`
	Errors                  int             `json:"errors"`
	Correctness             float64         `json:"correctness"`
	Confidence95            harbor.Interval `json:"confidence_95"`
	MinimumDetectableEffect float64         `json:"minimum_detectable_effect"`
}

// ModelResult is one model's complete Harbor report.
type ModelResult struct {
	Model  Model         `json:"model"`
	Report harbor.Report `json:"report"`
}

// CategoryModelScore is one model's result within one category.
type CategoryModelScore struct {
	ModelID     string  `json:"model_id"`
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
	Skipped     int     `json:"skipped"`
	Errors      int     `json:"errors"`
	Correctness float64 `json:"correctness"`
}

// CategoryScore compares all models for one category.
type CategoryScore struct {
	Category        string               `json:"category"`
	MeanCorrectness float64              `json:"mean_correctness"`
	Models          []CategoryModelScore `json:"models"`
}

// Standing is one deterministic leaderboard row.
type Standing struct {
	Rank        int     `json:"rank"`
	ModelID     string  `json:"model_id"`
	Correctness float64 `json:"correctness"`
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
	Errors      int     `json:"errors"`
}

// Report is a stable version-1 cross-model scorecard. It contains no current
// time, elapsed durations, credentials, or hosted experiment identifiers.
type Report struct {
	Version         int             `json:"version"`
	RequestedModels int             `json:"requested_models"`
	RequestedTasks  int             `json:"requested_tasks"`
	Runs            int             `json:"runs"`
	Error           string          `json:"error,omitempty"`
	Overall         Summary         `json:"overall"`
	Models          []ModelResult   `json:"models"`
	Categories      []CategoryScore `json:"categories"`
	Leaderboard     []Standing      `json:"leaderboard"`
}

// Evaluate executes every task for every model in model-major declaration order.
// Invalid work limits produce a deterministic report error before any runner call.
func (scorecard Scorecard) Evaluate(ctx context.Context, tasks ...harbor.Task) Report {
	limits := scorecard.withDefaults()
	report := Report{
		Version:         1,
		RequestedModels: len(limits.models),
		RequestedTasks:  len(tasks),
		Models:          []ModelResult{},
		Categories:      []CategoryScore{},
		Leaderboard:     []Standing{},
	}
	if err := limits.validateWork(len(tasks)); err != nil {
		report.Error = err.Error()
		report.Overall = summarize(0, 0, 0, 0)
		return report
	}

	scorecardCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	report.Runs = len(limits.models) * len(tasks)
	report.Models = make([]ModelResult, 0, len(limits.models))
	for _, model := range limits.models {
		model := model
		benchmark := harbor.NewBenchmark(modelRunner{runner: limits.runner, model: model})
		benchmark.MaxTasks = limits.MaxTasks
		benchmark.Timeout = limits.ModelTimeout
		benchmark.TrialTimeout = limits.TrialTimeout
		benchmark.MinimumReward = limits.MinimumReward
		modelReport := benchmark.Evaluate(scorecardCtx, tasks...)
		report.Models = append(report.Models, ModelResult{Model: model, Report: modelReport})
	}
	report.Overall = overallSummary(report.Models)
	report.Categories = categoryScores(report.Models)
	report.Leaderboard = leaderboard(report.Models)
	return report
}

type modelRunner struct {
	runner Runner
	model  Model
}

func (runner modelRunner) Run(ctx context.Context, task harbor.Task) (harbor.Trial, error) {
	return runner.runner.Run(ctx, runner.model, task)
}

func (scorecard Scorecard) withDefaults() Scorecard {
	scorecard.MaxModels = positiveOrDefault(scorecard.MaxModels, defaultMaxModels, "maximum models")
	scorecard.MaxTasks = positiveOrDefault(scorecard.MaxTasks, defaultMaxTasks, "maximum tasks")
	scorecard.MaxRuns = positiveOrDefault(scorecard.MaxRuns, defaultMaxRuns, "maximum runs")
	scorecard.Timeout = durationOrDefault(scorecard.Timeout, defaultTimeout, "timeout")
	scorecard.ModelTimeout = durationOrDefault(scorecard.ModelTimeout, defaultModelTimeout, "model timeout")
	scorecard.TrialTimeout = durationOrDefault(scorecard.TrialTimeout, defaultTrialTimeout, "trial timeout")
	if math.IsNaN(scorecard.MinimumReward) || math.IsInf(scorecard.MinimumReward, 0) {
		panic("scorecard minimum reward must be finite")
	}
	validateModels(scorecard.models)
	return scorecard
}

func (scorecard Scorecard) validateWork(tasks int) error {
	if len(scorecard.models) > scorecard.MaxModels {
		return fmt.Errorf("scorecard requires %d models, limit is %d", len(scorecard.models), scorecard.MaxModels)
	}
	if tasks > scorecard.MaxTasks {
		return fmt.Errorf("scorecard requires %d tasks, limit is %d", tasks, scorecard.MaxTasks)
	}
	if tasks != 0 && len(scorecard.models) > scorecard.MaxRuns/tasks {
		return fmt.Errorf("scorecard requires %d runs, limit is %d", len(scorecard.models)*tasks, scorecard.MaxRuns)
	}
	return nil
}

func validateModels(models []Model) {
	if len(models) == 0 {
		panic("scorecard requires at least one model")
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if !modelIDPattern.MatchString(model.ID) {
			panic("scorecard model ID has an unsafe form")
		}
		if !modelProviderPattern.MatchString(model.Provider) {
			panic("scorecard model provider has an unsafe form")
		}
		if _, exists := seen[model.ID]; exists {
			panic("scorecard model IDs must be unique")
		}
		seen[model.ID] = struct{}{}
	}
}

func overallSummary(models []ModelResult) Summary {
	var passed, failed, skipped, errors int
	for _, model := range models {
		evaluation := model.Report.Evaluation
		passed += evaluation.Passed
		failed += evaluation.Failed
		skipped += evaluation.Skipped
		errors += evaluation.Errors
	}
	return summarize(passed, failed, skipped, errors)
}

func summarize(passed, failed, skipped, errors int) Summary {
	scored := passed + failed
	summary := Summary{
		Total:                   scored + skipped + errors,
		Passed:                  passed,
		Failed:                  failed,
		Skipped:                 skipped,
		Errors:                  errors,
		Confidence95:            harbor.WilsonInterval(passed, scored),
		MinimumDetectableEffect: harbor.MinimumDetectableEffect(scored),
	}
	if scored > 0 {
		summary.Correctness = roundRatio(passed, scored)
	}
	return summary
}

type categoryCounts struct {
	passed, failed, skipped, errors int
}

func categoryScores(models []ModelResult) []CategoryScore {
	byCategory := map[string]map[string]categoryCounts{}
	for _, model := range models {
		for _, result := range model.Report.Evaluation.Results {
			category := strings.TrimSpace(result.Category)
			if category == "" {
				category = "uncategorized"
			}
			if byCategory[category] == nil {
				byCategory[category] = map[string]categoryCounts{}
			}
			counts := byCategory[category][model.Model.ID]
			switch result.Status {
			case daeval.StatusPassed:
				counts.passed++
			case daeval.StatusFailed:
				counts.failed++
			case daeval.StatusSkipped:
				counts.skipped++
			case daeval.StatusError:
				counts.errors++
			}
			byCategory[category][model.Model.ID] = counts
		}
	}
	categories := make([]string, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	results := make([]CategoryScore, 0, len(categories))
	for _, category := range categories {
		result := CategoryScore{Category: category, Models: make([]CategoryModelScore, 0, len(models))}
		totalCorrectness := 0.0
		scoredModels := 0
		for _, model := range models {
			counts := byCategory[category][model.Model.ID]
			scored := counts.passed + counts.failed
			correctness := 0.0
			if scored > 0 {
				correctness = roundRatio(counts.passed, scored)
				totalCorrectness += correctness
				scoredModels++
			}
			result.Models = append(result.Models, CategoryModelScore{
				ModelID: model.Model.ID, Passed: counts.passed, Failed: counts.failed,
				Skipped: counts.skipped, Errors: counts.errors, Correctness: correctness,
			})
		}
		if scoredModels > 0 {
			result.MeanCorrectness = roundFloat(totalCorrectness / float64(scoredModels))
		}
		results = append(results, result)
	}
	return results
}

func leaderboard(models []ModelResult) []Standing {
	standings := make([]Standing, 0, len(models))
	for _, model := range models {
		evaluation := model.Report.Evaluation
		standings = append(standings, Standing{
			ModelID: model.Model.ID, Correctness: evaluation.Correctness,
			Passed: evaluation.Passed, Failed: evaluation.Failed, Errors: evaluation.Errors,
		})
	}
	sort.SliceStable(standings, func(i, j int) bool {
		left, right := standings[i], standings[j]
		if left.Correctness != right.Correctness {
			return left.Correctness > right.Correctness
		}
		if left.Passed != right.Passed {
			return left.Passed > right.Passed
		}
		if left.Errors != right.Errors {
			return left.Errors < right.Errors
		}
		return left.ModelID < right.ModelID
	})
	for index := range standings {
		standings[index].Rank = index + 1
	}
	return standings
}

func roundRatio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return roundFloat(float64(numerator) / float64(denominator))
}

func roundFloat(value float64) float64 {
	return math.Round(value*100) / 100
}

func positiveOrDefault(value, fallback int, name string) int {
	if value < 0 {
		panic("scorecard " + name + " must not be negative")
	}
	if value == 0 {
		return fallback
	}
	return value
}

func durationOrDefault(value, fallback time.Duration, name string) time.Duration {
	if value < 0 {
		panic("scorecard " + name + " must not be negative")
	}
	if value == 0 {
		return fallback
	}
	return value
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
