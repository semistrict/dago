// Package harbor runs sandboxed benchmarks through caller-supplied transports.
//
// The package performs no network or process execution itself. Runner is the
// authority boundary: implementations must isolate untrusted tasks and honor
// context cancellation.
package harbor

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/daeval"
	"github.com/semistrict/dago/damessage"
)

const (
	defaultMaxTasks            = 1_000
	defaultMaxInstructionBytes = 1 << 20
	defaultMaxMetadataEntries  = 128
	defaultMaxMetadataBytes    = 64 << 10
	defaultMaxChecks           = 1_000
	defaultMaxResultBytes      = 4 << 20
	defaultMaxFailureBytes     = 30_000
	defaultMaxResultItems      = 100_000
	defaultMaxSteps            = 1_000
	defaultMaxToolCalls        = 10_000
	defaultMaxObservations     = 10_000
	defaultTrialTimeout        = 10 * time.Minute
	defaultBenchmarkTimeout    = time.Hour
	defaultMinimumReward       = 1.0
	defaultConfidenceZ         = 1.96
)

// Runner executes a task in a sandbox. Implementations must honor ctx and must
// not expose host credentials or files to the task unless explicitly intended.
type Runner interface {
	Run(context.Context, Task) (Trial, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(context.Context, Task) (Trial, error)

// Run implements Runner.
func (run RunnerFunc) Run(ctx context.Context, task Task) (Trial, error) {
	return run(ctx, task)
}

// Task is one sandbox benchmark case. Name and Instruction are required; use
// NewTask to construct values. Checks supplement the sandbox verifier reward.
type Task struct {
	Name         string
	Instruction  string
	Category     string
	Metadata     map[string]string
	Correctness  []daeval.Check
	Expectations []daeval.Check
	Skip         string
}

// NewTask constructs a task from its required name and instruction.
func NewTask(name, instruction string) Task {
	if strings.TrimSpace(name) == "" {
		panic("Harbor task name is required")
	}
	if strings.TrimSpace(instruction) == "" {
		panic("Harbor task instruction is required")
	}
	return Task{Name: name, Instruction: instruction}
}

// Trial is the bounded, provider-neutral result returned by a Runner. Reward
// should point to the verifier's score. A missing reward is treated as zero.
type Trial struct {
	Trajectory daeval.Trajectory
	Reward     *float64
	Exception  string
	ExitCodes  []int
}

// Benchmark executes tasks sequentially. NewBenchmark supplies useful bounded
// defaults; exported fields may be adjusted before Evaluate.
type Benchmark struct {
	runner Runner

	MaxTasks            int
	MaxInstructionBytes int
	MaxMetadataEntries  int
	MaxMetadataBytes    int
	MaxChecks           int
	MaxResultBytes      int
	MaxFailureBytes     int
	MaxResultItems      int
	MaxSteps            int
	MaxToolCalls        int
	MaxObservations     int
	TrialTimeout        time.Duration
	Timeout             time.Duration
	MinimumReward       float64
}

// NewBenchmark constructs a benchmark around the required sandbox runner.
// Construction cannot fail; a nil runner is a programmer error and panics.
func NewBenchmark(runner Runner) Benchmark {
	if isNil(runner) {
		panic("Harbor runner is required")
	}
	return Benchmark{runner: runner}
}

// FailureCategory distinguishes capability failures from infrastructure noise.
type FailureCategory string

const (
	FailureCapability   FailureCategory = "capability"
	FailureInfraOOM     FailureCategory = "infra_oom"
	FailureInfraTimeout FailureCategory = "infra_timeout"
	FailureInfraSandbox FailureCategory = "infra_sandbox"
	FailureUnknown      FailureCategory = "unknown"
)

// Infrastructure reports whether the category is environmental rather than a
// model or agent capability failure.
func (category FailureCategory) Infrastructure() bool {
	return category == FailureInfraOOM || category == FailureInfraTimeout || category == FailureInfraSandbox
}

// TrialResult adds benchmark reward and attribution to a daeval result.
type TrialResult struct {
	Name            string          `json:"name"`
	Reward          float64         `json:"reward"`
	RewardPresent   bool            `json:"reward_present"`
	FailureCategory FailureCategory `json:"failure_category,omitempty"`
	Infrastructure  bool            `json:"infrastructure_failure,omitempty"`
}

// Interval is a Wilson score confidence interval.
type Interval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// Report combines the existing deterministic behavioral report with Harbor
// reward, failure attribution, and noise-aware statistics.
type Report struct {
	Version                 int           `json:"version"`
	Evaluation              daeval.Report `json:"evaluation"`
	Trials                  []TrialResult `json:"trials"`
	OmittedTasks            int           `json:"omitted_tasks,omitempty"`
	InfrastructureFailures  int           `json:"infrastructure_failures"`
	CapabilityFailures      int           `json:"capability_failures"`
	UnknownFailures         int           `json:"unknown_failures"`
	Confidence95            Interval      `json:"confidence_95"`
	MinimumDetectableEffect float64       `json:"minimum_detectable_effect"`
}

type completedTrial struct {
	trial    Trial
	category FailureCategory
	err      error
}

// Evaluate runs tasks in declaration order and returns a deterministic report.
// Runner implementations must honor cancellation; the harness supplies both a
// benchmark deadline and a per-trial deadline.
func (benchmark Benchmark) Evaluate(ctx context.Context, tasks ...Task) Report {
	limits := benchmark.withDefaults()
	omitted := 0
	if len(tasks) > limits.MaxTasks {
		omitted = len(tasks) - limits.MaxTasks
		tasks = tasks[:limits.MaxTasks]
	}

	benchmarkCtx, cancelBenchmark := context.WithTimeout(ctx, limits.Timeout)
	defer cancelBenchmark()

	evaluations := make([]daeval.Evaluation, 0, len(tasks))
	completed := make([]completedTrial, 0, len(tasks))
	for _, task := range tasks {
		task = cloneTask(task)
		result := completedTrial{}
		evaluationSkip := task.Skip
		if err := limits.validateTask(task); err != nil {
			result.err = err
			result.category = FailureUnknown
			evaluationSkip = ""
		} else if task.Skip == "" {
			if err := benchmarkCtx.Err(); err != nil {
				result.err = err
				result.category = ClassifyFailure(err.Error())
			} else {
				runnerTask := task
				runnerTask.Correctness = nil
				runnerTask.Expectations = nil
				trialCtx, cancelTrial := context.WithTimeout(benchmarkCtx, limits.TrialTimeout)
				result.trial, result.err = invokeRunner(limits.runner, trialCtx, runnerTask)
				trialContextError := trialCtx.Err()
				cancelTrial()
				if result.err == nil && trialContextError != nil {
					result.err = trialContextError
				}
				if result.err != nil {
					result.category = ClassifyFailure(result.err.Error(), result.trial.ExitCodes...)
				} else if err := limits.validateTrial(result.trial); err != nil {
					result.err = err
					result.category = FailureUnknown
				} else {
					exitCodes := append([]int(nil), result.trial.ExitCodes...)
					exitCodes = append(exitCodes, ExtractExitCodes(result.trial.Trajectory)...)
					result.category = ClassifyFailure(result.trial.Exception, exitCodes...)
					if result.trial.Exception != "" || result.category.Infrastructure() {
						result.err = fmt.Errorf("Harbor trial failed: %s", truncate(result.trial.Exception, 4_096))
						if result.trial.Exception == "" {
							result.err = fmt.Errorf("Harbor trial failed with %s", result.category)
						}
					}
				}
			}
		}
		completed = append(completed, result)

		current := result
		evaluation := daeval.NewEvaluation(func(context.Context) (daeval.Trajectory, error) {
			if current.err != nil {
				return daeval.Trajectory{}, current.err
			}
			return current.trial.Trajectory, nil
		})
		evaluation.Name = task.Name
		evaluation.Category = task.Category
		evaluation.Skip = evaluationSkip
		evaluation.Correctness = append([]daeval.Check{minimumRewardCheck{
			reward:  rewardValue(result.trial.Reward),
			minimum: limits.MinimumReward,
		}}, task.Correctness...)
		evaluation.Expectations = append([]daeval.Check(nil), task.Expectations...)
		evaluations = append(evaluations, evaluation)
	}

	evaluationReport := (daeval.Harness{MaxFailureBytes: limits.MaxFailureBytes}).Evaluate(context.Background(), evaluations...)
	report := Report{
		Version:      1,
		Evaluation:   evaluationReport,
		Trials:       make([]TrialResult, 0, len(completed)),
		OmittedTasks: omitted,
	}
	for index, result := range completed {
		trialResult := TrialResult{
			Name:          evaluationReport.Results[index].Name,
			Reward:        rewardValue(result.trial.Reward),
			RewardPresent: result.trial.Reward != nil,
		}
		status := evaluationReport.Results[index].Status
		if status == daeval.StatusFailed {
			trialResult.FailureCategory = FailureCapability
			report.CapabilityFailures++
		} else if status == daeval.StatusError {
			trialResult.FailureCategory = result.category
			trialResult.Infrastructure = result.category.Infrastructure()
			if trialResult.Infrastructure {
				report.InfrastructureFailures++
			} else {
				report.UnknownFailures++
			}
		}
		report.Trials = append(report.Trials, trialResult)
	}
	scored := evaluationReport.Passed + evaluationReport.Failed
	report.Confidence95 = WilsonInterval(evaluationReport.Passed, scored)
	report.MinimumDetectableEffect = MinimumDetectableEffect(scored)
	return report
}

func (benchmark Benchmark) withDefaults() Benchmark {
	benchmark.MaxTasks = positiveOrDefault(benchmark.MaxTasks, defaultMaxTasks, "maximum tasks")
	benchmark.MaxInstructionBytes = positiveOrDefault(benchmark.MaxInstructionBytes, defaultMaxInstructionBytes, "maximum task text bytes")
	benchmark.MaxMetadataEntries = positiveOrDefault(benchmark.MaxMetadataEntries, defaultMaxMetadataEntries, "maximum metadata entries")
	benchmark.MaxMetadataBytes = positiveOrDefault(benchmark.MaxMetadataBytes, defaultMaxMetadataBytes, "maximum metadata bytes")
	benchmark.MaxChecks = positiveOrDefault(benchmark.MaxChecks, defaultMaxChecks, "maximum checks")
	benchmark.MaxResultBytes = positiveOrDefault(benchmark.MaxResultBytes, defaultMaxResultBytes, "maximum result bytes")
	benchmark.MaxFailureBytes = positiveOrDefault(benchmark.MaxFailureBytes, defaultMaxFailureBytes, "maximum failure bytes")
	benchmark.MaxResultItems = positiveOrDefault(benchmark.MaxResultItems, defaultMaxResultItems, "maximum result items")
	benchmark.MaxSteps = positiveOrDefault(benchmark.MaxSteps, defaultMaxSteps, "maximum steps")
	benchmark.MaxToolCalls = positiveOrDefault(benchmark.MaxToolCalls, defaultMaxToolCalls, "maximum tool calls")
	benchmark.MaxObservations = positiveOrDefault(benchmark.MaxObservations, defaultMaxObservations, "maximum observations")
	benchmark.TrialTimeout = durationOrDefault(benchmark.TrialTimeout, defaultTrialTimeout, "trial timeout")
	benchmark.Timeout = durationOrDefault(benchmark.Timeout, defaultBenchmarkTimeout, "benchmark timeout")
	if benchmark.MinimumReward == 0 {
		benchmark.MinimumReward = defaultMinimumReward
	}
	if math.IsNaN(benchmark.MinimumReward) || math.IsInf(benchmark.MinimumReward, 0) {
		panic("Harbor minimum reward must be finite")
	}
	return benchmark
}

func positiveOrDefault(value, fallback int, name string) int {
	if value < 0 {
		panic("Harbor " + name + " must not be negative")
	}
	if value == 0 {
		return fallback
	}
	return value
}

func durationOrDefault(value, fallback time.Duration, name string) time.Duration {
	if value < 0 {
		panic("Harbor " + name + " must not be negative")
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

func (benchmark Benchmark) validateTask(task Task) error {
	if strings.TrimSpace(task.Name) == "" {
		return fmt.Errorf("Harbor task name is required")
	}
	if strings.TrimSpace(task.Instruction) == "" {
		return fmt.Errorf("Harbor task instruction is required")
	}
	if !utf8.ValidString(task.Name) || !utf8.ValidString(task.Category) || !utf8.ValidString(task.Instruction) || !utf8.ValidString(task.Skip) {
		return fmt.Errorf("Harbor task text must be valid UTF-8")
	}
	if len(task.Name)+len(task.Category)+len(task.Instruction)+len(task.Skip) > benchmark.MaxInstructionBytes {
		return fmt.Errorf("Harbor task text exceeds %d bytes", benchmark.MaxInstructionBytes)
	}
	if len(task.Metadata) > benchmark.MaxMetadataEntries {
		return fmt.Errorf("Harbor metadata exceeds %d entries", benchmark.MaxMetadataEntries)
	}
	bytes := 0
	for key, value := range task.Metadata {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return fmt.Errorf("Harbor metadata must be valid UTF-8")
		}
		bytes += len(key) + len(value)
		if bytes > benchmark.MaxMetadataBytes {
			return fmt.Errorf("Harbor metadata exceeds %d bytes", benchmark.MaxMetadataBytes)
		}
	}
	if len(task.Correctness)+len(task.Expectations) > benchmark.MaxChecks {
		return fmt.Errorf("Harbor task exceeds %d checks", benchmark.MaxChecks)
	}
	return nil
}

func (benchmark Benchmark) validateTrial(trial Trial) error {
	if trial.Reward != nil && (math.IsNaN(*trial.Reward) || math.IsInf(*trial.Reward, 0)) {
		return fmt.Errorf("Harbor reward must be finite")
	}
	if len(trial.Trajectory.Steps) > benchmark.MaxSteps {
		return fmt.Errorf("Harbor trajectory exceeds %d steps", benchmark.MaxSteps)
	}
	toolCalls, observations, bytes, items := 0, 0, len(trial.Exception), len(trial.ExitCodes)
	for _, step := range trial.Trajectory.Steps {
		toolCalls += len(step.Action.ToolCalls)
		observations += len(step.Observations)
		items += 1 + messageItems(step.Action)
		if toolCalls > benchmark.MaxToolCalls {
			return fmt.Errorf("Harbor trajectory exceeds %d tool calls", benchmark.MaxToolCalls)
		}
		if observations > benchmark.MaxObservations {
			return fmt.Errorf("Harbor trajectory exceeds %d observations", benchmark.MaxObservations)
		}
		bytes += messageBytes(step.Action)
		for _, observation := range step.Observations {
			bytes += messageBytes(observation)
			items += 1 + messageItems(observation)
		}
		if bytes > benchmark.MaxResultBytes || items > benchmark.MaxResultItems {
			if items > benchmark.MaxResultItems {
				return fmt.Errorf("Harbor result exceeds %d structural items", benchmark.MaxResultItems)
			}
			return fmt.Errorf("Harbor result exceeds %d bytes", benchmark.MaxResultBytes)
		}
	}
	for path, content := range trial.Trajectory.Files {
		bytes += len(path) + len(content)
		items++
		if bytes > benchmark.MaxResultBytes || items > benchmark.MaxResultItems {
			if items > benchmark.MaxResultItems {
				return fmt.Errorf("Harbor result exceeds %d structural items", benchmark.MaxResultItems)
			}
			return fmt.Errorf("Harbor result exceeds %d bytes", benchmark.MaxResultBytes)
		}
	}
	return nil
}

func messageItems(message damessage.Message) int {
	items := len(message.Content) + len(message.ToolCalls) + len(message.InvalidToolCalls) + len(message.Metadata) + len(message.ResponseMetadata) + len(message.OtherUsage)
	for _, block := range message.Content {
		items += len(block.Citations) + len(block.Extra)
	}
	if message.Usage != nil {
		items += len(message.Usage.InputDetails) + len(message.Usage.OutputDetails)
	}
	for _, usage := range message.OtherUsage {
		items += len(usage.InputDetails) + len(usage.OutputDetails)
	}
	return items
}

func invokeRunner(runner Runner, ctx context.Context, task Task) (trial Trial, err error) {
	defer func() {
		if recover() != nil {
			trial = Trial{}
			err = fmt.Errorf("Harbor runner panicked")
		}
	}()
	return runner.Run(ctx, task)
}

func messageBytes(message damessage.Message) int {
	bytes := len(message.ID) + len(message.Name) + len(message.ToolCallID) + len(message.Artifact)
	for _, block := range message.Content {
		bytes += len(block.ID) + len(block.Text) + len(block.Reasoning) + len(block.URL) + len(block.Data) + len(block.MIMEType) + len(block.Name) + len(block.NonStandard)
		for _, citation := range block.Citations {
			bytes += len(citation.ID) + len(citation.URL) + len(citation.Title) + len(citation.CitedText)
		}
		for key, value := range block.Extra {
			bytes += len(key) + len(value)
		}
	}
	for _, call := range message.ToolCalls {
		bytes += len(call.ID) + len(call.Name) + len(call.Arguments)
	}
	for _, call := range message.InvalidToolCalls {
		bytes += len(call.ID) + len(call.Name) + len(call.Arguments) + len(call.Error)
	}
	for key, value := range message.Metadata {
		bytes += len(key) + len(value)
	}
	for key, value := range message.ResponseMetadata {
		bytes += len(key) + len(value)
	}
	if message.Usage != nil {
		bytes += usageBytes(message.Usage.Provider, message.Usage.Model, message.Usage.URL, message.Usage.InputDetails, message.Usage.OutputDetails)
	}
	for _, usage := range message.OtherUsage {
		bytes += len(usage.Purpose) + usageBytes(usage.Provider, usage.Model, usage.URL, usage.InputDetails, usage.OutputDetails)
	}
	return bytes
}

func usageBytes(provider, model, url string, input, output map[string]int) int {
	bytes := len(provider) + len(model) + len(url)
	for key := range input {
		bytes += len(key)
	}
	for key := range output {
		bytes += len(key)
	}
	return bytes
}

func cloneTask(task Task) Task {
	copy := task
	copy.Metadata = make(map[string]string, len(task.Metadata))
	for key, value := range task.Metadata {
		copy.Metadata[key] = value
	}
	copy.Correctness = append([]daeval.Check(nil), task.Correctness...)
	copy.Expectations = append([]daeval.Check(nil), task.Expectations...)
	return copy
}

type minimumRewardCheck struct {
	reward  float64
	minimum float64
}

func (check minimumRewardCheck) Evaluate(daeval.Trajectory) (bool, string) {
	if check.reward >= check.minimum {
		return true, ""
	}
	return false, fmt.Sprintf("Harbor reward is %g, want at least %g", check.reward, check.minimum)
}

func rewardValue(reward *float64) float64 {
	if reward == nil || math.IsNaN(*reward) || math.IsInf(*reward, 0) {
		return 0
	}
	return *reward
}

var exitCodePattern = regexp.MustCompile(`(?i)exit(?:_|-|\s)code["\s:]+([0-9]+)`)

// ExtractExitCodes structurally searches only tool observations, never model
// output, avoiding false infrastructure attribution from assistant prose.
func ExtractExitCodes(trajectory daeval.Trajectory) []int {
	var codes []int
	for _, step := range trajectory.Steps {
		for _, observation := range step.Observations {
			for _, match := range exitCodePattern.FindAllStringSubmatch(observation.TextContent(), -1) {
				code, err := strconv.Atoi(match[1])
				if err == nil && code != 0 {
					codes = append(codes, code)
				}
			}
		}
	}
	return codes
}

var (
	oomPatterns     = []string{"oomkilled", "out of memory", "cannot allocate memory", "memory allocation failed", "signal 9", "sigkill", "exit code 137"}
	timeoutPatterns = []string{"timed out", "deadline exceeded", "exit code 124"}
	sandboxPatterns = []string{"sandbox crashed", "sandbox exited unexpectedly", "sandbox error", "sandbox failure", "connection refused", "connection reset", "broken pipe", "network unreachable", "no route to host", "exec failed"}
)

// ClassifyFailure attributes a failed trial. Exit codes take precedence over
// controlled exception text; trajectory model output is never pattern-matched.
func ClassifyFailure(exception string, exitCodes ...int) FailureCategory {
	for _, code := range exitCodes {
		if code == 137 {
			return FailureInfraOOM
		}
		if code == 124 {
			return FailureInfraTimeout
		}
	}
	if exception == "" {
		return FailureCapability
	}
	lower := strings.ToLower(exception)
	if containsAny(lower, oomPatterns) {
		return FailureInfraOOM
	}
	if containsAny(lower, timeoutPatterns) {
		return FailureInfraTimeout
	}
	if containsAny(lower, sandboxPatterns) {
		return FailureInfraSandbox
	}
	return FailureUnknown
}

func containsAny(text string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

// ExampleID returns the deterministic UUID-shaped ID used for task
// instructions with the upstream-compatible default seed of 42.
func ExampleID(instruction string) string {
	return ExampleIDWithSeed(instruction, 42)
}

// ExampleIDWithSeed returns a deterministic task ID for instruction and seed.
func ExampleIDWithSeed(instruction string, seed uint64) string {
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], seed)
	hash := sha256.New()
	_, _ = hash.Write(prefix[:])
	_, _ = hash.Write([]byte(strings.TrimSpace(instruction)))
	digest := hash.Sum(nil)
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

// WilsonInterval computes a 95% binomial Wilson score interval.
func WilsonInterval(successes, total int) Interval {
	return WilsonIntervalAtZ(successes, total, defaultConfidenceZ)
}

// WilsonIntervalAtZ computes a binomial Wilson score interval for a positive
// finite z-score.
func WilsonIntervalAtZ(successes, total int, z float64) Interval {
	if successes < 0 || total < 0 || successes > total {
		panic("invalid success counts")
	}
	if z <= 0 || math.IsNaN(z) || math.IsInf(z, 0) {
		panic("Wilson z-score must be finite and positive")
	}
	if total == 0 {
		return Interval{}
	}
	p := float64(successes) / float64(total)
	z2 := z * z
	denominator := 1 + z2/float64(total)
	center := (p + z2/(2*float64(total))) / denominator
	margin := (z / denominator) * math.Sqrt(p*(1-p)/float64(total)+z2/(4*float64(total*total)))
	return Interval{Lower: math.Max(0, center-margin), Upper: math.Min(1, center+margin)}
}

// MinimumDetectableEffect estimates a conservative 95% two-run binomial effect
// size using p=0.5. Empty runs return one.
func MinimumDetectableEffect(total int) float64 {
	return MinimumDetectableEffectAt(total, defaultConfidenceZ, 0.5)
}

// MinimumDetectableEffectAt estimates a two-run binomial effect size. z must
// be positive and p must be within [0,1]. Empty runs return one.
func MinimumDetectableEffectAt(total int, z, p float64) float64 {
	if total < 0 {
		panic("trial count must not be negative")
	}
	if z <= 0 || math.IsNaN(z) || math.IsInf(z, 0) {
		panic("effect z-score must be finite and positive")
	}
	if p < 0 || p > 1 || math.IsNaN(p) {
		panic("base proportion must be within [0,1]")
	}
	if total == 0 {
		return 1
	}
	return z * math.Sqrt(2*p*(1-p)/float64(total))
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "... [truncated]"
}
