package server

import (
	"testing"

	"github.com/semistrict/dago/examples/shelley/claudetool"
)

func TestNewServerRejectsMissingRequiredDependencies(t *testing.T) {
	if server, err := NewServer(nil, &testLLMManager{}, claudetool.ToolSetConfig{}, nil, false, "", ""); err == nil || server != nil {
		t.Fatalf("missing database = (%v, %v), want nil and error", server, err)
	}
	var manager *testLLMManager
	database, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)
	if server, err := NewServer(database, manager, claudetool.ToolSetConfig{}, nil, false, "", ""); err == nil || server != nil {
		t.Fatalf("typed-nil model provider = (%v, %v), want nil and error", server, err)
	}
}
