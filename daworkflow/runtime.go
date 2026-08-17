//go:build !tinygo

package daworkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/semistrict/dago/internal/quickjs"
)

type workflowHostResponse struct {
	OK     bool  `json:"ok"`
	Value  any   `json:"value"`
	Tokens int64 `json:"tokens"`
}

type preparedWorkflow struct {
	meta   Meta
	shared *workflowShared
	engine *quickjs.Engine
	module *quickjs.Module
}

func runWorkflow(ctx context.Context, runner AgentRunner, script string, args any, options Options) (Result, error) {
	prepared, err := prepareWorkflowRun(ctx, runner, script, args, options)
	if err != nil {
		return Result{}, fmt.Errorf("prepare workflow: %w", err)
	}
	defer prepared.close()
	return completeWorkflowRun(ctx, prepared)
}

func prepareWorkflowRun(ctx context.Context, runner AgentRunner, script string, args any, options Options) (*preparedWorkflow, error) {
	shared := newWorkflowShared(runner, options)
	return prepareWorkflow(ctx, shared, script, args, 0, 0)
}

func completeWorkflowRun(ctx context.Context, prepared *preparedWorkflow) (Result, error) {
	value, logs, err := prepared.run(ctx)
	events, journal, calls, tokens := prepared.shared.snapshot()
	result := Result{Version: 1, Meta: prepared.meta, Value: value, Logs: logs, Events: events, Journal: journal, AgentCalls: calls, Tokens: tokens}
	if err != nil {
		name := prepared.meta.Name
		if name == "" {
			name = "<unknown>"
		}
		return result, fmt.Errorf("run workflow %q: %w", name, err)
	}
	return result, nil
}

func executeWorkflow(ctx context.Context, shared *workflowShared, script string, args any, depth, callBase int) (Meta, any, []string, error) {
	prepared, err := prepareWorkflow(ctx, shared, script, args, depth, callBase)
	if err != nil {
		return Meta{}, nil, nil, err
	}
	defer prepared.close()
	value, logs, err := prepared.run(ctx)
	return prepared.meta, value, logs, err
}

func prepareWorkflow(ctx context.Context, shared *workflowShared, script string, args any, depth, callBase int) (*preparedWorkflow, error) {
	if depth > 1 {
		return nil, fmt.Errorf("workflow nesting exceeds one level")
	}
	hosts := map[string]quickjs.HostFunction{}
	replay := newWorkflowReplayGate()
	hosts["__workflow_events"] = func(ctx context.Context, arguments []any) (any, error) {
		if len(arguments) != 1 {
			return nil, fmt.Errorf("workflow event batch requires one argument")
		}
		for _, raw := range workflowSlice(arguments[0]) {
			entry, _ := raw.(map[string]any)
			event := Event{Kind: workflowString(entry["kind"]), Phase: workflowString(entry["phase"]), Message: workflowString(entry["message"])}
			if event.Kind != "phase" && event.Kind != "log" {
				return nil, fmt.Errorf("invalid workflow event kind %q", event.Kind)
			}
			if err := shared.emit(ctx, event); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	hosts["__workflow_agent"] = func(ctx context.Context, arguments []any) (any, error) {
		if len(arguments) != 3 {
			return nil, fmt.Errorf("agent requires call number, prompt, and options")
		}
		call := callBase + workflowInt(arguments[0])
		request, err := workflowAgentRequest(arguments[1], arguments[2])
		if err != nil {
			return nil, err
		}
		response, err := shared.runAgent(ctx, call, workflowInt(arguments[0]), request, replay)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": response.OK, "value": response.Value, "tokens": response.Tokens}, nil
	}
	hosts["__workflow_nested"] = func(ctx context.Context, arguments []any) (any, error) {
		if depth >= 1 {
			return nil, fmt.Errorf("nested workflows cannot invoke another workflow")
		}
		if shared.opts.Resolver == nil {
			return nil, fmt.Errorf("workflow resolver is not configured")
		}
		if len(arguments) < 1 || len(arguments) > 2 {
			return nil, fmt.Errorf("workflow requires a reference and optional arguments")
		}
		reference := workflowReference(arguments[0])
		if reference == "" {
			return nil, fmt.Errorf("workflow reference is required")
		}
		source, err := shared.opts.Resolver.ResolveWorkflow(ctx, reference)
		if err != nil {
			return nil, fmt.Errorf("resolve workflow %q: %w", reference, err)
		}
		var childArgs any
		if len(arguments) == 2 {
			childArgs = arguments[1]
		}
		childID := int(shared.nesting.Add(1))
		_, value, _, err := executeWorkflow(ctx, shared, source, childArgs, depth+1, childID*1_000_000)
		return value, err
	}
	engine, err := quickjs.New(ctx, nil, quickjs.Options{
		MemoryLimit: shared.opts.MemoryLimit, StackLimit: shared.opts.StackLimit,
		Timeout: shared.opts.Timeout, HostFunctions: hosts,
	})
	if err != nil {
		return nil, fmt.Errorf("create workflow JavaScript runtime: %w", err)
	}
	fail := func(err error) (*preparedWorkflow, error) {
		_ = engine.Close(context.Background())
		return nil, err
	}
	prelude, err := workflowProgram(args, shared.opts)
	if err != nil {
		return fail(err)
	}
	if _, err := engine.Eval(ctx, prelude, shared.opts.Timeout); err != nil {
		return fail(err)
	}
	module, err := engine.LoadWorkflowModule(ctx, script, shared.opts.Timeout)
	if err != nil {
		return fail(err)
	}
	rawMeta, _, err := module.Export(ctx, "meta")
	if err != nil {
		module.Close(context.Background())
		return fail(fmt.Errorf("read workflow metadata export: %w", err))
	}
	meta, err := decodeWorkflowMeta(rawMeta)
	if err != nil {
		module.Close(context.Background())
		return fail(err)
	}
	return &preparedWorkflow{meta: meta, shared: shared, engine: engine, module: module}, nil
}

func (prepared *preparedWorkflow) run(ctx context.Context) (any, []string, error) {
	outcome, err := prepared.module.AwaitExport(ctx, "__dago_workflow_result", prepared.shared.opts.Timeout)
	if err != nil {
		return nil, nil, err
	}
	wrapped, ok := outcome.Value.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("workflow returned invalid runtime envelope %T", outcome.Value)
	}
	logs := make([]string, 0)
	for _, value := range workflowSlice(wrapped["logs"]) {
		logs = append(logs, workflowString(value))
	}
	return wrapped["value"], logs, nil
}

func (prepared *preparedWorkflow) close() {
	if prepared == nil {
		return
	}
	if prepared.module != nil {
		prepared.module.Close(context.Background())
		prepared.module = nil
	}
	if prepared.engine != nil {
		_ = prepared.engine.Close(context.Background())
		prepared.engine = nil
	}
}

func workflowProgram(args any, options Options) (string, error) {
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode workflow arguments: %w", err)
	}
	if string(encodedArgs) == "" {
		encodedArgs = []byte("null")
	}
	total := options.TokenBudget
	program := fmt.Sprintf(`(() => {
const __workflowState = { phase: '', logs: [], events: [], call: 0, spent: 0 };
const __workflowArgs = Object.freeze(%s);
function phase(title) {
  __workflowState.phase = String(title);
  __workflowState.events.push({kind: 'phase', phase: __workflowState.phase});
}
function log(message) {
  const text = String(message);
  __workflowState.logs.push(text);
  __workflowState.events.push({kind: 'log', phase: __workflowState.phase, message: text});
}
async function __flushWorkflowEvents() {
  if (__workflowState.events.length) await __workflow_events(__workflowState.events.splice(0));
}
async function agent(prompt, opts = {}) {
  await __flushWorkflowEvents();
  const selected = Object.assign({}, opts);
  if (!selected.phase && __workflowState.phase) selected.phase = __workflowState.phase;
  if (%d > 0 && __workflowState.spent >= %d) throw new Error('workflow token budget exhausted');
  const response = await __workflow_agent(++__workflowState.call, String(prompt), selected);
  if (!response || !response.ok) return null;
  __workflowState.spent += Number(response.tokens || 0);
  return response.value;
}
async function parallel(thunks) {
  if (!Array.isArray(thunks)) throw new TypeError('parallel requires an array');
  if (thunks.length > %d) throw new RangeError('parallel item limit exceeded');
  return Promise.all(thunks.map(async thunk => { try { return await thunk(); } catch (_) { return null; } }));
}
async function pipeline(items, ...stages) {
  if (!Array.isArray(items)) throw new TypeError('pipeline requires an array');
  if (items.length > %d) throw new RangeError('pipeline item limit exceeded');
  return Promise.all(items.map(async (item, index) => {
    let previous = item;
    try {
      for (const stage of stages) previous = await stage(previous, item, index);
      return previous;
    } catch (_) { return null; }
  }));
}
async function workflow(reference, childArgs = null) { return __workflow_nested(reference, childArgs); }
const __workflowBudget = Object.freeze({
  total: %d,
  spent: () => __workflowState.spent,
  remaining: () => %d > 0 ? Math.max(0, %d - __workflowState.spent) : Infinity,
});
Date.now = () => { throw new Error('Date.now() is unavailable in workflows; pass time through args'); };
Math.random = () => { throw new Error('Math.random() is unavailable in workflows'); };
const __workflowDate = Date;
globalThis.Date = function(...values) {
  if (values.length === 0) throw new Error('argumentless Date is unavailable in workflows; pass time through args');
  return new __workflowDate(...values);
};
globalThis.Date.prototype = __workflowDate.prototype;
globalThis.Date.parse = __workflowDate.parse;
globalThis.Date.UTC = __workflowDate.UTC;
globalThis.Date.now = () => { throw new Error('Date.now() is unavailable in workflows; pass time through args'); };
Object.defineProperties(globalThis, {
  args: {value: __workflowArgs},
  phase: {value: phase},
  log: {value: log},
  agent: {value: agent},
  parallel: {value: parallel},
  pipeline: {value: pipeline},
  workflow: {value: workflow},
  budget: {value: __workflowBudget},
});
globalThis.__dago_run_workflow = async body => {
  await Promise.resolve();
  const value = await body();
  await __flushWorkflowEvents();
  return {value, logs: __workflowState.logs};
};
})()
`, encodedArgs, total, total, options.MaxItems, options.MaxItems, total, total, total)
	return program, nil
}

func decodeWorkflowMeta(value any) (Meta, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return Meta{}, fmt.Errorf("encode workflow metadata export: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var meta Meta
	if err := decoder.Decode(&meta); err != nil {
		return Meta{}, fmt.Errorf("decode workflow metadata export: %w", err)
	}
	if strings.TrimSpace(meta.Name) == "" || strings.TrimSpace(meta.Description) == "" {
		return Meta{}, fmt.Errorf("workflow metadata name and description are required")
	}
	seen := map[string]bool{}
	for index, phase := range meta.Phases {
		if strings.TrimSpace(phase.Title) == "" {
			return Meta{}, fmt.Errorf("workflow metadata phase %d requires a title", index)
		}
		if seen[phase.Title] {
			return Meta{}, fmt.Errorf("workflow metadata has duplicate phase %q", phase.Title)
		}
		seen[phase.Title] = true
	}
	return meta, nil
}

type workflowReplayGate struct {
	mu   sync.Mutex
	cond *sync.Cond
	next int
	live bool
}

func newWorkflowReplayGate() *workflowReplayGate {
	gate := &workflowReplayGate{next: 1}
	gate.cond = sync.NewCond(&gate.mu)
	return gate
}

func (gate *workflowReplayGate) cached(shared *workflowShared, localCall, absoluteCall int, key string) (JournalEntry, bool) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for localCall != gate.next {
		gate.cond.Wait()
	}
	entry, exists := shared.resume[absoluteCall]
	cached := !gate.live && exists && entry.Key == key
	if !cached {
		gate.live = true
	}
	gate.next++
	gate.cond.Broadcast()
	return entry, cached
}

func (shared *workflowShared) runAgent(ctx context.Context, call, localCall int, request AgentRequest, replay *workflowReplayGate) (workflowHostResponse, error) {
	key, err := workflowRequestKey(request)
	if err != nil {
		return workflowHostResponse{}, err
	}
	if cached, ok := replay.cached(shared, localCall, call, key); ok {
		shared.mu.Lock()
		shared.journal[call] = cached
		shared.mu.Unlock()
		if err := shared.emit(ctx, Event{Kind: "agent_finished", Phase: request.Phase, Label: request.Label, Call: call, Tokens: cached.Response.Tokens, Cached: true}); err != nil {
			return workflowHostResponse{}, err
		}
		return workflowHostResponse{OK: true, Value: normalizeWorkflowValue(cached.Response.Value), Tokens: cached.Response.Tokens}, nil
	}
	shared.mu.Lock()
	if shared.opts.TokenBudget > 0 && shared.tokens >= shared.opts.TokenBudget {
		shared.mu.Unlock()
		return workflowHostResponse{}, ErrBudgetExhausted
	}
	if shared.agentCalls >= shared.opts.MaxAgents {
		shared.mu.Unlock()
		return workflowHostResponse{}, fmt.Errorf("workflow agent limit exceeded: %d", shared.opts.MaxAgents)
	}
	shared.agentCalls++
	shared.mu.Unlock()

	select {
	case shared.sem <- struct{}{}:
		defer func() { <-shared.sem }()
	case <-ctx.Done():
		return workflowHostResponse{}, ctx.Err()
	}
	if err := shared.emit(ctx, Event{Kind: "agent_started", Phase: request.Phase, Label: request.Label, Call: call}); err != nil {
		return workflowHostResponse{}, err
	}

	compiled, err := compileWorkflowSchema(request.Schema)
	if err != nil {
		return workflowHostResponse{}, err
	}
	var response AgentResponse
	var attemptTokens int64
	var progressMu sync.Mutex
	var reportedTokens int64
	for attempt := 0; attempt <= shared.opts.SchemaRetries; attempt++ {
		request.ReportTokens = func(tokens int64) {
			if tokens < 0 {
				return
			}
			progressMu.Lock()
			cumulative := attemptTokens + tokens
			if cumulative <= reportedTokens {
				progressMu.Unlock()
				return
			}
			reportedTokens = cumulative
			progressMu.Unlock()
			_ = shared.emit(ctx, Event{Kind: "agent_progress", Phase: request.Phase, Label: request.Label, Call: call, Tokens: cumulative})
		}
		response, err = shared.runner.RunWorkflowAgent(ctx, request)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return workflowHostResponse{}, err
			}
			if response.Tokens < 0 {
				return workflowHostResponse{}, fmt.Errorf("workflow agent returned negative token usage")
			}
			shared.recordAgent(call, key, request, AgentResponse{Tokens: attemptTokens + response.Tokens})
			_ = shared.emit(ctx, Event{Kind: "agent_failed", Phase: request.Phase, Label: request.Label, Message: err.Error(), Call: call, Tokens: attemptTokens + response.Tokens})
			return workflowHostResponse{OK: false}, nil
		}
		if response.Tokens < 0 {
			return workflowHostResponse{}, fmt.Errorf("workflow agent returned negative token usage")
		}
		attemptTokens += response.Tokens
		response.Value = normalizeWorkflowValue(response.Value)
		if compiled == nil || compiled.Validate(response.Value) == nil {
			break
		}
		if attempt == shared.opts.SchemaRetries {
			shared.recordAgent(call, key, request, AgentResponse{Tokens: attemptTokens})
			_ = shared.emit(ctx, Event{Kind: "agent_failed", Phase: request.Phase, Label: request.Label, Message: "structured result did not match schema", Call: call, Tokens: attemptTokens})
			return workflowHostResponse{OK: false}, nil
		}
	}
	response.Tokens = attemptTokens
	shared.recordAgent(call, key, request, response)
	if err := shared.emit(ctx, Event{Kind: "agent_finished", Phase: request.Phase, Label: request.Label, Call: call, Tokens: response.Tokens}); err != nil {
		return workflowHostResponse{}, err
	}
	return workflowHostResponse{OK: true, Value: response.Value, Tokens: response.Tokens}, nil
}

func (shared *workflowShared) recordAgent(call int, key string, request AgentRequest, response AgentResponse) {
	entry := JournalEntry{Version: 1, Call: call, Key: key, Request: request, Response: response}
	shared.mu.Lock()
	shared.tokens += response.Tokens
	shared.journal[call] = entry
	shared.mu.Unlock()
}

func workflowAgentRequest(prompt, rawOptions any) (AgentRequest, error) {
	request := AgentRequest{Prompt: workflowString(prompt)}
	options, _ := rawOptions.(map[string]any)
	request.Label = workflowString(options["label"])
	request.Phase = workflowString(options["phase"])
	request.Model = workflowString(options["model"])
	request.Effort = workflowString(options["effort"])
	request.Isolation = workflowString(options["isolation"])
	request.AgentType = workflowString(options["agentType"])
	if request.Prompt == "" {
		return request, fmt.Errorf("workflow agent prompt is required")
	}
	if request.Effort != "" && !workflowOneOf(request.Effort, "low", "medium", "high", "xhigh", "max") {
		return request, fmt.Errorf("unsupported workflow effort %q", request.Effort)
	}
	if request.Isolation != "" && request.Isolation != "worktree" {
		return request, fmt.Errorf("unsupported workflow isolation %q", request.Isolation)
	}
	if schema, exists := options["schema"]; exists && schema != nil {
		encoded, err := json.Marshal(schema)
		if err != nil || !json.Valid(encoded) {
			return request, fmt.Errorf("workflow agent schema is invalid")
		}
		request.Schema = encoded
	}
	return request, nil
}

func workflowRequestKey(request AgentRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func compileWorkflowSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("compile workflow agent schema: %w", err)
	}
	if reference := externalWorkflowSchemaReference(document); reference != "" {
		return nil, fmt.Errorf("external workflow schema reference %q is not allowed", reference)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const location = "urn:dago:workflow-agent"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, fmt.Errorf("compile workflow agent schema: %w", err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("compile workflow agent schema: %w", err)
	}
	return compiled, nil
}

func externalWorkflowSchemaReference(value any) string {
	switch value := value.(type) {
	case map[string]any:
		if ref, _ := value["$ref"].(string); ref != "" && !strings.HasPrefix(ref, "#") {
			return ref
		}
		for _, item := range value {
			if ref := externalWorkflowSchemaReference(item); ref != "" {
				return ref
			}
		}
	case []any:
		for _, item := range value {
			if ref := externalWorkflowSchemaReference(item); ref != "" {
				return ref
			}
		}
	}
	return ""
}

func workflowReference(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if object, ok := value.(map[string]any); ok {
		return workflowString(object["scriptPath"])
	}
	return ""
}

func workflowSlice(value any) []any {
	values, _ := value.([]any)
	return values
}

func workflowString(value any) string {
	text, _ := value.(string)
	return text
}

func workflowInt(value any) int {
	switch value := value.(type) {
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func workflowOneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}
