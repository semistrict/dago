package dacode

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/semistrict/dago/damcp"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
)

type mcpRuntimeLogin func(context.Context, *http.Client, string, talonmcp.Server, talonmcp.TokenStore, talonmcp.Interaction, oauthpolicy.Options) error

type mcpRuntimeController interface {
	SnapshotMCP() ([]mcpViewerServer, bool, error)
	LoginMCP(context.Context, string, talonmcp.Interaction) error
	ToggleMCPDisabled(context.Context, string) error
	ReconnectMCP(context.Context) error
}

func (runner *reloadableRunner) SnapshotMCP() ([]mcpViewerServer, bool, error) {
	runner.mu.RLock()
	defer runner.mu.RUnlock()
	if runner.closed || runner.mcpBundle == nil {
		return nil, false, errors.New("MCP runtime is unavailable")
	}
	return mcpViewerServersFromRuntime(runner.mcpResolution, runner.mcpBundle, runner.mcpDisabled), runner.mcpPending, nil
}

func (runner *reloadableRunner) ToggleMCPDisabled(ctx context.Context, serverName string) error {
	if ctx == nil {
		panic("dacode: MCP toggle context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runner.reloadMu.Lock()
	defer runner.reloadMu.Unlock()
	serverName = strings.TrimSpace(serverName)
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.closed || runner.mcpBundle == nil {
		return errors.New("MCP runtime is unavailable")
	}
	configured := false
	for _, connection := range runner.mcpResolution.runtimeConnections() {
		if connection.Name == serverName {
			configured = true
			break
		}
	}
	if !configured {
		return errors.New("MCP server cannot be changed in this session")
	}
	if runner.mcpDisabled == nil {
		runner.mcpDisabled = make(map[string]bool)
	}
	if runner.mcpDisabled[serverName] {
		delete(runner.mcpDisabled, serverName)
	} else {
		runner.mcpDisabled[serverName] = true
	}
	runner.mcpPending = true
	return nil
}

func (runner *reloadableRunner) ReconnectMCP(ctx context.Context) error {
	_, err := runner.ReloadPlugins(ctx)
	return err
}

func (runner *reloadableRunner) LoginMCP(ctx context.Context, serverName string, interaction talonmcp.Interaction) error {
	if ctx == nil || interaction == nil {
		panic("dacode: MCP login dependencies are required")
	}
	runner.mu.RLock()
	resolution := runner.mcpResolution
	tokenDirectory := runner.mcpTokenDir
	login := runner.mcpLogin
	closed := runner.closed
	runner.mu.RUnlock()
	if closed || tokenDirectory == "" || login == nil {
		return errors.New("MCP login is unavailable")
	}
	connection, err := selectMCPLoginConnection(resolution, serverName)
	if err != nil {
		return err
	}
	server := talonmcp.Server{
		Type: connection.Transport, URL: connection.URL, Auth: connection.Auth,
		Headers: cloneStringMap(connection.Headers), AllowedTools: append([]string(nil), connection.AllowedTools...),
		DisabledTools: append([]string(nil), connection.DisabledTools...),
	}
	store := talonmcp.NewFileTokenStore(tokenDirectory)
	if err := login(ctx, mcpHTTPClient(nil), connection.Name, server, store, interaction, oauthpolicy.Options{}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("MCP OAuth login failed")
	}
	return nil
}

func cloneMCPDisabled(values map[string]bool) map[string]bool {
	result := make(map[string]bool, len(values))
	for name, disabled := range values {
		if disabled {
			result[name] = true
		}
	}
	return result
}

func filterMCPRuntimeConnections(resolution mcpConfigResolution, disabled map[string]bool) []damcp.Connection {
	connections := resolution.runtimeConnections()
	result := make([]damcp.Connection, 0, len(connections))
	for _, connection := range connections {
		if !disabled[connection.Name] {
			result = append(result, connection)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}
