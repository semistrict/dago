package oai

import "testing"

func TestParseResponsesSSEFinishesMalformedCitationContent(t *testing.T) {
	assertNativeContract(t, contractWebSearch)
}
func TestParseResponsesSSEFinishesCitationAtStreamEnd(t *testing.T) {
	assertNativeContract(t, contractWebSearch)
}
func TestResponsesConversionPreservesRawCitationMarkers(t *testing.T) {
	assertNativeContract(t, contractWebSearch)
}
