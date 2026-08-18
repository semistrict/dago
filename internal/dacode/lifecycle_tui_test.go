package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

type lifecycleRestartController struct {
	err   error
	calls int
}

func (controller *lifecycleRestartController) Restart(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	controller.calls++
	return controller.err
}

type lifecycleResumeRunner struct {
	*fakeRunner
	metadata          sessionInfo
	checkpointLoaded  string
	checkpointErr     error
	switchedDirectory string
	directoryErr      error
	compactCalls      int
	compactCheckpoint string
}

func (runner *lifecycleResumeRunner) SessionMetadata(ctx context.Context, threadID string) (sessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return sessionInfo{}, err
	}
	if threadID != runner.metadata.ThreadID {
		return sessionInfo{}, errors.New("wrong thread")
	}
	return runner.metadata, nil
}

func (runner *lifecycleResumeRunner) LoadSessionCheckpoint(ctx context.Context, threadID, checkpointID string) ([]damessage.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if threadID != runner.metadata.ThreadID || checkpointID != runner.metadata.CheckpointID {
		return nil, errors.New("wrong checkpoint")
	}
	runner.checkpointLoaded = checkpointID
	if runner.checkpointErr != nil {
		return nil, runner.checkpointErr
	}
	return append([]damessage.Message(nil), runner.sessionMessages[threadID]...), nil
}

func (runner *lifecycleResumeRunner) SwitchSessionDirectory(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if runner.directoryErr != nil {
		return runner.directoryErr
	}
	runner.switchedDirectory = directory
	return nil
}

func (runner *lifecycleResumeRunner) CompactSession(ctx context.Context, threadID, checkpointID string) (sessionCompactionResult, error) {
	if err := ctx.Err(); err != nil {
		return sessionCompactionResult{}, err
	}
	if threadID != runner.metadata.ThreadID {
		return sessionCompactionResult{}, errors.New("wrong compact thread")
	}
	runner.compactCalls++
	runner.compactCheckpoint = checkpointID
	return sessionCompactionResult{Output: "Conversation compacted.", CheckpointID: "checkpoint-compacted"}, nil
}

func TestTUIOnboardingPersistsCompletionAndAppliesSelectedModel(t *testing.T) {
	stateDirectory := t.TempDir()
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, t.TempDir(), "openai:old", "thread", false, false, "")
	model.configureLifecycle(stateDirectory, nil, defaultSessionResumeOptions(model.workingDir))
	model.onboarding = newOnboardingState(nil, []modelSelectorEntry{{Spec: "openai:selected", Label: "Selected"}}, "openai:old")
	model.onboarding.step = onboardingGoalCriteria
	model.onboarding.result = onboardingResult{Name: "Ada", Model: "openai:selected"}

	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil || model.onboarding != nil || model.modelName != "openai:selected" {
		t.Fatalf("onboarding completion = handled %t, command %v, state %#v, model %q", handled, command, model.onboarding, model.modelName)
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, item := range batch {
			if item != nil {
				model.Update(item())
			}
		}
	} else {
		model.Update(message)
	}
	if data, err := os.ReadFile(onboardingMarkerPath(stateDirectory)); err != nil || string(data) != "1\n" {
		t.Fatalf("completion marker = %q, %v", data, err)
	}
	memory, err := os.ReadFile(filepath.Join(stateDirectory, defaultAgentName, agentInstructionsFilename))
	if err != nil || !strings.Contains(string(memory), `preferred name is "Ada"`) {
		t.Fatalf("onboarding memory = %q, %v", memory, err)
	}
}

func TestTUIRestartConfiguredUnavailableAndErrorPaths(t *testing.T) {
	controller := &lifecycleRestartController{}
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	model.restartController = controller
	if _, ok := model.slashCommand("/restart"); !ok || model.restartPrompt == nil {
		t.Fatal("configured restart did not open its prompt")
	}
	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil || !model.restarting {
		t.Fatalf("restart confirmation = %t, %v, %t", handled, command, model.restarting)
	}
	model.Update(command())
	if controller.calls != 1 || model.restarting || len(model.items) == 0 || model.items[len(model.items)-1].text != "Local agent server restarted." {
		t.Fatalf("restart completion = calls %d, restarting %t, transcript %q", controller.calls, model.restarting, model.renderTranscript())
	}

	unavailable := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	unavailable.slashCommand("/restart")
	if len(unavailable.items) == 0 || !strings.Contains(unavailable.items[len(unavailable.items)-1].text, "Restart is unavailable") {
		t.Fatalf("unavailable transcript = %q", unavailable.renderTranscript())
	}

	failing := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	failing.restartController = &lifecycleRestartController{err: errors.New("fixture restart failed")}
	failing.slashCommand("/restart")
	command, _ = failing.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	failing.Update(command())
	if len(failing.items) == 0 || !strings.Contains(failing.items[len(failing.items)-1].text, "fixture restart failed") {
		t.Fatalf("failure transcript = %q", failing.renderTranscript())
	}
}

func TestTUIResumePromptsApplyDecisionsLoadExactCheckpointAndForceCompaction(t *testing.T) {
	current, stored := t.TempDir(), t.TempDir()
	runner := &lifecycleResumeRunner{
		fakeRunner: &fakeRunner{
			agentName: "reviewer", agents: []agentInfo{{Name: "reviewer"}, {Name: "builder"}},
			sessionMessages: map[string][]damessage.Message{"thread": {damessage.Human("old task"), damessage.Assistant("old answer")}},
			streams:         []eventStream{&fakeEventStream{result: dagent.Result{}}},
		},
		metadata: sessionInfo{ThreadID: "thread", CheckpointID: "checkpoint-exact", Agent: "builder", Directory: stored, ContextTokens: 401_000},
	}
	model := newTUIModel(t.Context(), runner, current, "openai:model", "new", false, false, "")
	model.resize(100, 30)
	model.resumeOptions = sessionResumeOptions{CurrentDirectory: current, CompactThreshold: 400_000, AbortMode: cwdResumeAbortThreadSwitch}
	model.sessionPicker = &sessionPickerState{sessions: []sessionInfo{{ThreadID: "thread"}}, resuming: true}

	prepared := prepareSession(t.Context(), runner, "thread", model.resumeOptions)()
	_, command := model.Update(prepared)
	if command != nil || model.resumeController == nil || !strings.Contains(model.View(), "Switch agents to resume?") {
		t.Fatalf("agent prompt = command %v, controller %#v, view %q", command, model.resumeController, model.View())
	}
	command, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || !strings.Contains(model.View(), "original directory") {
		t.Fatalf("cwd prompt = command %v, view %q", command, model.View())
	}
	command, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || !strings.Contains(model.View(), "Compact this thread?") {
		t.Fatalf("compact prompt = command %v, view %q", command, model.View())
	}
	command, _ = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if command == nil {
		t.Fatal("final resume decision did not start exact load")
	}
	loaded := command()
	_, compactCommand := model.Update(loaded)
	storedResolved, err := filepath.EvalSymlinks(stored)
	if err != nil {
		t.Fatal(err)
	}
	if compactCommand == nil || runner.checkpointLoaded != "checkpoint-exact" || runner.switchedDirectory != storedResolved || runner.agentName != "builder" {
		t.Fatalf("resume application = command %v, checkpoint %q, directory %q, agent %q", compactCommand, runner.checkpointLoaded, runner.switchedDirectory, runner.agentName)
	}
	message := compactCommand().(compactionFinishedMsg)
	model.Update(message)
	if runner.compactCalls != 1 || runner.compactCheckpoint != "checkpoint-exact" || message.err != nil || message.result.Failed {
		t.Fatalf("compaction = calls %d, checkpoint %q, message %#v", runner.compactCalls, runner.compactCheckpoint, message)
	}
}

func TestTUIResumeDirectorySwitchFailsClosedWithoutRuntimeSupport(t *testing.T) {
	current, stored := t.TempDir(), t.TempDir()
	runner := &resumeFlowRunner{
		agentName: defaultAgentName,
		agents:    []agentInfo{{Name: defaultAgentName}},
		metadata:  sessionInfo{ThreadID: "thread", CheckpointID: "checkpoint", Agent: defaultAgentName, Directory: stored},
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread", sessionResumeOptions{CurrentDirectory: current})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Apply(resumePromptSwitchCWD); err != nil {
		t.Fatal(err)
	}
	message := continueSessionResume(t.Context(), &lifecycleAgentRunnerAdapter{resumeFlowRunner: runner}, controller)().(sessionLoadedMsg)
	if message.err == nil || !strings.Contains(message.err.Error(), "runtime restart is unavailable") || runner.loads != 0 {
		t.Fatalf("unsupported switch = %v, loads %d", message.err, runner.loads)
	}
}

func TestTUIResumeRollsBackAgentAndDirectoryWhenExactLoadFails(t *testing.T) {
	current, stored := t.TempDir(), t.TempDir()
	runner := &lifecycleResumeRunner{
		fakeRunner: &fakeRunner{agentName: "reviewer", agents: []agentInfo{{Name: "reviewer"}, {Name: "builder"}}},
		metadata: sessionInfo{
			ThreadID: "thread", CheckpointID: "checkpoint", Agent: "builder", Directory: stored,
		},
		checkpointErr: errors.New("checkpoint changed"),
	}
	controller, err := prepareSessionResume(t.Context(), runner, "thread", sessionResumeOptions{CurrentDirectory: current})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Apply(resumePromptSwitchAgent); err != nil {
		t.Fatal(err)
	}
	if err := controller.Apply(resumePromptSwitchCWD); err != nil {
		t.Fatal(err)
	}
	message := continueSessionResume(t.Context(), runner, controller)().(sessionLoadedMsg)
	currentResolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatal(err)
	}
	if message.err == nil || !strings.Contains(message.err.Error(), "checkpoint changed") || runner.agentName != "reviewer" || runner.switchedDirectory != currentResolved {
		t.Fatalf("rollback = error %v, agent %q, directory %q", message.err, runner.agentName, runner.switchedDirectory)
	}
	model := newTUIModel(t.Context(), runner, current, "openai:model", "new", false, false, "")
	model.sessionPicker = &sessionPickerState{resuming: true}
	model.resumeController = controller
	model.Update(message)
	if model.resumeController != nil || model.sessionPicker.resuming || model.sessionPicker.err == nil {
		t.Fatalf("failed resume left an active modal: controller %#v, picker %#v", model.resumeController, model.sessionPicker)
	}
}

// lifecycleAgentRunnerAdapter supplies the wider TUI interface without adding
// a directory-switch capability; the test pins the fail-closed type assertion.
type lifecycleAgentRunnerAdapter struct {
	*fakeRunner
	resumeFlowRunner *resumeFlowRunner
}

func (runner *lifecycleAgentRunnerAdapter) AgentName() string {
	return runner.resumeFlowRunner.AgentName()
}
func (runner *lifecycleAgentRunnerAdapter) ListAgents(ctx context.Context) ([]agentInfo, error) {
	return runner.resumeFlowRunner.ListAgents(ctx)
}
func (runner *lifecycleAgentRunnerAdapter) SessionMetadata(ctx context.Context, threadID string) (sessionInfo, error) {
	return runner.resumeFlowRunner.SessionMetadata(ctx, threadID)
}
func (runner *lifecycleAgentRunnerAdapter) LoadSessionCheckpoint(ctx context.Context, threadID, checkpointID string) ([]damessage.Message, error) {
	return runner.resumeFlowRunner.LoadSessionCheckpoint(ctx, threadID, checkpointID)
}
