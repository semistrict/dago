package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
)

func TestCompiledInvokeAppliesParallelUpdatesDeterministically(t *testing.T) {
	schema := Schema{Fields: map[string]Field{
		"items": Aggregate(
			func() any { return []string{} },
			appendStringWrites,
			cloneStringSlice,
		),
	}}
	builder := NewBuilder(schema)
	mustAddNode(t, builder, "a", func(context.Context, dastate.Values, Runtime) (Command, error) {
		return Command{Update: dastate.Values{"items": []string{"a"}}}, nil
	})
	mustAddNode(t, builder, "b", func(context.Context, dastate.Values, Runtime) (Command, error) {
		return Command{Update: dastate.Values{"items": []string{"b"}}}, nil
	})
	mustAddEdge(t, builder, Start, "a")
	mustAddEdge(t, builder, Start, "b")
	mustAddEdge(t, builder, "a", End)
	mustAddEdge(t, builder, "b", End)
	graph := mustCompile(t, builder, CompileOptions{MaxConcurrency: 2})

	result, err := graph.Invoke(context.Background(), Invocation{
		Config: dacheckpoint.Config{ThreadID: "parallel"},
		State:  dastate.Values{"items": []string{"input"}},
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	assertStateValue(t, result.State, "items", []string{"input", "a", "b"})
}

func TestCompiledInvokeRejectsConcurrentLastValueWrites(t *testing.T) {
	schema := Schema{Fields: map[string]Field{"value": LastValue(identityClone)}}
	builder := NewBuilder(schema)
	for _, name := range []string{"a", "b"} {
		name := name
		mustAddNode(t, builder, name, func(context.Context, dastate.Values, Runtime) (Command, error) {
			return Command{Update: dastate.Values{"value": name}}, nil
		})
		mustAddEdge(t, builder, Start, name)
		mustAddEdge(t, builder, name, End)
	}
	graph := mustCompile(t, builder, CompileOptions{})
	_, err := graph.Invoke(context.Background(), Invocation{Config: dacheckpoint.Config{ThreadID: "conflict"}})
	if !errors.Is(err, ErrInvalidStateUpdate) {
		t.Fatalf("Invoke() error = %v, want %v", err, ErrInvalidStateUpdate)
	}
}

func TestCompiledConditionalRoutingSeesUpdatedState(t *testing.T) {
	schema := Schema{Fields: map[string]Field{"route": LastValue(identityClone)}}
	builder := NewBuilder(schema)
	mustAddNode(t, builder, "choose", func(context.Context, dastate.Values, Runtime) (Command, error) {
		return Command{Update: dastate.Values{"route": "right"}}, nil
	})
	mustAddNode(t, builder, "right", func(context.Context, dastate.Values, Runtime) (Command, error) {
		return Command{Update: dastate.Values{"route": "done"}}, nil
	})
	mustAddEdge(t, builder, Start, "choose")
	if err := builder.AddConditional("choose", func(_ context.Context, values dastate.Values) ([]string, error) {
		if values["route"] != "right" {
			return nil, fmt.Errorf("route value = %v", values["route"])
		}
		return []string{"right"}, nil
	}); err != nil {
		t.Fatalf("AddConditional() error = %v", err)
	}
	mustAddEdge(t, builder, "right", End)
	graph := mustCompile(t, builder, CompileOptions{})
	result, err := graph.Invoke(context.Background(), Invocation{Config: dacheckpoint.Config{ThreadID: "route"}})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	assertStateValue(t, result.State, "route", "done")
}

func TestCompiledDeltaStateSurvivesRepeatedInvocations(t *testing.T) {
	schema := Schema{Fields: map[string]Field{
		"items": Delta(
			func() any { return []string{} }, appendStringWrites, cloneStringSlice, 1000,
		),
	}}
	builder := NewBuilder(schema)
	calls := 0
	mustAddNode(t, builder, "append", func(context.Context, dastate.Values, Runtime) (Command, error) {
		calls++
		return Command{Update: dastate.Values{"items": []string{fmt.Sprintf("node-%d", calls)}}}, nil
	})
	mustAddEdge(t, builder, Start, "append")
	mustAddEdge(t, builder, "append", End)
	saver := dacheckpoint.NewMemorySaver()
	graph := mustCompile(t, builder, CompileOptions{Saver: saver})
	config := dacheckpoint.Config{ThreadID: "delta"}

	first, err := graph.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"input-1"}},
	})
	if err != nil {
		t.Fatalf("first Invoke() error = %v", err)
	}
	assertStateValue(t, first.State, "items", []string{"input-1", "node-1"})
	second, err := graph.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"input-2"}},
	})
	if err != nil {
		t.Fatalf("second Invoke() error = %v", err)
	}
	assertStateValue(t, second.State, "items", []string{"input-1", "node-1", "input-2", "node-2"})

	tuple, err := saver.GetTuple(context.Background(), second.Config)
	if err != nil || tuple == nil {
		t.Fatalf("GetTuple() = %+v, %v", tuple, err)
	}
	if _, storedFullValue := tuple.Checkpoint.ChannelValues["items"]; storedFullValue {
		t.Fatal("non-snapshot delta checkpoint stored a full value")
	}
}

func TestRetainedThreadStateSkipsDeltaReplayAndInvalidatesAfterFailure(t *testing.T) {
	schema := Schema{Fields: map[string]Field{
		"items": Delta(
			func() any { return []string{} }, appendStringWrites, cloneStringSlice, 1000,
		),
	}}
	failing := false
	builder := NewBuilder(schema)
	mustAddNode(t, builder, "append", func(context.Context, dastate.Values, Runtime) (Command, error) {
		if failing {
			failing = false
			return Command{}, errors.New("fail once")
		}
		return Command{Update: dastate.Values{"items": []string{"node"}}}, nil
	})
	mustAddEdge(t, builder, Start, "append")
	mustAddEdge(t, builder, "append", End)

	baseSaver := dacheckpoint.NewMemorySaver()
	seed := mustCompile(t, builder, CompileOptions{Saver: baseSaver})
	config := dacheckpoint.Config{ThreadID: "retained"}
	if _, err := seed.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"seed"}},
	}); err != nil {
		t.Fatal(err)
	}

	saver := &deltaHistoryCountingSaver{Saver: baseSaver}
	retained := mustCompile(t, builder, CompileOptions{Saver: saver, RetainThreadState: true})
	if _, err := retained.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"first"}},
	}); err != nil {
		t.Fatal(err)
	}
	if saver.historyCalls != 1 {
		t.Fatalf("history calls after restore = %d, want 1", saver.historyCalls)
	}
	if _, err := retained.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"second"}},
	}); err != nil {
		t.Fatal(err)
	}
	if saver.historyCalls != 1 {
		t.Fatalf("history calls after retained invoke = %d, want 1", saver.historyCalls)
	}

	failing = true
	if _, err := retained.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"fail"}},
	}); err == nil {
		t.Fatal("failing Invoke() error = nil")
	}
	if _, err := retained.Invoke(context.Background(), Invocation{
		Config: config, State: dastate.Values{"items": []string{"recover"}},
	}); err != nil {
		t.Fatal(err)
	}
	if saver.historyCalls != 2 {
		t.Fatalf("history calls after invalidation = %d, want 2", saver.historyCalls)
	}
}

type deltaHistoryCountingSaver struct {
	dacheckpoint.Saver
	historyCalls int
}

func (saver *deltaHistoryCountingSaver) GetDeltaChannelHistory(
	ctx context.Context,
	config dacheckpoint.Config,
	channels []string,
) (map[string]dacheckpoint.DeltaHistory, error) {
	saver.historyCalls++
	return saver.Saver.GetDeltaChannelHistory(ctx, config, channels)
}

func TestCompiledInterruptAndResume(t *testing.T) {
	schema := Schema{Fields: map[string]Field{"answer": LastValue(identityClone)}}
	builder := NewBuilder(schema)
	mustAddNode(t, builder, "approval", func(_ context.Context, _ dastate.Values, runtime Runtime) (Command, error) {
		if runtime.Resume == nil {
			return Command{Interrupt: &Interrupt{ID: "approval", Value: "continue?"}}, nil
		}
		return Command{Update: dastate.Values{"answer": runtime.Resume}}, nil
	})
	mustAddEdge(t, builder, Start, "approval")
	mustAddEdge(t, builder, "approval", End)
	saver := dacheckpoint.NewMemorySaver()
	graph := mustCompile(t, builder, CompileOptions{Saver: saver})
	config := dacheckpoint.Config{ThreadID: "interrupt"}

	paused, err := graph.Invoke(context.Background(), Invocation{Config: config})
	if err != nil {
		t.Fatalf("paused Invoke() error = %v", err)
	}
	if len(paused.Interrupts) != 1 || paused.Interrupts[0].ID != "approval" {
		t.Fatalf("interrupts = %+v", paused.Interrupts)
	}
	resumed, err := graph.Invoke(context.Background(), Invocation{Config: config, Resume: "yes"})
	if err != nil {
		t.Fatalf("resumed Invoke() error = %v", err)
	}
	assertStateValue(t, resumed.State, "answer", "yes")
}

func TestCompiledContainsNodePanic(t *testing.T) {
	builder := NewBuilder(Schema{})
	mustAddNode(t, builder, "panic", func(context.Context, dastate.Values, Runtime) (Command, error) {
		panic("boom")
	})
	mustAddEdge(t, builder, Start, "panic")
	mustAddEdge(t, builder, "panic", End)
	graph := mustCompile(t, builder, CompileOptions{})
	if _, err := graph.Invoke(context.Background(), Invocation{Config: dacheckpoint.Config{ThreadID: "panic"}}); err == nil {
		t.Fatal("Invoke() error = nil, want panic error")
	}
}

func TestCompiledRetriesWholeNodeAndHonorsPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		builder := NewBuilder(Schema{Fields: map[string]Field{"value": LastValue(identityClone)}})
		var calls atomic.Int32
		mustAddNode(t, builder, "retry", func(context.Context, dastate.Values, Runtime) (Command, error) {
			if calls.Add(1) < 3 {
				return Command{}, errors.New("transient")
			}
			return Command{Update: dastate.Values{"value": "done"}}, nil
		})
		mustAddEdge(t, builder, Start, "retry")
		mustAddEdge(t, builder, "retry", End)
		graph := mustCompile(t, builder, CompileOptions{Retry: RetryPolicy{Attempts: 3, Backoff: 500 * time.Millisecond}})
		started := time.Now()
		result, err := graph.Invoke(t.Context(), Invocation{Config: dacheckpoint.Config{ThreadID: "retry"}})
		if err != nil {
			t.Fatal(err)
		}
		assertStateValue(t, result.State, "value", "done")
		if calls.Load() != 3 || time.Since(started) != time.Second {
			t.Fatalf("calls = %d, elapsed = %s; want 3 calls in 1s", calls.Load(), time.Since(started))
		}
	})
}

func TestCompiledRetryBackoffIsCancelable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		builder := NewBuilder(Schema{})
		var calls atomic.Int32
		mustAddNode(t, builder, "retry", func(context.Context, dastate.Values, Runtime) (Command, error) {
			calls.Add(1)
			return Command{}, errors.New("transient")
		})
		mustAddEdge(t, builder, Start, "retry")
		graph := mustCompile(t, builder, CompileOptions{Retry: RetryPolicy{Attempts: 3, Backoff: 500 * time.Millisecond}})
		ctx, cancel := context.WithCancel(t.Context())
		started := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := graph.Invoke(ctx, Invocation{Config: dacheckpoint.Config{ThreadID: "cancel-retry"}})
			done <- err
		}()

		synctest.Wait()
		if calls.Load() != 1 || time.Since(started) != 0 {
			t.Fatalf("before cancel: calls=%d elapsed=%s; want one call and active backoff", calls.Load(), time.Since(started))
		}
		cancel()
		synctest.Wait()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Invoke() error = %v, want canceled", err)
		}
		if calls.Load() != 1 || time.Since(started) != 0 {
			t.Fatalf("after cancel: calls=%d elapsed=%s; want no retry or clock advance", calls.Load(), time.Since(started))
		}
	})
}

func TestEphemeralValueLivesForOneSuperstep(t *testing.T) {
	builder := NewBuilder(Schema{Fields: map[string]Field{
		"signal": Ephemeral(identityClone),
		"seen":   Aggregate(func() any { return []string{} }, appendStringWrites, cloneStringSlice),
	}})
	mustAddNode(t, builder, "first", func(_ context.Context, values dastate.Values, _ Runtime) (Command, error) {
		if values["signal"] != "input" {
			return Command{}, fmt.Errorf("first signal = %v", values["signal"])
		}
		return Command{Update: dastate.Values{"seen": []string{"first"}}}, nil
	})
	mustAddNode(t, builder, "second", func(_ context.Context, values dastate.Values, _ Runtime) (Command, error) {
		if _, exists := values["signal"]; exists {
			return Command{}, fmt.Errorf("second unexpectedly saw signal")
		}
		return Command{Update: dastate.Values{"seen": []string{"second"}}}, nil
	})
	mustAddEdge(t, builder, Start, "first")
	mustAddEdge(t, builder, "first", "second")
	mustAddEdge(t, builder, "second", End)
	graph := mustCompile(t, builder, CompileOptions{})
	result, err := graph.Invoke(context.Background(), Invocation{
		Config: dacheckpoint.Config{ThreadID: "ephemeral"}, State: dastate.Values{"signal": "input"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStateValue(t, result.State, "seen", []string{"first", "second"})
}

func TestRuntimeProvidesStoreCacheAndPreOverlayState(t *testing.T) {
	memoryStore := dastore.NewMemory()
	memoryCache := dacache.NewMemory()
	builder := NewBuilder(Schema{Fields: map[string]Field{"value": LastValue(identityClone)}})
	mustAddNode(t, builder, "sender", func(context.Context, dastate.Values, Runtime) (Command, error) {
		return Command{Sends: []Send{{Node: "receiver", Input: dastate.Values{"overlay": "send"}}}}, nil
	})
	mustAddNode(t, builder, "receiver", func(ctx context.Context, values dastate.Values, runtime Runtime) (Command, error) {
		if runtime.Store != memoryStore || runtime.Cache != memoryCache {
			return Command{}, errors.New("runtime dependencies not propagated")
		}
		if runtime.Deps != "invocation" {
			return Command{}, fmt.Errorf("runtime deps = %v", runtime.Deps)
		}
		if runtime.Configurable["response_mode"] != "concise" {
			return Command{}, fmt.Errorf("runtime configurable = %#v", runtime.Configurable)
		}
		if values["overlay"] != "send" {
			return Command{}, errors.New("send overlay missing")
		}
		if _, leaked := runtime.Previous["overlay"]; leaked {
			return Command{}, errors.New("send overlay leaked into previous state")
		}
		return Command{Update: dastate.Values{"value": "done"}}, nil
	})
	mustAddEdge(t, builder, Start, "sender")
	mustAddEdge(t, builder, "sender", End)
	mustAddEdge(t, builder, "receiver", End)
	graph := mustCompile(t, builder, CompileOptions{Store: memoryStore, Cache: memoryCache, Deps: "compiled"})
	result, err := graph.Invoke(context.Background(), Invocation{
		Config: dacheckpoint.Config{ThreadID: "runtime"}, Deps: "invocation",
		Configurable: map[string]any{"response_mode": "concise"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStateValue(t, result.State, "value", "done")
}

func TestCompiledStreamOrdersTaskUpdateAndValuesEvents(t *testing.T) {
	builder := NewBuilder(Schema{Fields: map[string]Field{"value": LastValue(identityClone)}})
	mustAddNode(t, builder, "node", func(context.Context, dastate.Values, Runtime) (Command, error) {
		return Command{Update: dastate.Values{"value": "done"}}, nil
	})
	mustAddEdge(t, builder, Start, "node")
	mustAddEdge(t, builder, "node", End)
	graph := mustCompile(t, builder, CompileOptions{})
	stream := graph.Stream(context.Background(), Invocation{Config: dacheckpoint.Config{ThreadID: "stream"}}, 1)
	defer stream.Close()

	var modes []EventMode
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		modes = append(modes, event.Mode)
	}
	want := []EventMode{EventTask, EventUpdate, EventValues}
	if !reflect.DeepEqual(modes, want) {
		t.Fatalf("event modes = %v, want %v", modes, want)
	}
	result, err := stream.Result(context.Background())
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	assertStateValue(t, result.State, "value", "done")
}

func appendStringWrites(current any, writes []any) (any, error) {
	result := cloneStringSlice(current).([]string)
	for _, write := range writes {
		values, ok := write.([]string)
		if !ok {
			return nil, fmt.Errorf("write type = %T", write)
		}
		result = append(result, values...)
	}
	return result, nil
}

func cloneStringSlice(value any) any {
	if value == nil {
		return []string{}
	}
	values := value.([]string)
	return append([]string{}, values...)
}

func mustAddNode(t *testing.T, builder *Builder, name string, node Node) {
	t.Helper()
	if err := builder.AddNode(name, node); err != nil {
		t.Fatalf("AddNode(%q) error = %v", name, err)
	}
}

func mustAddEdge(t *testing.T, builder *Builder, from, to string) {
	t.Helper()
	if err := builder.AddEdge(from, to); err != nil {
		t.Fatalf("AddEdge(%q, %q) error = %v", from, to, err)
	}
}

func mustCompile(t *testing.T, builder *Builder, options CompileOptions) *Compiled {
	t.Helper()
	graph, err := builder.Compile(options)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return graph
}

func assertStateValue(t *testing.T, values dastate.Values, key string, want any) {
	t.Helper()
	if !reflect.DeepEqual(values[key], want) {
		t.Fatalf("state[%q] = %#v, want %#v", key, values[key], want)
	}
}
