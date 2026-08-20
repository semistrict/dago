package dacode

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago/daaskuser"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

func askUserTestInterrupt() dagent.Interrupt {
	optional := false
	return dagent.Interrupt{ID: daaskuser.InterruptID, Value: daaskuser.AskRequest{
		Type: daaskuser.ToolName, ToolCallID: "call-question", Questions: []daaskuser.Question{
			{Question: "Project name?", Type: daaskuser.QuestionText},
			{Question: "Pick a color", Type: daaskuser.QuestionMultipleChoice, Choices: []daaskuser.Choice{{Value: "red"}, {Value: "blue"}}},
			{Question: "Optional detail", Type: daaskuser.QuestionText, Required: &optional},
		},
	}}
}

func TestDecodeAskUserRequestValidatesDiscriminatorAndSchema(t *testing.T) {
	request, err := decodeAskUserRequest(askUserTestInterrupt())
	if err != nil || request.ToolCallID != "call-question" || len(request.Questions) != 3 {
		t.Fatalf("request = %#v, err = %v", request, err)
	}
	invalid := askUserTestInterrupt()
	invalid.ID = "approval"
	if _, err := decodeAskUserRequest(invalid); err == nil {
		t.Fatal("mismatched interrupt ID was accepted")
	}
	invalid = askUserTestInterrupt()
	invalid.Value = daaskuser.AskRequest{Type: daaskuser.ToolName, ToolCallID: "call", Questions: []daaskuser.Question{{Question: " ", Type: daaskuser.QuestionText}}}
	if _, err := decodeAskUserRequest(invalid); err == nil {
		t.Fatal("blank question was accepted")
	}
}

func TestAskUserCollectsTextChoicesOptionalAnswersAndResumes(t *testing.T) {
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "model", "thread", false, false, "")
	model.resize(100, 35)
	model.addToolCall(damessage.ToolCall{ID: "call-question", Name: daaskuser.ToolName, Arguments: json.RawMessage(`{"questions":["redacted"]}`)})
	if err := model.presentAskUser(askUserTestInterrupt()); err != nil {
		t.Fatal(err)
	}
	model.askUser.ready = true
	model.askUser.questions[0].input.SetValue("alpha\nbeta")
	if command := model.confirmAskUserAnswer(); command != nil || model.askUser.current != 1 {
		t.Fatalf("first confirmation command = %v, current = %d", command, model.askUser.current)
	}
	model.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if command := model.confirmAskUserAnswer(); command != nil || model.askUser.current != 2 {
		t.Fatalf("choice confirmation command = %v, current = %d", command, model.askUser.current)
	}
	command := model.confirmAskUserAnswer()
	if command == nil || model.askUser != nil || len(runner.inputs) != 1 {
		t.Fatalf("submit command = %v, ask state = %#v, inputs = %d", command, model.askUser, len(runner.inputs))
	}
	encoded, err := json.Marshal(runner.inputs[0].Resume)
	if err != nil {
		t.Fatal(err)
	}
	var response daaskuser.AnswerResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != daaskuser.AnswerAnswered || len(response.Answers) != 3 || response.Answers[0] != "alpha\nbeta" || response.Answers[1] != "blue" || response.Answers[2] != "" {
		t.Fatalf("resume response = %#v", runner.inputs[0].Resume)
	}
	item := model.items[model.toolItems["call-question"]]
	if !item.done || item.failed || item.text != askUserAnsweredSummary {
		t.Fatalf("pending row = %#v", item)
	}
}

func TestAskUserRequiredOtherAndQuestionNavigation(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(90, 30)
	if err := model.presentAskUser(askUserTestInterrupt()); err != nil {
		t.Fatal(err)
	}
	model.askUser.ready = true
	if command := model.confirmAskUserAnswer(); command != nil || model.askUser.warning != askUserMissingAnswer {
		t.Fatalf("blank required answer command = %v, warning = %q", command, model.askUser.warning)
	}
	model.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyTab})
	model.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyDown})
	model.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if input := model.askUser.activeInput(); input == nil {
		t.Fatal("Other did not open its free-form input")
	} else {
		input.SetValue("green")
	}
	model.handleAskUserKey(testShiftKey(tea.KeyTab))
	if model.askUser.current != 0 {
		t.Fatalf("shift-tab current = %d, want 0", model.askUser.current)
	}
	view := model.View()
	for _, expected := range []string{"Agent has 3 Questions for you", "Project name? (required)", askUserOtherChoice, "Tab/Shift+Tab switch question", "Esc to cancel"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view missing %q:\n%s", expected, view)
		}
	}
}

func TestAskUserCancelRejectsRowAndCancelsPendingRun(t *testing.T) {
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "model", "thread", false, false, "")
	model.addToolCall(damessage.ToolCall{ID: "call-question", Name: daaskuser.ToolName})
	if err := model.presentAskUser(askUserTestInterrupt()); err != nil {
		t.Fatal(err)
	}
	model.askUser.ready = true
	command, handled := model.handleAskUserKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !handled || command == nil || model.askUser != nil {
		t.Fatalf("handled = %v, command = %v, state = %#v", handled, command, model.askUser)
	}
	message := command()
	if _, ok := message.(askUserCancelledMsg); !ok || len(runner.cancelled) != 1 || runner.cancelled[0] != "thread" {
		t.Fatalf("message = %T, cancelled = %#v", message, runner.cancelled)
	}
	item := model.items[model.toolItems["call-question"]]
	if !item.done || !item.failed || item.text != "" {
		t.Fatalf("cancelled row = %#v", item)
	}
	if got := model.items[len(model.items)-1].text; got != "Question dismissed." {
		t.Fatalf("dismissal item = %q", got)
	}
}

func TestAskUserAnswerTranscriptStaysPrivateWhileUIAndAuditUseSummary(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(100, 30)
	model.addToolCall(damessage.ToolCall{ID: "call-question", Name: daaskuser.ToolName, Arguments: json.RawMessage(`{"questions":["Name?"]}`)})
	secret := "private-answer-4815"
	model.completeTool(damessage.Message{
		Role: damessage.RoleTool, Name: daaskuser.ToolName, ToolCallID: "call-question",
		Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: "Q: Name?\nA: " + secret}},
	})
	if strings.Contains(model.renderTranscript(), secret) || !strings.Contains(model.renderTranscript(), askUserAnsweredSummary) {
		t.Fatalf("collapsed row leaked answer or omitted summary:\n%s", model.renderTranscript())
	}
	if item := model.items[model.toolItems["call-question"]]; !strings.Contains(item.text, secret) {
		t.Fatal("authoritative answer transcript was not retained in session state")
	}
	audit := model.reviewTranscript()
	if strings.Contains(audit, secret) || !strings.Contains(audit, "result: "+askUserAnsweredSummary) {
		t.Fatalf("audit transcript leaked answer or omitted summary:\n%s", audit)
	}

	model.items[model.toolItems["call-question"]].failed = true
	audit = model.reviewTranscript()
	if strings.Contains(audit, secret) || !strings.Contains(audit, "result: "+askUserFailedSummary) {
		t.Fatalf("failure audit leaked answer or omitted summary:\n%s", audit)
	}
}

func TestAskUserRendersAgentTextTerminalSafely(t *testing.T) {
	interrupt := askUserTestInterrupt()
	request := interrupt.Value.(daaskuser.AskRequest)
	request.Questions[0].Question = "Name?\x1b[2J (required)"
	interrupt.Value = request
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(90, 30)
	if err := model.presentAskUser(interrupt); err != nil {
		t.Fatal(err)
	}
	view := model.View()
	if strings.Contains(view, "\x1b[2J") || !strings.Contains(view, "<U+001B CONTROL>[2J") {
		t.Fatalf("unsafe question rendering:\n%s", view)
	}
	if strings.Count(view, "required") != 2 { // current question and another required question
		t.Fatalf("trailing annotation was not normalized:\n%s", view)
	}
}

func TestAskUserEditorTargetsActiveAnswerOnly(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	if err := model.presentAskUser(askUserTestInterrupt()); err != nil {
		t.Fatal(err)
	}
	model.askUser.ready = true
	model.editDraft = func(text string) (tea.Cmd, error) {
		if text != "draft" {
			t.Fatalf("editor text = %q", text)
		}
		return func() tea.Msg { return editorFinishedMsg{text: "edited answer"} }, nil
	}
	model.askUser.questions[0].input.SetValue("draft")
	command, handled := model.handleAskUserKey(testCtrlKey('x'))
	if !handled || command == nil {
		t.Fatalf("handled = %v, command = %v", handled, command)
	}
	model.Update(command())
	if got := model.askUser.questions[0].input.Value(); got != "edited answer" {
		t.Fatalf("answer = %q", got)
	}
	if model.composer.Value() != "" {
		t.Fatalf("composer was changed to %q", model.composer.Value())
	}
}
