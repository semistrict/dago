package loop

import (
	"encoding/json"
	"testing"

	"shelley.exe.dev/llm"
)

func TestMessagesToDagoOmitsRedundantShelleyEnvelope(t *testing.T) {
	for _, original := range []llm.Message{
		{Role: llm.MessageRoleUser, Content: llm.TextContent("request")},
		{Role: llm.MessageRoleUser, Content: []llm.Content{{Type: llm.ContentTypeText, Text: "database request", ToolInput: json.RawMessage("null")}}},
		{Role: llm.MessageRoleAssistant, Content: llm.TextContent("response"), EndOfTurn: true},
	} {
		native, err := messagesToDago([]llm.Message{original})
		if err != nil {
			t.Fatal(err)
		}
		if len(native) != 1 {
			t.Fatalf("native messages = %d, want 1", len(native))
		}
		if raw := native[0].Metadata[shelleyMessageMetadata]; len(raw) != 0 {
			t.Fatalf("redundant Shelley envelope was retained: %s", raw)
		}
		projected, err := messagesFromDago(native)
		if err != nil {
			t.Fatal(err)
		}
		if len(projected) != 1 {
			t.Fatalf("round trip = %#v, want %#v", projected, original)
		}
		originalJSON, _ := json.Marshal(original)
		projectedJSON, _ := json.Marshal(projected[0])
		if string(projectedJSON) != string(originalJSON) {
			t.Fatalf("round trip = %#v, want %#v", projected, original)
		}
	}
}

func TestMessagesToDagoRetainsLossyShelleyEnvelope(t *testing.T) {
	original := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type: llm.ContentTypeThinking, Thinking: "reasoning", Signature: "provider-signature",
		}},
		EndOfTurn: true,
	}
	native, err := messagesToDago([]llm.Message{original})
	if err != nil {
		t.Fatal(err)
	}
	if len(native) != 1 || len(native[0].Metadata[shelleyMessageMetadata]) == 0 {
		t.Fatal("lossy message did not retain its Shelley envelope")
	}
	projected, err := messagesFromDago(native)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || len(projected[0].Content) != 1 ||
		projected[0].Content[0].Thinking != original.Content[0].Thinking ||
		projected[0].Content[0].Signature != original.Content[0].Signature {
		t.Fatalf("round trip = %#v, want %#v", projected, original)
	}
}
