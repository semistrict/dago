package dacode

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
)

func TestRunnerCostReportRebuildsDurableParentAndSubagentUsage(t *testing.T) {
	child := modeltest.New(damodel.Profile{Provider: "test", Model: "child"}, modeltest.Step{Response: damodel.Response{Message: usageMessage("child done", 30, 5, "child")}})
	parent := modeltest.New(damodel.Profile{Provider: "test", Model: "parent", ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{
			Role:      damessage.RoleAssistant,
			ToolCalls: []damessage.ToolCall{{ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"work","subagent_type":"worker"}`)}},
			Usage:     &damessage.Usage{InputTokens: 20, OutputTokens: 4, Provider: "test", Model: "parent"},
		}}},
		modeltest.Step{Response: damodel.Response{Message: usageMessage("done", 40, 8, "parent")}},
	)
	saver := dacheckpoint.NewMemorySaver()
	agent := dago.New(parent,
		dago.WithSaver(saver),
		dago.WithSubagents(dago.NewSubagent("worker", "Worker", child, dago.WithSystemMessage(damessage.System("Work.")))),
	)
	if _, err := agent.Invoke(t.Context(), dagent.FromCheckpoint(dacheckpoint.Config{ThreadID: "thread-cost"}), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
	runner := &dagoRunner{
		agent: agent, profile: parent.Profile(),
		costEstimator: dacost.EstimatorFunc(func(_ string, _ string, usage damessage.Usage) (float64, bool) {
			return float64(usage.InputTokens+usage.OutputTokens) / 1000, true
		}),
	}
	report, err := runner.CostReport(context.Background(), "thread-cost")
	if err != nil {
		t.Fatal(err)
	}
	if report.RequestCount != 3 || report.InputTokens != 90 || report.OutputTokens != 17 || report.PricedRequestCount != 3 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Purposes) != 2 || report.Purposes[0].Purpose != dacost.PurposeAssistant || report.Purposes[1].Purpose != dacost.PurposeSubagent {
		t.Fatalf("purposes = %#v", report.Purposes)
	}
}

func TestRunnerCostReportMissingThreadHasUsefulEmptyDefault(t *testing.T) {
	agent := dago.New(modeltest.New(damodel.Profile{}, modeltest.Step{}), dago.WithSaver(dacheckpoint.NewMemorySaver()))
	report, err := (&dagoRunner{agent: agent}).CostReport(t.Context(), "missing")
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 || report.RequestCount != 0 || report.Models == nil || report.Purposes == nil {
		t.Fatalf("empty report = %#v", report)
	}
}

func TestRunnerOwnedUsagePersistsInParentCheckpoint(t *testing.T) {
	agent := dago.New(
		modeltest.New(damodel.Profile{Provider: "test", Model: "main"}, modeltest.Step{Response: damodel.Response{Message: usageMessage("done", 10, 2, "main")}}),
		dago.WithSaver(dacheckpoint.NewMemorySaver()),
	)
	if _, err := agent.Invoke(t.Context(), dagent.FromCheckpoint(dacheckpoint.Config{ThreadID: "owned"}), dagent.Prompt("go")); err != nil {
		t.Fatal(err)
	}
	runner := &dagoRunner{agent: agent}
	runner.recordOwnedUsage(t.Context(), "owned", []damessage.PurposedUsage{{
		Purpose: string(dacost.PurposeAuto),
		Usage:   damessage.Usage{InputTokens: 7, OutputTokens: 1, Provider: "test", Model: "reviewer"},
	}})
	report, err := runner.CostReport(t.Context(), "owned")
	if err != nil {
		t.Fatal(err)
	}
	if report.RequestCount != 2 || len(report.Purposes) != 2 || report.Purposes[1].Purpose != dacost.PurposeAuto {
		t.Fatalf("owned report = %#v", report)
	}
}

func TestRunnerPendingUsageIsBoundedAndCloned(t *testing.T) {
	runner := &dagoRunner{}
	usage := []damessage.PurposedUsage{{Purpose: string(dacost.PurposeAssistant), Usage: damessage.Usage{InputDetails: map[string]int{"cache_read": 3}}}}
	for index := 0; index < maximumPendingCostThreads+1; index++ {
		runner.queuePendingUsage(fmt.Sprintf("thread-%d", index), usage)
	}
	usage[0].Usage.InputDetails["cache_read"] = 99
	if len(runner.pendingUsage) != maximumPendingCostThreads {
		t.Fatalf("pending threads = %d", len(runner.pendingUsage))
	}
	stored := runner.takePendingUsage("thread-0")
	if len(stored) != 1 || stored[0].Usage.InputDetails["cache_read"] != 3 {
		t.Fatalf("pending usage = %#v", stored)
	}
}

func usageMessage(text string, input, output int, model string) damessage.Message {
	message := damessage.Assistant(text)
	message.Usage = &damessage.Usage{InputTokens: input, OutputTokens: output, Provider: "test", Model: model}
	return message
}
