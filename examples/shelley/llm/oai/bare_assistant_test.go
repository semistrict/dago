package oai

import "testing"

func TestBareAssistantMessage(t *testing.T) { assertNativeContract(t, contractRequest) }
func TestToolMessageNotBare(t *testing.T)   { assertNativeContract(t, contractTools) }
