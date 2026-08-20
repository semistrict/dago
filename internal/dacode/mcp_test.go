package dacode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damcp"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
	"golang.org/x/oauth2"
)

type mcpRoundTripFunc func(*http.Request) (*http.Response, error)

func (function mcpRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type testOAuthHandler struct {
	token      string
	authorized int
}

func (handler *testOAuthHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: handler.token}), nil
}

func (handler *testOAuthHandler) Authorize(_ context.Context, _ *http.Request, response *http.Response) error {
	handler.authorized++
	handler.token = "new-token"
	return response.Body.Close()
}

func TestConnectMCPServersDiscoversAndInvokesHTTPTools(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type input struct {
		Value string `json:"value"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "from_t3", Description: "Return a value.", Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: new(false),
	}},
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
	if mcpToolRequiresApproval(tools[0]) || !mcpToolRequiresApproval(tools[1]) {
		t.Fatalf("MCP annotation policy was not preserved: %#v", tools)
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

func TestConnectConfiguredMCPServersPrefixesFiltersAndPreservesSafety(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "configured", Version: "1"}, nil)
	type input struct {
		Value string `json:"value"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "read_data", Description: "Read data.", Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: new(false),
	}}, func(_ context.Context, _ *mcp.CallToolRequest, arguments input) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arguments.Value}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "write_data", Description: "Write data."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		if request.Header.Get("X-Token") != "configured-token" {
			t.Errorf("X-Token = %q", request.Header.Get("X-Token"))
		}
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	bundle, err := connectConfiguredMCPServers(t.Context(), []damcp.Connection{
		{Name: "alpha", Transport: "http", URL: httpServer.URL, Headers: map[string]string{"X-Token": "configured-token"}, AllowedTools: []string{"read_*"}},
		{Name: "beta", Transport: "http", URL: httpServer.URL, Headers: map[string]string{"X-Token": "configured-token"}, DisabledTools: []string{"write_*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	if len(bundle.Tools) != 2 || bundle.Tools[0].Definition().Name != "alpha_read_data" || bundle.Tools[1].Definition().Name != "beta_read_data" {
		t.Fatalf("tools = %#v", bundle.Tools)
	}
	if len(bundle.Servers) != 2 || len(bundle.Servers[0].Tools) != 1 || bundle.Servers[0].Error != "" {
		t.Fatalf("servers = %#v", bundle.Servers)
	}
	for _, tool := range bundle.Tools {
		if mcpToolRequiresApproval(tool) {
			t.Fatalf("read-only tool lost protocol safety metadata: %s", tool.Definition().Name)
		}
	}
	result, err := bundle.Tools[0].Execute(t.Context(), json.RawMessage(`{"value":"ok"}`), datool.Runtime{})
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != "ok" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestConfiguredMCPTransportAttachesNonInteractiveProviderOAuth(t *testing.T) {
	t.Parallel()
	tokenDirectory := t.TempDir()
	for _, transportName := range []string{"http", "sse"} {
		transport, err := configuredMCPTransportWithOAuth(damcp.Connection{
			Name: "secure", Transport: transportName, URL: "https://tools.example/mcp", Auth: "oauth",
		}, tokenDirectory)
		if err != nil {
			t.Fatalf("%s transport: %v", transportName, err)
		}
		switch typed := transport.(type) {
		case *mcp.StreamableClientTransport:
			if typed.OAuthHandler == nil {
				t.Fatal("streamable HTTP transport has no OAuth handler")
			}
		case *mcp.SSEClientTransport:
			if _, ok := typed.HTTPClient.Transport.(*configuredMCPOAuthTransport); !ok {
				t.Fatalf("SSE transport HTTP policy = %T", typed.HTTPClient.Transport)
			}
		default:
			t.Fatalf("transport = %T", transport)
		}
	}
	if _, err := configuredMCPTransport(damcp.Connection{
		Name: "secure", Transport: "http", URL: "https://tools.example/mcp", Auth: "oauth",
	}); err == nil || !strings.Contains(err.Error(), "token directory") {
		t.Fatalf("missing token directory error = %v", err)
	}
}

func TestConfiguredMCPOAuthTransportRetriesWithRotatedToken(t *testing.T) {
	t.Parallel()
	handler := &testOAuthHandler{token: "old-token"}
	calls := 0
	transport := &configuredMCPOAuthTransport{
		handler: handler,
		base: mcpRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			want := "Bearer old-token"
			status := http.StatusUnauthorized
			if calls == 2 {
				want, status = "Bearer new-token", http.StatusOK
			}
			if value := request.Header.Get("Authorization"); value != want {
				t.Errorf("call %d Authorization = %q, want %q", calls, value, want)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
		}),
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://tools.example/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls != 2 || handler.authorized != 1 {
		t.Fatalf("status=%d calls=%d authorized=%d", response.StatusCode, calls, handler.authorized)
	}
}

func TestMCPToolMetadataAndResultsAreBounded(t *testing.T) {
	if _, err := adaptMCPTool(nil, "server", &mcp.Tool{
		Name: "large_schema", Description: "Large.", InputSchema: map[string]any{"description": strings.Repeat("x", maxMCPToolSchemaBytes)},
	}); err == nil {
		t.Fatal("oversized MCP schema was accepted")
	}
	if _, err := adaptMCPTool(nil, "server", &mcp.Tool{
		Name: "large_description", Description: strings.Repeat("x", 64<<10+1), InputSchema: map[string]any{"type": "object"},
	}); err == nil {
		t.Fatal("oversized MCP description was accepted")
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "bounded", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "large_result", Description: "Large result."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", maxMCPToolResultBytes+1)}}}, nil, nil
		})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true},
	))
	defer httpServer.Close()
	bundle, err := connectConfiguredMCPServers(t.Context(), []damcp.Connection{{Name: "bounded", Transport: "http", URL: httpServer.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	if len(bundle.Tools) != 1 {
		t.Fatalf("tools = %#v", bundle.Tools)
	}
	if _, err := bundle.Tools[0].Execute(t.Context(), json.RawMessage(`{}`), datool.Runtime{}); err == nil || !strings.Contains(err.Error(), "result exceeds its bound") {
		t.Fatalf("oversized result error = %v", err)
	}
}

func TestMCPReadOnlyClassificationUsesCoherentProtocolAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations *mcp.ToolAnnotations
		wantReview  bool
	}{
		{name: "missing annotations", wantReview: true},
		{name: "default annotations", annotations: &mcp.ToolAnnotations{}, wantReview: true},
		{name: "read only", annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}, wantReview: false},
		{name: "explicitly nondestructive read only", annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(false)}, wantReview: false},
		{name: "contradictory hints", annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(true)}, wantReview: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := &mcp.Tool{
				Name: "opaque", Description: "Opaque action.", InputSchema: map[string]any{"type": "object"},
				Annotations: test.annotations,
			}
			tool, err := adaptMCPTool(nil, "remote", remote)
			if err != nil {
				t.Fatal(err)
			}
			if got := mcpToolRequiresApproval(tool); got != test.wantReview {
				t.Fatalf("requires approval = %t, want %t", got, test.wantReview)
			}
			if remote.Annotations != nil {
				remote.Annotations.ReadOnlyHint = !remote.Annotations.ReadOnlyHint
				if got := mcpToolRequiresApproval(tool); got != test.wantReview {
					t.Fatalf("classification changed after remote metadata mutation: %t", got)
				}
			}
		})
	}
}

func TestHeadlessMCPPolicyFailsClosedWithoutNameHeuristics(t *testing.T) {
	var unsafeExecutions atomic.Int32
	var safeExecutions atomic.Int32
	var localExecutions atomic.Int32
	tool := func(name string, executions *atomic.Int32) datool.Tool {
		return datool.Func{Spec: datool.Definition{
			Name: name, Description: "Action.", InputSchema: json.RawMessage(`{"type":"object"}`),
		}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
			executions.Add(1)
			return datool.TextResult("executed"), nil
		}}
	}
	unsafe := adaptedMCPTool{Tool: tool("opaque_remote_action", &unsafeExecutions), safety: mcpToolSafety{}}
	safe := adaptedMCPTool{Tool: tool("delete_everything", &safeExecutions), safety: mcpToolSafety{annotated: true, readOnly: true}}
	local := tool("mcp_write_all_records", &localExecutions)

	guarded, rules := applyHeadlessMCPPolicy([]datool.Tool{unsafe, safe, local}, false)
	if len(rules) != 0 {
		t.Fatalf("unexpected approval rules: %#v", rules)
	}
	for index, candidate := range guarded {
		result, err := candidate.Execute(t.Context(), json.RawMessage(`{}`), datool.Runtime{})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if result.Status != damessage.ToolStatusError || !strings.Contains(result.Content[0].Text, "requires approval") {
				t.Fatalf("guard result = %#v", result)
			}
		} else if result.Status == damessage.ToolStatusError {
			t.Fatalf("tool %d was rejected: %#v", index, result)
		}
	}
	if unsafeExecutions.Load() != 0 || safeExecutions.Load() != 1 || localExecutions.Load() != 1 {
		t.Fatalf("executions: unsafe=%d safe=%d local=%d", unsafeExecutions.Load(), safeExecutions.Load(), localExecutions.Load())
	}
}

func TestHeadlessMCPPolicyExecutesWriteOnlyAfterApproval(t *testing.T) {
	var executions atomic.Int32
	implementation := datool.Func{Spec: datool.Definition{
		Name: "sync[*]?/records", Description: "Synchronize records.", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, Run: func(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
		executions.Add(1)
		return datool.TextResult("synchronized"), nil
	}}
	tools, rules := applyHeadlessMCPPolicy([]datool.Tool{
		adaptedMCPTool{Tool: implementation, safety: mcpToolSafety{annotated: true, readOnly: false}},
	}, true)
	if len(rules) != 1 {
		t.Fatalf("approval rules = %#v", rules)
	}
	matched, err := rules[0].Applies(dagent.ToolCallRequest{
		Call: damessage.ToolCall{Name: "sync[*]?/records"}, Tool: tools[0],
	})
	if err != nil || !matched {
		t.Fatalf("exact adversarial name match = %t, %v", matched, err)
	}
	matched, err = rules[0].Applies(dagent.ToolCallRequest{
		Call: damessage.ToolCall{Name: "syncXx/records"}, Tool: tools[0],
	})
	if err != nil || matched {
		t.Fatalf("lookalike name match = %t, %v", matched, err)
	}

	model := modeltest.New(damodel.Profile{ToolCalling: true},
		modeltest.Step{Response: damodel.Response{Message: damessage.Message{
			Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
				ID: "call-1", Name: "sync[*]?/records", Arguments: json.RawMessage(`{}`),
			}},
		}}},
		modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("done")}},
	)
	agent := dagent.New(model, dagent.Options{
		Tools: tools, Middleware: []dagent.Middleware{dagent.HumanApproval(rules)}, Saver: dacheckpoint.NewMemorySaver(),
	})
	config := dacheckpoint.Config{ThreadID: "headless-mcp-approval"}
	paused, err := agent.Invoke(t.Context(), dagent.FromCheckpoint(config), dagent.Prompt("sync"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paused.Interrupts) != 1 || executions.Load() != 0 {
		t.Fatalf("interrupts = %#v, executions = %d", paused.Interrupts, executions.Load())
	}
	resumed, err := agent.Invoke(t.Context(), dagent.FromCheckpoint(config), dagent.Resume(dagent.ApprovalResponse{Decisions: map[string]dagent.ApprovalChoice{
		"call-1": {Decision: dagent.ApprovalApprove},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Interrupts) != 0 || executions.Load() != 1 {
		t.Fatalf("interrupts = %#v, executions = %d", resumed.Interrupts, executions.Load())
	}
}

func TestCLIHeadlessDefaultsToApprovalAndRejectsPolicyBypass(t *testing.T) {
	options, err := parseCLI([]string{"--non-interactive", "sync records"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !options.headless || !options.autoApprove || options.yolo {
		t.Fatalf("headless defaults = %#v", options)
	}

	err = Run(t.Context(), []string{"--non-interactive", "sync records", "--yolo"}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--yolo cannot be used with --non-interactive") {
		t.Fatalf("headless policy bypass error = %v", err)
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
