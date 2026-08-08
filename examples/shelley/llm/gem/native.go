package gem

import (
	"net/http"

	dmodel "github.com/semistrict/dago/model"
	dgemini "github.com/semistrict/dago/providers/gemini"

	"shelley.exe.dev/llm"
)

type NativeOptions struct {
	SupportsImages    bool
	SupportsReasoning bool
	ThinkingLevel     llm.ThinkingLevel
	ReasoningEffort   string
}

func NewNative(apiKey, modelID, endpoint string, httpClient *http.Client, options NativeOptions) llm.Service {
	var defaultReasoning *dmodel.Reasoning
	effort := options.ReasoningEffort
	if effort == "" && options.ThinkingLevel != llm.ThinkingLevelDefault {
		effort = options.ThinkingLevel.ThinkingEffort()
	}
	if options.SupportsReasoning && effort != "" {
		defaultReasoning = &dmodel.Reasoning{Effort: effort, Summary: "auto"}
	}
	metadata := &Service{Model: modelID}
	chat, err := dgemini.New(apiKey, dgemini.Options{
		Model: modelID, BaseURL: endpoint, HTTPClient: httpClient,
		ContextWindow: metadata.TokenContextWindow(), SupportsImages: options.SupportsImages,
		SupportsReasoning: options.SupportsReasoning, DefaultReasoning: defaultReasoning,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	service, err := llm.NewNativeServiceWithOptions(chat, llm.NativeServiceOptions{
		Provider: "gemini", ModelID: modelID, BaseURL: endpoint,
		SupportsImages: options.SupportsImages, SupportsReasoning: options.SupportsReasoning,
		MaxImageBytes: 20 * 1024 * 1024,
	})
	if err != nil {
		return llm.UnavailableNativeService(err)
	}
	return NewNativeService(Service{
		HTTPC: httpClient, URL: endpoint, APIKey: apiKey, Model: modelID,
		ThinkingLevel: options.ThinkingLevel, ReasoningEffort: options.ReasoningEffort,
		SupportsImages_: options.SupportsImages,
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
