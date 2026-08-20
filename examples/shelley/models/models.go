package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	dopenai "github.com/semistrict/dago/daproviders/openai"
	dopenrouter "github.com/semistrict/dago/daproviders/openrouter"
	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/db/generated"
	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/llm/llmhttp"
	"github.com/semistrict/dago/examples/shelley/loop"
	"github.com/semistrict/dago/examples/shelley/models/modelsdev"
)

// Provider identifies an LLM upstream API family.
type Provider string

const (
	ProviderOpenAI     Provider = "openai"
	ProviderOpenRouter Provider = "openrouter"
	ProviderBuiltIn    Provider = "builtin"
)

// SourceCustomLabel is the label used for custom (DB-backed) models.
const SourceCustomLabel = "custom"

// Provider-default BARE base URLs (origins). API-protocol path suffixes
// like "/v1" or "/v1/messages" are appended by the per-API-type service
// factory in Model.Build — keeping that knowledge out of the catalog
// and out of any caller that hands a baseURL to Build.
const (
	DefaultOpenAIBaseURL = "https://api.openai.com"
)

// APIType identifies the wire protocol Shelley uses to talk to a model.
// Multiple APIType values can share a Provider (notably OpenAI: Responses
// API vs. Chat Completions).
type APIType string

const (
	APITypeOpenAIResponses     APIType = "openai-responses"
	APITypeOpenRouterResponses APIType = "openrouter-responses"
	APITypeBuiltIn             APIType = "builtin"
)

// Model is one entry in Shelley's catalog of built-in models.
type Model struct {
	// ID is the user-facing identifier.
	ID string

	// Provider is the upstream API family.
	Provider Provider

	// Description is a human-readable description.
	Description string

	// Tags is a comma-separated list of tags (e.g. "slug").
	Tags string

	// APIModelName is the model name sent on the wire (e.g. "claude-opus-4-7").
	// Also used to match against an LLM integration's /v1/models allow-list.
	APIModelName string

	// APIType identifies the wire protocol used to talk to this model.
	APIType APIType

	// DefaultBaseURL is the base URL the provider package uses when no
	// explicit URL is configured. Shown in `shelley models` so users can
	// see exactly which endpoint each model will be reached at.
	DefaultBaseURL string

	// Build constructs a native dago chat model given a BARE base
	// URL (origin + any non-API prefix, e.g. "https://api.example.test"
	// or "" for the provider package default), an API key, and an HTTP
	// client. The function is responsible for appending its own
	// API-protocol path ("/v1", "/v1/messages", "/v1beta", ...) — the
	// caller never encodes those.
	Build func(baseURL, apiKey string, httpc *http.Client) (damodel.Chat, error)
}

// Built is a ready-to-use model, shaped to mirror a row in the custom
// models database table. The Manager treats built-in and custom models
// uniformly via this struct.
type Built struct {
	ID          string
	DisplayName string
	Provider    Provider
	Source      string // human-readable origin ("$OPENAI_API_KEY", "custom", ...)
	Tags        string
	Chat        damodel.Chat

	// APIType is the wire protocol used to talk to this model.
	APIType APIType

	// BaseURL is the resolved upstream base URL (after applying any source
	// override on top of the catalog's DefaultBaseURL).
	BaseURL string
}

// Config holds runtime configuration for the Manager. Built-in models
// are passed in pre-materialized; custom models are loaded from DB.
type Config struct {
	Logger *slog.Logger

	// DB holds custom models; optional.
	DB *db.DB

	// HTTPC is the shared HTTP client used to back custom models loaded
	// from DB. If nil, a default llmhttp client is created.
	HTTPC *http.Client
}

// --- Catalog ---------------------------------------------------------------

const defaultOpenAIMaxOutputTokens = 32768

// OpenAIResponsesOptions describes capabilities attached to a native Responses model.
type OpenAIResponsesOptions struct {
	ContextWindow         int
	MaxOutputTokens       int
	SupportsImages        bool
	SupportsReasoning     bool
	SupportsWebSearch     bool
	UseSimplifiedPatch    bool
	MaxImageBytes         int
	DefaultReasoningLevel string
	ReasoningEffort       string
}

// NewOpenAIResponses constructs a native dago Responses model.
func NewOpenAIResponses(apiKey, modelID, baseURL string, httpClient *http.Client, options OpenAIResponsesOptions) damodel.Chat {
	validateOpenAIResponsesOptions(options)
	effort := options.ReasoningEffort
	if effort == "" {
		effort = options.DefaultReasoningLevel
	}
	var defaultReasoning *damodel.Reasoning
	if options.SupportsReasoning && effort != "" && effort != "off" {
		defaultReasoning = &damodel.Reasoning{Effort: effort, Summary: "auto"}
	}
	clientOptions := dopenai.Options{
		BaseURL: openAIResponsesBaseURL(baseURL), HTTPClient: httpClient,
		ContextWindow: options.ContextWindow, MaxOutputTokens: options.MaxOutputTokens,
		DefaultReasoning: defaultReasoning, WebSearch: options.SupportsWebSearch,
	}
	chat := dopenai.NewAPIKey(apiKey, modelID, clientOptions)
	return damodel.WithProfile(chat, func(profile *damodel.Profile) {
		profile.SupportsImages = options.SupportsImages
		profile.SupportsReasoning = options.SupportsReasoning
		profile.SupportsWebSearch = options.SupportsWebSearch
		profile.UseSimplifiedPatch = options.UseSimplifiedPatch
		profile.MaxImageBytes = options.MaxImageBytes
		profile.DefaultReasoningLevel = options.DefaultReasoningLevel
		if options.SupportsReasoning {
			profile.ReasoningLevels = standardReasoningLevels()
		}
	})
}

// OpenRouterResponsesOptions describes capabilities attached to an OpenRouter Responses model.
type OpenRouterResponsesOptions = OpenAIResponsesOptions

// NewOpenRouterResponses constructs a native OpenRouter Responses model.
func NewOpenRouterResponses(apiKey, modelID, baseURL string, httpClient *http.Client, options OpenRouterResponsesOptions) damodel.Chat {
	validateOpenAIResponsesOptions(options)
	effort := options.ReasoningEffort
	if effort == "" {
		effort = options.DefaultReasoningLevel
	}
	var defaultReasoning *damodel.Reasoning
	if options.SupportsReasoning && effort != "" && effort != "off" {
		defaultReasoning = &damodel.Reasoning{Effort: effort, Summary: "auto"}
	}
	requireParameters := true
	chat := dopenrouter.New(apiKey, modelID, dopenrouter.Options{
		BaseURL: openRouterResponsesBaseURL(baseURL), HTTPClient: httpClient,
		ContextWindow: options.ContextWindow, MaxOutputTokens: options.MaxOutputTokens,
		DefaultReasoning: defaultReasoning, WebSearch: options.SupportsWebSearch,
		AppTitle: "Shelley",
		Routing:  &dopenrouter.ProviderRouting{RequireParameters: &requireParameters},
	})
	return damodel.WithProfile(chat, func(profile *damodel.Profile) {
		profile.SupportsImages = options.SupportsImages
		profile.SupportsReasoning = options.SupportsReasoning
		profile.SupportsWebSearch = options.SupportsWebSearch
		profile.UseSimplifiedPatch = options.UseSimplifiedPatch
		profile.MaxImageBytes = options.MaxImageBytes
		profile.DefaultReasoningLevel = options.DefaultReasoningLevel
		if options.SupportsReasoning {
			profile.ReasoningLevels = standardReasoningLevels()
		}
	})
}

func validateOpenAIResponsesOptions(options OpenAIResponsesOptions) {
	if options.ContextWindow < 0 || options.MaxOutputTokens < 0 || options.MaxImageBytes < 0 {
		panic("shelley models: model limits cannot be negative")
	}
	if _, err := validateReasoningEffort(options.DefaultReasoningLevel); err != nil {
		panic("shelley models: invalid default reasoning level")
	}
	if _, err := validateReasoningEffort(options.ReasoningEffort); err != nil {
		panic("shelley models: invalid reasoning effort")
	}
}

func validateReasoningEffort(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", "off", "none", "minimal", "low", "medium", "high", "xhigh":
		return value, nil
	default:
		return "", fmt.Errorf("reasoning effort must be off, none, minimal, low, medium, high, or xhigh; got %q", value)
	}
}

func validateSupportSetting(field, value string) (string, error) {
	switch value {
	case "", "auto":
		return "auto", nil
	case "yes", "no":
		return value, nil
	default:
		return "", fmt.Errorf("%s must be auto, yes, or no; got %q", field, value)
	}
}

// openAIResponsesModel constructs one catalog entry for OpenAI's Responses API.
//
// The `baseURL` parameter is a BARE origin/prefix with NO API-protocol
// path on it. The native builder owns protocol URL normalization.
func openAIResponsesModel(id, description string, contextWindow int, supportsReasoning bool) Model {
	return Model{
		ID: id, Provider: ProviderOpenAI, Description: description,
		APIModelName: id, APIType: APITypeOpenAIResponses,
		DefaultBaseURL: DefaultOpenAIBaseURL,
		Build: func(baseURL, apiKey string, httpc *http.Client) (damodel.Chat, error) {
			return NewOpenAIResponses(apiKey, id, baseURL, httpc, OpenAIResponsesOptions{
				ContextWindow: contextWindow, MaxOutputTokens: defaultOpenAIMaxOutputTokens,
				SupportsImages: true, SupportsReasoning: supportsReasoning,
				SupportsWebSearch: true, MaxImageBytes: 20 * 1024 * 1024,
				DefaultReasoningLevel: "medium",
			}), nil
		},
	}
}

func openAIResponsesBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return ""
	}
	value = strings.TrimSuffix(value, "/responses")
	if strings.HasSuffix(value, "/v1") {
		return value
	}
	return value + "/v1"
}

func openRouterResponsesBaseURL(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return ""
	}
	return strings.TrimSuffix(value, "/responses")
}

func standardReasoningLevels() []string {
	return []string{"off", "minimal", "low", "medium", "high", "xhigh"}
}

// All returns all available models in Shelley.
//
// Order is significant: it is the display order in the model picker and, when
// no default is configured, the first ready model is the default. Integrations
// supply their own ordered catalogs instead of using this order.
//
// Models are organized by "family" — the usual notion of a model lineage from
// one provider/trainer (the "Opus" line, the "GPT-5" line, and so on).
//
// Only the newest release in a family holds that family's flagship slot near
// the top of the list. Older releases in the same family are obviated by the
// newer one and drop into the secondary group lower down (they stay selectable,
// just deprioritized). A different family is never obviated by a higher-numbered
// release of another family, so each family keeps its own flagship slot.
//
// There is one surprising wrinkle: Opus <= 4.6 and Opus >= 4.7 are treated as
// two separate families even though both are "Opus". The reason is the
// tokenizer. Opus 4.7 introduced a new tokenizer (inherited by 4.8) that emits
// more tokens for the same text — the per-token rates are identical across 4.6,
// 4.7, and 4.8, but the same prompt costs more under 4.7/4.8. That difference is
// large enough that we keep 4.6 in its own flagship slot rather than letting it
// be obviated by 4.7/4.8.
//
// When adding a newer release of an existing family, put it in the family's
// flagship slot and move the prior release down into the secondary group.
func All() []Model {
	return []Model{
		openAIResponsesModel("gpt-5.6-sol", "GPT-5.6 Sol", 272000, true),
		openAIResponsesModel("gpt-5.6-terra", "GPT-5.6 Terra", 272000, true),
		openAIResponsesModel("gpt-5.6-luna", "GPT-5.6 Luna", 272000, true),
		openAIResponsesModel("gpt-5.5", "GPT-5.5", 272000, false),
		openAIResponsesModel("gpt-5.4", "GPT-5.4", 304000, false),
		openAIResponsesModel("gpt-5.4-mini", "GPT-5.4 mini", 304000, false),
		openAIResponsesModel("gpt-5.4-nano", "GPT-5.4 nano", 304000, false),
		openAIResponsesModel("gpt-5.3-codex", "GPT-5.3 Codex", 288000, false),
		{
			ID: "predictable", Provider: ProviderBuiltIn,
			Description:    "Deterministic test model (no API key)",
			APIType:        APITypeBuiltIn,
			DefaultBaseURL: "-",
			Build: func(url, apiKey string, httpc *http.Client) (damodel.Chat, error) {
				return loop.NewPredictableService(), nil
			},
		},
	}
}

// ByID returns the model with the given ID, or nil if not found.
func ByID(id string) *Model {
	for _, m := range All() {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

// Default returns the default catalog model.
func Default() Model {
	if m := ByID("gpt-5.6-sol"); m != nil {
		return *m
	}
	return All()[0]
}

// --- Manager ---------------------------------------------------------------

// Manager owns the live set of LLM services for a Shelley server.
type Manager struct {
	mu         sync.RWMutex
	models     map[string]modelEntry
	modelOrder []string
	logger     *slog.Logger
	db         *db.DB
	httpc      *http.Client
}

type modelEntry struct {
	chat        damodel.Chat
	provider    Provider
	modelID     string
	source      string
	displayName string
	tags        string
	baseURL     string
	apiType     APIType
}

// loggingChat wraps a native chat model with request and usage logging.
type loggingChat struct {
	chat     damodel.Chat
	logger   *slog.Logger
	modelID  string
	provider Provider
}

func (l *loggingChat) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	start := time.Now()
	response, err := l.chat.Invoke(ctx, request)
	l.finish(ctx, start, response.Message.Usage, err)
	return response, err
}

func (l *loggingChat) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	start := time.Now()
	stream, err := l.chat.Stream(ctx, request)
	if err != nil {
		l.finish(ctx, start, nil, err)
		return nil, err
	}
	return &loggingStream{Stream: stream, owner: l, ctx: ctx, started: start}, nil
}

func (l *loggingChat) Profile() damodel.Profile { return l.chat.Profile() }

func (l *loggingChat) BindTools(definitions []datool.Definition) (damodel.Chat, error) {
	binder, ok := l.chat.(damodel.Binder)
	if !ok {
		return l, nil
	}
	bound, err := binder.BindTools(definitions)
	if err != nil {
		return nil, err
	}
	copy := *l
	copy.chat = bound
	return &copy, nil
}

func (l *loggingChat) finish(ctx context.Context, started time.Time, native *dmessage.Usage, err error) {
	attrs := []any{"model", l.modelID, "duration_seconds", time.Since(started).Seconds()}
	if err != nil {
		attrs = append(attrs, "error", err)
		l.logger.Error("LLM request failed", attrs...)
		return
	}
	usage := legacyUsage(native, l.modelID, string(l.provider))
	if !usage.IsZero() {
		attrs = append(attrs, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "cost_usd", usage.CostUSD)
	}
	l.logger.Info("LLM request completed", attrs...)
	if purpose := llmhttp.PurposeFromContext(ctx); purpose != "" && !usage.IsZero() {
		if collect := llmhttp.UsageCollectorFromContext(ctx); collect != nil {
			collect(purpose, usage)
		}
	}
}

type loggingStream struct {
	damodel.Stream
	owner   *loggingChat
	ctx     context.Context
	started time.Time
	usage   *dmessage.Usage
	done    bool
}

func (stream *loggingStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *loggingStream) Next(ctx context.Context) (damodel.Chunk, error) {
	chunk, err := stream.Stream.Next(ctx)
	if chunk.MessageDelta.Usage != nil {
		usage := *chunk.MessageDelta.Usage
		stream.usage = &usage
	}
	if err != nil && !stream.done {
		stream.done = true
		if err == io.EOF {
			stream.owner.finish(stream.ctx, stream.started, stream.usage, nil)
		} else {
			stream.owner.finish(stream.ctx, stream.started, stream.usage, err)
		}
	}
	return chunk, err
}

func legacyUsage(native *dmessage.Usage, modelID, _ string) llm.Usage {
	if native == nil {
		return llm.Usage{}
	}
	usage := llm.Usage{InputTokens: uint64(native.InputTokens), OutputTokens: uint64(native.OutputTokens), CostUSD: native.CostUSD, Model: native.Model, URL: native.URL}
	if usage.Model == "" {
		usage.Model = modelID
	}
	usage.CacheReadInputTokens = uint64(native.InputDetails["cache_read"])
	return usage
}

// NewManager registers the supplied built-in models, then loads custom
// models from cfg.DB.
func NewManager(built []Built, cfg Config) (*Manager, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	httpc := cfg.HTTPC
	if httpc == nil {
		httpc = llmhttp.NewClient(nil)
	}
	m := &Manager{
		models: map[string]modelEntry{},
		logger: logger,
		db:     cfg.DB,
		httpc:  httpc,
	}

	m.registerBuiltModelsLocked(built)

	if err := m.loadCustomModels(); err != nil {
		return nil, fmt.Errorf("load custom models: %w", err)
	}
	return m, nil
}

func (m *Manager) registerBuiltModelsLocked(built []Built) {
	for _, b := range built {
		if strings.TrimSpace(b.ID) == "" || nilChat(b.Chat) {
			panic("shelley models: built model ID and chat are required")
		}
		if _, exists := m.models[b.ID]; exists {
			panic("shelley models: duplicate built model " + b.ID)
		}
		dn := b.DisplayName
		if dn == "" {
			dn = b.ID
		}
		m.models[b.ID] = modelEntry{
			chat:        b.Chat,
			provider:    b.Provider,
			modelID:     b.ID,
			source:      b.Source,
			displayName: dn,
			tags:        b.Tags,
			baseURL:     b.BaseURL,
			apiType:     b.APIType,
		}
		m.modelOrder = append(m.modelOrder, b.ID)
		if m.logger != nil {
			m.logger.Info("Registered model", "id", b.ID, "source", b.Source)
		}
	}
}

func nilChat(chat damodel.Chat) bool {
	if chat == nil {
		return true
	}
	value := reflect.ValueOf(chat)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (m *Manager) customModelRows() ([]generated.Model, error) {
	if m.db == nil {
		return nil, nil
	}
	return m.db.GetModels(context.Background())
}

func (m *Manager) loadCustomModels() error {
	dbModels, err := m.customModelRows()
	if err != nil {
		return err
	}
	return m.loadCustomModelsLocked(dbModels)
}

func (m *Manager) loadCustomModelsLocked(dbModels []generated.Model) error {
	for _, model := range dbModels {
		if _, exists := m.models[model.ModelID]; exists {
			continue
		}
		chat, err := m.createChatFromModel(&model)
		if err != nil {
			return fmt.Errorf("configure custom model %q: %w", model.ModelID, err)
		}
		m.models[model.ModelID] = modelEntry{
			chat:        chat,
			provider:    providerForAPIType(model.ProviderType),
			modelID:     model.ModelID,
			source:      SourceCustomLabel,
			displayName: model.DisplayName,
			tags:        model.Tags,
		}
		m.modelOrder = append(m.modelOrder, model.ModelID)
	}
	return nil
}

// RefreshCustomModels reloads custom models from the database. Call this
// after adding or removing custom models via the UI.
func (m *Manager) RefreshCustomModels() error {
	dbModels, err := m.customModelRows()
	if err != nil {
		return err
	}
	candidate := &Manager{models: map[string]modelEntry{}, logger: m.logger, db: m.db, httpc: m.httpc}
	m.mu.RLock()
	for _, id := range m.modelOrder {
		entry := m.models[id]
		if entry.source != SourceCustomLabel {
			candidate.models[id] = entry
			candidate.modelOrder = append(candidate.modelOrder, id)
		}
	}
	m.mu.RUnlock()
	if err := candidate.loadCustomModelsLocked(dbModels); err != nil {
		return err
	}
	m.mu.Lock()
	m.models, m.modelOrder = candidate.models, candidate.modelOrder
	m.mu.Unlock()
	return nil
}

// RefreshBuiltModels replaces the non-custom models with a freshly discovered
// built-in/catalog set, then re-applies DB-backed custom models.
func (m *Manager) RefreshBuiltModels(built []Built) error {
	dbModels, err := m.customModelRows()
	if err != nil {
		return err
	}
	candidate := &Manager{models: map[string]modelEntry{}, logger: m.logger, db: m.db, httpc: m.httpc}
	candidate.registerBuiltModelsLocked(built)
	if err := candidate.loadCustomModelsLocked(dbModels); err != nil {
		return err
	}
	m.mu.Lock()
	m.models, m.modelOrder = candidate.models, candidate.modelOrder
	m.mu.Unlock()
	return nil
}

// GetChat returns the native model contract for modelID.
func (m *Manager) GetChat(modelID string) (damodel.Chat, error) {
	m.mu.RLock()
	entry, ok := m.models[modelID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unsupported model: %s", modelID)
	}
	if entry.chat == nil {
		return nil, fmt.Errorf("model %s has no chat implementation", modelID)
	}
	if m.logger != nil {
		return &loggingChat{chat: entry.chat, logger: m.logger, modelID: entry.modelID, provider: entry.provider}, nil
	}
	return entry.chat, nil
}

func (m *Manager) GetAvailableModels() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]string, len(m.modelOrder))
	copy(result, m.modelOrder)
	return result
}

func (m *Manager) HasModel(modelID string) bool {
	m.mu.RLock()
	_, ok := m.models[modelID]
	m.mu.RUnlock()
	return ok
}

// ModelInfo contains display name, tags, source, base URL, and API type for a model.
type ModelInfo struct {
	DisplayName string
	Tags        string
	Source      string
	BaseURL     string
	APIType     string
	Profile     damodel.Profile
}

func (m *Manager) GetModelInfo(modelID string) *ModelInfo {
	m.mu.RLock()
	entry, ok := m.models[modelID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return &ModelInfo{DisplayName: entry.displayName, Tags: entry.tags, Source: entry.source, BaseURL: entry.baseURL, APIType: string(entry.apiType), Profile: entry.chat.Profile()}
}

type reasoningMapping struct {
	level  llm.ThinkingLevel
	effort string
}

type reasoningChat struct {
	damodel.Chat
	supported     bool
	levels        []llm.ThinkingLevel
	mapping       map[llm.ThinkingLevel]reasoningMapping
	defaultSource llm.ThinkingLevel
}

func (chat *reasoningChat) Profile() damodel.Profile {
	profile := chat.Chat.Profile()
	profile.SupportsReasoning = chat.supported
	profile.ReasoningLevels = nil
	profile.DefaultReasoningLevel = "off"
	if chat.supported {
		for _, level := range chat.levels {
			profile.ReasoningLevels = append(profile.ReasoningLevels, level.Name())
		}
		if len(profile.ReasoningLevels) == 0 {
			profile.ReasoningLevels = standardReasoningLevels()
		}
		if mapped, ok := chat.mapping[chat.defaultSource]; ok {
			profile.DefaultReasoningLevel = nativeReasoningEffort(mapped)
		} else if profile.DefaultReasoningLevel == "" || profile.DefaultReasoningLevel == "off" {
			profile.DefaultReasoningLevel = chat.defaultSource.Name()
		}
	}
	return profile
}

func (chat *reasoningChat) BindTools(definitions []datool.Definition) (damodel.Chat, error) {
	binder, ok := chat.Chat.(damodel.Binder)
	if !ok {
		return chat, nil
	}
	bound, err := binder.BindTools(definitions)
	if err != nil {
		return nil, err
	}
	copy := *chat
	copy.Chat = bound
	return &copy, nil
}

func (chat *reasoningChat) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	configured, err := chat.configure(request)
	if err != nil {
		return damodel.Response{}, err
	}
	return chat.Chat.Invoke(ctx, configured)
}

func (chat *reasoningChat) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	configured, err := chat.configure(request)
	if err != nil {
		return nil, err
	}
	return chat.Chat.Stream(ctx, configured)
}

func (chat *reasoningChat) configure(request damodel.Request) (damodel.Request, error) {
	copyRequest := request
	if !chat.supported {
		copyRequest.Reasoning = nil
		return copyRequest, nil
	}
	if request.Reasoning == nil {
		if mapped, ok := chat.mapping[chat.defaultSource]; ok {
			copyRequest.Reasoning = &damodel.Reasoning{Effort: nativeReasoningEffort(mapped)}
		}
		return copyRequest, nil
	}
	if len(chat.mapping) == 0 {
		return copyRequest, nil
	}
	level, err := llm.ParseThinkingLevelStrict(request.Reasoning.Effort)
	if err != nil {
		return damodel.Request{}, err
	}
	mapped, ok := chat.mapping[level]
	if !ok {
		return damodel.Request{}, fmt.Errorf("reasoning level %q is not supported by this model", request.Reasoning.Effort)
	}
	reasoning := *request.Reasoning
	reasoning.Effort = nativeReasoningEffort(mapped)
	copyRequest.Reasoning = &reasoning
	return copyRequest, nil
}

func nativeReasoningEffort(mapped reasoningMapping) string {
	if mapped.effort != "" {
		return mapped.effort
	}
	return mapped.level.ThinkingEffort()
}

// WrapReasoningConfig applies custom-model capability and level mapping.
func WrapReasoningConfig(chat damodel.Chat, endpoint, modelName, support, rawMap string) (damodel.Chat, error) {
	if nilChat(chat) {
		return nil, errors.New("reasoning model is required")
	}
	canonicalSupport, err := validateSupportSetting("reasoning support", support)
	if err != nil {
		return nil, err
	}
	supported, err := ResolveSupportsReasoning(endpoint, modelName, canonicalSupport)
	if err != nil {
		return nil, err
	}
	mapping, levels, err := parseReasoningMap(rawMap)
	if err != nil {
		return nil, err
	}
	defaultSource, err := llm.ParseThinkingLevelStrict(chat.Profile().DefaultReasoningLevel)
	if err != nil {
		return nil, fmt.Errorf("default reasoning level: %w", err)
	}
	if defaultSource == llm.ThinkingLevelDefault {
		defaultSource = llm.ThinkingLevelMedium
	}
	return &reasoningChat{Chat: chat, supported: supported, levels: levels, mapping: mapping, defaultSource: defaultSource}, nil
}

func parseReasoningMap(raw string) (map[llm.ThinkingLevel]reasoningMapping, []llm.ThinkingLevel, error) {
	if raw == "" {
		return nil, nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, nil, fmt.Errorf("parse reasoning map: %w", err)
	}
	mapping := make(map[llm.ThinkingLevel]reasoningMapping, len(values))
	levels := make([]llm.ThinkingLevel, 0, len(values))
	for _, level := range []llm.ThinkingLevel{llm.ThinkingLevelOff, llm.ThinkingLevelMinimal, llm.ThinkingLevelLow, llm.ThinkingLevelMedium, llm.ThinkingLevelHigh, llm.ThinkingLevelXHigh} {
		mappedName, ok := values[level.Name()]
		if !ok || mappedName == "" {
			continue
		}
		mapped, err := llm.ParseThinkingLevelStrict(mappedName)
		if err != nil {
			return nil, nil, fmt.Errorf("reasoning map %q: %w", level.Name(), err)
		}
		mapping[level] = reasoningMapping{level: mapped}
		levels = append(levels, level)
	}
	if len(mapping) != len(values) {
		return nil, nil, errors.New("reasoning map contains an unsupported source level")
	}
	return mapping, levels, nil
}

// createChatFromModel creates a native chat model from database configuration.
func (m *Manager) createChatFromModel(model *generated.Model) (damodel.Chat, error) {
	if strings.TrimSpace(model.ApiKey) == "" || strings.TrimSpace(model.ModelName) == "" {
		return nil, fmt.Errorf("custom model API key and model name are required")
	}
	if model.MaxTokens < 0 {
		return nil, fmt.Errorf("custom model max tokens cannot be negative")
	}
	imageSupport, err := validateSupportSetting("image support", model.ImageSupport)
	if err != nil {
		return nil, err
	}
	reasoningSupport, err := validateSupportSetting("reasoning support", model.ReasoningSupport)
	if err != nil {
		return nil, err
	}
	supportsImages, err := ResolveSupportsImages(model.Endpoint, model.ModelName, imageSupport)
	if err != nil {
		return nil, err
	}
	effort := model.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	if _, err := validateReasoningEffort(effort); err != nil {
		return nil, err
	}
	options := OpenAIResponsesOptions{
		MaxOutputTokens: int(model.MaxTokens), SupportsImages: supportsImages,
		SupportsReasoning: true,
		MaxImageBytes:     20 * 1024 * 1024, DefaultReasoningLevel: "medium", ReasoningEffort: effort,
	}
	var chat damodel.Chat
	switch APIType(model.ProviderType) {
	case APITypeOpenAIResponses:
		options.SupportsWebSearch = true
		chat = NewOpenAIResponses(model.ApiKey, model.ModelName, model.Endpoint, m.httpc, options)
	case APITypeOpenRouterResponses:
		chat = NewOpenRouterResponses(model.ApiKey, model.ModelName, model.Endpoint, m.httpc, options)
	default:
		return nil, fmt.Errorf("unknown provider type %q", model.ProviderType)
	}
	return WrapReasoningConfig(chat, model.Endpoint, model.ModelName, reasoningSupport, model.ReasoningMap)
}

func providerForAPIType(value string) Provider {
	switch APIType(value) {
	case APITypeOpenAIResponses:
		return ProviderOpenAI
	case APITypeOpenRouterResponses:
		return ProviderOpenRouter
	default:
		return Provider(value)
	}
}

// ResolveSupportsImages turns a stored image_support value ("auto"|"yes"|"no")
// into a SupportsImages bool. "auto" is resolved from the model's endpoint URL
// and name; unknown models default to allowing images. Invalid stored enum
// values return an error rather than enabling a capability.
func ResolveSupportsImages(endpoint, modelName, imageSupport string) (bool, error) {
	switch imageSupport {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	case "auto", "":
		supported, found := modelsdev.LookupImageSupport(endpoint, modelName)
		if !found {
			return true, nil
		}
		return supported, nil
	default:
		return false, fmt.Errorf("invalid image support %q: want auto, yes, or no", imageSupport)
	}
}

// ResolveSupportsReasoning turns a stored reasoning_support value
// ("auto"|"yes"|"no") into a capability boolean. Unknown models default to
// supporting reasoning so custom endpoints remain configurable. Invalid stored
// enum values return an error rather than enabling a capability.
func ResolveSupportsReasoning(endpoint, modelName, reasoningSupport string) (bool, error) {
	switch reasoningSupport {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	case "auto", "":
		supported, found := modelsdev.LookupReasoningSupport(endpoint, modelName)
		if !found {
			return true, nil
		}
		return supported, nil
	default:
		return false, fmt.Errorf("invalid reasoning support %q: want auto, yes, or no", reasoningSupport)
	}
}
