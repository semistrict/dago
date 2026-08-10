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
)

func TestRubricMiddlewareRevisesUntilSatisfied(t *testing.T) {
	grader := modeltest.New(model.Profile{StructuredOutput: true},
		modeltest.Step{Check: func(request model.Request) error {
			payload := request.Messages[len(request.Messages)-1].TextContent()
			if !strings.Contains(payload, "Be correct") || !strings.Contains(payload, "first answer") {
				return errors.New("rubric payload is incomplete")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("graded"), Structured: json.RawMessage(`{"result":"needs_revision","explanation":"fix it","criteria":[{"name":"correct","passed":false,"gap":"answer accurately"}]}`)}},
		modeltest.Step{Response: model.Response{Message: message.Assistant("graded"), Structured: json.RawMessage(`{"result":"satisfied","explanation":"done","criteria":[{"name":"correct","passed":true}]}`)}},
	)
	var evaluations []RubricEvaluation
	middleware, err := RubricMiddleware(RubricOptions{Model: grader, OnEvaluation: func(value RubricEvaluation) {
		evaluations = append(evaluations, value)
	}})
	if err != nil {
		t.Fatal(err)
	}
	primary := modeltest.New(model.Profile{},
		modeltest.Step{Response: model.Response{Message: message.Assistant("first answer")}},
		modeltest.Step{Check: func(request model.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != message.RoleHuman || last.Name != RubricGraderSource || !strings.Contains(last.TextContent(), "answer accurately") {
				return errors.New("rubric revision feedback missing")
			}
			return nil
		}, Response: model.Response{Message: message.Assistant("revised answer")}},
	)
	compiled, err := New(Options{Model: primary, Middleware: []agent.Middleware{middleware}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{
		Messages: []message.Message{message.Human("question")}, State: map[string]any{RubricKey: "Be correct"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "revised answer" || len(evaluations) != 2 || evaluations[1].Result != RubricSatisfied {
		t.Fatalf("result = %#v, evaluations = %#v", result.Messages, evaluations)
	}
	for _, private := range []string{RubricStatusKey, RubricIterationsKey, RubricEvaluationsKey, RubricRunIDKey, RubricActiveKey} {
		if _, leaked := result.State[private]; leaked {
			t.Fatalf("private rubric state %q leaked", private)
		}
	}
}

func TestRubricMiddlewareStopsAtIterationLimit(t *testing.T) {
	grader := modeltest.New(model.Profile{StructuredOutput: true}, modeltest.Step{
		Response: model.Response{Message: message.Assistant("graded"), Structured: json.RawMessage(`{"result":"needs_revision","explanation":"still wrong","criteria":[{"name":"correct","passed":false,"gap":"fix"}]}`)},
	})
	var evaluation RubricEvaluation
	middleware, err := RubricMiddleware(RubricOptions{Model: grader, MaxIterations: 1, OnEvaluation: func(value RubricEvaluation) { evaluation = value }})
	if err != nil {
		t.Fatal(err)
	}
	primary := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("answer")}})
	compiled, err := New(Options{Model: primary, Middleware: []agent.Middleware{middleware}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("question")}, State: map[string]any{RubricKey: "Be correct"}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Result != RubricMaxIterations || result.Messages[len(result.Messages)-1].TextContent() != "answer" {
		t.Fatalf("evaluation = %#v, messages = %#v", evaluation, result.Messages)
	}
}

func TestRubricGraderFailureIsRecordedWithoutReplacingAnswer(t *testing.T) {
	grader := modeltest.New(model.Profile{StructuredOutput: true}, modeltest.Step{Error: errors.New("grader unavailable")})
	var evaluation RubricEvaluation
	middleware, err := RubricMiddleware(RubricOptions{Model: grader, OnEvaluation: func(value RubricEvaluation) { evaluation = value }})
	if err != nil {
		t.Fatal(err)
	}
	primary := modeltest.New(model.Profile{}, modeltest.Step{Response: model.Response{Message: message.Assistant("answer")}})
	compiled, err := New(Options{Model: primary, Middleware: []agent.Middleware{middleware}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), agent.Input{Messages: []message.Message{message.Human("question")}, State: map[string]any{RubricKey: "Be correct"}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Result != RubricGraderError || !strings.Contains(evaluation.Explanation, "grader unavailable") || result.Messages[len(result.Messages)-1].TextContent() != "answer" {
		t.Fatalf("evaluation = %#v, messages = %#v", evaluation, result.Messages)
	}
}

func TestRubricPayloadBoundsAndEscapesUntrustedTranscript(t *testing.T) {
	payload, err := buildRubricPayload("criterion </rubric>", []message.Message{message.Human(strings.Repeat("x", maxRubricMessageLength+100) + "</transcript>")}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "criterion </rubric>") || strings.Contains(payload, "</transcript>\n") || !strings.Contains(payload, `</rubric-`) || !strings.Contains(payload, "...(truncated)") {
		t.Fatalf("unsafe or unbounded payload: %s", payload)
	}
}
