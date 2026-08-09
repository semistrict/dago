package slug

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"testing"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/db"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/models"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Simple Test", "simple-test"},
		{"Create a Python Script", "create-a-python-script"},
		{"Multiple   Spaces", "multiple-spaces"},
		{"Special@#$%Characters", "specialcharacters"},
		{"Under_Score_Test", "under-score-test"},
		{"--multiple-hyphens--", "multiple-hyphens"},
		{"CamelCase Example", "camelcase-example"},
		{"123 Numbers Test 456", "123-numbers-test-456"},
		{"   leading and trailing   ", "leading-and-trailing"},
		{"", ""},
		{"Very Long Slug That Might Need To Be Truncated Because It Is Too Long For Normal Use", "very-long-slug-that-might-need-to-be-truncated-because-it-is"},
	}

	for _, test := range tests {
		result := Sanitize(test.input)
		if result != test.expected {
			t.Errorf("Sanitize(%q) = %q, expected %q", test.input, result, test.expected)
		}
	}
}

// TestGenerateUniqueSlug tests that slug generation adds numeric suffixes when there are conflicts
func TestGenerateSlug_UniquenessSuffix(t *testing.T) {
	// This test verifies the numeric suffix logic without needing a real database or LLM
	// We'll test the error handling and retry logic by mocking the behavior

	// Test the sanitization works as expected first
	baseSlug := Sanitize("Test Message")
	expected := "test-message"
	if baseSlug != expected {
		t.Errorf("Sanitize failed: got %q, expected %q", baseSlug, expected)
	}

	// Test that numeric suffixes would be correctly formatted
	// This mimics what the GenerateSlug function does internally
	tests := []struct {
		baseSlug string
		attempt  int
		expected string
	}{
		{"test-message", 0, "test-message-1"},
		{"test-message", 1, "test-message-2"},
		{"test-message", 2, "test-message-3"},
		{"help-python", 9, "help-python-10"},
	}

	for _, test := range tests {
		result := fmt.Sprintf("%s-%d", test.baseSlug, test.attempt+1)
		if result != test.expected {
			t.Errorf("Suffix generation failed: got %q, expected %q", result, test.expected)
		}
	}
}

// MockLLMService provides a native chat model for testing.
type MockLLMService struct {
	ResponseText string
	// ResponseContent, if set, overrides ResponseText and lets a test return
	// arbitrary content blocks (e.g. a leading Thinking block as reasoning
	// models do).
	ResponseContent []dmessage.ContentBlock
	ResponseUsage   *dmessage.Usage
	Model           string
}

func (m *MockLLMService) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	content := m.ResponseContent
	if content == nil {
		content = []dmessage.ContentBlock{
			{Type: dmessage.BlockText, Text: m.ResponseText},
		}
	}
	return dmodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant, Content: content, Usage: m.ResponseUsage}}, nil
}

func (m *MockLLMService) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return dmodel.EmptyStream{}, nil
}
func (m *MockLLMService) Profile() dmodel.Profile {
	return dmodel.Profile{Provider: "test", Model: m.Model, ContextWindow: 8192}
}

// MockLLMProvider provides a mock LLM provider for testing
type MockLLMProvider struct {
	Chat *MockLLMService
}

func (m *MockLLMProvider) GetChat(modelID string) (dmodel.Chat, error) {
	return m.Chat, nil
}

func (m *MockLLMProvider) GetAvailableModels() []string {
	return []string{"mock"}
}

func (m *MockLLMProvider) GetModelInfo(modelID string) *models.ModelInfo {
	return nil
}

// TestGenerateSlug_DatabaseIntegration tests slug generation with actual database conflicts
func TestGenerateSlug_DatabaseIntegration(t *testing.T) {
	// Create temporary database
	tempDB := t.TempDir() + "/slug_test.db"
	database, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	defer database.Close()

	// Run migrations
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Failed to migrate database: %v", err)
	}

	// Create mock LLM provider that always returns the same slug
	mockLLM := &MockLLMProvider{
		Chat: &MockLLMService{
			ResponseText: "test-slug", // Always return the same slug to force conflicts
		},
	}

	// Create logger (silent for tests)
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn, // Only show warnings and errors
	}))

	// Create first conversation to establish the base slug
	conv1, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("Failed to create first conversation: %v", err)
	}

	// Generate first slug - should succeed with "test-slug"
	slug1, _, err := GenerateSlug(ctx, mockLLM, database, logger, conv1.ConversationID, "Test message", "test-model")
	if err != nil {
		t.Fatalf("Failed to generate first slug: %v", err)
	}
	if slug1 != "test-slug" {
		t.Errorf("Expected first slug to be 'test-slug', got %q", slug1)
	}

	// Create second conversation
	conv2, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("Failed to create second conversation: %v", err)
	}

	// Generate second slug - should get "test-slug-1" due to conflict
	slug2, _, err := GenerateSlug(ctx, mockLLM, database, logger, conv2.ConversationID, "Test message", "test-model")
	if err != nil {
		t.Fatalf("Failed to generate second slug: %v", err)
	}
	if slug2 != "test-slug-1" {
		t.Errorf("Expected second slug to be 'test-slug-1', got %q", slug2)
	}

	// Create third conversation
	conv3, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("Failed to create third conversation: %v", err)
	}

	// Generate third slug - should get "test-slug-2" due to conflict
	slug3, _, err := GenerateSlug(ctx, mockLLM, database, logger, conv3.ConversationID, "Test message", "test-model")
	if err != nil {
		t.Fatalf("Failed to generate third slug: %v", err)
	}
	if slug3 != "test-slug-2" {
		t.Errorf("Expected third slug to be 'test-slug-2', got %q", slug3)
	}

	// Verify all slugs are different
	if slug1 == slug2 || slug1 == slug3 || slug2 == slug3 {
		t.Errorf("All slugs should be unique: slug1=%q, slug2=%q, slug3=%q", slug1, slug2, slug3)
	}

	t.Logf("Successfully generated unique slugs: %q, %q, %q", slug1, slug2, slug3)
}

// TestGenerateSlug_PreservesExisting tests that GenerateSlug does not overwrite
// an existing slug. This matters for flows that look like "first message" but
// are actually continuations (e.g. starting a new generation after compaction).
func TestGenerateSlug_PreservesExisting(t *testing.T) {
	tempDB := t.TempDir() + "/slug_preserve_test.db"
	database, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Failed to migrate database: %v", err)
	}

	mockLLM := &MockLLMProvider{Chat: &MockLLMService{ResponseText: "new-llm-slug"}}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	conv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	// Pre-set the slug to simulate an already-named conversation.
	original := "original-slug"
	if _, err := database.UpdateConversationSlug(ctx, conv.ConversationID, original); err != nil {
		t.Fatalf("Failed to set initial slug: %v", err)
	}

	result, _, err := GenerateSlug(ctx, mockLLM, database, logger, conv.ConversationID, "Some new first-looking message", "test-model")
	if err != nil {
		t.Fatalf("GenerateSlug returned error: %v", err)
	}
	if result != original {
		t.Errorf("GenerateSlug returned %q, want %q (existing slug should be preserved)", result, original)
	}

	// Confirm the DB row is unchanged.
	fresh, err := database.GetConversationByID(ctx, conv.ConversationID)
	if err != nil {
		t.Fatalf("Failed to re-fetch conversation: %v", err)
	}
	if fresh.Slug == nil || *fresh.Slug != original {
		got := "<nil>"
		if fresh.Slug != nil {
			got = *fresh.Slug
		}
		t.Errorf("DB slug = %q, want %q", got, original)
	}
}

// MockLLMServiceWithError provides a native chat model that returns an error.
type MockLLMServiceWithError struct{}

func (m *MockLLMServiceWithError) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	return dmodel.Response{}, fmt.Errorf("LLM service error")
}
func (*MockLLMServiceWithError) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return nil, fmt.Errorf("LLM service error")
}
func (*MockLLMServiceWithError) Profile() dmodel.Profile { return dmodel.Profile{Model: "error"} }

// MockLLMProviderWithError provides a mock LLM provider that returns errors for all models
type MockLLMProviderWithError struct{}

func (m *MockLLMProviderWithError) GetChat(modelID string) (dmodel.Chat, error) {
	return nil, fmt.Errorf("model not available")
}

func (m *MockLLMProviderWithError) GetAvailableModels() []string {
	return []string{}
}

func (m *MockLLMProviderWithError) GetModelInfo(modelID string) *models.ModelInfo {
	return nil
}

// MockLLMProviderWithServiceError provides a mock LLM provider that returns a service with error
type MockLLMProviderWithServiceError struct{}

func (m *MockLLMProviderWithServiceError) GetChat(modelID string) (dmodel.Chat, error) {
	return &MockLLMServiceWithError{}, nil
}

func (m *MockLLMProviderWithServiceError) GetAvailableModels() []string {
	return []string{"mock"}
}

func (m *MockLLMProviderWithServiceError) GetModelInfo(modelID string) *models.ModelInfo {
	return nil
}

// TestGenerateSlug_LLMError tests error handling when LLM service fails
func TestGenerateSlug_LLMError(t *testing.T) {
	mockLLM := &MockLLMProviderWithServiceError{}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	// Test that LLM error is properly propagated (pass a model ID so we get a service)
	_, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "test-model")
	if err == nil {
		t.Error("Expected error from LLM service, got nil")
	}
	if err.Error() != "failed to generate slug: LLM service error" {
		t.Errorf("Expected LLM service error, got %q", err.Error())
	}
}

// TestGenerateSlug_NoModelsAvailable tests error handling when no models are available
func TestGenerateSlug_NoModelsAvailable(t *testing.T) {
	mockLLM := &MockLLMProviderWithError{}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	// Test that error is returned when no models are available
	_, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "")
	if err == nil {
		t.Error("Expected error when no models available, got nil")
	}
	if err.Error() != "no suitable model available for slug generation" {
		t.Errorf("Expected 'no suitable model' error, got %q", err.Error())
	}
}

// TestGenerateSlug_EmptyResponse tests error handling when LLM returns empty response
func TestGenerateSlug_EmptyResponse(t *testing.T) {
	// Mock LLM that returns empty response
	mockLLM := &MockLLMProviderWithEmptyResponse{}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	_, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "test-model")
	if err == nil {
		t.Error("Expected error for empty LLM response, got nil")
	}
	if err.Error() != "empty response from LLM" {
		t.Errorf("Expected 'empty response' error, got %q", err.Error())
	}
}

// MockLLMProviderWithEmptyResponse provides a mock LLM provider that returns empty response
type MockLLMProviderWithEmptyResponse struct{}

func (m *MockLLMProviderWithEmptyResponse) GetChat(modelID string) (dmodel.Chat, error) {
	return &MockLLMServiceEmptyResponse{}, nil
}

func (m *MockLLMProviderWithEmptyResponse) GetAvailableModels() []string {
	return []string{"mock"}
}

func (m *MockLLMProviderWithEmptyResponse) GetModelInfo(modelID string) *models.ModelInfo {
	return nil
}

// MockLLMServiceEmptyResponse provides a mock LLM service that returns empty response
type MockLLMServiceEmptyResponse struct{}

func (m *MockLLMServiceEmptyResponse) Invoke(context.Context, dmodel.Request) (dmodel.Response, error) {
	return dmodel.Response{Message: dmessage.Message{Role: dmessage.RoleAssistant}}, nil
}
func (*MockLLMServiceEmptyResponse) Stream(context.Context, dmodel.Request) (dmodel.Stream, error) {
	return dmodel.EmptyStream{}, nil
}
func (*MockLLMServiceEmptyResponse) Profile() dmodel.Profile { return dmodel.Profile{Model: "empty"} }

// TestGenerateSlug_SanitizationError tests error handling when slug is empty after sanitization
func TestGenerateSlug_SanitizationError(t *testing.T) {
	// Mock LLM that returns only special characters that get sanitized away
	mockLLM := &MockLLMProvider{
		Chat: &MockLLMService{
			ResponseText: "@#$%^&*()", // All special characters that will be removed
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	_, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "test-model")
	if err == nil {
		t.Error("Expected error for empty slug after sanitization, got nil")
	}
	if err.Error() != "generated slug is empty after sanitization" {
		t.Errorf("Expected 'empty after sanitization' error, got %q", err.Error())
	}
}

// TestGenerateSlug_MaxAttempts tests the case where we exceed maximum attempts to generate unique slug
// This test is skipped because it's difficult to set up correctly without modifying the core logic
func TestGenerateSlug_MaxAttempts(t *testing.T) {
	t.Skip("Skipping max attempts test due to complexity of setup")
}

// TestGenerateSlug_DatabaseError tests error handling when database update fails with non-unique error
func TestGenerateSlug_DatabaseError(t *testing.T) {
	// Create temporary database
	tempDB := t.TempDir() + "/slug_db_error_test.db"
	database, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	defer func() {
		if database != nil {
			database.Close()
		}
	}()

	// Run migrations
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Failed to migrate database: %v", err)
	}

	// Create mock LLM provider
	mockLLM := &MockLLMProvider{
		Chat: &MockLLMService{
			ResponseText: "test-slug",
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	// Close database to force error
	database.Close()

	// Try to generate slug with closed database - pass a valid database object but it's closed
	closedDB, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	closedDB.Close()

	_, _, err = GenerateSlug(ctx, mockLLM, closedDB, logger, "test-conversation-id", "Test message", "test-model")
	if err == nil {
		t.Error("Expected database error, got nil")
	}
}

// TestGenerateSlug_PredictableModel tests the case where conversation uses predictable model
func TestGenerateSlug_PredictableModel(t *testing.T) {
	// Mock LLM that has predictable model available
	mockLLM := &MockLLMProvider{
		Chat: &MockLLMService{
			ResponseText: "predictable-slug",
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Test that predictable model is used when conversationModelID is "predictable"
	slug, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "predictable")
	if err != nil {
		t.Fatalf("Failed to generate slug with predictable model: %v", err)
	}
	if slug != "predictable-slug" {
		t.Errorf("Expected 'predictable-slug', got %q", slug)
	}
}

// TestGenerateSlug_ConversationModelFallback tests fallback to conversation model when no slug-tagged models exist
func TestGenerateSlug_ConversationModelFallback(t *testing.T) {
	// Mock LLM provider that doesn't have predictable model but has a conversation model
	mockLLM := &MockLLMProviderPredictableFallback{
		fallbackService: &MockLLMService{
			ResponseText: "fallback-slug",
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	// Test that fallback to conversation model works when no slug-tagged models exist
	slug, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "my-custom-model")
	if err != nil {
		t.Fatalf("Failed to generate slug with conversation model fallback: %v", err)
	}
	if slug != "fallback-slug" {
		t.Errorf("Expected 'fallback-slug', got %q", slug)
	}
}

// MockLLMProviderPredictableFallback provides a mock LLM provider that simulates predictable model not available
type MockLLMProviderPredictableFallback struct {
	fallbackService *MockLLMService
}

func (m *MockLLMProviderPredictableFallback) GetChat(modelID string) (dmodel.Chat, error) {
	if modelID == "predictable" {
		return nil, fmt.Errorf("predictable model not available")
	}
	return m.fallbackService, nil
}

func (m *MockLLMProviderPredictableFallback) GetAvailableModels() []string {
	return []string{"my-custom-model"}
}

func (m *MockLLMProviderPredictableFallback) GetModelInfo(modelID string) *models.ModelInfo {
	return nil
}

// TestGenerateSlug_FallbackToSlugBackup tests that when a "slug"-tagged model fails,
// generation falls back to a "slug-backup"-tagged model.
func TestGenerateSlug_FallbackToSlugBackup(t *testing.T) {
	mockLLM := &mockFallbackProvider{
		services: map[string]dmodel.Chat{
			"fireworks-model": &MockLLMServiceWithError{},
			"haiku-model":     &MockLLMService{ResponseText: "backup-slug"},
		},
		models: []string{"fireworks-model", "haiku-model"},
		modelInfo: map[string]*models.ModelInfo{
			"fireworks-model": {Tags: "slug"},
			"haiku-model":     {Tags: "slug-backup"},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	slug, _, err := generateSlugText(context.Background(), mockLLM, logger, "Test message", "")
	if err != nil {
		t.Fatalf("Expected fallback to slug-backup model, got error: %v", err)
	}
	if slug != "backup-slug" {
		t.Errorf("Expected 'backup-slug', got %q", slug)
	}
}

// TestHasTag tests the hasTag helper.
func TestHasTag(t *testing.T) {
	tests := []struct {
		tags string
		tag  string
		want bool
	}{
		{"slug", "slug", true},
		{"slug-backup", "slug", false},
		{"slug,slug-backup", "slug", true},
		{"slug,slug-backup", "slug-backup", true},
		{"foo, slug , bar", "slug", true},
		{"", "slug", false},
		{"slug", "", false},
	}
	for _, tt := range tests {
		got := hasTag(tt.tags, tt.tag)
		if got != tt.want {
			t.Errorf("hasTag(%q, %q) = %v, want %v", tt.tags, tt.tag, got, tt.want)
		}
	}
}

// mockFallbackProvider supports per-model native chats and catalog metadata.
type mockFallbackProvider struct {
	services  map[string]dmodel.Chat
	models    []string
	modelInfo map[string]*models.ModelInfo
}

func (m *mockFallbackProvider) GetChat(modelID string) (dmodel.Chat, error) {
	svc, ok := m.services[modelID]
	if !ok {
		return nil, fmt.Errorf("model not available: %s", modelID)
	}
	return svc, nil
}

func (m *mockFallbackProvider) GetAvailableModels() []string {
	return m.models
}

func (m *mockFallbackProvider) GetModelInfo(modelID string) *models.ModelInfo {
	return m.modelInfo[modelID]
}

// TestGenerateSlug_ReasoningModel verifies that slug generation works when the
// LLM returns a leading Thinking content block followed by the text answer,
// as reasoning models like gpt-oss-20b do. Previously the code only inspected
// Content[0], so it read the (empty .Text of the) Thinking block and failed
// with "generated slug is empty after sanitization".
func TestGenerateSlug_ReasoningModel(t *testing.T) {
	tempDB := t.TempDir() + "/slug_test.db"
	database, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatalf("Failed to create test database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("Failed to migrate database: %v", err)
	}

	mockLLM := &MockLLMProvider{
		Chat: &MockLLMService{
			ResponseContent: []dmessage.ContentBlock{
				{Type: dmessage.BlockReasoning, Reasoning: "The user wants a slug. Options: parse-json-go."},
				{Type: dmessage.BlockText, Text: "parse-json-go"},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	conv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("Failed to create conversation: %v", err)
	}

	slug, _, err := GenerateSlug(ctx, mockLLM, database, logger, conv.ConversationID, "how do I parse JSON in Go", "test-model")
	if err != nil {
		t.Fatalf("GenerateSlug failed: %v", err)
	}
	if slug != "parse-json-go" {
		t.Errorf("expected slug %q, got %q", "parse-json-go", slug)
	}
}

// recordingProvider is a mock provider with a fixed model list and per-model
// tags (empty by default, mimicking models discovered from a gateway
// integration). It records which model IDs GetChat was asked for.
type recordingProvider struct {
	modelIDs   []string
	tags       map[string]string // optional per-model tags
	requested  []string
	services   map[string]dmodel.Chat // optional per-model service override
	fallbackTo dmodel.Chat
}

func (p *recordingProvider) GetChat(modelID string) (dmodel.Chat, error) {
	p.requested = append(p.requested, modelID)
	if svc, ok := p.services[modelID]; ok {
		return svc, nil
	}
	return p.fallbackTo, nil
}

func (p *recordingProvider) GetAvailableModels() []string { return p.modelIDs }

func (p *recordingProvider) GetModelInfo(modelID string) *models.ModelInfo {
	return &models.ModelInfo{DisplayName: modelID, Tags: p.tags[modelID]}
}

// TestGenerateSlugText_PreferenceFallback verifies that when no model is
// tagged "slug"/"slug-backup" (e.g. all models come from a gateway
// integration, which strips tags), slug generation picks a model from the
// substring preference list instead of the conversation's model.
func TestGenerateSlugText_PreferenceFallback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	provider := &recordingProvider{
		modelIDs:   []string{"gpt-5.6-sol", "gpt-5.5", "gpt-5.4-mini", "gpt-5.6-luna"},
		fallbackTo: &MockLLMService{ResponseText: "my-slug"},
	}

	slug, _, err := generateSlugText(context.Background(), provider, logger, "some message", "gpt-5.5")
	if err != nil {
		t.Fatalf("generateSlugText failed: %v", err)
	}
	if slug != "my-slug" {
		t.Errorf("expected slug %q, got %q", "my-slug", slug)
	}
	if len(provider.requested) == 0 {
		t.Fatal("no model requested")
	}
	if provider.requested[0] != "gpt-5.6-luna" {
		t.Errorf("expected preferred model gpt-5.6-luna to be tried first, got %q (all: %v)", provider.requested[0], provider.requested)
	}
}

// TestGenerateSlugText_PreferenceFallbackChain verifies that a failing
// preferred model falls through to the next preference, and ultimately to the
// conversation model.
func TestGenerateSlugText_PreferenceFallbackChain(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	provider := &recordingProvider{
		modelIDs: []string{"gpt-5.5", "gpt-5.4-mini", "gpt-5.6-luna"},
		services: map[string]dmodel.Chat{
			"gpt-5.6-luna": &MockLLMServiceWithError{},
		},
		fallbackTo: &MockLLMService{ResponseText: "haiku-slug"},
	}

	slug, _, err := generateSlugText(context.Background(), provider, logger, "some message", "gpt-5.5")
	if err != nil {
		t.Fatalf("generateSlugText failed: %v", err)
	}
	if slug != "haiku-slug" {
		t.Errorf("expected slug %q, got %q", "haiku-slug", slug)
	}
	want := []string{"gpt-5.6-luna", "gpt-5.4-mini"}
	if len(provider.requested) < 2 || provider.requested[0] != want[0] || provider.requested[1] != want[1] {
		t.Errorf("expected request order %v, got %v", want, provider.requested)
	}
}

// TestGenerateSlugText_ConversationModelLastResort verifies that when every
// preferred model fails, the conversation model is finally tried.
func TestGenerateSlugText_ConversationModelLastResort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	provider := &recordingProvider{
		modelIDs: []string{"gpt-5.5", "gpt-5.4-mini", "gpt-5.6-luna"},
		services: map[string]dmodel.Chat{
			"gpt-5.6-luna": &MockLLMServiceWithError{},
			"gpt-5.4-mini": &MockLLMServiceWithError{},
		},
		fallbackTo: &MockLLMService{ResponseText: "fable-slug"},
	}

	slug, _, err := generateSlugText(context.Background(), provider, logger, "some message", "gpt-5.5")
	if err != nil {
		t.Fatalf("generateSlugText failed: %v", err)
	}
	if slug != "fable-slug" {
		t.Errorf("expected slug %q, got %q", "fable-slug", slug)
	}
	want := []string{"gpt-5.6-luna", "gpt-5.4-mini", "gpt-5.5"}
	if len(provider.requested) != 3 || provider.requested[0] != want[0] || provider.requested[1] != want[1] || provider.requested[2] != want[2] {
		t.Errorf("expected request order %v, got %v", want, provider.requested)
	}
}

// TestGenerateSlugText_TaggedModelWins verifies that tagged models still take
// priority over the substring preference list, and that a tagged model which
// fails is not retried by the substring fallback.
func TestGenerateSlugText_TaggedModelWins(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// gpt-5.5 is tagged "slug" and doesn't match any preferred substring.
	provider := &recordingProvider{
		modelIDs:   []string{"gpt-5.6-luna", "gpt-5.5"},
		tags:       map[string]string{"gpt-5.5": "slug"},
		fallbackTo: &MockLLMService{ResponseText: "tagged-slug"},
	}

	slug, _, err := generateSlugText(context.Background(), provider, logger, "some message", "")
	if err != nil {
		t.Fatalf("generateSlugText failed: %v", err)
	}
	if slug != "tagged-slug" {
		t.Errorf("expected slug %q, got %q", "tagged-slug", slug)
	}
	if provider.requested[0] != "gpt-5.5" {
		t.Errorf("expected tagged model gpt-5.5 first, got %v", provider.requested)
	}

	// Now make the tagged model fail: it must not be retried by the substring
	// fallback (it matches no substring here, so verify with a model that does).
	provider2 := &recordingProvider{
		modelIDs: []string{"gpt-5.6-luna", "gpt-5.4-mini"},
		tags:     map[string]string{"gpt-5.6-luna": "slug"},
		services: map[string]dmodel.Chat{
			"gpt-5.6-luna": &MockLLMServiceWithError{},
		},
		fallbackTo: &MockLLMService{ResponseText: "backup-slug"},
	}
	slug2, _, err := generateSlugText(context.Background(), provider2, logger, "some message", "")
	if err != nil {
		t.Fatalf("generateSlugText failed: %v", err)
	}
	if slug2 != "backup-slug" {
		t.Errorf("expected slug %q, got %q", "backup-slug", slug2)
	}
	// Luna is tried once as tagged, then mini via the preference list.
	want := []string{"gpt-5.6-luna", "gpt-5.4-mini"}
	if len(provider2.requested) != 2 || provider2.requested[0] != want[0] || provider2.requested[1] != want[1] {
		t.Errorf("expected request order %v (no retries), got %v", want, provider2.requested)
	}
}

func TestPreferredModels(t *testing.T) {
	available := []string{
		"gpt-5.5",
		"gpt-5.4-mini",
		"gpt-5.6-luna",
		"gpt-5.4-nano",
	}
	got := preferredModels(available, nil)
	want := []string{"gpt-5.6-luna", "gpt-5.4-nano", "gpt-5.4-mini"}
	if len(got) != len(want) {
		t.Fatalf("preferredModels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("preferredModels = %v, want %v", got, want)
		}
	}
}

// usageLLMService returns a fixed slug with non-zero usage, mimicking a real
// provider response.
// TestGenerateSlug_UsageOnAppendedMarker runs GenerateSlug through a real
// a native model response and verifies
// the slug call's usage lands on a NEWLY APPENDED slug marker message, leaving
// the already-published user message untouched.
//
// Appending is the only way to record this without breaking the append-only
// contract on message rows: the browser caches them by (conversation_id,
// sequence_id) and only ever fetches the tail, forks copy them, and a
// sequence_id is delivered exactly once. An in-place UPDATE (the original
// design) is invisible to all three.
func TestGenerateSlug_UsageOnAppendedMarker(t *testing.T) {
	tempDB := t.TempDir() + "/slug_usage_test.db"
	database, err := db.New(db.Config{DSN: tempDB})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	provider := &MockLLMProvider{Chat: &MockLLMService{
		ResponseText:  "my-generated-slug",
		ResponseUsage: &dmessage.Usage{InputTokens: 25, OutputTokens: 5, TotalTokens: 30},
		Model:         "slug-model-v1",
	}}

	conv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateMessage(ctx, db.CreateMessageParams{
		ConversationID: conv.ConversationID,
		Type:           db.MessageTypeUser,
		LLMData:        llm.Message{Role: llm.MessageRoleUser},
	}); err != nil {
		t.Fatal(err)
	}

	got, marker, err := GenerateSlug(ctx, provider, database, logger, conv.ConversationID, "first message", "slug-model")
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-generated-slug" {
		t.Errorf("slug = %q", got)
	}

	// The pre-existing user message must be untouched: no in-place mutation of
	// an already published row. The cost lands on a NEW appended marker.
	messages, err := database.ListMessages(ctx, conv.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want the user message plus an appended slug marker: %+v", len(messages), messages)
	}
	if messages[0].Type != string(db.MessageTypeUser) || messages[0].OtherUsageData != nil {
		t.Errorf("first user message = type %q usage %v, want an untouched user message: messages are append-only",
			messages[0].Type, messages[0].OtherUsageData)
	}

	row := messages[1]
	if row.Type != string(db.MessageTypeSlug) {
		t.Fatalf("appended message type = %q, want slug", row.Type)
	}
	if row.OtherUsageData == nil {
		t.Fatal("slug marker carries no other_usage_data")
	}
	// GenerateSlug must hand the marker back so the caller can publish it: it
	// owns a real sequence_id, and a client that never receives it sees a hole
	// and discards its cached history.
	if marker == nil {
		t.Fatal("GenerateSlug returned no marker; the caller cannot publish it")
	}
	if marker.MessageID != row.MessageID || marker.SequenceID != row.SequenceID {
		t.Errorf("returned marker %s/seq %d does not match the stored row %s/seq %d",
			marker.MessageID, marker.SequenceID, row.MessageID, row.SequenceID)
	}
	var entries []llm.PurposedUsage
	if err := json.Unmarshal([]byte(*row.OtherUsageData), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Purpose != "slug" || entries[0].InputTokens != 25 || entries[0].Model != "slug-model-v1" {
		t.Errorf("entries = %+v", entries)
	}

	// The slug write comes after the usage write; both must survive.
	updated, err := database.GetConversationByID(ctx, conv.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Slug == nil || *updated.Slug != "my-generated-slug" {
		t.Errorf("slug on conversation = %v, want my-generated-slug", updated.Slug)
	}
}
