package damcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverConfigsMergesStandardSourcesByServerName(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	writeMCPConfig(t, filepath.Join(home, ".deepagents", ".mcp.json"), `{
  "mcpServers": {
    "alpha": {"command":"user-alpha"},
    "shared": {"command":"user-shared"}
  }
}`)
	writeMCPConfig(t, filepath.Join(project, ".deepagents", ".mcp.json"), `{
  "mcpServers": {
    "beta": {"command":"project-beta"},
    "shared": {"command":"project-subdir"}
  }
}`)
	writeMCPConfig(t, filepath.Join(project, ".mcp.json"), `{
  "mcpServers": {
    "shared": {"command":"project-root"}
  }
}`)
	report, err := DiscoverConfigs(t.Context(), home, project, ConfigOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sources) != 3 || !report.Sources[0].Exists || !report.Sources[1].Exists || !report.Sources[2].Exists {
		t.Fatalf("sources = %#v", report.Sources)
	}
	if names := configuredServerNames(report.Servers); !slices.Equal(names, []string{"alpha", "beta", "shared"}) {
		t.Fatalf("server names = %#v", names)
	}
	shared := report.Servers[2]
	if shared.Scope != ProjectConfigScope || shared.Source != filepath.Join(project, ".mcp.json") || !strings.Contains(string(shared.Definition), "project-root") {
		t.Fatalf("shared = %#v", shared)
	}
	report.Servers[2].Definition[0] = '['
	again, err := DiscoverConfigs(t.Context(), home, project, ConfigOptions{})
	if err != nil || len(again.Servers) != 3 || again.Servers[2].Definition[0] != '{' {
		t.Fatalf("rediscovery = %#v, %v", again, err)
	}
}

func TestDiscoverConfigsSurfacesInvalidOptionalSourcesAndRequiresExplicit(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	writeMCPConfig(t, filepath.Join(home, ".deepagents", ".mcp.json"), `{bad`)
	writeMCPConfig(t, filepath.Join(project, ".mcp.json"), `{"mcpServers":{"ok":{"command":"server"}}}`)
	report, err := DiscoverConfigs(t.Context(), home, project, ConfigOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Servers) != 1 || len(report.Diagnostics) != 1 || strings.Contains(report.Diagnostics[0].Reason, "{bad") {
		t.Fatalf("report = %#v", report)
	}
	missing := filepath.Join(project, "missing.json")
	if _, err := DiscoverConfigs(t.Context(), home, project, ConfigOptions{ExplicitPath: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing error = %v", err)
	}
	relative := filepath.Join("config", "mcp.json")
	writeMCPConfig(t, filepath.Join(project, relative), `{"mcpServers":{"explicit":{"command":"server"}}}`)
	explicit, err := DiscoverConfigs(t.Context(), home, project, ConfigOptions{ExplicitPath: relative})
	if err != nil || len(explicit.Sources) != 1 || explicit.Sources[0].Scope != ExplicitConfigScope || len(explicit.Servers) != 1 {
		t.Fatalf("explicit report = %#v, %v", explicit, err)
	}
}

func TestDiscoverConfigsRejectsLinksBoundsAndStaticInputs(t *testing.T) {
	project := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.json")
	writeMCPConfig(t, target, `{"mcpServers":{"linked":{"command":"server"}}}`)
	if err := os.Symlink(target, filepath.Join(project, ".mcp.json")); err == nil {
		report, discoverErr := DiscoverConfigs(t.Context(), "", project, ConfigOptions{})
		if discoverErr != nil || len(report.Servers) != 0 || len(report.Diagnostics) != 1 {
			t.Fatalf("linked report = %#v, %v", report, discoverErr)
		}
	}
	large := filepath.Join(project, ".deepagents", ".mcp.json")
	writeMCPConfig(t, large, `{"mcpServers":{"large":{"command":"`+strings.Repeat("x", 128)+`"}}}`)
	report, err := DiscoverConfigs(t.Context(), "", project, ConfigOptions{MaxConfigBytes: 64})
	if err != nil || len(report.Diagnostics) == 0 {
		t.Fatalf("bounded report = %#v, %v", report, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverConfigs(ctx, "", project, ConfigOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	for _, invoke := range []func(){
		func() { _, _ = DiscoverConfigs(nil, "", "", ConfigOptions{}) },
		func() { _, _ = DiscoverConfigs(t.Context(), "relative", "", ConfigOptions{}) },
		func() { _, _ = DiscoverConfigs(t.Context(), "", "relative", ConfigOptions{}) },
		func() { _, _ = DiscoverConfigs(t.Context(), "", "", ConfigOptions{MaxServers: -1}) },
		func() { _, _ = DiscoverConfigs(t.Context(), "", "", ConfigOptions{ExplicitPath: "relative"}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected static input panic")
				}
			}()
			invoke()
		}()
	}
}

func TestResolveConnectionInterpolatesAndValidatesWithoutMutation(t *testing.T) {
	definition := []byte(`{
  "type":"streamable-http",
  "url":"https://tools.example/${TENANT}",
  "headers":{"Authorization":"Bearer ${TOKEN}","X-Default":"${MISSING:-fallback}"},
  "allowedTools":["read_*","server_lookup"],
  "unknownFutureField":true
}`)
	server := ConfiguredServer{Name: "server", Definition: definition}
	environment := map[string]string{"TENANT": "team", "TOKEN": "secret"}
	connection, err := ResolveConnection(server, func(name string) (string, bool) {
		value, ok := environment[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if connection.Transport != "http" || connection.URL != "https://tools.example/team" {
		t.Fatalf("connection = %#v", connection)
	}
	if connection.Headers["Authorization"] != "Bearer secret" || connection.Headers["X-Default"] != "fallback" {
		t.Fatalf("headers = %#v", connection.Headers)
	}
	if !connection.MatchTool("read_file") || !connection.MatchTool("lookup") || connection.MatchTool("delete_file") {
		t.Fatalf("unexpected tool-filter result: %#v", connection)
	}
	connection.Headers["Authorization"] = "changed"
	if strings.Contains(string(server.Definition), "changed") || !strings.Contains(string(server.Definition), "${TOKEN}") {
		t.Fatal("resolved connection mutated raw definition")
	}
	if got := connection.HTTPHeaders().Get("X-Default"); got != "fallback" {
		t.Fatalf("HTTP header = %q", got)
	}
}

func TestResolveConnectionSupportsBoundedAbsoluteStdioWorkingDirectory(t *testing.T) {
	working := t.TempDir()
	definition, err := json.Marshal(map[string]any{"command": "helper", "cwd": "${PLUGIN_ROOT}"})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := ResolveConnection(ConfiguredServer{Name: "plugin", Definition: definition}, func(name string) (string, bool) {
		return working, name == "PLUGIN_ROOT"
	})
	if err != nil || connection.CWD != working {
		t.Fatalf("connection = %#v, %v", connection, err)
	}
	for _, raw := range []string{`{"command":"helper","cwd":"relative"}`, `{"type":"http","url":"https://tools.example","cwd":"/tmp"}`} {
		if _, err := ResolveConnection(ConfiguredServer{Name: "plugin", Definition: []byte(raw)}, os.LookupEnv); err == nil {
			t.Fatalf("unsafe working directory accepted: %s", raw)
		}
	}
}

func TestResolveConnectionRejectsUnsafeOrMalformedDefinitions(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	credentialURL := `https://user:` + `pass@tools.example`
	tests := []string{
		`{"command":"${UNSET}"}`,
		`{"command":"${BAD-default}"}`,
		`{"type":"stdio","command":"run","url":"https://tools.example"}`,
		`{"type":"http","url":"` + credentialURL + `"}`,
		`{"type":"http","url":"https://tools.example/#fragment"}`,
		`{"type":"http","url":"https://tools.example","auth":"oauth","headers":{"Authorization":"secret"}}`,
		`{"type":"http","url":"http://tools.example","auth":"oauth"}`,
		`{"type":"stdio","command":"run","auth":"oauth"}`,
		`{"type":"http","transport":"stdio","url":"https://tools.example"}`,
		`{"command":"run","allowedTools":["read"],"disabledTools":["write"]}`,
		`{"command":"run","allowedTools":["["]}`,
		`[]`,
	}
	for _, definition := range tests {
		_, err := ResolveConnection(ConfiguredServer{Name: "server", Definition: []byte(definition)}, lookup)
		if err == nil {
			t.Fatalf("definition accepted: %s", definition)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user:pass") {
			t.Fatalf("error leaked definition value: %v", err)
		}
	}
	deferred, err := ResolveConnection(ConfiguredServer{Name: "server", Definition: []byte(`{"command":"${EMPTY:-default}"}`)}, func(string) (string, bool) { return "", true })
	if err != nil || deferred.Command != "default" {
		t.Fatalf("empty fallback = %#v, %v", deferred, err)
	}
	for _, invoke := range []func(){
		func() {
			_, _ = ResolveConnection(ConfiguredServer{Name: "server", Definition: []byte(`{"command":"run"}`)}, nil)
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected static input panic")
				}
			}()
			invoke()
		}()
	}
}

func configuredServerNames(servers []ConfiguredServer) []string {
	result := make([]string, len(servers))
	for index, server := range servers {
		result[index] = server.Name
	}
	return result
}

func writeMCPConfig(t *testing.T, filePath, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
