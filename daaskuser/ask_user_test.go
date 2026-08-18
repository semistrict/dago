package daaskuser_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/daaskuser"
	"github.com/semistrict/dago/dacheckpoint"
	checkpointsqlite "github.com/semistrict/dago/dacheckpoint/sqlite"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func boolPointer(value bool) *bool { return &value }

func execute(t *testing.T, tool datool.Tool, arguments string, resume any) datool.Result {
	t.Helper()
	result, err := tool.Execute(context.Background(), json.RawMessage(arguments), datool.Runtime{CallID: "ask-1", Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func resultText(result datool.Result) string {
	if len(result.Content) == 0 {
		return ""
	}
	return result.Content[0].Text
}

func TestToolInterruptCarriesRequiredOptionalTextAndChoices(t *testing.T) {
	tool := daaskuser.NewTool("")
	arguments := `{"questions":[{"question":"Name?","type":"text"},{"question":"Editor?","type":"multiple_choice","choices":[{"value":"Vim"},{"value":"Emacs"}],"required":false}]}`
	paused := execute(t, tool, arguments, nil)
	if paused.Interrupt == nil || paused.Interrupt.ID != daaskuser.InterruptID {
		t.Fatalf("interrupt = %#v", paused.Interrupt)
	}
	request, ok := datool.InterruptAs[daaskuser.AskRequest](*paused.Interrupt)
	if !ok {
		t.Fatalf("interrupt payload type = %T", paused.Interrupt.Value)
	}
	if request.Type != daaskuser.ToolName || request.ToolCallID != "ask-1" || len(request.Questions) != 2 {
		t.Fatalf("request = %#v", request)
	}
	if !request.Questions[0].IsRequired() || request.Questions[1].IsRequired() {
		t.Fatalf("required defaults = %v, %v", request.Questions[0].IsRequired(), request.Questions[1].IsRequired())
	}
	if got := request.Questions[1].Choices; len(got) != 2 || got[0].Value != "Vim" || got[1].Value != "Emacs" {
		t.Fatalf("choices = %#v", got)
	}

	answered := execute(t, tool, arguments, daaskuser.AnswerResponse{Answers: []string{"Ramon", ""}})
	if answered.Status != "" || resultText(answered) != "Q: Name?\nA: Ramon\n\nQ: Editor?\nA: " {
		t.Fatalf("answered = %#v", answered)
	}

	customChoice := execute(t, tool, arguments, daaskuser.AnswerResponse{Status: daaskuser.AnswerAnswered, Answers: []string{"Ramon", "Helix"}})
	if !strings.Contains(resultText(customChoice), "Q: Editor?\nA: Helix") {
		t.Fatalf("custom choice result = %#v", customChoice)
	}
}

func TestToolRejectsInvalidQuestions(t *testing.T) {
	tool := daaskuser.NewTool("")
	tests := []struct {
		name      string
		arguments string
	}{
		{name: "empty", arguments: `{"questions":[]}`},
		{name: "empty text", arguments: `{"questions":[{"question":" ","type":"text"}]}`},
		{name: "text choices", arguments: `{"questions":[{"question":"Name?","type":"text","choices":[{"value":"A"}]}]}`},
		{name: "choice missing options", arguments: `{"questions":[{"question":"Pick?","type":"multiple_choice"}]}`},
		{name: "choice empty option", arguments: `{"questions":[{"question":"Pick?","type":"multiple_choice","choices":[{"value":" "}]}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), json.RawMessage(test.arguments), datool.Runtime{CallID: "ask"})
			if !errors.Is(err, datool.ErrInvalidArguments) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestToolNormalizesCancellationAndInvalidAnswers(t *testing.T) {
	tool := daaskuser.NewTool("")
	arguments := `{"questions":[{"question":"Proceed?","type":"text"}]}`

	cancelled := execute(t, tool, arguments, map[string]any{"status": "cancelled", "answers": []any{"ignored"}})
	if cancelled.Status != "" || resultText(cancelled) != "Q: Proceed?\nA: (cancelled)" {
		t.Fatalf("cancelled = %#v", cancelled)
	}

	tests := []struct {
		name   string
		resume any
		text   string
	}{
		{name: "malformed", resume: "bad", text: "invalid ask_user response payload"},
		{name: "missing answer", resume: map[string]any{"status": "answered"}, text: "ask_user answer count mismatch"},
		{name: "empty required", resume: map[string]any{"answers": []any{""}}, text: "required answer 0 is empty"},
		{name: "unknown status", resume: map[string]any{"status": "later", "answers": []any{"yes"}}, text: "invalid ask_user response status"},
		{name: "client error", resume: map[string]any{"status": "error", "error": "prompt failed"}, text: "prompt failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := execute(t, tool, arguments, test.resume)
			if result.Status != damessage.ToolStatusError || !strings.Contains(resultText(result), "A: (error: "+test.text) {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCheckpointRestoredToolDecodesJSONShapedResume(t *testing.T) {
	saver, err := checkpointsqlite.Open(filepath.Join(t.TempDir(), "ask-user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()

	call := damessage.ToolCall{ID: "ask-1", Name: daaskuser.ToolName, Arguments: json.RawMessage(`{"questions":[{"question":"Which color?","type":"multiple_choice","choices":[{"value":"blue"},{"value":"green"}]}]}`)}
	firstModel := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{call}}}})
	config := dacheckpoint.Config{ThreadID: "ask-user-restore"}
	first := dagent.New(firstModel, dagent.Options{Middleware: []dagent.Middleware{daaskuser.Middleware()}, Saver: saver})
	paused, err := first.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("ask me")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 {
		t.Fatalf("interrupts = %#v", paused.Interrupts)
	}

	secondModel := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Check: func(request damodel.Request) error {
		last := request.Messages[len(request.Messages)-1]
		if last.Role != damessage.RoleTool || last.Name != daaskuser.ToolName || last.TextContent() != "Q: Which color?\nA: blue" {
			return fmt.Errorf("restored tool result = %#v", last)
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	second := dagent.New(secondModel, dagent.Options{Middleware: []dagent.Middleware{daaskuser.Middleware()}, Saver: saver})
	resumed, err := second.Invoke(context.Background(), dagent.Input{Config: config, Resume: map[string]any{
		"status": "answered", "answers": []any{"blue"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Interrupts) != 0 || resumed.Messages[len(resumed.Messages)-1].TextContent() != "done" {
		t.Fatalf("resumed = %#v", resumed)
	}
}

func TestMiddlewareIsExplicitOptIn(t *testing.T) {
	without := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Check: func(request damodel.Request) error {
		for _, tool := range request.Tools {
			if tool.Name == daaskuser.ToolName {
				return errors.New("ask_user was enabled implicitly")
			}
		}
		if request.SystemMessage != nil && strings.Contains(request.SystemMessage.TextContent(), daaskuser.ToolName) {
			return errors.New("ask_user prompt was enabled implicitly")
		}
		return nil
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	if _, err := dagent.New(without, dagent.Options{}).Invoke(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	with := modeltest.New(damodel.Profile{ToolCalling: true}, modeltest.Step{Check: func(request damodel.Request) error {
		guidance := request.SystemMessage != nil && strings.Contains(request.SystemMessage.TextContent(), daaskuser.ToolName)
		if len(request.Messages) > 0 && request.Messages[0].Role == damessage.RoleSystem && strings.Contains(request.Messages[0].TextContent(), daaskuser.ToolName) {
			guidance = true
		}
		if !guidance {
			return errors.New("ask_user guidance missing")
		}
		for _, tool := range request.Tools {
			if tool.Name == daaskuser.ToolName {
				return nil
			}
		}
		return errors.New("ask_user tool missing")
	}, Response: damodel.Response{Message: damessage.Assistant("done")}})
	if _, err := dagent.New(with, dagent.Options{Middleware: []dagent.Middleware{daaskuser.Middleware()}}).Invoke(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
}

func TestQuestionRequiredDefaults(t *testing.T) {
	if !(daaskuser.Question{}).IsRequired() || (daaskuser.Question{Required: boolPointer(false)}).IsRequired() {
		t.Fatal("required default or explicit optional value is incorrect")
	}
}
