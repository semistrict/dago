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
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestBrowserConstructorsPanicForTypedNilDependencies(t *testing.T) {
	var workspace *browserWorkspace
	var saver *dacheckpoint.MemorySaver
	executor := ShellExecutor(func(context.Context, ShellRequest) (ShellResponse, error) { return ShellResponse{}, nil })
	for name, construct := range map[string]func(){
		"workspace": func() { NewWithWorkspaceAndSaver(workspace, executor, dacheckpoint.NewMemorySaver()) },
		"saver":     func() { NewWithShellAndSaver(executor, saver) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("constructor did not panic")
				}
			}()
			construct()
		})
	}
}

func TestConfigureWebGPUModelPanicsForTypedNil(t *testing.T) {
	var model *modeltest.Predictable
	defer func() {
		if recover() == nil {
			t.Fatal("typed-nil WebGPU model was accepted")
		}
	}()
	New().ConfigureWebGPUModel(model)
}

func TestBrowserAppRunsdagoTurnAndPublishesOrderedMessages(t *testing.T) {
	app := New()
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
			if frame.ConversationState != nil {
				frames = append(frames, frame)
			}
			if frame.ConversationState != nil && !frame.ConversationState.Working {
				goto complete
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for completed dago turn")
		}
	}

complete:
	if len(frames) != 3 {
		t.Fatalf("stateful event count = %d, want 3", len(frames))
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

func TestBrowserAppStreamsToolUseResultAndFinalAnswer(t *testing.T) {
	app := newApp(nil, dacheckpoint.NewMemorySaver())
	events := make(chan json.RawMessage, 16)
	app.SetEventSink(func(event json.RawMessage) { events <- append(json.RawMessage(nil), event...) })
	response := app.Handle(Request{
		Method: "POST", URL: "/api/conversations/new",
		Body: `{"message":"tool: write_file {\"file_path\":\"/workspace/tool.txt\",\"content\":\"visible\"}"}`,
	})
	if !app.Continue(response.ContinueConversation) {
		t.Fatal("prepared tool turn did not continue")
	}
	waitForCompletedFrame(t, events)

	var loaded struct {
		Messages []apiMessage `json:"messages"`
	}
	conversationID := response.ContinueConversation
	got := app.Handle(Request{Method: "GET", URL: "/api/conversation/" + conversationID})
	if err := json.Unmarshal(got.Body, &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 4 {
		t.Fatalf("tool turn message count = %d, want 4: %#v", len(loaded.Messages), loaded.Messages)
	}
	wantTypes := []string{"user", "agent", "tool", "agent"}
	for index, want := range wantTypes {
		if loaded.Messages[index].Type != want {
			t.Fatalf("message %d type = %q, want %q", index, loaded.Messages[index].Type, want)
		}
	}
	if !strings.Contains(*loaded.Messages[1].LLMData, `"ToolName":"write_file"`) {
		t.Fatalf("tool-use projection = %s", *loaded.Messages[1].LLMData)
	}
	if !strings.Contains(*loaded.Messages[2].LLMData, `"ToolUseID"`) {
		t.Fatalf("tool-result projection = %s", *loaded.Messages[2].LLMData)
	}
}

func TestBrowserConversationLifecycleWorksLocally(t *testing.T) {
	app := newApp(nil, dacheckpoint.NewMemorySaver())
	events := make(chan json.RawMessage, 64)
	app.SetEventSink(func(event json.RawMessage) { events <- append(json.RawMessage(nil), event...) })
	created := app.Handle(Request{Method: "POST", URL: "/api/conversations/new", Body: `{"message":"hello"}`})
	if created.Status != http.StatusAccepted || !app.Continue(created.ContinueConversation) {
		t.Fatalf("create response = %#v", created)
	}
	waitForCompletedFrame(t, events)

	forkedResponse := app.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + created.ContinueConversation + "/fork",
		Body: `{"sequence_id":2}`,
	})
	if forkedResponse.Status != http.StatusCreated {
		t.Fatalf("fork response = %#v", forkedResponse)
	}
	var forked Conversation
	if err := json.Unmarshal(forkedResponse.Body, &forked); err != nil {
		t.Fatal(err)
	}
	if forked.ConversationID == created.ContinueConversation || forked.ParentConversationID != nil {
		t.Fatalf("forked conversation = %#v", forked)
	}
	forkedTurn := app.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + forked.ConversationID + "/chat",
		Body: `{"message":"echo: fork continued"}`,
	})
	if !app.Continue(forkedTurn.ContinueConversation) {
		t.Fatal("forked turn did not continue")
	}
	waitForCompletedFrame(t, events)

	newGeneration := app.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + created.ContinueConversation + "/new-generation",
	})
	if newGeneration.Status != http.StatusOK {
		t.Fatalf("new generation response = %#v", newGeneration)
	}
	var generation Conversation
	if err := json.Unmarshal(newGeneration.Body, &generation); err != nil {
		t.Fatal(err)
	}
	if generation.CurrentGeneration != 2 {
		t.Fatalf("current generation = %d, want 2", generation.CurrentGeneration)
	}
	waitForCompletedFrame(t, events)
	secondGeneration := app.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + created.ContinueConversation + "/chat",
		Body: `{"message":"echo: clean context"}`,
	})
	if !app.Continue(secondGeneration.ContinueConversation) {
		t.Fatal("second generation did not continue")
	}
	waitForCompletedFrame(t, events)

	loaded := app.Handle(Request{Method: "GET", URL: "/api/conversation/" + created.ContinueConversation})
	var history struct {
		Messages []apiMessage `json:"messages"`
	}
	if err := json.Unmarshal(loaded.Body, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 4 || history.Messages[2].Generation != 2 || history.Messages[3].Generation != 2 {
		t.Fatalf("generation history = %#v", history.Messages)
	}
}

func TestBrowserDraftSearchQueueAndRetryWorkLocally(t *testing.T) {
	app := New()
	draftResponse := app.Handle(Request{
		Method: "POST", URL: "/api/conversations/draft",
		Body: `{"draft":"initial words","model":"browser-predictable","cwd":"/workspace"}`,
	})
	var draft Conversation
	if err := json.Unmarshal(draftResponse.Body, &draft); err != nil {
		t.Fatal(err)
	}
	updated := app.Handle(Request{
		Method: "PUT", URL: "/api/conversation/" + draft.ConversationID + "/draft",
		Body: `{"draft":"durable needle"}`,
	})
	if updated.Status != http.StatusOK {
		t.Fatalf("draft update = %#v", updated)
	}
	search := app.Handle(Request{Method: "GET", URL: "/api/conversations/search?q=needle"})
	var results []conversationWithState
	if err := json.Unmarshal(search.Body, &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ConversationID != draft.ConversationID {
		t.Fatalf("search results = %#v", results)
	}

	events := make(chan json.RawMessage, 64)
	app.SetEventSink(func(event json.RawMessage) { events <- append(json.RawMessage(nil), event...) })
	created := app.Handle(Request{
		Method: "POST", URL: "/api/conversations/new", Body: `{"message":"delay: 100ms"}`,
	})
	if !app.Continue(created.ContinueConversation) {
		t.Fatal("delayed turn did not continue")
	}
	queued := app.Handle(Request{
		Method: "POST", URL: "/api/conversation/" + created.ContinueConversation + "/chat",
		Body: `{"message":"echo: queued turn","queue":true}`,
	})
	if queued.Status != http.StatusAccepted || !strings.Contains(string(queued.Body), "queued") {
		t.Fatalf("queue response = %#v", queued)
	}
	waitForCompletedFrame(t, events)
	waitForCompletedFrame(t, events)
	loaded := app.Handle(Request{Method: "GET", URL: "/api/conversation/" + created.ContinueConversation})
	if !strings.Contains(string(loaded.Body), "queued turn") {
		t.Fatalf("queued turn history = %s", loaded.Body)
	}

	failed := app.Handle(Request{Method: "POST", URL: "/api/conversations/new", Body: `{"message":"error: retry me"}`})
	if !app.Continue(failed.ContinueConversation) {
		t.Fatal("failing turn did not continue")
	}
	waitForCompletedFrame(t, events)
	retry := app.Handle(Request{Method: "POST", URL: "/api/conversation/" + failed.ContinueConversation + "/retry"})
	if retry.Status != http.StatusAccepted || !app.Continue(retry.ContinueConversation) {
		t.Fatalf("retry response = %#v", retry)
	}
	waitForCompletedFrame(t, events)

	cancelled := app.Handle(Request{Method: "POST", URL: "/api/conversations/new", Body: `{"message":"delay: 1s"}`})
	if !app.Continue(cancelled.ContinueConversation) {
		t.Fatal("cancellable turn did not continue")
	}
	app.Handle(Request{Method: "POST", URL: "/api/conversation/" + cancelled.ContinueConversation + "/cancel"})
	waitForCompletedFrame(t, events)
	cancelledHistory := app.Handle(Request{Method: "GET", URL: "/api/conversation/" + cancelled.ContinueConversation})
	if strings.Contains(string(cancelledHistory.Body), "Agent error:") {
		t.Fatalf("cancelled turn was projected as an error: %s", cancelledHistory.Body)
	}
}

func TestBrowserFileAPIsPersistFilesAndEmptyDirectories(t *testing.T) {
	app := New()
	created := app.Handle(Request{Method: "POST", URL: "/api/create-directory", Body: `{"path":"/workspace/empty"}`})
	if created.Status != http.StatusCreated {
		t.Fatalf("create directory = %#v", created)
	}
	written := app.Handle(Request{
		Method: "POST", URL: "/api/write-file", Body: `{"path":"/workspace/notes/plan.txt","content":"browser plan"}`,
	})
	if written.Status != http.StatusOK {
		t.Fatalf("write file = %#v", written)
	}
	read := app.Handle(Request{Method: "GET", URL: "/api/read-file?path=%2Fworkspace%2Fnotes%2Fplan.txt"})
	if read.Status != http.StatusOK || !strings.Contains(string(read.Body), "browser plan") {
		t.Fatalf("read file = %#v", read)
	}
	snapshot, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := restored.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	listed := restored.Handle(Request{Method: "GET", URL: "/api/list-directory?path=%2Fworkspace"})
	if !strings.Contains(string(listed.Body), `"name":"empty"`) || !strings.Contains(string(listed.Body), `"name":"notes"`) {
		t.Fatalf("restored directory listing = %s", listed.Body)
	}
}

func TestBrowserSnapshotRestoresConversationsAndWorkspace(t *testing.T) {
	app := New()
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

	restored := New()
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
	app := New()
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

	restored := New()
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
	app := newApp(nil, saver)
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

	restored := newApp(nil, saver)
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
	app := New()
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
	if capabilities.Runtime != "wasm" || !contains(capabilities.Unavailable, "host_processes") || !contains(capabilities.Unavailable, "unrestricted_host_filesystem") {
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

	app := New()
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

func TestBrowserCustomModelRejectsUnknownEnumsAndNegativeLimits(t *testing.T) {
	base := CustomModel{
		ProviderType: "openai-responses", Endpoint: "https://example.test",
		APIKey: "key", ModelName: "model", MaxTokens: 1,
		ReasoningSupport: "auto", ImageSupport: "auto",
	}
	for name, mutate := range map[string]func(*CustomModel){
		"reasoning support": func(model *CustomModel) { model.ReasoningSupport = "maybe" },
		"image support":     func(model *CustomModel) { model.ImageSupport = "maybe" },
		"reasoning effort":  func(model *CustomModel) { model.ReasoningEffort = "turbo" },
		"negative tokens":   func(model *CustomModel) { model.MaxTokens = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			model := base
			mutate(&model)
			if _, err := customModelChat(model); err == nil {
				t.Fatal("customModelChat accepted invalid configuration")
			}
		})
	}
}

func TestBrowserOpenRouterCustomModelUsesSelectedProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer browser-router-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "deepseek/deepseek-v4-flash-0731" {
			t.Errorf("model = %#v", body["model"])
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{"id":"resp_browser","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"test successful"}]}]}`)
	}))
	defer upstream.Close()

	app := New()
	modelJSON, err := json.Marshal(CustomModel{
		DisplayName: "Browser DeepSeek", ProviderType: "openrouter-responses", Endpoint: upstream.URL,
		APIKey: "browser-router-secret", ModelName: "deepseek/deepseek-v4-flash-0731",
		MaxTokens: 1_000_000, ReasoningSupport: "yes", ImageSupport: "no",
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
	testBody, err := json.Marshal(CustomModel{
		ModelID: model.ModelID, ProviderType: model.ProviderType, Endpoint: model.Endpoint,
		ModelName: model.ModelName, ReasoningSupport: model.ReasoningSupport,
	})
	if err != nil {
		t.Fatal(err)
	}
	tested := app.Handle(Request{Method: "POST", URL: "/api/custom-models-test", Body: string(testBody)})
	if tested.Status != http.StatusOK || !strings.Contains(string(tested.Body), `"success":true`) {
		t.Fatalf("test model response = %#v body=%s", tested, tested.Body)
	}
}

func TestBrowserOpenAIKeyConfiguresStandardModelCatalogWithoutPersistingKey(t *testing.T) {
	app := New()
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
	app := New()
	app.ConfigureWebGPUModel(newPredictableModel())
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

func TestBrowserShellExecutesAgainstAndUpdatesDurableWorkspace(t *testing.T) {
	workspace, err := newBrowserWorkspace(map[string]dabackend.FileData{
		workspaceRoot + "/README.md": {Content: "browser workspace", Encoding: dabackend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := newAppWithWorkspace(workspace, func(ctx context.Context, request ShellRequest) (ShellResponse, error) {
		if request.Cwd != workspaceRoot {
			return ShellResponse{}, fmt.Errorf("shell cwd = %q", request.Cwd)
		}
		if _, err := workspace.Write(ctx, workspaceRoot+"/shell.txt", "from just-bash\n"); err != nil {
			return ShellResponse{}, err
		}
		return ShellResponse{Stdout: "from just-bash\n", ExitCode: 0}, nil
	}, nil)
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

type independentlyPersistedWorkspace struct {
	*browserWorkspace
}

func TestBrowserOwnedWorkspaceFilesStayOutOfApplicationSnapshot(t *testing.T) {
	memory, err := newBrowserWorkspace(map[string]dabackend.FileData{
		workspaceRoot + "/project.txt": {Content: "selected directory contents", Encoding: dabackend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := &independentlyPersistedWorkspace{browserWorkspace: memory}
	app := newAppWithWorkspace(workspace, func(ctx context.Context, request ShellRequest) (ShellResponse, error) {
		if request.Cwd != workspaceRoot {
			return ShellResponse{}, fmt.Errorf("shell cwd = %q", request.Cwd)
		}
		if _, err := workspace.Write(ctx, workspaceRoot+"/shell.txt", "shared"); err != nil {
			return ShellResponse{}, err
		}
		return ShellResponse{}, nil
	}, nil)
	response := app.Handle(Request{Method: "POST", URL: "/api/browser-shell", Body: `{"command":"touch shell.txt"}`})
	if response.Status != http.StatusOK {
		t.Fatalf("browser shell response = %#v", response)
	}
	saved, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "selected directory contents") || strings.Contains(string(saved), "shared") || strings.Contains(string(saved), `"files"`) {
		t.Fatalf("independently persisted workspace leaked into application snapshot: %s", saved)
	}
}

func TestBrowserShellDoesNotBlockSnapshotsAndPreservesConcurrentFileChanges(t *testing.T) {
	started := make(chan ShellRequest, 1)
	release := make(chan struct{})
	workspace, err := newBrowserWorkspace(nil)
	if err != nil {
		t.Fatal(err)
	}
	app := newAppWithWorkspace(workspace, func(ctx context.Context, request ShellRequest) (ShellResponse, error) {
		started <- request
		<-release
		if _, err := workspace.Write(ctx, workspaceRoot+"/shell.txt", "shell"); err != nil {
			return ShellResponse{}, err
		}
		return ShellResponse{}, nil
	}, nil)
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
