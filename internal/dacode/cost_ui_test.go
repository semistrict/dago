package dacode

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/damessage"
)

func TestCostCommandLoadsDurableModelPurposeCacheAndUnpricedBreakdown(t *testing.T) {
	runner := &costUIRunner{fakeRunner: &fakeRunner{}, report: detailedCostUIReport()}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-cost", false, false, "")
	model.resize(100, 30)
	command, handled := model.slashCommand("/cost")
	if !handled || command == nil {
		t.Fatalf("handled=%t command=%v", handled, command)
	}
	message := command()
	model.Update(message)
	text := model.items[len(model.items)-1].text
	for _, expected := range []string{
		"Estimated thread cost: $0.0015",
		"2 recorded requests • 1.5k input • 300 output tokens",
		"Cache • 400 read • 50 written",
		"1 request could not be priced.",
		"By model", "openai:gpt-5.6-terra", "local:free-model", "unpriced",
		"By purpose", "Assistant", "Subagents", "Model time: 1.5s",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cost report missing %q:\n%s", expected, text)
		}
	}
	if status := ansi.Strip(model.renderStatus()); !strings.Contains(status, "$0.0015") {
		t.Fatalf("status missing cost:\n%s", status)
	}
}

func TestTokenCommandIncludesSessionAndCacheTotalsFromReport(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-cost", false, false, "")
	model.setLastUsage(damessage.Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150})
	model.costStats = sessionCostState{loaded: true, report: detailedCostUIReport()}
	result := model.tokenUsageSummary()
	for _, expected := range []string{"Session: 1.5k input · 300 output", "Cache: 400 read · 50 written"} {
		if !strings.Contains(result, expected) {
			t.Fatalf("token report missing %q:\n%s", expected, result)
		}
	}
}

func TestCostReportIgnoresStaleThreadAndGenerationResults(t *testing.T) {
	runner := &costUIRunner{fakeRunner: &fakeRunner{}, report: detailedCostUIReport()}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-a", false, false, "")
	first := model.requestCostReport(false)
	second := model.requestCostReport(false)
	model.applyCostReport(first().(sessionCostMsg))
	if model.costStats.loaded {
		t.Fatal("stale generation replaced current stats")
	}
	model.threadID = "thread-b"
	model.applyCostReport(second().(sessionCostMsg))
	if model.costStats.loaded {
		t.Fatal("stale thread replaced current stats")
	}
}

func TestCostReportFailureIsGenericAndPreservesPriorSnapshot(t *testing.T) {
	runner := &costUIRunner{fakeRunner: &fakeRunner{}, err: errors.New("api_key=never-print-this")}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-cost", false, false, "")
	model.costStats = sessionCostState{loaded: true, report: detailedCostUIReport()}
	command, _ := model.slashCommand("/cost")
	model.Update(command())
	last := model.items[len(model.items)-1].text
	if last != "Thread usage is temporarily unavailable." || strings.Contains(last, "never-print-this") || !model.costStats.loaded || model.costStats.report.RequestCount != 2 {
		t.Fatalf("failure state = %q %#v", last, model.costStats)
	}
}

func TestExitUsageSummaryIsEmptyUntilUsageAndThenDeterministic(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-cost", false, false, "")
	if got := model.exitUsageSummary(); got != "" {
		t.Fatalf("empty exit summary = %q", got)
	}
	model.costStats = sessionCostState{loaded: true, report: detailedCostUIReport()}
	first, second := model.exitUsageSummary(), model.exitUsageSummary()
	if first != second || !strings.HasPrefix(first, "Session usage\nEstimated thread cost: $0.0015") {
		t.Fatalf("exit summary = %q", first)
	}
}

func TestCostPricingFailureKeepsProviderReportedCostsVisible(t *testing.T) {
	report := detailedCostUIReport()
	result := formatSessionCostReport(report, true)
	if !strings.Contains(result, "$0.0015") || !strings.Contains(result, "Pricing catalog unavailable; provider-reported costs are still shown.") {
		t.Fatalf("pricing fallback = %s", result)
	}
}

type costUIRunner struct {
	*fakeRunner
	report     dacost.Report
	err        error
	pricingErr error
}

func (runner *costUIRunner) CostReport(context.Context, string) (dacost.Report, error) {
	return runner.report, runner.err
}

func (runner *costUIRunner) CostPricingError() error { return runner.pricingErr }

func detailedCostUIReport() dacost.Report {
	return dacost.Report{
		Version: 1, RequestCount: 2, InputTokens: 1_500, OutputTokens: 300,
		CacheReadTokens: 400, CacheWriteTokens: 50, CostUSD: 0.0015,
		PricedRequestCount: 1, UnpricedRequestCount: 1, WallTimeSeconds: 1.5,
		Models: []dacost.ModelStats{
			{Provider: "local", Model: "free-model", RequestCount: 1, InputTokens: 500, OutputTokens: 100},
			{Provider: "openai", Model: "gpt-5.6-terra", RequestCount: 1, InputTokens: 1_000, OutputTokens: 200, CostUSD: 0.0015, PricedRequestCount: 1},
		},
		Purposes: []dacost.PurposeStats{
			{Purpose: dacost.PurposeAssistant, RequestCount: 1, InputTokens: 1_000, OutputTokens: 200, CostUSD: 0.0015, PricedRequestCount: 1},
			{Purpose: dacost.PurposeSubagent, RequestCount: 1, InputTokens: 500, OutputTokens: 100},
		},
	}
}
