package oauthpolicy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/semistrict/dago/datalon/mcp"
)

func TestLoginRejectsNonOAuthServerWithoutIO(t *testing.T) {
	t.Parallel()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, context.Canceled
	})}
	err := Login(t.Context(), client, "server", mcp.Server{
		Type: "http", URL: "https://example.com/mcp",
	}, &memoryStore{}, &interaction{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "not configured for OAuth") {
		t.Fatalf("Login error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("Login performed %d network calls for an invalid server", calls)
	}
}

func TestLoginRequiresStaticDependencies(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Login accepted a nil context")
		}
	}()
	_ = Login(nil, &http.Client{}, "server", mcp.Server{
		Type: "http", URL: "https://example.com/mcp", Auth: "oauth",
	}, &memoryStore{}, &interaction{}, Options{})
}
