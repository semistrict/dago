package damanaged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RegisterMCPProvider creates or resolves the caller's per-user OAuth provider
// for one registered MCP server.
func (client *Client) RegisterMCPProvider(ctx context.Context, serverID string) (map[string]any, error) {
	if err := validateAgentID(serverID); err != nil {
		return nil, errors.New("MCP server ID is invalid")
	}
	return client.oauthObject(ctx, http.MethodPost, mcpServersPath+"/"+url.PathEscape(serverID)+"/oauth-provider", map[string]any{})
}

// CreateAuthSession begins a managed OAuth session for a required provider ID.
func (client *Client) CreateAuthSession(ctx context.Context, providerID string, scopes []string, fresh bool) (map[string]any, error) {
	if err := validateAgentID(providerID); err != nil {
		return nil, errors.New("OAuth provider ID is invalid")
	}
	if len(scopes) > 64 {
		return nil, errors.New("OAuth scope count exceeds limit")
	}
	seen := map[string]struct{}{}
	for _, scope := range scopes {
		if scope == "" || scope != strings.TrimSpace(scope) || len(scope) > 256 || strings.ContainsAny(scope, "\x00\r\n") {
			return nil, errors.New("OAuth scope is invalid")
		}
		if _, exists := seen[scope]; exists {
			return nil, errors.New("OAuth scope is duplicated")
		}
		seen[scope] = struct{}{}
	}
	strategy := "REUSE"
	if fresh {
		strategy = "CREATE"
	}
	if scopes == nil {
		scopes = []string{}
	}
	return client.oauthObject(ctx, http.MethodPost, "/v1/deepagents/auth-sessions", map[string]any{
		"provider_id": providerID, "scopes": scopes, "strategy": strategy,
	})
}

// GetAuthSession fetches or briefly long-polls one exact session.
func (client *Client) GetAuthSession(ctx context.Context, sessionID string, waitSeconds int) (map[string]any, error) {
	if err := validateAgentID(sessionID); err != nil || waitSeconds < 0 || waitSeconds > 30 {
		return nil, errors.New("OAuth session request is invalid")
	}
	body, err := client.request(ctx, http.MethodGet, "/v1/deepagents/auth-sessions/"+url.PathEscape(sessionID), url.Values{"wait_seconds": {fmt.Sprint(waitSeconds)}}, nil)
	if err != nil {
		return nil, err
	}
	return decodeOAuthObject(body)
}

func (client *Client) oauthObject(ctx context.Context, method, path string, payload map[string]any) (map[string]any, error) {
	client.requireInitialized()
	encoded, err := client.marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	body, err := client.request(ctx, method, path, nil, encoded)
	if err != nil {
		return nil, err
	}
	return decodeOAuthObject(body)
}

func decodeOAuthObject(body []byte) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil || result == nil {
		return nil, errors.New("OAuth response is not an object")
	}
	return result, nil
}
