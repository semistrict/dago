package dacode

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daworkflow"
	"github.com/semistrict/dago/daworkspace"
)

const defaultModel = "gpt-5.6-terra"

const sessionWorkingDirectoryKey = "__dacode_working_directory"

const workspaceGuidancePrompt = `<workspace_instructions>
{agent_memory}
</workspace_instructions>

<workspace_instruction_rules>
These files contain project instructions. Follow the instructions that apply to each file you touch. More deeply scoped files take precedence, while direct user instructions take precedence over workspace files. Read listed subdirectory guidance before editing within its directory.
</workspace_instruction_rules>`

type eventStream interface {
	Next(context.Context) (dagent.Event, error)
	Result(context.Context) (dagent.Result, error)
	Close() error
}

type agentRunner interface {
	Start(context.Context, dagent.Input) eventStream
	Cancel(context.Context, string) error
	Review(context.Context, approvalReviewRequest) (approvalReviewResult, error)
	ListSessions(context.Context) ([]sessionInfo, error)
	LoadSession(context.Context, string) ([]damessage.Message, error)
	Goal(context.Context, string) (*dagoal.Goal, error)
	SetGoal(context.Context, string, dagoal.SetRequest) (*dagoal.Goal, error)
	ClearGoal(context.Context, string) (bool, error)
	StartWorkflow(context.Context, daworkflow.StartRequest) (daworkflow.Status, error)
	Workflows() []daworkflow.Status
	RunningWorkflows() int
	CancelWorkflow(string) bool
	WaitWorkflowCompletion(context.Context) (daworkflow.Status, bool)
}

type dagoRunner struct {
	agent      *dagent.Agent
	reviewer   *dagent.Agent
	saver      *checkpointsqlite.Saver
	database   *sql.DB
	workingDir string
	goals      *dagoal.Service
	workflows  *daworkflow.Manager
	completed  *workflowCompletionQueue
}

type workflowCompletionQueue struct {
	mu    sync.Mutex
	items []daworkflow.Status
	ready chan struct{}
}

func newWorkflowCompletionQueue() *workflowCompletionQueue {
	return &workflowCompletionQueue{ready: make(chan struct{}, 1)}
}

func (queue *workflowCompletionQueue) Push(status daworkflow.Status) {
	queue.mu.Lock()
	queue.items = append(queue.items, status)
	queue.mu.Unlock()
	select {
	case queue.ready <- struct{}{}:
	default:
	}
}

func (queue *workflowCompletionQueue) Wait(ctx context.Context) (daworkflow.Status, bool) {
	for {
		queue.mu.Lock()
		if len(queue.items) > 0 {
			status := queue.items[0]
			queue.items[0] = daworkflow.Status{}
			queue.items = queue.items[1:]
			more := len(queue.items) > 0
			queue.mu.Unlock()
			if more {
				select {
				case queue.ready <- struct{}{}:
				default:
				}
			}
			return status, true
		}
		queue.mu.Unlock()
		select {
		case <-queue.ready:
		case <-ctx.Done():
			return daworkflow.Status{}, false
		}
	}
}

func (runner *dagoRunner) Start(ctx context.Context, input dagent.Input) eventStream {
	state := input.State.Clone()
	if state == nil {
		state = dastate.Values{}
	}
	state[sessionWorkingDirectoryKey] = runner.workingDir
	input.State = state
	return runner.agent.Stream(ctx, input)
}

func (runner *dagoRunner) Cancel(ctx context.Context, threadID string) error {
	_, err := runner.agent.Cancel(ctx, dagent.Input{Config: dacheckpoint.Config{ThreadID: threadID}})
	return err
}

func (runner *dagoRunner) ListSessions(ctx context.Context) ([]sessionInfo, error) {
	rows, err := runner.database.QueryContext(ctx, `
SELECT thread_id, MAX(checkpoint_id)
FROM checkpoints
WHERE checkpoint_ns = ''
GROUP BY thread_id
ORDER BY MAX(checkpoint_id) DESC
LIMIT 50`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	type sessionRow struct {
		threadID, checkpointID string
	}
	var latest []sessionRow
	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(&row.threadID, &row.checkpointID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read session: %w", err)
		}
		latest = append(latest, row)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close session list: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	sessions := make([]sessionInfo, 0, len(latest))
	for _, row := range latest {
		tuple, err := runner.saver.GetTuple(ctx, dacheckpoint.Config{
			ThreadID: row.threadID, CheckpointID: row.checkpointID,
		})
		if err != nil {
			return nil, fmt.Errorf("read session %q: %w", row.threadID, err)
		}
		messages, err := runner.LoadSession(ctx, row.threadID)
		if err != nil {
			return nil, err
		}
		item := sessionInfo{ThreadID: row.threadID, MessageCount: len(messages)}
		if tuple != nil {
			item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, tuple.Checkpoint.Timestamp)
			item.Directory, _ = tuple.Checkpoint.ChannelValues[sessionWorkingDirectoryKey].(string)
		}
		for _, message := range messages {
			if message.Role == damessage.RoleHuman && strings.TrimSpace(message.TextContent()) != "" {
				item.Preview = strings.TrimSpace(message.TextContent())
				break
			}
		}
		sessions = append(sessions, item)
	}
	return sessions, nil
}

func (runner *dagoRunner) LoadSession(ctx context.Context, threadID string) ([]damessage.Message, error) {
	snapshot, err := runner.agent.State(ctx, dacheckpoint.Config{ThreadID: threadID})
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", threadID, err)
	}
	if snapshot.Config.ThreadID == "" {
		return nil, fmt.Errorf("session %q was not found", threadID)
	}
	return decodeSessionMessages(snapshot.State[dagent.MessagesKey])
}

func decodeSessionMessages(value any) ([]damessage.Message, error) {
	if value == nil {
		return nil, nil
	}
	if messages, ok := value.([]damessage.Message); ok {
		result := make([]damessage.Message, len(messages))
		for index := range messages {
			result[index] = messages[index].Clone()
		}
		return result, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("session messages have type %T", value)
	}
	result := make([]damessage.Message, len(items))
	for index, item := range items {
		message, ok := item.(damessage.Message)
		if !ok {
			return nil, fmt.Errorf("session message %d has type %T", index, item)
		}
		result[index] = message.Clone()
	}
	return result, nil
}

func (runner *dagoRunner) Goal(ctx context.Context, threadID string) (*dagoal.Goal, error) {
	return runner.goals.Get(ctx, dacheckpoint.Config{ThreadID: threadID})
}

func (runner *dagoRunner) SetGoal(ctx context.Context, threadID string, request dagoal.SetRequest) (*dagoal.Goal, error) {
	return runner.goals.Set(ctx, dacheckpoint.Config{ThreadID: threadID}, request)
}

func (runner *dagoRunner) ClearGoal(ctx context.Context, threadID string) (bool, error) {
	return runner.goals.Clear(ctx, dacheckpoint.Config{ThreadID: threadID})
}

func (runner *dagoRunner) StartWorkflow(ctx context.Context, request daworkflow.StartRequest) (daworkflow.Status, error) {
	return runner.workflows.Start(ctx, request)
}

func (runner *dagoRunner) Workflows() []daworkflow.Status { return runner.workflows.List() }

func (runner *dagoRunner) RunningWorkflows() int { return runner.workflows.Running() }

func (runner *dagoRunner) CancelWorkflow(runID string) bool { return runner.workflows.Cancel(runID) }

func (runner *dagoRunner) WaitWorkflowCompletion(ctx context.Context) (daworkflow.Status, bool) {
	return runner.completed.Wait(ctx)
}

type runnerOptions struct {
	Authentication modelAuthentication
	BaseURL        string
	Model          string
	WorkingDir     string
	StateDir       string
	ReviewTools    bool
	Shell          bool
	AutoReview     bool
	ReviewModel    string
	Tools          []datool.Tool
}

func newRunner(options runnerOptions) (agentRunner, io.Closer, error) {
	model := options.Authentication.model(options.Model, options.BaseURL)
	var err error

	var backend dabackend.Backend
	if options.Shell {
		backend, err = dabackend.NewLocalShell(dabackend.LocalShellOptions{
			Filesystem: dabackend.FilesystemOptions{Root: options.WorkingDir},
			InheritEnv: true,
		})
	} else {
		backend, err = dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: options.WorkingDir})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open workspace: %w", err)
	}

	if err := os.MkdirAll(options.StateDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create state directory: %w", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(options.StateDir, "threads.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("open session database: %w", err)
	}
	database.SetMaxOpenConns(1)
	saver := checkpointsqlite.New(database, nil)
	if err := saver.Setup(context.Background()); err != nil {
		_ = database.Close()
		return nil, nil, err
	}

	filesystem := dago.Filesystem{}
	var interruptOn []dagent.ApprovalRule
	if options.ReviewTools {
		interruptOn = mutatingToolApprovalRules()
	}

	memory, guidanceSummary := workspaceContext(options.WorkingDir)
	skills := workspaceSkills(options.WorkingDir, os.Getenv("HERDR_ENV"))
	var reviewer *dagent.Agent
	if options.AutoReview {
		reviewModel := options.Authentication.model(options.ReviewModel, options.BaseURL)
		var reviewErr error
		readOnly, reviewErr := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: options.WorkingDir})
		if reviewErr != nil {
			_ = database.Close()
			return nil, nil, fmt.Errorf("open review workspace: %w", reviewErr)
		}
		reviewer = newApprovalReviewer(reviewModel, readOnly)
	}

	systemText := `You are dacode, an interactive coding agent. Work as a careful senior engineer inside the configured workspace. Filesystem tool paths are virtual: use / for the workspace root and never pass a host filesystem path. Inspect relevant files before making claims, use the available filesystem and shell tools to complete requested work, preserve unrelated user changes, and verify edits with focused tests. Keep final responses concise and concrete.`
	systemText += guidanceSummary
	system := damessage.System(systemText)
	goalOptions := dagoal.Options{}
	workflowCompletions := newWorkflowCompletionQueue()
	workflowRunner := &dacodeWorkflowAgentRunner{
		authentication: options.Authentication, baseURL: options.BaseURL, model: options.Model,
		backend: backend, tools: append([]datool.Tool(nil), options.Tools...), filesystem: filesystem,
		skills: skills, memory: memory, system: systemText, approvalRules: interruptOn,
		reviewer: reviewer, workingDir: options.WorkingDir,
	}
	workflowManager := daworkflow.NewManager(workflowRunner, daworkflow.Options{
		Resolver:         workspaceWorkflowResolver{root: options.WorkingDir, stateRoot: options.StateDir},
		SessionDirectory: options.StateDir,
		Completed: func(_ context.Context, status daworkflow.Status) {
			workflowCompletions.Push(status)
		},
	})
	agent := dago.NewAgent(
		model,
		dago.WithName("dacode"),
		dago.WithSystemMessage(system),
		dago.WithBackend(backend),
		dago.WithTools(options.Tools...),
		dago.WithFilesystem(filesystem),
		dago.WithSkills(skills),
		dago.WithMemory(memory),
		dago.WithTodo(),
		dago.WithMiddleware(dagoal.Middleware(goalOptions), daworkflow.Middleware(workflowManager)),
		dago.WithApprovalRules(interruptOn...),
		dago.WithSaver(saver),
		dago.WithRetainedThreadState(),
		dago.WithStateFields(sessionStateFields()),
	)
	runner := &dagoRunner{
		agent: agent, reviewer: reviewer, saver: saver, database: database, workingDir: options.WorkingDir,
		goals: dagoal.NewService(agent, goalOptions), workflows: workflowManager, completed: workflowCompletions,
	}
	return runner, &sessionClosers{closers: []io.Closer{workflowManager, database}}, nil
}

func sessionStateFields() map[string]dagent.StateField {
	return map[string]dagent.StateField{
		sessionWorkingDirectoryKey: dagent.Field(dagent.FieldSpec[string]{
			Kind: dagent.FieldLast, Contract: "dacode.session-working-directory.v1", Private: true,
			Clone: func(value string) string { return value },
		}),
	}
}

func mutatingToolApprovalRules() []dagent.ApprovalRule {
	decisions := []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject}
	return []dagent.ApprovalRule{
		{Pattern: "write_file", Description: "Allow this file write?", AllowedDecisions: decisions},
		{Pattern: "edit_file", Description: "Allow this file edit?", AllowedDecisions: decisions},
		{Pattern: "delete", Description: "Allow this file deletion?", AllowedDecisions: decisions},
		{Pattern: "execute", Description: "Allow this shell command?", AllowedDecisions: decisions},
	}
}

func workspaceContext(root string) (dago.Memory, string) {
	virtualRoot, err := filepath.Abs(root)
	if err != nil {
		virtualRoot = filepath.Clean(root)
	}
	guidance := daworkspace.DiscoverGuidance(context.Background(), daworkspace.GuidanceOptions{
		Root: root, WorkingDirectory: root, TrustWorkspace: true,
	})
	memory := dago.Memory{}
	if len(guidance.Root) > 0 {
		memory.Sources = make([]string, 0, len(guidance.Root))
		memory.Contents = make(map[string]string, len(guidance.Root))
		memory.SystemPrompt = dago.PromptTemplate{Mode: dago.PromptCustom, Text: workspaceGuidancePrompt}
		for _, file := range guidance.Root {
			virtualPath, ok := virtualWorkspacePath(virtualRoot, file.Path)
			if !ok {
				continue
			}
			memory.Sources = append(memory.Sources, virtualPath)
			memory.Contents[virtualPath] = file.Content
		}
	}
	virtualSubdirectories := make([]string, 0, len(guidance.Subdirectories))
	for _, filePath := range guidance.Subdirectories {
		virtualPath, ok := virtualWorkspacePath(virtualRoot, filePath)
		if ok {
			virtualSubdirectories = append(virtualSubdirectories, virtualPath)
		}
	}
	return memory, daworkspace.FormatSubdirectoryGuidance(virtualSubdirectories, 10)
}

func virtualWorkspacePath(root, filePath string) (string, bool) {
	relative, err := filepath.Rel(root, filePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if relative == "." {
		return "/", true
	}
	return "/" + strings.TrimPrefix(filepath.ToSlash(relative), "/"), true
}

func existingVirtualDirectories(root string, names ...string) []string {
	candidates := make([]string, 0, len(names))
	for _, name := range names {
		candidates = append(candidates, filepath.Join(root, filepath.FromSlash(name)))
	}
	existing := daworkspace.ExistingDirectories(candidates...)
	result := make([]string, 0, len(existing))
	for _, directory := range existing {
		relative, err := filepath.Rel(root, directory)
		if err == nil {
			result = append(result, "/"+strings.TrimPrefix(filepath.ToSlash(relative), "/"))
		}
	}
	return result
}

func workspaceSkills(root, herdrEnvironment string) dago.Skills {
	options := dago.Skills{Sources: existingVirtualDirectories(root,
		".agents/skills", ".claude/skills", ".deepagents/skills")}
	if herdrEnvironment != "1" {
		return options
	}
	options.Catalog = []dago.Skill{{
		Name:        "herdr",
		Description: "Control Herdr, a terminal multiplexer for coding agents. Use only when the user explicitly mentions Herdr or asks to use Herdr to inspect or control panes, tabs, workspaces, commands, or another agent. Do not use merely because a task could benefit from a background terminal, delegation, or parallel work. Requires HERDR_ENV=1.",
		License:     "Apache-2.0",
		Body:        "Run `herdr --skill` with the shell, read its complete output, and follow those release-matched instructions before using Herdr.",
	}}
	return options
}

func newThreadID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate thread id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
