package daeval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/dagent"
)

const defaultFailureBytes = 30_000

// Run produces one trajectory. Runs should honor context cancellation.
type Run func(context.Context) (Trajectory, error)

// Invoke constructs a Run for a compiled agent. Both required inputs are
// positional; invocation failures remain runtime errors.
func Invoke(agent *dagent.Agent, input any) Run {
	if agent == nil {
		panic("evaluation agent is required")
	}
	return func(ctx context.Context) (Trajectory, error) {
		result, err := agent.Invoke(ctx, input)
		if err != nil {
			return Trajectory{}, err
		}
		return TrajectoryFromResult(result, nil), nil
	}
}

// Evaluation describes one behavioral case. Correctness checks determine pass
// or fail. Expectations are diagnostic and contribute efficiency metrics but
// never turn correct behavior into a failure.
type Evaluation struct {
	Name         string
	Category     string
	Correctness  []Check
	Expectations []Check
	Skip         string
	run          Run
}

// NewEvaluation constructs a behavioral case. The operationally required Run
// is positional; optional labels and checks remain ordinary value fields.
func NewEvaluation(run Run) Evaluation {
	if run == nil {
		panic("evaluation run is required")
	}
	return Evaluation{run: run}
}

// Harness evaluates cases sequentially in declaration order. Its zero value is
// ready to use and bounds individual failure details at 30,000 bytes. Negative
// limits are invalid and panic when evaluation begins.
type Harness struct {
	MaxFailureBytes int
}

// Status is one evaluation outcome.
type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
	StatusError   Status = "error"
)

// Finding is a failed correctness check or unmet soft expectation.
type Finding struct {
	Message string `json:"message"`
}

// Result is one evaluation's deterministic result.
type Result struct {
	Name         string    `json:"name"`
	Category     string    `json:"category"`
	Status       Status    `json:"status"`
	Steps        int       `json:"steps,omitempty"`
	ToolCalls    int       `json:"tool_calls,omitempty"`
	Failures     []Finding `json:"failures,omitempty"`
	Expectations []Finding `json:"unmet_expectations,omitempty"`
	Error        string    `json:"error,omitempty"`
	Skip         string    `json:"skip,omitempty"`
}

// Report is a stable version-1 aggregate. Errors are execution failures, not
// incorrect behavior, and therefore do not enter correctness denominators.
type Report struct {
	Version        int                `json:"version"`
	Total          int                `json:"total"`
	Passed         int                `json:"passed"`
	Failed         int                `json:"failed"`
	Skipped        int                `json:"skipped"`
	Errors         int                `json:"errors"`
	Correctness    float64            `json:"correctness"`
	CategoryScores map[string]float64 `json:"category_scores"`
	StepRatio      *float64           `json:"step_ratio,omitempty"`
	ToolCallRatio  *float64           `json:"tool_call_ratio,omitempty"`
	Results        []Result           `json:"results"`
}

// Evaluate runs evaluations sequentially and returns deterministic aggregate
// results. Wall-clock duration and creation timestamps are deliberately absent.
func (harness Harness) Evaluate(ctx context.Context, evaluations ...Evaluation) Report {
	limit := harness.MaxFailureBytes
	if limit < 0 {
		panic("evaluation maximum failure bytes must not be negative")
	}
	if limit == 0 {
		limit = defaultFailureBytes
	}
	report := Report{
		Version:        1,
		Total:          len(evaluations),
		CategoryScores: map[string]float64{},
		Results:        make([]Result, 0, len(evaluations)),
	}
	type categoryCount struct{ passed, total int }
	categories := map[string]categoryCount{}
	var expectedSteps, actualSteps, expectedCalls, actualCalls int

	for index, evaluation := range evaluations {
		name := strings.TrimSpace(evaluation.Name)
		if name == "" {
			name = fmt.Sprintf("evaluation_%d", index+1)
		}
		category := strings.TrimSpace(evaluation.Category)
		if category == "" {
			category = "uncategorized"
		}
		result := Result{Name: name, Category: category}
		if evaluation.Skip != "" {
			result.Status = StatusSkipped
			result.Skip = truncate(evaluation.Skip, limit)
			report.Skipped++
			report.Results = append(report.Results, result)
			continue
		}
		if evaluation.run == nil {
			result.Status = StatusError
			result.Error = "evaluation run is required"
			report.Errors++
			report.Results = append(report.Results, result)
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Status = StatusError
			result.Error = truncate(err.Error(), limit)
			report.Errors++
			report.Results = append(report.Results, result)
			continue
		}
		trajectory, err := evaluation.run(ctx)
		if err != nil {
			result.Status = StatusError
			result.Error = truncate(err.Error(), limit)
			report.Errors++
			report.Results = append(report.Results, result)
			continue
		}
		result.Steps = len(trajectory.Steps)
		result.ToolCalls = trajectory.ToolCallCount()
		for _, check := range evaluation.Correctness {
			if check == nil {
				result.Failures = append(result.Failures, Finding{Message: "nil correctness check"})
				continue
			}
			if passed, detail := check.Evaluate(trajectory); !passed {
				result.Failures = append(result.Failures, Finding{Message: truncate(detail+"\n\ntrajectory:\n"+trajectory.String(), limit)})
			}
		}
		for _, check := range evaluation.Expectations {
			if check == nil {
				result.Expectations = append(result.Expectations, Finding{Message: "nil expectation"})
				continue
			}
			if passed, detail := check.Evaluate(trajectory); !passed {
				result.Expectations = append(result.Expectations, Finding{Message: truncate(detail, limit)})
			}
			switch typed := check.(type) {
			case stepCountCheck:
				expectedSteps += typed.expected
				actualSteps += len(trajectory.Steps)
			case toolCallCountCheck:
				expectedCalls += typed.expected
				actualCalls += trajectory.ToolCallCount()
			}
		}
		bucket := categories[category]
		bucket.total++
		if len(result.Failures) == 0 {
			result.Status = StatusPassed
			report.Passed++
			bucket.passed++
		} else {
			result.Status = StatusFailed
			report.Failed++
		}
		categories[category] = bucket
		report.Results = append(report.Results, result)
	}

	if scored := report.Passed + report.Failed; scored > 0 {
		report.Correctness = roundRatio(report.Passed, scored)
	}
	keys := make([]string, 0, len(categories))
	for category := range categories {
		keys = append(keys, category)
	}
	sort.Strings(keys)
	for _, category := range keys {
		counts := categories[category]
		report.CategoryScores[category] = roundRatio(counts.passed, counts.total)
	}
	if expectedSteps > 0 {
		ratio := roundRatio(actualSteps, expectedSteps)
		report.StepRatio = &ratio
	}
	if expectedCalls > 0 {
		ratio := roundRatio(actualCalls, expectedCalls)
		report.ToolCallRatio = &ratio
	}
	return report
}

func roundRatio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	value := float64(numerator) / float64(denominator)
	return float64(int(value*100+0.5)) / 100
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	suffix := "\n\n... [truncated]"
	if limit <= len(suffix) {
		return suffix[:limit]
	}
	end := limit - len(suffix)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + suffix
}
