package dago

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/serde"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/darepository"
	"github.com/semistrict/dago/dastate"
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
	compiled := New(primary, WithFilesystem(Filesystem{}), WithMiddleware(middleware))

	result, err := compiled.Invoke(context.Background(), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}))
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

func TestRubricCheckpointsGraderUsagePrivately(t *testing.T) {
	graded := damessage.Assistant("graded")
	graded.Usage = &damessage.Usage{InputTokens: 25, OutputTokens: 5, Provider: "test", Model: "grader"}
	grader := modeltest.New(damodel.Profile{StructuredOutput: true}, modeltest.Step{Response: damodel.Response{
		Message: graded, Structured: json.RawMessage(`{"result":"satisfied","explanation":"done","criteria":[]}`),
	}})
	saver := dacheckpoint.NewMemorySaver()
	agent := New(
		modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}),
		WithMiddleware(Rubric(grader, RubricOptions{})), WithSaver(saver),
	)
	result, err := agent.Invoke(t.Context(), dagent.FromCheckpoint(dacheckpoint.Config{ThreadID: "rubric-usage"}), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}))
	if err != nil {
		t.Fatal(err)
	}
	var usage []damessage.PurposedUsage
	for _, message := range result.Messages {
		if message.Role == damessage.RoleAssistant {
			usage = message.OtherUsage
		}
	}
	if len(usage) != 1 || usage[0].Purpose != "assistant" || usage[0].InputTokens != 25 || usage[0].Model != "grader" {
		t.Fatalf("rubric usage = %#v", usage)
	}
}

func TestRubricWithRepositoryExposesOnlyBoundedReadTools(t *testing.T) {
	backend := dabackend.NewMemory(map[string]dabackend.FileData{"/repo/result.txt": {Content: "verified", Encoding: dabackend.EncodingUTF8}})
	grader := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true},
		modeltest.Step{Check: func(request damodel.Request) error {
			for _, tool := range request.Tools {
				if tool.Name == "write_file" || tool.Name == "execute" {
					return errors.New("grader received mutation authority")
				}
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"/repo/result.txt"}`)}}}}},
		modeltest.Step{Check: func(request damodel.Request) error {
			if !strings.Contains(request.Messages[len(request.Messages)-1].TextContent(), "verified") {
				return errors.New("repository evidence missing")
			}
			return nil
		}, Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"satisfied","explanation":"verified","criteria":[]}`)}},
	)
	middleware := RubricWithRepository(grader, backend, darepository.Options{Root: "/repo"}, RubricOptions{})
	compiled := New(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}), WithMiddleware(middleware))
	result, err := compiled.Invoke(t.Context(), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Verify the result"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.State[RubricStatusKey] != string(RubricSatisfied) {
		t.Fatalf("status = %#v", result.State[RubricStatusKey])
	}
}

func TestRubricRetriesOneTransientGraderTransportFailure(t *testing.T) {
	grader := modeltest.New(damodel.Profile{StructuredOutput: true},
		modeltest.Step{Error: io.ErrUnexpectedEOF},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"satisfied","explanation":"done","criteria":[]}`)}},
	)
	middleware := Rubric(grader, RubricOptions{})
	compiled := New(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}), WithMiddleware(middleware))
	result, err := compiled.Invoke(t.Context(), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.State[RubricStatusKey] != string(RubricSatisfied) || grader.Remaining() != 0 {
		t.Fatalf("status=%#v remaining=%d", result.State[RubricStatusKey], grader.Remaining())
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
	compiled := New(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}), WithFilesystem(Filesystem{}), WithMiddleware(middleware))
	_, err := compiled.Invoke(t.Context(), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}), dagent.WithConfigurable(map[string]any{"tenant": "request-tenant"}))
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
	compiled := New(primary, WithFilesystem(Filesystem{}), WithMiddleware(middleware), WithSaver(saver))
	config := dacheckpoint.Config{ThreadID: "rubric-limit"}
	result, err := compiled.Invoke(context.Background(), dagent.FromCheckpoint(config), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}))
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

func TestRubricMiddlewareReadsDynamicIterationLimit(t *testing.T) {
	limit := 2
	grader := modeltest.New(damodel.Profile{StructuredOutput: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("graded"), Structured: json.RawMessage(`{"result":"needs_revision","explanation":"again","criteria":[{"name":"correct","passed":false,"gap":"fix"}]}`)}},
	)
	middleware := Rubric(grader, RubricOptions{MaxIterationsFunc: func() int { return limit }})
	limit = 1
	agent := New(
		modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}),
		WithMiddleware(middleware),
	)
	result, err := agent.Invoke(t.Context(), dagent.Prompt("question"), dagent.WithState(dastate.Values{RubricKey: "Be correct"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.State[RubricStatusKey] != string(RubricMaxIterations) {
		t.Fatalf("status = %#v", result.State[RubricStatusKey])
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
			compiled := New(modeltest.New(damodel.Profile{}, modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("answer")}}), WithFilesystem(Filesystem{}), WithMiddleware(middleware), WithSaver(saver))
			config := dacheckpoint.Config{ThreadID: "rubric-terminal-" + test.name}
			result, err := compiled.Invoke(t.Context(), dagent.FromCheckpoint(config), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}))
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
	compiled := New(primary, WithFilesystem(Filesystem{}), WithMiddleware(middleware))

	result, err := compiled.Invoke(context.Background(), dagent.Prompt("question"), dagent.WithState(map[string]any{RubricKey: "Be correct"}))
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

func TestRubricSnapshotFromStateIsDetachedAndFailsClosed(t *testing.T) {
	values := dastate.Values{
		RubricKey:            "  - tests pass  ",
		RubricStatusKey:      string(RubricNeedsRevision),
		RubricIterationsKey:  2,
		RubricEvaluationsKey: []RubricEvaluation{{GradingRunID: "run", Iteration: 1, Result: RubricNeedsRevision, Criteria: []RubricCriterionEvaluation{{Name: "tests", Gap: "one fails"}}}},
	}
	snapshot := RubricSnapshotFromState(values)
	if snapshot.Criteria != "- tests pass" || snapshot.Status != RubricNeedsRevision || snapshot.Iterations != 2 || len(snapshot.Evaluations) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	snapshot.Evaluations[0].Criteria[0].Gap = "changed"
	original := values[RubricEvaluationsKey].([]RubricEvaluation)
	if original[0].Criteria[0].Gap != "one fails" {
		t.Fatalf("snapshot aliases checkpoint state: %#v", original)
	}

	malformed := RubricSnapshotFromState(dastate.Values{
		RubricKey:           12,
		RubricStatusKey:     "future_status",
		RubricIterationsKey: -3,
	})
	if malformed.Criteria != "" || malformed.Status != "" || malformed.Iterations != 0 || len(malformed.Evaluations) != 0 {
		t.Fatalf("malformed snapshot = %#v", malformed)
	}
}
