package daacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"iter"
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
	"github.com/semistrict/dago/daworkflow"
)

func TestNewPanicsForTypedNilRunner(t *testing.T) {
	var runner *dagent.Agent
	defer func() {
		if recover() == nil {
			t.Fatal("New did not panic")
		}
	}()
	New(runner, Options{})
}

func TestServerStreamsPromptToolsAndApproval(t *testing.T) {
	var executions atomic.Int32
	var workingDirectory string
	write := datool.MustNew("write_file", "Write a file.", func(ctx context.Context, input struct {
		Path string `json:"path"`
	}) (string, error) {
		executions.Add(1)
		runtime, _ := datool.RuntimeFromContext(ctx)
		value, _ := runtime.Configurable.Get(ConfigurableCWD)
		workingDirectory, _ = value.(string)
		return "wrote " + input.Path, nil
	})
	script := modeltest.New(damodel.Profile{ToolCalling: true, NativeStreaming: true},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if got := request.Messages[len(request.Messages)-1].TextContent(); got != "please write[guide](/workspace/guide.md)" {
					return &testError{"prompt", got}
				}
				return nil
			},
			Chunks: []damodel.Chunk{{
				MessageDelta: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
					ID: "call-1", Name: "write_file", Arguments: json.RawMessage(`{"path":"/note.txt"}`),
				}}}, Done: true,
			}},
		},
		modeltest.Step{Chunks: []damodel.Chunk{
			{MessageDelta: damessage.Assistant("done")},
			{MessageDelta: damessage.Message{Role: damessage.RoleAssistant}, Done: true},
		}},
	)
	runner := dagent.New(script, dagent.Options{
		Tools: []datool.Tool{write}, Saver: dacheckpoint.NewMemorySaver(),
		Middleware: []dagent.Middleware{dagent.HumanApproval([]dagent.ApprovalRule{{
			Pattern: "write_file", Description: "Allow this write?",
			AllowedDecisions: []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject},
		}})},
	})
	server := New(runner, Options{Name: "test-agent", Version: "1.2.3"})
	client := &testClient{permission: dagent.ApprovalApprove}
	connection, stop := startTestConnection(t, server, client)
	defer stop()

	ctx := t.Context()
	initialized, err := connection.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	if initialized.ProtocolVersion != 1 || initialized.AgentInfo == nil || initialized.AgentInfo.Name != "test-agent" || initialized.AgentInfo.Version != "1.2.3" {
		t.Fatalf("initialize = %#v", initialized)
	}
	if initialized.AgentCapabilities.SessionCapabilities.Close == nil || initialized.AgentCapabilities.SessionCapabilities.AdditionalDirectories != nil || initialized.AgentCapabilities.LoadSession {
		t.Fatalf("capabilities = %#v", initialized.AgentCapabilities)
	}

	root := t.TempDir()
	created, err := connection.NewSession(ctx, acp.NewSessionRequest{Cwd: root, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	messageID := "d5c6908d-b87c-4910-a1ed-1b4cbcacb4a0"
	response, err := connection.Prompt(ctx, acp.PromptRequest{
		SessionId: created.SessionId, MessageId: &messageID,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("please write"), acp.ResourceLinkBlock("guide", "/workspace/guide.md"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != acp.StopReasonEndTurn || response.UserMessageId == nil || *response.UserMessageId != messageID {
		t.Fatalf("prompt response = %#v", response)
	}
	if executions.Load() != 1 || workingDirectory != root || script.Remaining() != 0 {
		t.Fatalf("executions = %d, cwd = %q, remaining = %d", executions.Load(), workingDirectory, script.Remaining())
	}

	permission, updates := client.snapshot()
	if permission == nil || permission.ToolCall.Title == nil || *permission.ToolCall.Title != "Allow this write?" || len(permission.Options) != 2 {
		t.Fatalf("permission = %#v", permission)
	}
	var started, completed, text bool
	for _, notification := range updates {
		switch {
		case notification.Update.ToolCall != nil:
			call := notification.Update.ToolCall
			started = call.ToolCallId == "call-1" && call.Kind == acp.ToolKindEdit && len(call.Locations) == 1 && call.Locations[0].Path == "/note.txt"
		case notification.Update.ToolCallUpdate != nil:
			call := notification.Update.ToolCallUpdate
			completed = call.ToolCallId == "call-1" && call.Status != nil && *call.Status == acp.ToolCallStatusCompleted
		case notification.Update.AgentMessageChunk != nil:
			block := notification.Update.AgentMessageChunk.Content.Text
			text = text || (block != nil && block.Text == "done")
		}
	}
	if !started || !completed || !text {
		t.Fatalf("updates = %#v", updates)
	}

	if _, err := connection.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId}); err != nil {
		t.Fatal(err)
	}
}

func TestServerCancelStopsPromptAndReturnsCancelled(t *testing.T) {
	model := &blockingChat{started: make(chan struct{})}
	runner := dagent.New(model, dagent.Options{})
	server := New(runner, Options{})
	connection, stop := startTestConnection(t, server, &testClient{})
	defer stop()

	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	response := make(chan acp.PromptResponse, 1)
	errors := make(chan error, 1)
	go func() {
		result, promptErr := connection.Prompt(t.Context(), acp.PromptRequest{
			SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("wait")},
		})
		response <- result
		errors <- promptErr
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	if err := connection.Cancel(t.Context(), acp.CancelNotification{SessionId: created.SessionId}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errors:
		if err != nil {
			t.Fatal(err)
		}
		if result := <-response; result.StopReason != acp.StopReasonCancelled {
			t.Fatalf("prompt response = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not stop after cancellation")
	}
}

func TestCloseSessionCancelsActivePromptBeforeClosingResources(t *testing.T) {
	model := &blockingChat{started: make(chan struct{})}
	runner := dagent.New(model, dagent.Options{})
	var closed atomic.Int32
	server := NewFactory(func(context.Context, SessionConfig) (Runner, io.Closer, error) {
		return runner, closerFunc(func() error { closed.Add(1); return nil }), nil
	}, Options{})
	connection, stop := startTestConnection(t, server, &testClient{})
	defer stop()
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	promptDone := make(chan error, 1)
	go func() {
		response, promptErr := connection.Prompt(t.Context(), acp.PromptRequest{SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock("wait")}})
		if promptErr == nil && response.StopReason != acp.StopReasonCancelled {
			promptErr = errors.New("prompt was not cancelled")
		}
		promptDone <- promptErr
	}()
	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	if _, err := connection.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: created.SessionId}); err != nil {
		t.Fatal(err)
	}
	if err := <-promptDone; err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 1 {
		t.Fatalf("closed = %d", closed.Load())
	}
}

func TestServerPublishesAndControlsWorkflowExtensions(t *testing.T) {
	base := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{})
	source := &workflowTestRunner{
		Runner:  base,
		updates: make(chan daworkflow.Status, 1),
		workflows: []daworkflow.Status{{
			Version: 1, TaskID: "workflow-1", RunID: "wf_1", Name: "review", Status: "running",
		}},
	}
	client := &testClient{extensions: make(chan testExtension, 1)}
	connection, stop := startTestConnection(t, NewFactory(func(context.Context, SessionConfig) (Runner, io.Closer, error) {
		return source, nil, nil
	}, Options{}), client)
	defer stop()
	if _, err := connection.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatal(err)
	}
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}

	source.updates <- source.workflows[0]
	select {
	case extension := <-client.extensions:
		if extension.method != WorkflowUpdateMethod {
			t.Fatalf("workflow extension method = %q", extension.method)
		}
		var update WorkflowUpdate
		if err := json.Unmarshal(extension.params, &update); err != nil {
			t.Fatal(err)
		}
		if update.Version != 1 || update.SessionID != string(created.SessionId) || update.Workflow.RunID != "wf_1" {
			t.Fatalf("workflow update = %#v", update)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workflow extension notification was not received")
	}

	raw, err := connection.CallExtension(t.Context(), WorkflowListMethod, workflowListRequest{
		Version: 1, SessionID: string(created.SessionId),
	})
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Version   int                 `json:"version"`
		Workflows []daworkflow.Status `json:"workflows"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Version != 1 || len(listed.Workflows) != 1 || listed.Workflows[0].RunID != "wf_1" {
		t.Fatalf("listed workflows = %#v", listed)
	}
	if _, err := connection.CallExtension(t.Context(), WorkflowCancelMethod, workflowCancelRequest{
		Version: 1, SessionID: string(created.SessionId), RunID: "wf_1",
	}); err != nil {
		t.Fatal(err)
	}
	if source.cancelled != "wf_1" {
		t.Fatalf("cancelled workflow = %q", source.cancelled)
	}
}

func TestServerSupportsT3SessionSetupAndDurableLoad(t *testing.T) {
	base := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{})
	var configs []SessionConfig
	var closed atomic.Int32
	factory := func(_ context.Context, config SessionConfig) (Runner, io.Closer, error) {
		configs = append(configs, config)
		return &loadableTestRunner{Runner: base, cwd: config.CWD, messages: []damessage.Message{
			damessage.Human("previous prompt"), damessage.System("not replayed"),
			{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{
				{Type: damessage.BlockText, Text: "previous response"},
				{Type: damessage.BlockImage, Data: []byte("image"), MIMEType: "image/png"},
				{Type: damessage.BlockAudio, Data: []byte("audio"), MIMEType: "audio/wav"},
				{Type: damessage.BlockFile, Text: "not replayed"},
			}, ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/guide.md"}`)}}},
			{Role: damessage.RoleTool, ToolCallID: "call-1", ToolStatus: damessage.ToolStatusError,
				Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "tool failed"}}},
		}}, closerFunc(func() error { closed.Add(1); return nil }), nil
	}
	category := acp.SessionConfigOptionCategoryModel
	values := acp.SessionConfigSelectOptionsUngrouped{{Name: "test-model", Value: "test-model"}}
	options := []acp.SessionConfigOption{{Select: &acp.SessionConfigOptionSelect{
		Id: "model", Name: "Model", Category: &category, CurrentValue: "test-model",
		Options: acp.SessionConfigSelectOptions{Ungrouped: &values},
	}}}
	server := NewFactory(factory, Options{
		LoadSession: true, ImagePrompts: true, EmbeddedContext: true,
		AuthMethods:   []acp.AuthMethod{{Agent: &acp.AuthMethodAgent{Id: "cursor_login", Name: "Configured"}}},
		ConfigOptions: options,
	})
	client := &testClient{}
	connection, stop := startTestConnection(t, server, client)
	defer stop()

	initialized, err := connection.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	if !initialized.AgentCapabilities.LoadSession || !initialized.AgentCapabilities.McpCapabilities.Http || !initialized.AgentCapabilities.McpCapabilities.Sse || len(initialized.AuthMethods) != 1 {
		t.Fatalf("initialize = %#v", initialized)
	}
	if _, err := connection.Authenticate(t.Context(), acp.AuthenticateRequest{MethodId: "unknown"}); err == nil {
		t.Fatal("unknown authentication method was accepted")
	}
	if _, err := connection.Authenticate(t.Context(), acp.AuthenticateRequest{MethodId: "cursor_login"}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	mcpServer := acp.McpServer{Http: &acp.McpServerHttpInline{Name: "t3", Url: "http://127.0.0.1/mcp", Headers: []acp.HttpHeader{}}}
	created, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: root, McpServers: []acp.McpServer{mcpServer}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.ConfigOptions) != 1 || len(configs) != 1 || len(configs[0].MCPServers) != 1 {
		t.Fatalf("created = %#v, configs = %#v", created, configs)
	}
	configured, err := connection.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: created.SessionId, ConfigId: "model", Value: "test-model",
	}})
	if err != nil || len(configured.ConfigOptions) != 1 {
		t.Fatalf("set config = %#v, %v", configured, err)
	}
	for _, request := range []acp.SetSessionConfigOptionRequest{
		{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: created.SessionId, ConfigId: "unknown", Value: "test-model"}},
		{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: created.SessionId, ConfigId: "model", Value: "unadvertised-model"}},
	} {
		if _, err := connection.SetSessionConfigOption(t.Context(), request); err == nil {
			t.Errorf("invalid config request %#v was accepted", request)
		}
	}
	if _, err := connection.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: created.SessionId}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: created.SessionId, ConfigId: "model", Value: "test-model",
	}}); err == nil {
		t.Fatal("closed session accepted a configuration update")
	}

	loaded, err := connection.LoadSession(t.Context(), acp.LoadSessionRequest{
		SessionId: "persisted", Cwd: root, McpServers: []acp.McpServer{mcpServer},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.ConfigOptions) != 1 || len(configs) != 2 || configs[1].ID != "persisted" {
		t.Fatalf("loaded = %#v, configs = %#v", loaded, configs)
	}
	_, updates := client.snapshot()
	if len(updates) != 6 || updates[0].Update.UserMessageChunk == nil || updates[1].Update.AgentMessageChunk == nil || updates[2].Update.AgentMessageChunk == nil || updates[2].Update.AgentMessageChunk.Content.Image == nil || updates[3].Update.AgentMessageChunk == nil || updates[3].Update.AgentMessageChunk.Content.Audio == nil || updates[4].Update.ToolCall == nil || updates[4].Update.ToolCall.RawInput == nil || updates[5].Update.ToolCallUpdate == nil || updates[5].Update.ToolCallUpdate.Status == nil || *updates[5].Update.ToolCallUpdate.Status != acp.ToolCallStatusFailed {
		t.Fatalf("replay updates = %#v", updates)
	}
	for index, update := range updates {
		if update.Meta["isReplay"] != true {
			t.Fatalf("replay update %d omitted replay metadata: %#v", index, update)
		}
	}
	if _, err := connection.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: "persisted"}); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 2 {
		t.Fatalf("closed = %d", closed.Load())
	}
}

func TestLoadSessionFailureClosesFactoryResources(t *testing.T) {
	var closed atomic.Int32
	server := NewFactory(func(context.Context, SessionConfig) (Runner, io.Closer, error) {
		return &failingLoadRunner{Runner: dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{})},
			closerFunc(func() error { closed.Add(1); return nil }), nil
	}, Options{LoadSession: true})
	connection, stop := startTestConnection(t, server, &testClient{})
	defer stop()
	_, err := connection.LoadSession(t.Context(), acp.LoadSessionRequest{
		SessionId: "missing", Cwd: t.TempDir(), McpServers: []acp.McpServer{},
	})
	if err == nil {
		t.Fatal("load failure was hidden")
	}
	if closed.Load() != 1 {
		t.Fatalf("closed = %d", closed.Load())
	}
}

func TestSessionFactoryFailureClosesReturnedResources(t *testing.T) {
	var closed atomic.Int32
	server := NewFactory(func(context.Context, AgentSessionContext) (Runner, io.Closer, error) {
		return nil, closerFunc(func() error { closed.Add(1); return nil }), errors.New("factory failed")
	}, Options{})
	connection, stop := startTestConnection(t, server, &testClient{})
	defer stop()
	_, err := connection.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err == nil || !strings.Contains(err.Error(), "factory failed") {
		t.Fatalf("NewSession error = %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("closed = %d", closed.Load())
	}
}

func TestLoadSessionRejectsDifferentPersistedWorkingDirectory(t *testing.T) {
	original := t.TempDir()
	requested := t.TempDir()
	var closed atomic.Int32
	base := dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{})
	server := NewFactory(func(context.Context, SessionConfig) (Runner, io.Closer, error) {
		return &loadableTestRunner{Runner: base, cwd: original, messages: []damessage.Message{damessage.Human("private history")}},
			closerFunc(func() error { closed.Add(1); return nil }), nil
	}, Options{LoadSession: true})
	client := &testClient{}
	connection, stop := startTestConnection(t, server, client)
	defer stop()
	_, err := connection.LoadSession(t.Context(), acp.LoadSessionRequest{
		SessionId: "persisted", Cwd: requested, McpServers: []acp.McpServer{},
	})
	if err == nil || !strings.Contains(err.Error(), "must match the working directory") {
		t.Fatalf("LoadSession error = %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("closed = %d", closed.Load())
	}
	_, updates := client.snapshot()
	if len(updates) != 0 {
		t.Fatalf("mismatched session leaked replay updates: %#v", updates)
	}
}

func TestLoadSessionDoesNotConstructUnadvertisedPersistedModel(t *testing.T) {
	var configs []AgentSessionContext
	root := t.TempDir()
	factory := func(_ context.Context, config AgentSessionContext) (Runner, io.Closer, error) {
		configs = append(configs, cloneAgentSessionContext(config))
		return &loadableTestRunner{
			Runner: dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{}),
			cwd:    root, model: "unadvertised-model",
		}, closerFunc(func() error { return nil }), nil
	}
	connection, stop := startTestConnection(t, NewFactory(factory, Options{
		LoadSession: true, ConfigOptions: testModelConfigOptions("alpha", "beta"),
	}), &testClient{})
	defer stop()
	loaded, err := connection.LoadSession(t.Context(), acp.LoadSessionRequest{
		SessionId: "persisted", Cwd: root, McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := currentModelOption(loaded.ConfigOptions); got != "alpha" {
		t.Fatalf("loaded model = %q", got)
	}
	if len(configs) != 1 || configs[0].Model != "alpha" {
		t.Fatalf("factory contexts = %#v", configs)
	}
}

func TestPromptMessageConvertsAdvertisedRichContent(t *testing.T) {
	agent := newProtocolAgent(t.Context(), dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{}), nil, Options{
		ImagePrompts: true, AudioPrompts: true, EmbeddedContext: true,
	})
	textResource := acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: "file:///guide.txt", Text: "guide"}}
	message, err := agent.promptMessage([]acp.ContentBlock{
		acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("image")), "image/png"),
		acp.AudioBlock(base64.StdEncoding.EncodeToString([]byte("audio")), "audio/wav"),
		acp.ResourceBlock(textResource),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Content) != 3 || message.Content[0].Type != damessage.BlockImage || string(message.Content[0].Data) != "image" || message.Content[1].Type != damessage.BlockAudio || string(message.Content[1].Data) != "audio" || message.Content[2].Type != damessage.BlockFile || message.Content[2].Text != "guide" {
		t.Fatalf("message = %#v", message)
	}
}

func TestPromptMessageRejectsUnadvertisedRichContent(t *testing.T) {
	agent := newProtocolAgent(t.Context(), dagent.New(modeltest.New(damodel.Profile{}), dagent.Options{}), nil, Options{})
	if _, err := agent.promptMessage([]acp.ContentBlock{acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("image")), "image/png")}); err == nil {
		t.Fatal("unadvertised image was accepted")
	}
}

type loadableTestRunner struct {
	Runner
	messages []damessage.Message
	cwd      string
	model    string
}

type failingLoadRunner struct{ Runner }

func (*failingLoadRunner) LoadACPSession(context.Context, string) (SessionState, error) {
	return SessionState{}, errors.New("session not found")
}

func (runner *loadableTestRunner) LoadACPSession(context.Context, string) (SessionState, error) {
	messages := make([]damessage.Message, len(runner.messages))
	for index := range runner.messages {
		messages[index] = runner.messages[index].Clone()
	}
	return SessionState{Messages: messages, CWD: runner.cwd, Model: runner.model}, nil
}

type closerFunc func() error

func (closer closerFunc) Close() error { return closer() }

type workflowTestRunner struct {
	Runner
	updates   chan daworkflow.Status
	workflows []daworkflow.Status
	cancelled string
}

func (runner *workflowTestRunner) SubscribeWorkflows() (<-chan daworkflow.Status, func()) {
	return runner.updates, func() {}
}

func (runner *workflowTestRunner) Workflows() []daworkflow.Status {
	return append([]daworkflow.Status(nil), runner.workflows...)
}

func (runner *workflowTestRunner) CancelWorkflow(runID string) bool {
	runner.cancelled = runID
	return runID == "wf_1"
}

type testClient struct {
	mu         sync.Mutex
	permission dagent.ApprovalDecision
	request    *acp.RequestPermissionRequest
	updates    []acp.SessionNotification
	extensions chan testExtension
}

type testExtension struct {
	method string
	params json.RawMessage
}

func (client *testClient) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if client.extensions != nil {
		client.extensions <- testExtension{method: method, params: append(json.RawMessage(nil), params...)}
	}
	return nil, nil
}

func (client *testClient) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	client.mu.Lock()
	copy := request
	client.request = &copy
	decision := client.permission
	client.mu.Unlock()
	if decision == "" {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(decision))}, nil
}

func (client *testClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	client.mu.Lock()
	client.updates = append(client.updates, notification)
	client.mu.Unlock()
	return nil
}

func (client *testClient) snapshot() (*acp.RequestPermissionRequest, []acp.SessionNotification) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.request, append([]acp.SessionNotification(nil), client.updates...)
}

func (*testClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}

func (*testClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (*testClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, nil
}

func (*testClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*testClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}

func (*testClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*testClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func startTestConnection(t *testing.T, server *Server, client acp.Client) (*acp.ClientSideConnection, func()) {
	t.Helper()
	clientToServerReader, clientToServerWriter := io.Pipe()
	serverToClientReader, serverToClientWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, clientToServerReader, serverToClientWriter) }()
	connection := acp.NewClientSideConnection(client, clientToServerWriter, serverToClientReader)
	stop := func() {
		_ = clientToServerWriter.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Error("server did not stop")
		}
		_ = serverToClientReader.Close()
		cancel()
	}
	return connection, stop
}

type blockingChat struct {
	started chan struct{}
	once    sync.Once
}

func (*blockingChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{}, io.EOF
}

func (model *blockingChat) Stream(ctx context.Context, _ damodel.Request) (damodel.Stream, error) {
	model.once.Do(func() { close(model.started) })
	return &blockingStream{ctx: ctx}, nil
}

func (*blockingChat) Profile() damodel.Profile { return damodel.Profile{NativeStreaming: true} }

type blockingStream struct{ ctx context.Context }

func (stream *blockingStream) Next(ctx context.Context) (damodel.Chunk, error) {
	select {
	case <-stream.ctx.Done():
		return damodel.Chunk{}, context.Cause(stream.ctx)
	case <-ctx.Done():
		return damodel.Chunk{}, context.Cause(ctx)
	}
}

func (*blockingStream) Close() error { return nil }

func (stream *blockingStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return func(yield func(damodel.Chunk, error) bool) {
		chunk, err := stream.Next(context.Background())
		yield(chunk, err)
	}
}

type testError struct {
	name string
	got  string
}

func (err *testError) Error() string { return err.name + " = " + err.got }

var _ acp.Client = (*testClient)(nil)
var _ damodel.Chat = (*blockingChat)(nil)
