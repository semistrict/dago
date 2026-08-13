package browserapp

import (
	"fmt"
	"net/http"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/daproviders/openrouter"
)

func customModelChat(model CustomModel) (damodel.Chat, error) {
	switch model.ProviderType {
	case "openai-responses":
		return openAIChat(model)
	case "openrouter-responses":
		return openRouterChat(model)
	default:
		return nil, fmt.Errorf("unsupported model provider %q", model.ProviderType)
	}
}

func openAIChat(model CustomModel) (damodel.Chat, error) {
	var reasoning *damodel.Reasoning
	if model.ReasoningEffort != "" {
		reasoning = &damodel.Reasoning{Effort: model.ReasoningEffort}
	}
	return openai.NewAPIKey(model.APIKey, openai.Options{
		Model: model.ModelName, BaseURL: model.Endpoint, ContextWindow: int(model.MaxTokens),
		HTTPClient:       &http.Client{Transport: http.DefaultTransport},
		DefaultReasoning: reasoning, WebSearch: true,
	})
}

func openRouterChat(model CustomModel) (damodel.Chat, error) {
	var reasoning *damodel.Reasoning
	if model.ReasoningEffort != "" {
		reasoning = &damodel.Reasoning{Effort: model.ReasoningEffort}
	}
	requireParameters := true
	chat, err := openrouter.New(model.APIKey, openrouter.Options{
		Model: model.ModelName, BaseURL: model.Endpoint, ContextWindow: int(model.MaxTokens),
		HTTPClient:       &http.Client{Transport: http.DefaultTransport},
		DefaultReasoning: reasoning,
		AppTitle:         "Shelley",
		Routing:          &openrouter.ProviderRouting{RequireParameters: &requireParameters},
	})
	if err != nil {
		return nil, err
	}
	return damodel.WithProfile(chat, func(profile *damodel.Profile) {
		profile.SupportsImages = model.ImageSupport != "no"
		profile.SupportsReasoning = model.ReasoningSupport != "no"
	}), nil
}
