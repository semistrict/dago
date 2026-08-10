package claudetool

import dmodel "github.com/semistrict/dago/model"

// LLMServiceProvider resolves native chat models for Shelley's app-specific
// yielding shell and one-shot model tool.
type LLMServiceProvider interface {
	GetChat(modelID string) (dmodel.Chat, error)
	GetAvailableModels() []string
}
