package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damcp"
	"github.com/semistrict/dago/damessage"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
	"github.com/semistrict/dago/datool"
)

const headlessMCPRejection = "This MCP action requires approval, but the current headless runtime has no approval policy. Run it interactively or configure automatic approval."

const (
	maxMCPToolSchemaBytes = 256 << 10
	maxMCPToolResultBytes = 4 << 20
	maxMCPResponseBytes   = 16 << 20
)

type mcpToolSafety struct {
	annotated   bool
	readOnly    bool
	destructive bool
}

func (safety mcpToolSafety) coherentlyReadOnly() bool {
	return safety.annotated && safety.readOnly && !safety.destructive
}

// mcpToolMetadata is deliberately private: only tools produced by MCP discovery
// can claim this provenance inside dacode's policy boundary.
type mcpToolMetadata interface {
	mcpSafety() mcpToolSafety
}

type adaptedMCPTool struct {
	datool.Tool
	safety mcpToolSafety
}

func (tool adaptedMCPTool) mcpSafety() mcpToolSafety { return tool.safety }

type rejectedHeadlessMCPTool struct {
	datool.Tool
	safety mcpToolSafety
}

func (tool rejectedHeadlessMCPTool) mcpSafety() mcpToolSafety { return tool.safety }

func (tool rejectedHeadlessMCPTool) Execute(context.Context, json.RawMessage, datool.Runtime) (datool.Result, error) {
	result := datool.TextResult(headlessMCPRejection)
	result.Status = damessage.ToolStatusError
	return result, nil
}

func mcpSafety(tool datool.Tool) (mcpToolSafety, bool) {
	metadata, ok := tool.(mcpToolMetadata)
	if !ok {
		return mcpToolSafety{}, false
	}
	return metadata.mcpSafety(), true
}

func mcpToolRequiresApproval(tool datool.Tool) bool {
	safety, isMCP := mcpSafety(tool)
	return isMCP && !safety.coherentlyReadOnly()
}

// applyHeadlessMCPPolicy either routes write-capable MCP tools through the
// existing approval flow or replaces them with a fail-closed execution guard.
// Its zero values are useful: no tools and no approval policy remain safe.
func applyHeadlessMCPPolicy(tools []datool.Tool, approvalEnabled bool) ([]datool.Tool, []dagent.ApprovalRule) {
	result := append([]datool.Tool(nil), tools...)
	decisions := []dagent.ApprovalDecision{dagent.ApprovalApprove, dagent.ApprovalReject}
	var rules []dagent.ApprovalRule
	for index, tool := range result {
		if !mcpToolRequiresApproval(tool) {
			continue
		}
		if !approvalEnabled {
			safety, _ := mcpSafety(tool)
			result[index] = rejectedHeadlessMCPTool{Tool: tool, safety: safety}
			continue
		}
		name := tool.Definition().Name
		rules = append(rules, dagent.ApprovalRule{
			Pattern: exactApprovalPattern(name), Description: "Allow this MCP action?",
			AllowedDecisions: decisions,
			When: func(request dagent.ToolCallRequest) bool {
				return mcpToolRequiresApproval(request.Tool)
			},
		})
	}
	return result, rules
}

func exactApprovalPattern(name string) string {
	var pattern strings.Builder
	for _, character := range name {
		switch character {
		case '\\', '*', '?', '[':
			pattern.WriteByte('\\')
		}
		pattern.WriteRune(character)
	}
	return pattern.String()
}

type mcpConnections struct{ sessions []*mcp.ClientSession }

func (connections *mcpConnections) Close() error {
	var result error
	for index := len(connections.sessions) - 1; index >= 0; index-- {
		if err := connections.sessions[index].Close(); err != nil && result == nil {
			result = err
		}
	}
	return result
}

type configuredMCPServerInfo struct {
	Name, Transport string
	Tools           []string
	Error           string
}

type configuredMCPBundle struct {
	Tools   []datool.Tool
	Servers []configuredMCPServerInfo
	mcpConnections
}

func connectConfiguredMCPServers(ctx context.Context, configured []damcp.Connection) (*configuredMCPBundle, error) {
	return connectConfiguredMCPServersWithOAuth(ctx, configured, "")
}

func connectConfiguredMCPServersWithOAuth(ctx context.Context, configured []damcp.Connection, tokenDirectory string) (*configuredMCPBundle, error) {
	if ctx == nil {
		panic("dacode: MCP connection context is required")
	}
	bundle := &configuredMCPBundle{}
	toolOwners := map[string]string{}
	for _, connection := range configured {
		if err := ctx.Err(); err != nil {
			_ = bundle.Close()
			return nil, err
		}
		info := configuredMCPServerInfo{Name: connection.Name, Transport: connection.Transport}
		transport, err := configuredMCPTransportWithOAuth(connection, tokenDirectory)
		if err != nil {
			info.Error = "invalid server transport"
			bundle.Servers = append(bundle.Servers, info)
			continue
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "dacode", Version: buildVersion()}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = bundle.Close()
				return nil, ctxErr
			}
			info.Error = "server connection failed"
			if connection.Auth == "oauth" {
				info.Error = "OAuth login is required or stored credentials could not be used"
			}
			bundle.Servers = append(bundle.Servers, info)
			continue
		}
		var serverTools []datool.Tool
		for remote, listErr := range session.Tools(ctx, nil) {
			if listErr != nil {
				err = listErr
				break
			}
			if len(serverTools) >= 512 {
				err = fmt.Errorf("server tool limit exceeded")
				break
			}
			if !connection.MatchTool(remote.Name) {
				continue
			}
			publicName := connection.Name + "_" + remote.Name
			if previous, exists := toolOwners[publicName]; exists {
				err = fmt.Errorf("tool name collides with server %q", previous)
				break
			}
			tool, adaptErr := adaptMCPToolAs(session, connection.Name, publicName, remote)
			if adaptErr != nil {
				err = adaptErr
				break
			}
			toolOwners[publicName] = connection.Name
			info.Tools = append(info.Tools, publicName)
			serverTools = append(serverTools, tool)
		}
		if err != nil {
			for _, name := range info.Tools {
				delete(toolOwners, name)
			}
			_ = session.Close()
			info.Tools = nil
			info.Error = "server tool discovery failed"
			bundle.Servers = append(bundle.Servers, info)
			continue
		}
		sort.Strings(info.Tools)
		bundle.Tools = append(bundle.Tools, serverTools...)
		bundle.sessions = append(bundle.sessions, session)
		bundle.Servers = append(bundle.Servers, info)
	}
	sort.Slice(bundle.Tools, func(i, j int) bool {
		return bundle.Tools[i].Definition().Name < bundle.Tools[j].Definition().Name
	})
	sort.Slice(bundle.Servers, func(i, j int) bool { return bundle.Servers[i].Name < bundle.Servers[j].Name })
	return bundle, nil
}

func configuredMCPTransport(connection damcp.Connection) (mcp.Transport, error) {
	return configuredMCPTransportWithOAuth(connection, "")
}

func configuredMCPTransportWithOAuth(connection damcp.Connection, tokenDirectory string) (mcp.Transport, error) {
	var handler sdkauth.OAuthHandler
	if connection.Auth == "oauth" {
		if tokenDirectory == "" {
			return nil, errors.New("OAuth token directory is required")
		}
		server := talonmcp.Server{Type: connection.Transport, URL: connection.URL, Auth: connection.Auth}
		handler = oauthpolicy.New(
			mcpHTTPClient(nil), talonmcp.NewFileTokenStore(tokenDirectory), runtimeMCPInteraction{},
			connection.Name, server, oauthpolicy.Options{},
		)
	}
	switch connection.Transport {
	case "http":
		return &mcp.StreamableClientTransport{
			Endpoint: connection.URL, HTTPClient: configuredMCPHTTPClient(connection), OAuthHandler: handler, DisableStandaloneSSE: true,
		}, nil
	case "sse":
		client := configuredMCPHTTPClient(connection)
		if handler != nil {
			client.Transport = &configuredMCPOAuthTransport{base: client.Transport, handler: handler}
		}
		return &mcp.SSEClientTransport{Endpoint: connection.URL, HTTPClient: client}, nil
	case "stdio":
		command := exec.Command(connection.Command, connection.Args...)
		command.Dir = connection.CWD
		overrides := make([]acp.EnvVariable, 0, len(connection.Env))
		for name, value := range connection.Env {
			overrides = append(overrides, acp.EnvVariable{Name: name, Value: value})
		}
		command.Env = mergedEnvironment(overrides)
		return &mcp.CommandTransport{Command: command}, nil
	default:
		return nil, fmt.Errorf("unsupported MCP transport")
	}
}

type runtimeMCPInteraction struct{}

func (runtimeMCPInteraction) Authorize(context.Context, string) (string, error) {
	return "", errors.New("interactive MCP OAuth login is required")
}

func (runtimeMCPInteraction) SelectSlackWorkspace(context.Context) (string, error) {
	return "", errors.New("interactive MCP OAuth login is required")
}

func (runtimeMCPInteraction) PresentDeviceCode(context.Context, oauthpolicy.DeviceCode) error {
	return errors.New("interactive MCP OAuth login is required")
}

type configuredMCPOAuthTransport struct {
	base    http.RoundTripper
	handler sdkauth.OAuthHandler
}

func (transport *configuredMCPOAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.roundTripAuthorized(request)
	if err != nil || response == nil || (response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden) {
		return response, err
	}
	if err := transport.handler.Authorize(request.Context(), request, response); err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	if request.Body != nil {
		if request.GetBody == nil {
			return nil, errors.New("cannot retry MCP OAuth request body")
		}
		retry.Body, err = request.GetBody()
		if err != nil {
			return nil, err
		}
	}
	return transport.roundTripAuthorized(retry)
}

func (transport *configuredMCPOAuthTransport) roundTripAuthorized(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	source, err := transport.handler.TokenSource(request.Context())
	if err != nil {
		return nil, err
	}
	if source != nil {
		token, err := source.Token()
		if err != nil {
			return nil, err
		}
		token.SetAuthHeader(clone)
	}
	return transport.base.RoundTrip(clone)
}

var _ talonmcp.Interaction = runtimeMCPInteraction{}
var _ oauthpolicy.WorkspaceSelector = runtimeMCPInteraction{}
var _ oauthpolicy.DeviceCodePresenter = runtimeMCPInteraction{}

func configuredMCPHTTPClient(connection damcp.Connection) *http.Client {
	headers := make([]acp.HttpHeader, 0, len(connection.Headers))
	for name, value := range connection.Headers {
		headers = append(headers, acp.HttpHeader{Name: name, Value: value})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].Name < headers[j].Name })
	return mcpHTTPClient(headers)
}

func connectMCPServers(ctx context.Context, servers []acp.McpServer) ([]datool.Tool, io.Closer, error) {
	connections := &mcpConnections{}
	var tools []datool.Tool
	names := map[string]string{}
	for _, server := range servers {
		name, transport, err := mcpTransport(server)
		if err != nil {
			_ = connections.Close()
			return nil, nil, err
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "dacode", Version: buildVersion()}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			_ = connections.Close()
			return nil, nil, fmt.Errorf("connect MCP server %q: %w", name, err)
		}
		connections.sessions = append(connections.sessions, session)
		for remote, listErr := range session.Tools(ctx, nil) {
			if listErr != nil {
				_ = connections.Close()
				return nil, nil, fmt.Errorf("list tools from MCP server %q: %w", name, listErr)
			}
			if previous, exists := names[remote.Name]; exists {
				_ = connections.Close()
				return nil, nil, fmt.Errorf("MCP tool %q is declared by both %q and %q", remote.Name, previous, name)
			}
			names[remote.Name] = name
			tool, toolErr := adaptMCPTool(session, name, remote)
			if toolErr != nil {
				_ = connections.Close()
				return nil, nil, toolErr
			}
			tools = append(tools, tool)
		}
	}
	return tools, connections, nil
}

func mcpTransport(server acp.McpServer) (string, mcp.Transport, error) {
	switch {
	case server.Http != nil:
		return server.Http.Name, &mcp.StreamableClientTransport{
			Endpoint: server.Http.Url, HTTPClient: mcpHTTPClient(server.Http.Headers), DisableStandaloneSSE: true,
		}, nil
	case server.Sse != nil:
		return server.Sse.Name, &mcp.SSEClientTransport{
			Endpoint: server.Sse.Url, HTTPClient: mcpHTTPClient(server.Sse.Headers),
		}, nil
	case server.Stdio != nil:
		command := exec.Command(server.Stdio.Command, server.Stdio.Args...)
		command.Env = mergedEnvironment(server.Stdio.Env)
		return server.Stdio.Name, &mcp.CommandTransport{Command: command}, nil
	case server.Acp != nil:
		return server.Acp.Name, nil, fmt.Errorf("ACP-routed MCP server %q is not supported", server.Acp.Name)
	default:
		return "", nil, fmt.Errorf("MCP server has no transport")
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers []acp.HttpHeader
}

type mcpResponseLimitTransport struct{ base http.RoundTripper }

type limitedMCPResponseBody struct {
	io.Reader
	io.Closer
}

func (transport headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for _, header := range transport.headers {
		clone.Header.Set(header.Name, header.Value)
	}
	return transport.base.RoundTrip(clone)
}

func (transport mcpResponseLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = limitedMCPResponseBody{
		Reader: io.LimitReader(response.Body, maxMCPResponseBytes+1), Closer: response.Body,
	}
	return response, nil
}

func mcpHTTPClient(headers []acp.HttpHeader) *http.Client {
	return &http.Client{
		Transport: mcpResponseLimitTransport{base: headerTransport{base: http.DefaultTransport, headers: append([]acp.HttpHeader(nil), headers...)}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func mergedEnvironment(overrides []acp.EnvVariable) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	for _, entry := range overrides {
		values[entry.Name] = entry.Value
	}
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}

func adaptMCPTool(session *mcp.ClientSession, serverName string, remote *mcp.Tool) (datool.Tool, error) {
	return adaptMCPToolAs(session, serverName, remote.Name, remote)
}

func adaptMCPToolAs(session *mcp.ClientSession, serverName, publicName string, remote *mcp.Tool) (datool.Tool, error) {
	schema, err := json.Marshal(remote.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("encode MCP tool %q schema: %w", remote.Name, err)
	}
	if len(schema) > maxMCPToolSchemaBytes {
		return nil, fmt.Errorf("MCP tool %q schema exceeds its bound", remote.Name)
	}
	if string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	description := strings.TrimSpace(remote.Description)
	if description == "" {
		description = fmt.Sprintf("Tool provided by the %s MCP server.", serverName)
	}
	if len(publicName) > 512 || len(description) > 64<<10 {
		return nil, fmt.Errorf("MCP tool %q metadata exceeds its bound", remote.Name)
	}
	spec := datool.Definition{Name: publicName, Description: description, InputSchema: schema}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("adapt MCP tool %q: %w", remote.Name, err)
	}
	implementation := datool.Func{Spec: spec, Run: func(ctx context.Context, arguments json.RawMessage, _ datool.Runtime) (datool.Result, error) {
		var decoded any
		if err := json.Unmarshal(arguments, &decoded); err != nil {
			return datool.Result{}, err
		}
		response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: remote.Name, Arguments: decoded})
		if err != nil {
			return datool.Result{}, err
		}
		result := datool.Result{}
		resultBytes := 0
		appendBlock := func(block damessage.ContentBlock, size int) error {
			if size < 0 || resultBytes > maxMCPToolResultBytes-size {
				return fmt.Errorf("MCP tool %q result exceeds its bound", remote.Name)
			}
			resultBytes += size
			result.Content = append(result.Content, block)
			return nil
		}
		for _, content := range response.Content {
			switch item := content.(type) {
			case *mcp.TextContent:
				err = appendBlock(damessage.ContentBlock{Type: damessage.BlockText, Text: item.Text}, len(item.Text))
			case *mcp.ImageContent:
				err = appendBlock(damessage.ContentBlock{Type: damessage.BlockImage, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType}, len(item.Data)+len(item.MIMEType))
			case *mcp.AudioContent:
				err = appendBlock(damessage.ContentBlock{Type: damessage.BlockAudio, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType}, len(item.Data)+len(item.MIMEType))
			case *mcp.ResourceLink:
				text := fmt.Sprintf("[%s](%s)", item.Name, item.URI)
				err = appendBlock(damessage.ContentBlock{Type: damessage.BlockText, Text: text}, len(text))
			default:
				encoded, marshalErr := content.MarshalJSON()
				if marshalErr != nil {
					return datool.Result{}, marshalErr
				}
				err = appendBlock(damessage.ContentBlock{Type: damessage.BlockText, Text: string(encoded)}, len(encoded))
			}
			if err != nil {
				return datool.Result{}, err
			}
		}
		if response.StructuredContent != nil {
			result.Structured, err = json.Marshal(response.StructuredContent)
			if err != nil {
				return datool.Result{}, err
			}
			if len(result.Structured) > maxMCPToolResultBytes-resultBytes {
				return datool.Result{}, fmt.Errorf("MCP tool %q result exceeds its bound", remote.Name)
			}
		}
		if response.IsError {
			result.Status = damessage.ToolStatusError
		}
		return result, nil
	}}
	safety := mcpToolSafety{}
	if remote.Annotations != nil {
		safety.annotated = true
		safety.readOnly = remote.Annotations.ReadOnlyHint
		safety.destructive = remote.Annotations.DestructiveHint != nil && *remote.Annotations.DestructiveHint
	}
	return adaptedMCPTool{Tool: implementation, safety: safety}, nil
}
