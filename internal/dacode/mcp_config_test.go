package dacode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/damcp"
)

func TestResolveMCPConfigAppliesPrecedenceTrustAndInterpolation(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	policy := filepath.Join(t.TempDir(), "config.toml")
	writeMCPConfigFile(t, filepath.Join(home, ".deepagents", ".mcp.json"), `{
  "mcpServers": {
    "user": {"command":"user-server","env":{"TOKEN":"${USER_TOKEN}"}},
    "shadowed": {"command":"user-shadowed"}
  }
}`)
	writeMCPConfigFile(t, filepath.Join(project, ".mcp.json"), `{
  "mcpServers": {
    "project": {"command":"project-server"},
    "shadowed": {"command":"project-shadowed"}
  }
}`)
	environment := map[string]string{"USER_TOKEN": "private-token"}
	lookup := func(name string) (string, bool) { value, ok := environment[name]; return value, ok }

	untrusted, err := resolveMCPConfig(t.Context(), home, project, "", policy, false, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if names := mcpConnectionNames(untrusted); !slices.Equal(names, []string{"user"}) {
		t.Fatalf("untrusted connections = %#v", names)
	}
	if len(untrusted.Prompt) != 2 || untrusted.Prompt[0].Name != "project" || untrusted.Prompt[1].Name != "shadowed" {
		t.Fatalf("prompt = %#v", untrusted.Prompt)
	}
	if untrusted.Connections[0].Env["TOKEN"] != "private-token" {
		t.Fatalf("user environment = %#v", untrusted.Connections[0].Env)
	}
	if text := mcpConfigPromptError(untrusted.Prompt).Error(); !strings.Contains(text, "project") || strings.Contains(text, "project-server") {
		t.Fatalf("prompt error = %q", text)
	}

	trusted, err := resolveMCPConfig(t.Context(), home, project, "", policy, true, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if names := mcpConnectionNames(trusted); !slices.Equal(names, []string{"project", "shadowed", "user"}) {
		t.Fatalf("trusted connections = %#v", names)
	}
	if len(trusted.Prompt) != 0 || trusted.Connections[1].Command != "project-shadowed" {
		t.Fatalf("trusted resolution = %#v", trusted)
	}
	trusted.Connections[0].Command = "changed"
	if strings.Contains(string(untrusted.Prompt[0].Definition), "changed") {
		t.Fatal("trust resolution shared mutable definitions")
	}
}

func TestResolveMCPConfigRejectsDisabledAndSkipsInvalidOrOAuth(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	policy := filepath.Join(t.TempDir(), "config.toml")
	writeMCPConfigFile(t, filepath.Join(project, ".mcp.json"), `{
  "mcpServers": {
    "disabled": {"command":"disabled-server"},
    "invalid": {"type":"remote","url":"https://tools.example"},
    "oauth": {"type":"http","url":"https://tools.example/mcp","auth":"oauth"}
  }
}`)
	environment := map[string]string{
		damcp.DangerouslyEnableProjectServersEnv: "disabled,invalid,oauth",
		damcp.DisabledProjectServersEnv:          "disabled",
	}
	lookup := func(name string) (string, bool) { value, ok := environment[name]; return value, ok }
	resolution, err := resolveMCPConfig(t.Context(), home, project, "", policy, false, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Connections) != 0 || len(resolution.OAuth) != 1 || resolution.OAuth[0].Name != "oauth" || len(resolution.Disabled) != 1 || resolution.Disabled[0].Name != "disabled" {
		t.Fatalf("resolution = %#v", resolution)
	}
	if len(resolution.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %#v", resolution.Diagnostics)
	}
	for _, diagnostic := range resolution.Diagnostics {
		if strings.Contains(diagnostic.Reason, "tools.example") || strings.Contains(diagnostic.Reason, "disabled-server") {
			t.Fatalf("diagnostic leaked definition: %#v", diagnostic)
		}
	}
}

func TestResolveMCPConfigTreatsExplicitConfigAsTrusted(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	explicit := filepath.Join(project, "chosen.json")
	writeMCPConfigFile(t, filepath.Join(project, ".mcp.json"), `{"mcpServers":{"ignored":{"command":"ignored"}}}`)
	writeMCPConfigFile(t, explicit, `{"mcpServers":{"chosen":{"command":"chosen"}}}`)
	resolution, err := resolveMCPConfig(
		t.Context(), home, project, explicit, filepath.Join(t.TempDir(), "config.toml"), false,
		func(string) (string, bool) { return "", false },
	)
	if err != nil {
		t.Fatal(err)
	}
	if names := mcpConnectionNames(resolution); !slices.Equal(names, []string{"chosen"}) || len(resolution.Prompt) != 0 {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestResolveMCPConfigStaticInputs(t *testing.T) {
	for _, invoke := range []func(){
		func() {
			_, _ = resolveMCPConfig(nil, "", "", "", "policy", false, func(string) (string, bool) { return "", false })
		},
		func() { _, _ = resolveMCPConfig(t.Context(), "", "", "", "policy", false, nil) },
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

func TestParseCLIMCPConfigOptions(t *testing.T) {
	options, err := parseCLI([]string{"--mcp-config", "config/mcp.json", "--trust-project-mcp"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if options.mcpConfigPath != "config/mcp.json" || !options.trustProjectMCP {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseCLI([]string{"--mcp-config="}, os.Stderr); err == nil {
		t.Fatal("empty --mcp-config was accepted")
	}
}

func TestRunHeadlessPublishesExplicitMCPConfigTools(t *testing.T) {
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "configured", Version: "1"}, nil)
	mcp.AddTool(mcpServer, &mcp.Tool{Name: "lookup", Description: "Look up a record.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "record"}}}, nil, nil
		})
	mcpHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer }, &mcp.StreamableHTTPOptions{JSONResponse: true},
	))
	defer mcpHTTP.Close()

	var requestBody []byte
	modelHTTP := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var err error
		requestBody, err = io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"mcp-headless\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer modelHTTP.Close()

	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "mcp.json")
	writeMCPConfigFile(t, configPath, fmt.Sprintf(`{"mcpServers":{"external":{"type":"http","url":%q}}}`, mcpHTTP.URL))
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", modelHTTP.URL)
	var stdout, stderr bytes.Buffer
	err := Run(t.Context(), []string{
		"-n", "inspect", "--no-stream", "--model", defaultModel, "--cwd", workspace, "--state-dir", t.TempDir(), "--mcp-config", configPath,
	}, bytes.NewReader(nil), &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v; stderr: %s", err, stderr.String())
	}
	if !bytes.Contains(requestBody, []byte(`external_lookup`)) {
		t.Fatalf("model request does not advertise configured MCP tool: %s", requestBody)
	}
}

func mcpConnectionNames(resolution mcpConfigResolution) []string {
	result := make([]string, len(resolution.Connections))
	for index, connection := range resolution.Connections {
		result[index] = connection.Name
	}
	return result
}

func writeMCPConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
