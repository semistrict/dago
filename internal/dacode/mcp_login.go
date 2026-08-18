package dacode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/dago/damcp"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
)

const (
	mcpLoginInputLimit = 32 << 10
	mcpLoginUsage      = "usage: dacode mcp login <server> [--mcp-config PATH] [--cwd PATH] [--trust-project-mcp] [--no-browser] [--slack-workspace ID]"
	mcpTokenDirectory  = "mcp-tokens"
)

type mcpLoginOptions struct {
	configPath     string
	workingDir     string
	slackWorkspace string
	trustProject   bool
	noBrowser      bool
	help           bool
}

type mcpLoginDependencies struct {
	httpClient *http.Client
	lookup     damcp.LookupEnv
	openURL    func(string) error
	home       func() (string, error)
	login      func(context.Context, *http.Client, string, talonmcp.Server, talonmcp.TokenStore, talonmcp.Interaction, oauthpolicy.Options) error
}

func defaultMCPLoginDependencies() mcpLoginDependencies {
	return mcpLoginDependencies{
		httpClient: &http.Client{Timeout: 2 * time.Minute},
		lookup:     os.LookupEnv,
		openURL:    openExternalURL,
		home:       os.UserHomeDir,
		login:      oauthpolicy.Login,
	}
}

func runMCPCommand(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runMCPCommandWithDependencies(ctx, arguments, stdin, stdout, stderr, defaultMCPLoginDependencies())
}

func runMCPCommandWithDependencies(
	ctx context.Context,
	arguments []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	dependencies mcpLoginDependencies,
) error {
	if ctx == nil || stdin == nil || stdout == nil || stderr == nil ||
		dependencies.httpClient == nil || dependencies.lookup == nil || dependencies.openURL == nil ||
		dependencies.home == nil || dependencies.login == nil {
		panic("dacode: MCP login dependencies are required")
	}
	options, positionals, err := parseMCPLoginArguments(arguments)
	if err != nil {
		return &commandExitError{code: 2, err: err}
	}
	if options.help || len(positionals) == 0 {
		_, err := fmt.Fprintln(stdout, mcpLoginUsage)
		return err
	}
	if len(positionals) != 2 || strings.ToLower(positionals[0]) != "login" || strings.TrimSpace(positionals[1]) == "" {
		return &commandExitError{code: 2, err: errors.New(mcpLoginUsage)}
	}
	serverName := positionals[1]
	homeDirectory, err := dependencies.home()
	if err != nil {
		return fmt.Errorf("resolve MCP user configuration directory: %w", err)
	}
	if homeDirectory == "" {
		return errors.New("resolve MCP user configuration directory: path is unavailable")
	}
	if !filepath.IsAbs(homeDirectory) {
		return errors.New("resolve MCP user configuration directory: path is not absolute")
	}
	workingDirectory, err := filepath.Abs(options.workingDir)
	if err != nil {
		return fmt.Errorf("resolve MCP project directory: %w", err)
	}
	policyPath := filepath.Join(homeDirectory, ".deepagents", "config.toml")
	resolution, err := resolveMCPConfig(
		ctx, homeDirectory, workingDirectory, options.configPath, policyPath,
		options.trustProject, dependencies.lookup,
	)
	if err != nil {
		return fmt.Errorf("resolve MCP login configuration: %w", err)
	}
	if options.configPath == "" && !mcpConfigSourceExists(resolution.Sources) {
		return &commandExitError{code: 2, err: errors.New("MCP OAuth login: no configuration file found")}
	}
	writeMCPLoginDiagnostics(stderr, resolution, serverName)
	connection, err := selectMCPLoginConnection(resolution, serverName)
	if err != nil {
		return err
	}
	tokenRoot := filepath.Join(homeDirectory, ".deepagents", ".state")
	store := talonmcp.NewFileTokenStore(filepath.Join(tokenRoot, mcpTokenDirectory))
	interaction := &mcpCLIInteraction{
		input:  bufio.NewReader(io.LimitReader(stdin, mcpLoginInputLimit+1)),
		output: stdout, openURL: dependencies.openURL, browser: !options.noBrowser,
		slackWorkspace: options.slackWorkspace,
	}
	server := talonmcp.Server{
		Type: connection.Transport, URL: connection.URL, Auth: connection.Auth,
		Headers: cloneStringMap(connection.Headers), AllowedTools: append([]string(nil), connection.AllowedTools...),
		DisabledTools: append([]string(nil), connection.DisabledTools...),
	}
	if err := dependencies.login(ctx, dependencies.httpClient, serverName, server, store, interaction, oauthpolicy.Options{}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("MCP OAuth login for %q failed; verify provider access and token-store permissions", serverName)
	}
	_, err = fmt.Fprintf(stdout, "MCP OAuth login for %s is ready (%s).\n", serverName, oauthpolicy.Select(connection.URL))
	return err
}

func mcpConfigSourceExists(sources []damcp.ConfigSource) bool {
	for _, source := range sources {
		if source.Exists {
			return true
		}
	}
	return false
}

func parseMCPLoginArguments(arguments []string) (mcpLoginOptions, []string, error) {
	options := mcpLoginOptions{workingDir: "."}
	positionals := make([]string, 0, 2)
	seen := map[string]bool{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--help" || argument == "-h":
			options.help = true
		case argument == "--trust-project-mcp":
			if seen[argument] {
				return mcpLoginOptions{}, nil, fmt.Errorf("%s may be specified once", argument)
			}
			seen[argument], options.trustProject = true, true
		case argument == "--no-browser":
			if seen[argument] {
				return mcpLoginOptions{}, nil, fmt.Errorf("%s may be specified once", argument)
			}
			seen[argument], options.noBrowser = true, true
		case argument == "--mcp-config" || argument == "--cwd" || argument == "--slack-workspace":
			if seen[argument] {
				return mcpLoginOptions{}, nil, fmt.Errorf("%s may be specified once", argument)
			}
			seen[argument] = true
			index++
			if index >= len(arguments) || strings.TrimSpace(arguments[index]) == "" {
				return mcpLoginOptions{}, nil, fmt.Errorf("%s requires one non-empty value", argument)
			}
			setMCPLoginOption(&options, argument, arguments[index])
		case strings.HasPrefix(argument, "--mcp-config=") || strings.HasPrefix(argument, "--cwd=") || strings.HasPrefix(argument, "--slack-workspace="):
			name, value, _ := strings.Cut(argument, "=")
			if seen[name] {
				return mcpLoginOptions{}, nil, fmt.Errorf("%s may be specified once", name)
			}
			if strings.TrimSpace(value) == "" {
				return mcpLoginOptions{}, nil, fmt.Errorf("%s requires one non-empty value", name)
			}
			seen[name] = true
			setMCPLoginOption(&options, name, value)
		case strings.HasPrefix(argument, "-"):
			return mcpLoginOptions{}, nil, fmt.Errorf("unknown mcp option %q", argument)
		default:
			positionals = append(positionals, argument)
		}
	}
	return options, positionals, nil
}

func setMCPLoginOption(options *mcpLoginOptions, name, value string) {
	switch name {
	case "--mcp-config":
		options.configPath = value
	case "--cwd":
		options.workingDir = value
	case "--slack-workspace":
		options.slackWorkspace = value
	}
}

func selectMCPLoginConnection(resolution mcpConfigResolution, serverName string) (damcp.Connection, error) {
	for _, connection := range resolution.OAuth {
		if connection.Name == serverName {
			return connection, nil
		}
	}
	for _, server := range resolution.Prompt {
		if server.Name == serverName {
			return damcp.Connection{}, fmt.Errorf("MCP server %q requires project trust before OAuth login", serverName)
		}
	}
	for _, server := range resolution.Disabled {
		if server.Name == serverName {
			return damcp.Connection{}, fmt.Errorf("MCP server %q is disabled by project policy", serverName)
		}
	}
	for _, connection := range resolution.Connections {
		if connection.Name == serverName {
			return damcp.Connection{}, fmt.Errorf("MCP server %q is not configured for OAuth", serverName)
		}
	}
	return damcp.Connection{}, fmt.Errorf("MCP OAuth server %q is not configured in a trusted source", serverName)
}

func writeMCPLoginDiagnostics(output io.Writer, resolution mcpConfigResolution, selected string) {
	filtered := resolution
	filtered.Diagnostics = nil
	for _, diagnostic := range resolution.Diagnostics {
		if diagnostic.Server == selected && diagnostic.Reason == "OAuth login is required before this server can connect" {
			continue
		}
		filtered.Diagnostics = append(filtered.Diagnostics, diagnostic)
	}
	writeMCPResolutionDiagnostics(output, filtered)
}

func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

type mcpCLIInteraction struct {
	input          *bufio.Reader
	output         io.Writer
	openURL        func(string) error
	browser        bool
	slackWorkspace string
}

func (interaction *mcpCLIInteraction) Authorize(ctx context.Context, authorizationURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	opened := interaction.browser && interaction.openURL(authorizationURL) == nil
	message := "Open this URL in a browser to authorize the MCP server:"
	if opened {
		message = "Opened a browser to authorize the MCP server. If it did not open, visit:"
	}
	if _, err := fmt.Fprintf(interaction.output, "%s\n%s\nPaste the final callback URL: ", message, authorizationURL); err != nil {
		return "", err
	}
	callback, err := interaction.readLine("OAuth callback URL")
	if err != nil {
		return "", err
	}
	if callback == "" {
		return "", errors.New("OAuth callback URL is required")
	}
	return callback, nil
}

func (interaction *mcpCLIInteraction) SelectSlackWorkspace(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if interaction.slackWorkspace != "" {
		return interaction.slackWorkspace, nil
	}
	if _, err := io.WriteString(interaction.output, "Slack workspace ID (leave blank to choose on Slack): "); err != nil {
		return "", err
	}
	return interaction.readLine("Slack workspace ID")
}

func (interaction *mcpCLIInteraction) PresentDeviceCode(ctx context.Context, device oauthpolicy.DeviceCode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if interaction.browser {
		_ = interaction.openURL(device.VerificationURI)
	}
	_, err := fmt.Fprintf(interaction.output, "Visit %s and enter code %s (expires in %s).\n", device.VerificationURI, device.UserCode, device.ExpiresIn.Round(time.Second))
	return err
}

func (interaction *mcpCLIInteraction) readLine(label string) (string, error) {
	if interaction == nil || interaction.input == nil {
		panic("dacode: initialized MCP interaction is required")
	}
	value, err := interaction.input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	if len(value) > mcpLoginInputLimit {
		return "", fmt.Errorf("%s exceeds %d bytes", label, mcpLoginInputLimit)
	}
	value = strings.TrimSpace(value)
	if value == "" && errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%s was not received before input closed", label)
	}
	return value, nil
}

var _ talonmcp.Interaction = (*mcpCLIInteraction)(nil)
var _ oauthpolicy.WorkspaceSelector = (*mcpCLIInteraction)(nil)
var _ oauthpolicy.DeviceCodePresenter = (*mcpCLIInteraction)(nil)
