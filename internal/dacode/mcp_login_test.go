package dacode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	talonmcp "github.com/semistrict/dago/datalon/mcp"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
	"golang.org/x/oauth2"
)

func TestRunMCPLoginUsesExplicitTrustedConfigAndPrivateStore(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "mcp.json")
	writeMCPConfigFile(t, configPath, `{"mcpServers":{"tools":{"type":"http","url":"https://tools.example/mcp","auth":"oauth","headers":{"X-Operator":"${MCP_OPERATOR}"}}}}`)
	loginCalls := 0
	dependencies := testMCPLoginDependencies(home)
	dependencies.lookup = func(name string) (string, bool) {
		if name == "MCP_OPERATOR" {
			return "private-operator-value", true
		}
		return "", false
	}
	dependencies.login = func(ctx context.Context, client *http.Client, name string, server talonmcp.Server, store talonmcp.TokenStore, interaction talonmcp.Interaction, options oauthpolicy.Options) error {
		loginCalls++
		if client == nil || name != "tools" || server.Auth != "oauth" || server.URL != "https://tools.example/mcp" || server.Headers["X-Operator"] != "private-operator-value" {
			t.Fatalf("login inputs = name %q, server %#v", name, server)
		}
		return store.Save(ctx, strings.Repeat("a", 64), &oauth2.Token{AccessToken: "stored-access-token"})
	}
	var stdout, stderr bytes.Buffer
	err := runMCPCommandWithDependencies(t.Context(), []string{
		"login", "tools", "--mcp-config", configPath, "--no-browser",
	}, strings.NewReader(""), &stdout, &stderr, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if loginCalls != 1 || !strings.Contains(stdout.String(), "tools is ready (generic)") {
		t.Fatalf("login calls = %d, stdout = %q", loginCalls, stdout.String())
	}
	for _, secret := range []string{"private-operator-value", "stored-access-token"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatalf("output leaked %q: %q", secret, stdout.String()+stderr.String())
		}
	}
	tokenPath := filepath.Join(home, ".deepagents", ".state", mcpTokenDirectory, strings.Repeat("a", 64)+".json")
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %o", info.Mode().Perm())
	}
}

func TestRunMCPLoginRejectsUntrustedProjectAndNonOAuthServers(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	writeMCPConfigFile(t, filepath.Join(project, ".mcp.json"), `{"mcpServers":{"project":{"type":"http","url":"https://tools.example/mcp","auth":"oauth"}}}`)
	writeMCPConfigFile(t, filepath.Join(home, ".deepagents", ".mcp.json"), `{"mcpServers":{"plain":{"type":"http","url":"https://tools.example/mcp"}}}`)
	dependencies := testMCPLoginDependencies(home)
	dependencies.login = func(context.Context, *http.Client, string, talonmcp.Server, talonmcp.TokenStore, talonmcp.Interaction, oauthpolicy.Options) error {
		t.Fatal("login must not run for an untrusted or non-OAuth server")
		return nil
	}
	for _, test := range []struct {
		server string
		want   string
	}{
		{server: "project", want: "requires project trust"},
		{server: "plain", want: "not configured for OAuth"},
		{server: "missing", want: "not configured in a trusted source"},
	} {
		var stderr bytes.Buffer
		err := runMCPCommandWithDependencies(t.Context(), []string{
			"login", test.server, "--cwd", project,
		}, strings.NewReader(""), io.Discard, &stderr, dependencies)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("login %s error = %v, want %q", test.server, err, test.want)
		}
	}
}

func TestMCPLoginInteractionsAreBoundedAndProviderCapable(t *testing.T) {
	t.Run("authorization", func(t *testing.T) {
		opened := ""
		var output bytes.Buffer
		interaction := &mcpCLIInteraction{
			input:  bufioReader("http://127.0.0.1:53682/callback?code=ok&state=state\n"),
			output: &output, browser: true, openURL: func(value string) error { opened = value; return nil },
		}
		callback, err := interaction.Authorize(t.Context(), "https://provider.example/authorize?state=state")
		if err != nil || !strings.Contains(callback, "code=ok") || opened == "" || !strings.Contains(output.String(), "Opened a browser") {
			t.Fatalf("Authorize callback=%q opened=%q output=%q err=%v", callback, opened, output.String(), err)
		}
	})
	t.Run("slack", func(t *testing.T) {
		interaction := &mcpCLIInteraction{input: bufioReader("T01234567\n"), output: io.Discard, openURL: func(string) error { return nil }}
		workspace, err := interaction.SelectSlackWorkspace(t.Context())
		if err != nil || workspace != "T01234567" {
			t.Fatalf("workspace=%q err=%v", workspace, err)
		}
	})
	t.Run("github", func(t *testing.T) {
		var output bytes.Buffer
		interaction := &mcpCLIInteraction{input: bufioReader(""), output: &output, openURL: func(string) error { return errors.New("browser unavailable") }, browser: true}
		err := interaction.PresentDeviceCode(t.Context(), oauthpolicy.DeviceCode{UserCode: "ABCD-EFGH", VerificationURI: "https://github.com/login/device", ExpiresIn: time.Minute})
		if err != nil || !strings.Contains(output.String(), "ABCD-EFGH") {
			t.Fatalf("device output=%q err=%v", output.String(), err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		interaction := &mcpCLIInteraction{input: bufioReader(strings.Repeat("x", mcpLoginInputLimit+1)), output: io.Discard, openURL: func(string) error { return nil }}
		if _, err := interaction.Authorize(t.Context(), "https://provider.example/authorize"); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized callback error = %v", err)
		}
	})
}

func TestMCPCommandParsingAndEarlyDispatch(t *testing.T) {
	options, positionals, err := parseMCPLoginArguments([]string{
		"login", "server", "--mcp-config=config.json", "--cwd", "workspace", "--trust-project-mcp", "--no-browser", "--slack-workspace", "T0123",
	})
	if err != nil || len(positionals) != 2 || options.configPath != "config.json" || options.workingDir != "workspace" || !options.trustProject || !options.noBrowser || options.slackWorkspace != "T0123" {
		t.Fatalf("options=%#v positionals=%#v err=%v", options, positionals, err)
	}
	if _, _, err := parseMCPLoginArguments([]string{"login", "server", "--mcp-config", ""}); err == nil {
		t.Fatal("empty config path was accepted")
	}
	var output bytes.Buffer
	if err := Run(t.Context(), []string{"mcp", "--help"}, strings.NewReader(""), &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "mcp login <server>") {
		t.Fatalf("help output = %q", output.String())
	}
}

func TestRunMCPLoginNoDiscoveredConfigUsesStatusTwo(t *testing.T) {
	t.Parallel()
	dependencies := testMCPLoginDependencies(t.TempDir())
	err := runMCPCommandWithDependencies(
		t.Context(), []string{"login", "missing"}, strings.NewReader(""), io.Discard, io.Discard, dependencies,
	)
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "no configuration file found") {
		t.Fatalf("error=%v exit=%d", err, ExitCode(err))
	}
}

func TestRunMCPLoginDoesNotExposeDependencyErrors(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "mcp.json")
	writeMCPConfigFile(t, configPath, `{"mcpServers":{"tools":{"type":"http","url":"https://tools.example/mcp","auth":"oauth"}}}`)
	dependencies := testMCPLoginDependencies(home)
	secret := "provider-body-with-access-token"
	dependencies.login = func(context.Context, *http.Client, string, talonmcp.Server, talonmcp.TokenStore, talonmcp.Interaction, oauthpolicy.Options) error {
		return errors.New(secret)
	}
	err := runMCPCommandWithDependencies(t.Context(), []string{
		"login", "tools", "--mcp-config", configPath,
	}, strings.NewReader(""), io.Discard, io.Discard, dependencies)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "token-store permissions") {
		t.Fatalf("sanitized error = %v", err)
	}
}

func TestRunMCPCommandRequiresDependencies(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("nil login dependency was accepted")
		}
	}()
	_ = runMCPCommandWithDependencies(t.Context(), []string{"--help"}, strings.NewReader(""), io.Discard, io.Discard, mcpLoginDependencies{})
}

func testMCPLoginDependencies(home string) mcpLoginDependencies {
	return mcpLoginDependencies{
		httpClient: &http.Client{},
		lookup:     func(string) (string, bool) { return "", false },
		openURL:    func(string) error { return nil },
		home:       func() (string, error) { return home, nil },
		login: func(context.Context, *http.Client, string, talonmcp.Server, talonmcp.TokenStore, talonmcp.Interaction, oauthpolicy.Options) error {
			return nil
		},
	}
}

func bufioReader(value string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(value))
}
