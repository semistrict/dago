package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"

	"github.com/semistrict/dago/examples/shelley/models"
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
	response, err := chat.Invoke(context.Background(), damodel.Request{Messages: []dmessage.Message{dmessage.Human("test")}})
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

func TestOpenRouterCustomModelTestEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/responses" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer openrouter-key" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "deepseek/deepseek-v4-flash-0731" {
			t.Fatalf("model = %#v", body["model"])
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "output": []any{map[string]any{
			"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "test successful"}},
		}}})
	}))
	defer upstream.Close()

	harness := NewTestHarness(t)
	body, err := json.Marshal(TestModelRequest{
		ProviderType: string(models.APITypeOpenRouterResponses), APIKey: "openrouter-key",
		Endpoint: upstream.URL + "/api/v1", ModelName: "deepseek/deepseek-v4-flash-0731",
		ReasoningSupport: "no",
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
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("result = %s", recorder.Body.String())
	}
}

func TestCreateOpenRouterCustomModel(t *testing.T) {
	harness := NewTestHarness(t)
	manager, err := models.NewManager(&models.Config{DB: harness.db})
	if err != nil {
		t.Fatal(err)
	}
	harness.server.llmManager = manager
	body, err := json.Marshal(CreateModelRequest{
		DisplayName: "DeepSeek V4 Flash 0731", ProviderType: string(models.APITypeOpenRouterResponses),
		Endpoint: "https://openrouter.ai/api/v1", APIKey: "openrouter-key",
		ModelName: "deepseek/deepseek-v4-flash-0731", MaxTokens: 1_000_000,
		ReasoningSupport: "yes", ImageSupport: "no",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/custom-models", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	harness.server.handleCreateModel(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var created ModelAPI
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ProviderType != string(models.APITypeOpenRouterResponses) || created.ModelName != "deepseek/deepseek-v4-flash-0731" {
		t.Fatalf("created model = %#v", created)
	}
	if !harness.server.llmManager.HasModel(created.ModelID) {
		t.Fatalf("model %q was not loaded into the manager", created.ModelID)
	}
}
