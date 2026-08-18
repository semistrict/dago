package datalon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"testing"
	"time"
)

type testChannel struct {
	id         string
	mu         sync.Mutex
	handler    Handler
	sends      []string
	events     *[]string
	startErr   error
	stopErr    error
	sendResult *SendResult
}

func (channel *testChannel) ID() string { return channel.id }
func (channel *testChannel) Start(_ context.Context, handler Handler) error {
	channel.handler = handler
	if channel.events != nil {
		*channel.events = append(*channel.events, "start:"+channel.id)
	}
	return channel.startErr
}
func (channel *testChannel) Stop(context.Context) error {
	if channel.events != nil {
		*channel.events = append(*channel.events, "stop:"+channel.id)
	}
	return channel.stopErr
}
func (channel *testChannel) Send(_ context.Context, conversationID, text string) SendResult {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	channel.sends = append(channel.sends, conversationID+":"+text)
	if channel.sendResult != nil {
		return *channel.sendResult
	}
	return SendResult{Success: true}
}

type testRuntime struct {
	mu       sync.Mutex
	events   *[]string
	requests []Request
	invoke   func(context.Context, Request) (Result, error)
	startErr error
	stopErr  error
}

func (runtime *testRuntime) Start(context.Context) error {
	if runtime.events != nil {
		*runtime.events = append(*runtime.events, "start:runtime")
	}
	return runtime.startErr
}
func (runtime *testRuntime) Stop(context.Context) error {
	if runtime.events != nil {
		*runtime.events = append(*runtime.events, "stop:runtime")
	}
	return runtime.stopErr
}
func (runtime *testRuntime) Invoke(ctx context.Context, request Request) (Result, error) {
	runtime.mu.Lock()
	runtime.requests = append(runtime.requests, request)
	runtime.mu.Unlock()
	if runtime.invoke != nil {
		return runtime.invoke(ctx, request)
	}
	return Result{Text: request.Text}, nil
}

type testScheduler struct {
	events   *[]string
	handler  ScheduledHandler
	startErr error
	stopErr  error
}

func (scheduler *testScheduler) Start(_ context.Context, handler ScheduledHandler) error {
	scheduler.handler = handler
	if scheduler.events != nil {
		*scheduler.events = append(*scheduler.events, "start:scheduler")
	}
	return scheduler.startErr
}
func (scheduler *testScheduler) Stop(context.Context) error {
	if scheduler.events != nil {
		*scheduler.events = append(*scheduler.events, "stop:scheduler")
	}
	return scheduler.stopErr
}

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{StateRoot: t.TempDir(), Workspace: t.TempDir()}
}

func TestHostLifecycleOrderAndScheduler(t *testing.T) {
	t.Parallel()
	events := []string{}
	runtime := &testRuntime{events: &events}
	first := &testChannel{id: "first", events: &events}
	second := &testChannel{id: "second", events: &events}
	scheduler := &testScheduler{events: &events}
	host := NewScheduledHost(runtime, scheduler, testConfig(t), first, second)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if scheduler.handler == nil || first.handler == nil || second.handler == nil {
		t.Fatal("start did not install handlers")
	}
	text, err := scheduler.handler(t.Context(), ScheduledJob{ID: "job", ConversationID: "thread", Prompt: "scheduled"})
	if err != nil || text != "scheduled" {
		t.Fatalf("scheduled result = %q, %v", text, err)
	}
	if err := host.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:runtime", "start:first", "start:second", "start:scheduler", "stop:second", "stop:first", "stop:scheduler", "stop:runtime"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if err := host.Stop(t.Context()); err != nil {
		t.Fatalf("second Stop = %v", err)
	}
}

func TestStartRollsBackPartialChannelFailure(t *testing.T) {
	t.Parallel()
	events := []string{}
	runtime := &testRuntime{events: &events}
	first := &testChannel{id: "first", events: &events}
	second := &testChannel{id: "second", events: &events, startErr: errors.New("offline")}
	host := NewHost(runtime, testConfig(t), first, second)
	if err := host.Start(t.Context()); err == nil {
		t.Fatal("Start unexpectedly succeeded")
	}
	want := []string{"start:runtime", "start:first", "start:second", "stop:first", "stop:runtime"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if host.Running() {
		t.Fatal("host remains running after rollback")
	}
}

func TestHostSerializesSameConversationAndRunsDifferentConversations(t *testing.T) {
	t.Parallel()
	entered := make(chan string, 3)
	release := make(chan struct{})
	runtime := &testRuntime{invoke: func(ctx context.Context, request Request) (Result, error) {
		entered <- request.ConversationID
		select {
		case <-release:
			return Result{Text: request.Text}, nil
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}}
	channel := &testChannel{id: "chat"}
	host := NewHost(runtime, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })
	errs := make(chan error, 3)
	go func() { errs <- host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: "a"}) }()
	if got := <-entered; got != "chat:one" {
		t.Fatalf("first conversation = %q", got)
	}
	go func() { errs <- host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: "b"}) }()
	go func() { errs <- host.Receive(t.Context(), channel, Message{ConversationID: "two", Text: "c"}) }()
	select {
	case got := <-entered:
		if got != "chat:two" {
			t.Fatalf("concurrent conversation = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("different conversation did not run concurrently")
	}
	select {
	case got := <-entered:
		t.Fatalf("same conversation ran concurrently: %q", got)
	default:
	}
	close(release)
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := <-entered; got != "chat:one" {
		t.Fatalf("queued conversation = %q", got)
	}
}

func TestStopCommandCancelsActiveAndQueuedConversationWork(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	runtime := &testRuntime{invoke: func(ctx context.Context, _ Request) (Result, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}
	channel := &testChannel{id: "chat"}
	host := NewHost(runtime, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })
	errs := make(chan error, 2)
	go func() { errs <- host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: "first"}) }()
	<-entered
	go func() { errs <- host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: "queued"}) }()
	state := host.conversation("chat:one")
	for {
		state.mu.Lock()
		waiting := state.waiting
		state.mu.Unlock()
		if waiting > 0 {
			break
		}
		goruntime.Gosched()
	}
	if err := host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: " /STOP extra "}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("cancelled receive = %v", err)
		}
	}
	select {
	case <-entered:
		t.Fatal("queued work invoked after /stop")
	default:
	}
	channel.mu.Lock()
	defer channel.mu.Unlock()
	if fmt.Sprint(channel.sends) != "[one:Stopped current run.]" {
		t.Fatalf("sends = %v", channel.sends)
	}
}

func TestHostPropagatesWorkspaceRecursionMetadataAndBounds(t *testing.T) {
	t.Parallel()
	runtime := &testRuntime{}
	channel := &testChannel{id: "chat"}
	config := testConfig(t)
	config.RecursionLimit = 19
	config.MaxMessageBytes = 4
	host := NewHost(runtime, config, channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })
	message := Message{ConversationID: "one", Text: "four", SenderID: "sender", MessageID: "message", Metadata: map[string]any{"own": true}}
	if err := host.Receive(t.Context(), channel, message); err != nil {
		t.Fatal(err)
	}
	if err := host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: "overs"}); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("large message error = %v", err)
	}
	runtime.mu.Lock()
	request := runtime.requests[0]
	runtime.mu.Unlock()
	if request.Workspace != config.Workspace || request.RecursionLimit != 19 || request.Metadata["channel"] != "chat" || request.Metadata["own"] != true {
		t.Fatalf("request = %+v", request)
	}
	if _, exists := message.Metadata["channel"]; exists {
		t.Fatal("Receive mutated caller metadata")
	}
}

func TestEchoFallbackAndStableJSON(t *testing.T) {
	t.Parallel()
	channel := &testChannel{id: "chat"}
	host := NewHost(nil, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })
	if err := host.Receive(t.Context(), channel, Message{ConversationID: "one", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	channel.mu.Lock()
	if fmt.Sprint(channel.sends) != "[one:hello]" {
		t.Fatalf("sends = %v", channel.sends)
	}
	channel.mu.Unlock()
	encoded, err := json.Marshal(Message{ConversationID: "one", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"conversation_id":"one","text":"hello"}` {
		t.Fatalf("JSON = %s", encoded)
	}
}

func TestHostRejectsDuplicateChannelIDsAndFailedSends(t *testing.T) {
	t.Parallel()
	first := &testChannel{id: "same"}
	second := &testChannel{id: "same"}
	if err := NewHost(nil, testConfig(t), first, second).Start(t.Context()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate error = %v", err)
	}
	failed := &testChannel{id: "failed", sendResult: &SendResult{Error: "unavailable", Retryable: true}}
	host := NewHost(nil, testConfig(t), failed)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })
	if err := host.Receive(t.Context(), failed, Message{ConversationID: "one", Text: "hello"}); !errors.Is(err, ErrSendFailed) {
		t.Fatalf("send error = %v", err)
	}
}

func TestConstructorsRejectTypedNilManagedComponents(t *testing.T) {
	t.Parallel()
	assertPanic := func(name string, call func()) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("constructor did not panic")
				}
			}()
			call()
		})
	}
	var channel *testChannel
	assertPanic("channel", func() { NewHost(nil, Config{}, channel) })
	var scheduler *testScheduler
	assertPanic("scheduler", func() { NewScheduledHost(nil, scheduler, Config{}) })
}

func TestStartReturnsRollbackFailureAndCanRestart(t *testing.T) {
	t.Parallel()
	startFailure := errors.New("start failure")
	rollbackFailure := errors.New("rollback failure")
	runtime := &testRuntime{startErr: startFailure, stopErr: rollbackFailure}
	host := NewHost(runtime, testConfig(t))
	err := host.Start(t.Context())
	if !errors.Is(err, startFailure) || !errors.Is(err, rollbackFailure) {
		t.Fatalf("Start error = %v", err)
	}
	if host.root != nil || host.cancel != nil || host.Running() {
		t.Fatal("failed start retained lifecycle state")
	}
	runtime.startErr, runtime.stopErr = nil, nil
	if err := host.Start(t.Context()); err != nil {
		t.Fatalf("restart after failure: %v", err)
	}
	if err := host.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestStopCancelsWorkBeforeStoppingRuntime(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	runtime := &testRuntime{invoke: func(ctx context.Context, _ Request) (Result, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return Result{}, ctx.Err()
	}}
	channel := &testChannel{id: "chat"}
	host := NewHost(runtime, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- host.Receive(context.Background(), channel, Message{ConversationID: "one", Text: "wait"})
	}()
	<-entered
	if err := host.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("runtime stopped before active invocation observed cancellation")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive error = %v", err)
	}
}
