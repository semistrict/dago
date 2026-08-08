package ant

import (
	"net/http"

	dmodel "github.com/semistrict/dago/model"
	danthropic "github.com/semistrict/dago/providers/anthropic"

	"shelley.exe.dev/llm"
)

type NativeOptions struct {
	SupportsImages    bool
	SupportsReasoning bool
	ThinkingLevel     llm.ThinkingLevel
}

func NewNative(apiKey, modelID, endpoint string, httpClient *http.Client, options NativeOptions) llm.Service {
	var defaultReasoning *dmodel.Reasoning
	if options.SupportsReasoning && options.ThinkingLevel != llm.ThinkingLevelDefault && options.ThinkingLevel != llm.ThinkingLevelOff {
		defaultReasoning = &dmodel.Reasoning{Effort: options.ThinkingLevel.ThinkingEffort(), Summary: "auto"}
	}
	chat, err := danthropic.New(apiKey, danthropic.Options{
		Model: modelID, BaseURL: endpoint, HTTPClient: httpClient,
		ContextWindow: 200000, MaxOutputTokens: maxOutputTokens(modelID),
		SupportsImages: options.SupportsImages, AdaptiveThinking: useAdaptiveThinking(modelID),
		DefaultReasoning: defaultReasoning,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	service, err := llm.NewNativeServiceWithOptions(chat, llm.NativeServiceOptions{
		Provider: "anthropic", ModelID: modelID, BaseURL: endpoint,
		SupportsImages: options.SupportsImages, SupportsReasoning: options.SupportsReasoning,
		MaxImageDimension: 2000, MaxImageBytes: 5 * 1024 * 1024,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	return NewNativeService(Service{
		HTTPC: httpClient, URL: endpoint, APIKey: apiKey, Model: modelID,
		ThinkingLevel: options.ThinkingLevel, SupportsImages_: options.SupportsImages,
	}, service)
}

func NewNativeService(config Service, native llm.Service) *Service {
	config.native = native
	return &config
}

func (s *Service) DagoChat() dmodel.Chat {
	if s == nil || s.native == nil {
		return nil
	}
	if native, ok := s.native.(interface{ DagoChat() dmodel.Chat }); ok {
		return native.DagoChat()
	}
	return nil
}
