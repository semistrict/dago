package server

import (
	"context"
	"strings"
	"testing"
)

func TestModelSetupHintOnlyAppearsForEmptyCatalog(t *testing.T) {
	if got := modelSetupHintForModels(context.Background(), nil); got != modelSetupHintLocal {
		t.Fatalf("empty catalog hint = %q", got)
	}
	if got := modelSetupHintForModels(context.Background(), []ModelInfo{{ID: "predictable", Ready: true}}); got != "" {
		t.Fatalf("non-empty catalog hint = %q", got)
	}
}

func TestUnsupportedModelMessage(t *testing.T) {
	if got := unsupportedModelMessage("missing", nil); !strings.Contains(got, "No AI models") {
		t.Fatalf("empty catalog message = %q", got)
	}
	if got := unsupportedModelMessage("missing", []ModelInfo{{ID: "predictable", Ready: true}}); got != "Unsupported model: missing" {
		t.Fatalf("non-empty catalog message = %q", got)
	}
}
