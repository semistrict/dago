package dagoruntime

import (
	"net/http"
	"strings"

	dopenai "github.com/semistrict/dago/providers/openai"

	"shelley.exe.dev/llm"
	"shelley.exe.dev/llm/oai"
)

type OpenAIResponsesOptions struct {
	Provider           string
	ContextWindow      int
	MaxOutputTokens    int
	SupportsImages     bool
	SupportsReasoning  bool
	UseSimplifiedPatch bool
	MaxImageBytes      int
}

func NewOpenAIResponses(apiKey, modelID, baseURL string, httpClient *http.Client, options OpenAIResponsesOptions) llm.Service {
	apiBaseURL := ""
	if baseURL != "" {
		apiBaseURL = strings.TrimRight(baseURL, "/") + "/v1"
	}
	chat, err := dopenai.NewAPIKey(apiKey, dopenai.Options{
		Model: modelID, BaseURL: apiBaseURL, HTTPClient: httpClient,
		ContextWindow: options.ContextWindow, MaxOutputTokens: options.MaxOutputTokens,
	})
	if err != nil {
		return Unavailable(err)
	}
	service, err := NewServiceWithOptions(chat, ServiceOptions{
		Provider: options.Provider, ModelID: modelID, BaseURL: apiBaseURL,
		SupportsImages: options.SupportsImages, SupportsReasoning: options.SupportsReasoning,
		UseSimplifiedPatch: options.UseSimplifiedPatch, MaxImageBytes: options.MaxImageBytes,
	})
	if err != nil {
		return Unavailable(err)
	}
	return oai.NewNativeResponsesService(oai.ResponsesService{
		HTTPC: httpClient, APIKey: apiKey, ModelURL: apiBaseURL,
		Model: oai.Model{
			ModelName: modelID, SupportsImages: options.SupportsImages,
			IsReasoningModel:   options.SupportsReasoning,
			UseSimplifiedPatch: options.UseSimplifiedPatch,
		},
		MaxTokens: options.MaxOutputTokens, ProviderName: options.Provider,
	}, service)
}
