package tracing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/datalon"
)

func TestRuntimeTracesSuccessfulChannelAndCronRuns(t *testing.T) {
	t.Parallel()
	runtime := &fakeRuntime{result: datalon.Result{Text: "answer"}}
	sink := &fakeSink{}
	traced := New(runtime, sink, "assistant", Options{})
	metadata := map[string]any{"trigger": "cron", "nested": map[string]any{"value": "kept"}}
	result, err := traced.Invoke(t.Context(), datalon.Request{ConversationID: "telegram:thread", Text: "question", Metadata: metadata})
	if err != nil || result.Text != "answer" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if len(sink.runs) != 1 || len(sink.completions) != 1 {
		t.Fatalf("runs = %d, completions = %d", len(sink.runs), len(sink.completions))
	}
	run := sink.runs[0]
	if run.Project != defaultProject || run.Name != defaultRunName || run.AssistantID != "assistant" || run.ConversationID != "telegram:thread" || run.Input != "question" {
		t.Fatalf("run = %#v", run)
	}
	if run.Metadata["assistant_id"] != "assistant" || run.Metadata["conversation_id"] != "telegram:thread" {
		t.Fatalf("metadata = %#v", run.Metadata)
	}
	if strings.Join(run.Tags, ",") != "deepagents-talon,assistant:assistant,trigger:cron" {
		t.Fatalf("tags = %v", run.Tags)
	}
	if sink.completions[0].Output != "answer" || sink.completions[0].Error != "" || sink.completions[0].EndTime.IsZero() {
		t.Fatalf("completion = %#v", sink.completions[0])
	}
	metadata["nested"].(map[string]any)["value"] = "changed"
	if run.Metadata["nested"].(map[string]any)["value"] != "kept" {
		t.Fatal("trace metadata aliases request metadata")
	}
}

func TestRuntimePreservesRuntimeErrorsAndTraceFailures(t *testing.T) {
	t.Parallel()
	runtimeErr := errors.New("runtime failure")
	reported := []error{}
	traced := New(&fakeRuntime{err: runtimeErr}, &fakeSink{beginErr: errors.New("offline")}, "assistant", Options{OnError: func(err error) { reported = append(reported, err) }})
	_, err := traced.Invoke(t.Context(), datalon.Request{})
	if !errors.Is(err, runtimeErr) {
		t.Fatalf("error = %v", err)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "start trace") {
		t.Fatalf("reported = %v", reported)
	}

	reported = nil
	sink := &fakeSink{endErr: errors.New("upload failed")}
	traced = New(&fakeRuntime{err: runtimeErr}, sink, "assistant", Options{OnError: func(err error) { reported = append(reported, err) }})
	_, err = traced.Invoke(t.Context(), datalon.Request{})
	if !errors.Is(err, runtimeErr) || len(sink.completions) != 1 || sink.completions[0].Error != runtimeErr.Error() {
		t.Fatalf("error = %v, completion = %#v", err, sink.completions)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "finish trace") {
		t.Fatalf("reported = %v", reported)
	}
}

func TestRuntimeFinishesAfterCancellationAndPanic(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sink := &fakeSink{}
	traced := New(&fakeRuntime{err: context.Canceled}, sink, "assistant", Options{})
	_, err := traced.Invoke(ctx, datalon.Request{})
	if !errors.Is(err, context.Canceled) || len(sink.completions) != 1 || sink.endContextErr != nil {
		t.Fatalf("error = %v, end context = %v", err, sink.endContextErr)
	}

	sink = &fakeSink{}
	traced = New(&fakeRuntime{panicValue: "private panic"}, sink, "assistant", Options{})
	defer func() {
		if recovered := recover(); recovered != "private panic" {
			t.Fatalf("panic = %#v", recovered)
		}
		if len(sink.completions) != 1 || sink.completions[0].Error != "runtime panicked" {
			t.Fatalf("completion = %#v", sink.completions)
		}
	}()
	_, _ = traced.Invoke(t.Context(), datalon.Request{})
}

func TestRuntimeEnvironmentGateAndBounds(t *testing.T) {
	t.Parallel()
	if Enabled(map[string]string{"LANGSMITH_TRACING": "true"}) {
		t.Fatal("tracing enabled without key")
	}
	if !Enabled(map[string]string{"LANGSMITH_TRACING": "YES", "LANGSMITH_API_KEY": "key"}) {
		t.Fatal("tracing was not enabled")
	}
	runtime := &fakeRuntime{result: datalon.Result{Text: "abcdef"}}
	sink := &fakeSink{}
	disabled := NewFromEnv(runtime, sink, "assistant", map[string]string{}, Options{})
	if _, err := disabled.Invoke(t.Context(), datalon.Request{Text: "abcdef"}); err != nil {
		t.Fatal(err)
	}
	if len(sink.runs) != 0 {
		t.Fatal("disabled tracing emitted a run")
	}

	enabled := NewFromEnv(runtime, sink, "assistant", map[string]string{"LANGSMITH_TRACING": "1", "LANGSMITH_API_KEY": "key"}, Options{MaxPayloadBytes: 4, MaxMetadataBytes: 8})
	if _, err := enabled.Invoke(t.Context(), datalon.Request{Text: "ééé", Metadata: map[string]any{"oversized": strings.Repeat("x", 100)}}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sink.runs[0].Input, "…") || !strings.HasPrefix(sink.runs[0].Input, "éé") {
		t.Fatalf("input = %q", sink.runs[0].Input)
	}
	if _, exists := sink.runs[0].Metadata["oversized"]; exists {
		t.Fatal("oversized metadata was retained")
	}
}

type fakeRuntime struct {
	result           datalon.Result
	err              error
	panicValue       any
	started, stopped int
}

func (runtime *fakeRuntime) Start(context.Context) error { runtime.started++; return nil }
func (runtime *fakeRuntime) Stop(context.Context) error  { runtime.stopped++; return nil }
func (runtime *fakeRuntime) Invoke(context.Context, datalon.Request) (datalon.Result, error) {
	if runtime.panicValue != nil {
		panic(runtime.panicValue)
	}
	return runtime.result, runtime.err
}

type fakeSink struct {
	mu                              sync.Mutex
	runs                            []Run
	completions                     []Completion
	beginErr, endErr, endContextErr error
}

func (sink *fakeSink) Begin(_ context.Context, run Run) (Span, error) {
	if sink.beginErr != nil {
		return nil, sink.beginErr
	}
	sink.mu.Lock()
	sink.runs = append(sink.runs, run)
	sink.mu.Unlock()
	return fakeSpan{sink}, nil
}

type fakeSpan struct{ sink *fakeSink }

func (span fakeSpan) End(ctx context.Context, completion Completion) error {
	span.sink.mu.Lock()
	defer span.sink.mu.Unlock()
	span.sink.endContextErr = ctx.Err()
	span.sink.completions = append(span.sink.completions, completion)
	return span.sink.endErr
}
