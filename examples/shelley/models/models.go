package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"
	dopenai "github.com/semistrict/dago/providers/openai"
	dtool "github.com/semistrict/dago/tool"

	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/llmhttp"
	"shelley.exe.dev/loop"
	"shelley.exe.dev/models/modelsdev"
)

// Provider identifies an LLM upstream API family.
type Provider string

const (
	ProviderOpenAI  Provider = "openai"
	ProviderBuiltIn Provider = "builtin"
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
	APITypeOpenAIResponses APIType = "openai-responses"
	APITypeBuiltIn         APIType = "builtin"
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

	// Build constructs a native Dago chat model given a BARE base
	// URL (origin + any non-API prefix, e.g. "https://llm.int.exe.xyz"
	// or "" for the provider package default), an API key, and an HTTP
	// client. The function is responsible for appending its own
	// API-protocol path ("/v1", "/v1/messages", "/v1beta", ...) — the
	// caller never encodes those.
	Build func(baseURL, apiKey string, httpc *http.Client) (dmodel.Chat, error)
}

// Built is a ready-to-use model, shaped to mirror a row in the custom
// models database table. The Manager treats built-in and custom models
// uniformly via this struct.
type Built struct {
	ID          string
	DisplayName string
	Provider    Provider
	Source      string // human-readable origin ("exe.dev gateway", "$OPENAI_API_KEY", "custom", ...)
	Tags        string
	Chat        dmodel.Chat

	// APIType is the wire protocol used to talk to this model.
	APIType APIType

	// BaseURL is the resolved upstream base URL (after applying any source
	// override on top of the catalog's DefaultBaseURL).
	BaseURL string
}

// Config holds runtime configuration for the Manager. Built-in models
// are passed in pre-materialized; custom models are loaded from DB.
type Config struct {
	// Models is the set of ready-to-use built-in models, in display order.
	Models []Built

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

// NewOpenAIResponses constructs a native Dago Responses model.
func NewOpenAIResponses(apiKey, modelID, baseURL string, httpClient *http.Client, options OpenAIResponsesOptions) (dmodel.Chat, error) {
	effort := options.ReasoningEffort
	if effort == "" {
		effort = options.DefaultReasoningLevel
	}
	var defaultReasoning *dmodel.Reasoning
	if options.SupportsReasoning && effort != "" && effort != "off" {
		defaultReasoning = &dmodel.Reasoning{Effort: effort, Summary: "auto"}
	}
	chat, err := dopenai.NewAPIKey(apiKey, dopenai.Options{
		Model: modelID, BaseURL: openAIResponsesBaseURL(baseURL), HTTPClient: httpClient,
		ContextWindow: options.ContextWindow, MaxOutputTokens: options.MaxOutputTokens,
		DefaultReasoning: defaultReasoning, WebSearch: options.SupportsWebSearch,
	})
	if err != nil {
		return nil, err
	}
	return WithProfile(chat, func(profile *dmodel.Profile) {
		profile.SupportsImages = options.SupportsImages
		profile.SupportsReasoning = options.SupportsReasoning
		profile.SupportsWebSearch = options.SupportsWebSearch
		profile.UseSimplifiedPatch = options.UseSimplifiedPatch
		profile.MaxImageBytes = options.MaxImageBytes
		profile.DefaultReasoningLevel = options.DefaultReasoningLevel
		if options.SupportsReasoning {
			profile.ReasoningLevels = standardReasoningLevels()
		}
	}), nil
}

// openAIResponsesModel constructs one catalog entry for the sole supported
// external provider protocol.
//
// The `baseURL` parameter is a BARE origin/prefix with NO API-protocol
// path on it. The native builder owns protocol URL normalization.
func openAIResponsesModel(id, description string, contextWindow int, supportsReasoning bool) Model {
	return Model{
		ID: id, Provider: ProviderOpenAI, Description: description,
		APIModelName: id, APIType: APITypeOpenAIResponses,
		DefaultBaseURL: DefaultOpenAIBaseURL,
		Build: func(baseURL, apiKey string, httpc *http.Client) (dmodel.Chat, error) {
			return NewOpenAIResponses(apiKey, id, baseURL, httpc, OpenAIResponsesOptions{
				ContextWindow: contextWindow, MaxOutputTokens: defaultOpenAIMaxOutputTokens,
				SupportsImages: true, SupportsReasoning: supportsReasoning,
				SupportsWebSearch: true, MaxImageBytes: 20 * 1024 * 1024,
				DefaultReasoningLevel: "medium",
			})
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

func standardReasoningLevels() []string {
	return []string{"off", "minimal", "low", "medium", "high", "xhigh"}
}

type profiledChat struct {
	dmodel.Chat
	profile dmodel.Profile
}

// WithProfile returns a native chat model with explicit catalog capabilities.
func WithProfile(chat dmodel.Chat, configure func(*dmodel.Profile)) dmodel.Chat {
	profile := chat.Profile()
	configure(&profile)
	return &profiledChat{Chat: chat, profile: profile}
}

func (chat *profiledChat) Profile() dmodel.Profile {
	profile := chat.profile
	profile.ReasoningLevels = append([]string(nil), profile.ReasoningLevels...)
	return profile
}

func (chat *profiledChat) BindTools(definitions []dtool.Definition) (dmodel.Chat, error) {
	binder, ok := chat.Chat.(dmodel.Binder)
	if !ok {
		return chat, nil
	}
	bound, err := binder.BindTools(definitions)
	if err != nil {
		return nil, err
	}
	return &profiledChat{Chat: bound, profile: chat.profile}, nil
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
			Build: func(url, apiKey string, httpc *http.Client) (dmodel.Chat, error) {
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

// IDs returns all catalog model IDs.
func IDs() []string {
	models := All()
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
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
	chat        dmodel.Chat
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
	chat     dmodel.Chat
	logger   *slog.Logger
	modelID  string
	provider Provider
}

func (l *loggingChat) Invoke(ctx context.Context, request dmodel.Request) (dmodel.Response, error) {
	start := time.Now()
	ctx = llmhttp.WithModelID(ctx, l.modelID)
	ctx = llmhttp.WithProvider(ctx, string(l.provider))
	response, err := l.chat.Invoke(ctx, request)
	l.finish(ctx, start, response.Message.Usage, err)
	return response, err
}

func (l *loggingChat) Stream(ctx context.Context, request dmodel.Request) (dmodel.Stream, error) {
	ctx = llmhttp.WithModelID(ctx, l.modelID)
	ctx = llmhttp.WithProvider(ctx, string(l.provider))
	start := time.Now()
	stream, err := l.chat.Stream(ctx, request)
	if err != nil {
		l.finish(ctx, start, nil, err)
		return nil, err
	}
	return &loggingStream{Stream: stream, owner: l, ctx: ctx, started: start}, nil
}

func (l *loggingChat) Profile() dmodel.Profile { return l.chat.Profile() }

func (l *loggingChat) BindTools(definitions []dtool.Definition) (dmodel.Chat, error) {
	binder, ok := l.chat.(dmodel.Binder)
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
	dmodel.Stream
	owner   *loggingChat
	ctx     context.Context
	started time.Time
	usage   *dmessage.Usage
	done    bool
}

func (stream *loggingStream) Next(ctx context.Context) (dmodel.Chunk, error) {
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
func NewManager(cfg *Config) (*Manager, error) {
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
		logger: cfg.Logger,
		db:     cfg.DB,
		httpc:  httpc,
	}

	m.registerBuiltModelsLocked(cfg.Models)

	if err := m.loadCustomModels(); err != nil && cfg.Logger != nil {
		cfg.Logger.Warn("Failed to load custom models", "error", err)
	}
	return m, nil
}

func (m *Manager) registerBuiltModelsLocked(built []Built) {
	for _, b := range built {
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
	m.loadCustomModelsLocked(dbModels)
	return nil
}

func (m *Manager) loadCustomModelsLocked(dbModels []generated.Model) {
	for _, model := range dbModels {
		if _, exists := m.models[model.ModelID]; exists {
			continue
		}
		chat, err := m.createChatFromModel(&model)
		if err != nil {
			if m.logger != nil {
				m.logger.Error("Could not configure custom model", "model_id", model.ModelID, "error", err)
			}
			continue
		}
		m.models[model.ModelID] = modelEntry{
			chat:        chat,
			provider:    Provider(model.ProviderType),
			modelID:     model.ModelID,
			source:      SourceCustomLabel,
			displayName: model.DisplayName,
			tags:        model.Tags,
		}
		m.modelOrder = append(m.modelOrder, model.ModelID)
	}
}

// RefreshCustomModels reloads custom models from the database. Call this
// after adding or removing custom models via the UI.
func (m *Manager) RefreshCustomModels() error {
	dbModels, err := m.customModelRows()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	newOrder := make([]string, 0, len(m.modelOrder))
	for _, id := range m.modelOrder {
		entry, ok := m.models[id]
		if ok && entry.source != SourceCustomLabel {
			newOrder = append(newOrder, id)
		} else {
			delete(m.models, id)
		}
	}
	m.modelOrder = newOrder
	m.loadCustomModelsLocked(dbModels)
	return nil
}

// RefreshBuiltModels replaces the non-custom models with a freshly discovered
// built-in/catalog set, then re-applies DB-backed custom models.
func (m *Manager) RefreshBuiltModels(built []Built) error {
	dbModels, err := m.customModelRows()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = map[string]modelEntry{}
	m.modelOrder = nil
	m.registerBuiltModelsLocked(built)
	m.loadCustomModelsLocked(dbModels)
	return nil
}

// GetChat returns the native model contract for modelID.
func (m *Manager) GetChat(modelID string) (dmodel.Chat, error) {
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
	Profile     dmodel.Profile
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
	dmodel.Chat
	supported     bool
	levels        []llm.ThinkingLevel
	mapping       map[llm.ThinkingLevel]reasoningMapping
	defaultSource llm.ThinkingLevel
}

func (chat *reasoningChat) Profile() dmodel.Profile {
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

func (chat *reasoningChat) BindTools(definitions []dtool.Definition) (dmodel.Chat, error) {
	binder, ok := chat.Chat.(dmodel.Binder)
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

func (chat *reasoningChat) Invoke(ctx context.Context, request dmodel.Request) (dmodel.Response, error) {
	configured, err := chat.configure(request)
	if err != nil {
		return dmodel.Response{}, err
	}
	return chat.Chat.Invoke(ctx, configured)
}

func (chat *reasoningChat) Stream(ctx context.Context, request dmodel.Request) (dmodel.Stream, error) {
	configured, err := chat.configure(request)
	if err != nil {
		return nil, err
	}
	return chat.Chat.Stream(ctx, configured)
}

func (chat *reasoningChat) configure(request dmodel.Request) (dmodel.Request, error) {
	copyRequest := request
	if !chat.supported {
		copyRequest.Reasoning = nil
		return copyRequest, nil
	}
	if request.Reasoning == nil {
		if mapped, ok := chat.mapping[chat.defaultSource]; ok {
			copyRequest.Reasoning = &dmodel.Reasoning{Effort: nativeReasoningEffort(mapped)}
		}
		return copyRequest, nil
	}
	if len(chat.mapping) == 0 {
		return copyRequest, nil
	}
	level := llm.ParseThinkingLevel(request.Reasoning.Effort)
	mapped, ok := chat.mapping[level]
	if !ok {
		return dmodel.Request{}, fmt.Errorf("reasoning level %q is not supported by this model", request.Reasoning.Effort)
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
func WrapReasoningConfig(chat dmodel.Chat, endpoint, modelName, support, rawMap string) dmodel.Chat {
	supported := ResolveSupportsReasoning(endpoint, modelName, support)
	mapping, levels := parseReasoningMap(rawMap)
	defaultSource := llm.ParseThinkingLevel(chat.Profile().DefaultReasoningLevel)
	if defaultSource == llm.ThinkingLevelDefault {
		defaultSource = llm.ThinkingLevelMedium
	}
	return &reasoningChat{Chat: chat, supported: supported, levels: levels, mapping: mapping, defaultSource: defaultSource}
}

func parseReasoningMap(raw string) (map[llm.ThinkingLevel]reasoningMapping, []llm.ThinkingLevel) {
	if raw == "" {
		return nil, nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, nil
	}
	mapping := make(map[llm.ThinkingLevel]reasoningMapping, len(values))
	levels := make([]llm.ThinkingLevel, 0, len(values))
	for _, level := range []llm.ThinkingLevel{llm.ThinkingLevelOff, llm.ThinkingLevelMinimal, llm.ThinkingLevelLow, llm.ThinkingLevelMedium, llm.ThinkingLevelHigh, llm.ThinkingLevelXHigh} {
		mappedName, ok := values[level.Name()]
		if !ok || mappedName == "" {
			continue
		}
		mapped := llm.ParseThinkingLevel(mappedName)
		if mapped == llm.ThinkingLevelDefault && mappedName != "default" {
			// Preserve the generic level as a safe fallback for providers that do
			// not consume verbatim efforts; providers that do consume it use effort.
			mapping[level] = reasoningMapping{level: level, effort: mappedName}
		} else {
			mapping[level] = reasoningMapping{level: mapped}
		}
		levels = append(levels, level)
	}
	return mapping, levels
}

// createChatFromModel creates a native chat model from database configuration.
func (m *Manager) createChatFromModel(model *generated.Model) (dmodel.Chat, error) {
	supportsImages := ResolveSupportsImages(model.Endpoint, model.ModelName, model.ImageSupport)
	if model.ProviderType != "openai-responses" {
		return nil, fmt.Errorf("unknown provider type %q", model.ProviderType)
	}
	effort := model.ReasoningEffort
	if effort == "" {
		effort = "medium"
	}
	chat, err := NewOpenAIResponses(model.ApiKey, model.ModelName, model.Endpoint, m.httpc, OpenAIResponsesOptions{
		MaxOutputTokens: int(model.MaxTokens), SupportsImages: supportsImages,
		SupportsReasoning: true, SupportsWebSearch: true,
		MaxImageBytes: 20 * 1024 * 1024, DefaultReasoningLevel: "medium", ReasoningEffort: effort,
	})
	if err != nil {
		return nil, err
	}
	return WrapReasoningConfig(chat, model.Endpoint, model.ModelName, model.ReasoningSupport, model.ReasoningMap), nil
}

// ResolveSupportsImages turns a stored image_support value ("auto"|"yes"|"no")
// into a SupportsImages bool. "auto" is resolved from the model's endpoint URL
// and name; unknown models default to allowing images.
func ResolveSupportsImages(endpoint, modelName, imageSupport string) bool {
	switch imageSupport {
	case "yes":
		return true
	case "no":
		return false
	case "auto", "":
		supported, found := modelsdev.LookupImageSupport(endpoint, modelName)
		if !found {
			return true
		}
		return supported
	default:
		return true
	}
}

// ResolveSupportsReasoning turns a stored reasoning_support value
// ("auto"|"yes"|"no") into a capability boolean. Unknown models default to
// supporting reasoning so custom endpoints remain configurable.
func ResolveSupportsReasoning(endpoint, modelName, reasoningSupport string) bool {
	switch reasoningSupport {
	case "yes":
		return true
	case "no":
		return false
	case "auto", "":
		supported, found := modelsdev.LookupReasoningSupport(endpoint, modelName)
		if !found {
			return true
		}
		return supported
	default:
		return true
	}
}
