package datalon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type approvalSend struct {
	conversation string
	text         string
}

type approvalChannel struct {
	id      string
	handler Handler
	sends   chan approvalSend
}

func newApprovalChannel(id string) *approvalChannel {
	return &approvalChannel{id: id, sends: make(chan approvalSend, 64)}
}

func (channel *approvalChannel) ID() string { return channel.id }
func (channel *approvalChannel) Start(_ context.Context, handler Handler) error {
	channel.handler = handler
	return nil
}
func (*approvalChannel) Stop(context.Context) error { return nil }
func (channel *approvalChannel) Send(_ context.Context, conversationID, text string) SendResult {
	channel.sends <- approvalSend{conversation: conversationID, text: text}
	return SendResult{Success: true, MessageID: "sent"}
}

func TestHostRoutesApprovalReplyToOriginatingSender(t *testing.T) {
	t.Parallel()
	channel := newApprovalChannel("chat")
	runtime := &testRuntime{invoke: approvalTestInvoke}
	host := NewHost(runtime, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })

	done := make(chan error, 1)
	go func() {
		done <- host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "run"})
	}()
	prompt := nextApprovalSend(t, channel)
	for _, expected := range []string{
		"Tool approval required.", "1. `dangerous_tool`", "Args: `{\"path\":\"/workspace\"}`", "Reply `👍` / `approve`",
	} {
		if !strings.Contains(prompt.text, expected) {
			t.Fatalf("prompt missing %q:\n%s", expected, prompt.text)
		}
	}
	if err := host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: " 👍🏽 "}); err != nil {
		t.Fatal(err)
	}
	result := nextApprovalSend(t, channel)
	if result.text != "decision:approve" {
		t.Fatalf("result = %+v", result)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHostKeepsSpoofedUnknownAndDuplicateRepliesSerialized(t *testing.T) {
	t.Parallel()
	channel := newApprovalChannel("chat")
	runtime := &testRuntime{invoke: approvalTestInvoke}
	host := NewHost(runtime, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })

	runDone := make(chan error, 1)
	go func() {
		runDone <- host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "run"})
	}()
	_ = nextApprovalSend(t, channel)

	queued := make(chan error, 2)
	go func() {
		queued <- host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "spoof", Text: "approve"})
	}()
	go func() {
		queued <- host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "maybe"})
	}()
	select {
	case send := <-channel.sends:
		t.Fatalf("unbound reply bypassed serialization: %+v", send)
	case <-time.After(20 * time.Millisecond):
	}
	if err := host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "deny"}); err != nil {
		t.Fatal(err)
	}
	if send := nextApprovalSend(t, channel); send.text != "decision:reject" {
		t.Fatalf("decision result = %+v", send)
	}
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		if err := <-queued; err != nil {
			t.Fatal(err)
		}
		send := nextApprovalSend(t, channel)
		seen[send.text] = true
	}
	if !seen["normal:approve"] || !seen["normal:maybe"] {
		t.Fatalf("serialized results = %v", seen)
	}

	if err := host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "approve"}); err != nil {
		t.Fatal(err)
	}
	if send := nextApprovalSend(t, channel); send.text != "normal:approve" {
		t.Fatalf("stale duplicate result = %+v", send)
	}
}

func TestHostScopesConcurrentApprovalsByChannelAndConversation(t *testing.T) {
	t.Parallel()
	first := newApprovalChannel("first")
	second := newApprovalChannel("second")
	host := NewHost(&testRuntime{invoke: approvalTestInvoke}, testConfig(t), first, second)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(t.Context()) })

	errCh := make(chan error, 2)
	go func() {
		errCh <- host.Receive(t.Context(), first, Message{ConversationID: "same", SenderID: "one", Text: "run"})
	}()
	go func() {
		errCh <- host.Receive(t.Context(), second, Message{ConversationID: "same", SenderID: "two", Text: "run"})
	}()
	_ = nextApprovalSend(t, first)
	_ = nextApprovalSend(t, second)
	if err := host.Receive(t.Context(), first, Message{ConversationID: "same", SenderID: "one", Text: "approve"}); err != nil {
		t.Fatal(err)
	}
	if send := nextApprovalSend(t, first); send.text != "decision:approve" {
		t.Fatalf("first result = %+v", send)
	}
	select {
	case send := <-second.sends:
		t.Fatalf("first reply resolved second channel: %+v", send)
	case <-time.After(20 * time.Millisecond):
	}
	if err := host.Receive(t.Context(), second, Message{ConversationID: "same", SenderID: "two", Text: "deny"}); err != nil {
		t.Fatal(err)
	}
	if send := nextApprovalSend(t, second); send.text != "decision:reject" {
		t.Fatalf("second result = %+v", send)
	}
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostApprovalTimesOutAndStopCancelsPending(t *testing.T) {
	t.Parallel()
	t.Run("timeout", func(t *testing.T) {
		channel := newApprovalChannel("chat")
		config := testConfig(t)
		config.ApprovalTimeout = 20 * time.Millisecond
		host := NewHost(&testRuntime{invoke: approvalTestInvoke}, config, channel)
		if err := host.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer host.Stop(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "run"})
		}()
		_ = nextApprovalSend(t, channel)
		if err := <-done; !errors.Is(err, ErrToolApprovalTimeout) {
			t.Fatalf("timeout error = %v", err)
		}
		if err := host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "approve"}); err != nil {
			t.Fatal(err)
		}
		if send := nextApprovalSend(t, channel); send.text != "normal:approve" {
			t.Fatalf("stale timeout reply = %+v", send)
		}
	})

	t.Run("stop command", func(t *testing.T) {
		channel := newApprovalChannel("chat")
		host := NewHost(&testRuntime{invoke: approvalTestInvoke}, testConfig(t), channel)
		if err := host.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		defer host.Stop(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "run"})
		}()
		_ = nextApprovalSend(t, channel)
		if err := host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "other", Text: "/stop"}); err != nil {
			t.Fatal(err)
		}
		if send := nextApprovalSend(t, channel); send.text != "Stopped current run." {
			t.Fatalf("stop result = %+v", send)
		}
		if err := <-done; err != nil {
			t.Fatalf("stopped run error = %v", err)
		}
	})

	t.Run("host stop", func(t *testing.T) {
		channel := newApprovalChannel("chat")
		host := NewHost(&testRuntime{invoke: approvalTestInvoke}, testConfig(t), channel)
		if err := host.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- host.Receive(context.Background(), channel, Message{ConversationID: "room", Text: "run"})
		}()
		_ = nextApprovalSend(t, channel)
		if err := host.Stop(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("host stop run error = %v", err)
		}
		host.approvals.mu.Lock()
		defer host.approvals.mu.Unlock()
		if len(host.approvals.pending) != 0 {
			t.Fatalf("pending approvals after Stop = %d", len(host.approvals.pending))
		}
	})
}

func TestHostApprovalRejectsAmbiguousOrOversizedRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config func(*Config)
		build  func(string) ToolApprovalRequest
	}{
		{name: "conversation mismatch", build: func(string) ToolApprovalRequest {
			return approvalRequest("other:room", "tool", json.RawMessage(`{}`))
		}},
		{name: "missing interrupt", build: func(conversation string) ToolApprovalRequest {
			request := approvalRequest(conversation, "tool", json.RawMessage(`{}`))
			request.InterruptID = ""
			return request
		}},
		{name: "invalid arguments", build: func(conversation string) ToolApprovalRequest {
			return approvalRequest(conversation, "tool", json.RawMessage(`{bad`))
		}},
		{name: "too many actions", config: func(config *Config) { config.MaxApprovalActions = 1 }, build: func(conversation string) ToolApprovalRequest {
			request := approvalRequest(conversation, "one", json.RawMessage(`{}`))
			request.Actions = append(request.Actions, ToolApprovalAction{Name: "two", Arguments: json.RawMessage(`{}`)})
			return request
		}},
		{name: "prompt too large", config: func(config *Config) { config.MaxApprovalPromptBytes = 64 }, build: func(conversation string) ToolApprovalRequest {
			return approvalRequest(conversation, "tool", json.RawMessage(`{"long":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			channel := newApprovalChannel("chat")
			config := testConfig(t)
			if test.config != nil {
				test.config(&config)
			}
			runtime := &testRuntime{invoke: func(ctx context.Context, request Request) (Result, error) {
				decision, err := request.ApprovalHandler(ctx, test.build(request.ConversationID))
				return Result{Text: string(decision)}, err
			}}
			host := NewHost(runtime, config, channel)
			if err := host.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer host.Stop(t.Context())
			err := host.Receive(t.Context(), channel, Message{ConversationID: "room", Text: "run"})
			if !errors.Is(err, ErrInvalidToolApproval) {
				t.Fatalf("error = %v", err)
			}
			select {
			case send := <-channel.sends:
				t.Fatalf("invalid request sent prompt: %+v", send)
			default:
			}
		})
	}
}

func TestHostRejectsSecondPendingApprovalForConversation(t *testing.T) {
	t.Parallel()
	channel := newApprovalChannel("chat")
	host := NewHost(nil, testConfig(t), channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer host.Stop(t.Context())
	config := host.Config()
	firstDone := make(chan error, 1)
	go func() {
		_, err := host.requestToolApproval(t.Context(), channel, "room", "chat:room", "operator", approvalRequest("chat:room", "tool", json.RawMessage(`{}`)), config)
		firstDone <- err
	}()
	_ = nextApprovalSend(t, channel)
	decision, err := host.requestToolApproval(t.Context(), channel, "room", "chat:room", "operator", approvalRequest("chat:room", "tool", json.RawMessage(`{}`)), config)
	if decision != ToolApprovalReject || !errors.Is(err, ErrToolApprovalPending) {
		t.Fatalf("second approval = %q, %v", decision, err)
	}
	if err := host.Receive(t.Context(), channel, Message{ConversationID: "room", SenderID: "operator", Text: "approve"}); err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestRequestJSONOmitsApprovalHandler(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(Request{
		ConversationID: "chat:room", Text: "run", Workspace: "/workspace", RecursionLimit: 10,
		ApprovalHandler: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalApprove, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Approval") || strings.Contains(string(encoded), "approval") {
		t.Fatalf("handler leaked into JSON: %s", encoded)
	}
}

func approvalTestInvoke(ctx context.Context, request Request) (Result, error) {
	if request.Text != "run" {
		return Result{Text: "normal:" + request.Text}, nil
	}
	if request.ApprovalHandler == nil {
		return Result{}, errors.New("approval handler missing")
	}
	decision, err := request.ApprovalHandler(ctx, approvalRequest(
		request.ConversationID,
		"dangerous_tool",
		json.RawMessage(`{"path":"/workspace"}`),
	))
	if err != nil {
		return Result{}, err
	}
	return Result{Text: "decision:" + string(decision)}, nil
}

func approvalRequest(conversation, name string, arguments json.RawMessage) ToolApprovalRequest {
	return ToolApprovalRequest{
		ConversationID: conversation, InterruptID: "interrupt-1",
		Actions: []ToolApprovalAction{{ID: "call-1", Name: name, Arguments: arguments}},
	}
}

func nextApprovalSend(t *testing.T, channel *approvalChannel) approvalSend {
	t.Helper()
	select {
	case send := <-channel.sends:
		return send
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel send")
		return approvalSend{}
	}
}
