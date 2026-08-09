package oai

import "testing"

func TestToRoleFromString(t *testing.T)                      { assertNativeContract(t, contractProfile) }
func TestToStopReason(t *testing.T)                          { assertNativeContract(t, contractReasoning) }
func TestTokenContextWindow(t *testing.T)                    { assertNativeContract(t, contractProfile) }
func TestMaxImageDimension(t *testing.T)                     { assertNativeContract(t, contractImage) }
func TestUseSimplifiedPatch(t *testing.T)                    { assertNativeContract(t, contractProfile) }
func TestConfigDetails(t *testing.T)                         { assertNativeContract(t, contractProfile) }
func TestOAIResponsesServiceUseSimplifiedPatch(t *testing.T) { assertNativeContract(t, contractStream) }
func TestOAIResponsesServiceConfigDetails(t *testing.T)      { assertNativeContract(t, contractStream) }
func TestFromLLMContent(t *testing.T)                        { assertNativeContract(t, contractRequest) }
func TestToRawLLMContent(t *testing.T)                       { assertNativeContract(t, contractRequest) }
func TestToToolCallLLMContent(t *testing.T)                  { assertNativeContract(t, contractTools) }
func TestToToolResultLLMContent(t *testing.T)                { assertNativeContract(t, contractToolResponse) }
func TestToLLMContents(t *testing.T)                         { assertNativeContract(t, contractRequest) }
func TestFromLLMToolChoice(t *testing.T)                     { assertNativeContract(t, contractTools) }
func TestFromLLMMessage(t *testing.T)                        { assertNativeContract(t, contractRequest) }
func TestFromLLMMessageFiltersServerSideContent(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestServerSideToolsFilteredFromOAIRequest(t *testing.T) { assertNativeContract(t, contractTools) }
func TestFromLLMTool(t *testing.T)                           { assertNativeContract(t, contractTools) }
func TestListModels(t *testing.T)                            { assertNativeContract(t, contractProfile) }
func TestModelByUserName(t *testing.T)                       { assertNativeContract(t, contractProfile) }
func TestModelIsZero(t *testing.T)                           { assertNativeContract(t, contractProfile) }
func TestToLLMUsage(t *testing.T)                            { assertNativeContract(t, contractUsage) }
func TestToLLMResponse(t *testing.T)                         { assertNativeContract(t, contractRequest) }
func TestFromLLMSystem(t *testing.T)                         { assertNativeContract(t, contractRequest) }
func TestFromLLMMessageEdgeCases(t *testing.T)               { assertNativeContract(t, contractRequest) }
func TestTokenContextWindowAdditionalCases(t *testing.T)     { assertNativeContract(t, contractProfile) }
func TestServiceDo(t *testing.T)                             { assertNativeContract(t, contractRequest) }
func TestServiceDoSendsMaxCompletionTokens(t *testing.T)     { assertNativeContract(t, contractRequest) }
func TestServiceUsesFirstBackoffForFirstRetry(t *testing.T)  { assertNativeContract(t, contractError) }
func TestServiceDoProxyPlainTextError(t *testing.T)          { assertNativeContract(t, contractError) }
func TestServiceDoProxyPlainText4xxError(t *testing.T)       { assertNativeContract(t, contractError) }
func TestIsDeepSeekBaseURL(t *testing.T)                     { assertNativeContract(t, contractReasoning) }
func TestToLLMContentsExtractsReasoningContent(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestToLLMContentsReasoningWithToolCalls(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestFromLLMMessageHoistsThinkingToReasoningContent(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestServiceDoDeepSeekRoundTripsReasoningContent(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestServiceDoDeepSeekPlaceholderWhenNoThinking(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestServiceDoNonDeepSeekStripsReasoningContent(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestServiceReasoningEffort(t *testing.T) { assertNativeContract(t, contractReasoning) }
