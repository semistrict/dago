package nemotron

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

func TestNemotronProgressBudgetStopsRepeatedCallLoop(t *testing.T) {
	middleware := ProgressBudget(ProgressBudgetOptions{MaxModelCalls: 99, MaxToolResults: 99, MaxRepeatedToolCalls: 3})
	messages := []damessage.Message{damessage.Human("Read it.")}
	for index := 0; index < 3; index++ {
		call := damessage.ToolCall{ID: string(rune('a' + index)), Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/x"}`)}
		assistant := damessage.Assistant("")
		assistant.ToolCalls = []damessage.ToolCall{call}
		messages = append(messages, assistant, damessage.Tool(call.ID, "page"))
	}
	called := false
	response, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{State: dastate.Values{dagent.MessagesKey: messages}}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		called = true
		return dagent.ModelResponse{}, nil
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

func TestNemotronProgressBudgetRejectsNegativeLimits(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative progress budget was accepted")
		}
	}()
	ProgressBudget(ProgressBudgetOptions{MaxModelCalls: -1})
}

func TestNemotronProgressBudgetAllowsNonconsecutiveCalls(t *testing.T) {
	middleware := ProgressBudget(ProgressBudgetOptions{MaxModelCalls: 99, MaxToolResults: 99, MaxRepeatedToolCalls: 3})
	messages := []damessage.Message{damessage.Human("Inspect.")}
	for index, name := range []string{"read_file", "grep", "read_file", "grep", "read_file"} {
		call := damessage.ToolCall{ID: string(rune('a' + index)), Name: name, Arguments: json.RawMessage(`{"path":"/x"}`)}
		assistant := damessage.Assistant("")
		assistant.ToolCalls = []damessage.ToolCall{call}
		messages = append(messages, assistant, damessage.Tool(call.ID, "ok"))
	}
	called := false
	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{Messages: messages}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		called = true
		return dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("done")}}, nil
	})
	if err != nil || !called {
		t.Fatalf("called = %v, error = %v", called, err)
	}
}

func TestNemotronProgressBudgetCountsOnlyActiveTurn(t *testing.T) {
	middleware := ProgressBudget(ProgressBudgetOptions{MaxModelCalls: 2, MaxToolResults: 99, MaxRepeatedToolCalls: 99})
	messages := []damessage.Message{
		damessage.Human("Old task"), damessage.Assistant("one"), damessage.Assistant("two"),
		damessage.Human("New task"), damessage.Assistant("one"),
	}
	called := false
	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{State: dastate.Values{dagent.MessagesKey: messages}}, func(context.Context, dagent.ModelRequest) (dagent.ModelResponse, error) {
		called = true
		return dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("two")}}, nil
	})
	if err != nil || !called {
		t.Fatalf("called = %v, error = %v", called, err)
	}
}

func TestNemotronProgressBudgetPrioritizesInformativeResults(t *testing.T) {
	messages := []damessage.Message{damessage.Human("Find it")}
	for index, pair := range [][2]string{{"glob", "No files found"}, {"lookup", `{"status":"ready","id":42}`}} {
		call := damessage.ToolCall{ID: string(rune('a' + index)), Name: pair[0], Arguments: json.RawMessage(`{}`)}
		assistant := damessage.Assistant("")
		assistant.ToolCalls = []damessage.ToolCall{call}
		messages = append(messages, assistant, damessage.Tool(call.ID, pair[1]))
	}
	text := nemotronBudgetFallback(messages, "test")
	if strings.Index(text, "lookup") > strings.Index(text, "glob") || !strings.Contains(text, `{"id":42,"status":"ready"}`) {
		t.Fatalf("fallback = %q", text)
	}
}

func TestNemotronPolicyPrefersDomainToolsForNonFileRequest(t *testing.T) {
	middleware := nemotronPolicyNudgeMiddleware()
	tools := []datool.Tool{
		testNemotronTool("read_file"), testNemotronTool("list_incidents"),
	}
	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{Messages: []damessage.Message{damessage.Human("Which incident has the most alerts?")}, Tools: tools}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		last := request.Messages[len(request.Messages)-1]
		if last.Name != nemotronDomainPreferSource || !strings.Contains(last.TextContent(), "list_incidents") {
			return dagent.ModelResponse{}, errors.New("domain-tool nudge missing")
		}
		return dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("ok")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNemotronPolicyPrefersFilesystemForNamedFile(t *testing.T) {
	middleware := nemotronPolicyNudgeMiddleware()
	_, err := middleware.WrapModelCall(context.Background(), dagent.ModelRequest{Messages: []damessage.Message{damessage.Human("Please read /tmp/report.txt")}, Tools: []datool.Tool{testNemotronTool("read_file")}}, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		last := request.Messages[len(request.Messages)-1]
		if last.Name != nemotronFilesystemSource || !strings.Contains(last.TextContent(), "first call read_file") {
			return dagent.ModelResponse{}, errors.New("filesystem nudge missing")
		}
		return dagent.ModelResponse{Messages: []damessage.Message{damessage.Assistant("ok")}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNemotronPolicyNudgesChainedActionAfterLookup(t *testing.T) {
	middleware := nemotronPolicyNudgeMiddleware()
	call := damessage.ToolCall{ID: "lookup", Name: "get_report", Arguments: json.RawMessage(`{}`)}
	assistant := damessage.Assistant("")
	assistant.ToolCalls = []damessage.ToolCall{call}
	messages := []damessage.Message{damessage.Human("Get the report and then email it"), assistant, damessage.Tool(call.ID, "report body")}
	update, err := middleware.BeforeModel(context.Background(), dastate.Values{dagent.MessagesKey: messages}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, err := policyMessages(update[dagent.MessagesKey])
	if err != nil || len(nudges) != 1 || nudges[0].Name != nemotronToolChainSource {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func TestNemotronPolicyNudgesApprovedAction(t *testing.T) {
	call := damessage.ToolCall{ID: "lookup", Name: "get_reservation_details", Arguments: json.RawMessage(`{"reservation_id":"ABC123"}`)}
	assistant := damessage.Assistant("")
	assistant.ToolCalls = []damessage.ToolCall{call}
	messages := []damessage.Message{
		damessage.Human("Can you check reservation ABC123?"), assistant,
		damessage.Tool(call.ID, `{"reservation_id":"ABC123"}`),
		damessage.Human("Go ahead and cancel it now."),
	}
	update, err := nemotronPolicyNudgeMiddleware().BeforeModel(context.Background(), dastate.Values{dagent.MessagesKey: messages}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[dagent.MessagesKey])
	if len(nudges) != 1 || nudges[0].Name != nemotronActionSource || !strings.Contains(nudges[0].TextContent(), "perform an action now") {
		t.Fatalf("update = %#v", update)
	}
}

func TestNemotronPolicyReturnsFromDeadFilesystemSearch(t *testing.T) {
	domain := damessage.ToolCall{ID: "domain", Name: "incident_catalog", Arguments: json.RawMessage(`{}`)}
	filesystem := damessage.ToolCall{ID: "grep", Name: "grep", Arguments: json.RawMessage(`{"pattern":"alert"}`)}
	first := damessage.Assistant("")
	first.ToolCalls = []damessage.ToolCall{domain}
	second := damessage.Assistant("")
	second.ToolCalls = []damessage.ToolCall{filesystem}
	messages := []damessage.Message{damessage.Human("Which service has the most firing alerts?"), first, damessage.Tool(domain.ID, "[41017,41029]"), second, damessage.Tool(filesystem.ID, "No matches found")}
	update, err := nemotronPolicyNudgeMiddleware().BeforeModel(context.Background(), dastate.Values{dagent.MessagesKey: messages}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[dagent.MessagesKey])
	if len(nudges) != 1 || nudges[0].Name != nemotronDomainNudgeSource || !strings.Contains(nudges[0].TextContent(), "non-filesystem API/domain tools") {
		t.Fatalf("update = %#v", update)
	}
}

func TestNemotronPolicyNudgesLongConversationTransition(t *testing.T) {
	read := damessage.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"/first.py"}`)}
	assistant := damessage.Assistant("")
	assistant.ToolCalls = []damessage.ToolCall{read}
	messages := []damessage.Message{
		damessage.Human("Read and summarize /first.py"), assistant, damessage.Tool(read.ID, "1  alpha"),
		damessage.Assistant("summary"), damessage.Human("Thanks. Move on to a new task: read /second.py"),
		damessage.Assistant("thinking"), damessage.Human("Actually do the same for another file /third.py."),
	}
	update, err := nemotronPolicyNudgeMiddleware().BeforeModel(context.Background(), dastate.Values{dagent.MessagesKey: messages}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[dagent.MessagesKey])
	if len(nudges) != 1 || nudges[0].Name != nemotronTransitionSource || !strings.Contains(nudges[0].TextContent(), "compact_conversation") {
		t.Fatalf("update = %#v", update)
	}
}

func TestNemotronFollowupDisciplineReentersOnce(t *testing.T) {
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("What time and timezone should the weekly report use?")}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if request.Messages[len(request.Messages)-1].Name != nemotronFollowupSource {
				return errors.New("follow-up repair nudge missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("Which delivery channel should receive the weekly report?")}},
	)
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{nemotronFollowupDiscipline()}})

	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("Send me a weekly report every Monday at 9am")}})
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
			update, err := middleware.AfterAgent(context.Background(), dastate.Values{dagent.MessagesKey: []damessage.Message{damessage.Human(test.user), damessage.Assistant(test.answer)}}, dagent.Runtime{})
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
	call := damessage.ToolCall{ID: "send", Name: "notify_channel", Arguments: json.RawMessage(`{"channel":"#deployments","message":"v2.0 released","subject":"Release v2.0"}`)}
	assistant := damessage.Assistant("")
	assistant.ToolCalls = []damessage.ToolCall{call}
	history := []damessage.Message{damessage.Human("Notify the channel that v2.0 was released"), assistant, damessage.Tool(call.ID, "Posted to #deployments")}
	script := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("Done.")}},
		modeltest.Step{Check: func(request damodel.Request) error {
			text := request.Messages[len(request.Messages)-1].TextContent()
			if !strings.Contains(text, "v2.0") || !strings.Contains(text, "Release v2.0") {
				return errors.New("literal repair nudge missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("Posted Release v2.0 to #deployments.")}},
	)
	compiled := dagent.New(script, dagent.Options{Middleware: []dagent.Middleware{nemotronFinalAnswerGuard()}})

	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "Posted Release v2.0 to #deployments." {
		t.Fatalf("messages = %#v", result.Messages)
	}
}

func TestNemotronFinalGuardHonorsExactReply(t *testing.T) {
	values := dastate.Values{dagent.MessagesKey: []damessage.Message{damessage.Human("Reply with exactly SHELLEY_OK"), damessage.Assistant("SHELLEY_OK")}}
	update, err := nemotronFinalAnswerGuard().AfterAgent(context.Background(), values, dagent.Runtime{})
	if err != nil || len(update) != 0 {
		t.Fatalf("update = %#v, error = %v", update, err)
	}
}

func TestNemotronFinalGuardRewritesOnlyVagueMutationResult(t *testing.T) {
	call := damessage.ToolCall{ID: "cancel", Name: "cancel_reservation", Arguments: json.RawMessage(`{"reservation_id":"ABC123"}`)}
	assistant := damessage.Assistant("")
	assistant.ToolCalls = []damessage.ToolCall{call}
	base := []damessage.Message{damessage.Human("Cancel reservation ABC123."), assistant, damessage.Tool(call.ID, `{"reservation_id":"ABC123","status":"cancelled","refund":"$25"}`)}
	middleware := nemotronFinalAnswerGuard()
	update, err := middleware.AfterAgent(context.Background(), dastate.Values{dagent.MessagesKey: append(base, damessage.Assistant("Done."))}, dagent.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	nudges, _ := policyMessages(update[dagent.MessagesKey])
	if len(nudges) != 1 || !strings.Contains(nudges[0].TextContent(), "cancel_reservation") || !strings.Contains(nudges[0].TextContent(), "cancelled") {
		t.Fatalf("update = %#v", update)
	}
	update, err = middleware.AfterAgent(context.Background(), dastate.Values{dagent.MessagesKey: append(base, damessage.Assistant("Reservation ABC123 was cancelled with a $25 refund."))}, dagent.Runtime{})
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

func testNemotronTool(name string) datool.Tool {
	return datool.Func{Spec: datool.Definition{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
