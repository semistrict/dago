package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/models"
)

func TestCustomModelWithThinking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "output": []any{
			map[string]any{"type": "reasoning", "id": "reason_1", "summary": []any{map[string]any{"type": "summary_text", "text": "checked"}}, "encrypted_content": "opaque"},
			map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "test successful"}}},
		}})
	}))
	defer upstream.Close()

	chat, err := models.NewOpenAIResponses("test-key", "gpt-native", upstream.URL, upstream.Client(), models.OpenAIResponsesOptions{
		SupportsReasoning: true, DefaultReasoningLevel: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := chat.Invoke(context.Background(), dmodel.Request{Messages: []dmessage.Message{dmessage.Human("test")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Message.Content) != 2 || response.Message.Content[0].Type != dmessage.BlockReasoning || response.Message.Content[1].Text != "test successful" {
		t.Fatalf("response content = %#v", response.Message.Content)
	}
}

func TestCustomModelTestEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "output": []any{map[string]any{
			"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "test successful"}},
		}}})
	}))
	defer upstream.Close()

	harness := NewTestHarness(t)
	body, err := json.Marshal(TestModelRequest{
		ProviderType: "openai-responses", APIKey: "test-key",
		Endpoint: upstream.URL, ModelName: "gpt-native",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/custom-models/test", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	harness.server.handleTestModel(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Message == "" {
		t.Fatalf("result = %#v", result)
	}
}
