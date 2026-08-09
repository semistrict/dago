package ant

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestTextContentNoExtraFields(t *testing.T) {
	providercontract.Run(t, "TestTextContentNoExtraFields")
}
