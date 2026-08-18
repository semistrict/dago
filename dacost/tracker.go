package dacost

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/semistrict/dago/damessage"
)

type modelKey struct {
	provider string
	model    string
}

type bucket struct {
	requests int
	input    int64
	output   int64
	cost     float64
	priced   int
}

type recordedRequest struct {
	provider   string
	model      string
	purpose    Purpose
	usage      damessage.Usage
	input      int64
	output     int64
	cacheRead  int64
	cacheWrite int64
	cost       *float64
	reported   bool
	finalized  bool
}

// Tracker is a concurrency-safe per-session request ledger and accumulator.
type Tracker struct {
	mu        sync.RWMutex
	estimator Estimator
	options   normalizedOptions
	requests  map[string]recordedRequest
	models    map[modelKey]bucket
	purposes  map[Purpose]bucket

	requestCount int
	input        int64
	output       int64
	cacheRead    int64
	cacheWrite   int64
	cost         float64
	priced       int
	wallTime     time.Duration
	anonymous    uint64
}

// NewTracker constructs a bounded tracker. A nil estimator is useful when the
// caller records only provider-reported costs.
func NewTracker(estimator Estimator, options Options) *Tracker {
	return &Tracker{
		estimator: estimator,
		options:   normalizeOptions(options),
		requests:  make(map[string]recordedRequest),
		models:    make(map[modelKey]bucket),
		purposes:  make(map[Purpose]bucket),
	}
}

// Record folds an observation into a logical request. Incremental observations
// are added to the request's raw usage, then the prior aggregate contribution is
// retracted and replaced. A completed observation for an existing request is a
// replay and is ignored.
func (tracker *Tracker) Record(requestID string, observation Observation) (Delta, error) {
	if tracker == nil {
		return Delta{}, fmt.Errorf("%w: nil tracker", ErrInvalidUsage)
	}
	if err := validateObservation(requestID, observation, tracker.options); err != nil {
		return Delta{}, err
	}
	if observation.Purpose == "" {
		observation.Purpose = PurposeAssistant
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if requestID == "" {
		if len(tracker.requests) >= tracker.options.maxRequests {
			return Delta{}, fmt.Errorf("%w: request ledger reached %d entries", ErrLimitExceeded, tracker.options.maxRequests)
		}
		if tracker.anonymous == math.MaxUint64 {
			return Delta{}, fmt.Errorf("%w: anonymous request sequence overflow", ErrLimitExceeded)
		}
		tracker.anonymous++
		requestID = "\x00anonymous:" + strconv.FormatUint(tracker.anonymous, 10)
	}
	previous, exists := tracker.requests[requestID]
	if exists && (previous.finalized || !observation.Incremental) {
		return Delta{Replayed: true}, nil
	}
	if !exists && len(tracker.requests) >= tracker.options.maxRequests {
		return Delta{}, fmt.Errorf("%w: request ledger reached %d entries", ErrLimitExceeded, tracker.options.maxRequests)
	}

	usage := cloneUsage(observation.Usage)
	if exists {
		merged, err := mergeUsage(previous.usage, usage, tracker.options.maxDetailKeys)
		if err != nil {
			return Delta{}, err
		}
		usage = merged
	}
	hasCounts := carriesTokenCounts(observation.Usage)
	namesModel := observation.Usage.Model != "" || observation.Usage.Provider != ""
	if !hasCounts && (!exists || !namesModel) {
		return Delta{}, nil
	}

	provider, model := observation.Usage.Provider, observation.Usage.Model
	if exists {
		if provider == "" {
			provider = previous.provider
		}
		if model == "" {
			model = previous.model
		}
	} else {
		if provider == "" {
			provider = observation.FallbackProvider
		}
		if model == "" {
			model = observation.FallbackModel
		}
	}
	usage.Provider, usage.Model = provider, model
	inputTokens, outputTokens := displayTokens(usage)
	input, output := int64(inputTokens), int64(outputTokens)
	cacheRead, cacheWrites := CacheTokenCounts(usage)
	cacheWrite := cacheWrites.Generic + cacheWrites.FiveMinute + cacheWrites.OneHour
	cost, reported, err := tracker.price(observation, usage, provider, model)
	if err != nil {
		return Delta{}, err
	}
	purpose := observation.Purpose
	if exists {
		purpose = previous.purpose
	}
	candidate := recordedRequest{
		provider: provider, model: model, purpose: purpose, usage: usage,
		input: input, output: output, cacheRead: int64(cacheRead), cacheWrite: int64(cacheWrite),
		cost: cost, reported: reported, finalized: !observation.Incremental,
	}
	if err := tracker.checkModelLimit(previous, exists, candidate); err != nil {
		return Delta{}, err
	}
	if err := tracker.checkTotals(previous, exists, candidate); err != nil {
		return Delta{}, err
	}
	if exists {
		tracker.remove(previous)
	}
	tracker.add(candidate)
	tracker.requests[requestID] = candidate

	delta := Delta{
		InputTokens: input, OutputTokens: output, RequestTokens: input + output,
		Recorded: true,
	}
	if exists {
		delta.InputTokens -= previous.input
		delta.OutputTokens -= previous.output
	}
	delta.CostUSD = costDelta(previous.cost, cost, exists)
	return delta, nil
}

func (tracker *Tracker) price(observation Observation, usage damessage.Usage, provider, model string) (*float64, bool, error) {
	if observation.ReportedCostUSD != nil {
		value := *observation.ReportedCostUSD
		return &value, true, nil
	}
	if usage.CostUSD > 0 {
		value := usage.CostUSD
		return &value, true, nil
	}
	if tracker.estimator == nil {
		return nil, false, nil
	}
	value, ok := tracker.estimator.Estimate(provider, model, cloneUsage(usage))
	if !ok {
		return nil, false, nil
	}
	if !finiteNonnegative(value) || value > maxEstimatedCostUSD {
		return nil, false, fmt.Errorf("%w: estimator returned a non-finite or negative cost", ErrInvalidUsage)
	}
	return &value, false, nil
}

// Finalize marks every open request as complete. It should be called at a
// stream-round boundary so replayed chunks after resume cannot be counted twice.
func (tracker *Tracker) Finalize() {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for id, request := range tracker.requests {
		request.finalized = true
		tracker.requests[id] = request
	}
}

// AddWallTime accumulates non-negative session execution time.
func (tracker *Tracker) AddWallTime(duration time.Duration) error {
	if tracker == nil {
		return fmt.Errorf("%w: nil tracker", ErrInvalidUsage)
	}
	if duration < 0 {
		return fmt.Errorf("%w: wall time cannot be negative", ErrInvalidUsage)
	}
	if duration == 0 {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if duration > time.Duration(math.MaxInt64)-tracker.wallTime {
		return fmt.Errorf("%w: wall time overflow", ErrInvalidUsage)
	}
	tracker.wallTime += duration
	return nil
}

// Reprice atomically recomputes every catalog-estimated request. Explicit
// provider-reported costs are retained. The returned value is the signed change
// in the session total.
func (tracker *Tracker) Reprice(estimator Estimator) (float64, error) {
	if tracker == nil {
		return 0, fmt.Errorf("%w: nil tracker", ErrInvalidUsage)
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	updates := make(map[string]*float64, len(tracker.requests))
	for id, request := range tracker.requests {
		if request.reported {
			continue
		}
		if estimator == nil {
			updates[id] = nil
			continue
		}
		value, ok := estimator.Estimate(request.provider, request.model, cloneUsage(request.usage))
		if !ok {
			updates[id] = nil
			continue
		}
		if !finiteNonnegative(value) || value > maxEstimatedCostUSD {
			return 0, fmt.Errorf("%w: estimator returned a non-finite or negative cost", ErrInvalidUsage)
		}
		updates[id] = new(value)
	}
	before := tracker.cost
	for id, cost := range updates {
		request := tracker.requests[id]
		tracker.remove(request)
		request.cost = cost
		tracker.add(request)
		tracker.requests[id] = request
	}
	tracker.estimator = estimator
	return tracker.cost - before, nil
}

func (tracker *Tracker) checkModelLimit(previous recordedRequest, exists bool, candidate recordedRequest) error {
	if candidate.model == "" {
		return nil
	}
	newKey := modelKey{candidate.provider, candidate.model}
	if _, present := tracker.models[newKey]; present {
		return nil
	}
	count := len(tracker.models)
	if exists && previous.model != "" {
		oldKey := modelKey{previous.provider, previous.model}
		if oldKey != newKey && tracker.models[oldKey].requests == 1 {
			count--
		}
	}
	if count >= tracker.options.maxModels {
		return fmt.Errorf("%w: model breakdown reached %d rows", ErrLimitExceeded, tracker.options.maxModels)
	}
	return nil
}

func (tracker *Tracker) checkTotals(previous recordedRequest, exists bool, candidate recordedRequest) error {
	input, output := tracker.input, tracker.output
	cacheRead, cacheWrite := tracker.cacheRead, tracker.cacheWrite
	if exists {
		input -= previous.input
		output -= previous.output
		cacheRead -= previous.cacheRead
		cacheWrite -= previous.cacheWrite
	}
	for _, pair := range [][2]int64{
		{input, candidate.input}, {output, candidate.output},
		{cacheRead, candidate.cacheRead}, {cacheWrite, candidate.cacheWrite},
	} {
		if _, err := safeAdd64(pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

func (tracker *Tracker) add(request recordedRequest) {
	tracker.requestCount++
	tracker.input += request.input
	tracker.output += request.output
	tracker.cacheRead += request.cacheRead
	tracker.cacheWrite += request.cacheWrite
	if request.cost != nil {
		tracker.cost += *request.cost
		tracker.priced++
	}
	tracker.purposes[request.purpose] = addBucket(tracker.purposes[request.purpose], request)
	if request.model != "" {
		key := modelKey{request.provider, request.model}
		tracker.models[key] = addBucket(tracker.models[key], request)
	}
}

func (tracker *Tracker) remove(request recordedRequest) {
	tracker.requestCount--
	tracker.input -= request.input
	tracker.output -= request.output
	tracker.cacheRead -= request.cacheRead
	tracker.cacheWrite -= request.cacheWrite
	if request.cost != nil {
		tracker.cost -= *request.cost
		tracker.priced--
	}
	if math.Abs(tracker.cost) < 1e-15 {
		tracker.cost = 0
	}
	purposeBucket := removeBucket(tracker.purposes[request.purpose], request)
	if purposeBucket.requests == 0 {
		delete(tracker.purposes, request.purpose)
	} else {
		tracker.purposes[request.purpose] = purposeBucket
	}
	if request.model != "" {
		key := modelKey{request.provider, request.model}
		modelBucket := removeBucket(tracker.models[key], request)
		if modelBucket.requests == 0 {
			delete(tracker.models, key)
		} else {
			tracker.models[key] = modelBucket
		}
	}
}

func addBucket(value bucket, request recordedRequest) bucket {
	value.requests++
	value.input += request.input
	value.output += request.output
	if request.cost != nil {
		value.cost += *request.cost
		value.priced++
	}
	return value
}

func removeBucket(value bucket, request recordedRequest) bucket {
	value.requests--
	value.input -= request.input
	value.output -= request.output
	if request.cost != nil {
		value.cost -= *request.cost
		value.priced--
	}
	if math.Abs(value.cost) < 1e-15 {
		value.cost = 0
	}
	return value
}

func carriesTokenCounts(usage damessage.Usage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0
}

func displayTokens(usage damessage.Usage) (int, int) {
	input := inclusiveInputTokens(usage)
	output := max(usage.OutputTokens, 0)
	if input == 0 && output == 0 {
		input = max(usage.TotalTokens, 0)
	}
	return input, output
}

func mergeUsage(left, right damessage.Usage, maxDetails int) (damessage.Usage, error) {
	result := cloneUsage(left)
	var err error
	if result.InputTokens, err = safeAdd(result.InputTokens, right.InputTokens); err != nil {
		return damessage.Usage{}, err
	}
	if result.OutputTokens, err = safeAdd(result.OutputTokens, right.OutputTokens); err != nil {
		return damessage.Usage{}, err
	}
	if result.TotalTokens, err = safeAdd(result.TotalTokens, right.TotalTokens); err != nil {
		return damessage.Usage{}, err
	}
	if result.CostUSD += right.CostUSD; !finiteNonnegative(result.CostUSD) {
		return damessage.Usage{}, fmt.Errorf("%w: accumulated cost overflow", ErrInvalidUsage)
	}
	if result.InputDetails, err = mergeDetails(result.InputDetails, right.InputDetails, maxDetails); err != nil {
		return damessage.Usage{}, err
	}
	if result.OutputDetails, err = mergeDetails(result.OutputDetails, right.OutputDetails, maxDetails); err != nil {
		return damessage.Usage{}, err
	}
	if right.Provider != "" {
		result.Provider = right.Provider
	}
	if right.Model != "" {
		result.Model = right.Model
	}
	return result, nil
}

func mergeDetails(left, right map[string]int, maximum int) (map[string]int, error) {
	if len(left) == 0 && len(right) == 0 {
		return nil, nil
	}
	if len(left)+len(right) > maximum*2 {
		return nil, fmt.Errorf("%w: too many token detail buckets", ErrLimitExceeded)
	}
	result := cloneDetails(left)
	for key, value := range right {
		if _, exists := result[key]; !exists && len(result) >= maximum {
			return nil, fmt.Errorf("%w: too many token detail buckets", ErrLimitExceeded)
		}
		merged, err := safeAdd(result[key], value)
		if err != nil {
			return nil, err
		}
		result[key] = merged
	}
	return result, nil
}

func safeAdd(left, right int) (int, error) {
	if (right > 0 && left > math.MaxInt-right) || (right < 0 && left < math.MinInt-right) {
		return 0, fmt.Errorf("%w: token count overflow", ErrInvalidUsage)
	}
	return left + right, nil
}

func safeAdd64(left, right int64) (int64, error) {
	if (right > 0 && left > math.MaxInt64-right) || (right < 0 && left < math.MinInt64-right) {
		return 0, fmt.Errorf("%w: token count overflow", ErrInvalidUsage)
	}
	return left + right, nil
}

func cloneUsage(usage damessage.Usage) damessage.Usage {
	usage.InputDetails = cloneDetails(usage.InputDetails)
	usage.OutputDetails = cloneDetails(usage.OutputDetails)
	return usage
}

func cloneDetails(details map[string]int) map[string]int {
	if len(details) == 0 {
		return nil
	}
	copy := make(map[string]int, len(details))
	for key, value := range details {
		copy[key] = value
	}
	return copy
}

func costDelta(previous, current *float64, existed bool) *float64 {
	if previous == nil && current == nil {
		return nil
	}
	value := 0.0
	if current != nil {
		value = *current
	}
	if existed && previous != nil {
		value -= *previous
	}
	return &value
}
