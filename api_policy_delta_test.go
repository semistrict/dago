package dago

import (
	"context"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/davideo"
)

type typedNilRunnable struct{}

type typedNilVideoExtractor struct{ davideo.Extractor }

func (*typedNilRunnable) Invoke(context.Context, ...dagent.RunOption) (dagent.Result, error) {
	return dagent.Result{}, nil
}

func TestStaticAPIsRejectTypedNilRequiredInterfaces(t *testing.T) {
	var runnable *typedNilRunnable
	requirePanicContaining(t, "runnable are required", func() {
		NewRunnableSubagent("worker", "Worker", runnable)
	})

	var model *modeltest.Scripted
	requirePanicContaining(t, "compaction model is nil", func() {
		_, _ = CompactConversation(t.Context(), model, nil)
	})
	requirePanicContaining(t, "rubric model is nil", func() {
		Rubric(model, RubricOptions{})
	})
	requirePanicContaining(t, "summarization model is nil", func() {
		SummarizationTool(model, dabackend.NewMemory(nil), SummarizationToolOptions{})
	})

	var backend *dabackend.Memory
	requirePanicContaining(t, "managed memory guard backend is nil", func() {
		ManagedMemoryGuard(backend, "/memory.md")
	})
	requirePanicContaining(t, "summarization backend is nil", func() {
		SummarizationTool(modeltest.New(damodel.Profile{}), backend, SummarizationToolOptions{})
	})

	var store *recordingConversationSubagentStore
	var runner *recordingConversationSubagentRunner
	requirePanicContaining(t, "store, runner, and working directory are required", func() {
		ConversationSubagentTool(store, &recordingConversationSubagentRunner{}, func() string { return "/workspace" }, "parent", "model")
	})
	requirePanicContaining(t, "store, runner, and working directory are required", func() {
		ConversationSubagentTool(&recordingConversationSubagentStore{}, runner, func() string { return "/workspace" }, "parent", "model")
	})
	for name, ids := range map[string][2]string{
		"parent": {"", "model"},
		"model":  {"parent", ""},
	} {
		t.Run("conversation identity "+name, func(t *testing.T) {
			requirePanicContaining(t, "parent conversation ID and model ID are required", func() {
				ConversationSubagentTool(&recordingConversationSubagentStore{}, &recordingConversationSubagentRunner{}, func() string { return "/workspace" }, ids[0], ids[1])
			})
		})
	}

	var asyncRunner *asyncRunnerStub
	requirePanicContaining(t, "runner is required", func() {
		AsyncSubagents([]AsyncSubagent{{Name: "worker", Description: "Worker", GraphID: "graph", Runner: asyncRunner}})
	})
}

func TestStaticAPIsRejectNegativeLimitsAndKeepZeroDefaults(t *testing.T) {
	model := func() damodel.Chat { return modeltest.New(damodel.Profile{ContextWindow: 100_000}) }

	for name, filesystem := range map[string]Filesystem{
		"read":            {ReadLimit: -1},
		"glob timeout":    {GlobTimeout: -time.Second},
		"execute timeout": {MaxExecuteTimeout: -time.Second},
		"video bytes":     {MaxVideoBytes: -1},
		"sampling rate":   {VideoSamplingRate: -0.5},
	} {
		t.Run("filesystem "+name, func(t *testing.T) {
			requirePanicContaining(t, "filesystem limits cannot be negative", func() {
				New(model(), WithFilesystem(filesystem))
			})
		})
	}

	for name, summarization := range map[string]Summarization{
		"keep messages": {KeepMessages: -1},
		"keep tokens":   {KeepTokens: -1},
		"overflow":      {OverflowClipTokens: -1},
		"argument max": {
			ArgumentTruncation: &ArgumentTruncationOptions{TriggerTokens: 1, KeepTokens: 1, MaxLength: -1},
		},
		"argument preview": {
			ArgumentTruncation: &ArgumentTruncationOptions{TriggerTokens: 1, KeepTokens: 1, PreviewLength: -1},
		},
	} {
		t.Run("summarization "+name, func(t *testing.T) {
			requirePanicContaining(t, "cannot be negative", func() {
				New(model(), WithSummarization(summarization))
			})
		})
	}

	requirePanicContaining(t, "skills maximum file bytes cannot be negative", func() {
		New(model(), WithSkills(Skills{Sources: []string{"/skills"}, MaxFileBytes: -1}))
	})
	requirePanicContaining(t, "compaction limits cannot be negative", func() {
		_, _ = CompactConversation(t.Context(), model(), nil, WithCompactKeepMessages(-1))
	})

	for name, timeouts := range map[string][2]time.Duration{
		"default":      {-time.Second, 0},
		"maximum":      {0, -time.Second},
		"relationship": {time.Hour, time.Minute},
	} {
		t.Run("conversation "+name, func(t *testing.T) {
			requirePanicContaining(t, "timeout", func() {
				ConversationSubagentTool(&recordingConversationSubagentStore{}, &recordingConversationSubagentRunner{}, func() string { return "/workspace" }, "parent", "model", ConversationSubagentOptions{DefaultTimeout: timeouts[0], MaxTimeout: timeouts[1]})
			})
		})
	}

	// The zero values remain valid and select bounded defaults.
	New(model(), WithFilesystem(Filesystem{}), WithSkills(Skills{Sources: []string{"/skills"}}))
	ConversationSubagentTool(&recordingConversationSubagentStore{}, &recordingConversationSubagentRunner{}, func() string { return "/workspace" }, "parent", "model")
}

func TestAgentOptionsTreatTypedNilOptionalInterfacesAsOmitted(t *testing.T) {
	var summaryModel *modeltest.Scripted
	New(modeltest.New(damodel.Profile{ContextWindow: 100_000}), WithSummarization(Summarization{Model: summaryModel}))

	var extractor *typedNilVideoExtractor
	New(modeltest.New(damodel.Profile{}), WithFilesystem(Filesystem{VideoExtractor: extractor}))
}
