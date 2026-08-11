package models

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	dmessage "github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"

	"github.com/semistrict/dago/examples/shelley/claudetool"
	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/db/generated"
	"github.com/semistrict/dago/examples/shelley/llm"
	"github.com/semistrict/dago/examples/shelley/llm/llmhttp"
	"github.com/semistrict/dago/examples/shelley/loop"
)

func testIDs() []string {
	models := All()
	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
	}
	return ids
}

// predictableBuilt returns a Built entry for the predictable test model.
// Tests that need a manager seeded with at least one model use this.
func predictableBuilt() Built {
	return Built{
		ID:       "predictable",
		Provider: ProviderBuiltIn,
		Source:   "test",
		Chat:     loop.NewPredictableService(),
	}
}

func TestAll(t *testing.T) {
	models := All()
	if len(models) == 0 {
		t.Fatal("expected at least one model")
	}
	for _, m := range models {
		if m.ID == "" {
			t.Errorf("model missing ID")
		}
		if m.Provider == "" {
			t.Errorf("model %s missing Provider", m.ID)
		}
		if m.Build == nil {
			t.Errorf("model %s missing Build", m.ID)
		}
	}
}

func TestByID(t *testing.T) {
	tests := []struct {
		id      string
		wantID  string
		wantNil bool
	}{
		{id: "gpt-5.6-sol", wantID: "gpt-5.6-sol"},
		{id: "gpt-5.6-terra", wantID: "gpt-5.6-terra"},
		{id: "gpt-5.6-luna", wantID: "gpt-5.6-luna"},
		{id: "gpt-5.5", wantID: "gpt-5.5"},
		{id: "gpt-5.5-pro", wantNil: true},
		{id: "gpt-5.4", wantID: "gpt-5.4"},
		{id: "gpt-5.4-mini", wantID: "gpt-5.4-mini"},
		{id: "gpt-5.4-nano", wantID: "gpt-5.4-nano"},
		{id: "gpt-5.3-codex", wantID: "gpt-5.3-codex"},
		{id: "deepseek-v4-pro-fireworks", wantNil: true},
		{id: "claude-opus-4.8", wantNil: true},
		{id: "grok-4.5", wantNil: true},
		{id: "nonexistent", wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			m := ByID(tt.id)
			if tt.wantNil {
				if m != nil {
					t.Errorf("ByID(%q) = %v, want nil", tt.id, m)
				}
				return
			}
			if m == nil {
				t.Fatalf("ByID(%q) = nil, want non-nil", tt.id)
			}
			if m.ID != tt.wantID {
				t.Errorf("ByID(%q).ID = %q, want %q", tt.id, m.ID, tt.wantID)
			}
		})
	}
}

func TestKimiK3FireworksCatalogEntry(t *testing.T) {
	if ByID("kimi-k3-fireworks") != nil {
		t.Fatal("unsupported Chat Completions model remains in the catalog")
	}
	for _, model := range All() {
		if model.APIType != APITypeOpenAIResponses && model.APIType != APITypeBuiltIn {
			t.Errorf("model %q uses unsupported API type %q", model.ID, model.APIType)
		}
	}
}

func TestDefault(t *testing.T) {
	if d := Default(); d.ID != "gpt-5.6-sol" {
		t.Errorf("Default().ID = %q, want %q", d.ID, "gpt-5.6-sol")
	}
}

func TestIDs(t *testing.T) {
	ids := testIDs()
	if len(ids) == 0 {
		t.Fatal("expected at least one model ID")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate model ID: %s", id)
		}
		seen[id] = true
	}
}

func TestNewManagerRegistersBuiltModels(t *testing.T) {
	mgr, err := NewManager(&Config{Models: []Built{predictableBuilt()}})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	chat, err := mgr.GetChat("predictable")
	if err != nil || chat == nil {
		t.Fatalf("GetChat(predictable) failed: chat=%v err=%v", chat, err)
	}
	info := mgr.GetModelInfo("predictable")
	if info == nil {
		t.Fatalf("GetModelInfo(predictable) = nil")
	}
	if info.Source != "test" {
		t.Errorf("source = %q, want %q", info.Source, "test")
	}
	if info.DisplayName != "predictable" {
		t.Errorf("display name = %q, want %q", info.DisplayName, "predictable")
	}
}

func TestGetAvailableModelsOrderStable(t *testing.T) {
	mgr, err := NewManager(&Config{Models: []Built{predictableBuilt()}})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	a := mgr.GetAvailableModels()
	b := mgr.GetAvailableModels()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("unstable lengths %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("index %d differs: %q vs %q", i, a[i], b[i])
		}
	}
}

func TestLoggingService(t *testing.T) {
	mockModel := &mockChat{}
	logger := slog.Default()
	loggingModel := &loggingChat{chat: mockModel, logger: logger, modelID: "test-model", provider: ProviderBuiltIn}

	response, err := loggingModel.Invoke(context.Background(), damodel.Request{Messages: []dmessage.Message{dmessage.Human("Hello")}})
	if err != nil || response.Message.TextContent() == "" {
		t.Fatalf("Invoke: response=%v err=%v", response, err)
	}
	if loggingModel.Profile().ContextWindow != mockModel.Profile().ContextWindow {
		t.Errorf("TokenContextWindow mismatch")
	}
	if loggingModel.Profile().MaxImageDimension != mockModel.Profile().MaxImageDimension {
		t.Errorf("MaxImageDimension mismatch")
	}
}

func TestLoggingServiceUsageCollector(t *testing.T) {
	type collected struct {
		purpose string
		usage   llm.Usage
	}
	var got []collected
	chat := &loggingChat{
		chat:    &mockChat{},
		logger:  slog.Default(),
		modelID: "test-model",
	}
	req := damodel.Request{Messages: []dmessage.Message{dmessage.Human("hi")}}
	ctxWithCollector := llmhttp.WithUsageCollector(context.Background(), func(purpose string, usage llm.Usage) {
		got = append(got, collected{purpose, usage})
	})

	// No purpose tag: nothing collected even with a collector.
	if _, err := chat.Invoke(ctxWithCollector, req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("collected %d calls without purpose, want 0", len(got))
	}

	// Purpose tag: collected, model falls back to modelID (mock leaves Model empty).
	ctx := llmhttp.WithPurpose(ctxWithCollector, "keyword_search")
	if _, err := chat.Invoke(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("collected %d calls, want 1", len(got))
	}
	r := got[0]
	if r.purpose != "keyword_search" || r.usage.Model != "test-model" {
		t.Errorf("collected purpose=%q model=%q, want keyword_search/test-model", r.purpose, r.usage.Model)
	}
	if r.usage.InputTokens != 10 || r.usage.OutputTokens != 5 || r.usage.CostUSD != 0.001 {
		t.Errorf("collected usage = %+v", r.usage)
	}

	// Zero usage: not collected even with a purpose tag.
	chat.chat = &mockChat{zeroUsage: true}
	if _, err := chat.Invoke(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("zero-usage response was collected (%d calls)", len(got))
	}

	// Purpose tag but no collector in ctx: no panic, nothing collected.
	chat.chat = &mockChat{}
	if _, err := chat.Invoke(llmhttp.WithPurpose(context.Background(), "keyword_search"), req); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("collector-less call was collected (%d calls)", len(got))
	}
}

type mockChat struct {
	tokenContextWindow int
	maxImageDimension  int
	useSimplifiedPatch bool
	zeroUsage          bool
	gotReasoning       string
	defaultReasoning   string
}

func (m *mockChat) Invoke(_ context.Context, request damodel.Request) (damodel.Response, error) {
	if request.Reasoning != nil {
		m.gotReasoning = request.Reasoning.Effort
	}
	message := dmessage.Assistant("Hello, world!")
	if !m.zeroUsage {
		message.Usage = &dmessage.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.001}
	}
	return damodel.Response{Message: message}, nil
}

func (m *mockChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

func (m *mockChat) Profile() damodel.Profile {
	contextWindow := m.tokenContextWindow
	if contextWindow == 0 {
		contextWindow = 4096
	}
	imageDimension := m.maxImageDimension
	if imageDimension == 0 {
		imageDimension = 2048
	}
	return damodel.Profile{ContextWindow: contextWindow, MaxImageDimension: imageDimension, MaxImageBytes: 5 * 1024 * 1024, SupportsImages: true, UseSimplifiedPatch: m.useSimplifiedPatch, SupportsReasoning: true, DefaultReasoningLevel: m.defaultReasoning}
}

func (m *mockChat) TokenContextWindow() int {
	if m.tokenContextWindow == 0 {
		return 4096
	}
	return m.tokenContextWindow
}

func (m *mockChat) MaxImageDimension() int {
	if m.maxImageDimension == 0 {
		return 2048
	}
	return m.maxImageDimension
}

func TestManagerGetService(t *testing.T) {
	mgr, err := NewManager(&Config{Models: []Built{predictableBuilt()}})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	if chat, err := mgr.GetChat("predictable"); err != nil || chat == nil {
		t.Errorf("GetChat(predictable): chat=%v err=%v", chat, err)
	}
	if _, err := mgr.GetChat("non-existent-model"); err == nil {
		t.Error("GetChat(non-existent) should have failed")
	}
}

func TestManagerHasModel(t *testing.T) {
	mgr, err := NewManager(&Config{Models: []Built{predictableBuilt()}})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	if !mgr.HasModel("predictable") {
		t.Error("HasModel(predictable) should return true")
	}
	if mgr.HasModel("claude-opus-4.7") {
		t.Error("HasModel(claude-opus-4.7) should return false without sources")
	}
	if mgr.HasModel("non-existent-model") {
		t.Error("HasModel(non-existent) should return false")
	}
}

func TestModelBuildSignature(t *testing.T) {
	// Each catalog model's Build must produce a non-nil native chat model when
	// given any URL/key and an http.Client.
	customClient := &http.Client{}
	for _, m := range All() {
		chat, err := m.Build("https://example.test/v1", "key", customClient)
		if err != nil || chat == nil {
			t.Errorf("Build(%s) returned nil", m.ID)
		}
	}
}

func TestUseSimplifiedPatch(t *testing.T) {
	logger := slog.Default()
	plain := &loggingChat{chat: &mockChat{}, logger: logger, modelID: "t1", provider: ProviderBuiltIn}
	if plain.Profile().UseSimplifiedPatch {
		t.Error("plain mock should not implement SimplifiedPatcher")
	}
	with := &loggingChat{chat: &mockChat{useSimplifiedPatch: true}, logger: logger, modelID: "t2", provider: ProviderBuiltIn}
	if !with.Profile().UseSimplifiedPatch {
		t.Error("simplified mock should return true")
	}
}

func TestRefreshCustomModelsConcurrent(t *testing.T) {
	testDB, err := db.New(db.Config{DSN: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	defer testDB.Close()
	if err := testDB.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test db: %v", err)
	}
	if _, err := testDB.CreateModel(context.Background(), generated.CreateModelParams{
		ModelID:      "custom-test-model",
		DisplayName:  "Test Model",
		ProviderType: "openai-responses",
		Endpoint:     "https://api.example.com/v1",
		ApiKey:       "test-key",
		ModelName:    "test-model",
		MaxTokens:    4096,
	}); err != nil {
		t.Fatalf("failed to create test model: %v", err)
	}

	mgr, err := NewManager(&Config{DB: testDB})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	var wg sync.WaitGroup
	const N = 10
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mgr.GetAvailableModels()
				mgr.HasModel("custom-test-model")
				mgr.GetModelInfo("custom-test-model")
				mgr.GetChat("custom-test-model")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			mgr.RefreshCustomModels()
		}
	}()
	wg.Wait()
}

func TestRefreshBuiltModelsReplacesBuiltModelsAndPreservesCustomModels(t *testing.T) {
	testDB, err := db.New(db.Config{DSN: t.TempDir() + "/test.db"})
	if err != nil {
		t.Fatalf("failed to create test db: %v", err)
	}
	defer testDB.Close()
	if err := testDB.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test db: %v", err)
	}
	if _, err := testDB.CreateModel(context.Background(), generated.CreateModelParams{
		ModelID:      "custom-test-model",
		DisplayName:  "Test Model",
		ProviderType: "openai-responses",
		Endpoint:     "https://api.example.com/v1",
		ApiKey:       "test-key",
		ModelName:    "test-model",
		MaxTokens:    4096,
	}); err != nil {
		t.Fatalf("failed to create test model: %v", err)
	}

	mgr, err := NewManager(&Config{
		Models: []Built{
			{
				ID:          "old-built",
				DisplayName: "Old Built",
				Provider:    ProviderBuiltIn,
				Source:      "old source",
				Chat:        &mockChat{},
			},
		},
		DB: testDB,
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	if err := mgr.RefreshBuiltModels([]Built{
		{
			ID:          "new-built",
			DisplayName: "New Built",
			Provider:    ProviderBuiltIn,
			Source:      "new source",
			Chat:        &mockChat{},
		},
	}); err != nil {
		t.Fatalf("RefreshBuiltModels failed: %v", err)
	}

	if mgr.HasModel("old-built") {
		t.Fatal("old built model was not removed")
	}
	if !mgr.HasModel("new-built") {
		t.Fatal("new built model was not added")
	}
	if !mgr.HasModel("custom-test-model") {
		t.Fatal("custom model was not preserved")
	}
	got := mgr.GetAvailableModels()
	want := []string{"new-built", "custom-test-model"}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestPreferredToolModelsAreRegistered(t *testing.T) {
	known := map[string]bool{}
	for _, m := range All() {
		known[m.ID] = true
	}
	for _, id := range claudetool.PreferredToolModels {
		if !known[id] {
			t.Errorf("PreferredToolModels contains %q which is not registered in models.All()", id)
		}
	}
}

func TestReasoningServiceMapping(t *testing.T) {
	inner := &mockChat{defaultReasoning: "medium"}
	svc := WrapReasoningConfig(inner, "", "unknown", "yes", `{"off":"off","minimal":"low","medium":"high"}`)

	levels := svc.Profile().ReasoningLevels
	if !reflect.DeepEqual(levels, []string{"off", "minimal", "medium"}) {
		t.Fatalf("levels = %v", levels)
	}
	if _, err := svc.Invoke(context.Background(), damodel.Request{Reasoning: &damodel.Reasoning{Effort: "minimal"}}); err != nil {
		t.Fatal(err)
	}
	if inner.gotReasoning != "low" {
		t.Fatalf("mapped level = %s, want low", inner.gotReasoning)
	}
}

func TestReasoningServiceDisabled(t *testing.T) {
	inner := &mockChat{}
	svc := WrapReasoningConfig(inner, "", "unknown", "no", "")
	if svc.Profile().SupportsReasoning {
		t.Fatal("disabled service reports reasoning support")
	}
	if _, err := svc.Invoke(context.Background(), damodel.Request{Reasoning: &damodel.Reasoning{Effort: "high"}}); err != nil {
		t.Fatal(err)
	}
	if inner.gotReasoning != "" {
		t.Fatalf("reasoning effort = %q, want omitted", inner.gotReasoning)
	}
}

func TestReasoningServiceRejectsUnsupportedLevel(t *testing.T) {
	svc := WrapReasoningConfig(&mockChat{}, "", "unknown", "yes", `{"low":"low"}`)
	_, err := svc.Invoke(context.Background(), damodel.Request{Reasoning: &damodel.Reasoning{Effort: "high"}})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error = %v, want unsupported-level error", err)
	}
}

func TestReasoningServiceMapsServiceDefault(t *testing.T) {
	inner := &mockChat{defaultReasoning: "medium"}
	svc := WrapReasoningConfig(inner, "", "unknown", "yes", `{"medium":"low"}`)
	if got := svc.Profile().DefaultReasoningLevel; got != "low" {
		t.Fatalf("default = %q, want low", got)
	}
	if _, err := svc.Invoke(context.Background(), damodel.Request{}); err != nil {
		t.Fatal(err)
	}
	if inner.gotReasoning != "low" {
		t.Fatalf("level = %s, want low", inner.gotReasoning)
	}
}
