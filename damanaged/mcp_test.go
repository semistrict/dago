package damanaged

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMCPRegistryCRUDAndToolDiscovery(t *testing.T) {
	var calls []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		switch {
		case request.URL.Path == mcpServersPath && request.Method == http.MethodGet:
			return response(http.StatusOK, `{"servers":[{"id":"s1","name":"Fleet","url":"https://tools.example"}]}`), nil
		case request.URL.Path == mcpServersPath && request.Method == http.MethodPost:
			body, _ := io.ReadAll(request.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			if payload["name"] != "Fleet" || payload["auth_type"] != "headers" || payload["headers"] == nil {
				t.Fatalf("payload = %#v", payload)
			}
			return response(http.StatusCreated, `{"id":"s1","name":"Fleet","url":"https://tools.example"}`), nil
		case request.URL.Path == mcpServersPath+"/s1" && request.Method == http.MethodGet:
			return response(http.StatusOK, `{"id":"s1","name":"Fleet","url":"https://tools.example","oauth_provider_id":"p1"}`), nil
		case request.URL.Path == mcpServersPath+"/s1" && request.Method == http.MethodPatch:
			return response(http.StatusOK, `{"id":"s1","name":"Fleet","url":"https://new.example"}`), nil
		case request.URL.Path == mcpServersPath+"/s1" && request.Method == http.MethodDelete:
			return response(http.StatusNoContent, ""), nil
		case request.URL.Path == "/v1/deepagents/mcp/tools":
			if request.URL.Query().Get("url") != "https://tools.example" || request.URL.Query().Get("oauth_provider_id") != "p1" {
				t.Fatalf("query = %v", request.URL.Query())
			}
			return response(http.StatusOK, `{"tools":[{"name":"read_url","description":"Read a URL"}]}`), nil
		default:
			t.Fatalf("request = %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
	client := New(&http.Client{Transport: transport}, "https://api.example.test", "secret", Options{})
	servers, err := client.ListMCPServers(context.Background())
	if err != nil || len(servers) != 1 {
		t.Fatalf("servers=%#v err=%v", servers, err)
	}
	if _, err := client.CreateMCPServer(context.Background(), "Fleet", "https://tools.example", MCPServerOptions{
		Headers: []MCPHeader{{Key: "X-Api-Key", Value: "secret"}},
	}); err != nil {
		t.Fatal(err)
	}
	server, err := client.GetMCPServer(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	newURL := "https://new.example"
	if _, err := client.UpdateMCPServer(context.Background(), "s1", MCPServerPatch{URL: &newURL}); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListMCPServerTools(context.Background(), server["url"].(string), server["oauth_provider_id"].(string))
	if err != nil || len(tools) != 1 || tools[0]["name"] != "read_url" {
		t.Fatalf("tools=%#v err=%v", tools, err)
	}
	if err := client.DeleteMCPServer(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 6 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestMCPRegistryRejectsSecretsInOAuthAndUnsafeInputs(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("transport must not be called")
		return nil, nil
	})}, "https://api.example.test", "secret", Options{})
	if _, err := client.CreateMCPServer(context.Background(), "OAuth", "https://tools.example", MCPServerOptions{
		AuthType: "oauth", Headers: []MCPHeader{{Key: "Authorization", Value: "secret"}},
	}); err == nil {
		t.Fatal("expected OAuth header rejection")
	}
	for _, rawURL := range []string{"http://tools.example", "https://user@tools.example", "https://tools.example/#fragment"} {
		if _, err := client.CreateMCPServer(context.Background(), "bad", rawURL, MCPServerOptions{}); err == nil {
			t.Fatalf("URL %q accepted", rawURL)
		}
	}
	if _, err := client.CreateMCPServer(context.Background(), "bad", "https://tools.example", MCPServerOptions{
		Headers: []MCPHeader{{Key: "X-Test\r\nInjected", Value: "x"}},
	}); err == nil {
		t.Fatal("expected header rejection")
	}
	if _, err := client.UpdateMCPServer(context.Background(), "s1", MCPServerPatch{}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPRegistryZeroOptionsLeaveAuthenticationUnspecified(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if _, exists := payload["auth_type"]; exists {
			t.Fatalf("zero options payload = %#v", payload)
		}
		return response(http.StatusCreated, `{"id":"s1","name":"Public","url":"https://tools.example"}`), nil
	})}, "https://api.example.test", "secret", Options{})
	if _, err := client.CreateMCPServer(context.Background(), "Public", "https://tools.example", MCPServerOptions{}); err != nil {
		t.Fatal(err)
	}
}
