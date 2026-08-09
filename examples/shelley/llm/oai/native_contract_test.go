package oai

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

type nativeContract int

const (
	contractProfile nativeContract = iota
	contractRequest
	contractTools
	contractReasoning
	contractImage
	contractToolResponse
	contractUsage
	contractError
	contractStream
	contractWebSearch
)

func assertNativeContract(t *testing.T, contract nativeContract) {
	t.Helper()
	_ = contract
	providercontract.Run(t, t.Name())
}
