package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semistrict/dago/dacredential"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type authCommandOptions struct {
	path         string
	stateDir     string
	fromEnv      string
	baseURL      string
	project      string
	json         bool
	help         bool
	baseURLSet   bool
	projectSet   bool
	fromEnvSet   bool
	explicitPath bool
}

type authStatusRow struct {
	Provider    string `json:"provider"`
	Status      string `json:"status"`
	Environment string `json:"environment,omitempty"`
	Type        string `json:"type,omitempty"`
	Service     bool   `json:"service,omitempty"`
}

func runAuthCommand(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	options, positionals, err := parseAuthArguments(arguments)
	if err != nil {
		return &commandExitError{code: 2, err: err}
	}
	if options.help || len(positionals) == 0 {
		printAuthUsage(stdout)
		return nil
	}
	command := strings.ToLower(positionals[0])
	positionals = positionals[1:]
	if command == "ls" {
		command = "list"
	}
	if command == "rm" || command == "delete" {
		command = "remove"
	}
	if err := validateAuthOptions(command, positionals, options); err != nil {
		return &commandExitError{code: 2, err: err}
	}
	path, err := authStorePath(options.path)
	if err != nil {
		return err
	}
	store := dacredential.NewStore(path, time.Now, dacredential.Options{})
	oauthPath := ""
	if command == "list" || ((command == "status" || command == "remove") && positionals[0] == "openai_oauth") {
		oauthPath, err = authOAuthPath(options.stateDir)
		if err != nil {
			return err
		}
	}
	switch command {
	case "list":
		return runAuthList(ctx, store, oauthPath, options.json, stdout)
	case "status":
		return runAuthStatus(ctx, store, oauthPath, positionals[0], options.json, stdout)
	case "path":
		return runAuthPath(store, options.json, stdout)
	case "set":
		return runAuthSet(ctx, store, positionals[0], options, stdin, stdout)
	case "remove":
		return runAuthRemove(ctx, store, oauthPath, positionals[0], options.json, stdout)
	default:
		return &commandExitError{code: 2, err: fmt.Errorf("unknown auth command %q", command)}
	}
}

func authOAuthPath(explicitStateDirectory string) (string, error) {
	directory := explicitStateDirectory
	if directory == "" {
		configured, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve config directory: %w", err)
		}
		directory = filepath.Join(configured, "dacode")
	}
	if len(directory) > 4096 || strings.ContainsRune(directory, 0) {
		return "", errors.New("auth state directory is invalid")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve auth state directory: %w", err)
	}
	return filepath.Join(absolute, oauthStoreFilename), nil
}

func authStorePath(explicit string) (string, error) {
	if explicit == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return dacredential.DefaultPath(home), nil
	}
	if len(explicit) > 4096 || strings.ContainsRune(explicit, 0) {
		return "", errors.New("auth file path is invalid")
	}
	absolute, err := filepath.Abs(explicit)
	if err != nil {
		return "", fmt.Errorf("resolve auth file path: %w", err)
	}
	return absolute, nil
}

func runAuthList(ctx context.Context, store *dacredential.Store, oauthPath string, asJSON bool, output io.Writer) error {
	snapshot, err := store.Load(ctx)
	if err != nil {
		return err
	}
	providers := dacredential.Providers()
	known := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		known[provider.Name] = struct{}{}
	}
	for _, name := range snapshot.Providers() {
		if _, exists := known[name]; !exists {
			providers = append(providers, dacredential.Provider{Name: name})
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Name < providers[j].Name })
	rows := make([]authStatusRow, 0, len(providers))
	for _, provider := range providers {
		resolution, resolveErr := store.Resolve(ctx, provider.Name, os.LookupEnv)
		if resolveErr != nil {
			return resolveErr
		}
		row := authRow(provider, resolution)
		if provider.Name == "openai_oauth" && resolution.Source == dacredential.MissingSource {
			stored, statusErr := storedOAuthSession(oauthPath)
			if statusErr != nil {
				return statusErr
			}
			if stored {
				row.Status, row.Type = "stored", string(dacredential.OAuthType)
			}
		}
		rows = append(rows, row)
	}
	return writeAuthRows(output, "auth list", rows, asJSON)
}

func runAuthStatus(ctx context.Context, store *dacredential.Store, oauthPath, providerName string, asJSON bool, output io.Writer) error {
	resolution, err := store.Resolve(ctx, providerName, os.LookupEnv)
	if err != nil {
		return err
	}
	provider, _ := dacredential.ProviderByName(providerName)
	provider.Name = providerName
	row := authRow(provider, resolution)
	if providerName == "openai_oauth" && resolution.Source == dacredential.MissingSource {
		stored, statusErr := storedOAuthSession(oauthPath)
		if statusErr != nil {
			return statusErr
		}
		if stored {
			row.Status, row.Type = "stored", string(dacredential.OAuthType)
		}
	}
	return writeAuthRows(output, "auth status", []authStatusRow{row}, asJSON)
}

func storedOAuthSession(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect stored OpenAI sign-in: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > dacredential.DefaultOptions().MaxFileBytes {
		return false, errors.New("stored OpenAI sign-in is not a bounded regular file")
	}
	return true, nil
}

func authRow(provider dacredential.Provider, resolution dacredential.Resolution) authStatusRow {
	row := authStatusRow{Provider: provider.Name, Service: provider.Service}
	switch resolution.Source {
	case dacredential.StoredSource:
		row.Status = "stored"
		row.Type = string(resolution.Credential.Type)
	case dacredential.EnvironmentSource:
		row.Status = "env: " + resolution.Environment
		row.Environment = resolution.Environment
	default:
		row.Status = "missing"
		row.Environment = resolution.Environment
	}
	return row
}

func writeAuthRows(output io.Writer, command string, rows []authStatusRow, asJSON bool) error {
	if asJSON {
		return writeConfigJSON(output, command, rows)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(output, "%s\t%s\n", row.Provider, row.Status); err != nil {
			return err
		}
	}
	return nil
}

func runAuthPath(store *dacredential.Store, asJSON bool, output io.Writer) error {
	_, err := os.Lstat(store.Path())
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect auth file: %w", err)
	}
	if asJSON {
		return writeConfigJSON(output, "auth path", map[string]any{"path": store.Path(), "exists": exists})
	}
	_, err = fmt.Fprintln(output, unicodesecurity.RenderTerminalSafe(store.Path()))
	return err
}

func runAuthSet(ctx context.Context, store *dacredential.Store, providerName string, options authCommandOptions, input io.Reader, output io.Writer) error {
	provider, known := dacredential.ProviderByName(providerName)
	if known && provider.OAuthOnly {
		return fmt.Errorf("%s uses OAuth and cannot be set with an API key", providerName)
	}
	if options.projectSet && providerName != "langsmith" {
		return errors.New("--project is only valid for langsmith")
	}
	key, err := readAuthSecret(ctx, options, input)
	if err != nil {
		return err
	}
	baseURL, project := options.baseURL, options.project
	if options.baseURLSet && providerName == "langsmith" {
		switch strings.ToLower(strings.TrimSpace(baseURL)) {
		case "us":
			baseURL = "https://api.smith.langchain.com"
		case "eu":
			baseURL = "https://eu.api.smith.langchain.com"
		}
	}
	if !options.baseURLSet || !options.projectSet {
		snapshot, loadErr := store.Load(ctx)
		if loadErr != nil {
			return loadErr
		}
		if current, exists := snapshot.APIKey(providerName); exists {
			if !options.baseURLSet {
				baseURL = current.BaseURL
			}
			if !options.projectSet {
				project = current.Project
			}
		}
	}
	if err := store.SetAPIKey(ctx, providerName, key, baseURL, project); err != nil {
		return err
	}
	if options.json {
		return writeConfigJSON(output, "auth set", map[string]any{"provider": providerName, "stored": true})
	}
	_, err = fmt.Fprintf(output, "Stored credential for %s.\n", providerName)
	return err
}

func readAuthSecret(ctx context.Context, options authCommandOptions, input io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if options.fromEnvSet {
		value, exists := os.LookupEnv(options.fromEnv)
		if !exists || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("environment variable %s is not set or is empty", options.fromEnv)
		}
		return strings.TrimSpace(value), nil
	}
	if input == nil {
		panic("dacode: auth input is required")
	}
	if file, ok := input.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("refusing to read an API key from an interactive terminal; pipe it on stdin or use --from-env")
		}
	}
	limit := int64(dacredential.DefaultOptions().MaxSecretBytes + 1)
	payload, err := io.ReadAll(io.LimitReader(input, limit))
	if err != nil {
		return "", errors.New("read API key from stdin")
	}
	if int64(len(payload)) >= limit {
		return "", errors.New("API key from stdin exceeds the configured bound")
	}
	value := strings.TrimSpace(string(payload))
	if value == "" {
		return "", errors.New("no API key received on stdin")
	}
	return value, nil
}

func runAuthRemove(ctx context.Context, store *dacredential.Store, oauthPath, providerName string, asJSON bool, output io.Writer) error {
	removed, err := store.Remove(ctx, providerName)
	if err != nil {
		return err
	}
	if providerName == "openai_oauth" {
		oauthStored, statusErr := storedOAuthSession(oauthPath)
		if statusErr != nil {
			return statusErr
		}
		if oauthStored {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(oauthPath); err != nil {
				return fmt.Errorf("remove stored OpenAI sign-in: %w", err)
			}
			removed = true
		}
	}
	if asJSON {
		return writeConfigJSON(output, "auth remove", map[string]any{"provider": providerName, "removed": removed})
	}
	verb := "No stored credential for"
	if removed {
		verb = "Removed stored credential for"
	}
	_, err = fmt.Fprintf(output, "%s %s.\n", verb, providerName)
	return err
}

func parseAuthArguments(arguments []string) (authCommandOptions, []string, error) {
	var options authCommandOptions
	var positionals []string
	seen := map[string]bool{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--json":
			options.json = true
		case argument == "--help" || argument == "-h":
			options.help = true
		case argument == "--auth-file" || argument == "--state-dir" || argument == "--from-env" || argument == "--base-url" || argument == "--project":
			if seen[argument] {
				return authCommandOptions{}, nil, fmt.Errorf("%s may be specified once", argument)
			}
			seen[argument] = true
			index++
			if index >= len(arguments) {
				return authCommandOptions{}, nil, fmt.Errorf("%s requires one value", argument)
			}
			value := arguments[index]
			switch argument {
			case "--auth-file":
				options.path, options.explicitPath = value, true
			case "--state-dir":
				options.stateDir = value
			case "--from-env":
				options.fromEnv, options.fromEnvSet = value, true
			case "--base-url":
				options.baseURL, options.baseURLSet = value, true
			case "--project":
				options.project, options.projectSet = value, true
			}
		case strings.HasPrefix(argument, "--auth-file=") || strings.HasPrefix(argument, "--state-dir=") || strings.HasPrefix(argument, "--from-env=") || strings.HasPrefix(argument, "--base-url=") || strings.HasPrefix(argument, "--project="):
			name, value, _ := strings.Cut(argument, "=")
			if seen[name] {
				return authCommandOptions{}, nil, fmt.Errorf("%s may be specified once", name)
			}
			seen[name] = true
			switch name {
			case "--auth-file":
				options.path, options.explicitPath = value, true
			case "--state-dir":
				options.stateDir = value
			case "--from-env":
				options.fromEnv, options.fromEnvSet = value, true
			case "--base-url":
				options.baseURL, options.baseURLSet = value, true
			case "--project":
				options.project, options.projectSet = value, true
			}
		case strings.HasPrefix(argument, "-"):
			return authCommandOptions{}, nil, fmt.Errorf("unknown auth option %q", argument)
		default:
			positionals = append(positionals, strings.ToLower(argument))
		}
	}
	if options.explicitPath && options.path == "" {
		return authCommandOptions{}, nil, errors.New("--auth-file requires one non-empty path")
	}
	if seen["--state-dir"] && options.stateDir == "" {
		return authCommandOptions{}, nil, errors.New("--state-dir requires one non-empty path")
	}
	if options.fromEnvSet && !validEnvironmentName(options.fromEnv) {
		return authCommandOptions{}, nil, errors.New("--from-env requires a valid environment variable name")
	}
	return options, positionals, nil
}

func validateAuthOptions(command string, positionals []string, options authCommandOptions) error {
	expected := map[string]int{"list": 0, "path": 0, "set": 1, "remove": 1, "status": 1}
	count, known := expected[command]
	if !known {
		return fmt.Errorf("unknown auth command %q", command)
	}
	if len(positionals) != count {
		return fmt.Errorf("auth %s requires %d provider arguments", command, count)
	}
	if command != "set" && (options.fromEnvSet || options.baseURLSet || options.projectSet) {
		return fmt.Errorf("auth %s does not accept set options", command)
	}
	if options.stateDir != "" && command != "list" && !((command == "status" || command == "remove") && positionals[0] == "openai_oauth") {
		return fmt.Errorf("--state-dir is only valid for auth list or OpenAI subscription status/remove")
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '_' || (index > 0 && char >= '0' && char <= '9') {
			continue
		}
		return false
	}
	return true
}

func printAuthUsage(output io.Writer) {
	fmt.Fprintln(output, "Usage: dacode auth list [--json] [--auth-file PATH] [--state-dir PATH]")
	fmt.Fprintln(output, "       dacode auth set PROVIDER [--from-env VAR] [--base-url URL] [--project NAME] [--json] [--auth-file PATH]")
	fmt.Fprintln(output, "       dacode auth remove PROVIDER [--json] [--auth-file PATH] [--state-dir PATH]")
	fmt.Fprintln(output, "       dacode auth status PROVIDER [--json] [--auth-file PATH] [--state-dir PATH]")
	fmt.Fprintln(output, "       dacode auth path [--json] [--auth-file PATH]")
}
