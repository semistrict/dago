package modelsources

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/examples/shelley/models"
)

type mapChat map[string]string

func (mapChat) Invoke(context.Context, damodel.Request) (damodel.Response, error) {
	return damodel.Response{}, nil
}

func (mapChat) Stream(context.Context, damodel.Request) (damodel.Stream, error) {
	return damodel.EmptyStream{}, nil
}

func (mapChat) Profile() damodel.Profile { return damodel.Profile{} }

func TestBuildUsesLocalCredentialsAndPredictableModel(t *testing.T) {
	built, err := Build(models.All(), []Source{Env("key"), Predictable()}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, model := range built {
		seen[model.ID] = model.Source
	}
	if seen["gpt-5.6-sol"] != "$OPENAI_API_KEY" {
		t.Fatalf("OpenAI source = %q", seen["gpt-5.6-sol"])
	}
	if seen["predictable"] != "builtin" {
		t.Fatalf("predictable source = %q", seen["predictable"])
	}
}

func TestBuildRejectsInvalidCatalogWithoutPartialResult(t *testing.T) {
	catalog := []models.Model{
		{ID: "first", Provider: models.ProviderBuiltIn, APIType: models.APITypeBuiltIn, Build: func(string, string, *http.Client) (damodel.Chat, error) {
			return modeltest.NewPredictable(modeltest.PredictableOptions{}), nil
		}},
		{ID: "broken", Provider: models.ProviderBuiltIn, APIType: models.APITypeBuiltIn, Build: func(string, string, *http.Client) (damodel.Chat, error) {
			return nil, errors.New("broken model")
		}},
	}
	built, err := Build(catalog, []Source{Predictable()}, nil, nil)
	if err == nil || built != nil {
		t.Fatalf("Build = (%v, %v), want nil result and error", built, err)
	}
}

func TestBuildRejectsTypedNilBuilderResult(t *testing.T) {
	catalog := []models.Model{{
		ID: "nil", Provider: models.ProviderBuiltIn, APIType: models.APITypeBuiltIn,
		Build: func(string, string, *http.Client) (damodel.Chat, error) {
			var chat *modeltest.Predictable
			return chat, nil
		},
	}}
	if built, err := Build(catalog, []Source{Predictable()}, nil, nil); err == nil || built != nil {
		t.Fatalf("Build = (%v, %v), want nil result and error", built, err)
	}
}

func TestBuildRejectsNilMapBuilderResult(t *testing.T) {
	catalog := []models.Model{{
		ID: "nil-map", Provider: models.ProviderBuiltIn, APIType: models.APITypeBuiltIn,
		Build: func(string, string, *http.Client) (damodel.Chat, error) {
			var chat mapChat
			return chat, nil
		},
	}}
	if built, err := Build(catalog, []Source{Predictable()}, nil, nil); err == nil || built != nil {
		t.Fatalf("Build = (%v, %v), want nil result and error", built, err)
	}
}

func TestBuildValidatesCatalogWithoutConfiguredSources(t *testing.T) {
	model := models.Model{
		ID: "duplicate", Provider: models.ProviderBuiltIn, APIType: models.APITypeBuiltIn,
		Build: func(string, string, *http.Client) (damodel.Chat, error) {
			return modeltest.NewPredictable(modeltest.PredictableOptions{}), nil
		},
	}
	if built, err := Build([]models.Model{model, model}, nil, nil, nil); err == nil || built != nil {
		t.Fatalf("duplicate catalog Build = (%v, %v), want nil and error", built, err)
	}
	model.APIType = models.APIType("unknown")
	if built, err := Build([]models.Model{model}, nil, nil, nil); err == nil || built != nil {
		t.Fatalf("invalid API type Build = (%v, %v), want nil and error", built, err)
	}
}

func TestBuildFirstSourceWins(t *testing.T) {
	built, err := Build(models.All(), []Source{Env("first"), Env("second")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, model := range built {
		counts[model.ID]++
	}
	for id, count := range counts {
		if count != 1 {
			t.Fatalf("model %s appeared %d times", id, count)
		}
	}
}
