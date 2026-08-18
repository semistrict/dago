package dacode

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dacost"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const maximumCostReportModelRows = 16

type sessionCostState struct {
	report             dacost.Report
	loaded             bool
	pricingUnavailable bool
	generation         uint64
}

type sessionCostMsg struct {
	threadID           string
	generation         uint64
	report             dacost.Report
	display            bool
	pricingUnavailable bool
	err                error
}

func (model *tuiModel) requestCostReport(display bool) tea.Cmd {
	reporter, ok := model.runner.(agentCostReporter)
	if !ok {
		return nil
	}
	model.costStats.generation++
	generation := model.costStats.generation
	threadID := model.threadID
	return func() tea.Msg {
		report, err := reporter.CostReport(model.ctx, threadID)
		return sessionCostMsg{
			threadID: threadID, generation: generation, report: report, display: display,
			pricingUnavailable: reporter.CostPricingError() != nil, err: err,
		}
	}
}

func (model *tuiModel) applyCostReport(message sessionCostMsg) {
	if message.threadID != model.threadID || message.generation != model.costStats.generation {
		return
	}
	if message.err != nil {
		if message.display {
			model.appendItem(transcriptItem{kind: itemError, text: "Thread usage is temporarily unavailable."})
			model.refreshTranscript()
		}
		return
	}
	model.costStats.report = message.report
	model.costStats.loaded = true
	model.costStats.pricingUnavailable = message.pricingUnavailable
	if message.display {
		model.appendItem(transcriptItem{kind: itemNotice, text: formatSessionCostReportWithGlyphs(message.report, message.pricingUnavailable, model.glyphs)})
	}
	model.refreshTranscript()
}

func formatSessionCostReport(report dacost.Report, pricingUnavailable bool) string {
	return formatSessionCostReportWithGlyphs(report, pricingUnavailable, unicodeUIGlyphs)
}

func formatSessionCostReportWithGlyphs(report dacost.Report, pricingUnavailable bool, glyphs uiGlyphs) string {
	if report.RequestCount == 0 {
		return "No model usage recorded for this thread yet."
	}
	requestLabel := "requests"
	if report.RequestCount == 1 {
		requestLabel = "request"
	}
	lines := make([]string, 0, 12+len(report.Models)+len(report.Purposes))
	if report.PricedRequestCount > 0 {
		lines = append(lines, "Estimated thread cost: "+formatCost(report.CostUSD))
	} else {
		lines = append(lines, "Cost estimate unavailable")
	}
	separator := " " + glyphs.Separator + " "
	lines = append(lines, "", fmt.Sprintf("%d recorded %s%s%s input%s%s output tokens",
		report.RequestCount, requestLabel, separator, formatTokenCount64(report.InputTokens), separator, formatTokenCount64(report.OutputTokens)))
	if report.CacheReadTokens > 0 || report.CacheWriteTokens > 0 {
		lines = append(lines, fmt.Sprintf("Cache%s%s read%s%s written",
			separator, formatTokenCount64(report.CacheReadTokens), separator, formatTokenCount64(report.CacheWriteTokens)))
	}
	if report.UnpricedRequestCount > 0 {
		lines = append(lines, fmt.Sprintf("%d %s could not be priced.", report.UnpricedRequestCount, pluralizeCostRequest(report.UnpricedRequestCount)))
	}
	if pricingUnavailable {
		lines = append(lines, "Pricing catalog unavailable; provider-reported costs are still shown.")
	}
	if len(report.Models) > 0 {
		lines = append(lines, "", "By model")
		limit := min(len(report.Models), maximumCostReportModelRows)
		for _, row := range report.Models[:limit] {
			label := boundedCostLabel(row.Provider+":"+row.Model, 96)
			if row.Provider == "" {
				label = boundedCostLabel(row.Model, 96)
			}
			lines = append(lines, fmt.Sprintf("- %s%s%d req%s%s in%s%s out%s%s",
				label, separator, row.RequestCount, separator, formatTokenCount64(row.InputTokens), separator, formatTokenCount64(row.OutputTokens), separator, formatCostAvailability(row.CostUSD, row.PricedRequestCount)))
		}
		if omitted := len(report.Models) - limit; omitted > 0 {
			lines = append(lines, fmt.Sprintf("- +%d more model rows", omitted))
		}
	}
	if len(report.Purposes) > 0 {
		lines = append(lines, "", "By purpose")
		for _, row := range report.Purposes {
			lines = append(lines, fmt.Sprintf("- %s%s%d req%s%s in%s%s out%s%s",
				costPurposeLabel(row.Purpose), separator, row.RequestCount, separator, formatTokenCount64(row.InputTokens), separator, formatTokenCount64(row.OutputTokens), separator, formatCostAvailability(row.CostUSD, row.PricedRequestCount)))
		}
	}
	if report.WallTimeSeconds > 0 {
		lines = append(lines, "", "Model time: "+formatCostDuration(report.WallTimeSeconds))
	}
	return strings.Join(lines, "\n")
}

func (model *tuiModel) statusCost() (string, bool) {
	if !model.costStats.loaded || model.costStats.report.RequestCount == 0 || model.costStats.report.PricedRequestCount == 0 {
		return "", false
	}
	return formatCost(model.costStats.report.CostUSD), true
}

func (model *tuiModel) exitUsageSummary() string {
	if !model.costStats.loaded || model.costStats.report.RequestCount == 0 {
		return ""
	}
	return "Session usage\n" + formatSessionCostReportWithGlyphs(model.costStats.report, model.costStats.pricingUnavailable, model.glyphs)
}

func formatCostAvailability(cost float64, priced int) string {
	if priced == 0 {
		return "unpriced"
	}
	return formatCost(cost)
}

func pluralizeCostRequest(count int) string {
	if count == 1 {
		return "request"
	}
	return "requests"
}

func costPurposeLabel(purpose dacost.Purpose) string {
	switch purpose {
	case dacost.PurposeAssistant:
		return "Assistant"
	case dacost.PurposeSubagent:
		return "Subagents"
	case dacost.PurposeOffload:
		return "Offload"
	case dacost.PurposeAuto:
		return "Auto mode"
	default:
		return "Other"
	}
}

func formatTokenCount64(count int64) string {
	if count <= 0 {
		return "0"
	}
	if count < 1_000 {
		return fmt.Sprintf("%d", count)
	}
	value := float64(count) / 1_000
	if count%1_000 == 0 {
		return fmt.Sprintf("%.0fk", value)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", value), "0"), ".") + "k"
}

func boundedCostLabel(value string, maximum int) string {
	value = unicodesecurity.RenderTerminalSafe(value)
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	if len(value) > maximum {
		value = truncate(value, maximum)
	}
	return value
}

func formatCostDuration(seconds float64) string {
	duration := time.Duration(seconds * float64(time.Second))
	if duration < time.Second {
		return duration.Round(time.Millisecond).String()
	}
	return duration.Round(100 * time.Millisecond).String()
}
