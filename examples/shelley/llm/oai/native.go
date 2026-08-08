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

func NewNativeResponses(apiKey, modelID, baseURL string, httpClient *http.Client, options NativeResponsesOptions) llm.Service {
	apiBaseURL := ""
	if baseURL != "" {
		apiBaseURL = strings.TrimRight(baseURL, "/") + "/v1"
	}
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
