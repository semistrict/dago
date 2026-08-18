package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/dago/damanaged"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

var mcpUUID = regexp.MustCompile(`\A[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\z`)

type managedMCPRegistry interface {
	ListMCPServers(context.Context) ([]damanaged.MCPServer, error)
	GetMCPServer(context.Context, string) (damanaged.MCPServer, error)
	CreateMCPServer(context.Context, string, string, damanaged.MCPServerOptions) (damanaged.MCPServer, error)
	UpdateMCPServer(context.Context, string, damanaged.MCPServerPatch) (damanaged.MCPServer, error)
	DeleteMCPServer(context.Context, string) error
	ListMCPServerTools(context.Context, string, string) ([]map[string]any, error)
	RegisterMCPProvider(context.Context, string) (map[string]any, error)
	CreateAuthSession(context.Context, string, []string, bool) (map[string]any, error)
	GetAuthSession(context.Context, string, int) (map[string]any, error)
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runMCPServers(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	client, err := managedClientFromEnvironment()
	if err != nil {
		return err
	}
	return runMCPServersWithClient(context.Background(), client, arguments, stdin, stdout, stderr)
}

func runMCPServersWithClient(ctx context.Context, client managedMCPRegistry, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if client == nil {
		panic("managed MCP registry client is required")
	}
	if len(arguments) == 0 {
		return errors.New("usage: dago mcp-servers list|add|get|update|delete|connect|tools")
	}
	switch arguments[0] {
	case "list":
		if len(arguments) != 1 {
			return errors.New("usage: dago mcp-servers list")
		}
		servers, err := client.ListMCPServers(ctx)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", safeCLIValue(server["id"]), safeCLIValue(server["name"]), safeCLIValue(server["url"])); err != nil {
				return err
			}
		}
		return nil
	case "get":
		if len(arguments) != 2 {
			return errors.New("usage: dago mcp-servers get ID|NAME|URL")
		}
		serverID, err := resolveMCPServerID(ctx, client, arguments[1])
		if err != nil {
			return err
		}
		server, err := client.GetMCPServer(ctx, serverID)
		if err != nil {
			return err
		}
		return writeJSON(stdout, redactMCPServer(server))
	case "add":
		return runMCPAdd(ctx, client, arguments[1:], stdout, stderr)
	case "update":
		return runMCPUpdate(ctx, client, arguments[1:], stdout, stderr)
	case "delete":
		return runMCPDelete(ctx, client, arguments[1:], stdin, stdout, stderr)
	case "tools":
		if len(arguments) != 2 {
			return errors.New("usage: dago mcp-servers tools ID|NAME|URL")
		}
		serverID, err := resolveMCPServerID(ctx, client, arguments[1])
		if err != nil {
			return err
		}
		server, err := client.GetMCPServer(ctx, serverID)
		if err != nil {
			return err
		}
		return printMCPTools(ctx, client, server, stdout)
	case "connect":
		return runMCPConnect(ctx, client, arguments[1:], stdout, stderr, openManagedURL)
	default:
		return fmt.Errorf("unknown mcp-servers command %q", arguments[0])
	}
}

func runMCPConnect(ctx context.Context, client managedMCPRegistry, arguments []string, stdout, stderr io.Writer, opener func(string) error) error {
	flags := flag.NewFlagSet("dago mcp-servers connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	forceNew := flags.Bool("force-new", false, "create a fresh authorization instead of reusing one")
	timeoutSeconds := flags.Int("timeout", 300, "seconds to wait; zero starts without polling")
	noBrowser := flags.Bool("no-browser", false, "print the verification URL without opening it")
	var scopes stringList
	flags.Var(&scopes, "scope", "OAuth scope; repeat for multiple")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 || *timeoutSeconds < 0 || *timeoutSeconds > 3600 {
		return errors.New("usage: dago mcp-servers connect [--scope SCOPE] [--force-new] [--timeout 0..3600] [--no-browser] ID|NAME|URL")
	}
	serverID, err := resolveMCPServerID(ctx, client, flags.Arg(0))
	if err != nil {
		return err
	}
	return connectMCPServerOAuth(ctx, client, serverID, scopes, *forceNew, *timeoutSeconds, *noBrowser, stdout, opener)
}

func connectMCPServerOAuth(ctx context.Context, client managedMCPRegistry, serverID string, scopes []string, forceNew bool, timeoutSeconds int, noBrowser bool, stdout io.Writer, opener func(string) error) error {
	provider, err := client.RegisterMCPProvider(ctx, serverID)
	if err != nil {
		return err
	}
	providerID, _ := provider["oauth_provider_id"].(string)
	if providerID == "" {
		return errors.New("OAuth provider registration returned no provider ID")
	}
	session, err := client.CreateAuthSession(ctx, providerID, scopes, forceNew)
	if err != nil {
		return err
	}
	status := oauthStatus(session)
	if status == "COMPLETED" {
		_, err := fmt.Fprintln(stdout, "MCP OAuth connection is ready.")
		return err
	}
	if status != "PENDING" {
		return fmt.Errorf("OAuth session ended with status %s", safeCLIValue(status))
	}
	verificationURL, _ := session["verification_url"].(string)
	if err := validateVerificationURL(verificationURL); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Open this URL to authorize the MCP server:\n  %s\n", safeCLIValue(verificationURL)); err != nil {
		return err
	}
	if !noBrowser && opener != nil {
		if err := opener(verificationURL); err != nil {
			_, _ = fmt.Fprintln(stdout, "Could not open the browser automatically; use the URL above.")
		}
	}
	sessionID, _ := session["id"].(string)
	if sessionID == "" {
		return errors.New("pending OAuth session returned no session ID")
	}
	if timeoutSeconds == 0 {
		_, err := fmt.Fprintln(stdout, "Authorization started; rerun connect to check it later.")
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	for {
		session, err = client.GetAuthSession(waitCtx, sessionID, 5)
		if err != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return errors.New("timed out waiting for OAuth completion")
			}
			return err
		}
		status = oauthStatus(session)
		switch status {
		case "COMPLETED":
			_, err := fmt.Fprintln(stdout, "MCP OAuth connection is ready.")
			return err
		case "PENDING":
			timer := time.NewTimer(time.Second)
			select {
			case <-waitCtx.Done():
				timer.Stop()
				return errors.New("timed out waiting for OAuth completion")
			case <-timer.C:
			}
			continue
		default:
			return fmt.Errorf("OAuth session ended with status %s", safeCLIValue(status))
		}
	}
}

func oauthStatus(session map[string]any) string {
	status, _ := session["status"].(string)
	return strings.ToUpper(strings.TrimSpace(status))
}

func validateVerificationURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("OAuth verification URL is invalid")
	}
	return nil
}

func openManagedURL(rawURL string) error {
	if err := validateVerificationURL(rawURL); err != nil {
		return err
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", rawURL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		command = exec.Command("xdg-open", rawURL)
	}
	return command.Start()
}

func runMCPAdd(ctx context.Context, client managedMCPRegistry, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dago mcp-servers add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "registry display name")
	authType := flags.String("auth-type", "", "headers or oauth")
	noTools := flags.Bool("no-tools", false, "skip best-effort tool discovery")
	connectOAuth := flags.Bool("connect", false, "connect the new OAuth server")
	forceNew := flags.Bool("force-new", false, "create a fresh authorization instead of reusing one")
	timeoutSeconds := flags.Int("timeout", 300, "seconds to wait; zero starts without polling")
	noBrowser := flags.Bool("no-browser", false, "print the verification URL without opening it")
	var rawHeaders stringList
	var scopes stringList
	flags.Var(&rawHeaders, "header", "KEY=VALUE header; repeat for multiple")
	flags.Var(&scopes, "scope", "OAuth scope; repeat for multiple")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 || *timeoutSeconds < 0 || *timeoutSeconds > 3600 {
		return errors.New("usage: dago mcp-servers add [options] HTTPS_URL")
	}
	rawURL := flags.Arg(0)
	headers, err := parseMCPHeaders(rawHeaders)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("MCP server URL is invalid")
	}
	if strings.TrimSpace(*name) == "" {
		*name = parsed.Hostname()
	}
	resolvedAuthType := *authType
	if resolvedAuthType == "" && len(headers) > 0 {
		resolvedAuthType = "headers"
	}
	if *connectOAuth && resolvedAuthType != "oauth" {
		return errors.New("--connect requires --auth-type oauth")
	}
	options := damanaged.MCPServerOptions{Headers: headers, AuthType: resolvedAuthType}
	if *authType == "oauth" {
		options.OAuthMode = "per_user_dynamic_client"
	}
	server, err := client.CreateMCPServer(ctx, *name, rawURL, options)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Created mcp_server %s: %s → %s\n", safeCLIValue(server["id"]), safeCLIValue(server["name"]), safeCLIValue(server["url"])); err != nil {
		return err
	}
	if *connectOAuth {
		serverID, _ := server["id"].(string)
		if serverID == "" {
			return errors.New("created OAuth MCP server returned no server ID")
		}
		if err := connectMCPServerOAuth(ctx, client, serverID, scopes, *forceNew, *timeoutSeconds, *noBrowser, stdout, openManagedURL); err != nil {
			return err
		}
	}
	if !*noTools && resolvedAuthType != "oauth" {
		if err := printMCPTools(ctx, client, server, stdout); err != nil {
			_, _ = fmt.Fprintf(stdout, "Run `dago mcp-servers tools %s` after the server is ready.\n", safeCLIValue(server["id"]))
		}
	}
	return nil
}

func runMCPUpdate(ctx context.Context, client managedMCPRegistry, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dago mcp-servers update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rawURL := flags.String("url", "", "replacement HTTPS MCP server URL")
	authType := flags.String("auth-type", "", "replacement auth type (headers)")
	clearHeaders := flags.Bool("clear-headers", false, "remove all stored headers")
	var rawHeaders stringList
	flags.Var(&rawHeaders, "header", "replacement KEY=VALUE header; repeat for multiple")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: dago mcp-servers update [options] ID|NAME|URL")
	}
	if *clearHeaders && len(rawHeaders) != 0 {
		return errors.New("use either --header or --clear-headers")
	}
	patch := damanaged.MCPServerPatch{}
	if *rawURL != "" {
		patch.URL = rawURL
	}
	if *authType != "" {
		patch.AuthType = authType
	}
	if len(rawHeaders) != 0 || *clearHeaders {
		headers, err := parseMCPHeaders(rawHeaders)
		if err != nil {
			return err
		}
		patch.Headers = &headers
	}
	serverID, err := resolveMCPServerID(ctx, client, flags.Arg(0))
	if err != nil {
		return err
	}
	server, err := client.UpdateMCPServer(ctx, serverID, patch)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Updated mcp_server %s: %s → %s\n", safeCLIValue(server["id"]), safeCLIValue(server["name"]), safeCLIValue(server["url"]))
	return err
}

func runMCPDelete(ctx context.Context, client managedMCPRegistry, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("dago mcp-servers delete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "delete without interactive confirmation")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: dago mcp-servers delete [--yes] ID|NAME|URL")
	}
	serverID, err := resolveMCPServerID(ctx, client, flags.Arg(0))
	if err != nil {
		return err
	}
	if !*yes {
		if _, err := fmt.Fprintf(stdout, "Delete MCP server %s? [y/N]: ", safeCLIValue(serverID)); err != nil {
			return err
		}
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 64), 1024)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(stdout, "Aborted.")
			return nil
		}
		answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if answer != "y" && answer != "yes" {
			_, _ = fmt.Fprintln(stdout, "Aborted.")
			return nil
		}
	}
	if err := client.DeleteMCPServer(ctx, serverID); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Deleted %s\n", safeCLIValue(serverID))
	return err
}

func resolveMCPServerID(ctx context.Context, client managedMCPRegistry, identifier string) (string, error) {
	candidate := strings.TrimSpace(identifier)
	if mcpUUID.MatchString(candidate) {
		return candidate, nil
	}
	servers, err := client.ListMCPServers(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, server := range servers {
		if name, _ := server["name"].(string); name == identifier {
			if id, _ := server["id"].(string); id != "" {
				matches = append(matches, id)
			}
		}
	}
	if len(matches) == 0 {
		target := normalizedMCPIdentifier(identifier)
		for _, server := range servers {
			if rawURL, _ := server["url"].(string); normalizedMCPIdentifier(rawURL) == target {
				if id, _ := server["id"].(string); id != "" {
					matches = append(matches, id)
				}
			}
		}
	}
	sort.Strings(matches)
	matches = compactStrings(matches)
	if len(matches) == 0 {
		return "", fmt.Errorf("no MCP server matches %q", safeCLIValue(identifier))
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("MCP server identifier %q is ambiguous", safeCLIValue(identifier))
	}
	return matches[0], nil
}

func normalizedMCPIdentifier(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" {
		return strings.TrimRight(strings.ToLower(strings.TrimSpace(value)), "/")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func parseMCPHeaders(raw []string) ([]damanaged.MCPHeader, error) {
	headers := make([]damanaged.MCPHeader, 0, len(raw))
	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, errors.New("--header must use KEY=VALUE without control characters")
		}
		headers = append(headers, damanaged.MCPHeader{Key: strings.TrimSpace(key), Value: value})
	}
	return headers, nil
}

func printMCPTools(ctx context.Context, client managedMCPRegistry, server damanaged.MCPServer, stdout io.Writer) error {
	rawURL, _ := server["url"].(string)
	if rawURL == "" {
		return errors.New("MCP server record has no URL")
	}
	providerID, _ := server["oauth_provider_id"].(string)
	tools, err := client.ListMCPServerTools(ctx, rawURL, providerID)
	if err != nil {
		return err
	}
	name, _ := server["name"].(string)
	if len(tools) == 0 {
		_, err := fmt.Fprintf(stdout, "No tools found for %s.\n", safeCLIValue(name))
		return err
	}
	entries := make([]map[string]string, 0, len(tools))
	for _, tool := range tools {
		toolName, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		if _, err := fmt.Fprintf(stdout, "  %s\t%s\n", safeCLIValue(toolName), safeCLIValue(strings.Split(description, "\n")[0])); err != nil {
			return err
		}
		entry := map[string]string{"name": toolName, "mcp_server_url": rawURL, "display_name": toolName}
		if name != "" {
			entry["mcp_server_name"] = name
		}
		entries = append(entries, entry)
	}
	if _, err := fmt.Fprintln(stdout, "\nAdd to tools.json:"); err != nil {
		return err
	}
	return writeJSON(stdout, map[string]any{"tools": entries, "interrupt_config": map[string]any{}})
}

func redactMCPServer(server damanaged.MCPServer) damanaged.MCPServer {
	copy := make(damanaged.MCPServer, len(server))
	for key, value := range server {
		copy[key] = value
	}
	if headers, ok := copy["headers"].([]any); ok {
		redacted := make([]any, len(headers))
		for index, value := range headers {
			if header, ok := value.(map[string]any); ok {
				item := make(map[string]any, len(header))
				for key, field := range header {
					item[key] = field
				}
				if _, exists := item["value"]; exists {
					item["value"] = "***"
				}
				redacted[index] = item
			} else {
				redacted[index] = value
			}
		}
		copy["headers"] = redacted
	}
	return copy
}

func safeCLIValue(value any) string {
	if value == nil {
		return ""
	}
	return unicodesecurity.RenderTerminalSafe(fmt.Sprint(value))
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
