package dacode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/datool"
)

func TestConnectMCPServersDiscoversAndInvokesHTTPTools(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type input struct {
		Value string `json:"value"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "from_t3", Description: "Return a value."},
		func(_ context.Context, _ *mcp.CallToolRequest, arguments input) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "received:" + arguments.Value}}}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "rich_failure", Description: "Return rich output."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.ImageContent{Data: []byte("pixels"), MIMEType: "image/png"}},
				StructuredContent: map[string]any{"retryable": true}, IsError: true,
			}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		if request.Header.Get("Authorization") != "Bearer session-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	tools, closer, err := connectMCPServers(t.Context(), []acp.McpServer{{Http: &acp.McpServerHttpInline{
		Name: "t3", Url: httpServer.URL, Headers: []acp.HttpHeader{{Name: "Authorization", Value: "Bearer session-token"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if len(tools) != 2 || tools[0].Definition().Name != "from_t3" {
		t.Fatalf("tools = %#v", tools)
	}
	result, err := tools[0].Execute(t.Context(), json.RawMessage(`{"value":"ok"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "received:ok" {
		t.Fatalf("result = %#v", result)
	}
	rich, err := tools[1].Execute(t.Context(), json.RawMessage(`{}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if rich.Status != "error" || len(rich.Content) != 1 || rich.Content[0].Type != "image" || string(rich.Content[0].Data) != "pixels" || string(rich.Structured) != `{"retryable":true}` {
		t.Fatalf("rich result = %#v", rich)
	}
}

func TestConnectMCPServersRejectsDuplicateToolNames(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "same", Description: "Same."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, string, error) {
			return nil, "ok", nil
		})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true},
	))
	defer httpServer.Close()
	_, _, err := connectMCPServers(t.Context(), []acp.McpServer{
		{Http: &acp.McpServerHttpInline{Name: "one", Url: httpServer.URL, Headers: []acp.HttpHeader{}}},
		{Http: &acp.McpServerHttpInline{Name: "two", Url: httpServer.URL, Headers: []acp.HttpHeader{}}},
	})
	if err == nil {
		t.Fatal("duplicate tool was accepted")
	}
}

func TestMCPTransportVariants(t *testing.T) {
	name, transport, err := mcpTransport(acp.McpServer{Sse: &acp.McpServerSseInline{
		Name: "events", Url: "http://127.0.0.1/events", Headers: []acp.HttpHeader{},
	}})
	if err != nil || name != "events" {
		t.Fatalf("SSE transport = %q, %T, %v", name, transport, err)
	}
	if _, ok := transport.(*mcp.SSEClientTransport); !ok {
		t.Fatalf("SSE transport type = %T", transport)
	}

	name, transport, err = mcpTransport(acp.McpServer{Stdio: &acp.McpServerStdio{
		Name: "local", Command: "tool-server", Args: []string{"--stdio"},
		Env: []acp.EnvVariable{{Name: "T3_SESSION", Value: "present"}},
	}})
	if err != nil || name != "local" {
		t.Fatalf("stdio transport = %q, %T, %v", name, transport, err)
	}
	command, ok := transport.(*mcp.CommandTransport)
	if !ok || command.Command.Path != "tool-server" || len(command.Command.Args) != 2 || !environmentContains(command.Command.Env, "T3_SESSION=present") {
		t.Fatalf("stdio transport = %#v", transport)
	}

	_, _, err = mcpTransport(acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "routed", Id: "one"}})
	if err == nil {
		t.Fatal("ACP-routed MCP transport was accepted")
	}
}

func environmentContains(environment []string, expected string) bool {
	for _, item := range environment {
		if item == expected {
			return true
		}
	}
	return false
}
