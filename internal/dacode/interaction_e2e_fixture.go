//go:build dacode_e2e_fixture

package dacode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/dainstall"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
)

type interactionFixtureRunner struct {
	*dagoRunner
	mu              sync.Mutex
	sessions        map[string]sessionInfo
	messages        map[string][]damessage.Message
	cancelSnapshots map[string]interactionFixtureSnapshot
	preferences     *interactionModelPreferences
}

type interactionFixtureSnapshot struct {
	session  sessionInfo
	messages []damessage.Message
	existed  bool
}

func newInteractionFixtureRunner() *interactionFixtureRunner {
	profile := damodel.Profile{Provider: "openai", Model: "gpt-5.6-terra", ContextWindow: 32_000}
	now := time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC)
	runner := &interactionFixtureRunner{
		dagoRunner: &dagoRunner{
			profile: profile, effort: newReasoningEffortManager(profile, ""),
			agentState: &agentIdentity{name: defaultAgentName}, agentDefault: &agentIdentity{name: defaultAgentName},
		},
		sessions: map[string]sessionInfo{}, messages: map[string][]damessage.Message{},
		cancelSnapshots: map[string]interactionFixtureSnapshot{},
	}
	for _, session := range []sessionInfo{
		{ThreadID: "fixture-current", CheckpointID: "checkpoint-current", Preview: "current checkpoint history", Directory: "/fixture", Agent: defaultAgentName, Branch: "main", CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-time.Minute), MessageCount: 2},
		{ThreadID: "thread-alpha", CheckpointID: "checkpoint-alpha", Preview: "alpha search target", Directory: "/fixture", Agent: defaultAgentName, Branch: "feature/alpha", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Minute), MessageCount: 4},
		{ThreadID: "thread-blue", CheckpointID: "checkpoint-blue", Preview: "blue agent work", Directory: "/fixture", Agent: "other", Branch: "feature/blue", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-3 * time.Minute), MessageCount: 6},
		{ThreadID: "thread-fail", CheckpointID: "checkpoint-fail", Preview: "delete failure target", Directory: "/fixture", Agent: defaultAgentName, Branch: "feature/fail", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-4 * time.Minute), MessageCount: 8},
	} {
		session.ThreadRevision = interactionFixtureRevision(session)
		runner.sessions[session.ThreadID] = session
		runner.messages[session.ThreadID] = []damessage.Message{
			damessage.Human(session.Preview), damessage.Assistant("Restored history for " + session.ThreadID + "."),
		}
	}
	return runner
}

func (runner *interactionFixtureRunner) Start(ctx context.Context, input runInput) eventStream {
	prompt := ""
	if len(input.Messages) != 0 {
		prompt = input.Messages[len(input.Messages)-1].TextContent()
	}
	modelSpec, _ := input.Configurable[dagent.RuntimeModelConfigKey].(string)
	if modelSpec == "" {
		modelSpec = "openai:gpt-5.6-terra"
	}
	threadID := input.Config.ThreadID
	text := "fixture reply: thread=" + threadID + " model=" + modelSpec + " prompt=" + prompt
	if prompt == "report fixture model default" && runner.preferences != nil {
		persisted, _ := runner.preferences.Default(ctx)
		text += " persisted-default=" + persisted
	}
	delay := 25 * time.Millisecond
	ignoreCancellation := false
	if strings.Contains(prompt, "slow") {
		delay = 5 * time.Second
	}
	if strings.Contains(prompt, "late") {
		delay = 2 * time.Second
		ignoreCancellation = true
	}
	messages := []damessage.Message{damessage.Assistant(text)}
	runner.mu.Lock()
	previous, existed := runner.sessions[threadID]
	runner.cancelSnapshots[threadID] = interactionFixtureSnapshot{
		session: previous, messages: append([]damessage.Message(nil), runner.messages[threadID]...), existed: existed,
	}
	runner.messages[threadID] = append(runner.messages[threadID], damessage.Human(prompt), messages[0])
	nextSession := sessionInfo{
		ThreadID: threadID, CheckpointID: "checkpoint-" + threadID, Preview: prompt,
		Directory: "/fixture", Agent: runner.AgentName(), Branch: "fixture", UpdatedAt: time.Now(),
		MessageCount: len(runner.messages[threadID]),
	}
	nextSession.ThreadRevision = interactionFixtureRevision(nextSession)
	runner.sessions[threadID] = nextSession
	runner.mu.Unlock()
	return &interactionFixtureStream{
		delay: delay, ignoreCancellation: ignoreCancellation, messages: messages,
		onResult: func(cancelled bool) {
			if cancelled {
				return
			}
			runner.mu.Lock()
			delete(runner.cancelSnapshots, threadID)
			runner.mu.Unlock()
		},
	}
}

func (runner *interactionFixtureRunner) ListAgents(context.Context) ([]agentInfo, error) {
	current := runner.AgentName()
	return []agentInfo{{Name: defaultAgentName, Current: current == defaultAgentName, Default: true}, {Name: "other", Current: current == "other"}}, nil
}

func (runner *interactionFixtureRunner) SwitchAgent(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name != defaultAgentName && name != "other" {
		return errors.New("fixture agent is unavailable")
	}
	runner.agentState.set(name)
	return nil
}

func (runner *interactionFixtureRunner) SetDefaultAgent(context.Context, string) (string, error) {
	return defaultAgentName, nil
}

func (runner *interactionFixtureRunner) Cancel(ctx context.Context, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	snapshot, ok := runner.cancelSnapshots[threadID]
	if !ok {
		return nil
	}
	if snapshot.existed {
		runner.sessions[threadID] = snapshot.session
		runner.messages[threadID] = append([]damessage.Message(nil), snapshot.messages...)
	} else {
		delete(runner.sessions, threadID)
		delete(runner.messages, threadID)
	}
	delete(runner.cancelSnapshots, threadID)
	return nil
}

func (runner *interactionFixtureRunner) ListSessions(ctx context.Context) ([]sessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	order := []string{"fixture-current", "thread-alpha", "thread-blue", "thread-fail"}
	result := make([]sessionInfo, 0, len(runner.sessions))
	seen := map[string]bool{}
	for _, id := range order {
		if session, ok := runner.sessions[id]; ok {
			result = append(result, session)
			seen[id] = true
		}
	}
	for id, session := range runner.sessions {
		if !seen[id] {
			result = append(result, session)
		}
	}
	return result, nil
}

func (runner *interactionFixtureRunner) SessionMetadata(ctx context.Context, threadID string) (sessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return sessionInfo{}, err
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	session, ok := runner.sessions[threadID]
	if !ok {
		return sessionInfo{}, errors.New("fixture thread was not found")
	}
	return session, nil
}

func (runner *interactionFixtureRunner) LoadSession(ctx context.Context, threadID string) ([]damessage.Message, error) {
	session, err := runner.SessionMetadata(ctx, threadID)
	if err != nil {
		return nil, err
	}
	return runner.LoadSessionCheckpoint(ctx, threadID, session.CheckpointID)
}

func (runner *interactionFixtureRunner) LoadSessionCheckpoint(ctx context.Context, threadID, checkpointID string) ([]damessage.Message, error) {
	session, err := runner.SessionMetadata(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if checkpointID != session.CheckpointID {
		return nil, errors.New("fixture checkpoint changed")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]damessage.Message(nil), runner.messages[threadID]...), nil
}

func (runner *interactionFixtureRunner) DeleteSession(ctx context.Context, threadID, checkpointID, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if threadID == "thread-fail" {
		return errors.New("fixture durable delete refused")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	session, ok := runner.sessions[threadID]
	if !ok {
		return errors.New("fixture thread was not found")
	}
	if session.CheckpointID != checkpointID || session.ThreadRevision != revision {
		return errors.New("fixture checkpoint changed")
	}
	delete(runner.sessions, threadID)
	delete(runner.messages, threadID)
	return nil
}

func interactionFixtureRevision(session sessionInfo) string {
	digest := sha256.Sum256([]byte(session.ThreadID + "\x00" + session.CheckpointID + "\x00" + session.Preview))
	return hex.EncodeToString(digest[:])
}

func (*interactionFixtureRunner) Goal(context.Context, string) (*dagoal.Goal, error) { return nil, nil }
func (*interactionFixtureRunner) SetGoal(context.Context, string, dagoal.SetRequest) (*dagoal.Goal, error) {
	return nil, nil
}
func (*interactionFixtureRunner) ClearGoal(context.Context, string) (bool, error) { return false, nil }
func (*interactionFixtureRunner) DraftGoalCriteria(context.Context, string, dagoal.CriteriaRequest) (dagoal.CriteriaProposal, error) {
	return dagoal.CriteriaProposal{}, nil
}
func (*interactionFixtureRunner) Rubric(context.Context, string) (dago.RubricSnapshot, error) {
	return dago.RubricSnapshot{}, nil
}
func (*interactionFixtureRunner) SetRubric(context.Context, string, string) (dago.RubricSnapshot, error) {
	return dago.RubricSnapshot{}, nil
}
func (*interactionFixtureRunner) ClearRubric(context.Context, string) (bool, error) {
	return false, nil
}
func (*interactionFixtureRunner) RubricSettings() (string, int)                { return "openai:gpt-5.6-terra", 3 }
func (*interactionFixtureRunner) SetRubricModel(context.Context, string) error { return nil }
func (*interactionFixtureRunner) SetRubricMaxIterations(int) error             { return nil }

type interactionFixtureStream struct {
	mu                 sync.Mutex
	resultOnce         sync.Once
	delay              time.Duration
	ignoreCancellation bool
	messages           []damessage.Message
	emitted            bool
	cancelled          bool
	onResult           func(bool)
}

func (stream *interactionFixtureStream) Next(ctx context.Context) (dagent.Event, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.emitted {
		return dagent.Event{}, io.EOF
	}
	stream.emitted = true
	if stream.ignoreCancellation {
		time.Sleep(stream.delay)
		stream.cancelled = ctx.Err() != nil
	} else {
		timer := time.NewTimer(stream.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return dagent.Event{}, ctx.Err()
		case <-timer.C:
		}
	}
	return dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{dagent.MessagesKey: stream.messages}}, nil
}

func (stream *interactionFixtureStream) Result(context.Context) (dagent.Result, error) {
	stream.resultOnce.Do(func() {
		if stream.onResult != nil {
			stream.onResult(stream.cancelled)
		}
	})
	return dagent.Result{Messages: stream.messages}, nil
}
func (*interactionFixtureStream) Close() error { return nil }

type interactionInstallController struct{}

func (interactionInstallController) Available(kind dainstall.Kind) []dainstall.Entry {
	if kind == dainstall.Package {
		return []dainstall.Entry{{Name: "external-helper", Kind: kind, Description: "Offline allowlisted fixture package"}}
	}
	if kind == dainstall.Extra {
		return []dainstall.Entry{{Name: "builtin-extra", Kind: kind, Description: "Already compiled", BuiltIn: true}}
	}
	return nil
}

func (interactionInstallController) Install(ctx context.Context, kind dainstall.Kind, name string, authorization dainstall.Authorization) (dainstall.Result, error) {
	if err := ctx.Err(); err != nil {
		return dainstall.Result{}, err
	}
	if kind != dainstall.Package || name != "external-helper" || authorization != dainstall.AuthorizationGranted {
		return dainstall.Result{}, dainstall.ErrUnknownDependency
	}
	return dainstall.Result{Name: name, Kind: kind, Status: dainstall.Installed}, nil
}

type interactionModelPreferences struct {
	mu          sync.Mutex
	defaultSpec string
	recent      string
}

func (preferences *interactionModelPreferences) Default(context.Context) (string, error) {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	return preferences.defaultSpec, nil
}
func (preferences *interactionModelPreferences) Recent(context.Context) (string, error) {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	return preferences.recent, nil
}
func (preferences *interactionModelPreferences) SetDefault(ctx context.Context, spec string) error {
	if strings.Contains(spec, "luna") {
		return errors.New("fixture preference write refused")
	}
	if spec == "openai:gpt-5.6-sol" {
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	preferences.defaultSpec = spec
	return nil
}
func (preferences *interactionModelPreferences) ClearDefault(context.Context) (bool, error) {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	changed := preferences.defaultSpec != ""
	preferences.defaultSpec = ""
	return changed, nil
}
func (preferences *interactionModelPreferences) SetRecent(_ context.Context, spec string) error {
	preferences.mu.Lock()
	defer preferences.mu.Unlock()
	preferences.recent = spec
	return nil
}

// RunInteractionFixture runs the deterministic offline command-interaction browser fixture.
func RunInteractionFixture(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, err := parseCLI(arguments, stderr)
	if err != nil {
		return err
	}
	if options.serveXtermJS {
		return serveXtermJS(ctx, xtermJSServerOptions{
			Address: options.xtermJSAddress, Arguments: xtermSessionArguments(arguments), Stdout: stdout, Stderr: stderr,
		})
	}
	if err := os.MkdirAll(options.stateDir, 0o700); err != nil {
		return err
	}
	preferences := &interactionModelPreferences{}
	runner := newInteractionFixtureRunner()
	runner.preferences = preferences
	threadID := options.resume
	if threadID == "" {
		threadID = "fixture-current"
	}
	model := newTUIModel(ctx, runner, "/fixture", "openai:gpt-5.6-terra", threadID, false, true, options.message)
	model.createThreadID = func() (string, error) { return "fixture-new-" + time.Now().Format("150405.000000"), nil }
	model.installController = interactionInstallController{}
	model.configureLifecycle(options.stateDir, nil, defaultSessionResumeOptions("/fixture"))
	_ = model.configureModelPreferences(preferences)
	_ = model.configureDisplaySettings(filepath.Join(options.stateDir, displaySettingsFilename))
	model.modelProviderAvailability = map[string]modelProviderAvailability{
		"openai":    {Install: modelRequirementReady, Credentials: modelRequirementReady},
		"anthropic": {Install: modelRequirementMissing, Credentials: modelRequirementMissing},
	}
	if os.Getenv("DACODE_FIXTURE_STARTUP_FAILED") == "1" {
		model.startupFailed = true
		model.appendItem(transcriptItem{kind: itemError, text: "Fixture startup failed; recovery commands remain available."})
	}
	if options.resumePicker {
		model.sessionPicker = &sessionPickerState{loading: true, startup: true}
	} else if options.resume != "" {
		model.sessionPicker = &sessionPickerState{sessions: []sessionInfo{{ThreadID: options.resume}}, resuming: true, startup: true}
	}
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	_, err = program.Run()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

var _ agentRunner = (*interactionFixtureRunner)(nil)
var _ sessionMetadataReader = (*interactionFixtureRunner)(nil)
var _ sessionCheckpointLoader = (*interactionFixtureRunner)(nil)
var _ threadSessionDeleter = (*interactionFixtureRunner)(nil)
