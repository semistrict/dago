package dacode

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/daaskuser"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/darepository"
	"github.com/semistrict/dago/daskill"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daworkflow"
	"github.com/semistrict/dago/daworkspace"
)

const defaultModel = "gpt-5.6-terra"

const maximumPendingCostThreads = 64

const (
	maximumThreadRevisionRows  = 100_000
	maximumThreadRevisionBytes = 64 << 20
)

const (
	sessionWorkingDirectoryKey = "__dacode_working_directory"
	sessionModelKey            = "__dacode_model"
	sessionContextTokensKey    = "_context_tokens"
)

type eventStream interface {
	Next(context.Context) (dagent.Event, error)
	Result(context.Context) (dagent.Result, error)
	Close() error
}

type agentRunner interface {
	Start(context.Context, dagent.Input) eventStream
	Profile() damodel.Profile
	ReasoningEffort() reasoningEffortContext
	SetReasoningEffort(string) error
	Tools() []datool.Definition
	AgentName() string
	ListAgents(context.Context) ([]agentInfo, error)
	SwitchAgent(context.Context, string) error
	SetDefaultAgent(context.Context, string) (string, error)
	Cancel(context.Context, string) error
	Review(context.Context, approvalReviewRequest) (approvalReviewResult, error)
	ListSessions(context.Context) ([]sessionInfo, error)
	LoadSession(context.Context, string) ([]damessage.Message, error)
	Goal(context.Context, string) (*dagoal.Goal, error)
	SetGoal(context.Context, string, dagoal.SetRequest) (*dagoal.Goal, error)
	ClearGoal(context.Context, string) (bool, error)
	DraftGoalCriteria(context.Context, string, dagoal.CriteriaRequest) (dagoal.CriteriaProposal, error)
	Rubric(context.Context, string) (dago.RubricSnapshot, error)
	SetRubric(context.Context, string, string) (dago.RubricSnapshot, error)
	ClearRubric(context.Context, string) (bool, error)
	RubricSettings() (string, int)
	SetRubricModel(context.Context, string) error
	SetRubricMaxIterations(int) error
}

type agentCostReporter interface {
	CostReport(context.Context, string) (dacost.Report, error)
	CostPricingError() error
}

type dagoRunner struct {
	agent          *dagent.Agent
	profile        damodel.Profile
	reviewer       *dagent.Agent
	mainReviewer   *dagent.Agent
	reviewerSpec   string
	reviewerModel  func(context.Context, string) (damodel.Chat, error)
	reviewBackend  dabackend.Backend
	saver          *checkpointsqlite.Saver
	database       *sql.DB
	workingDir     string
	goals          *dagoal.Service
	criteria       *dagoal.CriteriaDrafter
	rubric         *rubricSettings
	agentState     *agentIdentity
	agentDefault   *agentIdentity
	stateDir       string
	effort         *reasoningEffortManager
	agentConfig    *daconfig.Store
	costEstimator  dacost.Estimator
	costPricingErr error
	costMu         sync.Mutex
	pendingUsage   map[string][]damessage.PurposedUsage
	hookStatus     *hookUISink
	workflows      *daworkflow.Manager
	completed      *workflowCompletionQueue
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

type agentIdentity struct {
	sync.RWMutex
	name string
}

func (identity *agentIdentity) current() string {
	identity.RLock()
	defer identity.RUnlock()
	return identity.name
}

func (identity *agentIdentity) set(name string) {
	identity.Lock()
	defer identity.Unlock()
	identity.name = name
}

func (runner *dagoRunner) Start(ctx context.Context, input dagent.Input) eventStream {
	state := input.State.Clone()
	if state == nil {
		state = dastate.Values{}
	}
	state[sessionWorkingDirectoryKey] = runner.workingDir
	input.State = state
	if pending := runner.takePendingUsage(input.Config.ThreadID); len(pending) > 0 {
		if len(input.Messages) == 0 {
			runner.queuePendingUsage(input.Config.ThreadID, pending)
		} else {
			last := len(input.Messages) - 1
			input.Messages[last] = input.Messages[last].Clone()
			input.Messages[last].OtherUsage = append(input.Messages[last].OtherUsage, pending...)
		}
	}
	return runner.agent.Stream(ctx, input)
}

func (runner *dagoRunner) NextHookStatus(ctx context.Context) (hookStatusUpdate, error) {
	if runner == nil || runner.hookStatus == nil {
		return hookStatusUpdate{}, errors.New("hook status is unavailable")
	}
	return runner.hookStatus.Next(ctx)
}

func (runner *dagoRunner) Profile() damodel.Profile {
	if runner == nil {
		return damodel.Profile{}
	}
	return runner.profile
}

func (runner *dagoRunner) ReasoningEffort() reasoningEffortContext {
	if runner == nil {
		return reasoningEffortContext{}
	}
	return runner.effort.Context()
}

func (runner *dagoRunner) SetReasoningEffort(level string) error {
	if runner == nil {
		return errors.New("agent runner is unavailable")
	}
	return runner.effort.Set(level)
}

func (runner *dagoRunner) Tools() []datool.Definition {
	if runner == nil || runner.agent == nil {
		return nil
	}
	tools := runner.agent.Tools()
	definitions := make([]datool.Definition, len(tools))
	for index, tool := range tools {
		definitions[index] = tool.Definition()
	}
	sort.Slice(definitions, func(left, right int) bool { return definitions[left].Name < definitions[right].Name })
	return definitions
}

func (runner *dagoRunner) AgentName() string {
	if runner == nil || runner.agentState == nil {
		return defaultAgentName
	}
	return runner.agentState.current()
}

func (runner *dagoRunner) ListAgents(ctx context.Context) ([]agentInfo, error) {
	if runner == nil {
		return nil, errors.New("agent runner is unavailable")
	}
	agents, err := discoverAgents(ctx, runner.stateDir, runner.AgentName())
	if err != nil {
		return nil, err
	}
	defaultName := ""
	if runner.agentDefault != nil {
		defaultName = runner.agentDefault.current()
	}
	for index := range agents {
		agents[index].Default = agents[index].Name == defaultName
	}
	return agents, nil
}

func (runner *dagoRunner) SwitchAgent(ctx context.Context, name string) error {
	if runner == nil || runner.agentState == nil {
		return errors.New("agent runner is unavailable")
	}
	_, err := loadAgentInstructions(ctx, runner.stateDir, name)
	if err != nil {
		return err
	}
	if err := ensureAgentMemoryFile(runner.stateDir, name); err != nil {
		return err
	}
	if err := writeAgentPreference(runner.stateDir, name, recentAgentPreference); err != nil {
		return err
	}
	if runner.agentConfig != nil {
		if err := runner.agentConfig.Set(ctx, "agents.recent", name); err != nil {
			return fmt.Errorf("save recent agent configuration: %w", err)
		}
	}
	runner.agentState.set(name)
	return nil
}

func (runner *dagoRunner) SetDefaultAgent(ctx context.Context, name string) (string, error) {
	if runner == nil {
		return "", errors.New("agent runner is unavailable")
	}
	selected, err := toggleDefaultAgent(ctx, runner.stateDir, name)
	if err != nil {
		return "", err
	}
	if runner.agentConfig == nil {
		if runner.agentDefault != nil {
			runner.agentDefault.set(selected)
		}
		return selected, nil
	}
	if selected == "" {
		_, err = runner.agentConfig.Unset(ctx, "agents.default")
	} else {
		err = runner.agentConfig.Set(ctx, "agents.default", selected)
	}
	if err != nil {
		return "", fmt.Errorf("save default agent configuration: %w", err)
	}
	if runner.agentDefault != nil {
		runner.agentDefault.set(selected)
	}
	return selected, nil
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
LIMIT 1024`)
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
		item, err := runner.SessionMetadata(ctx, row.threadID)
		if err != nil {
			if strings.Contains(err.Error(), "outside the current") {
				continue
			}
			return nil, err
		}
		sessions = append(sessions, item)
	}
	return sessions, nil
}

type threadRevisionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func computeThreadRevision(ctx context.Context, queryer threadRevisionQueryer, threadID string) (string, error) {
	var checkpointRows, checkpointBytes, writeRows, writeBytes int64
	if err := queryer.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(
  length(checkpoint_ns) + length(checkpoint_id) + length(COALESCE(parent_checkpoint_id, '')) +
  length(COALESCE(type, '')) + length(COALESCE(checkpoint, X'')) + length(COALESCE(metadata, X''))
), 0)
FROM checkpoints WHERE thread_id = ?`, threadID).Scan(&checkpointRows, &checkpointBytes); err != nil {
		return "", fmt.Errorf("inspect thread revision checkpoints: %w", err)
	}
	if err := queryer.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(
  length(checkpoint_ns) + length(checkpoint_id) + length(task_id) + length(channel) +
  length(COALESCE(type, '')) + length(COALESCE(value, X'')) + 8
), 0)
FROM writes WHERE thread_id = ?`, threadID).Scan(&writeRows, &writeBytes); err != nil {
		return "", fmt.Errorf("inspect thread revision writes: %w", err)
	}
	if checkpointRows <= 0 || checkpointRows+writeRows > maximumThreadRevisionRows ||
		checkpointBytes < 0 || writeBytes < 0 || checkpointBytes+writeBytes > maximumThreadRevisionBytes {
		return "", errors.New("thread revision exceeds safe inspection limits")
	}
	digest := sha256.New()
	checkpointSet, err := queryer.QueryContext(ctx, `
SELECT checkpoint_ns, checkpoint_id, COALESCE(parent_checkpoint_id, ''), COALESCE(type, ''),
       COALESCE(checkpoint, X''), COALESCE(metadata, X'')
FROM checkpoints WHERE thread_id = ?
ORDER BY checkpoint_ns, checkpoint_id`, threadID)
	if err != nil {
		return "", fmt.Errorf("read thread revision checkpoints: %w", err)
	}
	for checkpointSet.Next() {
		var namespace, checkpointID, parentID, typeTag string
		var checkpoint, metadata []byte
		if err := checkpointSet.Scan(&namespace, &checkpointID, &parentID, &typeTag, &checkpoint, &metadata); err != nil {
			checkpointSet.Close()
			return "", fmt.Errorf("scan thread revision checkpoint: %w", err)
		}
		writeThreadRevisionField(digest, []byte("checkpoint"))
		for _, field := range [][]byte{[]byte(namespace), []byte(checkpointID), []byte(parentID), []byte(typeTag), checkpoint, metadata} {
			writeThreadRevisionField(digest, field)
		}
	}
	if err := checkpointSet.Close(); err != nil {
		return "", fmt.Errorf("close thread revision checkpoints: %w", err)
	}
	if err := checkpointSet.Err(); err != nil {
		return "", fmt.Errorf("read thread revision checkpoints: %w", err)
	}
	writeSet, err := queryer.QueryContext(ctx, `
SELECT checkpoint_ns, checkpoint_id, task_id, idx, channel, COALESCE(type, ''), COALESCE(value, X'')
FROM writes WHERE thread_id = ?
ORDER BY checkpoint_ns, checkpoint_id, task_id, idx`, threadID)
	if err != nil {
		return "", fmt.Errorf("read thread revision writes: %w", err)
	}
	for writeSet.Next() {
		var namespace, checkpointID, taskID, channel, typeTag string
		var index int64
		var value []byte
		if err := writeSet.Scan(&namespace, &checkpointID, &taskID, &index, &channel, &typeTag, &value); err != nil {
			writeSet.Close()
			return "", fmt.Errorf("scan thread revision write: %w", err)
		}
		var indexBytes [8]byte
		binary.BigEndian.PutUint64(indexBytes[:], uint64(index))
		writeThreadRevisionField(digest, []byte("write"))
		for _, field := range [][]byte{[]byte(namespace), []byte(checkpointID), []byte(taskID), indexBytes[:], []byte(channel), []byte(typeTag), value} {
			writeThreadRevisionField(digest, field)
		}
	}
	if err := writeSet.Close(); err != nil {
		return "", fmt.Errorf("close thread revision writes: %w", err)
	}
	if err := writeSet.Err(); err != nil {
		return "", fmt.Errorf("read thread revision writes: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeThreadRevisionField(writer io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

// SessionMetadata inspects the latest durable checkpoint without changing the
// active agent or loading the thread into the terminal application. It is kept
// outside agentRunner so older embedding runners remain source compatible.
func (runner *dagoRunner) SessionMetadata(ctx context.Context, threadID string) (sessionInfo, error) {
	return runner.sessionMetadata(ctx, dacheckpoint.Config{ThreadID: threadID})
}

func (runner *dagoRunner) sessionMetadata(ctx context.Context, config dacheckpoint.Config) (sessionInfo, error) {
	if runner == nil || runner.agent == nil || runner.saver == nil {
		return sessionInfo{}, errors.New("agent runner is unavailable")
	}
	if err := validateResumeThreadID(config.ThreadID); err != nil {
		return sessionInfo{}, err
	}
	if config.CheckpointID != "" && (len(config.CheckpointID) > maximumResumeThreadIDBytes || strings.ContainsRune(config.CheckpointID, 0)) {
		return sessionInfo{}, errors.New("resume checkpoint id is invalid")
	}
	tuple, err := runner.saver.GetTuple(ctx, config)
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q checkpoint: %w", config.ThreadID, err)
	}
	if tuple == nil {
		return sessionInfo{}, fmt.Errorf("session %q was not found", config.ThreadID)
	}
	addressed := config
	addressed.CheckpointID = tuple.Checkpoint.ID
	snapshot, err := runner.agent.State(ctx, addressed)
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q: %w", config.ThreadID, err)
	}
	if snapshot.Config.ThreadID == "" {
		return sessionInfo{}, fmt.Errorf("session %q was not found", config.ThreadID)
	}
	sessionAgent, _ := tuple.Checkpoint.ChannelValues[sessionAgentNameKey].(string)
	if sessionAgent == "" {
		sessionAgent, _ = snapshot.State[sessionAgentNameKey].(string)
	}
	if sessionAgent == "" {
		sessionAgent = defaultAgentName
	}
	if err := validateAgentName(sessionAgent); err != nil {
		return sessionInfo{}, fmt.Errorf("session %q has an invalid agent identity: %w", config.ThreadID, err)
	}
	currentGeneration, err := readAgentGeneration(ctx, runner.stateDir, sessionAgent)
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q profile: %w", config.ThreadID, err)
	}
	sessionGeneration, _ := tuple.Checkpoint.ChannelValues[sessionAgentGenerationKey].(string)
	if sessionGeneration != currentGeneration {
		return sessionInfo{}, fmt.Errorf("session %q is outside the current %q profile namespace", config.ThreadID, sessionAgent)
	}
	messages, err := decodeSessionMessages(snapshot.State[dagent.MessagesKey])
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q messages: %w", config.ThreadID, err)
	}
	item := sessionInfo{
		ThreadID:      config.ThreadID,
		CheckpointID:  tuple.Checkpoint.ID,
		Agent:         sessionAgent,
		MessageCount:  len(messages),
		ContextTokens: sessionContextTokens(tuple.Checkpoint.ChannelValues, messages),
	}
	item.Directory, _ = tuple.Checkpoint.ChannelValues[sessionWorkingDirectoryKey].(string)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, tuple.Checkpoint.Timestamp)
	var firstCheckpointID string
	if err := runner.database.QueryRowContext(ctx, `
SELECT MIN(checkpoint_id)
FROM checkpoints
WHERE thread_id = ? AND checkpoint_ns = ''`, config.ThreadID).Scan(&firstCheckpointID); err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q creation checkpoint: %w", config.ThreadID, err)
	}
	first, err := runner.saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: config.ThreadID, CheckpointID: firstCheckpointID})
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q creation time: %w", config.ThreadID, err)
	}
	if first == nil || first.Checkpoint.ID != firstCheckpointID {
		return sessionInfo{}, fmt.Errorf("inspect session %q creation time: checkpoint is unavailable", config.ThreadID)
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, first.Checkpoint.Timestamp)
	revisionTransaction, err := runner.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q revision: %w", config.ThreadID, err)
	}
	defer revisionTransaction.Rollback()
	item.ThreadRevision, err = computeThreadRevision(ctx, revisionTransaction, config.ThreadID)
	if err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q revision: %w", config.ThreadID, err)
	}
	if err := revisionTransaction.Commit(); err != nil {
		return sessionInfo{}, fmt.Errorf("inspect session %q revision: %w", config.ThreadID, err)
	}
	for _, message := range messages {
		if message.Role == damessage.RoleHuman && strings.TrimSpace(message.TextContent()) != "" {
			item.Preview = strings.TrimSpace(message.TextContent())
			break
		}
	}
	return item, nil
}

func (runner *dagoRunner) LoadSession(ctx context.Context, threadID string) ([]damessage.Message, error) {
	metadata, err := runner.SessionMetadata(ctx, threadID)
	if err != nil {
		return nil, err
	}
	return runner.LoadSessionCheckpoint(ctx, threadID, metadata.CheckpointID)
}

// DeleteSession removes only a thread whose latest durable checkpoint still
// matches the exact checkpoint approved in the selector.
func (runner *dagoRunner) DeleteSession(ctx context.Context, threadID, checkpointID, revision string) error {
	if runner == nil || runner.saver == nil {
		return errors.New("agent runner is unavailable")
	}
	if validThreadSelectorID(checkpointID) != checkpointID {
		return errors.New("session checkpoint id is invalid")
	}
	if validThreadRevision(revision) != revision {
		return errors.New("session revision is invalid")
	}
	metadata, err := runner.SessionMetadata(ctx, threadID)
	if err != nil {
		return err
	}
	if metadata.CheckpointID != checkpointID {
		return errors.New("session checkpoint changed before deletion")
	}
	transaction, err := runner.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete session %q: %w", threadID, err)
	}
	defer transaction.Rollback()
	var latestCheckpointID string
	if err := transaction.QueryRowContext(ctx, `
SELECT MAX(checkpoint_id)
FROM checkpoints
WHERE thread_id = ? AND checkpoint_ns = ''`, threadID).Scan(&latestCheckpointID); err != nil {
		return fmt.Errorf("delete session %q: inspect latest checkpoint: %w", threadID, err)
	}
	if latestCheckpointID != checkpointID {
		return errors.New("session checkpoint changed before deletion")
	}
	currentRevision, err := computeThreadRevision(ctx, transaction, threadID)
	if err != nil {
		return fmt.Errorf("delete session %q: %w", threadID, err)
	}
	if currentRevision != revision {
		return errors.New("session revision changed before deletion")
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM checkpoints WHERE thread_id = ?", threadID); err != nil {
		return fmt.Errorf("delete session %q checkpoints: %w", threadID, err)
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM writes WHERE thread_id = ?", threadID); err != nil {
		return fmt.Errorf("delete session %q writes: %w", threadID, err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("delete session %q: %w", threadID, err)
	}
	return nil
}

// LoadSessionCheckpoint loads only the checkpoint whose trust metadata was
// approved. It prevents a concurrent latest-checkpoint change from bypassing
// the resume controller's decisions.
func (runner *dagoRunner) LoadSessionCheckpoint(ctx context.Context, threadID, checkpointID string) ([]damessage.Message, error) {
	if _, err := runner.sessionMetadata(ctx, dacheckpoint.Config{ThreadID: threadID, CheckpointID: checkpointID}); err != nil {
		return nil, err
	}
	snapshot, err := runner.agent.State(ctx, dacheckpoint.Config{ThreadID: threadID, CheckpointID: checkpointID})
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", threadID, err)
	}
	if snapshot.Config.ThreadID == "" {
		return nil, fmt.Errorf("session %q was not found", threadID)
	}
	return decodeSessionMessages(snapshot.State[dagent.MessagesKey])
}

func (runner *dagoRunner) CompactSession(ctx context.Context, threadID, checkpointID string) (sessionCompactionResult, error) {
	if runner == nil || runner.agent == nil || runner.saver == nil {
		return sessionCompactionResult{}, errors.New("session compaction is unavailable")
	}
	if err := validateResumeThreadID(threadID); err != nil {
		return sessionCompactionResult{}, err
	}
	metadata, err := runner.sessionMetadata(ctx, dacheckpoint.Config{ThreadID: threadID, CheckpointID: checkpointID})
	if err != nil {
		return sessionCompactionResult{}, err
	}
	config := dacheckpoint.Config{ThreadID: threadID, CheckpointID: metadata.CheckpointID}
	tuple, err := runner.saver.GetTuple(ctx, config)
	if err != nil {
		return sessionCompactionResult{}, fmt.Errorf("load compaction checkpoint: %w", err)
	}
	if tuple == nil || tuple.Checkpoint.ID != metadata.CheckpointID {
		return sessionCompactionResult{}, errors.New("compaction checkpoint changed before execution")
	}
	snapshot, err := runner.agent.State(ctx, config)
	if err != nil {
		return sessionCompactionResult{}, fmt.Errorf("reconstruct compaction checkpoint: %w", err)
	}
	if snapshot.Config.CheckpointID != metadata.CheckpointID {
		return sessionCompactionResult{}, errors.New("compaction checkpoint changed while reconstructing state")
	}
	var compact datool.Tool
	for _, tool := range runner.agent.Tools() {
		if tool.Definition().Name == "compact_conversation" {
			compact = tool
			break
		}
	}
	if compact == nil {
		return sessionCompactionResult{}, errors.New("compact_conversation tool is unavailable")
	}
	callID, err := newThreadID()
	if err != nil {
		return sessionCompactionResult{}, fmt.Errorf("create compaction identity: %w", err)
	}
	toolState := snapshot.State.Clone()
	if event, exists := tuple.Checkpoint.ChannelValues["_summarization_event"]; exists {
		toolState["_summarization_event"] = event
	}
	result, err := compact.Execute(ctx, json.RawMessage(`{"force":true}`), datool.Runtime{
		CallID: callID, ThreadID: threadID, CheckpointID: metadata.CheckpointID,
		State: toolState,
	})
	if err != nil {
		return sessionCompactionResult{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return sessionCompactionResult{}, err
	}
	output := damessage.Message{Content: result.Content}.TextContent()
	failed := result.Status == damessage.ToolStatusError || strings.HasPrefix(output, "Compaction failed:")
	if failed {
		return sessionCompactionResult{Output: output, CheckpointID: metadata.CheckpointID, Failed: true}, nil
	}
	committedCheckpoint := metadata.CheckpointID
	if len(result.Update) > 0 {
		snapshot, updateErr := runner.agent.UpdateState(ctx, config, dastate.Values(result.Update))
		if updateErr != nil {
			return sessionCompactionResult{}, fmt.Errorf("commit conversation compaction: %w", updateErr)
		}
		committedCheckpoint = snapshot.Config.CheckpointID
	}
	return sessionCompactionResult{Output: output, CheckpointID: committedCheckpoint}, nil
}

func (runner *dagoRunner) CostReport(ctx context.Context, threadID string) (dacost.Report, error) {
	if runner == nil || runner.agent == nil {
		return dacost.Report{}, errors.New("agent runner is unavailable")
	}
	snapshot, err := runner.agent.State(ctx, dacheckpoint.Config{ThreadID: threadID})
	if errors.Is(err, dacheckpoint.ErrCheckpointMissing) {
		return dacost.ReportMessages(nil, runner.costEstimator, dacost.MessageOptions{})
	}
	if err != nil {
		return dacost.Report{}, fmt.Errorf("read session cost %q: %w", threadID, err)
	}
	if snapshot.Config.ThreadID == "" {
		return dacost.ReportMessages(nil, runner.costEstimator, dacost.MessageOptions{})
	}
	messages, err := decodeSessionMessages(snapshot.State[dagent.MessagesKey])
	if err != nil {
		return dacost.Report{}, fmt.Errorf("read session cost %q: %w", threadID, err)
	}
	return dacost.ReportMessages(messages, runner.costEstimator, dacost.MessageOptions{
		FallbackProvider: runner.profile.Provider,
		FallbackModel:    runner.profile.Model,
	})
}

func (runner *dagoRunner) CostPricingError() error {
	if runner == nil {
		return errors.New("agent runner is unavailable")
	}
	return runner.costPricingErr
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
	goal, err := runner.goals.Set(ctx, dacheckpoint.Config{ThreadID: threadID}, request)
	if err != nil || goal == nil {
		return goal, err
	}
	if request.Criteria != nil || request.Status != nil {
		criteria := ""
		if goal.Actionable() {
			criteria = goal.Criteria
		}
		if _, updateErr := runner.writeRubric(ctx, threadID, criteria); updateErr != nil {
			return goal, updateErr
		}
	}
	return goal, nil
}

func (runner *dagoRunner) ClearGoal(ctx context.Context, threadID string) (bool, error) {
	cleared, err := runner.goals.Clear(ctx, dacheckpoint.Config{ThreadID: threadID})
	if err != nil || !cleared {
		return cleared, err
	}
	_, err = runner.writeRubric(ctx, threadID, "")
	return true, err
}

func (runner *dagoRunner) StartWorkflow(ctx context.Context, request daworkflow.StartRequest) (daworkflow.Status, error) {
	if runner == nil || runner.workflows == nil {
		return daworkflow.Status{}, errors.New("workflow runtime is unavailable")
	}
	return runner.workflows.Start(ctx, request)
}

func (runner *dagoRunner) Workflows() []daworkflow.Status {
	if runner == nil || runner.workflows == nil {
		return nil
	}
	return runner.workflows.List()
}

func (runner *dagoRunner) RunningWorkflows() int {
	if runner == nil || runner.workflows == nil {
		return 0
	}
	return runner.workflows.Running()
}

func (runner *dagoRunner) CancelWorkflow(runID string) bool {
	return runner != nil && runner.workflows != nil && runner.workflows.Cancel(runID)
}

func (runner *dagoRunner) WaitWorkflowCompletion(ctx context.Context) (daworkflow.Status, bool) {
	if runner == nil || runner.completed == nil {
		return daworkflow.Status{}, false
	}
	return runner.completed.Wait(ctx)
}

func (runner *dagoRunner) DraftGoalCriteria(ctx context.Context, threadID string, request dagoal.CriteriaRequest) (dagoal.CriteriaProposal, error) {
	if runner == nil || runner.criteria == nil {
		return dagoal.CriteriaProposal{}, errors.New("goal criteria drafting is unavailable")
	}
	proposal, usage, err := runner.criteria.DraftWithUsage(ctx, request)
	runner.recordOwnedUsage(ctx, threadID, usage)
	return proposal, err
}

func (runner *dagoRunner) recordOwnedUsage(ctx context.Context, threadID string, usage []damessage.PurposedUsage) {
	if runner == nil || runner.agent == nil || threadID == "" || len(usage) == 0 {
		return
	}
	if len(usage) > 256 {
		usage = usage[:256]
	}
	snapshot, err := runner.agent.State(ctx, dacheckpoint.Config{ThreadID: threadID})
	if err != nil || snapshot.Config.ThreadID == "" {
		runner.queuePendingUsage(threadID, usage)
		return
	}
	messages, err := decodeSessionMessages(snapshot.State[dagent.MessagesKey])
	if err != nil || len(messages) == 0 {
		runner.queuePendingUsage(threadID, usage)
		return
	}
	last := len(messages) - 1
	messages[last].OtherUsage = append(messages[last].OtherUsage, clonePurposedUsage(usage)...)
	if len(messages[last].OtherUsage) > 256 {
		messages[last].OtherUsage = messages[last].OtherUsage[:256]
	}
	if _, err := runner.agent.UpdateState(ctx, dacheckpoint.Config{ThreadID: threadID}, dastate.Values{
		dagent.MessagesKey: dastate.Overwrite{Value: messages},
	}); err != nil {
		runner.queuePendingUsage(threadID, usage)
	}
}

func (runner *dagoRunner) queuePendingUsage(threadID string, usage []damessage.PurposedUsage) {
	if threadID == "" || len(usage) == 0 {
		return
	}
	runner.costMu.Lock()
	defer runner.costMu.Unlock()
	if runner.pendingUsage == nil {
		runner.pendingUsage = make(map[string][]damessage.PurposedUsage)
	}
	if _, exists := runner.pendingUsage[threadID]; !exists && len(runner.pendingUsage) >= maximumPendingCostThreads {
		return
	}
	current := append(runner.pendingUsage[threadID], clonePurposedUsage(usage)...)
	if len(current) > 256 {
		current = current[:256]
	}
	runner.pendingUsage[threadID] = current
}

func (runner *dagoRunner) takePendingUsage(threadID string) []damessage.PurposedUsage {
	runner.costMu.Lock()
	defer runner.costMu.Unlock()
	usage := runner.pendingUsage[threadID]
	delete(runner.pendingUsage, threadID)
	return clonePurposedUsage(usage)
}

func clonePurposedUsage(usage []damessage.PurposedUsage) []damessage.PurposedUsage {
	result := make([]damessage.PurposedUsage, len(usage))
	for index := range usage {
		result[index] = usage[index]
		result[index].Usage.InputDetails = cloneIntMap(usage[index].Usage.InputDetails)
		result[index].Usage.OutputDetails = cloneIntMap(usage[index].Usage.OutputDetails)
	}
	return result
}

func cloneIntMap(source map[string]int) map[string]int {
	if source == nil {
		return nil
	}
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (runner *dagoRunner) Rubric(ctx context.Context, threadID string) (dago.RubricSnapshot, error) {
	state, err := runner.agent.State(ctx, dacheckpoint.Config{ThreadID: threadID})
	if errors.Is(err, dacheckpoint.ErrCheckpointMissing) {
		return dago.RubricSnapshot{}, nil
	}
	if err != nil {
		return dago.RubricSnapshot{}, fmt.Errorf("read rubric: %w", err)
	}
	return dago.RubricSnapshotFromState(state.State), nil
}

func (runner *dagoRunner) SetRubric(ctx context.Context, threadID, criteria string) (dago.RubricSnapshot, error) {
	criteria = strings.TrimSpace(criteria)
	if criteria == "" {
		return dago.RubricSnapshot{}, errors.New("rubric criteria are required")
	}
	return runner.writeRubric(ctx, threadID, criteria)
}

func (runner *dagoRunner) ClearRubric(ctx context.Context, threadID string) (bool, error) {
	current, err := runner.Rubric(ctx, threadID)
	if err != nil || current.Criteria == "" {
		return false, err
	}
	_, err = runner.writeRubric(ctx, threadID, "")
	return err == nil, err
}

func (runner *dagoRunner) writeRubric(ctx context.Context, threadID, criteria string) (dago.RubricSnapshot, error) {
	values := dastate.Values{
		dago.RubricKey: criteria, dago.RubricStatusKey: nil, dago.RubricIterationsKey: 0,
		dago.RubricEvaluationsKey: nil, dago.RubricRunIDKey: nil, dago.RubricActiveKey: nil,
	}
	state, err := runner.agent.UpdateState(ctx, dacheckpoint.Config{ThreadID: threadID}, values)
	if err != nil {
		return dago.RubricSnapshot{}, fmt.Errorf("write rubric: %w", err)
	}
	return dago.RubricSnapshotFromState(state.State), nil
}

func (runner *dagoRunner) RubricSettings() (string, int) { return runner.rubric.Values() }

func (runner *dagoRunner) SetRubricModel(ctx context.Context, model string) error {
	return runner.rubric.SetModel(ctx, model)
}

func (runner *dagoRunner) SetRubricMaxIterations(value int) error {
	return runner.rubric.SetMaxIterations(value)
}

type runnerOptions struct {
	Authentication      modelAuthentication
	BaseURL             string
	Model               string
	WorkingDir          string
	ConfigurationDir    string
	StateDir            string
	AgentName           string
	DefaultAgent        string
	RecentAgent         string
	AgentConfig         *daconfig.Store
	ReviewTools         bool
	Shell               bool
	ShellAllowList      shellAllowList
	AutoReview          bool
	ReviewModel         string
	Tools               []datool.Tool
	Headless            bool
	RecursionLimit      int
	MemoryReadOnly      bool
	RubricModel         string
	RubricMaxIterations int
	Backend             dabackend.Backend
	Plugins             pluginRuntimeComponents
	HookStatus          *hookUISink
}

func runnerPathInstructions(workingDir string, localHostPaths bool) string {
	if localHostPaths {
		return fmt.Sprintf("Filesystem and shell tools use unrestricted host paths. The current working directory is %q. Use that real path for file operations; in shell commands, relative paths start there and / is the host filesystem root.", workingDir)
	}
	return fmt.Sprintf("Filesystem and shell tools run in a remote sandbox whose working directory is %q. Use sandbox paths only; local host paths are unavailable.", workingDir)
}

func newRunner(options runnerOptions) (agentRunner, io.Closer, error) {
	if options.ConfigurationDir == "" {
		options.ConfigurationDir = options.WorkingDir
	}
	options.Authentication.decorateModel = dacost.NormalizeUsage
	model, err := options.Authentication.resolveModel(context.Background(), options.Model, options.BaseURL)
	if err != nil {
		return nil, nil, err
	}

	if options.ShellAllowList.configured() {
		options.Shell = true
	}
	var backend dabackend.Backend
	var localContextSandbox dabackend.Sandbox
	localHostPaths := options.Backend == nil
	if options.Backend != nil {
		backend = options.Backend
		localContextSandbox, _ = dabackend.SandboxOf(backend)
	} else if options.Shell {
		backend, err = dabackend.NewLocalShell(dabackend.LocalShellOptions{
			Filesystem: dabackend.FilesystemOptions{Root: options.WorkingDir, AllowHostPaths: true},
			InheritEnv: true,
		})
		localContextSandbox, _ = dabackend.SandboxOf(backend)
	} else {
		backend, err = dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: options.WorkingDir, AllowHostPaths: true})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open workspace: %w", err)
	}
	if options.ShellAllowList.restrictive() {
		backend = enforceShellAllowList(backend, options.ShellAllowList)
	}

	if err := os.MkdirAll(options.StateDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create state directory: %w", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(options.StateDir, "threads.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("open session database: %w", err)
	}
	database.SetMaxOpenConns(1)
	saver := checkpointsqlite.New(database)
	if err := saver.Setup(context.Background()); err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	if err := setupAutoClassifierCounters(context.Background(), database); err != nil {
		_ = database.Close()
		return nil, nil, err
	}

	filesystem := dago.Filesystem{}
	var interruptOn []dagent.ApprovalRule
	if options.Headless {
		var mcpRules []dagent.ApprovalRule
		options.Tools, mcpRules = applyHeadlessMCPPolicy(options.Tools, options.ReviewTools)
		if options.ReviewTools {
			interruptOn = append(interruptOn, mcpRules...)
		}
	}
	if options.ReviewTools {
		localRules := approvalRulesForShellAllowList(defaultToolApprovalRules(), options.ShellAllowList)
		interruptOn = append(localRules, interruptOn...)
	}
	interruptOn = approvalRulesForThreadModes(
		interruptOn,
		newApprovalModeStore(filepath.Join(options.StateDir, approvalPreferencesFilename)),
	)
	goalOptions := dagoal.Options{}
	selectedAgent, err := resolveInitialAgentConfigured(context.Background(), options.StateDir, options.AgentName, options.DefaultAgent, options.RecentAgent)
	if err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	agentState := &agentIdentity{name: selectedAgent}
	effectiveDefault := options.DefaultAgent
	if available, _ := agentAvailable(context.Background(), options.StateDir, effectiveDefault); effectiveDefault == "" || !available {
		effectiveDefault, _ = configuredDefaultAgent(options.StateDir)
		if available, _ := agentAvailable(context.Background(), options.StateDir, effectiveDefault); !available {
			effectiveDefault = ""
		}
	}
	agentDefault := &agentIdentity{name: effectiveDefault}
	if options.AgentConfig != nil {
		if err := options.AgentConfig.Set(context.Background(), "agents.recent", selectedAgent); err != nil {
			_ = database.Close()
			return nil, nil, fmt.Errorf("save recent agent configuration: %w", err)
		}
	}
	agentMemory, err := openAgentMemory(options.StateDir, agentState)
	if err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_ = database.Close()
		return nil, nil, fmt.Errorf("resolve user skills home: %w", err)
	}
	skillRoutes, hasUserAgents, hasUserClaude, err := runtimeUserSkillRoutes(home)
	if err != nil {
		_ = database.Close()
		return nil, nil, fmt.Errorf("open user skills: %w", err)
	}
	for mount, pluginBackend := range options.Plugins.SkillRoutes {
		if _, exists := skillRoutes[mount]; exists {
			_ = database.Close()
			return nil, nil, fmt.Errorf("mount plugin skills: duplicate route %q", mount)
		}
		skillRoutes[mount] = pluginBackend
	}
	skillRoutes[agentMemoryMount] = agentMemory
	backend = dabackend.NewComposite(backend, skillRoutes)
	effort := newReasoningEffortManager(model.Profile(), filepath.Join(options.StateDir, reasoningEffortFilename))
	localPrices, priceErr := dacost.LoadCatalog(filepath.Join(options.StateDir, "prices.json"), dacost.CatalogOptions{})
	if priceErr != nil {
		localPrices = dacost.NewCatalog(nil, dacost.CatalogOptions{})
	}
	costEstimator := dacost.NewPricer(nil, dacost.BundledCatalog(), localPrices, dacost.PricerOptions{})
	rubricSettings := newRubricSettings(model, options.Model, func(ctx context.Context, spec string) (damodel.Chat, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return options.Authentication.resolveModel(ctx, spec, options.BaseURL)
	})
	if options.RubricModel != "" {
		if err := rubricSettings.SetModel(context.Background(), options.RubricModel); err != nil {
			_ = database.Close()
			return nil, nil, fmt.Errorf("configure rubric model: %w", err)
		}
	}
	if options.RubricMaxIterations > 0 {
		if err := rubricSettings.SetMaxIterations(options.RubricMaxIterations); err != nil {
			_ = database.Close()
			return nil, nil, err
		}
	}
	runtimeModel := dagent.RuntimeModel(dagent.ModelResolverFunc(func(ctx context.Context, spec string) (damodel.Chat, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return options.Authentication.resolveModel(ctx, spec, options.BaseURL)
	}), dagent.RuntimeModelOptions{})
	criteria := dagoal.NewCriteriaDrafter(model, backend, darepository.Options{}, dagoal.CriteriaOptions{})
	rubric := dago.RubricWithRepository(rubricSettings, backend, darepository.Options{}, dago.RubricOptions{MaxIterationsFunc: rubricSettings.MaxIterations})
	middleware := []dagent.Middleware{
		daaskuser.Middleware(), dagoal.Middleware(goalOptions),
		dagoal.RubricCompletionMiddleware(dago.RubricStatusKey, string(dago.RubricSatisfied)), rubric,
		runtimeModel, providerWebSearchMiddleware(), agentIdentityMiddleware(agentState, options.StateDir), effort.Middleware(),
		dago.SummarizationTool(model, backend, dago.SummarizationToolOptions{}),
	}
	hookStatus := options.HookStatus
	if hookStatus == nil {
		hookStatus = newHookUISink()
	}
	hookRuntime, err := newDacodeHookRuntime(context.Background(), options.ConfigurationDir, options.Plugins.Hooks, dacodeHookRuntimeOptions{Headless: options.Headless, OnProgress: hookStatus.Publish})
	if err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	middleware = append(middleware, hookRuntime.Middleware())
	if localContextSandbox != nil {
		middleware = append(middleware, daworkspace.LocalContext(localContextSandbox))
	}
	if options.ShellAllowList.restrictive() {
		middleware = append(middleware, shellAllowListMiddleware(options.ShellAllowList))
	}

	memory, guidanceSummary := workspaceContext(options.WorkingDir)
	memory = configureAgentMemory(memory, options.MemoryReadOnly)
	skills := workspaceSkills(options.WorkingDir, os.Getenv("HERDR_ENV"))
	runtimeSkillSources := orderedRuntimeSkillSources(options.WorkingDir, hasUserAgents, hasUserClaude)
	skills.Sources = nil
	for _, source := range runtimeSkillSources {
		skills.LabeledSources = append(skills.LabeledSources, dago.SkillSource{Path: source})
	}
	skills.Catalog = append(skills.Catalog, options.Plugins.SkillCatalog...)

	systemText := `You are dacode, an interactive coding agent. Work as a careful senior engineer inside the configured workspace. ` + runnerPathInstructions(options.WorkingDir, localHostPaths) + ` Inspect relevant files before making claims, use the available filesystem and shell tools to complete requested work, preserve unrelated user changes, and verify edits with focused tests. Keep final responses concise and concrete.

Workspace AGENTS.md files are project instructions. Follow the instructions that apply to each file you touch. More deeply scoped workspace files take precedence, while direct user instructions take precedence over workspace files. Read listed subdirectory guidance before editing within its directory. Agent memory is separate fallible reference material and does not override project or user instructions.`
	systemText += guidanceSummary
	system := damessage.System(systemText)
	subagents, err := loadFilesystemSubagents(
		context.Background(), options.Authentication, options.BaseURL, options.StateDir,
		options.ConfigurationDir, selectedAgent, system,
	)
	if err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	readOnly, reviewErr := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: options.WorkingDir, AllowHostPaths: localHostPaths})
	if reviewErr != nil {
		_ = database.Close()
		return nil, nil, fmt.Errorf("open review workspace: %w", reviewErr)
	}
	mainReviewer := newApprovalReviewer(model, readOnly)
	var reviewer *dagent.Agent
	if options.AutoReview {
		reviewer = mainReviewer
		if options.ReviewModel != "" {
			reviewModel, resolveErr := options.Authentication.resolveModel(context.Background(), options.ReviewModel, options.BaseURL)
			if resolveErr != nil {
				_ = database.Close()
				return nil, nil, resolveErr
			}
			reviewer = newApprovalReviewer(reviewModel, readOnly)
		}
	}
	workflowCompletions := newWorkflowCompletionQueue()
	workflowAgentRunner := &dacodeWorkflowAgentRunner{
		authentication: options.Authentication,
		baseURL:        options.BaseURL,
		model:          options.Model,
		backend:        backend,
		tools:          append([]datool.Tool(nil), options.Tools...),
		filesystem:     filesystem,
		skills:         skills,
		memory:         memory,
		system:         systemText,
		approvalRules:  interruptOn,
		reviewer:       reviewer,
		workingDir:     options.WorkingDir,
	}
	workflowManager := daworkflow.NewManager(workflowAgentRunner, daworkflow.Options{
		Resolver:         workspaceWorkflowResolver{root: options.WorkingDir, stateRoot: options.StateDir},
		SessionDirectory: options.StateDir,
		Completed: func(_ context.Context, status daworkflow.Status) {
			workflowCompletions.Push(status)
		},
	})
	middleware = append(middleware, daworkflow.Middleware(workflowManager))
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
		dago.WithSubagents(subagents...),
		dago.WithMiddleware(middleware...),
		dago.WithApprovalRules(interruptOn...),
		dago.WithRecursionLimit(options.RecursionLimit),
		dago.WithSaver(saver),
		dago.WithRetainedThreadState(),
		dago.WithStateFields(sessionStateFields()),
	)
	runner := &dagoRunner{
		agent: agent, profile: model.Profile(), reviewer: reviewer, mainReviewer: mainReviewer,
		saver: saver, database: database, workingDir: options.WorkingDir,
		goals: dagoal.NewService(agent, goalOptions), criteria: criteria, rubric: rubricSettings,
		agentState: agentState, agentDefault: agentDefault,
		stateDir: options.StateDir, effort: effort, agentConfig: options.AgentConfig,
		costEstimator: costEstimator, costPricingErr: priceErr, hookStatus: hookStatus,
		workflows: workflowManager, completed: workflowCompletions,
	}
	if options.AgentConfig != nil {
		activeProfile := model.Profile()
		if activeProfile.Provider != "" && activeProfile.Model != "" {
			preferences := modelconfig.NewPreferenceStore(options.AgentConfig)
			if err := preferences.SetRecent(context.Background(), activeProfile.Provider+":"+activeProfile.Model); err != nil {
				_ = database.Close()
				return nil, nil, fmt.Errorf("save recent model configuration: %w", err)
			}
		}
	}
	runner.reviewerSpec = options.ReviewModel
	runner.reviewBackend = readOnly
	runner.reviewerModel = func(ctx context.Context, spec string) (damodel.Chat, error) {
		return options.Authentication.resolveModel(ctx, spec, options.BaseURL)
	}
	return runner, &sessionClosers{closers: []io.Closer{workflowManager, hookRuntime, database}}, nil
}

func approvalRulesForThreadModes(rules []dagent.ApprovalRule, store *approvalModeStore) []dagent.ApprovalRule {
	configured := append([]dagent.ApprovalRule(nil), rules...)
	for index := range configured {
		previous := configured[index].When
		configured[index].When = func(request dagent.ToolCallRequest) bool {
			if previous != nil && !previous(request) {
				return false
			}
			if store == nil {
				return true
			}
			mode, err := store.Load(request.Runtime.Config.ThreadID)
			return err != nil || mode != approvalYOLO
		}
	}
	return configured
}

func dacodeGeneralSubagent(system damessage.Message) dago.Subagent {
	return dago.NewSubagent(
		"general-purpose",
		"General-purpose agent for researching complex questions, searching for files and content, and executing multi-step tasks. It has the same workspace and memory boundaries as the main agent.",
		nil,
		dago.WithSystemMessage(system),
	)
}

func configureAgentMemory(memory dago.Memory, readOnly bool) dago.Memory {
	memory.Sources = append([]string{agentMemorySourcePath}, memory.Sources...)
	memory.ReadOnly = readOnly
	return memory
}

func agentIdentityMiddleware(identity *agentIdentity, stateDir string) dagent.Middleware {
	if identity == nil {
		panic("agent identity is nil")
	}
	if stateDir == "" {
		panic("agent state directory is empty")
	}
	return dagent.Middleware{
		Name: "_dacode_agent_identity",
		BeforeAgent: func(ctx context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			if value, exists := values[sessionAgentNameKey]; exists {
				name, ok := value.(string)
				if !ok || validateAgentName(name) != nil {
					return nil, fmt.Errorf("invalid session agent identity %T", value)
				}
				currentGeneration, err := readAgentGeneration(ctx, stateDir, name)
				if err != nil {
					return nil, err
				}
				savedGeneration := ""
				if generationValue, present := values[sessionAgentGenerationKey]; present {
					var valid bool
					savedGeneration, valid = generationValue.(string)
					if !valid {
						return nil, fmt.Errorf("invalid session agent generation %T", generationValue)
					}
				}
				if savedGeneration != currentGeneration {
					return nil, fmt.Errorf("session is outside the current %q profile namespace", name)
				}
				return nil, nil
			}
			updates := dastate.Values{sessionAgentNameKey: identity.current()}
			generation, err := readAgentGeneration(ctx, stateDir, identity.current())
			if err != nil {
				return nil, err
			}
			if generation != "" {
				updates[sessionAgentGenerationKey] = generation
			}
			return updates, nil
		},
		WrapModelCall: func(ctx context.Context, request dagent.ModelRequest, next dagent.ModelHandler) (dagent.ModelResponse, error) {
			name, _ := request.State[sessionAgentNameKey].(string)
			if name == "" {
				name = identity.current()
			}
			if name == defaultAgentName {
				return next(ctx, request)
			}
			section := "<agent_identity>\n" + name + "\n</agent_identity>"
			if request.SystemMessage == nil {
				system := damessage.System(section)
				request.SystemMessage = &system
			} else {
				system := request.SystemMessage.Clone()
				system.Content = append(system.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: "\n\n" + section})
				request.SystemMessage = &system
			}
			return next(ctx, request)
		},
		AfterAgent: func(_ context.Context, values dastate.Values, _ dagent.Runtime) (dastate.Values, error) {
			messages, err := decodeSessionMessages(values[dagent.MessagesKey])
			if err != nil {
				return nil, err
			}
			return dastate.Values{sessionContextTokensKey: contextTokensFromMessages(messages)}, nil
		},
	}
}

func sessionStateFields() map[string]dagent.StateField {
	return map[string]dagent.StateField{
		sessionWorkingDirectoryKey: dagent.Field(dagent.FieldSpec[string]{
			Kind: dagent.FieldLast, Contract: "dacode.session-working-directory.v1", Private: true,
			Clone: func(value string) string { return value },
		}),
		sessionModelKey: dagent.Field(dagent.FieldSpec[string]{
			Kind: dagent.FieldLast, Contract: "dacode.session-model.v1", Private: true,
			Clone: func(value string) string { return value },
		}),
		sessionAgentNameKey: dagent.Field(dagent.FieldSpec[string]{
			Kind: dagent.FieldLast, Contract: "dacode.session-agent-name.v1",
			Clone: func(value string) string { return value },
		}),
		sessionAgentGenerationKey: dagent.Field(dagent.FieldSpec[string]{
			Kind: dagent.FieldLast, Contract: "dacode.session-agent-generation.v1", Private: true,
			Clone: func(value string) string { return value },
		}),
		sessionContextTokensKey: dagent.Field(dagent.FieldSpec[int]{
			Kind: dagent.FieldLast, Contract: "dacode.session-context-tokens.v1", Private: true,
			Clone: func(value int) int { return value },
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
		".deepagents/skills", ".agents/skills", ".claude/skills")}
	for _, skill := range daskill.Builtins() {
		options.Catalog = append(options.Catalog, skill)
	}
	if herdrEnvironment != "1" {
		return options
	}
	options.Catalog = append(options.Catalog, dago.Skill{
		Name:        "herdr",
		Description: "Control Herdr, a terminal multiplexer for coding agents. Use only when the user explicitly mentions Herdr or asks to use Herdr to inspect or control panes, tabs, workspaces, commands, or another agent. Do not use merely because a task could benefit from a background terminal, delegation, or parallel work. Requires HERDR_ENV=1.",
		License:     "Apache-2.0",
		Body:        "Run `herdr --skill` with the shell, read its complete output, and follow those release-matched instructions before using Herdr.",
	})
	return options
}

func newThreadID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate thread id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
