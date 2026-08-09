package gem

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestDefaultReasoningLevel(t *testing.T) { providercontract.Run(t, "TestDefaultReasoningLevel") }
