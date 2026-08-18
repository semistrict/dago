package dacost

import (
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
)

func TestReportMessagesRestoresParentAndTransferredUsage(t *testing.T) {
	started := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	messages := []damessage.Message{
		{Role: damessage.RoleAssistant, Usage: &damessage.Usage{
			InputTokens: 100, OutputTokens: 20, Provider: "", Model: "",
			StartedAt: started, FinishedAt: started.Add(2 * time.Second),
		}},
		{Role: damessage.RoleTool, OtherUsage: []damessage.PurposedUsage{
			{Purpose: "subagent", Usage: damessage.Usage{InputTokens: 50, OutputTokens: 10, Provider: "test", Model: "child"}},
			{Purpose: "summary", Usage: damessage.Usage{InputTokens: 30, OutputTokens: 5, Provider: "test", Model: "summary"}},
		}},
	}
	estimator := EstimatorFunc(func(provider, model string, usage damessage.Usage) (float64, bool) {
		if provider != "test" {
			t.Fatalf("provider = %q", provider)
		}
		return float64(usage.InputTokens+usage.OutputTokens) / 1000, true
	})
	report, err := ReportMessages(messages, estimator, MessageOptions{FallbackProvider: "test", FallbackModel: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if report.RequestCount != 3 || report.InputTokens != 180 || report.OutputTokens != 35 || report.PricedRequestCount != 3 {
		t.Fatalf("report = %#v", report)
	}
	if report.WallTimeSeconds != 0 {
		t.Fatalf("reconstructed wall time = %v", report.WallTimeSeconds)
	}
	if len(report.Models) != 3 || len(report.Purposes) != 3 {
		t.Fatalf("models = %#v, purposes = %#v", report.Models, report.Purposes)
	}
	if report.Purposes[0].Purpose != PurposeAssistant || report.Purposes[1].Purpose != PurposeSubagent || report.Purposes[2].Purpose != PurposeOffload {
		t.Fatalf("purposes = %#v", report.Purposes)
	}
}

func TestReportMessagesDoesNotApplyParentFallbackToNestedUsage(t *testing.T) {
	report, err := ReportMessages([]damessage.Message{{Role: damessage.RoleTool, OtherUsage: []damessage.PurposedUsage{{
		Purpose: "custom-side-call", Usage: damessage.Usage{InputTokens: 10, OutputTokens: 2},
	}}}}, EstimatorFunc(func(provider, model string, _ damessage.Usage) (float64, bool) {
		if provider != "" || model != "" {
			t.Fatalf("nested fallback = %q:%q", provider, model)
		}
		return 0, false
	}), MessageOptions{FallbackProvider: "parent-provider", FallbackModel: "parent-model"})
	if err != nil {
		t.Fatal(err)
	}
	if report.RequestCount != 1 || report.PricedRequestCount != 0 || report.Purposes[0].Purpose != PurposeOffload {
		t.Fatalf("report = %#v", report)
	}
}

func TestTransferUsageFlattensNestedOwnershipAndClonesDetails(t *testing.T) {
	details := map[string]int{"cache_read": 4}
	messages := []damessage.Message{
		{Role: damessage.RoleAssistant, Usage: &damessage.Usage{InputTokens: 10, OutputTokens: 2, InputDetails: details}},
		{Role: damessage.RoleTool, OtherUsage: []damessage.PurposedUsage{{Purpose: "summarization", Usage: damessage.Usage{InputTokens: 7, OutputTokens: 1}}}},
	}
	transferred, err := TransferUsage(messages, PurposeSubagent, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(transferred) != 2 || transferred[0].Purpose != "subagent" || transferred[1].Purpose != "offload" {
		t.Fatalf("transferred = %#v", transferred)
	}
	details["cache_read"] = 999
	if transferred[0].InputDetails["cache_read"] != 4 {
		t.Fatalf("transfer aliased input details: %#v", transferred[0].InputDetails)
	}
}

func TestTransferUsageFailsClosedAtBound(t *testing.T) {
	messages := []damessage.Message{{Role: damessage.RoleAssistant, Usage: &damessage.Usage{InputTokens: 1}}, {Role: damessage.RoleAssistant, Usage: &damessage.Usage{InputTokens: 1}}}
	if _, err := TransferUsage(messages, PurposeSubagent, 1); err == nil {
		t.Fatal("expected bound error")
	}
}
