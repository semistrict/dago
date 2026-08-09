package ant

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestAnthropicImageToolResult(t *testing.T) {
	providercontract.Run(t, "TestAnthropicImageToolResult")
}
