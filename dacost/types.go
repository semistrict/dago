// Package dacost provides provider-neutral, bounded token and cost accounting.
//
// A Tracker records one logical model request once even when incremental stream
// chunks revise its usage. Pricing is advisory only; it never authorizes or
// limits model execution.
package dacost

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/semistrict/dago/damessage"
)

var (
	ErrInvalidUsage    = errors.New("invalid usage observation")
	ErrLimitExceeded   = errors.New("cost accounting limit exceeded")
	ErrInvalidCatalog  = errors.New("invalid pricing catalog")
	ErrUnsupportedData = errors.New("unsupported cost accounting data")
)

// Purpose is the billing/display class of a model request.
type Purpose string

const (
	PurposeAssistant Purpose = "assistant"
	PurposeSubagent  Purpose = "subagent"
	PurposeOffload   Purpose = "offload"
	PurposeAuto      Purpose = "auto"
)

var purposeOrder = []Purpose{PurposeAssistant, PurposeSubagent, PurposeOffload, PurposeAuto}

func (purpose Purpose) valid() bool {
	return purpose == PurposeAssistant || purpose == PurposeSubagent || purpose == PurposeOffload || purpose == PurposeAuto
}

// ClassifyPurpose maps stream topology and request-source metadata to a stable
// accounting bucket.
func ClassifyPurpose(mainAgent bool, source string) Purpose {
	if !mainAgent {
		return PurposeSubagent
	}
	switch source {
	case "summarization":
		return PurposeOffload
	case "auto_mode_classifier":
		return PurposeAuto
	default:
		return PurposeAssistant
	}
}

// Observation describes one complete response or one incremental stream chunk.
// Model and Provider on Usage are response-reported values; fallbacks identify
// the configured request when the provider has not named them yet.
type Observation struct {
	Usage            damessage.Usage
	FallbackProvider string
	FallbackModel    string
	Purpose          Purpose
	Incremental      bool
	ReportedCostUSD  *float64
}

// Delta is the signed change made by Record. CostUSD is nil when neither the
// old nor the revised request was priceable. Replayed observations have no
// effect.
type Delta struct {
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	CostUSD       *float64 `json:"cost_usd,omitempty"`
	RequestTokens int64    `json:"request_tokens"`
	Recorded      bool     `json:"recorded"`
	Replayed      bool     `json:"replayed,omitempty"`
}

// Estimator prices one request's inclusive usage. The boolean distinguishes an
// unavailable price from a legitimate zero-dollar estimate.
type Estimator interface {
	Estimate(provider, model string, usage damessage.Usage) (float64, bool)
}

// EstimatorFunc adapts a function to Estimator.
type EstimatorFunc func(provider, model string, usage damessage.Usage) (float64, bool)

func (fn EstimatorFunc) Estimate(provider, model string, usage damessage.Usage) (float64, bool) {
	return fn(provider, model, usage)
}

// Options bounds one Tracker. Zero fields select conservative defaults.
type Options struct {
	MaxRequests   int
	MaxModels     int
	MaxDetailKeys int
	MaxIDBytes    int
	MaxNameBytes  int
}

type normalizedOptions struct {
	maxRequests   int
	maxModels     int
	maxDetailKeys int
	maxIDBytes    int
	maxNameBytes  int
}

func normalizeOptions(options Options) normalizedOptions {
	if options.MaxRequests < 0 || options.MaxModels < 0 || options.MaxDetailKeys < 0 || options.MaxIDBytes < 0 || options.MaxNameBytes < 0 {
		panic("dacost: limits cannot be negative")
	}
	if options.MaxRequests == 0 {
		options.MaxRequests = 4096
	}
	if options.MaxModels == 0 {
		options.MaxModels = 256
	}
	if options.MaxDetailKeys == 0 {
		options.MaxDetailKeys = 64
	}
	if options.MaxIDBytes == 0 {
		options.MaxIDBytes = 512
	}
	if options.MaxNameBytes == 0 {
		options.MaxNameBytes = 256
	}
	if options.MaxRequests > 65_536 || options.MaxModels > 4096 || options.MaxDetailKeys > 256 || options.MaxIDBytes > 4096 || options.MaxNameBytes > 4096 {
		panic("dacost: limits exceed hard safety maximums")
	}
	return normalizedOptions{
		maxRequests: options.MaxRequests, maxModels: options.MaxModels,
		maxDetailKeys: options.MaxDetailKeys, maxIDBytes: options.MaxIDBytes,
		maxNameBytes: options.MaxNameBytes,
	}
}

func validateObservation(requestID string, observation Observation, options normalizedOptions) error {
	if len(requestID) > options.maxIDBytes || strings.TrimSpace(requestID) != requestID || containsControl(requestID) {
		return fmt.Errorf("%w: request ID is padded, contains controls, or is too long", ErrInvalidUsage)
	}
	purpose := observation.Purpose
	if purpose == "" {
		purpose = PurposeAssistant
	}
	if !purpose.valid() {
		return fmt.Errorf("%w: unknown purpose %q", ErrInvalidUsage, purpose)
	}
	for _, value := range []string{observation.Usage.Provider, observation.Usage.Model, observation.FallbackProvider, observation.FallbackModel} {
		if len(value) > options.maxNameBytes || strings.TrimSpace(value) != value || containsControl(value) {
			return fmt.Errorf("%w: provider or model is padded, contains controls, or is too long", ErrInvalidUsage)
		}
	}
	if !observation.Incremental && (observation.Usage.InputTokens < 0 || observation.Usage.OutputTokens < 0 || observation.Usage.TotalTokens < 0) {
		return fmt.Errorf("%w: complete usage cannot contain negative tokens", ErrInvalidUsage)
	}
	if len(observation.Usage.InputDetails) > options.maxDetailKeys || len(observation.Usage.OutputDetails) > options.maxDetailKeys {
		return fmt.Errorf("%w: too many token detail buckets", ErrLimitExceeded)
	}
	if observation.ReportedCostUSD != nil && (!finiteNonnegative(*observation.ReportedCostUSD) || *observation.ReportedCostUSD > maxEstimatedCostUSD) {
		return fmt.Errorf("%w: reported cost must be finite and non-negative", ErrInvalidUsage)
	}
	if observation.Usage.CostUSD < 0 || observation.Usage.CostUSD > maxEstimatedCostUSD || math.IsNaN(observation.Usage.CostUSD) || math.IsInf(observation.Usage.CostUSD, 0) {
		return fmt.Errorf("%w: usage cost must be finite and non-negative", ErrInvalidUsage)
	}
	return nil
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}
