package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

type resumeFlowRunner struct {
	agentName   string
	agents      []agentInfo
	metadata    sessionInfo
	messages    []damessage.Message
	metadataErr error
	loads       int
}

func (runner *resumeFlowRunner) AgentName() string { return runner.agentName }

func (runner *resumeFlowRunner) ListAgents(context.Context) ([]agentInfo, error) {
	return append([]agentInfo(nil), runner.agents...), nil
}

func (runner *resumeFlowRunner) SessionMetadata(ctx context.Context, _ string) (sessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return sessionInfo{}, err
	}
	return runner.metadata, runner.metadataErr
}

func (runner *resumeFlowRunner) LoadSession(context.Context, string) ([]damessage.Message, error) {
	runner.loads++
	return append([]damessage.Message(nil), runner.messages...), nil
}

func (runner *resumeFlowRunner) LoadSessionCheckpoint(_ context.Context, _, checkpointID string) ([]damessage.Message, error) {
	if checkpointID != runner.metadata.CheckpointID {
		return nil, errors.New("wrong checkpoint")
	}
	runner.loads++
	return append([]damessage.Message(nil), runner.messages...), nil
}

func TestSessionResumeControllerGatesLoadThroughOrderedPrompts(t *testing.T) {
	current := t.TempDir()
	stored := t.TempDir()
	runner := &resumeFlowRunner{
		agentName: "reviewer",
		agents:    []agentInfo{{Name: "reviewer"}, {Name: "builder"}},
		metadata: sessionInfo{
			ThreadID: "thread-1", CheckpointID: "checkpoint-1", Agent: "builder", Directory: stored,
			ContextTokens: defaultCompactResumeThreshold + 1,
		},
		messages: []damessage.Message{damessage.Human("continue")},
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread-1", defaultSessionResumeOptions(current))
	if err != nil {
		t.Fatal(err)
	}
	if controller.Prompt().Agent == nil {
		t.Fatalf("first prompt = %#v", controller.Prompt())
	}
	if _, _, err := loadPreparedSession(t.Context(), runner, controller); err == nil || runner.loads != 0 {
		t.Fatalf("premature load = %d, %v", runner.loads, err)
	}
	if err := controller.Apply(resumePromptSwitchAgent); err != nil {
		t.Fatal(err)
	}
	if controller.Prompt().CWD == nil {
		t.Fatalf("second prompt = %#v", controller.Prompt())
	}
	if err := controller.Apply(resumePromptSwitchCWD); err != nil {
		t.Fatal(err)
	}
	if controller.Prompt().Compact == nil {
		t.Fatalf("third prompt = %#v", controller.Prompt())
	}
	if err := controller.Apply(resumePromptCompactNow); err != nil {
		t.Fatal(err)
	}
	decision, ready := controller.Decision()
	storedResolved, err := filepath.EvalSymlinks(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || !decision.SwitchAgent || !decision.SwitchDirectory || !decision.Compact || decision.Agent != "builder" || decision.Directory != storedResolved {
		t.Fatalf("decision = %#v, %t", decision, ready)
	}
	session, messages, err := loadPreparedSession(t.Context(), runner, controller)
	if err != nil {
		t.Fatal(err)
	}
	if runner.loads != 1 || session.ThreadID != "thread-1" || len(messages) != 1 {
		t.Fatalf("load = %d, %#v, %#v", runner.loads, session, messages)
	}
	if _, _, err := loadPreparedSession(t.Context(), runner, controller); err == nil || runner.loads != 1 {
		t.Fatalf("duplicate load = %d, %v", runner.loads, err)
	}
}

func TestSessionResumeControllerStayKeepAndCancel(t *testing.T) {
	current := t.TempDir()
	stored := t.TempDir()
	runner := &resumeFlowRunner{
		agentName: defaultAgentName,
		agents:    []agentInfo{{Name: defaultAgentName}},
		metadata:  sessionInfo{ThreadID: "thread-2", CheckpointID: "checkpoint-2", Agent: defaultAgentName, Directory: stored, ContextTokens: 500},
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread-2", sessionResumeOptions{
		CurrentDirectory: current, CompactThreshold: 100, AbortMode: cwdResumeAbortThreadSwitch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Apply(resumePromptCompactNow); err == nil || controller.Prompt().CWD == nil {
		t.Fatalf("wrong-stage action = %v, prompt = %#v", err, controller.Prompt())
	}
	if err := controller.Apply(resumePromptStayCWD); err != nil {
		t.Fatal(err)
	}
	if err := controller.Apply(resumePromptKeepContext); err != nil {
		t.Fatal(err)
	}
	decision, ready := controller.Decision()
	currentResolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatal(err)
	}
	if !ready || decision.Directory != currentResolved || decision.SwitchDirectory || decision.Compact {
		t.Fatalf("decision = %#v, %t", decision, ready)
	}

	cancel, err := prepareSessionResume(t.Context(), runner, "thread-2", sessionResumeOptions{
		CurrentDirectory: current, CompactThreshold: 0, AbortMode: cwdResumeAbortLaunch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cancel.Apply(resumePromptAbort); err != nil {
		t.Fatal(err)
	}
	if _, ready := cancel.Decision(); ready || !cancel.Canceled() {
		t.Fatal("canceled resume became ready")
	}
	if _, _, err := loadPreparedSession(t.Context(), runner, cancel); err == nil {
		t.Fatal("canceled resume loaded")
	}
}

func TestSessionResumeControllerCrossAgentCancelAndThresholdBoundary(t *testing.T) {
	current := t.TempDir()
	runner := &resumeFlowRunner{
		agentName: defaultAgentName,
		agents:    []agentInfo{{Name: defaultAgentName}, {Name: "builder"}},
		metadata: sessionInfo{
			ThreadID: "thread", CheckpointID: "checkpoint", Agent: "builder",
			ContextTokens: defaultCompactResumeThreshold,
		},
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread", defaultSessionResumeOptions(current))
	if err != nil {
		t.Fatal(err)
	}
	if controller.Prompt().Agent == nil {
		t.Fatalf("prompt = %#v", controller.Prompt())
	}
	if err := controller.Apply(resumePromptCancelAgent); err != nil {
		t.Fatal(err)
	}
	if !controller.Canceled() {
		t.Fatal("agent cancellation was not terminal")
	}

	runner.agentName = "builder"
	controller, err = prepareSessionResume(t.Context(), runner, "thread", defaultSessionResumeOptions(current))
	if err != nil {
		t.Fatal(err)
	}
	if !controller.Prompt().Empty() {
		t.Fatalf("threshold equality prompted: %#v", controller.Prompt())
	}
	runner.metadata.ContextTokens++
	controller, err = prepareSessionResume(t.Context(), runner, "thread", defaultSessionResumeOptions(current))
	if err != nil {
		t.Fatal(err)
	}
	if controller.Prompt().Compact == nil {
		t.Fatalf("above threshold prompt = %#v", controller.Prompt())
	}
}

func TestSessionResumeControllerRejectsUntrustedMetadata(t *testing.T) {
	current := t.TempDir()
	tests := []struct {
		name     string
		threadID string
		metadata sessionInfo
		agents   []agentInfo
		want     string
	}{
		{name: "mismatched thread", threadID: "wanted", metadata: sessionInfo{ThreadID: "other", CheckpointID: "checkpoint", Agent: defaultAgentName}, want: "did not match"},
		{name: "relative directory", threadID: "wanted", metadata: sessionInfo{ThreadID: "wanted", CheckpointID: "checkpoint", Agent: defaultAgentName, Directory: "../other"}, want: "absolute"},
		{name: "missing cross agent", threadID: "wanted", metadata: sessionInfo{ThreadID: "wanted", CheckpointID: "checkpoint", Agent: "builder"}, agents: []agentInfo{{Name: defaultAgentName}}, want: "not available"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &resumeFlowRunner{agentName: defaultAgentName, agents: test.agents, metadata: test.metadata}
			_, err := prepareSessionResume(t.Context(), runner, test.threadID, defaultSessionResumeOptions(current))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := prepareSessionResume(t.Context(), &resumeFlowRunner{}, strings.Repeat("x", maximumResumeThreadIDBytes+1), defaultSessionResumeOptions(current)); err == nil {
		t.Fatal("oversized thread id accepted")
	}
	if _, err := prepareSessionResume(t.Context(), &resumeFlowRunner{}, "thread", sessionResumeOptions{CurrentDirectory: current, CompactThreshold: -1}); err == nil {
		t.Fatal("negative threshold accepted")
	}
}

func TestPreparedSessionRejectsCheckpointReplacement(t *testing.T) {
	current := t.TempDir()
	runner := &resumeFlowRunner{
		agentName: defaultAgentName,
		metadata: sessionInfo{
			ThreadID: "thread", CheckpointID: "approved-checkpoint", Agent: defaultAgentName,
		},
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread", sessionResumeOptions{CurrentDirectory: current})
	if err != nil {
		t.Fatal(err)
	}
	if _, ready := controller.Decision(); !ready {
		t.Fatal("resume was not ready")
	}
	runner.metadata.CheckpointID = "replacement-checkpoint"
	if _, _, err := loadPreparedSession(t.Context(), runner, controller); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("replacement error = %v", err)
	}
	if runner.loads != 0 {
		t.Fatalf("replacement loaded %d times", runner.loads)
	}
}

func TestSessionResumeControllerTreatsDirectoryAliasesAsEqual(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation requires privileges on some Windows hosts")
	}
	realDirectory := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	runner := &resumeFlowRunner{
		agentName: defaultAgentName,
		metadata:  sessionInfo{ThreadID: "thread", CheckpointID: "checkpoint", Agent: defaultAgentName, Directory: alias},
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread", sessionResumeOptions{CurrentDirectory: realDirectory})
	if err != nil {
		t.Fatal(err)
	}
	if _, ready := controller.Decision(); !ready || !controller.Prompt().Empty() {
		t.Fatalf("alias unexpectedly prompted: %#v", controller.Prompt())
	}
}

func TestSessionContextTokensPreferDirectContextAndPersistedMetadata(t *testing.T) {
	usage := damessage.Assistant("answer")
	usage.Usage = &damessage.Usage{InputTokens: 80, OutputTokens: 20, TotalTokens: 500}
	if got := contextTokensFromMessages([]damessage.Message{usage}); got != 100 {
		t.Fatalf("context tokens = %d", got)
	}
	usage.Usage = &damessage.Usage{TotalTokens: 500}
	if got := contextTokensFromMessages([]damessage.Message{usage}); got != 500 {
		t.Fatalf("fallback context tokens = %d", got)
	}
	if got := sessionContextTokens(map[string]any{sessionContextTokensKey: int64(700)}, []damessage.Message{usage}); got != 700 {
		t.Fatalf("persisted context tokens = %d", got)
	}
}

func TestConfiguredSessionResumeOptionsUsesManifestDefaultAndZeroOverride(t *testing.T) {
	resolver := daconfig.NewResolver(cliConfigManifest, func(string) (string, bool) { return "", false }, daconfig.ResolverOptions{})
	snapshot, err := resolver.Resolve(nil, daconfig.Layer{})
	if err != nil {
		t.Fatal(err)
	}
	options := configuredSessionResumeOptions(resolvedCLIConfig{snapshot: snapshot}, "/workspace", cwdResumeAbortLaunch)
	if options.CompactThreshold != defaultCompactResumeThreshold || options.AbortMode != cwdResumeAbortLaunch {
		t.Fatalf("default options = %#v", options)
	}
	snapshot, err = resolver.Resolve(nil, daconfig.NewLayer("test", map[string]any{"threads.compact_on_resume_threshold": 0}))
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredSessionResumeOptions(resolvedCLIConfig{snapshot: snapshot}, "/workspace", 0).CompactThreshold; got != 0 {
		t.Fatalf("disabled threshold = %d", got)
	}
}

func TestSessionResumePreparationHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &resumeFlowRunner{metadataErr: errors.New("must not replace cancellation")}
	_, err := prepareSessionResume(ctx, runner, "thread", defaultSessionResumeOptions(t.TempDir()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerSessionMetadataReadsPrivateCheckpointFields(t *testing.T) {
	stateDir := t.TempDir()
	runnerValue, closer, err := newRunner(runnerOptions{
		Authentication: modelAuthentication{apiKey: "test-key"},
		Model:          defaultModel,
		WorkingDir:     t.TempDir(),
		StateDir:       stateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	runner := runnerValue.(*dagoRunner)
	usageMessage := damessage.Assistant("done")
	usageMessage.Usage = &damessage.Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 999}
	first, err := runner.agent.UpdateState(t.Context(), dacheckpoint.Config{ThreadID: "metadata-thread"}, dastate.Values{
		dagent.MessagesKey:         []damessage.Message{damessage.Human("task"), usageMessage},
		sessionWorkingDirectoryKey: "/private/project",
		sessionContextTokensKey:    150,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := runner.SessionMetadata(t.Context(), "metadata-thread")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ThreadID != "metadata-thread" || metadata.Agent != defaultAgentName || metadata.Directory != "/private/project" || metadata.ContextTokens != 150 || metadata.MessageCount != 2 || metadata.Preview != "task" || metadata.UpdatedAt.IsZero() {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata.CheckpointID == "" || metadata.CheckpointID != first.Config.CheckpointID {
		t.Fatalf("checkpoint identity = %q, state = %#v", metadata.CheckpointID, first.Config)
	}
	_, err = runner.agent.UpdateState(t.Context(), dacheckpoint.Config{ThreadID: "metadata-thread"}, dastate.Values{
		dagent.MessagesKey: []damessage.Message{damessage.Human("replacement")},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := runner.LoadSessionCheckpoint(t.Context(), "metadata-thread", metadata.CheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].TextContent() != "task" {
		t.Fatalf("exact checkpoint messages = %#v", messages)
	}
}

func TestAgentIdentityMiddlewareRecordsLatestContextSize(t *testing.T) {
	middleware := agentIdentityMiddleware(&agentIdentity{name: defaultAgentName}, t.TempDir())
	message := damessage.Assistant("answer")
	message.Usage = &damessage.Usage{InputTokens: 25, OutputTokens: 5, TotalTokens: 100}
	update, err := middleware.AfterAgent(t.Context(), dastate.Values{dagent.MessagesKey: []damessage.Message{message}}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if update[sessionContextTokensKey] != 30 {
		t.Fatalf("context update = %#v", update)
	}
}
