package gem

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestBuildGeminiRequest(t *testing.T) { providercontract.Run(t, "TestBuildGeminiRequest") }
func TestConvertToolSchemas(t *testing.T) { providercontract.Run(t, "TestConvertToolSchemas") }
func TestService_Do_MockResponse(t *testing.T) {
	providercontract.Run(t, "TestService_Do_MockResponse")
}
func TestConvertResponseWithToolCall(t *testing.T) {
	providercontract.Run(t, "TestConvertResponseWithToolCall")
}
func TestGeminiHeaderCapture(t *testing.T)   { providercontract.Run(t, "TestGeminiHeaderCapture") }
func TestHeaderCostIntegration(t *testing.T) { providercontract.Run(t, "TestHeaderCostIntegration") }
func TestTokenContextWindow(t *testing.T)    { providercontract.Run(t, "TestTokenContextWindow") }
func TestMaxImageDimension(t *testing.T)     { providercontract.Run(t, "TestMaxImageDimension") }
func TestEnsureToolIDs(t *testing.T)         { providercontract.Run(t, "TestEnsureToolIDs") }
func TestCalculateUsage(t *testing.T)        { providercontract.Run(t, "TestCalculateUsage") }
func TestCalculateUsageWithFunctionResponse(t *testing.T) {
	providercontract.Run(t, "TestCalculateUsageWithFunctionResponse")
}
func TestCalculateUsageWithEmptyText(t *testing.T) {
	providercontract.Run(t, "TestCalculateUsageWithEmptyText")
}
func TestCalculateUsageWithComplexFunctionCall(t *testing.T) {
	providercontract.Run(t, "TestCalculateUsageWithComplexFunctionCall")
}
func TestConvertResponseWithThinking(t *testing.T) {
	providercontract.Run(t, "TestConvertResponseWithThinking")
}
func TestConvertResponseWithRegularText(t *testing.T) {
	providercontract.Run(t, "TestConvertResponseWithRegularText")
}
func TestConvertResponseWithMixedContent(t *testing.T) {
	providercontract.Run(t, "TestConvertResponseWithMixedContent")
}
func TestConvertResponseGemini3FinalAnswerWithSignature(t *testing.T) {
	providercontract.Run(t, "TestConvertResponseGemini3FinalAnswerWithSignature")
}
func TestBuildGeminiRequestWithThinking(t *testing.T) {
	providercontract.Run(t, "TestBuildGeminiRequestWithThinking")
}
func TestBuildGeminiRequestWithRedactedThinking(t *testing.T) {
	providercontract.Run(t, "TestBuildGeminiRequestWithRedactedThinking")
}
func TestRoundTripThinking(t *testing.T) { providercontract.Run(t, "TestRoundTripThinking") }
func TestBuildGeminiRequestSkipsOpenAIResponsesReasoningMetadata(t *testing.T) {
	providercontract.Run(t, "TestBuildGeminiRequestSkipsOpenAIResponsesReasoningMetadata")
}
func TestThinkingConfig(t *testing.T) { providercontract.Run(t, "TestThinkingConfig") }
func TestBuildGeminiRequestImage(t *testing.T) {
	providercontract.Run(t, "TestBuildGeminiRequestImage")
}
func TestBuildGeminiRequestImageInToolResult(t *testing.T) {
	providercontract.Run(t, "TestBuildGeminiRequestImageInToolResult")
}
func TestThinkingConfigRequestOverride(t *testing.T) {
	providercontract.Run(t, "TestThinkingConfigRequestOverride")
}
