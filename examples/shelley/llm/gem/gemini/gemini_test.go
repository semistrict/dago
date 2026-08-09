package gemini

import (
	"testing"

	"shelley.exe.dev/llm/providercontract"
)

func TestGenerateContent(t *testing.T) { providercontract.Run(t, "TestGenerateContent") }
