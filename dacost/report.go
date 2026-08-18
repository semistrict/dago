package dacost

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
)

const reportVersion = 1

// ModelStats is one deterministic provider/model report row.
type ModelStats struct {
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	RequestCount       int     `json:"request_count"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	PricedRequestCount int     `json:"priced_request_count"`
}

// PurposeStats is one deterministic request-purpose report row.
type PurposeStats struct {
	Purpose            Purpose `json:"purpose"`
	RequestCount       int     `json:"request_count"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	CostUSD            float64 `json:"cost_usd"`
	PricedRequestCount int     `json:"priced_request_count"`
}

// Report is the stable JSON-safe snapshot of one tracker or merged session.
// An unpriced request contributes tokens and request counts but not CostUSD.
type Report struct {
	Version              int            `json:"version"`
	RequestCount         int            `json:"request_count"`
	InputTokens          int64          `json:"input_tokens"`
	OutputTokens         int64          `json:"output_tokens"`
	CacheReadTokens      int64          `json:"cache_read_tokens"`
	CacheWriteTokens     int64          `json:"cache_write_tokens"`
	CostUSD              float64        `json:"cost_usd"`
	PricedRequestCount   int            `json:"priced_request_count"`
	UnpricedRequestCount int            `json:"unpriced_request_count"`
	WallTimeSeconds      float64        `json:"wall_time_seconds"`
	Models               []ModelStats   `json:"models"`
	Purposes             []PurposeStats `json:"purposes"`
}

// Report returns an immutable, deterministically ordered snapshot.
func (tracker *Tracker) Report() Report {
	if tracker == nil {
		return Report{Version: reportVersion, Models: []ModelStats{}, Purposes: []PurposeStats{}}
	}
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	report := Report{
		Version: reportVersion, RequestCount: tracker.requestCount,
		InputTokens: tracker.input, OutputTokens: tracker.output,
		CacheReadTokens: tracker.cacheRead, CacheWriteTokens: tracker.cacheWrite,
		CostUSD: tracker.cost, PricedRequestCount: tracker.priced,
		UnpricedRequestCount: tracker.requestCount - tracker.priced,
		WallTimeSeconds:      tracker.wallTime.Seconds(),
		Models:               make([]ModelStats, 0, len(tracker.models)),
		Purposes:             make([]PurposeStats, 0, len(tracker.purposes)),
	}
	for key, value := range tracker.models {
		report.Models = append(report.Models, ModelStats{
			Provider: key.provider, Model: key.model, RequestCount: value.requests,
			InputTokens: value.input, OutputTokens: value.output,
			CostUSD: value.cost, PricedRequestCount: value.priced,
		})
	}
	sort.Slice(report.Models, func(left, right int) bool {
		if report.Models[left].Provider != report.Models[right].Provider {
			return report.Models[left].Provider < report.Models[right].Provider
		}
		return report.Models[left].Model < report.Models[right].Model
	})
	for _, purpose := range purposeOrder {
		value, ok := tracker.purposes[purpose]
		if !ok {
			continue
		}
		report.Purposes = append(report.Purposes, PurposeStats{
			Purpose: purpose, RequestCount: value.requests,
			InputTokens: value.input, OutputTokens: value.output,
			CostUSD: value.cost, PricedRequestCount: value.priced,
		})
	}
	return report
}

// DecodeReport reads one bounded versioned report. A zero maximum selects 1 MiB.
func DecodeReport(reader io.Reader, maximum int64) (Report, error) {
	if reader == nil {
		return Report{}, fmt.Errorf("%w: report reader is required", ErrInvalidUsage)
	}
	if maximum < 0 {
		panic("dacost: report byte limit cannot be negative")
	}
	if maximum == 0 {
		maximum = 1 << 20
	}
	if maximum > 64<<20 {
		panic("dacost: report byte limit exceeds hard safety maximum")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return Report{}, fmt.Errorf("%w: read report: %v", ErrInvalidUsage, err)
	}
	if int64(len(data)) > maximum {
		return Report{}, fmt.Errorf("%w: report exceeds %d bytes", ErrLimitExceeded, maximum)
	}
	if err := validateUniqueJSONKeys(data); err != nil {
		return Report{}, fmt.Errorf("%w: decode report: %v", ErrInvalidUsage, err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, fmt.Errorf("%w: decode report: %v", ErrInvalidUsage, err)
	}
	if err := validateReport(report, 4096); err != nil {
		return Report{}, err
	}
	if report.Models == nil {
		report.Models = []ModelStats{}
	}
	if report.Purposes == nil {
		report.Purposes = []PurposeStats{}
	}
	return report, nil
}

// MergeReports combines turn reports into a deterministic session report. A
// zero row limit selects 4096. Inputs are never mutated.
func MergeReports(reports []Report, maximumRows int) (Report, error) {
	if maximumRows < 0 {
		panic("dacost: report row limit cannot be negative")
	}
	if maximumRows == 0 {
		maximumRows = 4096
	}
	if maximumRows > 65_536 {
		panic("dacost: report row limit exceeds hard safety maximum")
	}
	models := make(map[modelKey]ModelStats)
	purposes := make(map[Purpose]PurposeStats)
	result := Report{Version: reportVersion, Models: []ModelStats{}, Purposes: []PurposeStats{}}
	for _, report := range reports {
		if err := validateReport(report, maximumRows); err != nil {
			return Report{}, err
		}
		var err error
		if result.RequestCount, err = safeAdd(result.RequestCount, report.RequestCount); err != nil {
			return Report{}, err
		}
		if result.InputTokens, err = safeAdd64(result.InputTokens, report.InputTokens); err != nil {
			return Report{}, err
		}
		if result.OutputTokens, err = safeAdd64(result.OutputTokens, report.OutputTokens); err != nil {
			return Report{}, err
		}
		if result.CacheReadTokens, err = safeAdd64(result.CacheReadTokens, report.CacheReadTokens); err != nil {
			return Report{}, err
		}
		if result.CacheWriteTokens, err = safeAdd64(result.CacheWriteTokens, report.CacheWriteTokens); err != nil {
			return Report{}, err
		}
		if result.PricedRequestCount, err = safeAdd(result.PricedRequestCount, report.PricedRequestCount); err != nil {
			return Report{}, err
		}
		result.CostUSD += report.CostUSD
		result.WallTimeSeconds += report.WallTimeSeconds
		if !finiteNonnegative(result.CostUSD) || !finiteNonnegative(result.WallTimeSeconds) {
			return Report{}, fmt.Errorf("%w: merged floating-point total overflow", ErrInvalidUsage)
		}
		for _, row := range report.Models {
			key := modelKey{row.Provider, row.Model}
			value := models[key]
			value.Provider, value.Model = row.Provider, row.Model
			var err error
			if value.RequestCount, err = safeAdd(value.RequestCount, row.RequestCount); err != nil {
				return Report{}, err
			}
			if value.InputTokens, err = safeAdd64(value.InputTokens, row.InputTokens); err != nil {
				return Report{}, err
			}
			if value.OutputTokens, err = safeAdd64(value.OutputTokens, row.OutputTokens); err != nil {
				return Report{}, err
			}
			value.CostUSD += row.CostUSD
			if value.PricedRequestCount, err = safeAdd(value.PricedRequestCount, row.PricedRequestCount); err != nil {
				return Report{}, err
			}
			if !finiteNonnegative(value.CostUSD) {
				return Report{}, fmt.Errorf("%w: merged model cost overflow", ErrInvalidUsage)
			}
			models[key] = value
		}
		for _, row := range report.Purposes {
			value := purposes[row.Purpose]
			value.Purpose = row.Purpose
			var err error
			if value.RequestCount, err = safeAdd(value.RequestCount, row.RequestCount); err != nil {
				return Report{}, err
			}
			if value.InputTokens, err = safeAdd64(value.InputTokens, row.InputTokens); err != nil {
				return Report{}, err
			}
			if value.OutputTokens, err = safeAdd64(value.OutputTokens, row.OutputTokens); err != nil {
				return Report{}, err
			}
			value.CostUSD += row.CostUSD
			if value.PricedRequestCount, err = safeAdd(value.PricedRequestCount, row.PricedRequestCount); err != nil {
				return Report{}, err
			}
			if !finiteNonnegative(value.CostUSD) {
				return Report{}, fmt.Errorf("%w: merged purpose cost overflow", ErrInvalidUsage)
			}
			purposes[row.Purpose] = value
		}
		if len(models)+len(purposes) > maximumRows {
			return Report{}, fmt.Errorf("%w: merged report exceeds %d rows", ErrLimitExceeded, maximumRows)
		}
	}
	result.UnpricedRequestCount = result.RequestCount - result.PricedRequestCount
	for _, row := range models {
		result.Models = append(result.Models, row)
	}
	sort.Slice(result.Models, func(left, right int) bool {
		if result.Models[left].Provider != result.Models[right].Provider {
			return result.Models[left].Provider < result.Models[right].Provider
		}
		return result.Models[left].Model < result.Models[right].Model
	})
	for _, purpose := range purposeOrder {
		if row, ok := purposes[purpose]; ok {
			result.Purposes = append(result.Purposes, row)
		}
	}
	return result, nil
}

func validateReport(report Report, maximumRows int) error {
	if report.Version != reportVersion {
		return fmt.Errorf("%w: report version %d", ErrUnsupportedData, report.Version)
	}
	if len(report.Models)+len(report.Purposes) > maximumRows {
		return fmt.Errorf("%w: report exceeds %d rows", ErrLimitExceeded, maximumRows)
	}
	integers := []int64{
		int64(report.RequestCount), report.InputTokens, report.OutputTokens,
		report.CacheReadTokens, report.CacheWriteTokens, int64(report.PricedRequestCount),
		int64(report.UnpricedRequestCount),
	}
	for _, value := range integers {
		if value < 0 {
			return fmt.Errorf("%w: report contains a negative total", ErrInvalidUsage)
		}
	}
	if report.PricedRequestCount > report.RequestCount || report.UnpricedRequestCount != report.RequestCount-report.PricedRequestCount {
		return fmt.Errorf("%w: inconsistent priced request counts", ErrInvalidUsage)
	}
	if !finiteNonnegative(report.CostUSD) || math.IsNaN(report.WallTimeSeconds) || math.IsInf(report.WallTimeSeconds, 0) || report.WallTimeSeconds < 0 {
		return fmt.Errorf("%w: report contains an invalid cost or duration", ErrInvalidUsage)
	}
	seenModels := make(map[modelKey]struct{}, len(report.Models))
	var modelRequests, modelPriced int
	var modelInput, modelOutput int64
	var modelCost float64
	for _, row := range report.Models {
		if row.Model == "" || len(row.Model) > 4096 || len(row.Provider) > 4096 || containsControl(row.Model) || containsControl(row.Provider) || row.RequestCount <= 0 || row.InputTokens < 0 || row.OutputTokens < 0 || row.PricedRequestCount < 0 || row.PricedRequestCount > row.RequestCount || !finiteNonnegative(row.CostUSD) {
			return fmt.Errorf("%w: invalid model row", ErrInvalidUsage)
		}
		key := modelKey{row.Provider, row.Model}
		if _, exists := seenModels[key]; exists {
			return fmt.Errorf("%w: duplicate model row", ErrInvalidUsage)
		}
		seenModels[key] = struct{}{}
		var err error
		if modelRequests, err = safeAdd(modelRequests, row.RequestCount); err != nil {
			return err
		}
		if modelInput, err = safeAdd64(modelInput, row.InputTokens); err != nil {
			return err
		}
		if modelOutput, err = safeAdd64(modelOutput, row.OutputTokens); err != nil {
			return err
		}
		if modelPriced, err = safeAdd(modelPriced, row.PricedRequestCount); err != nil {
			return err
		}
		modelCost += row.CostUSD
	}
	if modelRequests > report.RequestCount || modelInput > report.InputTokens || modelOutput > report.OutputTokens || modelPriced > report.PricedRequestCount || modelCost > report.CostUSD+1e-9 {
		return fmt.Errorf("%w: model rows exceed report totals", ErrInvalidUsage)
	}
	seenPurposes := make(map[Purpose]struct{}, len(report.Purposes))
	var purposeRequests, purposePriced int
	var purposeInput, purposeOutput int64
	var purposeCost float64
	for _, row := range report.Purposes {
		if !row.Purpose.valid() || row.RequestCount <= 0 || row.InputTokens < 0 || row.OutputTokens < 0 || row.PricedRequestCount < 0 || row.PricedRequestCount > row.RequestCount || !finiteNonnegative(row.CostUSD) {
			return fmt.Errorf("%w: invalid purpose row", ErrInvalidUsage)
		}
		if _, exists := seenPurposes[row.Purpose]; exists {
			return fmt.Errorf("%w: duplicate purpose row", ErrInvalidUsage)
		}
		seenPurposes[row.Purpose] = struct{}{}
		var err error
		if purposeRequests, err = safeAdd(purposeRequests, row.RequestCount); err != nil {
			return err
		}
		if purposeInput, err = safeAdd64(purposeInput, row.InputTokens); err != nil {
			return err
		}
		if purposeOutput, err = safeAdd64(purposeOutput, row.OutputTokens); err != nil {
			return err
		}
		if purposePriced, err = safeAdd(purposePriced, row.PricedRequestCount); err != nil {
			return err
		}
		purposeCost += row.CostUSD
	}
	if purposeRequests != report.RequestCount || purposeInput != report.InputTokens || purposeOutput != report.OutputTokens || purposePriced != report.PricedRequestCount || math.Abs(purposeCost-report.CostUSD) > 1e-9 {
		return fmt.Errorf("%w: purpose rows do not match report totals", ErrInvalidUsage)
	}
	return nil
}
