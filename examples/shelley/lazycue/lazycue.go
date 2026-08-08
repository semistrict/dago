// Package lazycue implements self-healing browser tests written as plain English.
//
// A test is a description string. The system hashes the description to look up a
// cached DSL test script stored as a JSON file in a .lazycue/ directory next to
// the tests. If cached, it executes the DSL. If the test passes, it's done. If
// it fails (or isn't cached), an LLM agent generates or fixes the DSL, and the
// new version is written back to the cache file.
package lazycue

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options configures a lazycue test run.
type Options struct {
	BaseURL          string // Base URL of the application under test (required)
	CacheDir         string // Directory holding cache JSON files (default: ".lazycue")
	Model            string // LLM model (default: "claude-sonnet-4-6")
	AnthropicBaseURL string // Anthropic API base URL (default: ANTHROPIC_BASE_URL or https://api.anthropic.com)
	AnthropicAPIKey  string // Anthropic API key (default: ANTHROPIC_API_KEY)
	Verbose          bool   // Verbose output
	ArtifactDir      string // If set, write per-step screenshots here and record their paths on StepResults
}

func (o *Options) defaults() {
	if o.CacheDir == "" {
		o.CacheDir = ".lazycue"
	}
	if o.Model == "" {
		o.Model = "claude-sonnet-4-6"
	}
	if o.AnthropicBaseURL == "" {
		if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" {
			o.AnthropicBaseURL = v
		} else {
			o.AnthropicBaseURL = "https://api.anthropic.com"
		}
	}
	if o.AnthropicAPIKey == "" {
		o.AnthropicAPIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
}

// perTestBudget bounds the whole resolution of a single lazycue test (cached
// execution + one retry + an optional LLM heal). It is kept under the `go test`
// package deadline (10m default) so that a single stuck test fails cleanly via
// t.Fatal rather than letting the package time out and panic, which would abort
// every other test in the package.
//
// It is set comfortably above agentBudget (the heal budget) rather than just
// above it: the heal does not start at t=0 — the cache check, browser launch,
// cached execution, and the one-shot retry all run under this same budget
// first, and under the flaky-long-wait conditions that trigger heals that
// pre-heal work (executed twice, with long wait timeouts) can consume a
// minute or more. The margin here ensures a legitimate heal still gets close
// to its full agentBudget while staying under the package deadline.
const perTestBudget = 8 * time.Minute

// RunMode describes how the test was resolved.
type RunMode string

const (
	RunModeCached    RunMode = "cached"    // test ran from cache
	RunModeGenerated RunMode = "generated" // agent generated fresh
	RunModeHealed    RunMode = "healed"    // agent fixed a cached test
)

// TestResult is the result of running a lazy test.
type TestResult struct {
	Pass           bool
	Error          string
	ScreenshotPath string
	Steps          []StepResult
	CacheVersion   int
	Description    string
	Mode           RunMode
	TotalDuration  time.Duration // wall-clock time for the entire test
	AgentDuration  time.Duration // time spent in the LLM agent (0 if cached)
	InputTokens    int           // total input tokens used by agent (0 if cached)
	OutputTokens   int           // total output tokens used by agent (0 if cached)
	EstimatedCost  float64       // estimated USD cost
}

// StepResult is the result of executing a single DSL step.
type StepResult struct {
	Action     string        `json:"action"`
	Summary    string        `json:"summary"` // e.g. "click #login-button"
	Pass       bool          `json:"pass"`
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"duration"`
	Screenshot string        `json:"screenshot,omitempty"` // path to PNG captured after this step (if enabled)
	Output     string        `json:"output,omitempty"`     // diagnostic output (e.g. the value returned by an eval step)
}

// Run executes a single lazy test described by the given plain-English description.
func Run(ctx context.Context, opts Options, description string) (*TestResult, error) {
	totalStart := time.Now()
	opts.defaults()

	// Bound the whole test resolution so a stall anywhere (browser exec, retry,
	// or heal) fails this one test cleanly instead of running until the package
	// test deadline and panicking (which takes the rest of the package down).
	ctx, cancel := context.WithTimeout(ctx, perTestBudget)
	defer cancel()

	if opts.BaseURL == "" {
		return nil, fmt.Errorf("BaseURL is required")
	}

	logf := func(format string, args ...any) {
		if opts.Verbose {
			log.Printf(format, args...)
		}
	}

	// Step 1: Check cache.
	logf("[lazycue] checking cache in %s", opts.CacheDir)
	cachedTest, cacheHit, err := GetCachedTest(opts.CacheDir, description)
	if err != nil {
		logf("[lazycue] warning: get cached test: %v", err)
	}
	if cachedTest != nil {
		logf("[lazycue] cache hit: v%d", cacheHit.Version)
	}

	var version int
	if cacheHit != nil {
		version = cacheHit.Version
	}

	// Step 2: Launch browser
	logf("[lazycue] launching browser for %s", opts.BaseURL)
	browser, err := NewBrowser(ctx)
	if err != nil {
		return nil, fmt.Errorf("launch browser: %w", err)
	}
	defer browser.Close()

	// Optional artifact collection: per-step screenshots written under a
	// per-test subdirectory of ArtifactDir.
	var collector *artifactCollector
	if opts.ArtifactDir != "" {
		subdir := filepath.Join(opts.ArtifactDir, DescriptionHash(description))
		if c, cErr := newArtifactCollector(subdir); cErr == nil {
			collector = c
			browser.SetScreenshotSink(c.sink())
		} else {
			logf("[lazycue] warning: artifact collector: %v", cErr)
		}
	}

	// Step 3: If cached, try executing
	if cachedTest != nil {
		steps, parseErr := ParseSteps(cachedTest.Steps)
		if parseErr == nil {
			logf("[lazycue] found cached v%d (%d steps)", version, len(steps))
			results, execErr := browser.ExecuteSteps(ctx, opts.BaseURL, steps)
			allPassed := execErr == nil
			for _, r := range results {
				if !r.Pass {
					allPassed = false
					break
				}
			}
			// A cached test can fail spuriously under load (e.g. the app is slow
			// to deliver the first message and an element doesn't appear within a
			// wait timeout). Retry once with a fresh browser before paying for an
			// LLM heal — a genuine app/test mismatch will fail both times.
			if !allPassed && ctx.Err() == nil {
				logf("[lazycue] cached test failed; retrying once with a fresh browser")
				browser.Close()
				if rb, rErr := NewBrowser(ctx); rErr == nil {
					browser = rb
					if collector != nil {
						browser.SetScreenshotSink(collector.sink())
					}
					results, execErr = browser.ExecuteSteps(ctx, opts.BaseURL, steps)
					allPassed = execErr == nil
					for _, r := range results {
						if !r.Pass {
							allPassed = false
							break
						}
					}
				} else {
					return nil, fmt.Errorf("relaunch browser for retry: %w", rErr)
				}
			}
			if allPassed {
				logf("[lazycue] cached test passed")
				if collector != nil {
					collector.attach(results)
				}
				return &TestResult{
					Pass:          true,
					Steps:         results,
					CacheVersion:  version,
					Description:   description,
					Mode:          RunModeCached,
					TotalDuration: time.Since(totalStart),
				}, nil
			}

			// Cached test failed — need to fix it
			logf("[lazycue] cached test failed, spawning agent to fix")
			failureDesc := summarizeFailure(results)

			// Reset browser for agent
			browser.Close()
			browser, err = NewBrowser(ctx)
			if err != nil {
				return nil, fmt.Errorf("relaunch browser: %w", err)
			}
			if collector != nil {
				browser.SetScreenshotSink(collector.sink())
			}

			agentStart := time.Now()
			agentResult, agentErr := RunAgent(ctx, &AgentConfig{
				Mode:             AgentModeFix,
				Description:      description,
				PreviousSteps:    cachedTest.Steps,
				PreviousError:    failureDesc,
				CacheFilePath:    CacheFilePath(opts.CacheDir, description),
				Browser:          browser,
				BaseURL:          opts.BaseURL,
				Model:            opts.Model,
				AnthropicBaseURL: opts.AnthropicBaseURL,
				AnthropicAPIKey:  opts.AnthropicAPIKey,
				Verbose:          opts.Verbose,
			})
			agentDur := time.Since(agentStart)
			if agentErr != nil {
				return nil, fmt.Errorf("agent fix: %w", agentErr)
			}

			newVersion := version + 1
			cost := float64(agentResult.InputTokens)*3.0/1_000_000 + float64(agentResult.OutputTokens)*15.0/1_000_000
			if agentResult.Success {
				// A cached test can fail spuriously under heavy CI load even after
				// the in-process retry (e.g. a 60s wait that just barely times out
				// when many tests run in parallel). The heal agent then often
				// concludes the steps were already correct and re-emits the SAME
				// steps. Rewriting the cache in that case only churns the tracked
				// JSON (bumping the version, refreshing metadata) for no behavioral
				// change, which breaks the queue's commit-back step. Only persist a
				// heal when the steps actually differ from what's already cached.
				if sameSteps(cachedTest.Steps, agentResult.StepsJSON) {
					logf("[lazycue] healed steps identical to cache; not rewriting (transient flake)")
					newVersion = version
				} else {
					meta := buildCacheMetadata(opts, agentResult, "healed")
					if saveErr := SaveCachedTest(opts.CacheDir, description, agentResult.StepsJSON, newVersion, meta); saveErr != nil {
						logf("[lazycue] warning: save cached test: %v", saveErr)
					}
				}
			}

			if collector != nil {
				collector.attach(agentResult.StepResults)
			}
			return &TestResult{
				Pass:           agentResult.Success,
				Error:          agentResult.Error,
				ScreenshotPath: agentResult.ScreenshotPath,
				Steps:          agentResult.StepResults,
				CacheVersion:   newVersion,
				Description:    description,
				Mode:           RunModeHealed,
				TotalDuration:  time.Since(totalStart),
				AgentDuration:  agentDur,
				InputTokens:    agentResult.InputTokens,
				OutputTokens:   agentResult.OutputTokens,
				EstimatedCost:  cost,
			}, nil
		}
		// Parse error — treat as uncached
		logf("[lazycue] cached test parse error: %v, regenerating", parseErr)
	}

	// Step 4: No cache — generate from scratch
	logf("[lazycue] no cached test, spawning agent to generate")
	agentStart := time.Now()
	agentResult, agentErr := RunAgent(ctx, &AgentConfig{
		Mode:             AgentModeGenerate,
		Description:      description,
		Browser:          browser,
		BaseURL:          opts.BaseURL,
		Model:            opts.Model,
		AnthropicBaseURL: opts.AnthropicBaseURL,
		AnthropicAPIKey:  opts.AnthropicAPIKey,
		Verbose:          opts.Verbose,
	})
	agentDur := time.Since(agentStart)
	if agentErr != nil {
		return nil, fmt.Errorf("agent generate: %w", agentErr)
	}

	cost := float64(agentResult.InputTokens)*3.0/1_000_000 + float64(agentResult.OutputTokens)*15.0/1_000_000
	if agentResult.Success {
		meta := buildCacheMetadata(opts, agentResult, "generated")
		if saveErr := SaveCachedTest(opts.CacheDir, description, agentResult.StepsJSON, 1, meta); saveErr != nil {
			logf("[lazycue] warning: save cached test: %v", saveErr)
		}
	}

	if collector != nil {
		collector.attach(agentResult.StepResults)
	}
	return &TestResult{
		Pass:           agentResult.Success,
		Error:          agentResult.Error,
		ScreenshotPath: agentResult.ScreenshotPath,
		Steps:          agentResult.StepResults,
		CacheVersion:   1,
		Description:    description,
		Mode:           RunModeGenerated,
		TotalDuration:  time.Since(totalStart),
		AgentDuration:  agentDur,
		InputTokens:    agentResult.InputTokens,
		OutputTokens:   agentResult.OutputTokens,
		EstimatedCost:  cost,
	}, nil
}

func summarizeFailure(results []StepResult) string {
	for i, r := range results {
		if !r.Pass {
			return fmt.Sprintf("step %d (%s) failed: %s", i, r.Action, r.Error)
		}
	}
	return "unknown failure"
}

// Harness holds options for running lazycue tests. Create one as a
// package-level var and call Test from each test function:
//
//	var browser = lazycue.New(lazycue.Options{BaseURL: "http://localhost:3000"})
//
//	func TestLogin(t *testing.T) {
//	    browser.Test(t, "Navigate to /login and verify the login form is visible")
//	}
//
// A Harness accumulates every TestResult it runs, so a TestMain can emit an
// aggregate report/summary after all tests finish (see Results).
type Harness struct {
	opts Options

	mu      sync.Mutex
	results []*TestResult
}

// New creates a Harness with the given options.
func New(opts Options) *Harness {
	return &Harness{opts: opts}
}

// Results returns a copy of every TestResult run through this Harness so far.
// Use it from TestMain to write an aggregate report/summary, e.g.:
//
//	code := m.Run()
//	lazycue.WriteReport(dir, app.Results())
func (h *Harness) Results() []*TestResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*TestResult(nil), h.results...)
}

// Test runs a self-healing browser test described in plain English.
// It calls t.Fatal if the test fails or encounters an error.
func (h *Harness) Test(t testing.TB, description string) {
	t.Helper()
	result, err := Run(t.Context(), h.opts, description)
	if err != nil {
		t.Fatalf("lazycue: %v", err)
	}
	h.mu.Lock()
	h.results = append(h.results, result)
	h.mu.Unlock()

	// Log step results.
	var sb strings.Builder
	for _, s := range result.Steps {
		mark := "✓"
		if !s.Pass {
			mark = "✗"
		}
		fmt.Fprintf(&sb, "  %s %s  %s", mark, s.Summary, s.Duration.Round(time.Millisecond))
		if s.Error != "" {
			fmt.Fprintf(&sb, "  %s", s.Error)
		}
		sb.WriteByte('\n')
	}
	if result.InputTokens > 0 {
		cost := float64(result.InputTokens)*3.0/1_000_000 + float64(result.OutputTokens)*15.0/1_000_000
		fmt.Fprintf(&sb, "  ⚡ %d in / %d out tokens  ~$%.3f\n", result.InputTokens, result.OutputTokens, cost)
	}
	t.Logf("lazycue [%s]: %s\n%s", result.Mode, description, sb.String())

	if !result.Pass {
		t.Fatalf("lazycue: test failed: %s", result.Error)
	}
}

// sameSteps reports whether two raw step JSON blobs describe the same sequence
// of DSL steps. It compares the parsed steps (not the raw bytes) so that
// differences in whitespace or key ordering don't count as a change. Used to
// avoid rewriting a cache file when a heal re-emits the already-cached steps
// after a transient flake.
func sameSteps(a, b []byte) bool {
	sa, err := ParseSteps(a)
	if err != nil {
		return false
	}
	sb, err := ParseSteps(b)
	if err != nil {
		return false
	}
	if len(sa) != len(sb) {
		return false
	}
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func buildCacheMetadata(opts Options, agentResult *AgentResult, mode string) *CacheMetadata {
	cost := float64(agentResult.InputTokens)*3.0/1_000_000 + float64(agentResult.OutputTokens)*15.0/1_000_000
	hostname, _ := os.Hostname()
	return &CacheMetadata{
		CreatedAt:        time.Now().UTC(),
		Hostname:         hostname,
		Model:            opts.Model,
		InputTokens:      agentResult.InputTokens,
		OutputTokens:     agentResult.OutputTokens,
		EstimatedCostUSD: cost,
		CIRun:            detectCIRun(),
		GitSHA:           detectGitSHA(),
		Mode:             mode,
	}
}
