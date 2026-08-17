package daworkflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

// Status is the lifecycle state of one background workflow run.
type Status struct {
	Version       int     `json:"version"`
	TaskID        string  `json:"task_id"`
	RunID         string  `json:"run_id"`
	Name          string  `json:"name"`
	Description   string  `json:"description,omitempty"`
	Phases        []Phase `json:"phases,omitempty"`
	Status        string  `json:"status"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	Events        []Event `json:"events,omitempty"`
	Result        *Result `json:"result,omitempty"`
	Error         string  `json:"error,omitempty"`
	ScriptPath    string  `json:"script_path,omitempty"`
	TranscriptDir string  `json:"transcript_dir,omitempty"`
	OutputPath    string  `json:"output_path,omitempty"`
}

// StartRequest requests an inline or saved workflow run.
type StartRequest struct {
	Script          string `json:"script,omitempty"`
	ScriptPath      string `json:"script_path,omitempty"`
	Name            string `json:"name,omitempty"`
	Args            any    `json:"args,omitempty"`
	ResumeFromRunID string `json:"resume_from_run_id,omitempty"`
}

type managedRun struct {
	status Status
	script string
	cancel context.CancelFunc
}

const workflowToolDescription = `Run explicitly requested multi-agent orchestration as a deterministic JavaScript workflow in the background. Workflows encode fan-out, verification, and synthesis in one script instead of choosing agents one at a time in the model loop.

WHEN TO USE
Workflows are opt-in. Use this tool only when the user explicitly asks for multi-agent orchestration or a workflow, or when a configured mode or skill explicitly invokes workflows. They fit work that needs more context than one agent can hold (audits, migrations, broad sweeps), independent parallel coverage, or adversarial verification. Do not use them for ordinary single-agent tasks.

SCOUT BEFORE AUTHORING
Before creating a repository workflow, inspect the repository yourself first. Read its applicable instruction files and architecture or contributor guidance, map the relevant packages and directories, and identify the concrete work boundaries. Use that evidence to choose agent scopes, prompts, phases, schemas, and an appropriate fan-out. Do not delegate the initial orientation or invent a generic workflow from the user's request alone. Keep this scouting brief and read-only; the workflow should perform the broad analysis or implementation after its structure is grounded in the repository.

INPUT AND SCRIPT ANATOMY
For a new run, supply exactly one of script, script_path, or name. name and script_path both resolve a saved workflow through the configured resolver; name is the convenient registry form while script_path can be an explicit saved-script path. args is passed to the script verbatim as JSON. To resume, supply resume_from_run_id from a completed same-session run and optionally script, script_path, or name; when none is supplied, the prior run's script is reused.

Scripts are plain JavaScript, not TypeScript, run in an async context with top-level await. They must begin with export const meta containing required name and description fields and optional whenToUse and phases fields. The JavaScript runtime evaluates the module and the host reads meta from the real module namespace before it starts driving the workflow body; metadata is not parsed or stripped by the host. The exported value must be synchronously initialized and JSON-safe. This meta declaration is the only source-level export: put executable statements directly after it and use a top-level return. Never add export default, async function main(), or a main() wrapper:

    export const meta = {
      name: 'refactor-analysis',
      description: 'Analyze and verify refactoring opportunities',
      whenToUse: 'Use for broad, independently verifiable repository analysis',
      phases: [
        { title: 'Scan', detail: '8 area analysts' },
        { title: 'Verify', detail: 'adversarial checks' },
      ],
    }
    // script body follows; top-level await is allowed

SCRIPT API
- agent(prompt, opts?) -> Promise<any>: spawn one subagent. Without schema, returns its final text as a raw workflow value rather than a user-facing message. With a JSON Schema in opts.schema, the worker must produce structured output and the call returns the validated object without script-side parsing; schema mismatches are retried up to the configured retry limit. Structured schemas must be strict: every object, including objects nested under array items, must declare all properties, list required fields, and set additionalProperties: false; never use an unconstrained {type: 'object'}. Options are label (display name), phase (progress group), model (override; normally omit to inherit), effort (low, medium, high, xhigh, or max; prefer low for mechanical stages and high tiers for difficult verification or judging), isolation: 'worktree' (host-dependent, expensive, and intended only for parallel file mutations), and agentType (host-dependent custom worker type). Set phase explicitly on agents inside parallel or pipeline stages to avoid races on phase()'s global state. In auto-review hosts, a denied tool call is returned to the worker as a rejection result rather than killing it, so it can choose a safe alternative; a worker that determines it cannot produce a useful result can explicitly terminate its branch, which produces null and an agent_failed event. A skipped agent or any other terminal worker error has the same null result, so always filter or explicitly handle null results. If failures leave no usable result, throw an error instead of returning null so the run cannot be mistaken for success.
- parallel(thunks) -> Promise<any[]>: run zero-argument promise-returning thunks concurrently. This is a barrier and waits for every thunk. A throwing thunk resolves to null; parallel itself does not reject for that branch. Always handle null entries.
- pipeline(items, stage1, stage2, ...) -> Promise<any[]>: run each item through its stages independently, with no barrier between stages. Item A may reach stage 3 while item B remains in stage 1, so wall-clock time approaches the slowest single-item chain. Each stage receives (previousResult, originalItem, index). A stage that throws turns that item into null and skips its remaining stages. Use pipeline by default for multi-stage per-item work. An agent failure returns null rather than throwing, so a stage must explicitly check it if later stages should be skipped.
- phase(title): start a progress group; subsequent agent calls inherit it unless opts.phase is explicit.
- log(message): emit a user-visible narrator line. Use log to disclose every cap, sample, top-N choice, or dropped-item rule.
- args: the tool input's args value as real JavaScript arrays, objects, and primitives. Use it to parameterize saved workflows.
- budget: {total, spent(), remaining()}. The configured token target is shared by the workflow and nested workflow calls. Once spent() >= total, another agent call throws. With no target, total is 0 and remaining() is Infinity. Guard budget-driven loops with budget.total, for example: while (budget.total && budget.remaining() > 50_000) { ... }.
- workflow(nameOrRef, childArgs?): run a saved workflow inline by name/path or {scriptPath}. A child shares the concurrency cap, agent counter, cancellation signal, and budget. Only one nesting level is allowed.

Only the APIs above are available. There is no workflow.task, task, Agent, or subagent_type API inside scripts. Use agent(prompt, opts) for workers, parallel() for concurrent fan-out, and workflow(reference, args) only for a saved nested workflow. agent() returns the worker's actual result, not a task or run ID.

The filesystem, Node APIs, Date.now(), Math.random(), and argumentless new Date() are unavailable. Nondeterminism would break exact resume. Pass timestamps through args and stamp results after completion.

BARRIER VERSUS PIPELINE
Put a parallel barrier between stages only when the next stage requires cross-item context from every prior result: full-set deduplication or merging, a global-count early exit, or prompts that compare other findings. Put per-item transforms, filters, and ordinary stage-to-stage work in pipeline. A barrier is justified when all scanners must finish before findings can be deduplicated and ranked for verification.

CONCURRENCY AND LIMITS
Concurrent agents are capped at min(16, available CPUs - 2), with a minimum of one; excess calls queue. A workflow has a lifetime maximum of 1000 live agent calls. A parallel or pipeline call accepts at most 4096 items and errors rather than truncating. Unless the request calls for more, follow a medium-size authoring guideline of about 15 agents. Worker access to session-connected tools (including tools discoverable through a host's tool search) and worktree/custom-agent support depends on the host; do not assume interactively authenticated tools are available in headless execution.

BACKGROUND EXECUTION AND ARTIFACTS
The tool validates and starts the workflow, then immediately returns task_id, run_id, status, and any configured script_path, transcript_dir, and output_path. Report those IDs instead of immediately polling. Use check_workflow only when status or results are needed, use list_workflows to inspect runs, and use cancel_workflow to stop one. A host may publish a completion notification; the full return value is available from check_workflow and, when persistence is configured, result.json.

Each persisted invocation stores its script so revisions can use script_path instead of resending source. Its run directory contains journal.jsonl, one agent-<call>.jsonl record per completed call, and result.json. Read the journal before diagnosing empty or unexpected values.

RESUME AND CACHING
Resume with {"script_path":"...","resume_from_run_id":"wf_..."}; stop a still-running prior run first. The runtime replays the script. Each agent call whose prompt and options exactly match the corresponding completed call is returned from cache immediately. The first edited or new call and every call after it run live. The same script and args can therefore be a complete cache hit. Cache matching is same-session and exact on deterministic request content. If no journal exists, inspect per-agent records and author a continuation workflow.

QUALITY PATTERNS
- Adversarial verify: assign independent skeptics to refute each finding; reject on majority refutation.
- Perspective-diverse verify: use different correctness, security, performance, and reproduction lenses rather than identical reviewers.
- Judge panel: run independent attempts, have parallel judges score them, then synthesize from the winner.
- Loop until dry: for unknown-size discovery, continue until K rounds find nothing new; deduplicate against everything seen, including rejected findings.
- Multi-modal sweep: search independently by container, content, entity, time, or another relevant axis.
- Completeness critic: ask a final worker what is missing and use that result to seed another round.
- Loop until budget: guard additional rounds with budget.total and budget.remaining().

CANONICAL SCAN, DEDUPLICATE, AND VERIFY EXAMPLE

    export const meta = {
      name: 'refactor-analysis',
      description: 'Find and adversarially verify high-impact refactors',
      phases: [
        { title: 'Scan', detail: 'independent area analysis' },
        { title: 'Verify', detail: 'adversarial checks' },
      ],
    }

    phase('Scan')
    const scans = await parallel(AREAS.map(area => () =>
      agent(scanPrompt(area), {
        label: 'scan:' + area.key,
        phase: 'Scan',
        schema: FINDINGS,
      })))

    const all = scans.flatMap((result, index) =>
      result?.findings?.map(finding => ({...finding, area: AREAS[index].key})) ?? [])
    const toVerify = dedupeAndRank(all).filter(finding => finding.impact === 'high')

    phase('Verify')
    const verified = await parallel(toVerify.map(finding => async () => {
      const verdict = await agent(refutePrompt(finding), {
        label: 'verify:' + finding.files[0],
        phase: 'Verify',
        schema: VERDICT,
      })
      return verdict ? {...finding, verification: verdict} : null
    }))

    return {
      confirmed: verified
        .filter(Boolean)
        .filter(result => result.verification.verdict === 'confirmed'),
    }

Saved workflows are resolved by name or script_path and parameterized with args. Hosts commonly load them from project workflow registries such as .claude/workflows or .agents/workflows. Nested workflow() resolves names and paths through the same registry.`

// Manager owns background workflow runs and their start/check/cancel tools.
// Close must be called to cancel unfinished work.
type Manager struct {
	lifetime       context.Context
	cancelLifetime context.CancelFunc
	runner         AgentRunner
	options        Options
	next           atomic.Uint64

	mu     sync.Mutex
	runs   map[string]*managedRun
	closed bool
}

// NewManager constructs a same-process background workflow service.
func NewManager(runner AgentRunner, options Options) *Manager {
	if runner == nil {
		panic("workflow agent runner is required")
	}
	options = New(runner, options).options
	// Runs outlive the request that starts them and are stopped by Cancel or Close.
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	return &Manager{lifetime: lifetime, cancelLifetime: cancelLifetime, runner: runner, options: options, runs: map[string]*managedRun{}}
}

// Middleware exposes workflow start, check, cancel, and list tools.
func Middleware(manager *Manager) dagent.Middleware {
	if manager == nil {
		panic("workflow manager is required")
	}
	middleware := dagent.Middleware{
		Name: "workflows", SerializedName: "WorkflowMiddleware",
		Tools: manager.Tools(),
	}
	middleware.WrapModelCall = func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
		const guidance = `Workflows are opt-in. Use the workflow tool only when the user explicitly asks for multi-agent orchestration, a workflow, or a configured workflow-triggering mode. A workflow runs in the background; after starting one, report its task and run IDs instead of immediately polling it.`
		message := request.SystemMessage.Clone()
		message.Content = append(message.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + guidance})
		request.SystemMessage = &message
		return next(ctx, request)
	}
	return middleware
}

// Start validates and launches a workflow, returning before agent work begins.
func (manager *Manager) Start(ctx context.Context, request StartRequest) (Status, error) {
	if manager == nil {
		return Status{}, fmt.Errorf("workflow manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	script := request.Script
	reference := request.ScriptPath
	provided := 0
	for _, value := range []string{script, request.ScriptPath, request.Name} {
		if strings.TrimSpace(value) != "" {
			provided++
		}
	}
	if provided > 1 {
		return Status{}, fmt.Errorf("workflow accepts exactly one of script, script_path, or name")
	}
	if request.Name != "" {
		reference = request.Name
	}
	var resume []JournalEntry
	if request.ResumeFromRunID != "" {
		manager.mu.Lock()
		previous := manager.runs[request.ResumeFromRunID]
		if previous == nil {
			manager.mu.Unlock()
			return Status{}, fmt.Errorf("workflow run %q is not available for resume", request.ResumeFromRunID)
		}
		if previous.status.Result == nil {
			manager.mu.Unlock()
			return Status{}, fmt.Errorf("workflow run %q has no completed journal", request.ResumeFromRunID)
		}
		resume = append([]JournalEntry(nil), previous.status.Result.Journal...)
		if provided == 0 {
			script = previous.script
		}
		manager.mu.Unlock()
	}
	if reference != "" {
		if manager.options.Resolver == nil {
			return Status{}, fmt.Errorf("workflow resolver is not configured")
		}
		resolved, err := manager.options.Resolver.ResolveWorkflow(ctx, reference)
		if err != nil {
			return Status{}, fmt.Errorf("resolve workflow %q: %w", reference, err)
		}
		script = resolved
	}
	sequence := manager.next.Add(1)
	runID := fmt.Sprintf("wf_%d", sequence)
	taskID := fmt.Sprintf("workflow-%d", sequence)
	stamp := time.Now().UTC().Format(time.RFC3339)
	status := Status{Version: 1, TaskID: taskID, RunID: runID, Status: "running", CreatedAt: stamp, UpdatedAt: stamp}
	runContext, cancel := context.WithCancel(manager.lifetime)
	managed := &managedRun{status: status, script: script, cancel: cancel}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		cancel()
		return Status{}, fmt.Errorf("workflow manager is closed")
	}
	manager.mu.Unlock()

	options := manager.options
	options.Resume = resume
	configuredProgress := options.Progress
	options.Progress = func(progressContext context.Context, event Event) error {
		manager.mu.Lock()
		managed.status.Events = append(managed.status.Events, event)
		managed.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		manager.mu.Unlock()
		if configuredProgress != nil {
			return configuredProgress(progressContext, event)
		}
		return nil
	}
	prepared, err := prepareWorkflowRun(runContext, manager.runner, script, request.Args, options)
	if err != nil {
		cancel()
		return Status{}, err
	}
	meta := prepared.meta
	status.Name = meta.Name
	status.Description = meta.Description
	status.Phases = append([]Phase(nil), meta.Phases...)
	if manager.options.SessionDirectory != "" {
		scriptDir := filepath.Join(manager.options.SessionDirectory, "workflows", "scripts")
		transcriptDir := filepath.Join(manager.options.SessionDirectory, "workflows", "runs", runID)
		if err := os.MkdirAll(scriptDir, 0o700); err != nil {
			prepared.close()
			cancel()
			return Status{}, fmt.Errorf("create workflow script directory: %w", err)
		}
		if err := os.MkdirAll(transcriptDir, 0o700); err != nil {
			prepared.close()
			cancel()
			return Status{}, fmt.Errorf("create workflow transcript directory: %w", err)
		}
		status.ScriptPath = filepath.Join(scriptDir, fileName(meta.Name)+"-"+runID+".js")
		status.TranscriptDir = transcriptDir
		status.OutputPath = filepath.Join(transcriptDir, "result.json")
		if err := os.WriteFile(status.ScriptPath, []byte(script), 0o600); err != nil {
			prepared.close()
			cancel()
			return Status{}, fmt.Errorf("persist workflow script: %w", err)
		}
	}
	managed.status = status
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		prepared.close()
		cancel()
		return Status{}, fmt.Errorf("workflow manager is closed")
	}
	manager.runs[runID] = managed
	manager.mu.Unlock()
	go manager.execute(runContext, managed, prepared, options)
	return status, nil
}

func (manager *Manager) execute(ctx context.Context, managed *managedRun, prepared *preparedWorkflow, options Options) {
	defer managed.cancel()
	defer prepared.close()
	result, err := completeWorkflowRun(ctx, prepared)
	persistErr := persistRun(managed.status, result)
	agentErr := failedRunError(result)
	manager.mu.Lock()
	managed.status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err == nil && persistErr == nil && agentErr == nil {
		managed.status.Status = "success"
		managed.status.Result = &result
	} else {
		if ctx.Err() != nil {
			managed.status.Status = "cancelled"
		} else {
			managed.status.Status = "error"
		}
		if err != nil {
			managed.status.Error = err.Error()
		} else if persistErr != nil {
			managed.status.Error = persistErr.Error()
		} else {
			managed.status.Error = agentErr.Error()
		}
		if result.Meta.Name != "" {
			managed.status.Result = &result
		}
	}
	status := cloneStatus(managed.status)
	manager.mu.Unlock()
	if options.Completed != nil {
		options.Completed(context.WithoutCancel(ctx), status)
	}
}

func failedRunError(result Result) error {
	failed, finished := 0, 0
	latest := ""
	for _, event := range result.Events {
		switch event.Kind {
		case "agent_failed":
			failed++
			if event.Message != "" {
				latest = event.Message
			}
		case "agent_finished":
			finished++
		}
	}
	if failed == 0 {
		return nil
	}
	if finished == 0 {
		if latest == "" {
			return fmt.Errorf("all %d workflow agent calls failed", failed)
		}
		return fmt.Errorf("all %d workflow agent calls failed: %s", failed, latest)
	}
	if result.Value != nil {
		return nil
	}
	if latest == "" {
		return fmt.Errorf("workflow returned null after %d agent failures", failed)
	}
	return fmt.Errorf("workflow returned null after %d agent failures: %s", failed, latest)
}

// Check returns an isolated snapshot of a workflow run.
func (manager *Manager) Check(runID string) (Status, bool) {
	if manager == nil {
		return Status{}, false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	run := manager.runs[runID]
	if run == nil {
		return Status{}, false
	}
	return cloneStatus(run.status), true
}

// Cancel requests cancellation of a running workflow.
func (manager *Manager) Cancel(runID string) bool {
	if manager == nil {
		return false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	run := manager.runs[runID]
	if run == nil || run.status.Status != "running" {
		return false
	}
	run.cancel()
	return true
}

// List returns workflow snapshots ordered by run ID.
func (manager *Manager) List() []Status {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make([]Status, 0, len(manager.runs))
	for _, run := range manager.runs {
		result = append(result, cloneStatus(run.status))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RunID < result[j].RunID })
	return result
}

// Running returns the number of active workflow runs.
func (manager *Manager) Running() int {
	if manager == nil {
		return 0
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	count := 0
	for _, run := range manager.runs {
		if run.status.Status == "running" {
			count++
		}
	}
	return count
}

// Tools returns the workflow, check_workflow, cancel_workflow, and list_workflows tools.
func (manager *Manager) Tools() []datool.Tool {
	if manager == nil {
		panic("workflow manager is nil")
	}
	start := datool.MustNew("workflow", workflowToolDescription, func(ctx context.Context, input StartRequest) (any, error) {
		status, err := manager.Start(ctx, input)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"version": 1, "task_id": status.TaskID, "run_id": status.RunID, "status": status.Status, "name": status.Name,
			"script_path": status.ScriptPath, "transcript_dir": status.TranscriptDir, "output_path": status.OutputPath,
		}, nil
	})
	type runInput struct {
		RunID string `json:"run_id"`
	}
	check := datool.MustNew("check_workflow", "Check a background workflow and return its current progress or completed result.", func(_ context.Context, input runInput) (any, error) {
		status, exists := manager.Check(strings.TrimSpace(input.RunID))
		if !exists {
			return nil, fmt.Errorf("workflow run %q was not found", input.RunID)
		}
		return status, nil
	})
	cancel := datool.MustNew("cancel_workflow", "Cancel a running background workflow.", func(_ context.Context, input runInput) (any, error) {
		if !manager.Cancel(strings.TrimSpace(input.RunID)) {
			return nil, fmt.Errorf("workflow run %q is not running", input.RunID)
		}
		return map[string]any{"version": 1, "run_id": input.RunID, "status": "cancelling"}, nil
	})
	list := datool.MustNew("list_workflows", "List background workflows and their current statuses.", func(context.Context, struct{}) (any, error) {
		return manager.List(), nil
	})
	return []datool.Tool{start, check, cancel, list}
}

// Close cancels all running workflows. It is safe to call more than once.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	manager.cancelLifetime()
	for _, run := range manager.runs {
		if run.status.Status == "running" {
			run.cancel()
		}
	}
	manager.mu.Unlock()
	return nil
}

func cloneStatus(status Status) Status {
	raw, _ := json.Marshal(status)
	var result Status
	_ = json.Unmarshal(raw, &result)
	return result
}

func persistRun(status Status, result Result) error {
	if status.TranscriptDir == "" {
		return nil
	}
	journalPath := filepath.Join(status.TranscriptDir, "journal.jsonl")
	journal, err := os.OpenFile(journalPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("persist workflow journal: %w", err)
	}
	encoder := json.NewEncoder(journal)
	for _, entry := range result.Journal {
		if err := encoder.Encode(map[string]any{"version": 1, "type": "result", "call": entry.Call, "key": entry.Key, "request": entry.Request, "response": entry.Response}); err != nil {
			_ = journal.Close()
			return fmt.Errorf("persist workflow journal: %w", err)
		}
		if err := persistAgentTranscript(status.TranscriptDir, entry); err != nil {
			_ = journal.Close()
			return err
		}
	}
	if err := journal.Close(); err != nil {
		return fmt.Errorf("persist workflow journal: %w", err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("persist workflow result: %w", err)
	}
	if err := os.WriteFile(status.OutputPath, encoded, 0o600); err != nil {
		return fmt.Errorf("persist workflow result: %w", err)
	}
	return nil
}

func persistAgentTranscript(directory string, entry JournalEntry) error {
	path := filepath.Join(directory, fmt.Sprintf("agent-%d.jsonl", entry.Call))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("persist workflow agent transcript: %w", err)
	}
	encoder := json.NewEncoder(file)
	if len(entry.Response.Transcript) == 0 {
		err = encoder.Encode(map[string]any{
			"version": 1, "type": "result", "call": entry.Call,
			"request": entry.Request, "response": entry.Response,
		})
	} else {
		for index, message := range entry.Response.Transcript {
			if err = encoder.Encode(map[string]any{
				"version": 1, "type": "message", "index": index, "message": message,
			}); err != nil {
				break
			}
		}
	}
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("persist workflow agent transcript: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("persist workflow agent transcript: %w", err)
	}
	return nil
}

func fileName(name string) string {
	var result strings.Builder
	for _, value := range strings.ToLower(name) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-' || value == '_' {
			result.WriteRune(value)
		} else if result.Len() > 0 && result.String()[result.Len()-1] != '-' {
			result.WriteByte('-')
		}
	}
	value := strings.Trim(result.String(), "-_")
	if value == "" {
		return "workflow"
	}
	return value
}
