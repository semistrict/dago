package dacode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/daproviders/modelconfig"
	"github.com/semistrict/dago/dastate"
)

type runnerCompactionModel struct {
	*modeltest.Predictable
	fail  atomic.Bool
	block atomic.Bool
}

func (model *runnerCompactionModel) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	if model.block.Load() {
		<-ctx.Done()
		return damodel.Response{}, context.Cause(ctx)
	}
	if model.fail.Load() {
		return damodel.Response{}, errors.New("fixture summary failed")
	}
	return model.Predictable.Invoke(ctx, request)
}

func TestRunnerCompactionExecutesAndCommitsAtExactCheckpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDirectory := t.TempDir()
	profile := damodel.Profile{
		Provider: "openai", Model: "fixture", ContextWindow: 200_000, MaxOutputTokens: 8_192,
		ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true, NativeStreaming: true,
	}
	model := &runnerCompactionModel{Predictable: modeltest.NewPredictable(modeltest.PredictableOptions{
		Profile: &profile, DefaultResponse: "offline conversation summary",
	})}
	credentials := dacredential.NewStore(filepath.Join(t.TempDir(), "auth.json"), time.Now, dacredential.Options{})
	resolver := modelconfig.NewResolver(
		credentials,
		func(name string) (string, bool) { return "fixture-key", name == "OPENAI_API_KEY" },
		map[string]modelconfig.Factory{"openai": func(context.Context, modelconfig.Spec, dacredential.Resolution, modelconfig.Construction) (damodel.Chat, error) {
			return model, nil
		}},
		modelconfig.Options{},
	)
	runnerValue, closer, err := newRunner(runnerOptions{
		Authentication: modelAuthentication{resolver: resolver},
		Model:          "openai:fixture",
		WorkingDir:     t.TempDir(),
		StateDir:       stateDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	runner := runnerValue.(*dagoRunner)
	generation, err := readAgentGeneration(t.Context(), stateDirectory, defaultAgentName)
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]damessage.Message, 0, 80)
	for index := range 40 {
		messages = append(messages,
			damessage.Human("request "+strings.Repeat("context ", 400+index)),
			damessage.Assistant("response "+strings.Repeat("detail ", 400+index)),
		)
	}
	seeded, err := runner.agent.UpdateState(t.Context(), dacheckpoint.Config{ThreadID: "compact-thread"}, dastate.Values{
		dagent.MessagesKey:         messages,
		sessionAgentNameKey:        defaultAgentName,
		sessionAgentGenerationKey:  generation,
		sessionWorkingDirectoryKey: runner.workingDir,
		sessionContextTokensKey:    401_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := runner.CompactSession(t.Context(), "compact-thread", seeded.Config.CheckpointID)
	if err != nil || result.Failed || result.CheckpointID == "" || result.CheckpointID == seeded.Config.CheckpointID || !strings.Contains(result.Output, "Conversation compacted") {
		t.Fatalf("compaction = %#v, %v", result, err)
	}
	tuple, err := runner.saver.GetTuple(t.Context(), dacheckpoint.Config{ThreadID: "compact-thread", CheckpointID: result.CheckpointID})
	if err != nil || tuple == nil {
		t.Fatalf("committed checkpoint = %#v, %v", tuple, err)
	}
	if _, exists := tuple.Checkpoint.ChannelValues["_summarization_event"]; !exists {
		t.Fatal("committed checkpoint omitted the private summarization event")
	}

	model.fail.Store(true)
	failed, err := runner.CompactSession(t.Context(), "compact-thread", result.CheckpointID)
	if err != nil || !failed.Failed || failed.CheckpointID != result.CheckpointID || !strings.Contains(failed.Output, "fixture summary failed") {
		t.Fatalf("failed compaction = %#v, %v", failed, err)
	}
	latest, err := runner.SessionMetadata(t.Context(), "compact-thread")
	if err != nil || latest.CheckpointID != result.CheckpointID {
		t.Fatalf("failure committed state: metadata = %#v, %v", latest, err)
	}

	model.fail.Store(false)
	model.block.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = runner.CompactSession(ctx, "compact-thread", result.CheckpointID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled compaction error = %v", err)
	}
	latest, err = runner.SessionMetadata(t.Context(), "compact-thread")
	if err != nil || latest.CheckpointID != result.CheckpointID {
		t.Fatalf("cancellation committed state: metadata = %#v, %v", latest, err)
	}
}
