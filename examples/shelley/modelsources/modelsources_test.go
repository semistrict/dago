package modelsources

import (
	"io"
	"log/slog"
	"testing"

	"github.com/semistrict/dago/examples/shelley/models"
)

func TestBuildUsesLocalCredentialsAndPredictableModel(t *testing.T) {
	built := Build(models.All(), []Source{Env("key"), Predictable()}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestBuildFirstSourceWins(t *testing.T) {
	built := Build(models.All(), []Source{Env("first"), Env("second")}, nil, nil)
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
