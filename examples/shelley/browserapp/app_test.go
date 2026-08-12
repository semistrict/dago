package browserapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
)

func TestBrowserAppRunsDagoTurnAndPublishesOrderedMessages(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan json.RawMessage, 8)
	app.SetEventSink(func(event json.RawMessage) { events <- append(json.RawMessage(nil), event...) })

	response := app.Handle(Request{Method: "POST", URL: "/api/conversations/new", Body: `{"message":"hello"}`})
	if response.Status != 202 || !response.Changed {
		t.Fatalf("new conversation response = %#v", response)
	}
	var created struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(response.Body, &created); err != nil {
		t.Fatal(err)
	}
	if !app.Continue(response.ContinueConversation) {
		t.Fatal("prepared turn did not continue")
	}

	var frames []struct {
		ConversationID    string       `json:"conversation_id"`
		Messages          []apiMessage `json:"messages"`
		ConversationState *struct {
			Working bool `json:"working"`
		} `json:"conversation_state"`
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case raw := <-events:
			var frame struct {
				ConversationID    string       `json:"conversation_id"`
				Messages          []apiMessage `json:"messages"`
				ConversationState *struct {
					Working bool `json:"working"`
				} `json:"conversation_state"`
			}
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			frames = append(frames, frame)
			if frame.ConversationState != nil && !frame.ConversationState.Working {
				goto complete
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for completed dago turn")
		}
	}

complete:
	if len(frames) != 2 {
		t.Fatalf("event count = %d, want 2", len(frames))
	}
	if frames[0].ConversationID != created.ConversationID || len(frames[0].Messages) != 1 || frames[0].Messages[0].Type != "user" {
		t.Fatalf("first frame = %#v", frames[0])
	}
	if len(frames[1].Messages) != 1 || frames[1].Messages[0].Type != "agent" {
		t.Fatalf("second frame = %#v", frames[1])
	}
	var projected struct {
		Content []struct {
			Type int
			Text string
		}
	}
	if err := json.Unmarshal([]byte(*frames[1].Messages[0].LLMData), &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Content) != 1 || projected.Content[0].Type != int(2) || projected.Content[0].Text != "Well, hi there!" {
		t.Fatalf("assistant projection = %#v", projected)
	}
}

func TestBrowserSnapshotRestoresConversationsAndWorkspace(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.workspace.Write(context.Background(), "/workspace/notes.txt", "durable"); err != nil {
		t.Fatal(err)
	}
	conversation, err := app.createRecord(modelID, workspaceRoot, nil, true, "remember this")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	restored, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(saved); err != nil {
		t.Fatal(err)
	}
	response := restored.Handle(Request{Method: "GET", URL: "/api/conversations/snapshot"})
	var listed struct {
		Conversations []conversationWithState `json:"conversations"`
	}
	if err := json.Unmarshal(response.Body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Conversations) != 1 || listed.Conversations[0].ConversationID != conversation.ConversationID || listed.Conversations[0].Draft != "remember this" {
		t.Fatalf("restored conversations = %#v", listed.Conversations)
	}
	read, err := restored.workspace.Read(context.Background(), "/workspace/notes.txt", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if read.Data == nil || read.Data.Content != "durable" {
		t.Fatalf("restored file = %#v", read)
	}
}

func TestBrowserRestoreKeepsLaterAssistantMessagesDistinct(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan json.RawMessage, 8)
	app.SetEventSink(func(event json.RawMessage) { events <- append(json.RawMessage(nil), event...) })
	response := app.Handle(Request{Method: "POST", URL: "/api/conversations/new", Body: `{"message":"hello"}`})
	var created struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(response.Body, &created); err != nil {
		t.Fatal(err)
	}
	if !app.Continue(response.ContinueConversation) {
		t.Fatal("prepared turn did not continue")
	}
	waitForCompletedFrame(t, events)
	saved, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	restored, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(saved); err != nil {
		t.Fatal(err)
	}
	restoredEvents := make(chan json.RawMessage, 8)
	restored.SetEventSink(func(event json.RawMessage) {
		restoredEvents <- append(json.RawMessage(nil), event...)
	})
	response = restored.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + created.ConversationID + "/chat",
		Body: `{"message":"echo: after reload"}`,
	})
	if response.Status != 202 {
		t.Fatalf("second turn status = %d, body = %s", response.Status, response.Body)
	}
	if !restored.Continue(response.ContinueConversation) {
		t.Fatal("restored turn did not continue")
	}
	waitForCompletedFrame(t, restoredEvents)

	response = restored.Handle(Request{Method: "GET", URL: "/api/conversation/" + created.ConversationID})
	var conversation struct {
		Messages []apiMessage `json:"messages"`
	}
	if err := json.Unmarshal(response.Body, &conversation); err != nil {
		t.Fatal(err)
	}
	if len(conversation.Messages) != 4 {
		t.Fatalf("message count after reload = %d, want 4: %#v", len(conversation.Messages), conversation.Messages)
	}
	if *conversation.Messages[1].LLMData == *conversation.Messages[3].LLMData || conversation.Messages[1].MessageID == conversation.Messages[3].MessageID {
		t.Fatalf("assistant messages were replaced across reload: %#v", conversation.Messages)
	}
	var final struct {
		Content []struct{ Text string }
	}
	if err := json.Unmarshal([]byte(*conversation.Messages[3].LLMData), &final); err != nil {
		t.Fatal(err)
	}
	if len(final.Content) != 1 || final.Content[0].Text != "after reload" {
		t.Fatalf("final assistant = %#v", final)
	}
}

func TestBrowserAppRestoresGraphCheckpointWithoutReplayingHistory(t *testing.T) {
	saver := dacheckpoint.NewMemorySaver()
	app, err := newApp(nil, saver)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan json.RawMessage, 8)
	app.SetEventSink(func(event json.RawMessage) { events <- append(json.RawMessage(nil), event...) })
	response := app.Handle(Request{Method: "POST", URL: "/api/conversations/new", Body: `{"message":"hello"}`})
	var created struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(response.Body, &created); err != nil {
		t.Fatal(err)
	}
	if !app.Continue(response.ContinueConversation) {
		t.Fatal("first turn did not continue")
	}
	waitForCompletedFrame(t, events)
	if tuple, err := saver.GetTuple(context.Background(), dacheckpoint.Config{ThreadID: created.ConversationID}); err != nil || tuple == nil {
		t.Fatalf("checkpoint after first turn = %#v, %v", tuple, err)
	}
	snapshot, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	restored, err := newApp(nil, saver)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	restoredEvents := make(chan json.RawMessage, 8)
	restored.SetEventSink(func(event json.RawMessage) {
		restoredEvents <- append(json.RawMessage(nil), event...)
	})
	response = restored.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + created.ConversationID + "/chat",
		Body: `{"message":"echo: after checkpoint"}`,
	})
	if !restored.Continue(response.ContinueConversation) {
		t.Fatal("restored turn did not continue")
	}
	waitForCompletedFrame(t, restoredEvents)

	response = restored.Handle(Request{Method: "GET", URL: "/api/conversation/" + created.ConversationID})
	var conversation struct {
		Messages []apiMessage `json:"messages"`
	}
	if err := json.Unmarshal(response.Body, &conversation); err != nil {
		t.Fatal(err)
	}
	if len(conversation.Messages) != 4 {
		t.Fatalf("messages after checkpoint restore = %d, want 4", len(conversation.Messages))
	}
}

func TestBrowserCapabilitiesDoNotPretendHostFeaturesExist(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	response := app.Handle(Request{Method: "GET", URL: "/api/capabilities"})
	if response.Status != 200 {
		t.Fatalf("capability status = %d", response.Status)
	}
	var capabilities struct {
		Runtime     string   `json:"runtime"`
		Unavailable []string `json:"unavailable"`
	}
	if err := json.Unmarshal(response.Body, &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Runtime != "wasm" || !contains(capabilities.Unavailable, "shell") || !contains(capabilities.Unavailable, "host_filesystem") {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	unsupported := app.Handle(Request{Method: "GET", URL: "/api/git/diffs"})
	if unsupported.Status != 501 || unsupported.Headers["X-Shelley-Capability"] != "unavailable" {
		t.Fatalf("unsupported response = %#v", unsupported)
	}
}

func TestBrowserCustomModelUsesDirectResponsesAPIWithoutPersistingKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer browser-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"id":"resp_browser","status":"completed","output":[{"type":"message","id":"msg_browser","role":"assistant","content":[{"type":"output_text","text":"test successful"}]}],"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}`)
	}))
	defer upstream.Close()

	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	modelJSON, err := json.Marshal(CustomModel{
		DisplayName: "Browser Luna", ProviderType: "openai-responses", Endpoint: upstream.URL,
		APIKey: "browser-secret", ModelName: "gpt-5.6-luna", MaxTokens: 200000,
		ReasoningSupport: "auto", ImageSupport: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	created := app.Handle(Request{Method: "POST", URL: "/api/custom-models", Body: string(modelJSON)})
	if created.Status != http.StatusCreated {
		t.Fatalf("create model status = %d, body = %s", created.Status, created.Body)
	}
	var model CustomModel
	if err := json.Unmarshal(created.Body, &model); err != nil {
		t.Fatal(err)
	}
	if model.ModelID == "" || !model.SupportsReasoning || !model.SupportsImages {
		t.Fatalf("created model = %#v", model)
	}
	if model.APIKey != "" || strings.Contains(string(created.Body), "browser-secret") {
		t.Fatal("create response exposed browser API key")
	}

	testBody, err := json.Marshal(CustomModel{
		ModelID: model.ModelID, ProviderType: model.ProviderType, Endpoint: model.Endpoint,
		ModelName: model.ModelName, ReasoningSupport: model.ReasoningSupport,
	})
	if err != nil {
		t.Fatal(err)
	}
	tested := app.Handle(Request{Method: "POST", URL: "/api/custom-models-test", Body: string(testBody)})
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(tested.Body, &result); err != nil {
		t.Fatal(err)
	}
	if tested.Status != http.StatusOK || !result.Success {
		t.Fatalf("test model response = %#v body=%s", tested, tested.Body)
	}

	snapshot, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot), "browser-secret") {
		t.Fatal("browser API key leaked into durable application snapshot")
	}
	listed := app.Handle(Request{Method: "GET", URL: "/api/custom-models"})
	if strings.Contains(string(listed.Body), "browser-secret") {
		t.Fatal("custom model listing exposed browser API key")
	}
	catalog := app.Handle(Request{Method: "GET", URL: "/api/models"})
	if !strings.Contains(string(catalog.Body), model.ModelID) {
		t.Fatalf("model catalog = %s", catalog.Body)
	}
}

func TestBrowserOpenAIKeyConfiguresStandardModelCatalogWithoutPersistingKey(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	response := app.Handle(Request{
		Method: "POST", URL: "/api/browser-openai-key", Body: `{"api_key":"one-time-browser-key"}`,
	})
	if response.Status != http.StatusOK {
		t.Fatalf("configure status = %d, body = %s", response.Status, response.Body)
	}
	catalog := app.Handle(Request{Method: "GET", URL: "/api/models"})
	for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-terra", "gpt-5.6-sol"} {
		if !strings.Contains(string(catalog.Body), model) {
			t.Fatalf("catalog omitted %s: %s", model, catalog.Body)
		}
	}
	if app.defaultModelID() != "gpt-5.6-luna" {
		t.Fatalf("default model = %q", app.defaultModelID())
	}
	snapshot, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot), "one-time-browser-key") {
		t.Fatal("OpenAI key leaked into durable application snapshot")
	}
}

func TestConfigureWebGPUModelAddsLocalDefaultWithoutPersistingModelState(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureWebGPUModel(newPredictableModel()); err != nil {
		t.Fatal(err)
	}
	catalog := app.Handle(Request{Method: "GET", URL: "/api/models"})
	if !strings.Contains(string(catalog.Body), webGPUModelID) || !strings.Contains(string(catalog.Body), webGPUModelName) {
		t.Fatalf("catalog omitted WebGPU model: %s", catalog.Body)
	}
	if app.defaultModelID() != webGPUModelID {
		t.Fatalf("default model = %q", app.defaultModelID())
	}
	snapshot, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot), webGPUModelID) {
		t.Fatalf("WebGPU runtime state leaked into snapshot: %s", snapshot)
	}
}

func TestReplaceWorkspaceRefreshesFilesWithoutReplacingAgentState(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.ReplaceWorkspace(map[string]dabackend.FileData{
		workspaceRoot + "/main.go": {Content: "package main\n", Encoding: dabackend.EncodingUTF8},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.workspace.Read(context.Background(), workspaceRoot+"/README.md", 0, 10); err == nil {
		t.Fatal("replaced workspace retained the previous README")
	}
	read, err := app.workspace.Read(context.Background(), workspaceRoot+"/main.go", 0, 10)
	if err != nil || read.Data == nil || read.Data.Content != "package main\n" {
		t.Fatalf("replacement file = %#v, %v", read, err)
	}
	if app.agents[modelID] == nil {
		t.Fatal("workspace replacement removed the configured agent")
	}
	if err := app.ReplaceWorkspace(map[string]dabackend.FileData{
		"/outside.txt": {Content: "no", Encoding: dabackend.EncodingUTF8},
	}); err == nil {
		t.Fatal("workspace replacement accepted a path outside /workspace")
	}
}

func TestBrowserShellExecutesAgainstAndUpdatesDurableWorkspace(t *testing.T) {
	app, err := NewWithShell(func(_ context.Context, request ShellRequest) (ShellResponse, error) {
		if request.Cwd != workspaceRoot || request.Files[workspaceRoot+"/README.md"].Content == "" {
			return ShellResponse{}, fmt.Errorf("shell did not receive browser workspace: %#v", request)
		}
		files := make(map[string]dabackend.FileData, len(request.Files)+1)
		for path, file := range request.Files {
			files[path] = file
		}
		files[workspaceRoot+"/shell.txt"] = dabackend.FileData{Content: "from just-bash\n", Encoding: dabackend.EncodingUTF8}
		return ShellResponse{Stdout: "from just-bash\n", ExitCode: 0, Files: files}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := app.Handle(Request{Method: "GET", URL: "/api/capabilities"})
	if !strings.Contains(string(capabilities.Body), `"shell"`) {
		t.Fatalf("browser capabilities omitted shell: %s", capabilities.Body)
	}
	tools := app.Handle(Request{Method: "GET", URL: "/api/tools"})
	if !strings.Contains(string(tools.Body), `"execute"`) {
		t.Fatalf("browser tools omitted execute: %s", tools.Body)
	}
	response := app.Handle(Request{Method: "POST", URL: "/api/browser-shell", Body: `{"command":"printf test"}`})
	if response.Status != http.StatusOK || !response.Changed || !strings.Contains(string(response.Body), "from just-bash") {
		t.Fatalf("browser shell response = %#v", response)
	}
	read, err := app.workspace.Read(context.Background(), workspaceRoot+"/shell.txt", 0, 10)
	if err != nil || read.Data == nil || read.Data.Content != "from just-bash\n" {
		t.Fatalf("shell workspace file = %#v, %v", read, err)
	}
	saved, err := app.Snapshot()
	if err != nil || !strings.Contains(string(saved), "from just-bash") {
		t.Fatalf("shell file was not durable: %s, %v", saved, err)
	}
}

func TestBrowserShellDoesNotBlockSnapshotsAndPreservesConcurrentFileChanges(t *testing.T) {
	started := make(chan ShellRequest, 1)
	release := make(chan struct{})
	app, err := NewWithShell(func(_ context.Context, request ShellRequest) (ShellResponse, error) {
		started <- request
		<-release
		files := make(map[string]dabackend.FileData, len(request.Files)+1)
		for path, file := range request.Files {
			files[path] = file
		}
		files[workspaceRoot+"/shell.txt"] = dabackend.FileData{Content: "shell", Encoding: dabackend.EncodingUTF8}
		return ShellResponse{Files: files}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := make(chan Response, 1)
	go func() {
		execution <- app.Handle(Request{Method: "POST", URL: "/api/browser-shell", Body: `{"command":"touch shell.txt"}`})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shell execution did not start")
	}
	snapshot := make(chan error, 1)
	go func() {
		_, snapshotErr := app.Snapshot()
		snapshot <- snapshotErr
	}()
	select {
	case snapshotErr := <-snapshot:
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot blocked while browser shell was running")
	}
	if _, err := app.workspace.Write(context.Background(), workspaceRoot+"/concurrent.txt", "file tool"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case response := <-execution:
		if response.Status != http.StatusOK {
			t.Fatalf("browser shell response = %#v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("shell execution did not finish")
	}
	for path, expected := range map[string]string{
		workspaceRoot + "/shell.txt":      "shell",
		workspaceRoot + "/concurrent.txt": "file tool",
	} {
		read, err := app.workspace.Read(context.Background(), path, 0, 10)
		if err != nil || read.Data == nil || read.Data.Content != expected {
			t.Fatalf("workspace file %s = %#v, %v", path, read, err)
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func waitForCompletedFrame(t *testing.T, events <-chan json.RawMessage) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case raw := <-events:
			var frame struct {
				ConversationState *struct {
					Working bool `json:"working"`
				} `json:"conversation_state"`
			}
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.ConversationState != nil && !frame.ConversationState.Working {
				return
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for completed turn")
		}
	}
}
