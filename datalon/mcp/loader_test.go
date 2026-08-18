package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/datool"
)

func TestClientConnectsFiltersAndExecutesRemoteTools(t *testing.T) {
	t.Parallel()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet", Description: "Greets", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, _ *sdkmcp.CallToolRequest, input map[string]any) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hello " + input["name"].(string)}}}, nil, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "hidden", Description: "Hidden", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdkmcp.CallToolRequest, map[string]any) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{}, nil, nil
	})
	httpServer := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{JSONResponse: true}))
	defer httpServer.Close()
	client := NewClient(httpServer.Client(), nil, Options{})
	bundle, err := client.Connect(t.Context(), Config{Servers: map[string]Server{"remote": {URL: httpServer.URL, AllowedTools: []string{"gre*"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	if len(bundle.Servers) != 1 || bundle.Servers[0].Error != "" || len(bundle.Tools) != 1 || bundle.Tools[0].Definition().Name != "greet" {
		t.Fatalf("bundle = %#v", bundle)
	}
	result, err := bundle.Tools[0].Execute(t.Context(), json.RawMessage(`{"name":"world"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello world" {
		t.Fatalf("result = %#v", result)
	}
}

func TestClientContainsServerFailuresAndBoundsResults(t *testing.T) {
	t.Parallel()
	client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })}, nil, Options{MaxToolResultBytes: 4})
	bundle, err := client.Connect(t.Context(), Config{Servers: map[string]Server{"remote": {URL: "https://example.test/mcp"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Servers) != 1 || !strings.Contains(bundle.Servers[0].Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("servers = %#v", bundle.Servers)
	}
	_, err = boundedResult(&sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "12345"}}}, 4)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteURLRequiresHTTPSOrLiteralLoopback(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"http://example.test/mcp", "ftp://example.test/mcp", "https://user@example.test/mcp", "https://example.test/mcp#secret"} {
		if _, err := validateRemoteURL(raw); err == nil {
			t.Errorf("validateRemoteURL(%q) succeeded", raw)
		}
	}
	for _, raw := range []string{"https://example.test/mcp", "http://127.0.0.1:8080/mcp", "http://[::1]/mcp", "http://localhost/mcp"} {
		if _, err := validateRemoteURL(raw); err != nil {
			t.Errorf("validateRemoteURL(%q) = %v", raw, err)
		}
	}
}

func TestResponseLimitClosesOverlargeProtocolBody(t *testing.T) {
	t.Parallel()
	transport := &responseLimitRoundTripper{base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("12345")), Header: make(http.Header), Request: request}, nil
	}), limit: 4}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("read error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
