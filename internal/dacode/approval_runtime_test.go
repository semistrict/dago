package dacode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	_ "modernc.org/sqlite"
)

func newApprovalRuntime(t *testing.T, model damodel.Chat) *dagoRunner {
	t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "auto.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := setupAutoClassifierCounters(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	backend, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := newApprovalReviewer(model, backend)
	return &dagoRunner{database: database, reviewer: reviewer, mainReviewer: reviewer, reviewBackend: backend}
}

func approvalRuntimeRequest(thread, turn string, ids ...string) approvalReviewRequest {
	requests := make([]dagent.ApprovalRequest, 0, len(ids))
	for _, id := range ids {
		requests = append(requests, dagent.ApprovalRequest{Call: damessage.ToolCall{
			ID: id, Name: "execute", Arguments: json.RawMessage(`{"command":"go test ./..."}`),
		}})
	}
	return approvalReviewRequest{ThreadID: thread, TurnID: turn, Mode: "auto", WorkingDir: "/work", Requests: requests}
}

func approvalBatchResponse(decisions ...approvalAssessment) damodel.Response {
	payload, _ := json.Marshal(approvalAssessmentBatch{Decisions: decisions})
	return approvalToolResponse(payload)
}

func approvalToolResponse(payload json.RawMessage) damodel.Response {
	message := damessage.Assistant("")
	message.ToolCalls = []damessage.ToolCall{{ID: "approval-result", Name: "approval_assessment", Arguments: payload}}
	return damodel.Response{Message: message}
}

func TestApprovalReviewerRequiresRepairableStructuredDecisionTool(t *testing.T) {
	checkToolRequest := func(request damodel.Request) error {
		if request.ResponseFormat != nil {
			return errors.New("approval reviewer used provider-native JSON")
		}
		if request.ToolChoice == nil || request.ToolChoice.Mode != "required" {
			return errors.New("approval decision tool was not required")
		}
		for _, tool := range request.Tools {
			if tool.Name == "approval_assessment" {
				return nil
			}
		}
		return errors.New("approval decision tool is missing")
	}
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true},
		modeltest.Step{
			Check:    checkToolRequest,
			Response: approvalToolResponse(json.RawMessage(`{"decisions":[{"tool_call_id":"execute"}]}`)),
		},
		modeltest.Step{
			Check: func(request damodel.Request) error {
				if err := checkToolRequest(request); err != nil {
					return err
				}
				if len(request.Messages) == 0 {
					return errors.New("structured correction feedback is missing")
				}
				last := request.Messages[len(request.Messages)-1]
				if last.Role != damessage.RoleTool || last.ToolStatus != damessage.ToolStatusError || !strings.Contains(last.TextContent(), "validation failed") {
					return errors.New("structured correction feedback is invalid")
				}
				return nil
			},
			Response: approvalBatchResponse(approvalAssessment{
				ToolCallID: "execute", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized.",
			}),
		},
	)
	runner := newApprovalRuntime(t, model)
	result, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "execute"))
	if err != nil || !result.Assessments["execute"].approved() {
		t.Fatalf("review = %#v, %v", result, err)
	}
}

func TestAutomaticReviewerClassifiesOneExactBatchAndRetainsMixedDenial(t *testing.T) {
	var calls atomic.Int32
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			calls.Add(1)
			if len(request.Messages) == 0 || !containsAll(request.Messages[len(request.Messages)-1].TextContent(), `"tool_call_id": "allow"`, `"tool_call_id": "deny"`) {
				return errors.New("batch prompt omitted a tool-call ID")
			}
			return nil
		},
		Response: approvalBatchResponse(
			approvalAssessment{ToolCallID: "allow", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized."},
			approvalAssessment{ToolCallID: "deny", RiskLevel: "high", UserAuthorization: "low", Outcome: "deny", Rationale: "Not authorized."},
		),
	})
	runner := newApprovalRuntime(t, model)
	result, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn-1", "allow", "deny"))
	if err != nil || len(result.Assessments) != 2 || calls.Load() != 1 {
		t.Fatalf("review = %#v, %v; calls = %d", result, err, calls.Load())
	}
	counters, err := runner.loadAutoClassifierCounters(t.Context(), "thread")
	if err != nil || counters.ConsecutiveDenials != 1 || counters.TotalDenials != 1 {
		t.Fatalf("counters = %#v, %v", counters, err)
	}
}

func TestBackgroundApprovalReviewUsesExactBatchContract(t *testing.T) {
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			if len(request.Messages) == 0 || !containsAll(request.Messages[len(request.Messages)-1].TextContent(), `"tool_call_id": "allow"`, `"tool_call_id": "deny"`) {
				return errors.New("batch prompt omitted a tool-call ID")
			}
			return nil
		},
		Response: approvalBatchResponse(
			approvalAssessment{ToolCallID: "allow", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized."},
			approvalAssessment{ToolCallID: "deny", RiskLevel: "high", UserAuthorization: "low", Outcome: "deny", Rationale: "Not authorized."},
		),
	})
	runner := newApprovalRuntime(t, model)
	result, err := reviewApprovals(t.Context(), runner.reviewer, approvalRuntimeRequest("", "", "allow", "deny"))
	if err != nil || len(result.Assessments) != 2 {
		t.Fatalf("review = %#v, %v", result, err)
	}
	if !result.Assessments["allow"].approved() || result.Assessments["deny"].approved() {
		t.Fatalf("assessments = %#v", result.Assessments)
	}
}

func TestAutomaticReviewerRejectsIncompleteAndDuplicateBatches(t *testing.T) {
	requests := approvalRuntimeRequest("thread", "turn", "one", "two").Requests
	for name, batch := range map[string]approvalAssessmentBatch{
		"incomplete": {Decisions: []approvalAssessment{{ToolCallID: "one", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "ok"}}},
		"duplicate": {Decisions: []approvalAssessment{
			{ToolCallID: "one", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "ok"},
			{ToolCallID: "one", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "ok"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateApprovalAssessmentBatch(batch, requests); err == nil {
				t.Fatal("invalid exact-ID coverage accepted")
			}
		})
	}
}

func TestAutomaticReviewerMalformedResultConsumesUnavailableBudget(t *testing.T) {
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{Response: approvalBatchResponse(
		approvalAssessment{ToolCallID: "unknown", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized."},
	)})
	runner := newApprovalRuntime(t, model)
	result, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "expected"))
	if err != nil || result.Assessments["expected"].Outcome != "deny" {
		t.Fatalf("malformed result = %#v, %v", result, err)
	}
	counters, err := runner.loadAutoClassifierCounters(t.Context(), "thread")
	if err != nil || counters.ConsecutiveUnavailable != 1 || counters.LastBatchID == "" {
		t.Fatalf("malformed counters = %#v, %v", counters, err)
	}
}

func TestAutomaticReviewerEscalatesTwentiethDenialInSameBatch(t *testing.T) {
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{Response: approvalBatchResponse(
		approvalAssessment{ToolCallID: "twenty", RiskLevel: "high", UserAuthorization: "low", Outcome: "deny", Rationale: "Not authorized."},
	)})
	runner := newApprovalRuntime(t, model)
	if err := runner.saveAutoClassifierCounters(t.Context(), "thread", autoClassifierCounters{TotalDenials: 19, LastMode: "auto", LastTurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "twenty")); err == nil {
		t.Fatal("twentieth denial did not require human approval")
	}
	counters, _ := runner.loadAutoClassifierCounters(t.Context(), "thread")
	if counters.TotalDenials != 20 {
		t.Fatalf("total denials = %d", counters.TotalDenials)
	}
}

func TestAutomaticReviewerUnavailableThresholdFallsBackToHuman(t *testing.T) {
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true},
		modeltest.Step{Error: errors.New("temporary provider failure")},
		modeltest.Step{Error: errors.New("temporary provider failure")},
	)
	runner := newApprovalRuntime(t, model)
	for _, id := range []string{"first", "second"} {
		result, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", id))
		if err != nil || result.Assessments[id].Outcome != "deny" {
			t.Fatalf("unavailable %s = %#v, %v", id, result, err)
		}
	}
	if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "third")); err == nil {
		t.Fatal("third unavailable batch did not require human approval")
	}
	if remaining := model.Remaining(); remaining != 0 {
		t.Fatalf("classifier invoked past threshold; remaining scripted calls = %d", remaining)
	}
}

func TestAutomaticReviewerMigratesUnavailableStateToInheritedMainModel(t *testing.T) {
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{
		Response: approvalBatchResponse(approvalAssessment{
			ToolCallID: "retry", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized.",
		}),
	})
	runner := newApprovalRuntime(t, model)
	if err := runner.saveAutoClassifierCounters(t.Context(), "thread", autoClassifierCounters{
		ConsecutiveUnavailable: autoUnavailableFallback,
		LastMode:               "auto",
		LastTurnID:             "turn",
		LastBatchID:            "legacy-batch",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "retry"))
	if err != nil || !result.Assessments["retry"].approved() {
		t.Fatalf("review after migration = %#v, %v", result, err)
	}
	counters, err := runner.loadAutoClassifierCounters(t.Context(), "thread")
	if err != nil || counters.ClassifierIdentity != inheritedAutoClassifierIdentity || counters.ConsecutiveUnavailable != 0 {
		t.Fatalf("migrated counters = %#v, %v", counters, err)
	}
}

func TestAutomaticReviewerCounterReadAndWriteFailuresRequireHuman(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		runner := newApprovalRuntime(t, modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}))
		if err := runner.database.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "read")); err == nil {
			t.Fatal("counter read failure did not require human approval")
		}
	})
	t.Run("write", func(t *testing.T) {
		var runner *dagoRunner
		model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{
			Check: func(damodel.Request) error { return runner.database.Close() },
			Response: approvalBatchResponse(approvalAssessment{
				ToolCallID: "write", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized.",
			}),
		})
		runner = newApprovalRuntime(t, model)
		if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "write")); err == nil {
			t.Fatal("counter write failure did not require human approval")
		}
	})
}

func TestAutomaticReviewerNewTurnAndExplicitResetClearConsecutiveFallbacks(t *testing.T) {
	model := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true},
		modeltest.Step{Response: approvalBatchResponse(approvalAssessment{ToolCallID: "new-turn", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized."})},
		modeltest.Step{Response: approvalBatchResponse(approvalAssessment{ToolCallID: "reset", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized."})},
	)
	runner := newApprovalRuntime(t, model)
	initial := autoClassifierCounters{ConsecutiveDenials: 3, LastMode: "auto", LastTurnID: "old"}
	if err := runner.saveAutoClassifierCounters(t.Context(), "thread", initial); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "new", "new-turn")); err != nil {
		t.Fatal(err)
	}
	counters, _ := runner.loadAutoClassifierCounters(t.Context(), "thread")
	if counters.ConsecutiveDenials != 0 || counters.ConsecutiveUnavailable != 0 {
		t.Fatalf("new-turn counters = %#v", counters)
	}
	counters.ConsecutiveDenials = 2
	counters.ConsecutiveUnavailable = 2
	if err := runner.saveAutoClassifierCounters(t.Context(), "thread", counters); err != nil {
		t.Fatal(err)
	}
	request := approvalRuntimeRequest("thread", "new", "reset")
	request.Reset = true
	if _, err := runner.Review(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	counters, _ = runner.loadAutoClassifierCounters(t.Context(), "thread")
	if counters.ConsecutiveDenials != 0 || counters.ConsecutiveUnavailable != 0 {
		t.Fatalf("explicit-reset counters = %#v", counters)
	}
}

func TestAutomaticReviewerRetriesConfigFaultAndCancellationDoesNotLatch(t *testing.T) {
	base := modeltest.New(damodel.Profile{ToolCalling: true, StructuredOutput: true}, modeltest.Step{Response: approvalBatchResponse(
		approvalAssessment{ToolCallID: "recovered", RiskLevel: "low", UserAuthorization: "high", Outcome: "allow", Rationale: "Authorized."},
	)})
	runner := newApprovalRuntime(t, base)
	var calls atomic.Int32
	runner.reviewerSpec = "provider:model"
	runner.reviewer = nil
	runner.reviewerModel = func(_ context.Context, _ string) (damodel.Chat, error) {
		calls.Add(1)
		return nil, errors.New("model unavailable")
	}
	first := approvalRuntimeRequest("thread", "turn", "first")
	if result, err := runner.Review(t.Context(), first); err != nil || result.Assessments["first"].Outcome != "deny" {
		t.Fatalf("first fault = %#v, %v", result, err)
	}
	if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "second")); err == nil || calls.Load() != 2 {
		t.Fatalf("repeat fault err = %v; construction calls = %d", err, calls.Load())
	}
	runner.reviewerModel = func(_ context.Context, _ string) (damodel.Chat, error) { return base, nil }
	if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "recovered")); err != nil {
		t.Fatal(err)
	}
	counters, _ := runner.loadAutoClassifierCounters(t.Context(), "thread")
	if counters.ClassifierConfigFailedSpec != "" {
		t.Fatalf("config latch not cleared: %#v", counters)
	}

	beforeCancel, _ := runner.loadAutoClassifierCounters(t.Context(), "thread")
	runner.reviewerModel = func(_ context.Context, _ string) (damodel.Chat, error) { return nil, context.Canceled }
	if _, err := runner.Review(t.Context(), approvalRuntimeRequest("thread", "turn", "cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("construction cancellation = %v", err)
	}
	counters, _ = runner.loadAutoClassifierCounters(t.Context(), "thread")
	if counters.ClassifierConfigFailedSpec != "" || counters.LastBatchID != beforeCancel.LastBatchID {
		t.Fatalf("cancellation mutated counters: %#v", counters)
	}
}

func TestValidateAutoClassifierPreservesCallerCancellation(t *testing.T) {
	runner := &dagoRunner{reviewerModel: func(ctx context.Context, _ string) (damodel.Chat, error) { return nil, ctx.Err() }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runner.ValidateAutoClassifier(ctx, "provider:model"); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation cancellation = %v", err)
	}
}

func TestAutoClassifierSuccessfulResultRequiresNonErrorToolStatus(t *testing.T) {
	allowed := map[string]struct{}{"call": {}}
	success := damessage.Tool("call", "done")
	failure := damessage.Tool("call", "failed")
	failure.ToolStatus = damessage.ToolStatusError
	if !autoClassifierHasSuccessfulResult([]damessage.Message{success}, allowed) {
		t.Fatal("successful allowed tool result did not reset counters")
	}
	if autoClassifierHasSuccessfulResult([]damessage.Message{failure}, allowed) {
		t.Fatal("failed allowed tool result reset counters")
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
