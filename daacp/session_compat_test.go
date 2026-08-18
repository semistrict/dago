package daacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

type modelSelectionStore struct {
	sync.Mutex
	values map[string]SessionState
}

type durableModelTestRunner struct {
	*dagent.Agent
	cwd        string
	model      string
	selections *modelSelectionStore
}

func (runner *durableModelTestRunner) LoadACPSession(ctx context.Context, id string) (SessionState, error) {
	snapshot, err := runner.State(ctx, dacheckpoint.Config{ThreadID: id})
	if err != nil {
		return SessionState{}, err
	}
	messages, ok := snapshot.State[dagent.MessagesKey].([]damessage.Message)
	if !ok {
		return SessionState{}, fmt.Errorf("session %q has no transcript", id)
	}
	runner.selections.Lock()
	selection, ok := runner.selections.values[id]
	runner.selections.Unlock()
	if !ok {
		return SessionState{}, fmt.Errorf("session %q has no model selection", id)
	}
	selection.Messages = make([]damessage.Message, len(messages))
	for index := range messages {
		selection.Messages[index] = messages[index].Clone()
	}
	return selection, nil
}

func (runner *durableModelTestRunner) SaveACPModelSelection(_ context.Context, id, model string) error {
	runner.selections.Lock()
	runner.selections.values[id] = SessionState{CWD: runner.cwd, Model: model}
	runner.selections.Unlock()
	return nil
}

func testModelConfigOptions(models ...string) []acp.SessionConfigOption {
	values := make(acp.SessionConfigSelectOptionsUngrouped, len(models))
	for index, model := range models {
		values[index] = acp.SessionConfigSelectOption{Name: model, Value: acp.SessionConfigValueId(model)}
	}
	category := acp.SessionConfigOptionCategoryModel
	return []acp.SessionConfigOption{{Select: &acp.SessionConfigOptionSelect{
		Id: modelConfigID, Name: "Model", Category: &category, CurrentValue: acp.SessionConfigValueId(models[0]),
		Options: acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}}
}

func currentModelOption(options []acp.SessionConfigOption) string {
	for _, option := range options {
		if option.Select != nil && option.Select.Id == modelConfigID {
			return string(option.Select.CurrentValue)
		}
	}
	return ""
}

func TestServerSupportsSuccessivePromptsInOneSession(t *testing.T) {
	var requests atomic.Int32
	model := modeltest.New(damodel.Profile{NativeStreaming: true},
		modeltest.Step{Check: checkLastPrompt(&requests, "first"), Chunks: responseChunks("first response")},
		modeltest.Step{Check: checkLastPrompt(&requests, "second"), Chunks: responseChunks("second response")},
	)
	runner := dagent.New(model, dagent.Options{Saver: dacheckpoint.NewMemorySaver()})
	client := &testClient{}
	connection, stop := startTestConnection(t, New(runner, Options{}), client)
	defer stop()
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first", "second"} {
		response, promptErr := connection.Prompt(t.Context(), acp.PromptRequest{
			SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(prompt)},
		})
		if promptErr != nil {
			t.Fatal(promptErr)
		}
		if response.StopReason != acp.StopReasonEndTurn {
			t.Fatalf("%q response = %#v", prompt, response)
		}
	}
	if requests.Load() != 2 || model.Remaining() != 0 {
		t.Fatalf("requests = %d, remaining = %d", requests.Load(), model.Remaining())
	}
	_, updates := client.snapshot()
	var text strings.Builder
	for _, update := range updates {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			text.WriteString(chunk.Content.Text.Text)
		}
	}
	if got := text.String(); got != "first responsesecond response" {
		t.Fatalf("assistant text = %q", got)
	}
}

func TestCancelledPromptDoesNotPoisonNextTurn(t *testing.T) {
	model := &recoverableChat{firstStarted: make(chan struct{})}
	runner := dagent.New(model, dagent.Options{Saver: dacheckpoint.NewMemorySaver()})
	client := &testClient{}
	connection, stop := startTestConnection(t, New(runner, Options{}), client)
	defer stop()
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan acp.PromptResponse, 1)
	firstErr := make(chan error, 1)
	go func() {
		response, promptErr := connection.Prompt(t.Context(), acp.PromptRequest{SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("hang")}})
		firstDone <- response
		firstErr <- promptErr
	}()
	select {
	case <-model.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first prompt did not start")
	}
	if err := connection.Cancel(t.Context(), acp.CancelNotification{SessionId: created.SessionId}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatal(err)
		}
		if response := <-firstDone; response.StopReason != acp.StopReasonCancelled {
			t.Fatalf("first response = %#v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled prompt did not return")
	}
	second, err := connection.Prompt(t.Context(), acp.PromptRequest{SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("continue")}})
	if err != nil {
		t.Fatal(err)
	}
	if second.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("second response = %#v", second)
	}
	_, updates := client.snapshot()
	found := false
	for _, update := range updates {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == "recovered" {
			found = true
		}
	}
	if !found {
		t.Fatalf("updates = %#v", updates)
	}
}

func TestSessionFactoryKeepsConcurrentEditorSessionsIsolated(t *testing.T) {
	factory := func(_ context.Context, config SessionConfig) (Runner, io.Closer, error) {
		model := modeltest.New(damodel.Profile{NativeStreaming: true}, modeltest.Step{Chunks: responseChunks(config.CWD)})
		return dagent.New(model, dagent.Options{Saver: dacheckpoint.NewMemorySaver()}), closerFunc(func() error { return nil }), nil
	}
	client := &testClient{}
	connection, stop := startTestConnection(t, NewFactory(factory, Options{}), client)
	defer stop()
	created := make([]acp.NewSessionResponse, 2)
	for index, cwd := range []string{"/workspace/one", "/workspace/two"} {
		response, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
		if err != nil {
			t.Fatal(err)
		}
		created[index] = response
	}
	var group sync.WaitGroup
	errorsBySession := make(chan error, 2)
	for _, session := range created {
		group.Add(1)
		go func() {
			defer group.Done()
			response, err := connection.Prompt(t.Context(), acp.PromptRequest{SessionId: session.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("identify")}})
			if err == nil && response.StopReason != acp.StopReasonEndTurn {
				err = errors.New("prompt did not end normally")
			}
			errorsBySession <- err
		}()
	}
	group.Wait()
	close(errorsBySession)
	for err := range errorsBySession {
		if err != nil {
			t.Fatal(err)
		}
	}
	updates := waitForUpdates(t, client, 2)
	want := map[acp.SessionId]string{created[0].SessionId: "/workspace/one", created[1].SessionId: "/workspace/two"}
	for _, update := range updates {
		chunk := update.Update.AgentMessageChunk
		if chunk == nil || chunk.Content.Text == nil {
			continue
		}
		if got := chunk.Content.Text.Text; got != want[update.SessionId] {
			t.Fatalf("session %q text = %q, want %q", update.SessionId, got, want[update.SessionId])
		}
		delete(want, update.SessionId)
	}
	if len(want) != 0 {
		t.Fatalf("missing session updates: %#v", want)
	}
}

func TestSessionModelSwitchRebuildsWithHistoryAndRestoresOnLoad(t *testing.T) {
	saver := dacheckpoint.NewMemorySaver()
	selections := &modelSelectionStore{values: map[string]SessionState{}}
	var mu sync.Mutex
	var contexts []AgentSessionContext
	var closed atomic.Int32
	factory := func(_ context.Context, config AgentSessionContext) (Runner, io.Closer, error) {
		mu.Lock()
		contexts = append(contexts, cloneAgentSessionContext(config))
		mu.Unlock()
		step := modeltest.Step{Chunks: responseChunks("alpha response")}
		if config.Model == "beta" {
			step = modeltest.Step{
				Check: func(request damodel.Request) error {
					var transcript []string
					for _, message := range request.Messages {
						if message.Role == damessage.RoleHuman || message.Role == damessage.RoleAssistant {
							transcript = append(transcript, string(message.Role)+":"+message.TextContent())
						}
					}
					want := []string{"human:first", "assistant:alpha response", "human:second"}
					if !slices.Equal(transcript, want) {
						return fmt.Errorf("transcript = %#v, want %#v", transcript, want)
					}
					return nil
				},
				Chunks: responseChunks("beta response"),
			}
		}
		runner := &durableModelTestRunner{
			Agent: dagent.New(modeltest.New(damodel.Profile{NativeStreaming: true}, step), dagent.Options{Saver: saver}),
			cwd:   config.CWD, model: config.Model, selections: selections,
		}
		return runner, closerFunc(func() error { closed.Add(1); return nil }), nil
	}
	options := Options{LoadSession: true, ConfigOptions: testModelConfigOptions("alpha", "beta")}
	root := t.TempDir()
	firstClient := &testClient{}
	connection, stop := startTestConnection(t, NewFactory(factory, options), firstClient)
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: root, McpServers: []acp.McpServer{}})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if got := currentModelOption(created.ConfigOptions); got != "alpha" {
		stop()
		t.Fatalf("new session model = %q", got)
	}
	if _, err := connection.Prompt(t.Context(), acp.PromptRequest{
		SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("first")},
	}); err != nil {
		stop()
		t.Fatal(err)
	}
	configured, err := connection.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: created.SessionId, ConfigId: modelConfigID, Value: "beta",
	}})
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if got := currentModelOption(configured.ConfigOptions); got != "beta" {
		stop()
		t.Fatalf("configured model = %q", got)
	}
	if _, err := connection.Prompt(t.Context(), acp.PromptRequest{
		SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("second")},
	}); err != nil {
		stop()
		t.Fatal(err)
	}
	sessionID := string(created.SessionId)
	stop()

	secondClient := &testClient{}
	restarted, stopRestarted := startTestConnection(t, NewFactory(factory, options), secondClient)
	defer stopRestarted()
	loaded, err := restarted.LoadSession(t.Context(), acp.LoadSessionRequest{
		SessionId: acp.SessionId(sessionID), Cwd: root, McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := currentModelOption(loaded.ConfigOptions); got != "beta" {
		t.Fatalf("loaded model = %q", got)
	}
	mu.Lock()
	gotContexts := append([]AgentSessionContext(nil), contexts...)
	mu.Unlock()
	if len(gotContexts) != 4 || gotContexts[0].Model != "alpha" || gotContexts[1].Model != "beta" || gotContexts[2].Model != "alpha" || gotContexts[3].Model != "beta" {
		t.Fatalf("factory contexts = %#v", gotContexts)
	}
	if _, err := restarted.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: acp.SessionId(sessionID)}); err != nil {
		t.Fatal(err)
	}
	if got := closed.Load(); got != 4 {
		t.Fatalf("closed runners = %d, want 4", got)
	}
}

func TestSessionModelSwitchPersistenceFailureKeepsCurrentRunner(t *testing.T) {
	var alphaClosed atomic.Int32
	var betaClosed atomic.Int32
	factory := func(_ context.Context, config AgentSessionContext) (Runner, io.Closer, error) {
		steps := []modeltest.Step(nil)
		closed := &betaClosed
		if config.Model == "alpha" {
			steps = append(steps, modeltest.Step{Chunks: responseChunks("still alpha")})
			closed = &alphaClosed
		}
		runner := &loadableTestRunner{
			Runner: dagent.New(modeltest.New(damodel.Profile{NativeStreaming: true}, steps...), dagent.Options{}),
			cwd:    config.CWD,
		}
		return runner, closerFunc(func() error { closed.Add(1); return nil }), nil
	}
	connection, stop := startTestConnection(t, NewFactory(factory, Options{
		LoadSession: true, ConfigOptions: testModelConfigOptions("alpha", "beta"),
	}), &testClient{})
	defer stop()
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = connection.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: created.SessionId, ConfigId: modelConfigID, Value: "beta",
	}})
	if err == nil || !strings.Contains(err.Error(), "durable configuration") {
		t.Fatalf("set model error = %v", err)
	}
	if betaClosed.Load() != 1 || alphaClosed.Load() != 0 {
		t.Fatalf("closed alpha = %d, beta = %d", alphaClosed.Load(), betaClosed.Load())
	}
	response, err := connection.Prompt(t.Context(), acp.PromptRequest{
		SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("continue")},
	})
	if err != nil || response.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("prompt = %#v, %v", response, err)
	}
}

func TestPermissionOutcomesMatchT3Expectations(t *testing.T) {
	tests := []struct {
		name       string
		permission dagent.ApprovalDecision
		wantStop   acp.StopReason
		wantError  string
		wantRuns   int32
	}{
		{name: "allow once", permission: dagent.ApprovalApprove, wantStop: acp.StopReasonEndTurn, wantRuns: 1},
		{name: "reject once", permission: dagent.ApprovalReject, wantStop: acp.StopReasonEndTurn},
		{name: "cancel dialog", wantStop: acp.StopReasonCancelled},
		{name: "unknown selected option", permission: "future-decision", wantError: "unknown option"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var runs atomic.Int32
			runner := approvalTestRunner(&runs)
			connection, stop := startTestConnection(t, New(runner, Options{}), &testClient{permission: test.permission})
			defer stop()
			created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
			if err != nil {
				t.Fatal(err)
			}
			response, promptErr := connection.Prompt(t.Context(), acp.PromptRequest{
				SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("change it")},
			})
			if test.wantError != "" {
				if promptErr == nil || !strings.Contains(promptErr.Error(), test.wantError) {
					t.Fatalf("error = %v", promptErr)
				}
				return
			}
			if promptErr != nil {
				t.Fatal(promptErr)
			}
			if response.StopReason != test.wantStop {
				t.Fatalf("response = %#v", response)
			}
			if runs.Load() != test.wantRuns {
				t.Fatalf("tool runs = %d", runs.Load())
			}
		})
	}
}

func TestUnsupportedInterruptsAndApprovalChoicesFailExplicitly(t *testing.T) {
	tests := []struct {
		name string
		tool datool.Tool
		rule dagent.ApprovalRule
		want string
	}{
		{
			name: "custom interrupt",
			tool: datool.Func{Spec: datool.Definition{Name: "custom_pause", Description: "Pause.", InputSchema: json.RawMessage(`{"type":"object"}`)}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
				return datool.Result{Interrupt: &datool.Interrupt{ID: "custom_pause", Value: "wait"}}, nil
			}},
			want: "cannot resume interrupt",
		},
		{
			name: "edit-only approval",
			tool: datool.MustNew("change", "Change a value.", func(context.Context, struct{}) (string, error) { return "changed", nil }),
			rule: dagent.ApprovalRule{Pattern: "change", AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalEdit}},
			want: "cannot represent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{
				Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: test.tool.Definition().Name, Arguments: json.RawMessage(`{}`)}},
			}}})
			options := dagent.Options{Tools: []datool.Tool{test.tool}, Saver: dacheckpoint.NewMemorySaver()}
			if test.rule.Pattern != "" {
				options.Middleware = []dagent.Middleware{dagent.HumanApproval([]dagent.ApprovalRule{test.rule})}
			}
			connection, stop := startTestConnection(t, New(dagent.New(model, options), Options{}), &testClient{permission: dagent.ApprovalApprove})
			defer stop()
			created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
			if err != nil {
				t.Fatal(err)
			}
			_, promptErr := connection.Prompt(t.Context(), acp.PromptRequest{SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("run")}})
			if promptErr == nil || !strings.Contains(promptErr.Error(), test.want) {
				t.Fatalf("error = %v", promptErr)
			}
		})
	}
}

func TestConfigOptionValidationMatchesAdvertisedState(t *testing.T) {
	options := []acp.SessionConfigOption{
		{Select: &acp.SessionConfigOptionSelect{Id: "model", CurrentValue: "default"}},
		{Boolean: &acp.SessionConfigOptionBoolean{Id: "fast", CurrentValue: true}},
	}
	tests := []struct {
		name    string
		request acp.SetSessionConfigOptionRequest
		wantErr bool
	}{
		{name: "current model", request: acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{ConfigId: "model", Value: "default"}}},
		{name: "unadvertised model", request: acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{ConfigId: "model", Value: "other"}}, wantErr: true},
		{name: "unknown select", request: acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{ConfigId: "other", Value: "default"}}, wantErr: true},
		{name: "current boolean", request: acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{ConfigId: "fast", Value: true}}},
		{name: "unsupported boolean", request: acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{ConfigId: "fast", Value: false}}, wantErr: true},
		{name: "empty request", request: acp.SetSessionConfigOptionRequest{}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateConfigOption(options, test.request)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func checkLastPrompt(count *atomic.Int32, want string) func(damodel.Request) error {
	return func(request damodel.Request) error {
		count.Add(1)
		if got := request.Messages[len(request.Messages)-1].TextContent(); got != want {
			return errors.New("last prompt = " + got)
		}
		return nil
	}
}

func responseChunks(text string) []damodel.Chunk {
	return []damodel.Chunk{{MessageDelta: damessage.Assistant(text)}, {MessageDelta: damessage.Message{Role: damessage.RoleAssistant}, Done: true}}
}

func approvalTestRunner(runs *atomic.Int32) *dagent.Agent {
	tool := datool.MustNew("change", "Change a value.", func(context.Context, struct{}) (string, error) { runs.Add(1); return "changed", nil })
	model := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "change", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	return dagent.New(model, dagent.Options{
		Tools: []datool.Tool{tool}, Saver: dacheckpoint.NewMemorySaver(),
		Middleware: []dagent.Middleware{dagent.HumanApproval([]dagent.ApprovalRule{{
			Pattern: "change", AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject},
		}})},
	})
}

type recoverableChat struct {
	calls        atomic.Int32
	firstStarted chan struct{}
	once         sync.Once
}

func (*recoverableChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{}, io.EOF
}

func (model *recoverableChat) Stream(ctx context.Context, _ damodel.Request) (damodel.Stream, error) {
	if model.calls.Add(1) == 1 {
		model.once.Do(func() { close(model.firstStarted) })
		return &blockingStream{ctx: ctx}, nil
	}
	return &chunkStream{chunks: responseChunks("recovered")}, nil
}

func (*recoverableChat) Profile() damodel.Profile { return damodel.Profile{NativeStreaming: true} }

type chunkStream struct {
	chunks []damodel.Chunk
	index  int
}

func (stream *chunkStream) Next(context.Context) (damodel.Chunk, error) {
	if stream.index >= len(stream.chunks) {
		return damodel.Chunk{}, io.EOF
	}
	chunk := stream.chunks[stream.index]
	stream.index++
	return chunk, nil
}
func (*chunkStream) Close() error { return nil }
func (stream *chunkStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(context.Background(), stream)
}

var _ damodel.Chat = (*recoverableChat)(nil)
