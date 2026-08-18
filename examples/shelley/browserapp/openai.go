package browserapp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
	"github.com/semistrict/dago/daproviders/openrouter"
)

func customModelChat(model CustomModel) (damodel.Chat, error) {
	if strings.TrimSpace(model.APIKey) == "" || strings.TrimSpace(model.ModelName) == "" || model.MaxTokens < 0 {
		return nil, fmt.Errorf("model API key, model name, and non-negative token limit are required")
	}
	if err := validateModelEnum("image_support", model.ImageSupport, "", "auto", "yes", "no"); err != nil {
		return nil, err
	}
	if err := validateModelEnum("reasoning_support", model.ReasoningSupport, "", "auto", "yes", "no"); err != nil {
		return nil, err
	}
	if err := validateModelEnum("reasoning_effort", model.ReasoningEffort, "", "off", "none", "minimal", "low", "medium", "high", "xhigh"); err != nil {
		return nil, err
	}
	switch model.ProviderType {
	case "openai-responses":
		return openAIChat(model)
	case "openrouter-responses":
		return openRouterChat(model)
	default:
		return nil, fmt.Errorf("unsupported model provider %q", model.ProviderType)
	}
}

func validateModelEnum(field, value string, allowed ...string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("unsupported %s %q", field, value)
}

func openAIChat(model CustomModel) (damodel.Chat, error) {
	var reasoning *damodel.Reasoning
	if model.ReasoningEffort != "" {
		reasoning = &damodel.Reasoning{Effort: model.ReasoningEffort}
	}
	return openai.NewAPIKey(model.APIKey, model.ModelName, openai.Options{
		BaseURL: model.Endpoint, ContextWindow: int(model.MaxTokens),
		HTTPClient:       &http.Client{Transport: http.DefaultTransport},
		DefaultReasoning: reasoning, WebSearch: true,
	}), nil
}

func openRouterChat(model CustomModel) (damodel.Chat, error) {
	var reasoning *damodel.Reasoning
	if model.ReasoningEffort != "" {
		reasoning = &damodel.Reasoning{Effort: model.ReasoningEffort}
	}
	requireParameters := true
	chat := openrouter.New(model.APIKey, model.ModelName, openrouter.Options{
		BaseURL: model.Endpoint, ContextWindow: int(model.MaxTokens),
		HTTPClient:       &http.Client{Transport: http.DefaultTransport},
		DefaultReasoning: reasoning,
		AppTitle:         "Shelley",
		Routing:          &openrouter.ProviderRouting{RequireParameters: &requireParameters},
	})
	return damodel.WithProfile(chat, func(profile *damodel.Profile) {
		profile.SupportsImages = model.ImageSupport != "no"
		profile.SupportsReasoning = model.ReasoningSupport != "no"
	}), nil
}
