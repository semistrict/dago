package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daworkflow"
)

const maxWorkflowScriptBytes = 2 << 20

const workflowAgentFailurePrefix = "__workflow_agent_failed__:"

type workflowAgentFailureRequest struct {
	Reason string `json:"reason"`
}

type workspaceWorkflowResolver struct {
	root      string
	stateRoot string
}

func (resolver workspaceWorkflowResolver) ResolveWorkflow(ctx context.Context, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("workflow reference is required")
	}
	candidates := resolver.candidates(reference)
	for _, candidate := range candidates {
		source, found, err := readWorkflowFile(ctx, candidate, resolver.root, resolver.stateRoot)
		if err != nil {
			return "", err
		}
		if found {
			return source, nil
		}
	}
	return "", fmt.Errorf("workflow %q was not found", reference)
}

func (resolver workspaceWorkflowResolver) candidates(reference string) []string {
	if filepath.IsAbs(reference) {
		return []string{reference}
	}
	result := []string{filepath.Join(resolver.root, filepath.FromSlash(reference))}
	if !strings.Contains(reference, "/") && !strings.Contains(reference, string(filepath.Separator)) {
		name := reference
		if filepath.Ext(name) == "" {
			name += ".js"
		}
		result = append(result,
			filepath.Join(resolver.root, ".claude", "workflows", name),
			filepath.Join(resolver.root, ".agents", "workflows", name),
		)
	}
	if resolver.stateRoot != "" {
		result = append(result, filepath.Join(resolver.stateRoot, filepath.FromSlash(reference)))
	}
	return result
}

func readWorkflowFile(ctx context.Context, path string, allowedRoots ...string) (string, bool, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve workflow %q: %w", path, err)
	}
	allowed := false
	for _, root := range allowedRoots {
		if root != "" && pathWithin(root, resolved) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", false, fmt.Errorf("workflow %q is outside the workspace and state directories", path)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", false, fmt.Errorf("open workflow %q: %w", path, err)
	}
	defer file.Close()
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	default:
	}
	source, err := io.ReadAll(io.LimitReader(file, maxWorkflowScriptBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("read workflow %q: %w", path, err)
	}
	if len(source) > maxWorkflowScriptBytes {
		return "", false, fmt.Errorf("workflow %q exceeds %d bytes", path, maxWorkflowScriptBytes)
	}
	return string(source), true, nil
}

func pathWithin(root, candidate string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return false
	}
	resolvedCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type dacodeWorkflowAgentRunner struct {
	authentication modelAuthentication
	baseURL        string
	model          string
	backend        dabackend.Backend
	tools          []datool.Tool
	filesystem     dago.Filesystem
	skills         dago.Skills
	memory         dago.Memory
	system         string
	approvalRules  []dagent.ApprovalRule
	reviewer       *dagent.Agent
	workingDir     string
	nextThread     atomic.Uint64
}

type workflowTokenTracker struct {
	report func(int64)

	mu            sync.Mutex
	authoritative int64
	input         int64
	output        damessage.Message
}

func (tracker *workflowTokenTracker) beginModel(ctx context.Context, request dagent.ModelRequest) {
	messages := append([]damessage.Message(nil), request.Messages...)
	if request.SystemMessage != nil {
		messages = append([]damessage.Message{request.SystemMessage.Clone()}, messages...)
	}
	tokens := damessage.ApproximateTokens(messages)
	if counter, ok := request.Model.(damodel.TokenCounter); ok {
		if counted, err := counter.CountTokens(ctx, messages); err == nil && counted > 0 {
			tokens = counted
		}
	}
	tracker.mu.Lock()
	tracker.input = int64(tokens)
	tracker.output = damessage.Message{Role: damessage.RoleAssistant}
	total := tracker.authoritative + tracker.input
	tracker.mu.Unlock()
	tracker.report(total)
}

func (tracker *workflowTokenTracker) observe(event dagent.Event) {
	if event.Mode != dagent.EventToken || event.Chunk == nil {
		return
	}
	delta := event.Chunk.MessageDelta
	if delta.Usage != nil && delta.Usage.TotalTokens > 0 {
		tracker.mu.Lock()
		total := tracker.authoritative + int64(delta.Usage.TotalTokens)
		tracker.mu.Unlock()
		tracker.report(total)
		return
	}
	tracker.mu.Lock()
	tracker.output.Content = append(tracker.output.Content, delta.Content...)
	tracker.output.ToolCalls = append(tracker.output.ToolCalls, delta.ToolCalls...)
	output := damessage.ApproximateTokens([]damessage.Message{tracker.output})
	total := tracker.authoritative + tracker.input + int64(output)
	tracker.mu.Unlock()
	tracker.report(total)
}

func (tracker *workflowTokenTracker) finishModel(response dagent.ModelResponse) {
	usage := damessage.AggregateUsage(response.Messages)
	tracker.mu.Lock()
	tracker.authoritative += int64(usage.TotalTokens)
	tracker.input = 0
	tracker.output = damessage.Message{}
	total := tracker.authoritative
	tracker.mu.Unlock()
	tracker.report(total)
}

func (runner *dacodeWorkflowAgentRunner) RunWorkflowAgent(ctx context.Context, request daworkflow.AgentRequest) (daworkflow.AgentResponse, error) {
	if request.AgentType != "" && request.AgentType != "general" {
		return daworkflow.AgentResponse{}, fmt.Errorf("workflow agent type %q is not configured", request.AgentType)
	}
	if request.Isolation == "worktree" {
		return daworkflow.AgentResponse{}, fmt.Errorf("workflow worktree isolation is not available in dacode")
	}
	modelName := request.Model
	if modelName == "" {
		modelName = runner.model
	}
	model, err := runner.authentication.resolveModel(ctx, modelName, runner.baseURL)
	if err != nil {
		return daworkflow.AgentResponse{}, fmt.Errorf("resolve workflow model: %w", err)
	}
	system := runner.system + "\n\nYou are a workflow worker. Complete only the assigned prompt and return the requested value without addressing the user or coordinating additional agents. A denied tool call is evidence that only that action was refused: continue with a safe alternative when possible. If the task cannot produce a useful result, call fail_workflow_agent with a concise reason to end this worker as a failed branch."
	workerTools := append([]datool.Tool(nil), runner.tools...)
	workerTools = append(workerTools, workflowAgentFailureTool())
	options := []dago.Option{
		dago.WithName("workflow-agent"),
		dago.WithSystemPrompt(system),
		dago.WithBackend(runner.backend),
		dago.WithTools(workerTools...),
		dago.WithFilesystem(runner.filesystem),
		dago.WithSkills(runner.skills),
		dago.WithMemory(runner.memory),
		dago.WithoutSubagents(),
	}
	var tokenTracker *workflowTokenTracker
	if request.ReportTokens != nil {
		tokenTracker = &workflowTokenTracker{report: request.ReportTokens}
		options = append(options, dago.WithMiddleware(dagent.Middleware{
			Name: "workflow_token_progress",
			WrapModelCall: func(ctx context.Context, modelRequest dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
				tokenTracker.beginModel(ctx, modelRequest)
				response, err := next(ctx, modelRequest)
				if err == nil {
					tokenTracker.finishModel(response)
				}
				return response, err
			},
		}))
	}
	if len(runner.approvalRules) > 0 {
		options = append(options, dago.WithApprovalRules(runner.approvalRules...))
		options = append(options, dago.WithSaver(dacheckpoint.NewMemorySaver()))
	}
	if request.Effort != "" {
		effort := request.Effort
		options = append(options, dago.WithMiddleware(dagent.Middleware{
			Name: "workflow_reasoning",
			WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
				request.Reasoning = &damodel.Reasoning{Effort: effort}
				return next(ctx, request)
			},
		}))
	}
	if len(request.Schema) > 0 {
		options = append(options, dago.WithMiddleware(dagent.Middleware{
			Name: "workflow_structured_output",
			WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
				// Keep workflow schemas out of provider-native JSON mode. A malformed
				// provider response is otherwise rejected below the agent loop, before
				// HandleErrors can ask the model to correct it. Requiring a tool call
				// also prevents a plain-text response from silently ending the worker.
				request.ToolChoice = &damodel.ToolChoice{Mode: "required"}
				return next(ctx, request)
			},
		}))
		options = append(options, dago.WithStructuredOutput(&dagent.StructuredOutput{
			Name: "workflow_result", Description: "Return the workflow worker result.", Schema: request.Schema,
			Strategy: dagent.StructuredTool, Strict: true, HandleErrors: true,
		}))
	}
	agent := dago.NewAgent(model, options...)
	config := dacheckpoint.Config{ThreadID: fmt.Sprintf("workflow-agent-%d", runner.nextThread.Add(1))}
	result, err := streamWorkflowAgent(ctx, agent, dagent.Input{
		Config: config, Messages: []damessage.Message{damessage.Human(request.Prompt)}, SkipValueEvents: true,
	}, tokenTracker)
	if err != nil {
		return daworkflow.AgentResponse{}, err
	}
	for len(result.Interrupts) > 0 {
		if runner.reviewer == nil {
			return daworkflow.AgentResponse{}, fmt.Errorf("workflow agent requires tool approval; background approval requires auto-review mode")
		}
		if len(result.Interrupts) != 1 || result.Interrupts[0].ID != "human_approval" {
			return daworkflow.AgentResponse{}, fmt.Errorf("workflow agent returned unsupported interrupt %q", result.Interrupts[0].ID)
		}
		requests, err := decodeApprovalRequests(result.Interrupts[0].Value)
		if err != nil {
			return daworkflow.AgentResponse{}, fmt.Errorf("decode workflow approval request: %w", err)
		}
		transcript := "[user, trusted]\n" + request.Prompt + "\n"
		assessment, err := reviewApprovals(ctx, runner.reviewer, approvalReviewRequest{
			WorkingDir: runner.workingDir, Transcript: transcript, Requests: requests,
		})
		if err != nil {
			return daworkflow.AgentResponse{}, fmt.Errorf("review workflow tool approval: %w", err)
		}
		decisions := make(map[string]dagent.ApprovalChoice, len(requests))
		for _, pending := range requests {
			verdict, exists := assessment.Assessments[pending.Call.ID]
			if !exists {
				return daworkflow.AgentResponse{}, fmt.Errorf("workflow approval reviewer omitted %s", pending.Call.Name)
			}
			decision := dagent.ApprovalApprove
			if !verdict.approved() {
				decision = dagent.ApprovalReject
			}
			decisions[pending.Call.ID] = dagent.ApprovalChoice{
				Decision: decision, Reason: verdict.Rationale, Message: verdict.Rationale,
			}
		}
		result, err = streamWorkflowAgent(ctx, agent, dagent.Input{
			Config: config, Resume: dagent.ApprovalResponse{Decisions: decisions}, SkipValueEvents: true,
		}, tokenTracker)
		if err != nil {
			return daworkflow.AgentResponse{}, err
		}
	}
	usage := damessage.AggregateUsage(result.Messages)
	if reason, failed := workflowAgentFailure(result); failed {
		return daworkflow.AgentResponse{Tokens: int64(usage.TotalTokens)}, fmt.Errorf("workflow agent chose to fail: %s", reason)
	}
	value := any(result.String())
	if len(result.Structured) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(result.Structured)))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return daworkflow.AgentResponse{}, fmt.Errorf("decode workflow agent result: %w", err)
		}
	}
	transcript := make([]damessage.Message, len(result.Messages))
	for index := range result.Messages {
		transcript[index] = result.Messages[index].Clone()
	}
	return daworkflow.AgentResponse{Value: value, Tokens: int64(usage.TotalTokens), Transcript: transcript}, nil
}

func streamWorkflowAgent(ctx context.Context, agent *dagent.Agent, input dagent.Input, tracker *workflowTokenTracker) (dagent.Result, error) {
	stream := agent.Stream(ctx, input)
	defer stream.Close()
	for {
		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return dagent.Result{}, err
		}
		if tracker != nil {
			tracker.observe(event)
		}
	}
	return stream.Result(ctx)
}

func workflowAgentFailureTool() datool.Tool {
	tool := datool.MustNew("fail_workflow_agent", "End this workflow worker as a failed branch when the assigned task cannot produce a useful result. Use only after considering safe alternatives, and explain why the branch cannot continue.", func(_ context.Context, input workflowAgentFailureRequest) (string, error) {
		reason := strings.TrimSpace(input.Reason)
		if reason == "" {
			reason = "no useful result can be produced"
		}
		return workflowAgentFailurePrefix + reason, nil
	})
	definition := tool.Definition()
	definition.Direct = true
	return datool.Func{Spec: definition, Run: func(ctx context.Context, arguments json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
		return tool.Execute(ctx, arguments, runtime)
	}}
}

func workflowAgentFailure(result dagent.Result) (string, bool) {
	if len(result.Messages) == 0 {
		return "", false
	}
	last := result.Messages[len(result.Messages)-1]
	if last.Role != damessage.RoleTool {
		return "", false
	}
	text := last.TextContent()
	if !strings.HasPrefix(text, workflowAgentFailurePrefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(text, workflowAgentFailurePrefix)), true
}
