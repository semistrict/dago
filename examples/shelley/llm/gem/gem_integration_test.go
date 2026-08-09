package gem

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestGeminiThinkingIntegration(t *testing.T) {
	providercontract.Run(t, "TestGeminiThinkingIntegration")
}
func TestGemini3ModelsIntegration(t *testing.T) {
	providercontract.Run(t, "TestGemini3ModelsIntegration")
}
func TestGeminiImageIntegration(t *testing.T) { providercontract.Run(t, "TestGeminiImageIntegration") }
