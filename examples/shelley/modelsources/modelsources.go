// Package modelsources materializes Shelley models from local credentials.
package modelsources

import (
	"log/slog"
	"net/http"

	"github.com/semistrict/dago/examples/shelley/llm/llmhttp"
	"github.com/semistrict/dago/examples/shelley/models"
)

type providerConn struct {
	baseURL string
	apiKey  string
}

// Source is one local origin from which catalog models can be materialized.
type Source struct {
	label          string
	providers      map[models.Provider]*providerConn
	providerLabels map[models.Provider]string
}

func (source Source) labelFor(provider models.Provider) string {
	if label, ok := source.providerLabels[provider]; ok {
		return label
	}
	return source.label
}

// Predictable returns the deterministic built-in test model source.
func Predictable() Source {
	return Source{label: "builtin", providers: map[models.Provider]*providerConn{models.ProviderBuiltIn: {}}}
}

// Env returns models backed by direct provider API keys.
func Env(openAIKey string) Source {
	providers := map[models.Provider]*providerConn{}
	labels := map[models.Provider]string{}
	if openAIKey != "" {
		providers[models.ProviderOpenAI] = &providerConn{apiKey: openAIKey}
		labels[models.ProviderOpenAI] = "$OPENAI_API_KEY"
	}
	return Source{label: "environment", providers: providers, providerLabels: labels}
}

// Build materializes catalog entries from sources in order. The first source
// to provide a model ID wins.
func Build(catalog []models.Model, sources []Source, httpClient *http.Client, logger *slog.Logger) []models.Built {
	if logger == nil {
		logger = slog.Default()
	}
	if httpClient == nil {
		httpClient = llmhttp.NewClient(nil)
	}
	seen := map[string]bool{}
	var result []models.Built
	for _, source := range sources {
		for _, model := range catalog {
			connection := source.providers[model.Provider]
			if connection == nil || seen[model.ID] {
				continue
			}
			chat, err := model.Build(connection.baseURL, connection.apiKey, httpClient)
			if err != nil {
				logger.Warn("Could not materialize model", "id", model.ID, "source", source.labelFor(model.Provider), "error", err)
				continue
			}
			seen[model.ID] = true
			baseURL := connection.baseURL
			if baseURL == "" {
				baseURL = model.DefaultBaseURL
			}
			result = append(result, models.Built{
				ID: model.ID, DisplayName: model.ID, Provider: model.Provider,
				Source: source.labelFor(model.Provider), Tags: model.Tags,
				Chat: chat, APIType: model.APIType, BaseURL: baseURL,
			})
		}
	}
	return result
}
