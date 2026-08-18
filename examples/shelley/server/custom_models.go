package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"

	"github.com/semistrict/dago/examples/shelley/db/generated"
	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/models"
)

// ModelAPI is the API representation of a model
type ModelAPI struct {
	ModelID         string `json:"model_id"`
	DisplayName     string `json:"display_name"`
	ProviderType    string `json:"provider_type"`
	Endpoint        string `json:"endpoint"`
	APIKey          string `json:"api_key"`
	ModelName       string `json:"model_name"`
	MaxTokens       int64  `json:"max_tokens"`
	Tags            string `json:"tags"` // Comma-separated tags (e.g., "slug" for slug generation)
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ImageSupport is one of "auto", "yes", or "no". "auto" is resolved
	// automatically from the model's endpoint and name.
	ImageSupport      string `json:"image_support"`
	ReasoningSupport  string `json:"reasoning_support"`
	ReasoningMap      string `json:"reasoning_map"`
	SupportsReasoning bool   `json:"supports_reasoning"`
	// SupportsImages is the resolved boolean that "image_support" evaluates
	// to for this model. It lets the UI show what "auto" resolves to.
	SupportsImages bool `json:"supports_images"`
}

// CreateModelRequest is the request body for creating a model.
type CreateModelRequest struct {
	DisplayName      string `json:"display_name"`
	ProviderType     string `json:"provider_type"`
	Endpoint         string `json:"endpoint"`
	APIKey           string `json:"api_key"`
	ModelName        string `json:"model_name"`
	MaxTokens        int64  `json:"max_tokens"`
	Tags             string `json:"tags"` // Comma-separated tags
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ImageSupport     string `json:"image_support"`     // "auto"|"yes"|"no"; empty = "auto"
	ReasoningSupport string `json:"reasoning_support"` // "auto"|"yes"|"no"; empty = "auto"
	ReasoningMap     string `json:"reasoning_map"`     // JSON map of Shelley level to provider-supported level
}

// UpdateModelRequest is the request body for updating a model.
type UpdateModelRequest struct {
	DisplayName      string  `json:"display_name"`
	ProviderType     string  `json:"provider_type"`
	Endpoint         string  `json:"endpoint"`
	APIKey           string  `json:"api_key"` // Empty string means keep existing
	ModelName        string  `json:"model_name"`
	MaxTokens        int64   `json:"max_tokens"`
	Tags             string  `json:"tags"` // Comma-separated tags
	ReasoningEffort  *string `json:"reasoning_effort,omitempty"`
	ImageSupport     string  `json:"image_support"`     // "auto"|"yes"|"no"; empty preserves existing
	ReasoningSupport string  `json:"reasoning_support"` // "auto"|"yes"|"no"; empty preserves existing
	ReasoningMap     string  `json:"reasoning_map"`
}

// validImageSupport returns the canonical value or an error.
func validSupportSetting(field, v string) (string, error) {
	switch v {
	case "", "auto":
		return "auto", nil
	case "yes", "no":
		return v, nil
	default:
		return "", fmt.Errorf("%s must be one of 'auto', 'yes', 'no'; got %q", field, v)
	}
}

func validImageSupport(v string) (string, error) {
	return validSupportSetting("image_support", v)
}

func validReasoningSupport(v string) (string, error) {
	return validSupportSetting("reasoning_support", v)
}

func validReasoningEffort(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "none" {
		return v, nil
	}
	if _, err := llm.ParseThinkingLevelStrict(v); err != nil {
		return "", fmt.Errorf("reasoning_effort: %w", err)
	}
	return v, nil
}

func validReasoningMap(raw string) error {
	if raw == "" {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return fmt.Errorf("reasoning_map must be a JSON object: %w", err)
	}
	valid := map[string]bool{"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true}
	for from, to := range values {
		mapped, err := llm.ParseThinkingLevelStrict(to)
		if !valid[from] || err != nil || mapped == llm.ThinkingLevelDefault {
			return fmt.Errorf("reasoning_map keys and values must use off, minimal, low, medium, high, or xhigh; got %q: %q", from, to)
		}
	}
	return nil
}

func validCustomProviderType(value string) bool {
	switch models.APIType(value) {
	case models.APITypeOpenAIResponses, models.APITypeOpenRouterResponses:
		return true
	default:
		return false
	}
}

// TestModelRequest is the request body for testing a model
type TestModelRequest struct {
	ModelID          string  `json:"model_id,omitempty"` // If provided, use stored API key
	ProviderType     string  `json:"provider_type"`
	Endpoint         string  `json:"endpoint"`
	APIKey           string  `json:"api_key"`
	ModelName        string  `json:"model_name"`
	ReasoningSupport string  `json:"reasoning_support"`
	ReasoningMap     string  `json:"reasoning_map"`
	ReasoningEffort  *string `json:"reasoning_effort,omitempty"`
}

func toModelAPI(m generated.Model) (ModelAPI, error) {
	supportsReasoning, err := models.ResolveSupportsReasoning(m.Endpoint, m.ModelName, m.ReasoningSupport)
	if err != nil {
		return ModelAPI{}, err
	}
	supportsImages, err := models.ResolveSupportsImages(m.Endpoint, m.ModelName, m.ImageSupport)
	if err != nil {
		return ModelAPI{}, err
	}
	return ModelAPI{
		ModelID:           m.ModelID,
		DisplayName:       m.DisplayName,
		ProviderType:      m.ProviderType,
		Endpoint:          m.Endpoint,
		APIKey:            m.ApiKey,
		ModelName:         m.ModelName,
		MaxTokens:         m.MaxTokens,
		Tags:              m.Tags,
		ReasoningEffort:   m.ReasoningEffort,
		ImageSupport:      m.ImageSupport,
		ReasoningSupport:  m.ReasoningSupport,
		ReasoningMap:      m.ReasoningMap,
		SupportsReasoning: supportsReasoning,
		SupportsImages:    supportsImages,
	}, nil
}

func (s *Server) handleCustomModels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListModels(w, r)
	case http.MethodPost:
		s.handleCreateModel(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.db.GetModels(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get models: %v", err), http.StatusInternalServerError)
		return
	}

	apiModels := make([]ModelAPI, len(models))
	for i, m := range models {
		apiModels[i], err = toModelAPI(m)
		if err != nil {
			http.Error(w, "Stored model has invalid capability configuration", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apiModels)
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	var req CreateModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// Validate required fields
	if req.DisplayName == "" || req.ProviderType == "" || req.Endpoint == "" || req.APIKey == "" || req.ModelName == "" {
		http.Error(w, "display_name, provider_type, endpoint, api_key, and model_name are required", http.StatusBadRequest)
		return
	}

	// Validate provider type
	if !validCustomProviderType(req.ProviderType) {
		http.Error(w, "provider_type must be 'openai-responses' or 'openrouter-responses'", http.StatusBadRequest)
		return
	}

	// Generate a human-readable model ID derived from the endpoint and model name.
	modelID, err := s.generateUniqueModelID(r.Context(), req.Endpoint, req.ModelName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to generate model ID: %v", err), http.StatusInternalServerError)
		return
	}

	if req.MaxTokens < 0 {
		http.Error(w, "max_tokens cannot be negative", http.StatusBadRequest)
		return
	}
	// Default max tokens
	if req.MaxTokens == 0 {
		req.MaxTokens = 200000
	}
	reasoningEffort, err := validReasoningEffort(req.ReasoningEffort)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	imageSupport, err := validImageSupport(req.ImageSupport)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reasoningSupport, err := validReasoningSupport(req.ReasoningSupport)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validReasoningMap(req.ReasoningMap); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	model, err := s.db.CreateModel(r.Context(), generated.CreateModelParams{
		ModelID:          modelID,
		DisplayName:      req.DisplayName,
		ProviderType:     req.ProviderType,
		Endpoint:         req.Endpoint,
		ApiKey:           req.APIKey,
		ModelName:        req.ModelName,
		MaxTokens:        req.MaxTokens,
		Tags:             req.Tags,
		ReasoningEffort:  reasoningEffort,
		ImageSupport:     imageSupport,
		ReasoningSupport: reasoningSupport,
		ReasoningMap:     req.ReasoningMap,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create model: %v", err), http.StatusInternalServerError)
		return
	}

	// Refresh the model manager's cache
	if err := s.llmManager.RefreshCustomModels(); err != nil {
		s.logger.Warn("Failed to refresh custom models cache", "error", err)
	}

	apiModel, err := toModelAPI(*model)
	if err != nil {
		http.Error(w, "Created model has invalid capability configuration", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(apiModel)
}

func (s *Server) handleCustomModel(w http.ResponseWriter, r *http.Request) {
	// Extract model ID from URL path: /api/custom-models/{id} or /api/custom-models/{id}/duplicate
	path := strings.TrimPrefix(r.URL.Path, "/api/custom-models/")
	if path == "" {
		http.Error(w, "Invalid model ID", http.StatusBadRequest)
		return
	}

	// Check for /duplicate suffix
	if strings.HasSuffix(path, "/duplicate") {
		modelID := strings.TrimSuffix(path, "/duplicate")
		if r.Method == http.MethodPost {
			s.handleDuplicateModel(w, r, modelID)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	if strings.Contains(path, "/") {
		http.Error(w, "Invalid model ID", http.StatusBadRequest)
		return
	}
	modelID := path

	switch r.Method {
	case http.MethodGet:
		s.handleGetModel(w, r, modelID)
	case http.MethodPut:
		s.handleUpdateModel(w, r, modelID)
	case http.MethodDelete:
		s.handleDeleteModel(w, r, modelID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request, modelID string) {
	model, err := s.db.GetModel(r.Context(), modelID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get model: %v", err), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	apiModel, err := toModelAPI(*model)
	if err != nil {
		http.Error(w, "Stored model has invalid capability configuration", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(apiModel)
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, r *http.Request, modelID string) {
	// First, get the existing model to get the current API key if not provided
	existing, err := s.db.GetModel(r.Context(), modelID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Model not found: %v", err), http.StatusNotFound)
		return
	}

	var req UpdateModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if !validCustomProviderType(req.ProviderType) {
		http.Error(w, "provider_type must be 'openai-responses' or 'openrouter-responses'", http.StatusBadRequest)
		return
	}

	// Use existing API key if not provided
	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = existing.ApiKey
	}

	if req.MaxTokens < 0 {
		http.Error(w, "max_tokens cannot be negative", http.StatusBadRequest)
		return
	}
	// Default max tokens
	if req.MaxTokens == 0 {
		req.MaxTokens = 200000
	}

	// Empty support settings preserve the existing values; otherwise validate.
	imageSupport := existing.ImageSupport
	if req.ImageSupport != "" {
		v, err := validImageSupport(req.ImageSupport)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		imageSupport = v
	}
	reasoningSupport := existing.ReasoningSupport
	if req.ReasoningSupport != "" {
		v, err := validReasoningSupport(req.ReasoningSupport)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reasoningSupport = v
	}
	if err := validReasoningMap(req.ReasoningMap); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reasoningEffort := existing.ReasoningEffort
	if req.ReasoningEffort != nil {
		var err error
		reasoningEffort, err = validReasoningEffort(*req.ReasoningEffort)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	model, err := s.db.UpdateModel(r.Context(), generated.UpdateModelParams{
		DisplayName:      req.DisplayName,
		ProviderType:     req.ProviderType,
		Endpoint:         req.Endpoint,
		ApiKey:           apiKey,
		ModelName:        req.ModelName,
		MaxTokens:        req.MaxTokens,
		Tags:             req.Tags,
		ReasoningEffort:  reasoningEffort,
		ImageSupport:     imageSupport,
		ReasoningSupport: reasoningSupport,
		ReasoningMap:     req.ReasoningMap,
		ModelID:          modelID,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to update model: %v", err), http.StatusInternalServerError)
		return
	}

	// Refresh the model manager's cache
	if err := s.llmManager.RefreshCustomModels(); err != nil {
		s.logger.Warn("Failed to refresh custom models cache", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	apiModel, err := toModelAPI(*model)
	if err != nil {
		http.Error(w, "Updated model has invalid capability configuration", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(apiModel)
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request, modelID string) {
	err := s.db.DeleteModel(r.Context(), modelID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to delete model: %v", err), http.StatusInternalServerError)
		return
	}

	// Refresh the model manager's cache
	if err := s.llmManager.RefreshCustomModels(); err != nil {
		s.logger.Warn("Failed to refresh custom models cache", "error", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// DuplicateModelRequest allows overriding fields when duplicating
type DuplicateModelRequest struct {
	DisplayName string `json:"display_name,omitempty"`
}

func (s *Server) handleDuplicateModel(w http.ResponseWriter, r *http.Request, modelID string) {
	// Get the source model (including API key)
	source, err := s.db.GetModel(r.Context(), modelID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Source model not found: %v", err), http.StatusNotFound)
		return
	}

	// Parse optional overrides
	var req DuplicateModelRequest
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req) // Ignore errors - all fields optional
	}

	// Generate a new human-readable model ID. Since the duplicate shares the
	// source's endpoint and model name, this naturally gets a numeric suffix.
	newModelID, err := s.generateUniqueModelID(r.Context(), source.Endpoint, source.ModelName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to generate model ID: %v", err), http.StatusInternalServerError)
		return
	}

	// Use provided display name or generate one
	displayName := req.DisplayName
	if displayName == "" {
		displayName = source.DisplayName + " (copy)"
	}

	// Create the duplicate with the same API key
	model, err := s.db.CreateModel(r.Context(), generated.CreateModelParams{
		ModelID:          newModelID,
		DisplayName:      displayName,
		ProviderType:     source.ProviderType,
		Endpoint:         source.Endpoint,
		ApiKey:           source.ApiKey, // Copy the API key!
		ModelName:        source.ModelName,
		MaxTokens:        source.MaxTokens,
		Tags:             "", // Don't copy tags
		ReasoningEffort:  source.ReasoningEffort,
		ImageSupport:     source.ImageSupport,
		ReasoningSupport: source.ReasoningSupport,
		ReasoningMap:     source.ReasoningMap,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to duplicate model: %v", err), http.StatusInternalServerError)
		return
	}

	// Refresh the model manager's cache
	if err := s.llmManager.RefreshCustomModels(); err != nil {
		s.logger.Warn("Failed to refresh custom models cache", "error", err)
	}

	apiModel, err := toModelAPI(*model)
	if err != nil {
		http.Error(w, "Duplicated model has invalid capability configuration", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(apiModel)
}

func (s *Server) handleTestModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req TestModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// A model ID supplies hidden legacy configuration and, when omitted by the
	// caller, its stored API key. The UI intentionally does not expose the
	// legacy reasoning_effort field, but Test must still mirror runtime.
	if req.ModelID != "" {
		model, err := s.db.GetModel(r.Context(), req.ModelID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Model not found: %v", err), http.StatusNotFound)
			return
		}
		if req.APIKey == "" {
			req.APIKey = model.ApiKey
		}
		if req.ReasoningEffort == nil {
			req.ReasoningEffort = &model.ReasoningEffort
		}
	}

	if req.ProviderType == "" || req.Endpoint == "" || req.APIKey == "" || req.ModelName == "" {
		http.Error(w, "provider_type, endpoint, api_key, and model_name are required", http.StatusBadRequest)
		return
	}

	reasoningEffort := ""
	if req.ReasoningEffort != nil {
		var err error
		reasoningEffort, err = validReasoningEffort(*req.ReasoningEffort)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if !validCustomProviderType(req.ProviderType) {
		http.Error(w, "Invalid provider_type", http.StatusBadRequest)
		return
	}
	supportsReasoning, err := models.ResolveSupportsReasoning(req.Endpoint, req.ModelName, req.ReasoningSupport)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	options := models.OpenAIResponsesOptions{
		SupportsImages: true, SupportsWebSearch: true,
		SupportsReasoning:     supportsReasoning,
		DefaultReasoningLevel: "medium", ReasoningEffort: reasoningEffort,
	}
	var chat damodel.Chat
	if models.APIType(req.ProviderType) == models.APITypeOpenRouterResponses {
		options.SupportsWebSearch = false
		chat = models.NewOpenRouterResponses(req.APIKey, req.ModelName, req.Endpoint, nil, options)
	} else {
		chat = models.NewOpenAIResponses(req.APIKey, req.ModelName, req.Endpoint, nil, options)
	}
	if _, err := validReasoningSupport(req.ReasoningSupport); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validReasoningMap(req.ReasoningMap); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chat, err = models.WrapReasoningConfig(chat, req.Endpoint, req.ModelName, req.ReasoningSupport, req.ReasoningMap)
	if err != nil {
		http.Error(w, "Invalid reasoning configuration", http.StatusBadRequest)
		return
	}

	// Send a simple test request
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	response, err := chat.Invoke(ctx, damodel.Request{Messages: []dmessage.Message{
		dmessage.Human("Say 'test successful' in exactly two words."),
	}})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("Test failed: %v", err),
		})
		return
	}

	// Check if we got a response with actual text content
	// (skip thinking blocks which may appear first)
	var responseText string
	for _, content := range response.Message.Content {
		if content.Type == dmessage.BlockText && content.Text != "" {
			responseText = content.Text
			break
		}
	}

	if responseText == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Test failed: empty response from model",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Test successful! Response: %s", responseText),
	})
}
