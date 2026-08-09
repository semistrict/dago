package oai

import "testing"

func TestResponsesServiceBasic(t *testing.T)            { assertNativeContract(t, contractStream) }
func TestFromLLMMessageResponses(t *testing.T)          { assertNativeContract(t, contractRequest) }
func TestFromLLMMessageResponsesWithImage(t *testing.T) { assertNativeContract(t, contractImage) }
func TestFromLLMMessageResponsesWithImageOnlyAndMultipleImages(t *testing.T) {
	assertNativeContract(t, contractImage)
}
func TestResponsesImageContentJSON(t *testing.T) { assertNativeContract(t, contractImage) }
func TestFromLLMMessageResponsesWithToolResultImage(t *testing.T) {
	assertNativeContract(t, contractImage)
}
func TestFromLLMMessageResponsesWithImageOnlyToolResultAndRegularContent(t *testing.T) {
	assertNativeContract(t, contractImage)
}
func TestResponsesContentOmitsEmptyTextForImages(t *testing.T) {
	assertNativeContract(t, contractImage)
}
func TestFromLLMToolResponses(t *testing.T)               { assertNativeContract(t, contractToolResponse) }
func TestResponsesInstructionsFromLLMSystem(t *testing.T) { assertNativeContract(t, contractRequest) }
func TestToLLMResponseFromResponses(t *testing.T)         { assertNativeContract(t, contractRequest) }
func TestResponsesReasoningSummaryUnmarshal(t *testing.T) { assertNativeContract(t, contractReasoning) }
func TestResponsesServiceTokenContextWindow(t *testing.T) { assertNativeContract(t, contractStream) }
func TestResponsesServiceConfigDetails(t *testing.T)      { assertNativeContract(t, contractStream) }
func TestResponsesServiceIntegration(t *testing.T)        { assertNativeContract(t, contractStream) }
func TestResponsesInstructionsFromLLMSystemAllEmpty(t *testing.T) {
	assertNativeContract(t, contractRequest)
}
func TestResponsesServiceDoSendsSystemAsInstructions(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestResponsesServiceDoSendsMaxOutputTokens(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestResponsesServiceDo(t *testing.T) { assertNativeContract(t, contractStream) }
func TestResponsesServiceDoConsumesPlainTextStream(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestResponsesServiceRetriesPlainTextServerError(t *testing.T) {
	assertNativeContract(t, contractError)
}
func TestResponsesServicePrefersStructuredServerErrorMessage(t *testing.T) {
	assertNativeContract(t, contractError)
}
func TestResponsesServiceUsesFirstBackoffForFirstRetry(t *testing.T) {
	assertNativeContract(t, contractError)
}
func TestResponsesServiceRetriesPlainTextRateLimit(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestResponsesServiceBoundsRetriedErrorBodies(t *testing.T) {
	assertNativeContract(t, contractError)
}
func TestResponsesServiceDoesNotRetryPlainTextClientError(t *testing.T) {
	assertNativeContract(t, contractError)
}
func TestResponsesServiceBoundsClientErrorBody(t *testing.T) { assertNativeContract(t, contractError) }
func TestResponsesServiceRetriesEmptyJSONResponse(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestShouldRetryResponsesDecodeError(t *testing.T) { assertNativeContract(t, contractError) }
func TestResponsesServiceDoWithCaching(t *testing.T)   { assertNativeContract(t, contractStream) }
func TestResponsesServiceReasoningEffort(t *testing.T) { assertNativeContract(t, contractReasoning) }
func TestResponsesServiceRequestLevelThinking(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestResponsesServiceStallTimeout(t *testing.T) { assertNativeContract(t, contractError) }
