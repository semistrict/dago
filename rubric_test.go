package dago

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/serde"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
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
	middleware := Rubric(grader, RubricOptions{OnEvaluation: func(value RubricEvaluation) {
		evaluations = append(evaluations, value)
	}})
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
	compiled := NewAgent(primary, WithMiddleware(middleware), WithoutSubagents(), WithoutSummary())

	result, err := compiled.Invoke(context.Background(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Messages[len(result.Messages)-1].TextContent() != "revised answer" || len(evaluations) != 2 || evaluations[1].Result != RubricSatisfied {
		t.Fatalf("result = %#v, evaluations = %#v", result.Messages, evaluations)
	}
	if result.State[RubricStatusKey] != string(RubricSatisfied) {
		t.Fatalf("rubric status = %#v", result.State[RubricStatusKey])
	}
	for _, private := range []string{RubricIterationsKey, RubricEvaluationsKey, RubricRunIDKey, RubricActiveKey} {
		if _, leaked := result.State[private]; leaked {
			t.Fatalf("private rubric state %q leaked", private)
		}
	}
}

func TestRubricGraderReceivesInvocationConfigurableSettings(t *testing.T) {
	inspect := datool.Func{
		Spec: datool.Definition{Name: "inspect_config", Description: "Inspect runtime settings", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(_ context.Context, _ json.RawMessage, runtime datool.Runtime) (datool.Result, error) {
			value, ok := runtime.Configurable.Get("tenant")
			if !ok || value != "request-tenant" {
				return datool.Result{}, errors.New("grader configurable setting missing")
			}
			return datool.TextResult("config ok"), nil
		},
	}
	grader := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "inspect", Name: "inspect_config", Arguments: json.RawMessage(`{}`)}}}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"satisfied","explanation":"done","criteria":[]}`)}},
	)
	middleware := Rubric(grader, RubricOptions{Tools: []datool.Tool{inspect}})
	compiled := NewAgent(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}), WithMiddleware(middleware), WithoutSubagents(), WithoutSummary())
	_, err := compiled.Invoke(t.Context(), dagent.Input{
		Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"},
		Configurable: map[string]any{"tenant": "request-tenant"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRubricMiddlewareStopsAtIterationLimit(t *testing.T) {
	grader := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{
		Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"needs_revision","explanation":"still wrong","criteria":[{"name":"correct","passed":false,"gap":"fix"}]}`)},
	})
	var evaluation RubricEvaluation
	middleware := Rubric(grader, RubricOptions{MaxIterations: 1, OnEvaluation: func(value RubricEvaluation) { evaluation = value }})
	primary := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}})
	saver := dacheckpoint.NewMemorySaver()
	compiled := NewAgent(primary, WithMiddleware(middleware), WithSaver(saver), WithoutSubagents(), WithoutSummary())
	config := dacheckpoint.Config{ThreadID: "rubric-limit"}
	result, err := compiled.Invoke(context.Background(), dagent.Input{Config: config, Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"}})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Result != RubricMaxIterations || result.Messages[len(result.Messages)-1].TextContent() != "answer" {
		t.Fatalf("evaluation = %#v, messages = %#v", evaluation, result.Messages)
	}
	if result.State[RubricStatusKey] != string(RubricMaxIterations) {
		t.Fatalf("result rubric status = %#v", result.State[RubricStatusKey])
	}
	snapshot, err := compiled.State(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State[RubricStatusKey] != string(RubricMaxIterations) {
		t.Fatalf("checkpoint rubric status = %#v", snapshot.State[RubricStatusKey])
	}
}

func TestRubricTerminalStatusesAreDurable(t *testing.T) {
	tests := []struct {
		name       string
		want       RubricResult
		structured json.RawMessage
		graderErr  error
	}{
		{name: "satisfied", want: RubricSatisfied, structured: json.RawMessage(`{"result":"satisfied","explanation":"done","criteria":[]}`)},
		{name: "failed", want: RubricFailed, structured: json.RawMessage(`{"result":"failed","explanation":"cannot satisfy","criteria":[]}`)},
		{name: "grader_error", want: RubricGraderError, graderErr: errors.New("grader unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grader := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{
				Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: test.structured}, Error: test.graderErr,
			})
			middleware := Rubric(grader, RubricOptions{})
			saver := dacheckpoint.NewMemorySaver()
			compiled := NewAgent(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}), WithMiddleware(middleware), WithSaver(saver), WithoutSubagents(), WithoutSummary())
			config := dacheckpoint.Config{ThreadID: "rubric-terminal-" + test.name}
			result, err := compiled.Invoke(t.Context(), dagent.Input{
				Config: config, Messages: []damessage.Message{damessage.Human("question")}, State: map[string]any{RubricKey: "Be correct"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.State[RubricStatusKey] != string(test.want) {
				t.Fatalf("result rubric status = %#v", result.State[RubricStatusKey])
			}
			snapshot, err := compiled.State(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.State[RubricStatusKey] != string(test.want) {
				t.Fatalf("checkpoint rubric status = %#v", snapshot.State[RubricStatusKey])
			}
		})
	}
}

func TestRubricGraderFailureIsRecordedWithoutReplacingAnswer(t *testing.T) {
	grader := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{Error: errors.New("grader unavailable")})
	var evaluation RubricEvaluation
	middleware := Rubric(grader, RubricOptions{OnEvaluation: func(value RubricEvaluation) { evaluation = value }})
	primary := modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}})
	compiled := NewAgent(primary, WithMiddleware(middleware), WithoutSubagents(), WithoutSummary())

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
