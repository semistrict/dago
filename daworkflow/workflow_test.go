//go:build !tinygo

package daworkflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
)

func TestWorkflowRunsParallelBarrierAndPipelineWithStructuredAgents(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	wantConcurrency := int32(min(2, max(1, runtime.GOMAXPROCS(0)-2)))
	release := make(chan struct{})
	var releaseOnce sync.Once
	runner := AgentFunc(func(ctx context.Context, request AgentRequest) (AgentResponse, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		if len(request.Schema) == 0 || request.Phase != "Scan" || !strings.HasPrefix(request.Label, "scan:") ||
			request.Model != "fast" || request.Effort != "high" || request.Isolation != "worktree" || request.AgentType != "reviewer" {
			return AgentResponse{}, errors.New("structured scan options missing")
		}
		if wantConcurrency > 1 {
			if current >= wantConcurrency {
				releaseOnce.Do(func() { close(release) })
			}
			select {
			case <-release:
			case <-time.After(3 * time.Second):
				return AgentResponse{}, errors.New("parallel agents did not overlap")
			case <-ctx.Done():
				return AgentResponse{}, ctx.Err()
			}
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return AgentResponse{}, ctx.Err()
		}
		return AgentResponse{Value: map[string]any{"finding": request.Prompt}, Tokens: 7}, nil
	})
	script := `export const meta = {
  name: 'scan-verify',
  description: 'scan areas',
  phases: [{title: 'Scan', detail: 'three scanners'}],
}
const schema = {
  type: 'object', required: ['finding'],
  properties: {finding: {type: 'string'}},
}
phase('Scan')
const scans = await parallel(args.areas.map(area => () =>
  agent(area, {label: 'scan:' + area, phase: 'Scan', model: 'fast', effort: 'high', isolation: 'worktree', agentType: 'reviewer', schema})))
const shaped = await pipeline(scans, async (scan, original, index) => ({...scan, index}))
log(shaped.length + ' scans')
return shaped`

	result, err := New(runner, Options{MaxConcurrency: 2}).Run(t.Context(), script, map[string]any{"areas": []string{"core", "ui", "tests"}})
	if err != nil {
		t.Fatal(err)
	}
	values, ok := result.Value.([]any)
	if !ok || len(values) != 3 || result.AgentCalls != 3 || result.Tokens != 21 {
		t.Fatalf("result = %#v", result)
	}
	if maximum.Load() != wantConcurrency {
		t.Fatalf("maximum concurrency = %d, want %d", maximum.Load(), wantConcurrency)
	}
	if len(result.Logs) != 1 || result.Logs[0] != "3 scans" {
		t.Fatalf("logs = %#v", result.Logs)
	}
	if result.Meta.Name != "scan-verify" || len(result.Journal) != 3 {
		t.Fatalf("metadata/journal = %#v / %#v", result.Meta, result.Journal)
	}
}

func TestWorkflowEmitsLiveAgentTokenProgress(t *testing.T) {
	runner := AgentFunc(func(_ context.Context, request AgentRequest) (AgentResponse, error) {
		if request.ReportTokens == nil {
			return AgentResponse{}, errors.New("token reporter is missing")
		}
		request.ReportTokens(12)
		request.ReportTokens(37)
		return AgentResponse{Value: "done", Tokens: 41}, nil
	})
	result, err := New(runner, Options{}).Run(t.Context(), `export const meta = {name: 'progress', description: 'report live worker usage'}
return agent('work', {label: 'scan:core', phase: 'Scan'})`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var progress []Event
	for _, event := range result.Events {
		if event.Kind == "agent_progress" {
			progress = append(progress, event)
		}
		if event.Timestamp == "" {
			t.Fatalf("event has no timestamp: %#v", event)
		}
	}
	if len(progress) != 2 || progress[0].Tokens != 12 || progress[1].Tokens != 37 || progress[1].Label != "scan:core" {
		t.Fatalf("progress = %#v", progress)
	}
	finished := result.Events[len(result.Events)-1]
	if finished.Kind != "agent_finished" || finished.Tokens != 41 {
		t.Fatalf("finished = %#v", finished)
	}
}

func TestWorkflowSchemaRetryAndFailureBecomeValues(t *testing.T) {
	var calls atomic.Int32
	runner := AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		switch calls.Add(1) {
		case 1:
			return AgentResponse{Value: map[string]any{"count": "wrong"}}, nil
		case 2:
			return AgentResponse{Value: map[string]any{"count": 2}}, nil
		default:
			return AgentResponse{}, errors.New("terminal provider error")
		}
	})
	script := `export const meta = {name: 'schema', description: 'schema retry'}
const schema = {type: 'object', required: ['count'], properties: {count: {type: 'integer'}}}
const valid = await agent('retry', {schema})
const failed = await agent('fail')
return {valid, failed}`
	result, err := New(runner, Options{SchemaRetries: 1}).Run(t.Context(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := result.Value.(map[string]any)
	valid, _ := value["valid"].(map[string]any)
	if value["failed"] != nil || valid["count"] != int64(2) {
		t.Fatalf("value = %#v; events = %#v; calls = %d", value, result.Events, calls.Load())
	}
}

func TestWorkflowResumeStopsCachingAfterFirstChangedCall(t *testing.T) {
	firstCalls := atomic.Int32{}
	runner := AgentFunc(func(_ context.Context, request AgentRequest) (AgentResponse, error) {
		firstCalls.Add(1)
		return AgentResponse{Value: request.Prompt, Tokens: 1}, nil
	})
	base := `export const meta = {name: 'resume', description: 'resume calls'}
const first = await agent('first')
const second = await agent('second')
const third = await agent('third')
return [first, second, third]`
	initial, err := New(runner, Options{}).Run(t.Context(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstCalls.Load() != 3 {
		t.Fatalf("initial calls = %d", firstCalls.Load())
	}
	var resumedCalls atomic.Int32
	resumeRunner := AgentFunc(func(_ context.Context, request AgentRequest) (AgentResponse, error) {
		resumedCalls.Add(1)
		return AgentResponse{Value: request.Prompt, Tokens: 2}, nil
	})
	changed := strings.Replace(base, "agent('second')", "agent('changed')", 1)
	resumed, err := New(resumeRunner, Options{Resume: initial.Journal}).Run(t.Context(), changed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resumedCalls.Load() != 2 || resumed.AgentCalls != 2 || resumed.Tokens != 4 {
		t.Fatalf("resumed calls/result = %d / %#v", resumedCalls.Load(), resumed)
	}
	values := resumed.Value.([]any)
	if values[0] != "first" || values[1] != "changed" || values[2] != "third" {
		t.Fatalf("resumed value = %#v", values)
	}
}

func TestWorkflowNestedWorkflowAndDeterminismGuards(t *testing.T) {
	runner := AgentFunc(func(_ context.Context, request AgentRequest) (AgentResponse, error) {
		return AgentResponse{Value: request.Prompt}, nil
	})
	child := `export const meta = {name: 'child', description: 'nested child'}
return agent('child:' + args.value)`
	resolver := ResolverFunc(func(_ context.Context, reference string) (string, error) {
		if reference != "child" {
			return "", errors.New("not found")
		}
		return child, nil
	})
	parent := `export const meta = {name: 'parent', description: 'nested parent'}
return workflow('child', {value: 42})`
	result, err := New(runner, Options{Resolver: resolver}).Run(t.Context(), parent, nil)
	if err != nil || result.Value != "child:42" {
		t.Fatalf("nested result = %#v, %v", result, err)
	}
	nondeterministic := `export const meta = {name: 'clock', description: 'reject clock'}
return Date.now()`
	if _, err := New(runner, Options{}).Run(t.Context(), nondeterministic, nil); err == nil || !strings.Contains(err.Error(), "Date.now") {
		t.Fatalf("Date.now error = %v", err)
	}
}

func TestWorkflowMetadataIsReadFromRuntimeModuleExport(t *testing.T) {
	script := `export const meta = {
  name: ['runtime', 'meta'].join('-'),
  description: ` + "`evaluated ${1 + 1}`" + `,
  phases: [{title: 'Run'}],
}
await Promise.resolve()
return meta.name`
	result, err := New(AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, nil
	}), Options{}).Run(t.Context(), script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Meta.Name != "runtime-meta" || result.Meta.Description != "evaluated 2" || result.Value != "runtime-meta" {
		t.Fatalf("result = %#v", result)
	}
}

func TestWorkflowRejectsExportDefaultMainWrapper(t *testing.T) {
	script := `export const meta = {name: 'wrapped', description: 'invalid wrapper'}
export default async function main() { return 'done' }`
	_, err := New(AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, nil
	}), Options{}).Run(t.Context(), script, nil)
	if err == nil || !strings.Contains(err.Error(), "SyntaxError") {
		t.Fatalf("wrapper error = %v", err)
	}
}

func TestWorkflowEnforcesBudgetAndCollectionLimits(t *testing.T) {
	runner := AgentFunc(func(_ context.Context, request AgentRequest) (AgentResponse, error) {
		return AgentResponse{Value: request.Prompt, Tokens: 5}, nil
	})
	budgeted := `export const meta = {name: 'budget', description: 'budget cap'}
await agent('first')
return agent('second')`
	if _, err := New(runner, Options{TokenBudget: 5}).Run(t.Context(), budgeted, nil); err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("budget error = %v", err)
	}
	oversized := `export const meta = {name: 'items', description: 'item cap'}
return parallel([() => 1, () => 2])`
	if _, err := New(runner, Options{MaxItems: 1}).Run(t.Context(), oversized, nil); err == nil || !strings.Contains(err.Error(), "item limit") {
		t.Fatalf("item limit error = %v", err)
	}
	agentLimited := `export const meta = {name: 'agents', description: 'agent cap'}
await agent('first')
return agent('second')`
	if _, err := New(runner, Options{MaxAgents: 1}).Run(t.Context(), agentLimited, nil); err == nil || !strings.Contains(err.Error(), "agent limit") {
		t.Fatalf("agent limit error = %v", err)
	}
}

func TestManagerStartsAndCancelsBackgroundRun(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	runner := AgentFunc(func(ctx context.Context, _ AgentRequest) (AgentResponse, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return AgentResponse{}, ctx.Err()
	})
	manager := NewManager(runner, Options{})
	t.Cleanup(func() { _ = manager.Close() })
	middleware := Middleware(manager)
	if middleware.Name != "workflows" || middleware.SerializedName != "WorkflowMiddleware" || len(middleware.Tools) != 4 || middleware.WrapModelCall == nil {
		t.Fatalf("middleware = %#v", middleware)
	}
	status, err := manager.Start(t.Context(), StartRequest{Script: `export const meta = {name: 'background', description: 'wait', phases: [{title: 'Hold'}]}
return agent('wait')`})
	if err != nil || status.Status != "running" || !strings.HasPrefix(status.RunID, "wf_") || status.Description != "wait" || len(status.Phases) != 1 || status.Phases[0].Title != "Hold" {
		t.Fatalf("start = %#v, %v", status, err)
	}
	if manager.Running() != 1 {
		t.Fatalf("running workflows = %d, want 1", manager.Running())
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("workflow agent did not start")
	}
	if !manager.Cancel(status.RunID) {
		t.Fatal("cancel returned false")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := manager.Check(status.RunID)
		if current.Status == "cancelled" {
			if manager.Running() != 0 {
				t.Fatalf("running workflows after cancellation = %d", manager.Running())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow did not become cancelled: %#v", manager.List())
}

func TestWorkflowToolDescriptionDocumentsAuthoringContract(t *testing.T) {
	manager := NewManager(AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, nil
	}), Options{})
	t.Cleanup(func() { _ = manager.Close() })
	description := manager.Tools()[0].Definition().Description
	for _, required := range []string{
		"WHEN TO USE",
		"SCOUT BEFORE AUTHORING",
		"Do not delegate the initial orientation",
		"exactly one of script, script_path, or name",
		"real module namespace",
		"metadata is not parsed",
		"Never add export default",
		"agent(prompt, opts?)",
		"never use an unconstrained {type: 'object'}",
		"throw an error instead of returning null",
		"parallel(thunks)",
		"pipeline(items, stage1, stage2, ...)",
		"phase(title)",
		"log(message)",
		"budget.remaining()",
		"workflow(nameOrRef, childArgs?)",
		"There is no workflow.task",
		"Date.now()",
		"BARRIER VERSUS PIPELINE",
		"min(16, available CPUs - 2)",
		"1000 live agent calls",
		"4096 items",
		"BACKGROUND EXECUTION AND ARTIFACTS",
		"journal.jsonl",
		"RESUME AND CACHING",
		`"resume_from_run_id":"wf_..."`,
		"Adversarial verify",
		"Perspective-diverse verify",
		"Judge panel",
		"Loop until dry",
		"Multi-modal sweep",
		"Completeness critic",
		"Loop until budget",
		"CANONICAL SCAN, DEDUPLICATE, AND VERIFY EXAMPLE",
		".filter(Boolean)",
	} {
		if !strings.Contains(description, required) {
			t.Errorf("workflow tool description does not document %q", required)
		}
	}
}

func TestManagerResolvesSavedWorkflowByName(t *testing.T) {
	var resolved string
	manager := NewManager(AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, nil
	}), Options{Resolver: ResolverFunc(func(_ context.Context, reference string) (string, error) {
		resolved = reference
		return `export const meta = {name: 'saved', description: 'saved workflow'}
return 'done'`, nil
	})})
	t.Cleanup(func() { _ = manager.Close() })
	status, err := manager.Start(t.Context(), StartRequest{Name: "refactor-analysis"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "refactor-analysis" || status.Name != "saved" {
		t.Fatalf("resolved = %q, status = %#v", resolved, status)
	}
}

func TestManagerMarksNullResultAfterAgentFailuresAsError(t *testing.T) {
	manager := NewManager(AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, errors.New("worker unavailable")
	}), Options{})
	t.Cleanup(func() { _ = manager.Close() })
	started, err := manager.Start(t.Context(), StartRequest{Script: `export const meta = {name: 'failed', description: 'all workers fail'}
return agent('inspect')`})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := manager.Check(started.RunID)
		if status.Status == "error" {
			if !strings.Contains(status.Error, "all 1 workflow agent calls failed") || !strings.Contains(status.Error, "worker unavailable") {
				t.Fatalf("status = %#v", status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow did not fail: %#v", manager.List())
}

func TestManagerMarksStructuredResultAsErrorWhenEveryAgentFails(t *testing.T) {
	manager := NewManager(AgentFunc(func(context.Context, AgentRequest) (AgentResponse, error) {
		return AgentResponse{}, errors.New("worker unavailable")
	}), Options{})
	t.Cleanup(func() { _ = manager.Close() })
	started, err := manager.Start(t.Context(), StartRequest{Script: `export const meta = {name: 'failed-set', description: 'all workers fail'}
const results = await parallel([() => agent('one'), () => agent('two')])
return {usable: results.filter(Boolean)}`})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := manager.Check(started.RunID)
		if status.Status == "error" {
			if !strings.Contains(status.Error, "all 2 workflow agent calls failed") {
				t.Fatalf("status = %#v", status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow did not fail: %#v", manager.List())
}

func TestManagerPersistsScriptJournalAndOutput(t *testing.T) {
	runner := AgentFunc(func(_ context.Context, request AgentRequest) (AgentResponse, error) {
		return AgentResponse{
			Value: "done:" + request.Prompt, Tokens: 3,
			Transcript: []damessage.Message{damessage.Human(request.Prompt), damessage.Assistant("done")},
		}, nil
	})
	completed := make(chan Status, 1)
	manager := NewManager(runner, Options{
		SessionDirectory: t.TempDir(),
		Completed: func(_ context.Context, status Status) {
			completed <- status
		},
	})
	t.Cleanup(func() { _ = manager.Close() })
	status, err := manager.Start(t.Context(), StartRequest{Script: `export const meta = {name: 'persist me', description: 'persist artifacts'}
return agent('work')`})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := manager.Check(status.RunID)
		if current.Status == "success" {
			select {
			case notified := <-completed:
				if notified.RunID != current.RunID || notified.Status != "success" {
					t.Fatalf("completion notification = %#v", notified)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("completion notification was not delivered")
			}
			for _, path := range []string{current.ScriptPath, filepath.Join(current.TranscriptDir, "journal.jsonl"), filepath.Join(current.TranscriptDir, "agent-1.jsonl"), current.OutputPath} {
				if info, err := os.Stat(path); err != nil || info.IsDir() {
					t.Fatalf("persisted file %q = %v, %v", path, info, err)
				}
			}
			content, err := os.ReadFile(current.OutputPath)
			if err != nil || !strings.Contains(string(content), "done:work") {
				t.Fatalf("output = %q, %v", content, err)
			}
			transcript, err := os.ReadFile(filepath.Join(current.TranscriptDir, "agent-1.jsonl"))
			if err != nil || !strings.Contains(string(transcript), `"type":"message"`) || !strings.Contains(string(transcript), "done") {
				t.Fatalf("transcript = %q, %v", transcript, err)
			}
			return
		}
		if current.Status == "error" {
			t.Fatalf("workflow persistence failed: %#v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("workflow persistence did not complete")
}
