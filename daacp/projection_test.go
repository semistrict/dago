package daacp

import (
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func TestToolKindProjectsT3RuntimeCategories(t *testing.T) {
	tests := []struct {
		name string
		want acp.ToolKind
	}{
		{"delete_file", acp.ToolKindDelete},
		{"remove_path", acp.ToolKindDelete},
		{"move_file", acp.ToolKindMove},
		{"rename_symbol", acp.ToolKindMove},
		{"write_file", acp.ToolKindEdit},
		{"apply_patch", acp.ToolKindEdit},
		{"read_file", acp.ToolKindRead},
		{"list_directory", acp.ToolKindRead},
		{"grep_search", acp.ToolKindSearch},
		{"find_files", acp.ToolKindSearch},
		{"web_fetch", acp.ToolKindFetch},
		{"http_request", acp.ToolKindFetch},
		{"execute_command", acp.ToolKindExecute},
		{"shell_terminal", acp.ToolKindExecute},
		{"write_todos", acp.ToolKindThink},
		{"think", acp.ToolKindThink},
		{"custom_extension", acp.ToolKindOther},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := toolKind(test.name); got != test.want {
				t.Fatalf("toolKind(%q) = %q, want %q", test.name, got, test.want)
			}
		})
	}
}

func TestToolLocationsUsesStablePathPrecedence(t *testing.T) {
	arguments := map[string]any{
		"path": "/first", "file_path": "/second", "file": "/third", "uri": "file:///fourth",
	}
	if got := toolLocations(arguments); !reflect.DeepEqual(got, []acp.ToolCallLocation{{Path: "/first"}}) {
		t.Fatalf("locations = %#v", got)
	}
	for _, arguments := range []any{nil, "not-an-object", map[string]any{"path": "", "other": "/ignored"}} {
		if got := toolLocations(arguments); len(got) != 0 {
			t.Fatalf("toolLocations(%#v) = %#v", arguments, got)
		}
	}
}

func TestProjectionMessagesAcceptsLiveAndRestoredForms(t *testing.T) {
	messages := []damessage.Message{
		damessage.Human("question"),
		{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/a"}`)}}},
		damessage.Tool("call-1", "answer"),
	}
	tests := []struct {
		name  string
		value any
		want  []damessage.Message
	}{
		{"typed", messages, messages},
		{"plain slice", []any{messages[0], messages[1]}, messages[:2]},
		{"overwrite starts at latest tool request", dastate.Overwrite{Value: messages}, messages[1:]},
		{"nil", nil, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := projectionMessages(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("messages = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestProjectionMessagesRejectsMalformedRuntimeUpdates(t *testing.T) {
	for _, value := range []any{"messages", []any{damessage.Human("ok"), 12}, dastate.Overwrite{Value: []any{"bad"}}} {
		if _, err := projectionMessages(value); err == nil {
			t.Errorf("projectionMessages(%#v) accepted malformed input", value)
		}
	}
}

func TestApprovalOptionsMapsOnlyRepresentableDecisions(t *testing.T) {
	options := approvalOptions([]dagent.ApprovalDecision{
		dagent.ApprovalApprove, "unsupported", dagent.ApprovalReject, dagent.ApprovalApprove,
	})
	if len(options) != 3 {
		t.Fatalf("options = %#v", options)
	}
	if options[0].Kind != acp.PermissionOptionKindAllowOnce || options[0].OptionId != "approve" {
		t.Fatalf("allow option = %#v", options[0])
	}
	if options[1].Kind != acp.PermissionOptionKindRejectOnce || options[1].OptionId != "reject" {
		t.Fatalf("reject option = %#v", options[1])
	}
	if options[2].Kind != acp.PermissionOptionKindAllowOnce {
		t.Fatalf("second allow option = %#v", options[2])
	}
	if got := approvalOptions([]dagent.ApprovalDecision{"always", "never"}); len(got) != 0 {
		t.Fatalf("unrepresentable options = %#v", got)
	}
}

func TestProjectorEmitsT3PlanContentAndToolUpdates(t *testing.T) {
	client := &testClient{}
	projector := startProjectorTestConnection(t, client)
	ctx := t.Context()

	err := projector.event(ctx, dagent.Event{Mode: dagent.EventToken, Chunk: &damodel.Chunk{MessageDelta: damessage.Message{
		Role: damessage.RoleAssistant,
		Content: []damessage.ContentBlock{
			{Type: damessage.BlockText, Text: "hello"},
			{Type: damessage.BlockReasoning, Reasoning: "thinking"},
			{Type: damessage.BlockImage, Data: []byte("ignored")},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	err = projector.event(ctx, dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{
		"todos": []dagent.Todo{
			{Content: "Inspect state", Status: "completed"},
			{Content: "Run tests", Status: "in_progress"},
			{Content: "Ship", Status: "pending"},
		},
		dagent.MessagesKey: []damessage.Message{{
			Role:      damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{ID: "tool-1", Name: "execute_command", Arguments: json.RawMessage(`{"path":"/workspace","command":"go test ./..."}`)}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	err = projector.event(ctx, dagent.Event{Mode: dagent.EventToolProgress, ToolProgress: &datool.Progress{
		CallID: "tool-1", Name: "execute_command", Output: "running",
	}})
	if err != nil {
		t.Fatal(err)
	}
	failed := damessage.Tool("tool-1", "exit status 1")
	failed.ToolStatus = damessage.ToolStatusError
	if err := projector.event(ctx, dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{dagent.MessagesKey: []damessage.Message{failed}}}); err != nil {
		t.Fatal(err)
	}

	updates := waitForUpdates(t, client, 6)
	if chunk := updates[0].Update.AgentMessageChunk; chunk == nil || chunk.Content.Text == nil || chunk.Content.Text.Text != "hello" {
		t.Fatalf("text update = %#v", updates[0])
	}
	if thought := updates[1].Update.AgentThoughtChunk; thought == nil || thought.Content.Text == nil || thought.Content.Text.Text != "thinking" {
		t.Fatalf("thought update = %#v", updates[1])
	}
	started := updates[2].Update.ToolCall
	if started == nil || started.Kind != acp.ToolKindExecute || started.Status != acp.ToolCallStatusPending || len(started.Locations) != 1 || started.Locations[0].Path != "/workspace" {
		t.Fatalf("tool start = %#v", updates[2])
	}
	plan := updates[3].Update.Plan
	if plan == nil || len(plan.Entries) != 3 || plan.Entries[0].Status != acp.PlanEntryStatusCompleted || plan.Entries[1].Status != acp.PlanEntryStatusInProgress || plan.Entries[2].Status != acp.PlanEntryStatusPending {
		t.Fatalf("plan update = %#v", updates[3])
	}
	progress := updates[4].Update.ToolCallUpdate
	if progress == nil || progress.Status == nil || *progress.Status != acp.ToolCallStatusInProgress || len(progress.Content) != 1 {
		t.Fatalf("tool progress = %#v", updates[4])
	}
	completed := updates[5].Update.ToolCallUpdate
	if completed == nil || completed.Status == nil || *completed.Status != acp.ToolCallStatusFailed || completed.RawOutput != "exit status 1" {
		t.Fatalf("tool completion = %#v", updates[5])
	}
}

func TestProjectorStartsProgressOnlyToolOnce(t *testing.T) {
	client := &testClient{}
	projector := startProjectorTestConnection(t, client)
	for _, output := range []string{"one", "two"} {
		if err := projector.event(t.Context(), dagent.Event{Mode: dagent.EventToolProgress, ToolProgress: &datool.Progress{
			CallID: "progress-only", Name: "search", Output: output,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	updates := waitForUpdates(t, client, 3)
	if updates[0].Update.ToolCall == nil || updates[0].Update.ToolCall.Kind != acp.ToolKindSearch {
		t.Fatalf("start = %#v", updates[0])
	}
	for index := 1; index < 3; index++ {
		if updates[index].Update.ToolCallUpdate == nil {
			t.Fatalf("progress %d = %#v", index, updates[index])
		}
	}
}

func TestProjectorTreatsTransparentToolLifecycleAsOrdinaryCall(t *testing.T) {
	client := &testClient{}
	projector := startProjectorTestConnection(t, client)
	parent := damessage.Message{
		Role: damessage.RoleAssistant,
		ToolCalls: []damessage.ToolCall{{
			ID: "eval-call", Name: "js_eval", Arguments: json.RawMessage(`{"code":"await tools.read_file({file_path:'/guide.md'})"}`),
		}},
		Metadata: map[string]json.RawMessage{
			"dago.ptc_transparency.v1": json.RawMessage(`{"parent_call_ids":["eval-call"]}`),
		},
	}
	if err := projector.event(t.Context(), dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{
		dagent.MessagesKey: []damessage.Message{parent},
	}}); err != nil {
		t.Fatal(err)
	}
	start := datool.Progress{
		CallID: "ptc-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/guide.md"}`),
		ParentCallID: "eval-call",
	}
	if err := projector.event(t.Context(), dagent.Event{Mode: dagent.EventToolProgress, ToolProgress: &start}); err != nil {
		t.Fatal(err)
	}
	completed := datool.Progress{
		CallID: "ptc-read", Name: "read_file", ParentCallID: "eval-call",
		Output: "contents", Status: damessage.ToolStatusSuccess,
	}
	if err := projector.event(t.Context(), dagent.Event{Mode: dagent.EventToolProgress, ToolProgress: &completed}); err != nil {
		t.Fatal(err)
	}
	parentResult := damessage.Tool("eval-call", "contents")
	parentResult.Name = "js_eval"
	if err := projector.event(t.Context(), dagent.Event{Mode: dagent.EventUpdate, Update: dastate.Values{
		dagent.MessagesKey: []damessage.Message{parentResult},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := projector.event(t.Context(), dagent.Event{Mode: dagent.EventToken, Chunk: &damodel.Chunk{MessageDelta: damessage.Message{
		Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "barrier"}},
	}}}); err != nil {
		t.Fatal(err)
	}

	updates := waitForAgentText(t, client, "barrier")
	if len(updates) != 3 {
		t.Fatalf("updates = %#v, want only the underlying tool lifecycle", updates)
	}
	started := updates[0].Update.ToolCall
	if started == nil || started.ToolCallId != "ptc-read" || started.Title != "read_file" || started.Status != acp.ToolCallStatusPending || len(started.Locations) != 1 || started.Locations[0].Path != "/guide.md" {
		t.Fatalf("tool start = %#v", updates[0])
	}
	input, ok := started.RawInput.(map[string]any)
	if !ok || input["file_path"] != "/guide.md" {
		t.Fatalf("raw input = %#v", started.RawInput)
	}
	finished := updates[1].Update.ToolCallUpdate
	if finished == nil || finished.Status == nil || *finished.Status != acp.ToolCallStatusCompleted || finished.RawOutput != "contents" || len(finished.Content) != 1 {
		t.Fatalf("tool completion = %#v", updates[1])
	}
}

func TestProjectorFallsBackToFinalMessageWhenNoTokensStream(t *testing.T) {
	client := &testClient{}
	projector := startProjectorTestConnection(t, client)
	message := damessage.Message{Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{
		{Type: damessage.BlockText, Text: "final"}, {Type: damessage.BlockReasoning, Reasoning: "reason"},
	}}
	if err := projector.updateMessages(t.Context(), []damessage.Message{message}); err != nil {
		t.Fatal(err)
	}
	if err := projector.finalMessages(t.Context()); err != nil {
		t.Fatal(err)
	}
	updates := waitForUpdates(t, client, 2)
	if updates[0].Update.AgentMessageChunk == nil || updates[1].Update.AgentThoughtChunk == nil {
		t.Fatalf("updates = %#v", updates)
	}
}

func TestProjectorRejectsMalformedPlan(t *testing.T) {
	projector := startProjectorTestConnection(t, &testClient{})
	if err := projector.updatePlan(t.Context(), func() {}); err == nil {
		t.Fatal("unencodable plan was accepted")
	}
	if err := projector.updatePlan(t.Context(), map[string]any{"status": "pending"}); err == nil {
		t.Fatal("non-list plan was accepted")
	}
}

func startProjectorTestConnection(t *testing.T, client *testClient) *projector {
	t.Helper()
	clientToServerReader, clientToServerWriter := io.Pipe()
	serverToClientReader, serverToClientWriter := io.Pipe()
	agent := newProtocolAgent(t.Context(), nil, nil, Options{})
	serverConnection := acp.NewAgentSideConnection(agent, serverToClientWriter, clientToServerReader)
	clientConnection := acp.NewClientSideConnection(client, clientToServerWriter, serverToClientReader)
	agent.setConnection(serverConnection)
	t.Cleanup(func() {
		_ = clientToServerWriter.Close()
		_ = serverToClientWriter.Close()
		_ = clientToServerReader.Close()
		_ = serverToClientReader.Close()
		select {
		case <-serverConnection.Done():
		case <-time.After(time.Second):
		}
		select {
		case <-clientConnection.Done():
		case <-time.After(time.Second):
		}
	})
	return newProjector(serverConnection, "session-1")
}

func waitForUpdates(t *testing.T, client *testClient, count int) []acp.SessionNotification {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, updates := client.snapshot()
		if len(updates) >= count {
			return updates
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d updates, want at least %d: %#v", len(updates), count, updates)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForAgentText(t *testing.T, client *testClient, text string) []acp.SessionNotification {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, updates := client.snapshot()
		for _, update := range updates {
			chunk := update.Update.AgentMessageChunk
			if chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == text {
				return updates
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not receive agent text %q: %#v", text, updates)
		}
		time.Sleep(time.Millisecond)
	}
}
