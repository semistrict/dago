// Command datalon exposes local long-running assistant utilities.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/dago/datalon/fleet"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
)

const importFleetUsage = "usage: datalon import-fleet <fleet-export.zip> <assistant-state-dir>"

const mcpUsage = "usage: datalon mcp config | datalon mcp login <server>"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithIO(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stdout io.Writer) error {
	return runWithIO(ctx, arguments, strings.NewReader(""), stdout)
}

func runWithIO(ctx context.Context, arguments []string, stdin io.Reader, stdout io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if len(arguments) > 0 && arguments[0] == "mcp" {
		return runMCP(ctx, arguments[1:], stdin, stdout)
	}
	if len(arguments) != 3 || arguments[0] != "import-fleet" {
		return fmt.Errorf("%s", importFleetUsage)
	}
	result, err := fleet.Import(ctx, arguments[1], arguments[2], fleet.Options{})
	if err != nil {
		return fmt.Errorf("import-fleet: %w", err)
	}
	if _, err := io.WriteString(stdout, fleet.FormatHandoff(result)); err != nil {
		return fmt.Errorf("write import-fleet summary: %w", err)
	}
	return nil
}

func runMCP(ctx context.Context, arguments []string, stdin io.Reader, stdout io.Writer) error {
	environment := commandEnvironment()
	home, _ := os.UserHomeDir()
	if len(arguments) == 1 && arguments[0] == "config" {
		_, err := io.WriteString(stdout, formatMCPConfig(talonmcp.Discover(environment, home)))
		return err
	}
	if len(arguments) != 2 || arguments[0] != "login" || strings.TrimSpace(arguments[1]) == "" {
		return fmt.Errorf("%s", mcpUsage)
	}
	configPath, err := talonmcp.Resolve(environment, home)
	if err != nil {
		return fmt.Errorf("resolve MCP config: %w", err)
	}
	if configPath == "" {
		return fmt.Errorf("MCP login: no configuration file found")
	}
	config, err := talonmcp.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("MCP login: %w", err)
	}
	server, ok := config.Servers[arguments[1]]
	if !ok {
		return fmt.Errorf("MCP login: server %q is not configured", arguments[1])
	}
	stateRoot := strings.TrimSpace(environment["DEEPAGENTS_TALON_HOME"])
	if stateRoot == "" {
		if home == "" {
			return fmt.Errorf("MCP login: user home is unavailable")
		}
		stateRoot = filepath.Join(home, ".deepagents")
	}
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	store := talonmcp.NewFileTokenStore(filepath.Join(stateRoot, ".state", "mcp-oauth"))
	interaction := &pasteInteraction{input: bufio.NewReader(io.LimitReader(stdin, 32<<10)), output: stdout}
	if err := talonmcp.Login(ctx, httpClient, arguments[1], server, store, interaction); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "MCP OAuth login for %s is ready.\n", arguments[1])
	return err
}

type pasteInteraction struct {
	input  *bufio.Reader
	output io.Writer
}

func (interaction *pasteInteraction) Authorize(ctx context.Context, authorizationURL string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(interaction.output, "Open this URL to authorize the MCP server:\n%s\nPaste the final callback URL: ", authorizationURL); err != nil {
		return "", err
	}
	callback, err := interaction.input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read OAuth callback: %w", err)
	}
	callback = strings.TrimSpace(callback)
	if callback == "" {
		return "", fmt.Errorf("OAuth callback URL is required")
	}
	return callback, nil
}

func formatMCPConfig(candidates []talonmcp.Candidate) string {
	var output strings.Builder
	output.WriteString("MCP config paths (highest to lowest precedence):\n")
	for _, candidate := range candidates {
		status := "missing"
		if candidate.Exists {
			status = "found"
		}
		if candidate.Selected {
			status = "selected"
		}
		fmt.Fprintf(&output, "  [%-8s] %s (%s)\n", status, candidate.Path, candidate.Source)
	}
	return output.String()
}

func commandEnvironment() map[string]string {
	result := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			result[name] = value
		}
	}
	return result
}
