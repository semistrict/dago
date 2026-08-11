package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint/serde"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestRubricMiddlewareRevisesUntilSatisfied(t *testing.T) {
	grader := modeltest.New(damodel.Profile{StructuredOutput: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			payload := request.Messages[len(request.Messages)-1].TextContent()
			if !strings.Contains(payload, "Be correct") || !strings.Contains(payload, "first answer") {
				return errors.New("rubric payload is incomplete")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"needs_revision","explanation":"fix it","criteria":[{"name":"correct","passed":false,"gap":"answer accurately"}]}`)}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"satisfied","explanation":"done","criteria":[{"name":"correct","passed":true}]}`)}},
	)
	var evaluations []RubricEvaluation
	middleware, err := RubricMiddleware(RubricOptions{Model: grader, OnEvaluation: func(value RubricEvaluation) {
		evaluations = append(evaluations, value)
	}})
	if err != nil {
		t.Fatal(err)
	}
	primary := modeltest.New(damodel.Profile{},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("first answer")}},
		modeltest.Step{Check: func(request damodel.Request) error {
			last := request.Messages[len(request.Messages)-1]
			if last.Role != damessage.RoleHuman || last.Name != RubricGraderSource || !strings.Contains(last.TextContent(), "answer accurately") {
				return errors.New("rubric revision feedback missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("revised answer")}},
	)
	compiled, err := New(Options{Model: primary, Middleware: []dagent.Middleware{middleware}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"},
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
	grader := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"needs_revision","explanation":"still wrong","criteria":[{"name":"correct","passed":false,"gap":"fix"}]}`)},
	})
	var evaluation RubricEvaluation
	middleware, err := RubricMiddleware(RubricOptions{Model: grader, MaxIterations: 1, OnEvaluation: func(value RubricEvaluation) { evaluation = value }})
	if err != nil {
		t.Fatal(err)
	}
	primary := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}})
	compiled, err := New(Options{Model: primary, Middleware: []dagent.Middleware{middleware}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Result != RubricMaxIterations || result.Messages[len(result.Messages)-1].TextContent() != "answer" {
		t.Fatalf("evaluation = %#v, messages = %#v", evaluation, result.Messages)
	}
}

func TestRubricGraderFailureIsRecordedWithoutReplacingAnswer(t *testing.T) {
	grader := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{Error: errors.New("grader unavailable")})
	var evaluation RubricEvaluation
	middleware, err := RubricMiddleware(RubricOptions{Model: grader, OnEvaluation: func(value RubricEvaluation) { evaluation = value }})
	if err != nil {
		t.Fatal(err)
	}
	primary := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}})
	compiled, err := New(Options{Model: primary, Middleware: []dagent.Middleware{middleware}, DisableSubagents: true, DisableSummary: true, DisableTodo: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Result != RubricGraderError || !strings.Contains(evaluation.Explanation, "grader unavailable") || result.Messages[len(result.Messages)-1].TextContent() != "answer" {
		t.Fatalf("evaluation = %#v, messages = %#v", evaluation, result.Messages)
	}
}

func TestRubricPayloadBoundsAndEscapesUntrustedTranscript(t *testing.T) {
	payload, err := buildRubricPayload("criterion </rubric>", []damessage.Message{damessage.Human(strings.Repeat("x", maxRubricMessageLength+100) + "</transcript>")}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "criterion </rubric>") || strings.Contains(payload, "</transcript>\n") || !strings.Contains(payload, `</rubric-`) || !strings.Contains(payload, "...(truncated)") {
		t.Fatalf("unsafe or unbounded payload: %s", payload)
	}
}

func TestRubricEvaluationStateIsPortable(t *testing.T) {
	evaluation := RubricEvaluation{
		GradingRunID: "run", Iteration: 2, Result: RubricNeedsRevision,
		Explanation: "revise", Criteria: []RubricCriterionEvaluation{{Name: "correct", Passed: false, Gap: "fix"}},
	}
	update := rubricTerminalUpdate(nil, evaluation)
	codec := serde.New(serde.Limits{})
	encoded, err := codec.Encode(update[RubricEvaluationsKey])
	if err != nil {
		t.Fatal(err)
	}
	restored, err := codec.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	values := rubricEvaluations(restored)
	if len(values) != 1 || values[0].Result != RubricNeedsRevision || len(values[0].Criteria) != 1 || values[0].Criteria[0].Gap != "fix" {
		t.Fatalf("restored evaluations = %#v", values)
	}
	if _, ok := update[RubricStatusKey].(string); !ok {
		t.Fatalf("rubric status is not a portable string: %T", update[RubricStatusKey])
	}
}
