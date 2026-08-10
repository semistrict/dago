package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/model/modeltest"
	"github.com/semistrict/dago/state"
	"github.com/semistrict/dago/tool"
)

func TestNemotronProgressBudgetStopsRepeatedCallLoop(t *testing.T) {
	middleware := NemotronProgressBudget(NemotronProgressBudgetOptions{MaxModelCalls: 99, MaxToolResults: 99, MaxRepeatedToolCalls: 3})
	messages := []message.Message{message.Human("Read it.")}
	for index := 0; index < 3; index++ {
		call := message.ToolCall{ID: string(rune('a' + index)), Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/x"}`)}
		assistant := message.Assistant("")
		assistant.ToolCalls = []message.ToolCall{call}
		messages = append(messages, assistant, message.Tool(call.ID, "page"))
	}
	called := false
	response, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{State: state.Values{agent.MessagesKey: messages}}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		called = true
		return agent.ModelResponse{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called || len(response.Messages) != 1 || response.Messages[0].Name != nemotronBudgetSource {
		t.Fatalf("called = %v, response = %#v", called, response)
	}
	if !strings.Contains(response.Messages[0].TextContent(), "- read_file: page") {
		t.Fatalf("fallback = %q", response.Messages[0].TextContent())
	}
}

func TestNemotronProgressBudgetAllowsNonconsecutiveCalls(t *testing.T) {
	middleware := NemotronProgressBudget(NemotronProgressBudgetOptions{MaxModelCalls: 99, MaxToolResults: 99, MaxRepeatedToolCalls: 3})
	messages := []message.Message{message.Human("Inspect.")}
	for index, name := range []string{"read_file", "grep", "read_file", "grep", "read_file"} {
		call := message.ToolCall{ID: string(rune('a' + index)), Name: name, Arguments: json.RawMessage(`{"path":"/x"}`)}
		assistant := message.Assistant("")
		assistant.ToolCalls = []message.ToolCall{call}
		messages = append(messages, assistant, message.Tool(call.ID, "ok"))
	}
	called := false
	_, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{Messages: messages}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		called = true
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("done")}}, nil
	})
	if err != nil || !called {
		t.Fatalf("called = %v, error = %v", called, err)
	}
}

func TestNemotronProgressBudgetCountsOnlyActiveTurn(t *testing.T) {
	middleware := NemotronProgressBudget(NemotronProgressBudgetOptions{MaxModelCalls: 2, MaxToolResults: 99, MaxRepeatedToolCalls: 99})
	messages := []message.Message{
		message.Human("Old task"), message.Assistant("one"), message.Assistant("two"),
		message.Human("New task"), message.Assistant("one"),
	}
	called := false
	_, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{State: state.Values{agent.MessagesKey: messages}}, func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		called = true
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("two")}}, nil
	})
	if err != nil || !called {
		t.Fatalf("called = %v, error = %v", called, err)
	}
}

func TestNemotronProgressBudgetPrioritizesInformativeResults(t *testing.T) {
	messages := []message.Message{message.Human("Find it")}
	for index, pair := range [][2]string{{"glob", "No files found"}, {"lookup", `{"status":"ready","id":42}`}} {
		call := message.ToolCall{ID: string(rune('a' + index)), Name: pair[0], Arguments: json.RawMessage(`{}`)}
		assistant := message.Assistant("")
		assistant.ToolCalls = []message.ToolCall{call}
		messages = append(messages, assistant, message.Tool(call.ID, pair[1]))
	}
	text := nemotronBudgetFallback(messages, "test")
	if strings.Index(text, "lookup") > strings.Index(text, "glob") || !strings.Contains(text, `{"id":42,"status":"ready"}`) {
		t.Fatalf("fallback = %q", text)
	}
}

func TestNemotronPolicyPrefersDomainToolsForNonFileRequest(t *testing.T) {
	middleware := nemotronPolicyNudgeMiddleware()
	tools := []tool.Tool{
		testNemotronTool("read_file"), testNemotronTool("list_incidents"),
	}
	_, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{Messages: []message.Message{message.Human("Which incident has the most alerts?")}, Tools: tools}, func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
		last := request.Messages[len(request.Messages)-1]
		if last.Name != nemotronDomainPreferSource || !strings.Contains(last.TextContent(), "list_incidents") {
			return agent.ModelResponse{}, errors.New("domain-tool nudge missing")
		}
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("ok")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNemotronPolicyPrefersFilesystemForNamedFile(t *testing.T) {
	middleware := nemotronPolicyNudgeMiddleware()
	_, err := middleware.WrapModelCall(context.Background(), agent.ModelRequest{Messages: []message.Message{message.Human("Please read /tmp/report.txt")}, Tools: []tool.Tool{testNemotronTool("read_file")}}, func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
		last := request.Messages[len(request.Messages)-1]
		if last.Name != nemotronFilesystemSource || !strings.Contains(last.TextContent(), "first call read_file") {
			return agent.ModelResponse{}, errors.New("filesystem nudge missing")
		}
		return agent.ModelResponse{Messages: []message.Message{message.Assistant("ok")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNemotronPolicyNudgesChainedActionAfterLookup(t *testing.T) {
	middleware := nemotronPolicyNudgeMiddleware()
	call := message.ToolCall{ID: "lookup", Name: "get_report", Arguments: json.RawMessage(`{}`)}
	assistant := message.Assistant("")
	assistant.ToolCalls = []message.ToolCall{call}
	messages := []message.Message{message.Human("Get the report and then email it"), assistant, message.Tool(call.ID, "report body")}
	update, err := middleware.BeforeModel(context.Background(), state.Values{agent.MessagesKey: messages}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, err := policyMessages(update[agent.MessagesKey])
	if err != nil || len(nudges) != 1 || nudges[0].Name != nemotronToolChainSource {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func TestNemotronPolicyNudgesApprovedAction(t *testing.T) {
	call := message.ToolCall{ID: "lookup", Name: "get_reservation_details", Arguments: json.RawMessage(`{"reservation_id":"ABC123"}`)}
	assistant := message.Assistant("")
	assistant.ToolCalls = []message.ToolCall{call}
	messages := []message.Message{
		message.Human("Can you check reservation ABC123?"), assistant,
		message.Tool(call.ID, `{"reservation_id":"ABC123"}`),
		message.Human("Go ahead and cancel it now."),
	}
	update, err := nemotronPolicyNudgeMiddleware().BeforeModel(context.Background(), state.Values{agent.MessagesKey: messages}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[agent.MessagesKey])
	if len(nudges) != 1 || nudges[0].Name != nemotronActionSource || !strings.Contains(nudges[0].TextContent(), "perform an action now") {
		t.Fatalf("update = %#v", update)
	}
}

func TestNemotronPolicyReturnsFromDeadFilesystemSearch(t *testing.T) {
	domain := message.ToolCall{ID: "domain", Name: "incident_catalog", Arguments: json.RawMessage(`{}`)}
	filesystem := message.ToolCall{ID: "grep", Name: "grep", Arguments: json.RawMessage(`{"pattern":"alert"}`)}
	first := message.Assistant("")
	first.ToolCalls = []message.ToolCall{domain}
	second := message.Assistant("")
	second.ToolCalls = []message.ToolCall{filesystem}
	messages := []message.Message{message.Human("Which service has the most firing alerts?"), first, message.Tool(domain.ID, "[41017,41029]"), second, message.Tool(filesystem.ID, "No matches found")}
	update, err := nemotronPolicyNudgeMiddleware().BeforeModel(context.Background(), state.Values{agent.MessagesKey: messages}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[agent.MessagesKey])
	if len(nudges) != 1 || nudges[0].Name != nemotronDomainNudgeSource || !strings.Contains(nudges[0].TextContent(), "non-filesystem API/domain tools") {
		t.Fatalf("update = %#v", update)
	}
}

func TestNemotronPolicyNudgesLongConversationTransition(t *testing.T) {
	read := message.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/first.py"}`)}
	assistant := message.Assistant("")
	assistant.ToolCalls = []message.ToolCall{read}
	messages := []message.Message{
		message.Human("Read and summarize /first.py"), assistant, message.Tool(read.ID, "1  alpha"),
		message.Assistant("summary"), message.Human("Thanks. Move on to a new task: read /second.py"),
		message.Assistant("thinking"), message.Human("Actually do the same for another file /third.py."),
	}
	update, err := nemotronPolicyNudgeMiddleware().BeforeModel(context.Background(), state.Values{agent.MessagesKey: messages}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[agent.MessagesKey])
	if len(nudges) != 1 || nudges[0].Name != nemotronTransitionSource || !strings.Contains(nudges[0].TextContent(), "compact_conversation") {
		t.Fatalf("update = %#v", update)
	}
}

func TestNemotronFollowupDisciplineReentersOnce(t *testing.T) {
	script := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("What time and timezone should the weekly report use?")}},
		modeltest.Step{Check: func(request model.Request) error {
			if request.Messages[len(request.Messages)-1].Name != nemotronFollowupSource {
				return errors.New("follow-up repair nudge missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("Which delivery channel should receive the weekly report?")}},
	)
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{nemotronFollowupDiscipline()}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("Send me a weekly report every Monday at 9am")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "Which delivery channel should receive the weekly report?" {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestNemotronFollowupDisciplineAnalysisScopeContracts(t *testing.T) {
	middleware := nemotronFollowupDiscipline()
	tests := []struct {
		name, user, answer string
		wantRewrite        bool
	}{
		{name: "analysis goal", user: "Can you analyze my data?", answer: "What file, database, API, or pasted data should I use?", wantRewrite: true},
		{name: "legitimate source", user: "Can you prepare a weekly report?", answer: "Which source should I use for the report?"},
		{name: "supplied scope", user: "Prepare a weekly report for the current project.", answer: "Which project should I use?", wantRewrite: true},
		{name: "exact", user: "Send a report weekly. Reply with the single word DONE.", answer: "DONE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			update, err := middleware.AfterAgent(context.Background(), state.Values{agent.MessagesKey: []message.Message{message.Human(test.user), message.Assistant(test.answer)}}, agent.Runtime{})
			if err != nil {
				t.Fatal(err)
			}
			if (len(update) > 0) != test.wantRewrite {
				t.Fatalf("update = %#v", update)
			}
		})
	}
}

func TestNemotronFinalGuardPreservesMutationLiteral(t *testing.T) {
	call := message.ToolCall{ID: "send", Name: "notify_channel", Arguments: json.RawMessage(`{"channel":"#deployments","message":"v2.0 released","subject":"Release v2.0"}`)}
	assistant := message.Assistant("")
	assistant.ToolCalls = []message.ToolCall{call}
	history := []message.Message{message.Human("Notify the channel that v2.0 was released"), assistant, message.Tool(call.ID, "Posted to #deployments")}
	script := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("Done.")}},
		modeltest.Step{Check: func(request model.Request) error {
			text := request.Messages[len(request.Messages)-1].TextContent()
			if !strings.Contains(text, "v2.0") || !strings.Contains(text, "Release v2.0") {
				return errors.New("literal repair nudge missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("Posted Release v2.0 to #deployments.")}},
	)
	compiled, err := agent.New(agent.Options{Model: script, Middleware: []agent.Middleware{nemotronFinalAnswerGuard()}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "Posted Release v2.0 to #deployments." {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestNemotronFinalGuardHonorsExactReply(t *testing.T) {
	values := state.Values{agent.MessagesKey: []message.Message{message.Human("Reply with exactly SHELLEY_OK"), message.Assistant("SHELLEY_OK")}}
	update, err := nemotronFinalAnswerGuard().AfterAgent(context.Background(), values, agent.Runtime{})
	if err != nil || len(update) != 0 {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func TestNemotronFinalGuardRewritesOnlyVagueMutationResult(t *testing.T) {
	call := message.ToolCall{ID: "cancel", Name: "cancel_reservation", Arguments: json.RawMessage(`{"reservation_id":"ABC123"}`)}
	assistant := message.Assistant("")
	assistant.ToolCalls = []message.ToolCall{call}
	base := []message.Message{message.Human("Cancel reservation ABC123."), assistant, message.Tool(call.ID, `{"reservation_id":"ABC123","status":"cancelled","refund":"$25"}`)}
	middleware := nemotronFinalAnswerGuard()
	update, err := middleware.AfterAgent(context.Background(), state.Values{agent.MessagesKey: append(base, message.Assistant("Done."))}, agent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[agent.MessagesKey])
	if len(nudges) != 1 || !strings.Contains(nudges[0].TextContent(), "cancel_reservation") || !strings.Contains(nudges[0].TextContent(), "cancelled") {
		t.Fatalf("update = %#v", update)
	}
	update, err = middleware.AfterAgent(context.Background(), state.Values{agent.MessagesKey: append(base, message.Assistant("Reservation ABC123 was cancelled with a $25 refund."))}, agent.Runtime{})
	if err != nil || len(update) != 0 {
		t.Fatalf("informative update = %#v, error = %v", update, err)
	}
}

func TestNemotronMutationClassification(t *testing.T) {
	for _, name := range []string{"create_issue", "revoke_access", "charge_card"} {
		if !nemotronToolIsMutation(name) {
			t.Errorf("%q not classified as mutation", name)
		}
	}
	for _, name := range []string{"delete", "get_charge", "search_archive", "write_file", "write_todos", "postal_code_lookup"} {
		if nemotronToolIsMutation(name) {
			t.Errorf("%q classified as mutation", name)
		}
	}
}

func testNemotronTool(name string) tool.Tool {
	return tool.Func{Spec: tool.Definition{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
