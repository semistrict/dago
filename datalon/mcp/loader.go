package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

const (
	defaultMaxToolResult = 4 << 20
	maxToolsPerServer    = 512
	maxToolSchema        = 256 << 10
)

// OAuthFactory supplies authentication for servers configured with
// "auth": "oauth". It is called only for those servers.
type OAuthFactory func(serverName string, server Server) (sdkauth.OAuthHandler, error)

// Options controls optional limits. Its zero value is useful and bounded.
type Options struct {
	MaxToolResultBytes int
	Environment        map[string]string
}

// Client connects configured MCP servers. Construction performs no I/O.
type Client struct {
	httpClient *http.Client
	oauth      OAuthFactory
	options    Options
}

// NewClient creates a loader. The caller-owned HTTP client is required so its
// network and proxy policy remains explicit. OAuth may be nil when unused.
func NewClient(httpClient *http.Client, oauth OAuthFactory, options Options) *Client {
	if httpClient == nil {
		panic("MCP HTTP client is required")
	}
	if options.MaxToolResultBytes < 0 {
		panic("MCP tool result limit cannot be negative")
	}
	if options.MaxToolResultBytes == 0 {
		options.MaxToolResultBytes = defaultMaxToolResult
	}
	if options.MaxToolResultBytes > 64<<20 {
		panic("MCP tool result limit exceeds 64 MiB")
	}
	return &Client{httpClient: httpClient, oauth: oauth, options: options}
}

// ServerInfo records one server's transport and discovered tools. Error is
// empty after a successful connection.
type ServerInfo struct {
	Name      string
	Transport string
	Tools     []string
	Error     string
}

// Bundle owns connected sessions and their adapted tools.
type Bundle struct {
	Tools    []datool.Tool
	Servers  []ServerInfo
	sessions []*sdkmcp.ClientSession
}

// Close releases all connected sessions in reverse order.
func (bundle *Bundle) Close() error {
	if bundle == nil {
		return nil
	}
	var first error
	for index := len(bundle.sessions) - 1; index >= 0; index-- {
		if err := bundle.sessions[index].Close(); err != nil && first == nil {
			first = err
		}
	}
	bundle.sessions = nil
	return first
}

// Connect validates config, connects each server, and exposes successfully
// loaded tools. Individual connection failures are recorded in Servers so one
// unavailable optional server does not disable the rest.
func (client *Client) Connect(ctx context.Context, config Config) (*Bundle, error) {
	if client == nil || client.httpClient == nil {
		panic("initialized MCP client is required")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	bundle := &Bundle{}
	names := make([]string, 0, len(config.Servers))
	for name := range config.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	toolOwners := map[string]string{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			_ = bundle.Close()
			return nil, err
		}
		server := config.Servers[name]
		info := ServerInfo{Name: name, Transport: normalizedType(server)}
		transport, err := client.transport(name, server)
		if err != nil {
			info.Error = publicError(err, server.URL)
			bundle.Servers = append(bundle.Servers, info)
			continue
		}
		session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "datalon", Version: "1"}, nil).Connect(ctx, transport, nil)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = bundle.Close()
				return nil, ctxErr
			}
			info.Error = publicError(err, server.URL)
			bundle.Servers = append(bundle.Servers, info)
			continue
		}
		serverTools := make([]datool.Tool, 0)
		for remote, listErr := range session.Tools(ctx, nil) {
			if listErr != nil {
				err = fmt.Errorf("list tools: %w", listErr)
				break
			}
			if len(serverTools) >= maxToolsPerServer {
				err = fmt.Errorf("server exceeds %d tools", maxToolsPerServer)
				break
			}
			if !toolEnabled(remote.Name, server) {
				continue
			}
			if owner, exists := toolOwners[remote.Name]; exists {
				err = fmt.Errorf("tool %q is also declared by server %q", remote.Name, owner)
				break
			}
			tool, adaptErr := client.adaptTool(session, name, remote)
			if adaptErr != nil {
				err = adaptErr
				break
			}
			toolOwners[remote.Name] = name
			info.Tools = append(info.Tools, remote.Name)
			serverTools = append(serverTools, tool)
		}
		if err != nil {
			for _, toolName := range info.Tools {
				delete(toolOwners, toolName)
			}
			_ = session.Close()
			info.Tools = nil
			info.Error = publicError(err, server.URL)
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
	return bundle, nil
}

// LoadDiscovered resolves the configured path, loads it, and connects its
// servers. The returned path is empty when no config exists, in which case an
// empty bundle is returned.
func (client *Client) LoadDiscovered(ctx context.Context, environment map[string]string, home string) (*Bundle, string, error) {
	configPath, err := Resolve(environment, home)
	if err != nil {
		return nil, "", err
	}
	if configPath == "" {
		return &Bundle{}, "", nil
	}
	config, err := LoadConfig(configPath)
	if err != nil {
		return nil, configPath, err
	}
	bundle, err := client.Connect(ctx, config)
	return bundle, configPath, err
}

func (client *Client) transport(name string, server Server) (sdkmcp.Transport, error) {
	switch normalizedType(server) {
	case "http":
		if _, err := validateRemoteURL(server.URL); err != nil {
			return nil, err
		}
		handler, err := client.oauthHandler(name, server)
		if err != nil {
			return nil, err
		}
		return &sdkmcp.StreamableClientTransport{
			Endpoint: server.URL, HTTPClient: client.serverHTTPClient(server.Headers),
			OAuthHandler: handler, DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil
	case "sse":
		if _, err := validateRemoteURL(server.URL); err != nil {
			return nil, err
		}
		handler, err := client.oauthHandler(name, server)
		if err != nil {
			return nil, err
		}
		httpClient := client.serverHTTPClient(server.Headers)
		if handler != nil {
			httpClient.Transport = &oauthRoundTripper{base: httpClient.Transport, handler: handler}
		}
		return &sdkmcp.SSEClientTransport{Endpoint: server.URL, HTTPClient: httpClient}, nil
	case "stdio":
		command := exec.Command(server.Command, append([]string(nil), server.Args...)...)
		command.Env = mergedEnvironment(client.options.Environment, server.Env)
		return &sdkmcp.CommandTransport{Command: command}, nil
	default:
		return nil, fmt.Errorf("unsupported transport")
	}
}

func (client *Client) oauthHandler(name string, server Server) (sdkauth.OAuthHandler, error) {
	if server.Auth != "oauth" {
		return nil, nil
	}
	if client.oauth == nil {
		return nil, fmt.Errorf("OAuth login is required for server %q", name)
	}
	handler, err := client.oauth(name, server)
	if err != nil {
		return nil, fmt.Errorf("prepare OAuth: %w", err)
	}
	if nilOAuthDependency(handler) {
		return nil, fmt.Errorf("OAuth provider returned no handler")
	}
	return handler, nil
}

func (client *Client) serverHTTPClient(headers map[string]string) *http.Client {
	copy := *client.httpClient
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &headerRoundTripper{base: base, headers: cloneMap(headers)}
	responseLimit := int64(client.options.MaxToolResultBytes)*2 + 1<<20
	if responseLimit > 128<<20 {
		responseLimit = 128 << 20
	}
	copy.Transport = &responseLimitRoundTripper{base: copy.Transport, limit: responseLimit}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

type responseLimitRoundTripper struct {
	base  http.RoundTripper
	limit int64
}

func (transport *responseLimitRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = http.MaxBytesReader(nil, response.Body, transport.limit)
	return response, nil
}

func (transport *headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for name, value := range transport.headers {
		clone.Header.Set(name, value)
	}
	return transport.base.RoundTrip(clone)
}

func (client *Client) adaptTool(session *sdkmcp.ClientSession, serverName string, remote *sdkmcp.Tool) (datool.Tool, error) {
	schema, err := json.Marshal(remote.InputSchema)
	if err != nil || len(schema) > maxToolSchema {
		return nil, fmt.Errorf("tool %q has an invalid or oversized schema", remote.Name)
	}
	if string(schema) == "null" {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	description := strings.TrimSpace(remote.Description)
	if description == "" {
		description = fmt.Sprintf("Tool provided by the %s MCP server.", serverName)
	}
	if len(description) > 64<<10 {
		return nil, fmt.Errorf("tool %q description is too large", remote.Name)
	}
	extra := map[string]json.RawMessage{}
	if remote.Annotations != nil {
		if encoded, encodeErr := json.Marshal(remote.Annotations); encodeErr == nil {
			extra["mcp_annotations"] = encoded
		}
	}
	definition := datool.Definition{Name: remote.Name, Description: description, InputSchema: schema, Extra: extra}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("adapt tool %q: %w", remote.Name, err)
	}
	limit := client.options.MaxToolResultBytes
	return datool.Func{Spec: definition, Run: func(ctx context.Context, arguments json.RawMessage, _ datool.Runtime) (datool.Result, error) {
		var decoded any
		if err := json.Unmarshal(arguments, &decoded); err != nil {
			return datool.Result{}, err
		}
		response, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: remote.Name, Arguments: decoded})
		if err != nil {
			return datool.Result{}, boundedWrappedError("MCP tool call failed", err)
		}
		return boundedResult(response, limit)
	}}, nil
}

func boundedResult(response *sdkmcp.CallToolResult, limit int) (datool.Result, error) {
	result := datool.Result{}
	used := 0
	appendBlock := func(block damessage.ContentBlock, size int) error {
		if size < 0 || used > limit-size {
			return fmt.Errorf("MCP tool result exceeds %d bytes", limit)
		}
		used += size
		result.Content = append(result.Content, block)
		return nil
	}
	for _, content := range response.Content {
		switch item := content.(type) {
		case *sdkmcp.TextContent:
			if err := appendBlock(damessage.ContentBlock{Type: damessage.BlockText, Text: item.Text}, len(item.Text)); err != nil {
				return datool.Result{}, err
			}
		case *sdkmcp.ImageContent:
			if err := appendBlock(damessage.ContentBlock{Type: damessage.BlockImage, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType}, len(item.Data)); err != nil {
				return datool.Result{}, err
			}
		case *sdkmcp.AudioContent:
			if err := appendBlock(damessage.ContentBlock{Type: damessage.BlockAudio, Data: append([]byte(nil), item.Data...), MIMEType: item.MIMEType}, len(item.Data)); err != nil {
				return datool.Result{}, err
			}
		case *sdkmcp.ResourceLink:
			text := fmt.Sprintf("[%s](%s)", item.Name, item.URI)
			if err := appendBlock(damessage.ContentBlock{Type: damessage.BlockText, Text: text}, len(text)); err != nil {
				return datool.Result{}, err
			}
		default:
			encoded, err := content.MarshalJSON()
			if err != nil {
				return datool.Result{}, err
			}
			if err := appendBlock(damessage.ContentBlock{Type: damessage.BlockText, Text: string(encoded)}, len(encoded)); err != nil {
				return datool.Result{}, err
			}
		}
	}
	if response.StructuredContent != nil {
		encoded, err := json.Marshal(response.StructuredContent)
		if err != nil {
			return datool.Result{}, err
		}
		if len(encoded) > limit-used {
			return datool.Result{}, fmt.Errorf("MCP tool result exceeds %d bytes", limit)
		}
		result.Structured = encoded
	}
	if response.IsError {
		result.Status = damessage.ToolStatusError
	}
	return result, nil
}

func normalizedType(server Server) string {
	value := strings.ToLower(strings.TrimSpace(server.Type))
	if value == "" {
		if server.Command != "" {
			return "stdio"
		}
		return "http"
	}
	if value == "streamable_http" {
		return "http"
	}
	return value
}

func toolEnabled(name string, server Server) bool {
	if len(server.AllowedTools) > 0 {
		return matchesAny(name, server.AllowedTools)
	}
	return !matchesAny(name, server.DisabledTools)
}

func matchesAny(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == name {
			return true
		}
		if matched, err := path.Match(pattern, name); err == nil && matched {
			return true
		}
	}
	return false
}

func pathMatch(pattern, name string) (bool, error) { return path.Match(pattern, name) }

func validateRemoteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("remote MCP URL is invalid")
	}
	if parsed.Scheme == "https" {
		return parsed, nil
	}
	if parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()) {
		return parsed, nil
	}
	return nil, fmt.Errorf("remote MCP URL must use HTTPS or loopback HTTP")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func mergedEnvironment(base, overrides map[string]string) []string {
	values := map[string]string{}
	if base == nil {
		for _, item := range os.Environ() {
			if name, value, ok := strings.Cut(item, "="); ok {
				values[name] = value
			}
		}
	} else {
		for name, value := range base {
			values[name] = value
		}
	}
	for name, value := range overrides {
		values[name] = value
	}
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}

func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}

func publicError(err error, endpoint string) string {
	message := err.Error()
	if endpoint != "" {
		message = strings.ReplaceAll(message, endpoint, "<MCP endpoint>")
	}
	return boundedErrorText(message)
}

type publicWrappedError struct {
	message string
	cause   error
}

func (err publicWrappedError) Error() string { return err.message }
func (err publicWrappedError) Unwrap() error { return err.cause }

func boundedWrappedError(prefix string, cause error) error {
	if cause == nil {
		return nil
	}
	return publicWrappedError{message: prefix + ": " + boundedErrorText(cause.Error()), cause: cause}
}

func boundedErrorText(message string) string {
	message = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\x00' {
			return ' '
		}
		return character
	}, message)
	const limit = 4096
	if len(message) <= limit {
		return message
	}
	message = message[:limit]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message + "…"
}

var _ io.Closer = (*Bundle)(nil)
