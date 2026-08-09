package oai

import "testing"

func TestResponsesReasoningStateStatelessRoundTrip(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestResponsesServiceCodexRequestContract(t *testing.T) { assertNativeContract(t, contractStream) }
func TestResponsesParallelToolCallContinuation(t *testing.T) {
	assertNativeContract(t, contractToolResponse)
}
func TestResponsesServiceOpenAIRequestDefaultsAreProviderIsolated(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestResponsesServiceXAIRequestsReasoningSummaries(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestResponsesServiceTextVerbosityFollowsModelMetadata(t *testing.T) {
	assertNativeContract(t, contractStream)
}
func TestParseResponsesSSEPreservesReasoningStateAndOutputOrder(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestParseResponsesSSECompletedReasoningStateWins(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
func TestParseResponsesSSERejectsInterruptedReasoningState(t *testing.T) {
	assertNativeContract(t, contractReasoning)
}
