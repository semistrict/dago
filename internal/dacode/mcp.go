package dacode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

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

func (transport headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for _, header := range transport.headers {
		clone.Header.Set(header.Name, header.Value)
	}
	return transport.base.RoundTrip(clone)
}

func mcpHTTPClient(headers []acp.HttpHeader) *http.Client {
	return &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: append([]acp.HttpHeader(nil), headers...)}}
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
	schema, err := json.Marshal(remote.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("encode MCP tool %q schema: %w", remote.Name, err)
	}
	if string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	description := strings.TrimSpace(remote.Description)
	if description == "" {
		description = fmt.Sprintf("Tool provided by the %s MCP server.", serverName)
	}
	spec := datool.Definition{Name: remote.Name, Description: description, InputSchema: schema}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("adapt MCP tool %q: %w", remote.Name, err)
	}
	return datool.Func{Spec: spec, Run: func(ctx context.Context, arguments json.RawMessage, _ datool.Runtime) (datool.Result, error) {
		var decoded any
		if err := json.Unmarshal(arguments, &decoded); err != nil {
			return datool.Result{}, err
		}
		response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: remote.Name, Arguments: decoded})
		if err != nil {
			return datool.Result{}, err
		}
		result := datool.Result{}
		for _, content := range response.Content {
			switch item := content.(type) {
			case *mcp.TextContent:
				result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: item.Text})
			case *mcp.ImageContent:
				result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockImage, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType})
			case *mcp.AudioContent:
				result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockAudio, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType})
			case *mcp.ResourceLink:
				result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: fmt.Sprintf("[%s](%s)", item.Name, item.URI)})
			default:
				encoded, marshalErr := content.MarshalJSON()
				if marshalErr != nil {
					return datool.Result{}, marshalErr
				}
				result.Content = append(result.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: string(encoded)})
			}
		}
		if response.StructuredContent != nil {
			result.Structured, err = json.Marshal(response.StructuredContent)
			if err != nil {
				return datool.Result{}, err
			}
		}
		if response.IsError {
			result.Status = damessage.ToolStatusError
		}
		return result, nil
	}}, nil
}
