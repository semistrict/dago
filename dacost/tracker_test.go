package dacost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
)

func TestTrackerRevisesChunkedRequestAndLateModelName(t *testing.T) {
	pricer := testPricer(map[string]Rates{
		"fallback": {InputMTok: new(1.0), OutputMTok: new(2.0)},
		"actual":   {InputMTok: new(3.0), OutputMTok: new(4.0)},
	})
	tracker := NewTracker(pricer, Options{})
	first, err := tracker.Record("request-1", Observation{
		Usage:            damessage.Usage{InputTokens: 100, OutputTokens: 10},
		FallbackProvider: "provider", FallbackModel: "fallback", Incremental: true,
	})
	if err != nil || !first.Recorded || first.RequestTokens != 110 {
		t.Fatalf("first Record() = %#v, %v", first, err)
	}
	second, err := tracker.Record("request-1", Observation{
		Usage: damessage.Usage{InputTokens: 20, OutputTokens: 5}, Incremental: true,
	})
	if err != nil || second.InputTokens != 20 || second.OutputTokens != 5 || second.RequestTokens != 135 {
		t.Fatalf("second Record() = %#v, %v", second, err)
	}
	late, err := tracker.Record("request-1", Observation{
		Usage: damessage.Usage{Provider: "provider", Model: "actual"}, Incremental: true,
	})
	if err != nil || !late.Recorded || late.InputTokens != 0 || late.OutputTokens != 0 || late.CostUSD == nil || *late.CostUSD <= 0 {
		t.Fatalf("late Record() = %#v, %v", late, err)
	}
	report := tracker.Report()
	if report.RequestCount != 1 || report.InputTokens != 120 || report.OutputTokens != 15 || len(report.Models) != 1 || report.Models[0].Model != "actual" {
		t.Fatalf("report = %#v", report)
	}
	wantCost := (120*3.0 + 15*4.0) / 1_000_000
	if math.Abs(report.CostUSD-wantCost) > 1e-12 || report.PricedRequestCount != 1 {
		t.Fatalf("cost = %.12f, priced = %d", report.CostUSD, report.PricedRequestCount)
	}
}

func TestTrackerAppliesNegativeCorrectionAndFinalizesRound(t *testing.T) {
	tracker := NewTracker(nil, Options{})
	if _, err := tracker.Record("google-1", Observation{
		Usage: damessage.Usage{InputTokens: 100, TotalTokens: 100}, Incremental: true,
	}); err != nil {
		t.Fatal(err)
	}
	delta, err := tracker.Record("google-1", Observation{
		Usage: damessage.Usage{InputTokens: -20, TotalTokens: -20}, Incremental: true,
	})
	if err != nil || delta.InputTokens != -20 || tracker.Report().InputTokens != 80 {
		t.Fatalf("correction = %#v, %v; report = %#v", delta, err, tracker.Report())
	}
	tracker.Finalize()
	replay, err := tracker.Record("google-1", Observation{
		Usage: damessage.Usage{OutputTokens: 5, TotalTokens: 5}, Incremental: true,
	})
	if err != nil || !replay.Replayed || tracker.Report().OutputTokens != 0 {
		t.Fatalf("replay = %#v, %v; report = %#v", replay, err, tracker.Report())
	}
}

func TestTrackerCompletedMessagesAreIdempotentAndPurposed(t *testing.T) {
	tracker := NewTracker(nil, Options{})
	cost := 0.0
	observations := []struct {
		id       string
		provider string
		model    string
		purpose  Purpose
	}{
		{"assistant", "openai", "same", PurposeAssistant},
		{"subagent", "azure", "same", PurposeSubagent},
		{"offload", "openai", "summary", PurposeOffload},
		{"auto", "openai", "classifier", PurposeAuto},
	}
	for _, item := range observations {
		_, err := tracker.Record(item.id, Observation{
			Usage:   damessage.Usage{Provider: item.provider, Model: item.model, InputTokens: 10, OutputTokens: 1},
			Purpose: item.purpose, ReportedCostUSD: &cost,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	replay, err := tracker.Record("assistant", Observation{Usage: damessage.Usage{InputTokens: 999}})
	if err != nil || !replay.Replayed {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	report := tracker.Report()
	if report.RequestCount != 4 || report.PricedRequestCount != 4 || report.UnpricedRequestCount != 0 || len(report.Models) != 4 || len(report.Purposes) != 4 {
		t.Fatalf("report = %#v", report)
	}
	for index, purpose := range purposeOrder {
		if report.Purposes[index].Purpose != purpose {
			t.Fatalf("purpose order = %#v", report.Purposes)
		}
	}
}

func TestTrackerRecordsIDLessUsageWithoutDeduplication(t *testing.T) {
	tracker := NewTracker(nil, Options{})
	for range 2 {
		if _, err := tracker.Record("", Observation{Usage: damessage.Usage{InputTokens: 3}}); err != nil {
			t.Fatal(err)
		}
	}
	if report := tracker.Report(); report.RequestCount != 2 || report.InputTokens != 6 {
		t.Fatalf("report = %#v", report)
	}
}

func TestTrackerRepriceIsAtomicAndPreservesReportedCost(t *testing.T) {
	one := EstimatorFunc(func(string, string, damessage.Usage) (float64, bool) { return 1, true })
	two := EstimatorFunc(func(string, string, damessage.Usage) (float64, bool) { return 2, true })
	tracker := NewTracker(one, Options{})
	if _, err := tracker.Record("estimated", Observation{Usage: damessage.Usage{Model: "m", InputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	reported := 3.0
	if _, err := tracker.Record("reported", Observation{Usage: damessage.Usage{Model: "m", InputTokens: 1}, ReportedCostUSD: &reported}); err != nil {
		t.Fatal(err)
	}
	delta, err := tracker.Reprice(two)
	if err != nil || delta != 1 || tracker.Report().CostUSD != 5 {
		t.Fatalf("Reprice() = %v, %v; report = %#v", delta, err, tracker.Report())
	}
	bad := EstimatorFunc(func(string, string, damessage.Usage) (float64, bool) { return math.Inf(1), true })
	if _, err := tracker.Reprice(bad); !errors.Is(err, ErrInvalidUsage) || tracker.Report().CostUSD != 5 {
		t.Fatalf("bad Reprice() err = %v; report = %#v", err, tracker.Report())
	}
}

func TestTrackerLimitsAndOverflowAreTransactional(t *testing.T) {
	tracker := NewTracker(nil, Options{MaxRequests: 1, MaxModels: 1, MaxDetailKeys: 1})
	if _, err := tracker.Record("one", Observation{Usage: damessage.Usage{Model: "m1", InputTokens: math.MaxInt}, Incremental: true}); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(tracker.Report())
	if _, err := tracker.Record("one", Observation{Usage: damessage.Usage{InputTokens: 1}, Incremental: true}); !errors.Is(err, ErrInvalidUsage) {
		t.Fatalf("overflow error = %v", err)
	}
	if _, err := tracker.Record("two", Observation{Usage: damessage.Usage{Model: "m2", InputTokens: 1}}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("request limit error = %v", err)
	}
	after, _ := json.Marshal(tracker.Report())
	if string(before) != string(after) {
		t.Fatalf("failed mutations changed report\nbefore=%s\nafter=%s", before, after)
	}
}

func TestTrackerConcurrentCompletedRequests(t *testing.T) {
	tracker := NewTracker(nil, Options{})
	var wait sync.WaitGroup
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			id := fmt.Sprintf("request-%d", index)
			if _, err := tracker.Record(id, Observation{Usage: damessage.Usage{Model: "m", InputTokens: 1}}); err != nil {
				t.Errorf("Record(%q): %v", id, err)
			}
		}(index)
	}
	wait.Wait()
	if report := tracker.Report(); report.RequestCount != 100 || report.InputTokens != 100 {
		t.Fatalf("report = %#v", report)
	}
}

func TestReportRoundTripMergeAndBounds(t *testing.T) {
	tracker := NewTracker(nil, Options{})
	if _, err := tracker.Record("one", Observation{Usage: damessage.Usage{Provider: "p", Model: "m", InputTokens: 2, OutputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.AddWallTime(250 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tracker.Report())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReport(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeReports([]Report{decoded, decoded}, 0)
	if err != nil || merged.RequestCount != 2 || merged.InputTokens != 4 || merged.WallTimeSeconds != 0.5 || len(merged.Models) != 1 {
		t.Fatalf("MergeReports() = %#v, %v", merged, err)
	}
	if _, err := DecodeReport(strings.NewReader(string(data)), 4); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("bounded DecodeReport() error = %v", err)
	}
	future := decoded
	future.Version++
	futureData, _ := json.Marshal(future)
	if _, err := DecodeReport(bytes.NewReader(futureData), 0); !errors.Is(err, ErrUnsupportedData) {
		t.Fatalf("future DecodeReport() error = %v", err)
	}
	duplicate := strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1)
	if _, err := DecodeReport(strings.NewReader(duplicate), 0); !errors.Is(err, ErrInvalidUsage) {
		t.Fatalf("duplicate-key DecodeReport() error = %v", err)
	}
}

func TestClassifyPurpose(t *testing.T) {
	cases := []struct {
		main   bool
		source string
		want   Purpose
	}{{false, "summarization", PurposeSubagent}, {true, "summarization", PurposeOffload}, {true, "auto_mode_classifier", PurposeAuto}, {true, "", PurposeAssistant}}
	for _, test := range cases {
		if got := ClassifyPurpose(test.main, test.source); got != test.want {
			t.Fatalf("ClassifyPurpose(%v, %q) = %q", test.main, test.source, got)
		}
	}
}

func testPricer(rates map[string]Rates) *Pricer {
	models := make([]ModelPrice, 0, len(rates))
	for model, price := range rates {
		models = append(models, ModelPrice{ID: model, Match: ModelMatch{Equals: model}, Prices: price})
	}
	catalog := NewCatalog([]ProviderPrices{{ID: "provider", Name: "Provider", APIPattern: "example", Models: models}}, CatalogOptions{})
	return NewPricer(catalog, nil, nil, PricerOptions{})
}
