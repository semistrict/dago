package lazycue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/datool"
)

// AgentMode specifies whether the agent should generate new steps or fix existing ones.
type AgentMode int

const (
	AgentModeGenerate AgentMode = iota
	AgentModeFix
)

// AgentConfig configures an agent run.
type AgentConfig struct {
	Mode          AgentMode
	Description   string
	PreviousSteps []byte // JSON of previous steps (fix mode)
	PreviousError string // Error from previous run (fix mode)
	CacheFilePath string // Path to the cached steps JSON for this test (fix mode); lets the agent inspect its git history for prior flakiness
	Browser       *Browser
	BaseURL       string
	Model         string
	OpenAIBaseURL string
	OpenAIAPIKey  string
	HTTPClient    *http.Client
	RepoRoot      string
	Verbose       bool
}

// AgentResult is the result of an agent run.
type AgentResult struct {
	Success        bool
	Error          string
	StepsJSON      []byte
	StepResults    []StepResult
	ScreenshotPath string
	InputTokens    int
	OutputTokens   int
}

// agentBudget bounds the total wall-clock time a single heal/generate agent run
// may consume. It is kept well under the `go test` package timeout (10m by
// default) so that a slow or stuck heal fails its one test gracefully (via a
// context-deadline error that surfaces as t.Fatal) instead of running until the
// package deadline and panicking with "test timed out", which aborts every
// other test in the package too.
const agentBudget = 5 * time.Minute

const modelMaxAttempts = 3
const maxAgentTurns = 25
const modelCallTimeout = 90 * time.Second

var modelRetryBackoff = 2 * time.Second

// --- Tool input types ---

type runStepsInput struct {
	Steps json.RawMessage `json:"steps" description:"Array of DSL step objects to execute"`
	// Final marks this as the complete test to save (not an exploratory probe).
	// When true and all steps pass, the harness caches exactly these steps.
	Final bool `json:"final,omitempty" description:"True only for the complete final test"`
}

type screenshotInput struct{}

type gitCommandInput struct {
	Command string `json:"command" description:"A read-only git command"`
}

// --- Agent implementation ---

func buildSystemPrompt() string {
	return `You are a browser test-writing agent. You write DSL test steps (JSON arrays) to test a web application.

Available DSL actions:
- navigate: {"action": "navigate", "url": "/path"} - Navigate to URL (relative to base)
- wait_visible: {"action": "wait_visible", "selector": "...", "timeout": "10s"} - Wait for element to be visible
- wait_hidden: {"action": "wait_hidden", "selector": "...", "timeout": "10s"} - Wait for element to be hidden
- wait_text: {"action": "wait_text", "text": "...", "timeout": "30s"} - Wait for text in page body
- wait_text_gone: {"action": "wait_text_gone", "text": "...", "timeout": "10s"} - Wait for text to disappear
- fill: {"action": "fill", "selector": "...", "value": "..."} - Fill input/textarea (React compatible)
- click: {"action": "click", "selector": "..."} - Click element
- press_key: {"action": "press_key", "key": "Enter"} - Press keyboard key
- screenshot: {"action": "screenshot"} - Take screenshot
- eval: {"action": "eval", "expression": "...", "expect": "..."} - Evaluate JS; if "expect" is set, assert the stringified result equals it. The run_steps result echoes the evaluated value back to you ("=> <value>"), so use eval WITHOUT expect to probe page state (selectors, scrollHeight, classes, etc.) while developing the test.
- assert_visible: {"action": "assert_visible", "selector": "..."} - Assert element is visible
- assert_not_visible: {"action": "assert_not_visible", "selector": "..."} - Assert element is not visible
- assert_text: {"action": "assert_text", "selector": "...", "text": "..."} - Assert exact text content
- assert_text_contains: {"action": "assert_text_contains", "selector": "...", "text": "..."} - Assert text contains substring
- assert_attribute: {"action": "assert_attribute", "selector": "...", "attribute": "...", "value": "..."}
- wait_url: {"action": "wait_url", "text": "...", "timeout": "10s"} (substring) or {"action": "wait_url", "value": "..."} (exact) - Wait for the browser URL to match. Use this (not assert_url) when a click triggers an async SPA route change, e.g. /new -> /c/<slug>.
- assert_url: {"action": "assert_url", "value": "..."} or {"action": "assert_url", "text": "..."} for contains - Asserts the URL immediately (no waiting).
- assert_title: {"action": "assert_title", "text": "..."}
- assert_count: {"action": "assert_count", "selector": "...", "count": 3}
- sleep: {"action": "sleep", "timeout": "1s"}

WORKFLOW:
1. Start by navigating to the appropriate page.
2. Use wait_visible and wait_text with appropriate timeouts before asserting.
3. Use the run_steps tool to test your DSL steps. Review the results and fix any failures.
4. Use screenshot to see what the page looks like if you're unsure about selectors or page state.
5. Use git_command to explore the codebase (grep for selectors, data-testid attributes, etc.) when you need to discover the app's structure.
6. Your FINAL call to run_steps must be the COMPLETE, passing test that exercises every part of the description. Submit it by setting "final": true on that run_steps call (exploratory probes must NOT set final). The harness caches exactly the steps from your final submission, so it must contain all the assertions — never submit a bare navigation/probe as final.
7. Minimize the number of tool calls. Aim for 1-3 run_steps calls total.

CRITICAL: WHEN FIXING A FAILING TEST:
- The test DESCRIPTION is the source of truth for what the application SHOULD do.
- If the description says "the title should be X" and the page shows "Y", that means the APPLICATION IS BROKEN, not the test.
- Only fix MECHANICAL issues: wrong selectors, missing waits, timing issues, wrong CSS selectors.
- NEVER change what the test asserts to match broken application behavior.
- If the application doesn't match the description, the test SHOULD fail. Report it as a genuine failure.
- If you determine the app is genuinely broken (not matching the description), output an empty steps array and explain the failure.

DISTINGUISHING FLAKES FROM GENUINE FAILURES:
Many heal requests are triggered not by a broken app but by a LOAD-INDUCED FLAKE:
under heavy CI parallelism the box is so starved that an element appears later
than a wait_* timeout allows, even though the same steps pass quickly when the
machine is idle. Treat an error as a likely flake when it is timeout-shaped
(e.g. "wait_visible/wait_text timed out", an element that intermittently doesn't
appear, an assert that fails only on a transient/empty state) AND the steps
themselves look correct for the description.
- A timeout-shaped error is NOT automatically a flake. The real discriminator is
  "appears LATE" vs "never appears / appears WRONG". Before deciding it's a flake
  you MUST confirm the app actually does the right thing now: navigate and use
  screenshot / eval (without expect) to observe the live page, and verify the
  awaited element/text is genuinely present and CORRECT per the description, just
  slow. If it is absent or shows the wrong content even after waiting, that is a
  GENUINE FAILURE (a real regression often looks timeout-shaped because the
  element never appears) — do not paper over it by raising timeouts.
- INVESTIGATE BEFORE EDITING. Use git_command to read the cached test's history:
  'git log --oneline -- <cache-file>' and 'git log -p -- <cache-file>'. Repeated
  past edits that only bumped timeouts or swapped a sleep for a wait are a strong
  signal this test flakes under load.
- Once you have CONFIRMED the behavior is correct-but-slow, the right fix is to
  make the SAME steps MORE ROBUST, never to weaken assertions: raise wait_*
  timeouts generously (e.g. 30s -> 60s), replace bare sleeps or implicit waits
  with explicit wait_visible/wait_text on the element you are about to assert,
  and wait for the precise post-condition (specific text/element/url) rather
  than a coarse one. Keep every assertion in the description intact.
- Conclude "genuine app failure" (empty steps array, with an explanation)
  whenever the live page does not match the description — including when an
  element never appears or shows wrong content despite waiting. When in doubt
  between flake and regression, prefer reporting the failure over masking it.`
}

func buildGenerateUserPrompt(description string) string {
	return fmt.Sprintf(`Write DSL test steps for this behavior:

%s

Write the complete test as a single run_steps call. If the description mentions specific selectors or page structure you're unsure about, use screenshot or git_command to discover them first.`, description)
}

func buildFixUserPrompt(description string, previousSteps []byte, previousError, cacheFilePath string) string {
	historyHint := ""
	if cacheFilePath != "" {
		historyHint = fmt.Sprintf(`

This test's cached steps live at: %s
Before editing, check whether this test has a history of flaking under load:
  git log --oneline -- %s
  git log -p -- %s
A history of timeout-only bumps is strong evidence the failure is a load flake,
not a broken app.`, cacheFilePath, cacheFilePath, cacheFilePath)
	}
	return fmt.Sprintf(`A previously generated DSL test is failing. Fix it.

Test description: %s

Previous DSL steps:
%s

Error:
%s

INSTRUCTIONS:
- The test DESCRIPTION is the source of truth for what the app SHOULD do.
- First decide: is this a LOAD-INDUCED FLAKE or a GENUINE app failure?
  A timeout-shaped error (wait_* timed out, an element that intermittently
  doesn't appear) can be either: "appears late" (flake) or "never appears /
  appears wrong" (real regression). Observe the LIVE page (navigate + screenshot
  / eval) and confirm the awaited element/text is actually present and correct
  per the description before calling it a flake.%s
- For a flake: keep every assertion, and make the same steps more robust —
  raise wait_* timeouts generously and wait explicitly on the precise element
  you are about to assert. Do NOT weaken assertions.
- For a mechanical bug: fix wrong selectors / missing waits only.
- NEVER change assertions to match broken application behavior.
- If the app genuinely doesn't match the description (e.g., wrong title, missing
  text, and robustness changes can't fix it), return an empty steps array [] and
  explain the failure.
- Use screenshots to verify the current state, then decide.`, description, string(previousSteps), previousError, historyHint)
}

type agentRunState struct {
	mu          sync.Mutex
	lastSteps   []byte
	lastResults []StepResult
	finalSteps  []byte
	finalResult []StepResult
	lastError   string
}

func (state *agentRunState) record(input runStepsInput, results []StepResult, err error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.lastSteps = append([]byte(nil), input.Steps...)
	state.lastResults = append([]StepResult(nil), results...)
	if input.Final {
		state.finalSteps = append([]byte(nil), input.Steps...)
		state.finalResult = append([]StepResult(nil), results...)
	}
	if err != nil {
		state.lastError = err.Error()
	}
}

func (state *agentRunState) snapshot() (lastSteps []byte, lastResults []StepResult, finalSteps []byte, finalResults []StepResult, lastError string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]byte(nil), state.lastSteps...), append([]StepResult(nil), state.lastResults...), append([]byte(nil), state.finalSteps...), append([]StepResult(nil), state.finalResult...), state.lastError
}

func buildTools(cfg *AgentConfig, state *agentRunState, logf func(string, ...any)) []datool.Tool {
	runSteps := datool.MustNew(
		"run_steps",
		"Execute an array of DSL test steps against the browser. When the complete test passes, call this tool with final=true so those exact steps can be cached.",
		func(ctx context.Context, input runStepsInput) (string, error) {
			steps, err := ParseSteps(input.Steps)
			if err != nil {
				return "", fmt.Errorf("parse steps: %w", err)
			}
			logf("tool: run_steps (%d steps, final=%t)", len(steps), input.Final)
			for index, step := range steps {
				logf("  [%d] %s", index, StepSummary(step))
			}
			results, executeErr := cfg.Browser.ExecuteSteps(ctx, cfg.BaseURL, steps)
			state.record(input, results, executeErr)
			var summary strings.Builder
			for index, result := range results {
				status := "PASS"
				if !result.Pass {
					status = "FAIL"
				}
				fmt.Fprintf(&summary, "Step %d [%s] %s (%s)", index, result.Action, status, result.Duration.Round(time.Millisecond))
				if result.Error != "" {
					summary.WriteString(": " + result.Error)
				}
				if result.Output != "" {
					summary.WriteString(" => " + truncateArg(result.Output, 200))
				}
				summary.WriteByte('\n')
			}
			if executeErr != nil {
				summary.WriteString("\nOverall: FAILED - " + executeErr.Error())
			} else {
				summary.WriteString("\nOverall: ALL STEPS PASSED")
			}
			return summary.String(), nil
		},
		datool.WithPropertyType("steps", "array"),
		datool.WithPropertyValue("steps", "items", map[string]any{"type": "object"}),
	)
	screenshot := datool.MustNew("screenshot", "Take a screenshot of the current browser page and return it as a PNG image.", func(ctx context.Context, _ screenshotInput) (datool.Result, error) {
		logf("tool: screenshot")
		png, err := cfg.Browser.Screenshot(ctx)
		if err != nil {
			return datool.Result{}, fmt.Errorf("screenshot: %w", err)
		}
		return datool.Result{Content: []damessage.ContentBlock{{Type: damessage.BlockImage, MIMEType: "image/png", Data: png}}}, nil
	})
	gitCommand := datool.MustNew("git_command", "Run one read-only git command in the repository root to inspect tracked source and history.", func(_ context.Context, input gitCommandInput) (string, error) {
		logf("tool: git_command %s", input.Command)
		return executeGitCommand(cfg.RepoRoot, input.Command), nil
	})
	return []datool.Tool{runSteps, screenshot, gitCommand}
}

// RunAgent generates or heals one browser test through dago's native agent
// graph. Model messages and tool results remain provider-neutral throughout;
// only the OpenAI Responses adapter knows the wire protocol.
func RunAgent(ctx context.Context, cfg *AgentConfig) (*AgentResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("agent config is required")
	}
	if cfg.Browser == nil {
		return nil, fmt.Errorf("browser is required")
	}
	logf := func(format string, args ...any) {
		if cfg.Verbose {
			log.Printf("[agent] "+format, args...)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, agentBudget)
	defer cancel()

	chat, err := openai.NewAPIKey(cfg.OpenAIAPIKey, openai.Options{
		Model: cfg.Model, BaseURL: cfg.OpenAIBaseURL, HTTPClient: cfg.HTTPClient,
		MaxOutputTokens: 8192,
		// LazyCue owns the user-visible retry budget below. Disable the
		// transport adapter's retry loop so three logical attempts cannot
		// multiply into twelve HTTP requests.
		RetryBackoff: []time.Duration{},
	})
	if err != nil {
		return nil, err
	}
	retrying := &retryingChat{inner: chat, attempts: modelMaxAttempts, backoff: modelRetryBackoff, verbose: cfg.Verbose}
	state := &agentRunState{}
	compiled, err := dagent.New(dagent.Options{
		Name: "lazycue", Model: retrying, SystemPrompt: buildSystemPrompt(),
		Tools: buildTools(cfg, state, logf), RecursionLimit: maxAgentTurns*2 + 3,
		MaxConcurrency: 1,
	})
	if err != nil {
		return nil, err
	}
	userPrompt := buildGenerateUserPrompt(cfg.Description)
	if cfg.Mode == AgentModeFix {
		userPrompt = buildFixUserPrompt(cfg.Description, cfg.PreviousSteps, cfg.PreviousError, cfg.CacheFilePath)
	}
	result, err := compiled.Invoke(ctx, dagent.Input{Messages: []damessage.Message{damessage.Human(userPrompt)}})
	if err != nil {
		return nil, fmt.Errorf("run native agent: %w", err)
	}

	if _, _, finalSteps, finalResults, _ := state.snapshot(); finalSteps == nil || !allPassed(finalResults) {
		result, err = compiled.Invoke(ctx, dagent.Input{Messages: append(result.Messages, damessage.Human("You have not produced a complete passing test. Call run_steps with the full test and final=true. If the application is genuinely broken, state that explicitly."))})
		if err != nil {
			return nil, fmt.Errorf("finalize native agent: %w", err)
		}
	}

	lastSteps, lastResults, finalSteps, finalResults, lastError := state.snapshot()
	inputTokens, outputTokens := usageFromMessages(result.Messages)
	for _, item := range result.Messages {
		if item.Role != damessage.RoleAssistant {
			continue
		}
		text := item.TextContent()
		logf("assistant: %s", truncate(text, 200))
		if isGenuineFailureSignal(text) {
			if lastError == "" {
				lastError = text
			}
			return &AgentResult{Success: false, Error: lastError, StepsJSON: lastSteps, StepResults: lastResults, InputTokens: inputTokens, OutputTokens: outputTokens}, nil
		}
	}
	if finalSteps != nil && allPassed(finalResults) {
		return &AgentResult{Success: true, StepsJSON: finalSteps, StepResults: finalResults, InputTokens: inputTokens, OutputTokens: outputTokens}, nil
	}
	if lastError == "" {
		lastError = "agent stopped without producing a complete passing test"
	}
	return &AgentResult{Success: false, Error: lastError, StepsJSON: lastSteps, StepResults: lastResults, InputTokens: inputTokens, OutputTokens: outputTokens}, nil
}

func usageFromMessages(messages []damessage.Message) (inputTokens, outputTokens int) {
	for _, item := range messages {
		if item.Usage == nil {
			continue
		}
		inputTokens += item.Usage.InputTokens
		outputTokens += item.Usage.OutputTokens
	}
	return inputTokens, outputTokens
}

func allPassed(results []StepResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if !r.Pass {
			return false
		}
	}
	return true
}

// gitExec runs a git command in the given directory and returns trimmed stdout.
func gitExec(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func executeGitCommand(repoRoot, command string) string {
	normalized := strings.TrimSpace(command)
	if !strings.HasPrefix(normalized, "git ") {
		return "Error: Only git commands are allowed"
	}

	allowedSubcommands := []string{"grep", "ls-files", "show", "log", "diff", "cat-file", "rev-parse", "for-each-ref"}
	parts := strings.Fields(normalized)
	if len(parts) < 2 {
		return "Error: Invalid git command"
	}
	subcommand := parts[1]

	allowed := false
	for _, a := range allowedSubcommands {
		if subcommand == a {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Sprintf("Error: Only these git subcommands are allowed: %s", strings.Join(allowedSubcommands, ", "))
	}

	// Execute the command (strip "git " prefix and pass args).
	args := parts[1:]
	out, err := gitExec(repoRoot, args...)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	if out == "" {
		return "(empty output)"
	}

	// Truncate long output.
	const maxLen = 8000
	if len(out) > maxLen {
		return out[:maxLen] + fmt.Sprintf("\n... (truncated, %d total chars)", len(out))
	}

	return out
}

type retryingChat struct {
	inner    damodel.Chat
	attempts int
	backoff  time.Duration
	verbose  bool
}

func (chat *retryingChat) Profile() damodel.Profile { return chat.inner.Profile() }

func (chat *retryingChat) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= chat.attempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, modelCallTimeout)
		response, err := chat.inner.Invoke(callCtx, request)
		cancel()
		if err == nil {
			return response, nil
		}
		lastErr = err
		if !isRetryableModelError(ctx, err) || attempt == chat.attempts {
			break
		}
		if err := chat.wait(ctx, attempt, lastErr); err != nil {
			return damodel.Response{}, err
		}
	}
	return damodel.Response{}, lastErr
}

func (chat *retryingChat) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	var lastErr error
	for attempt := 1; attempt <= chat.attempts; attempt++ {
		stream, err := chat.inner.Stream(ctx, request)
		if err == nil {
			return stream, nil
		}
		lastErr = err
		if !isRetryableModelError(ctx, err) || attempt == chat.attempts {
			break
		}
		if err := chat.wait(ctx, attempt, lastErr); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (chat *retryingChat) wait(ctx context.Context, attempt int, lastErr error) error {
	delay := time.Duration(attempt) * chat.backoff
	if chat.verbose {
		log.Printf("[agent] model request failed (attempt %d/%d), retrying in %s: %v", attempt, chat.attempts, delay, lastErr)
	}
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("model request: %w (last error: %v)", ctx.Err(), lastErr)
	}
}

func isRetryableModelError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusRequestTimeout || apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	text := err.Error()
	return strings.Contains(text, "openai: request:") || strings.Contains(text, "openai: decode response:")
}

// isGenuineFailureSignal checks if the agent's text output indicates the
// APPLICATION is genuinely broken (not a mechanical test issue).
func isGenuineFailureSignal(text string) bool {
	lower := strings.ToLower(text)
	signals := []string{
		"genuine application failure",
		"genuine failure",
		"application is broken",
		"app is broken",
		"the application does not match",
		"not a mechanical",
		"not a test issue",
		"the app is genuinely broken",
		"this is a real bug",
		"this is a genuine bug",
	}
	for _, s := range signals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
