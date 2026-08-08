package oai

import (
	"net/http"
	"strings"

	dmodel "github.com/semistrict/dago/model"
	dopenai "github.com/semistrict/dago/providers/openai"

	"shelley.exe.dev/llm"
)

type NativeResponsesOptions struct {
	Provider           string
	ContextWindow      int
	MaxOutputTokens    int
	SupportsImages     bool
	SupportsReasoning  bool
	UseSimplifiedPatch bool
	MaxImageBytes      int
	ThinkingLevel      llm.ThinkingLevel
	ReasoningEffort    string
}

type NativeChatOptions struct {
	Provider           string
	ContextWindow      int
	MaxOutputTokens    int
	SupportsImages     bool
	SupportsReasoning  bool
	UseSimplifiedPatch bool
	ThinkingLevel      llm.ThinkingLevel
	ReasoningEffort    string
}

func NewNativeChat(apiKey, modelID, apiBaseURL string, httpClient *http.Client, options NativeChatOptions) llm.Service {
	var defaultReasoning *dmodel.Reasoning
	effort := options.ReasoningEffort
	if effort == "" && options.ThinkingLevel != llm.ThinkingLevelDefault && options.ThinkingLevel != llm.ThinkingLevelOff {
		effort = options.ThinkingLevel.ThinkingEffort()
	}
	if effort != "" {
		defaultReasoning = &dmodel.Reasoning{Effort: effort}
	}
	chat, err := dopenai.NewChatCompletions(apiKey, dopenai.ChatOptions{
		Model: modelID, BaseURL: apiBaseURL, HTTPClient: httpClient,
		Provider: options.Provider, ContextWindow: options.ContextWindow,
		MaxOutputTokens: options.MaxOutputTokens, SupportsImages: options.SupportsImages,
		SupportsReasoning: options.SupportsReasoning, ParallelToolCalls: true,
		DefaultReasoning: defaultReasoning,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	service, err := llm.NewNativeServiceWithOptions(chat, llm.NativeServiceOptions{
		Provider: options.Provider, ModelID: modelID, BaseURL: apiBaseURL,
		SupportsImages: options.SupportsImages, SupportsReasoning: options.SupportsReasoning,
		UseSimplifiedPatch: options.UseSimplifiedPatch, MaxImageBytes: 20 * 1024 * 1024,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	return NewNativeChatService(Service{
		HTTPC: httpClient, APIKey: apiKey, ModelURL: apiBaseURL,
		Model:     Model{ModelName: modelID, SupportsImages: options.SupportsImages, IsReasoningModel: options.SupportsReasoning, UseSimplifiedPatch: options.UseSimplifiedPatch},
		MaxTokens: options.MaxOutputTokens, ProviderName: options.Provider,
		ThinkingLevel: options.ThinkingLevel, ReasoningEffort: options.ReasoningEffort,
	}, service)
}

func NewNativeResponses(apiKey, modelID, baseURL string, httpClient *http.Client, options NativeResponsesOptions) llm.Service {
	apiBaseURL := responsesAPIBaseURL(baseURL)
	var defaultReasoning *dmodel.Reasoning
	if options.SupportsReasoning {
		effort := options.ReasoningEffort
		if effort == "" && options.ThinkingLevel != llm.ThinkingLevelDefault && options.ThinkingLevel != llm.ThinkingLevelOff {
			effort = options.ThinkingLevel.ThinkingEffort()
		}
		if effort != "" {
			defaultReasoning = &dmodel.Reasoning{Effort: effort, Summary: "auto"}
		}
	}
	chat, err := dopenai.NewAPIKey(apiKey, dopenai.Options{
		Model: modelID, BaseURL: apiBaseURL, HTTPClient: httpClient,
		ContextWindow: options.ContextWindow, MaxOutputTokens: options.MaxOutputTokens,
		DefaultReasoning: defaultReasoning,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	service, err := llm.NewNativeServiceWithOptions(chat, llm.NativeServiceOptions{
		Provider: options.Provider, ModelID: modelID, BaseURL: apiBaseURL,
		SupportsImages: options.SupportsImages, SupportsReasoning: options.SupportsReasoning,
		UseSimplifiedPatch: options.UseSimplifiedPatch, MaxImageBytes: options.MaxImageBytes,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	return NewNativeResponsesService(ResponsesService{
		HTTPC: httpClient, APIKey: apiKey, ModelURL: apiBaseURL,
		Model: Model{
			ModelName: modelID, SupportsImages: options.SupportsImages,
			IsReasoningModel:   options.SupportsReasoning,
			UseSimplifiedPatch: options.UseSimplifiedPatch,
		},
		MaxTokens: options.MaxOutputTokens, ProviderName: options.Provider,
		ThinkingLevel: options.ThinkingLevel, ReasoningEffort: options.ReasoningEffort,
	}, service)
}

// responsesAPIBaseURL accepts the bare provider URL used by the built-in
// catalog as well as the protocol URL entered for a custom model. The native
// provider appends /responses when issuing a request, so store only the API
// base here.
func responsesAPIBaseURL(value string) string {
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
