package damanaged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const mcpServersPath = "/v1/deepagents/mcp-servers"

// MCPServer is one provider-owned MCP registry record.
type MCPServer map[string]any

// MCPHeader is one explicit secret-bearing server header.
type MCPHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MCPServerOptions controls optional authentication for CreateMCPServer. Its
// zero value leaves authentication unspecified for the service default.
type MCPServerOptions struct {
	Headers   []MCPHeader
	AuthType  string
	OAuthMode string
}

// MCPServerPatch controls fields changed by UpdateMCPServer. Nil fields are
// preserved; a non-nil empty header slice clears headers.
type MCPServerPatch struct {
	URL      *string
	Headers  *[]MCPHeader
	AuthType *string
}

// ListMCPServers returns the bounded workspace registry.
func (client *Client) ListMCPServers(ctx context.Context) ([]MCPServer, error) {
	client.requireInitialized()
	body, err := client.request(ctx, http.MethodGet, mcpServersPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var list []MCPServer
	if err := json.Unmarshal(body, &list); err == nil {
		if len(list) > maxProjectEntries {
			return nil, errors.New("MCP server registry exceeds item limit")
		}
		return list, nil
	}
	var envelope struct {
		Servers []MCPServer `json:"servers"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Servers == nil || len(envelope.Servers) > maxProjectEntries {
		return nil, errors.New("MCP server registry response is invalid")
	}
	return envelope.Servers, nil
}

// GetMCPServer fetches one exact server ID.
func (client *Client) GetMCPServer(ctx context.Context, serverID string) (MCPServer, error) {
	if err := validateAgentID(serverID); err != nil {
		return nil, fmt.Errorf("MCP server ID: %w", err)
	}
	body, err := client.request(ctx, http.MethodGet, mcpServersPath+"/"+url.PathEscape(serverID), nil, nil)
	if err != nil {
		return nil, err
	}
	return decodeMCPServer(body)
}

// CreateMCPServer registers a required name and HTTPS URL.
func (client *Client) CreateMCPServer(ctx context.Context, name, rawURL string, options MCPServerOptions) (MCPServer, error) {
	if err := validateMCPName(name); err != nil {
		return nil, err
	}
	normalized, err := validateMCPURL(rawURL)
	if err != nil {
		return nil, err
	}
	authType := options.AuthType
	if authType == "" && len(options.Headers) > 0 {
		authType = "headers"
	}
	if authType != "" && authType != "headers" && authType != "oauth" {
		return nil, errors.New("MCP auth type must be headers or oauth")
	}
	if err := validateMCPHeaders(options.Headers); err != nil {
		return nil, err
	}
	if authType == "oauth" && len(options.Headers) != 0 {
		return nil, errors.New("OAuth MCP servers cannot include static headers")
	}
	payload := map[string]any{"name": strings.TrimSpace(name), "url": normalized}
	if authType != "" {
		payload["auth_type"] = authType
	}
	if len(options.Headers) > 0 {
		payload["headers"] = options.Headers
	}
	if options.OAuthMode != "" {
		if authType != "oauth" || options.OAuthMode != "per_user_dynamic_client" {
			return nil, errors.New("MCP OAuth mode is invalid")
		}
		payload["oauth_mode"] = options.OAuthMode
	}
	return client.writeMCPServer(ctx, http.MethodPost, mcpServersPath, payload)
}

// UpdateMCPServer applies a non-empty bounded patch to one exact server ID.
func (client *Client) UpdateMCPServer(ctx context.Context, serverID string, patch MCPServerPatch) (MCPServer, error) {
	if err := validateAgentID(serverID); err != nil {
		return nil, fmt.Errorf("MCP server ID: %w", err)
	}
	payload := map[string]any{}
	if patch.URL != nil {
		normalized, err := validateMCPURL(*patch.URL)
		if err != nil {
			return nil, err
		}
		payload["url"] = normalized
	}
	if patch.Headers != nil {
		if err := validateMCPHeaders(*patch.Headers); err != nil {
			return nil, err
		}
		payload["headers"] = *patch.Headers
	}
	if patch.AuthType != nil {
		if *patch.AuthType != "headers" {
			return nil, errors.New("MCP update auth type must be headers")
		}
		payload["auth_type"] = *patch.AuthType
	}
	if len(payload) == 0 {
		return nil, errors.New("MCP server update is empty")
	}
	return client.writeMCPServer(ctx, http.MethodPatch, mcpServersPath+"/"+url.PathEscape(serverID), payload)
}

// DeleteMCPServer permanently removes one registry entry.
func (client *Client) DeleteMCPServer(ctx context.Context, serverID string) error {
	if err := validateAgentID(serverID); err != nil {
		return fmt.Errorf("MCP server ID: %w", err)
	}
	_, err := client.request(ctx, http.MethodDelete, mcpServersPath+"/"+url.PathEscape(serverID), nil, nil)
	return err
}

// ListMCPServerTools discovers a registered HTTPS server's bounded tools.
func (client *Client) ListMCPServerTools(ctx context.Context, rawURL, oauthProviderID string) ([]map[string]any, error) {
	normalized, err := validateMCPURL(rawURL)
	if err != nil {
		return nil, err
	}
	query := url.Values{"url": {normalized}}
	if oauthProviderID != "" {
		if err := validateAgentID(oauthProviderID); err != nil {
			return nil, errors.New("MCP OAuth provider ID is invalid")
		}
		query.Set("oauth_provider_id", oauthProviderID)
	}
	body, err := client.request(ctx, http.MethodGet, "/v1/deepagents/mcp/tools", query, nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Tools == nil || len(envelope.Tools) > maxProjectEntries {
		return nil, errors.New("MCP tool response is invalid")
	}
	for index, tool := range envelope.Tools {
		name, ok := tool["name"].(string)
		if !ok || validateMCPName(name) != nil {
			return nil, fmt.Errorf("MCP tool %d has an invalid name", index)
		}
	}
	return envelope.Tools, nil
}

func (client *Client) writeMCPServer(ctx context.Context, method, path string, payload map[string]any) (MCPServer, error) {
	client.requireInitialized()
	encoded, err := client.marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	body, err := client.request(ctx, method, path, nil, encoded)
	if err != nil {
		return nil, err
	}
	return decodeMCPServer(body)
}

func decodeMCPServer(body []byte) (MCPServer, error) {
	var server MCPServer
	if err := json.Unmarshal(body, &server); err != nil || server == nil {
		return nil, errors.New("MCP server response is not an object")
	}
	return server, nil
}

func validateMCPName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return errors.New("MCP server or tool name is invalid")
	}
	return nil
}

func validateMCPURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("MCP server URL must be HTTPS without credentials or fragment")
	}
	return parsed.String(), nil
}

func validateMCPHeaders(headers []MCPHeader) error {
	if len(headers) > 64 {
		return errors.New("MCP server has too many headers")
	}
	seen := map[string]struct{}{}
	for _, header := range headers {
		key := strings.TrimSpace(header.Key)
		canonical := http.CanonicalHeaderKey(key)
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(header.Value) || len(key) > 256 || len(header.Value) > 16<<10 {
			return errors.New("MCP server header is invalid")
		}
		if _, exists := seen[canonical]; exists {
			return errors.New("MCP server header is duplicated")
		}
		seen[canonical] = struct{}{}
	}
	return nil
}
