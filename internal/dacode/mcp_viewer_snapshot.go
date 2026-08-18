package dacode

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/semistrict/dago/datool"
)

// mcpViewerServersFromRuntime projects one resolved configuration and its
// connection result into app-neutral viewer rows. It performs no I/O and
// tolerates a nil bundle while connections are still being established.
func mcpViewerServersFromRuntime(resolution mcpConfigResolution, bundle *configuredMCPBundle, pendingDisabled map[string]bool) []mcpViewerServer {
	connections := resolution.runtimeConnections()
	authByName := make(map[string]string, len(connections))
	transportByName := make(map[string]string, len(connections))
	for _, connection := range connections {
		authByName[connection.Name] = connection.Auth
		transportByName[connection.Name] = connection.Transport
	}

	definitions := map[string]datool.Definition{}
	if bundle != nil {
		definitions = make(map[string]datool.Definition, len(bundle.Tools))
		for _, tool := range bundle.Tools {
			if tool == nil {
				continue
			}
			definition := tool.Definition()
			definitions[definition.Name] = definition
		}
	}

	servers := make(map[string]mcpViewerServer, len(connections)+len(resolution.Disabled))
	if bundle != nil {
		for _, info := range bundle.Servers {
			server := mcpViewerServer{Name: info.Name, Transport: info.Transport, Status: mcpViewerOK}
			if info.Error != "" {
				server.Status, server.Detail = mcpViewerError, info.Error
				if authByName[info.Name] == "oauth" {
					server.Status = mcpViewerUnauthenticated
				}
			} else {
				server.Tools = make([]mcpViewerTool, 0, len(info.Tools))
				for _, name := range info.Tools {
					server.Tools = append(server.Tools, mcpViewerToolFromDefinition(name, definitions[name]))
				}
			}
			servers[info.Name] = server
		}
	}
	for _, connection := range connections {
		if pendingDisabled[connection.Name] {
			servers[connection.Name] = mcpViewerServer{
				Name: connection.Name, Transport: connection.Transport, Status: mcpViewerDisabled,
				Detail: "Disabled for this session.", PendingReconnect: true,
			}
			continue
		}
		if _, exists := servers[connection.Name]; exists {
			continue
		}
		servers[connection.Name] = mcpViewerServer{
			Name: connection.Name, Transport: connection.Transport,
			Status: mcpViewerAwaitingReconnect, Detail: "Connection is still loading.",
		}
	}
	for _, disabled := range resolution.Disabled {
		servers[disabled.Name] = mcpViewerServer{
			Name: disabled.Name, Transport: transportByName[disabled.Name], Status: mcpViewerDisabled,
			Detail: "Disabled by project policy.", PendingReconnect: pendingDisabled[disabled.Name],
		}
	}

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]mcpViewerServer, 0, len(names))
	for _, name := range names {
		result = append(result, servers[name])
	}
	return result
}

func mcpViewerToolFromDefinition(fallbackName string, definition datool.Definition) mcpViewerTool {
	name := definition.Name
	if name == "" {
		name = fallbackName
	}
	tool := mcpViewerTool{Name: name, Description: definition.Description}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if len(definition.InputSchema) == 0 || json.Unmarshal(definition.InputSchema, &schema) != nil {
		return tool
	}
	required := make(map[string]struct{}, len(schema.Required))
	for _, field := range schema.Required {
		required[field] = struct{}{}
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	tool.Parameters = make([]mcpViewerParameter, 0, len(names))
	for _, name := range names {
		_, isRequired := required[name]
		tool.Parameters = append(tool.Parameters, mcpViewerParameter{
			Name: name, Type: mcpViewerSchemaType(schema.Properties[name]), Required: isRequired,
		})
	}
	return tool
}

func mcpViewerSchemaType(raw json.RawMessage) string {
	var value struct {
		Type any `json:"type"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "any"
	}
	switch typed := value.Type.(type) {
	case string:
		if typed != "" {
			return typed
		}
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			part, ok := item.(string)
			if ok && part != "" {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	return "any"
}
