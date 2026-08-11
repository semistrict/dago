package claudetool

import "github.com/semistrict/dago/damodel"

// LLMServiceProvider resolves native chat models for Shelley's app-specific
// yielding shell and one-shot model tool.
type LLMServiceProvider interface {
	GetChat(modelID string) (damodel.Chat, error)
	GetAvailableModels() []string
}
