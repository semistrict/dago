package oauthpolicy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/semistrict/dago/datalon/mcp"
)

// Login connects one OAuth-configured MCP server using the provider policy
// selected from its exact endpoint hostname. Tokens are written only through
// store; configuration files are never modified.
func Login(
	ctx context.Context,
	httpClient *http.Client,
	serverName string,
	server mcp.Server,
	store mcp.TokenStore,
	interaction mcp.Interaction,
	options Options,
) error {
	if ctx == nil || httpClient == nil || isNil(store) || isNil(interaction) {
		panic("OAuth login context, HTTP client, token store, and interaction are required")
	}
	if server.Auth != "oauth" {
		return fmt.Errorf("server %q is not configured for OAuth", serverName)
	}
	service := New(httpClient, store, interaction, serverName, server, options)
	if service.Provider() == ProviderGitHub {
		return service.Authorize(ctx, nil, nil)
	}
	client := mcp.NewClient(httpClient, NewFactory(httpClient, store, interaction, options), mcp.Options{})
	bundle, err := client.Connect(ctx, mcp.Config{Servers: map[string]mcp.Server{serverName: server}})
	if err != nil {
		return err
	}
	defer bundle.Close()
	if len(bundle.Servers) != 1 || bundle.Servers[0].Error != "" {
		if len(bundle.Servers) == 1 {
			return fmt.Errorf("MCP OAuth login failed: %s", bundle.Servers[0].Error)
		}
		return fmt.Errorf("MCP OAuth login failed")
	}
	return nil
}
