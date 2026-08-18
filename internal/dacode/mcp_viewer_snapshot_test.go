package dacode

import (
	"encoding/json"
	"testing"

	"github.com/semistrict/dago/damcp"
	"github.com/semistrict/dago/datool"
)

func TestMCPViewerRuntimeProjectionPreservesToolsAuthAndDisabledState(t *testing.T) {
	readTool := datool.Func{Spec: datool.Definition{
		Name: "files_read", Description: "Read a file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"optional":{"type":["string","null"]},"path":{"type":"string"}},"required":["path"]}`),
	}}
	resolution := mcpConfigResolution{
		Connections: []damcp.Connection{{Name: "files", Transport: "stdio"}},
		OAuth:       []damcp.Connection{{Name: "remote", Transport: "http", Auth: "oauth"}},
		Disabled:    []damcp.Server{{Name: "blocked"}},
	}
	bundle := &configuredMCPBundle{
		Tools: []datool.Tool{readTool},
		Servers: []configuredMCPServerInfo{
			{Name: "files", Transport: "stdio", Tools: []string{"files_read"}},
			{Name: "remote", Transport: "http", Error: "OAuth login is required or stored credentials could not be used"},
		},
	}
	servers := mcpViewerServersFromRuntime(resolution, bundle, map[string]bool{"blocked": true})
	files := findMCPViewerServer(t, servers, "files")
	if files.Status != mcpViewerOK || len(files.Tools) != 1 || files.Tools[0].Description != "Read a file." {
		t.Fatalf("files projection = %#v", files)
	}
	if parameters := files.Tools[0].Parameters; len(parameters) != 2 || parameters[0].Name != "optional" || parameters[0].Type != "string | null" || parameters[0].Required || parameters[1].Name != "path" || !parameters[1].Required {
		t.Fatalf("schema parameters = %#v", parameters)
	}
	remote := findMCPViewerServer(t, servers, "remote")
	if remote.Status != mcpViewerUnauthenticated || remote.Detail == "" || len(remote.Tools) != 0 {
		t.Fatalf("OAuth projection = %#v", remote)
	}
	blocked := findMCPViewerServer(t, servers, "blocked")
	if blocked.Status != mcpViewerDisabled || !blocked.PendingReconnect || blocked.Detail != "Disabled by project policy." {
		t.Fatalf("disabled projection = %#v", blocked)
	}
}

func TestMCPViewerRuntimeProjectionUsesLoadingStateWithoutBundle(t *testing.T) {
	resolution := mcpConfigResolution{Connections: []damcp.Connection{{Name: "loading", Transport: "sse"}}}
	servers := mcpViewerServersFromRuntime(resolution, nil, nil)
	if len(servers) != 1 || servers[0].Status != mcpViewerAwaitingReconnect || servers[0].Transport != "sse" {
		t.Fatalf("loading projection = %#v", servers)
	}
}

func TestMCPViewerSchemaTypeFailsClosedOnMalformedExternalShapes(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{bad`), json.RawMessage(`{"type":42}`), json.RawMessage(`{"type":["string",42]}`)} {
		got := mcpViewerSchemaType(raw)
		if string(raw) == `{"type":["string",42]}` {
			if got != "string" {
				t.Fatalf("partially valid union type = %q", got)
			}
		} else if got != "any" {
			t.Fatalf("schema type %s = %q", raw, got)
		}
	}
}
