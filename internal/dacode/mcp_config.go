package dacode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/semistrict/dago/damcp"
)

type mcpConfigResolution struct {
	Connections []damcp.Connection
	OAuth       []damcp.Connection
	Sources     []damcp.ConfigSource
	Prompt      []damcp.Server
	Disabled    []damcp.Server
	Diagnostics []damcp.ConfigDiagnostic
}

func resolveMCPConfig(
	ctx context.Context,
	homeDirectory, projectRoot, explicitPath, policyPath string,
	trustProject bool,
	lookup damcp.LookupEnv,
) (mcpConfigResolution, error) {
	if ctx == nil {
		panic("dacode: MCP config context is required")
	}
	if lookup == nil {
		panic("dacode: MCP process environment lookup is required")
	}
	report, err := damcp.DiscoverConfigs(ctx, homeDirectory, projectRoot, damcp.ConfigOptions{ExplicitPath: explicitPath})
	if err != nil {
		return mcpConfigResolution{}, err
	}
	resolution := mcpConfigResolution{
		Sources:     append([]damcp.ConfigSource(nil), report.Sources...),
		Diagnostics: append([]damcp.ConfigDiagnostic(nil), report.Diagnostics...),
	}
	activated := make(map[string]damcp.ConfiguredServer, len(report.Servers))
	var project []damcp.Server
	projectDefinitions := map[string]damcp.ConfiguredServer{}
	for _, server := range report.Servers {
		if server.Scope != damcp.ProjectConfigScope {
			activated[server.Name] = server
			continue
		}
		definition := damcp.Server{Name: server.Name, Definition: append(json.RawMessage(nil), server.Definition...)}
		project = append(project, definition)
		projectDefinitions[server.Name] = server
	}
	if len(project) > 0 {
		store := damcp.NewStore(policyPath, lookup, damcp.Options{})
		policy, loadErr := store.Load(ctx)
		if loadErr != nil {
			return mcpConfigResolution{}, loadErr
		}
		if policy.LoadFailed() {
			resolution.Diagnostics = append(resolution.Diagnostics, damcp.ConfigDiagnostic{Path: policyPath, Reason: "MCP trust policy could not be fully loaded"})
		}
		trust, resolveErr := policy.Resolve(projectRoot, project, trustProject)
		if resolveErr != nil {
			return mcpConfigResolution{}, resolveErr
		}
		resolution.Prompt = cloneMCPTrustServers(trust.Prompt)
		resolution.Disabled = cloneMCPTrustServers(trust.Disabled)
		for _, allowed := range trust.Allowed {
			activated[allowed.Name] = projectDefinitions[allowed.Name]
		}
	}
	names := make([]string, 0, len(activated))
	for name := range activated {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := activated[name]
		connection, resolveErr := damcp.ResolveConnection(server, lookup)
		if resolveErr != nil {
			resolution.Diagnostics = append(resolution.Diagnostics, damcp.ConfigDiagnostic{
				Path: server.Source, Server: server.Name, Reason: "server definition is invalid or unresolved",
			})
			continue
		}
		if connection.Auth == "oauth" {
			resolution.OAuth = append(resolution.OAuth, connection)
			continue
		}
		resolution.Connections = append(resolution.Connections, connection)
	}
	sort.Slice(resolution.Diagnostics, func(i, j int) bool {
		if resolution.Diagnostics[i].Path == resolution.Diagnostics[j].Path {
			return resolution.Diagnostics[i].Server < resolution.Diagnostics[j].Server
		}
		return resolution.Diagnostics[i].Path < resolution.Diagnostics[j].Path
	})
	return resolution, nil
}

func (resolution mcpConfigResolution) runtimeConnections() []damcp.Connection {
	result := make([]damcp.Connection, 0, len(resolution.Connections)+len(resolution.OAuth))
	result = append(result, resolution.Connections...)
	result = append(result, resolution.OAuth...)
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func cloneMCPTrustServers(servers []damcp.Server) []damcp.Server {
	result := make([]damcp.Server, len(servers))
	for index, server := range servers {
		result[index] = damcp.Server{Name: server.Name, Definition: append(json.RawMessage(nil), server.Definition...)}
	}
	return result
}

func mcpConfigPromptError(prompt []damcp.Server) error {
	if len(prompt) == 0 {
		return nil
	}
	names := make([]string, len(prompt))
	for index, server := range prompt {
		names[index] = server.Name
	}
	sort.Strings(names)
	return fmt.Errorf("project MCP servers require trust before connection: %v", names)
}

func writeMCPResolutionDiagnostics(output io.Writer, resolution mcpConfigResolution) {
	if output == nil {
		return
	}
	for _, diagnostic := range resolution.Diagnostics {
		if diagnostic.Server == "" {
			fmt.Fprintf(output, "MCP config %q: %s.\n", diagnostic.Path, diagnostic.Reason)
		} else {
			fmt.Fprintf(output, "MCP server %q from %q: %s.\n", diagnostic.Server, diagnostic.Path, diagnostic.Reason)
		}
	}
	if err := mcpConfigPromptError(resolution.Prompt); err != nil {
		fmt.Fprintf(output, "%s; rerun with --trust-project-mcp or remember an exact approval.\n", err)
	}
}

func writeConfiguredMCPDiagnostics(output io.Writer, servers []configuredMCPServerInfo) {
	if output == nil {
		return
	}
	for _, server := range servers {
		if server.Error != "" {
			fmt.Fprintf(output, "MCP server %q: %s.\n", server.Name, server.Error)
		}
	}
}
