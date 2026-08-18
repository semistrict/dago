package mcp

import (
	"net/http"
	"testing"
)

func TestNewClientZeroDefaultAndNegativeLimit(t *testing.T) {
	client := NewClient(http.DefaultClient, nil, Options{})
	if client.options.MaxToolResultBytes != defaultMaxToolResult {
		t.Fatalf("maximum tool result bytes = %d", client.options.MaxToolResultBytes)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("negative static limit did not panic")
		}
	}()
	NewClient(http.DefaultClient, nil, Options{MaxToolResultBytes: -1})
}
