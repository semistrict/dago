package damanaged

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestManagedOAuthProviderAndSessionFlow(t *testing.T) {
	var calls []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		body, _ := io.ReadAll(request.Body)
		switch request.URL.Path {
		case mcpServersPath + "/s1/oauth-provider":
			return response(http.StatusOK, `{"oauth_provider_id":"p1"}`), nil
		case "/v1/deepagents/auth-sessions":
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if payload["provider_id"] != "p1" || payload["strategy"] != "CREATE" {
				t.Fatalf("payload = %#v", payload)
			}
			return response(http.StatusCreated, `{"id":"session-1","status":"PENDING","verification_url":"https://auth.example"}`), nil
		case "/v1/deepagents/auth-sessions/session-1":
			if request.URL.Query().Get("wait_seconds") != "5" {
				t.Fatalf("query = %v", request.URL.Query())
			}
			return response(http.StatusOK, `{"id":"session-1","status":"COMPLETED"}`), nil
		default:
			t.Fatalf("request = %s", request.URL)
			return nil, nil
		}
	})
	client := New(&http.Client{Transport: transport}, "https://api.example.test", "secret", Options{})
	provider, err := client.RegisterMCPProvider(context.Background(), "s1")
	if err != nil || provider["oauth_provider_id"] != "p1" {
		t.Fatalf("provider=%#v err=%v", provider, err)
	}
	if _, err := client.CreateAuthSession(context.Background(), "p1", []string{"read", "write"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetAuthSession(context.Background(), "session-1", 5); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestManagedOAuthRejectsInvalidScopesAndWaitsBeforeNetwork(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("transport must not be called")
		return nil, nil
	})}, "https://api.example.test", "secret", Options{})
	if _, err := client.CreateAuthSession(context.Background(), "p1", []string{"read", "read"}, false); err == nil {
		t.Fatal("expected duplicate scope rejection")
	}
	if _, err := client.GetAuthSession(context.Background(), "s1", 31); err == nil {
		t.Fatal("expected wait rejection")
	}
}

func TestCreateAuthSessionEncodesDefaultScopesAsArray(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		scopes, ok := payload["scopes"].([]any)
		if !ok || len(scopes) != 0 {
			t.Fatalf("scopes = %#v, want empty array", payload["scopes"])
		}
		return response(http.StatusCreated, `{"id":"session-1","status":"PENDING"}`), nil
	})}, "https://api.example.test", "secret", Options{})
	if _, err := client.CreateAuthSession(context.Background(), "provider-1", nil, false); err != nil {
		t.Fatal(err)
	}
}
