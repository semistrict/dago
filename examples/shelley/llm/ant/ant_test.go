package ant

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestIsClaudeModel(t *testing.T)      { providercontract.Run(t, "TestIsClaudeModel") }
func TestClaudeModelName(t *testing.T)    { providercontract.Run(t, "TestClaudeModelName") }
func TestTokenContextWindow(t *testing.T) { providercontract.Run(t, "TestTokenContextWindow") }
func TestSupportsServerSideWebSearch(t *testing.T) {
	providercontract.Run(t, "TestSupportsServerSideWebSearch")
}
func TestMaxImageDimension(t *testing.T) { providercontract.Run(t, "TestMaxImageDimension") }
func TestMaxImageBytes(t *testing.T)     { providercontract.Run(t, "TestMaxImageBytes") }
func TestToLLMUsage(t *testing.T)        { providercontract.Run(t, "TestToLLMUsage") }
func TestToLLMContent(t *testing.T)      { providercontract.Run(t, "TestToLLMContent") }
func TestToLLMResponse(t *testing.T)     { providercontract.Run(t, "TestToLLMResponse") }
func TestToLLMResponseRefusalDetails(t *testing.T) {
	providercontract.Run(t, "TestToLLMResponseRefusalDetails")
}
func TestParseSSEStreamRefusalDetails(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamRefusalDetails")
}
func TestFromLLMToolUse(t *testing.T) { providercontract.Run(t, "TestFromLLMToolUse") }
func TestFromLLMMessage(t *testing.T) { providercontract.Run(t, "TestFromLLMMessage") }
func TestFromLLMMessageSkipsCorruptThinking(t *testing.T) {
	providercontract.Run(t, "TestFromLLMMessageSkipsCorruptThinking")
}
func TestFromLLMMessageSkipsEmptyTextBlocks(t *testing.T) {
	providercontract.Run(t, "TestFromLLMMessageSkipsEmptyTextBlocks")
}
func TestFromLLMRequestSkipsEmptyMessages(t *testing.T) {
	providercontract.Run(t, "TestFromLLMRequestSkipsEmptyMessages")
}
func TestFromLLMToolChoice(t *testing.T) { providercontract.Run(t, "TestFromLLMToolChoice") }
func TestFromLLMTool(t *testing.T)       { providercontract.Run(t, "TestFromLLMTool") }
func TestFromLLMSystem(t *testing.T)     { providercontract.Run(t, "TestFromLLMSystem") }
func TestMapped(t *testing.T)            { providercontract.Run(t, "TestMapped") }
func TestUsageAdd(t *testing.T)          { providercontract.Run(t, "TestUsageAdd") }
func TestFromLLMRequestStripsOldThinkingBlocks(t *testing.T) {
	providercontract.Run(t, "TestFromLLMRequestStripsOldThinkingBlocks")
}
func TestFromLLMRequest(t *testing.T)         { providercontract.Run(t, "TestFromLLMRequest") }
func TestMaxOutputTokensCapping(t *testing.T) { providercontract.Run(t, "TestMaxOutputTokensCapping") }
func TestMaxOutputTokensMatchModelsDevAPI(t *testing.T) {
	providercontract.Run(t, "TestMaxOutputTokensMatchModelsDevAPI")
}
func TestConfigDetails(t *testing.T)  { providercontract.Run(t, "TestConfigDetails") }
func TestDo(t *testing.T)             { providercontract.Run(t, "TestDo") }
func TestFromLLMContent(t *testing.T) { providercontract.Run(t, "TestFromLLMContent") }
func TestInverted(t *testing.T)       { providercontract.Run(t, "TestInverted") }
func TestToLLMContentWithNestedToolResults(t *testing.T) {
	providercontract.Run(t, "TestToLLMContentWithNestedToolResults")
}
func TestParseSSEStreamText(t *testing.T) { providercontract.Run(t, "TestParseSSEStreamText") }
func TestParseSSEStreamMultipleDeltas(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamMultipleDeltas")
}
func TestParseSSEStreamDropsEmptyTextBlock(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamDropsEmptyTextBlock")
}
func TestParseSSEStreamToolUse(t *testing.T) { providercontract.Run(t, "TestParseSSEStreamToolUse") }
func TestParseSSEStreamToolUseEmptyInput(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamToolUseEmptyInput")
}
func TestParseSSEStreamThinking(t *testing.T) { providercontract.Run(t, "TestParseSSEStreamThinking") }
func TestParseSSEStreamPing(t *testing.T)     { providercontract.Run(t, "TestParseSSEStreamPing") }
func TestParseSSEStreamNoMessageStart(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamNoMessageStart")
}
func TestParseSSEStreamIncomplete(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamIncomplete")
}
func TestParseSSEStreamError(t *testing.T) { providercontract.Run(t, "TestParseSSEStreamError") }
func TestDoRetriesOnInvalidThinkingSignature(t *testing.T) {
	providercontract.Run(t, "TestDoRetriesOnInvalidThinkingSignature")
}
func TestDoClientError(t *testing.T)        { providercontract.Run(t, "TestDoClientError") }
func TestServiceConfigDetails(t *testing.T) { providercontract.Run(t, "TestServiceConfigDetails") }
func TestDoStartTimeEndTime(t *testing.T)   { providercontract.Run(t, "TestDoStartTimeEndTime") }
func TestLiveAnthropicModels(t *testing.T)  { providercontract.Run(t, "TestLiveAnthropicModels") }
func TestParseSSEStreamRecordedResponse(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamRecordedResponse")
}
func TestParseSSEStreamConnectionReset(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamConnectionReset")
}
func TestParseSSEStreamTruncated(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamTruncated")
}
func TestParseSSEStreamTruncatedMidContentBlock(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamTruncatedMidContentBlock")
}
func TestDoRetriesOnTruncatedStream(t *testing.T) {
	providercontract.Run(t, "TestDoRetriesOnTruncatedStream")
}
func TestDoStopsRetryingOnContextCancel(t *testing.T) {
	providercontract.Run(t, "TestDoStopsRetryingOnContextCancel")
}
func TestDoFailsAfterMaxRetriesOnTruncatedStream(t *testing.T) {
	providercontract.Run(t, "TestDoFailsAfterMaxRetriesOnTruncatedStream")
}
func TestParseSSEStreamMultiLineData(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamMultiLineData")
}
func TestParseSSEStreamErrorIncludesData(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamErrorIncludesData")
}
func TestParseSSEStreamTruncatedJSON(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamTruncatedJSON")
}
func TestIterSSEEventsComments(t *testing.T) { providercontract.Run(t, "TestIterSSEEventsComments") }
func TestIterSSEEventsNoTrailingNewline(t *testing.T) {
	providercontract.Run(t, "TestIterSSEEventsNoTrailingNewline")
}
func TestParseSSEStreamInvalidCharInJSON(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamInvalidCharInJSON")
}
func TestServerToolUseContentRoundTrip(t *testing.T) {
	providercontract.Run(t, "TestServerToolUseContentRoundTrip")
}
func TestFromLLMContentDropsNullRawMessages(t *testing.T) {
	providercontract.Run(t, "TestFromLLMContentDropsNullRawMessages")
}
func TestWebSearchToolResultContentRoundTrip(t *testing.T) {
	providercontract.Run(t, "TestWebSearchToolResultContentRoundTrip")
}
func TestTextWithCitationsRoundTrip(t *testing.T) {
	providercontract.Run(t, "TestTextWithCitationsRoundTrip")
}
func TestParseSSEStreamWebSearch(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamWebSearch")
}
func TestParseSSEStreamServerToolUseWithInputDeltas(t *testing.T) {
	providercontract.Run(t, "TestParseSSEStreamServerToolUseWithInputDeltas")
}
func TestPauseTurnStopReason(t *testing.T) { providercontract.Run(t, "TestPauseTurnStopReason") }
func TestServerSideToolJSON(t *testing.T)  { providercontract.Run(t, "TestServerSideToolJSON") }
func TestWebSearchContentSurvivesJSONRoundTrip(t *testing.T) {
	providercontract.Run(t, "TestWebSearchContentSurvivesJSONRoundTrip")
}
func TestUseAdaptiveThinking(t *testing.T) { providercontract.Run(t, "TestUseAdaptiveThinking") }
func TestFromLLMRequestThinkingLevels(t *testing.T) {
	providercontract.Run(t, "TestFromLLMRequestThinkingLevels")
}
func TestSanitizeServerToolBlocks(t *testing.T) {
	providercontract.Run(t, "TestSanitizeServerToolBlocks")
}
func TestSanitizeServerToolBlocksDoesNotMutateInput(t *testing.T) {
	providercontract.Run(t, "TestSanitizeServerToolBlocksDoesNotMutateInput")
}
func TestServerToolBlocksLiveAnthropic(t *testing.T) {
	providercontract.Run(t, "TestServerToolBlocksLiveAnthropic")
}
