package dacost

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"

	"github.com/semistrict/dago/damessage"
)

const (
	maxRateUSDPerMillion = 1_000_000
	maxEstimatedCostUSD  = 1_000_000_000
)

// CacheWrites is the normalized cache-write breakdown. TTL detail wins over a
// generic write count because providers commonly report both representations
// for the same tokens.
type CacheWrites struct {
	Generic    int `json:"generic"`
	FiveMinute int `json:"five_minute"`
	OneHour    int `json:"one_hour"`
}

// CacheTokenCounts returns disjoint cache buckets clamped to the inclusive
// input-token total. Reads consume the budget first, followed by generic, five
// minute, and one hour writes, biasing malformed provider data toward a lower
// estimate.
func CacheTokenCounts(usage damessage.Usage) (int, CacheWrites) {
	input := inclusiveInputTokens(usage)
	details := usage.InputDetails
	read := positiveDetail(details, "cache_read")
	five := positiveDetail(details, "ephemeral_5m_input_tokens")
	hour := positiveDetail(details, "ephemeral_1h_input_tokens")
	generic := 0
	if five == 0 && hour == 0 {
		generic = positiveDetail(details, "cache_creation")
		if generic == 0 {
			generic = positiveDetail(details, "cache_write")
		}
	}
	read = min(read, input)
	remaining := input - read
	generic = min(generic, remaining)
	remaining -= generic
	five = min(five, remaining)
	remaining -= five
	hour = min(hour, remaining)
	return read, CacheWrites{Generic: generic, FiveMinute: five, OneHour: hour}
}

// inclusiveInputTokens accepts both common normalization conventions: input as
// an inclusive provider total, or input as uncached tokens with cache details
// carried separately. A total-only record remains unpriceable because there is
// no defensible input/output split.
func inclusiveInputTokens(usage damessage.Usage) int {
	input := max(usage.InputTokens, 0)
	hasDetails := len(usage.InputDetails) != 0
	if input > 0 || usage.OutputTokens > 0 || hasDetails {
		derived := max(usage.TotalTokens-usage.OutputTokens, 0)
		input = max(input, derived)
	}
	return input
}

func positiveDetail(details map[string]int, key string) int {
	return max(details[key], 0)
}

// Rates contains USD prices per million tokens. Nil means the bucket has no
// distinct price and remains part of its ordinary input or output total.
type Rates struct {
	InputMTok           *float64 `json:"input_mtok,omitempty"`
	OutputMTok          *float64 `json:"output_mtok,omitempty"`
	CacheReadMTok       *float64 `json:"cache_read_mtok,omitempty"`
	CacheWriteMTok      *float64 `json:"cache_write_mtok,omitempty"`
	CacheWrite5mMTok    *float64 `json:"cache_write_5m_mtok,omitempty"`
	CacheWrite1hMTok    *float64 `json:"cache_write_1h_mtok,omitempty"`
	OutputReasoningMTok *float64 `json:"output_reasoning_mtok,omitempty"`
	InputAudioMTok      *float64 `json:"input_audio_mtok,omitempty"`
	OutputAudioMTok     *float64 `json:"output_audio_mtok,omitempty"`
}

// ModelMatch is one bounded model identifier predicate. Exactly one predicate
// must be present at each level.
type ModelMatch struct {
	Equals     string       `json:"equals,omitempty"`
	StartsWith string       `json:"starts_with,omitempty"`
	Contains   string       `json:"contains,omitempty"`
	Regex      string       `json:"regex,omitempty"`
	Or         []ModelMatch `json:"or,omitempty"`
}

// ModelPrice is one model match and its rates.
type ModelPrice struct {
	ID            string     `json:"id"`
	Match         ModelMatch `json:"match"`
	Prices        Rates      `json:"prices"`
	PriceComments string     `json:"price_comments,omitempty"`
}

// ProviderPrices groups model prices under a provider identifier.
type ProviderPrices struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	APIPattern string       `json:"api_pattern"`
	Models     []ModelPrice `json:"models"`
}

// CatalogOptions bounds pricing-catalog parsing. Zero values select defaults.
type CatalogOptions struct {
	MaxBytes       int64
	MaxProviders   int
	MaxModels      int
	MaxMatchDepth  int
	MaxStringBytes int
}

type catalogOptions struct {
	maxBytes       int64
	maxProviders   int
	maxModels      int
	maxMatchDepth  int
	maxStringBytes int
}

func normalizeCatalogOptions(options CatalogOptions) catalogOptions {
	if options.MaxBytes < 0 || options.MaxProviders < 0 || options.MaxModels < 0 || options.MaxMatchDepth < 0 || options.MaxStringBytes < 0 {
		panic("dacost: catalog limits cannot be negative")
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 4 << 20
	}
	if options.MaxProviders == 0 {
		options.MaxProviders = 128
	}
	if options.MaxModels == 0 {
		options.MaxModels = 4096
	}
	if options.MaxMatchDepth == 0 {
		options.MaxMatchDepth = 8
	}
	if options.MaxStringBytes == 0 {
		options.MaxStringBytes = 512
	}
	if options.MaxBytes > 64<<20 || options.MaxProviders > 4096 || options.MaxModels > 65_536 || options.MaxMatchDepth > 16 || options.MaxStringBytes > 4096 {
		panic("dacost: catalog limits exceed hard safety maximums")
	}
	return catalogOptions{
		maxBytes: options.MaxBytes, maxProviders: options.MaxProviders,
		maxModels: options.MaxModels, maxMatchDepth: options.MaxMatchDepth,
		maxStringBytes: options.MaxStringBytes,
	}
}

type compiledMatch struct {
	match ModelMatch
	regex *regexp.Regexp
	or    []compiledMatch
}

type compiledModel struct {
	model ModelPrice
	match compiledMatch
}

type compiledProvider struct {
	provider ProviderPrices
	models   []compiledModel
}

// Catalog is an immutable, validated pricing catalog.
type Catalog struct {
	providers []compiledProvider
}

// NewCatalog validates caller-owned static catalog entries. Invalid entries
// panic because they are programmer configuration; use DecodeCatalog for
// external data.
func NewCatalog(providers []ProviderPrices, options CatalogOptions) *Catalog {
	catalog, err := buildCatalog(providers, normalizeCatalogOptions(options))
	if err != nil {
		panic(err)
	}
	return catalog
}

// DecodeCatalog reads one bounded provider-array JSON document.
func DecodeCatalog(reader io.Reader, options CatalogOptions) (*Catalog, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: reader is required", ErrInvalidCatalog)
	}
	limits := normalizeCatalogOptions(options)
	data, err := io.ReadAll(io.LimitReader(reader, limits.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read catalog: %v", ErrInvalidCatalog, err)
	}
	if int64(len(data)) > limits.maxBytes {
		return nil, fmt.Errorf("%w: catalog exceeds %d bytes", ErrLimitExceeded, limits.maxBytes)
	}
	if strings.TrimSpace(string(data)) == "" {
		return buildCatalog(nil, limits)
	}
	if err := validateUniqueJSONKeys(data); err != nil {
		return nil, fmt.Errorf("%w: decode provider array: %v", ErrInvalidCatalog, err)
	}
	var providers []ProviderPrices
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("%w: decode provider array: %v", ErrInvalidCatalog, err)
	}
	if providers == nil && strings.TrimSpace(string(data)) != "[]" {
		return nil, fmt.Errorf("%w: top-level value must be a provider array", ErrInvalidCatalog)
	}
	return buildCatalog(providers, limits)
}

// LoadCatalog loads a local override file. A missing file is an empty catalog;
// other I/O and validation errors are explicit. The file must be regular and is
// read only once by this call.
func LoadCatalog(path string, options CatalogOptions) (*Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: catalog path is required", ErrInvalidCatalog)
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewCatalog(nil, options), nil
		}
		return nil, fmt.Errorf("%w: open catalog: %v", ErrInvalidCatalog, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat catalog: %v", ErrInvalidCatalog, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: catalog is not a regular file", ErrInvalidCatalog)
	}
	return DecodeCatalog(file, options)
}

func buildCatalog(providers []ProviderPrices, options catalogOptions) (*Catalog, error) {
	if len(providers) > options.maxProviders {
		return nil, fmt.Errorf("%w: more than %d providers", ErrLimitExceeded, options.maxProviders)
	}
	result := &Catalog{providers: make([]compiledProvider, 0, len(providers))}
	providerIDs := make(map[string]struct{}, len(providers))
	modelCount := 0
	for _, provider := range providers {
		if err := validateCatalogString(provider.ID, options, "provider id"); err != nil {
			return nil, err
		}
		if err := validateCatalogString(provider.Name, options, "provider name"); err != nil {
			return nil, err
		}
		if provider.APIPattern == "" || len(provider.APIPattern) > options.maxStringBytes || containsControl(provider.APIPattern) {
			return nil, fmt.Errorf("%w: provider api_pattern is empty or too long", ErrInvalidCatalog)
		}
		if _, err := regexp.Compile(provider.APIPattern); err != nil {
			return nil, fmt.Errorf("%w: invalid provider api_pattern", ErrInvalidCatalog)
		}
		providerKey := strings.ToLower(provider.ID)
		if _, exists := providerIDs[providerKey]; exists {
			return nil, fmt.Errorf("%w: duplicate provider %q", ErrInvalidCatalog, provider.ID)
		}
		providerIDs[providerKey] = struct{}{}
		compiled := compiledProvider{provider: cloneProvider(provider), models: make([]compiledModel, 0, len(provider.Models))}
		modelIDs := make(map[string]struct{}, len(provider.Models))
		for _, model := range provider.Models {
			model = cloneModel(model)
			modelCount++
			if modelCount > options.maxModels {
				return nil, fmt.Errorf("%w: more than %d models", ErrLimitExceeded, options.maxModels)
			}
			if err := validateCatalogString(model.ID, options, "model id"); err != nil {
				return nil, err
			}
			if _, exists := modelIDs[model.ID]; exists {
				return nil, fmt.Errorf("%w: duplicate model %q for provider %q", ErrInvalidCatalog, model.ID, provider.ID)
			}
			modelIDs[model.ID] = struct{}{}
			if err := validateRates(model.Prices); err != nil {
				return nil, fmt.Errorf("%w: provider %q model %q: %v", ErrInvalidCatalog, provider.ID, model.ID, err)
			}
			match, err := compileMatch(model.Match, options, 1)
			if err != nil {
				return nil, fmt.Errorf("%w: provider %q model %q: %v", ErrInvalidCatalog, provider.ID, model.ID, err)
			}
			compiled.models = append(compiled.models, compiledModel{model: model, match: match})
		}
		result.providers = append(result.providers, compiled)
	}
	return result, nil
}

func validateCatalogString(value string, options catalogOptions, field string) error {
	if value == "" || len(value) > options.maxStringBytes || strings.TrimSpace(value) != value || containsControl(value) {
		return fmt.Errorf("%w: %s is empty, padded, or too long", ErrInvalidCatalog, field)
	}
	return nil
}

func validateRates(rates Rates) error {
	values := []*float64{
		rates.InputMTok, rates.OutputMTok, rates.CacheReadMTok, rates.CacheWriteMTok,
		rates.CacheWrite5mMTok, rates.CacheWrite1hMTok, rates.OutputReasoningMTok,
		rates.InputAudioMTok, rates.OutputAudioMTok,
	}
	seen := false
	for _, value := range values {
		if value == nil {
			continue
		}
		seen = true
		if !finiteNonnegative(*value) || *value > maxRateUSDPerMillion {
			return fmt.Errorf("rate must be finite, non-negative, and no more than %g USD per million tokens", float64(maxRateUSDPerMillion))
		}
	}
	if !seen {
		return fmt.Errorf("at least one rate is required")
	}
	return nil
}

func compileMatch(match ModelMatch, options catalogOptions, depth int) (compiledMatch, error) {
	if depth > options.maxMatchDepth {
		return compiledMatch{}, fmt.Errorf("model match exceeds depth %d", options.maxMatchDepth)
	}
	count := 0
	for _, value := range []string{match.Equals, match.StartsWith, match.Contains, match.Regex} {
		if value != "" {
			count++
			if len(value) > options.maxStringBytes || containsControl(value) {
				return compiledMatch{}, fmt.Errorf("model match value is too long")
			}
		}
	}
	if len(match.Or) > 0 {
		count++
	}
	if count != 1 {
		return compiledMatch{}, fmt.Errorf("model match must contain exactly one predicate")
	}
	result := compiledMatch{match: match}
	if match.Regex != "" {
		compiled, err := regexp.Compile(match.Regex)
		if err != nil {
			return compiledMatch{}, fmt.Errorf("invalid model regex")
		}
		result.regex = compiled
	}
	if len(match.Or) > 0 {
		if len(match.Or) > 64 {
			return compiledMatch{}, fmt.Errorf("model match has too many alternatives")
		}
		result.or = make([]compiledMatch, 0, len(match.Or))
		for _, alternative := range match.Or {
			compiled, err := compileMatch(alternative, options, depth+1)
			if err != nil {
				return compiledMatch{}, err
			}
			result.or = append(result.or, compiled)
		}
	}
	return result, nil
}

func (match compiledMatch) matches(model string) bool {
	switch {
	case match.match.Equals != "":
		return model == match.match.Equals
	case match.match.StartsWith != "":
		return strings.HasPrefix(model, match.match.StartsWith)
	case match.match.Contains != "":
		return strings.Contains(model, match.match.Contains)
	case match.regex != nil:
		return match.regex.MatchString(model)
	default:
		for _, alternative := range match.or {
			if alternative.matches(model) {
				return true
			}
		}
		return false
	}
}

func cloneProvider(provider ProviderPrices) ProviderPrices {
	models := make([]ModelPrice, len(provider.Models))
	for index, model := range provider.Models {
		models[index] = cloneModel(model)
	}
	provider.Models = models
	return provider
}

func cloneModel(model ModelPrice) ModelPrice {
	model.Match = cloneMatch(model.Match)
	model.Prices = cloneRates(model.Prices)
	return model
}

func cloneMatch(match ModelMatch) ModelMatch {
	if len(match.Or) > 0 {
		alternatives := make([]ModelMatch, len(match.Or))
		for index, alternative := range match.Or {
			alternatives[index] = cloneMatch(alternative)
		}
		match.Or = alternatives
	}
	return match
}

func cloneRates(rates Rates) Rates {
	clone := func(value *float64) *float64 {
		if value == nil {
			return nil
		}
		return new(*value)
	}
	rates.InputMTok = clone(rates.InputMTok)
	rates.OutputMTok = clone(rates.OutputMTok)
	rates.CacheReadMTok = clone(rates.CacheReadMTok)
	rates.CacheWriteMTok = clone(rates.CacheWriteMTok)
	rates.CacheWrite5mMTok = clone(rates.CacheWrite5mMTok)
	rates.CacheWrite1hMTok = clone(rates.CacheWrite1hMTok)
	rates.OutputReasoningMTok = clone(rates.OutputReasoningMTok)
	rates.InputAudioMTok = clone(rates.InputAudioMTok)
	rates.OutputAudioMTok = clone(rates.OutputAudioMTok)
	return rates
}

// PricerOptions configures provider aliases and providers whose non-API calls
// must never be estimated. Zero values include common aliases and local Ollama.
type PricerOptions struct {
	Aliases                 map[string]string
	UnpriceableProviders    []string
	AllowCrossProviderMatch bool
}

// Pricer applies a primary catalog first, then local overrides, then bundled
// stopgaps. Overrides therefore cannot replace an authoritative primary rate.
type Pricer struct {
	primary                 *Catalog
	local                   *Catalog
	bundled                 *Catalog
	aliases                 map[string]string
	unpriceable             map[string]struct{}
	allowCrossProviderMatch bool
}

// NewPricer constructs an immutable pricer. Nil catalogs are empty.
func NewPricer(primary, bundled, local *Catalog, options PricerOptions) *Pricer {
	aliases := map[string]string{
		"azure_openai": "azure", "google_genai": "google", "google_vertexai": "google",
		"bedrock": "aws", "bedrock_converse": "aws", "anthropic_bedrock": "aws", "anthropic_vertex": "google",
		"mistralai": "mistral", "xai": "x-ai",
	}
	for from, to := range options.Aliases {
		from, to = strings.TrimSpace(strings.ToLower(from)), strings.TrimSpace(strings.ToLower(to))
		if from == "" || to == "" || len(from) > 256 || len(to) > 256 {
			panic("dacost: invalid provider alias")
		}
		aliases[from] = to
	}
	unpriceable := map[string]struct{}{"ollama": {}}
	for _, provider := range options.UnpriceableProviders {
		provider = strings.TrimSpace(strings.ToLower(provider))
		if provider == "" || len(provider) > 256 {
			panic("dacost: invalid unpriceable provider")
		}
		unpriceable[provider] = struct{}{}
	}
	return &Pricer{
		primary: primary, bundled: bundled, local: local, aliases: aliases,
		unpriceable: unpriceable, allowCrossProviderMatch: options.AllowCrossProviderMatch,
	}
}

// Estimate implements Estimator.
func (pricer *Pricer) Estimate(provider, model string, usage damessage.Usage) (float64, bool) {
	if pricer == nil {
		return 0, false
	}
	model = strings.TrimSpace(model)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if model == "" || len(model) > 4096 || len(provider) > 4096 || containsControl(model) || containsControl(provider) {
		return 0, false
	}
	if alias := pricer.aliases[provider]; alias != "" {
		provider = alias
	}
	if _, blocked := pricer.unpriceable[provider]; blocked {
		return 0, false
	}
	for _, catalog := range []*Catalog{pricer.primary, pricer.local, pricer.bundled} {
		if rates, ok := catalog.lookup(provider, model, pricer.allowCrossProviderMatch); ok {
			return estimateRates(provider, usage, rates)
		}
	}
	return 0, false
}

func (catalog *Catalog) lookup(provider, model string, allowCrossProvider bool) (Rates, bool) {
	if catalog == nil {
		return Rates{}, false
	}
	for _, candidate := range catalog.providers {
		if strings.EqualFold(candidate.provider.ID, provider) {
			for _, priced := range candidate.models {
				if priced.match.matches(model) {
					return priced.model.Prices, true
				}
			}
			return Rates{}, false
		}
	}
	if provider != "" && !allowCrossProvider {
		return Rates{}, false
	}
	for _, candidate := range catalog.providers {
		for _, priced := range candidate.models {
			if priced.match.matches(model) {
				return priced.model.Prices, true
			}
		}
	}
	return Rates{}, false
}

func estimateRates(provider string, usage damessage.Usage, rates Rates) (float64, bool) {
	input, output := inclusiveInputTokens(usage), max(usage.OutputTokens, 0)
	if input == 0 && output == 0 {
		return 0, false
	}
	read, writes := CacheTokenCounts(usage)
	remainingInput := input
	cost := 0.0
	priceBucket := func(count int, rate *float64, remaining *int) {
		if rate == nil || count <= 0 {
			return
		}
		count = min(count, *remaining)
		*remaining -= count
		cost += float64(count) * *rate / 1_000_000
	}
	priceBucket(read, rates.CacheReadMTok, &remainingInput)
	priceBucket(writes.Generic, rates.CacheWriteMTok, &remainingInput)
	fiveRate := rates.CacheWrite5mMTok
	if fiveRate == nil {
		fiveRate = rates.CacheWriteMTok
	}
	priceBucket(writes.FiveMinute, fiveRate, &remainingInput)
	hourRate := rates.CacheWrite1hMTok
	if hourRate == nil {
		hourRate = rates.CacheWriteMTok
	}
	priceBucket(writes.OneHour, hourRate, &remainingInput)
	inputAudio := min(positiveDetail(usage.InputDetails, "audio"), input)
	if read == 0 && writes == (CacheWrites{}) {
		priceBucket(inputAudio, rates.InputAudioMTok, &remainingInput)
	}
	if rates.InputMTok != nil {
		cost += float64(remainingInput) * *rates.InputMTok / 1_000_000
	}

	reasoning := min(positiveDetail(usage.OutputDetails, "reasoning"), output)
	if provider == "perplexity" {
		output += reasoning
	}
	remainingOutput := output
	priceBucket(reasoning, rates.OutputReasoningMTok, &remainingOutput)
	outputAudio := min(positiveDetail(usage.OutputDetails, "audio"), output)
	if reasoning == 0 {
		priceBucket(outputAudio, rates.OutputAudioMTok, &remainingOutput)
	}
	if rates.OutputMTok != nil {
		cost += float64(remainingOutput) * *rates.OutputMTok / 1_000_000
	}
	if !finiteNonnegative(cost) || math.IsInf(cost, 0) || cost > maxEstimatedCostUSD {
		return 0, false
	}
	return cost, true
}
