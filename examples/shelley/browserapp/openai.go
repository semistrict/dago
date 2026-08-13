package browserapp

import (
	"net/http"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
)

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
