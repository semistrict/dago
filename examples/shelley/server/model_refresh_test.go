package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"shelley.exe.dev/loop"
	"shelley.exe.dev/models"
)

func TestHandleModelRefreshReturnsRefreshedModels(t *testing.T) {
	mgr, err := models.NewManager(&models.Config{
		Models: []models.Built{
			{
				ID:       "old-built",
				Provider: models.ProviderBuiltIn,
				Source:   "old source",
				Chat:     loop.NewPredictableService(),
			},
		},
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	s := &Server{
		llmManager: mgr,
		logger:     slog.Default(),
		refreshBuiltModels: func(context.Context) ([]models.Built, error) {
			return []models.Built{
				{
					ID:       "new-built",
					Provider: models.ProviderBuiltIn,
					Source:   "new source",
					Chat:     loop.NewPredictableService(),
				},
				{
					ID:       models.Default().ID,
					Provider: models.ProviderOpenAI,
					Source:   "new source",
					Chat:     loop.NewPredictableService(),
				},
			}, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/models/refresh", nil)
	rec := httptest.NewRecorder()
	s.handleModelRefresh(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got []ModelInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 || got[0].ID != "new-built" || got[0].Source != "new source" {
		t.Fatalf("models = %+v, want new-built first", got)
	}
	if !got[0].IsDefault || got[1].IsDefault {
		t.Fatalf("models = %+v, want first refreshed model marked default", got)
	}
	if mgr.HasModel("old-built") {
		t.Fatal("old built model was not removed")
	}
}

func TestHandleModelsAssignsTiers(t *testing.T) {
	mgr, err := models.NewManager(&models.Config{
		Models: []models.Built{
			{ID: "gpt-5.6-sol", Provider: models.ProviderOpenAI, Chat: loop.NewPredictableService()},
			{ID: "gpt-5.5", Provider: models.ProviderOpenAI, Chat: loop.NewPredictableService()},
		},
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	s := &Server{llmManager: mgr, logger: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	s.handleModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	var got []ModelInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	tiers := map[string]int{}
	for _, m := range got {
		tiers[m.ID] = m.Tier
	}
	if tiers["gpt-5.6-sol"] != models.Tier1 {
		t.Errorf("sol tier = %d, want %d", tiers["gpt-5.6-sol"], models.Tier1)
	}
	if tiers["gpt-5.5"] != models.Tier2 {
		t.Errorf("gpt-5.5 tier = %d, want %d", tiers["gpt-5.5"], models.Tier2)
	}
}

func TestAssignModelTiersKeepsCustomModelsProminent(t *testing.T) {
	modelList := []ModelInfo{
		{ID: "gpt-5.6-sol", Source: "llm.int.exe.xyz", Ready: true},
		{ID: "upstream-only", Source: "llm.int.exe.xyz", Ready: true},
		{ID: "my-custom-model", Source: models.SourceCustomLabel, Ready: true},
	}

	assignModelTiers(modelList)

	want := map[string]int{
		"gpt-5.6-sol":     models.Tier1,
		"upstream-only":   models.Tier2,
		"my-custom-model": models.Tier1,
	}
	for _, model := range modelList {
		if model.Tier != want[model.ID] {
			t.Errorf("%s tier = %d, want %d", model.ID, model.Tier, want[model.ID])
		}
	}
}
