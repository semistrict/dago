package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/daconfig"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const defaultConfigFilename = "config.json"

var cliConfigManifest = daconfig.NewManifest(
	daconfig.Option{Key: "models.default", Group: "Models", Summary: "Default model used for new runs", Kind: daconfig.KindString, Environment: "OPENAI_MODEL", Default: "", Persist: true},
	daconfig.Option{Key: "models.recent", Group: "Models", Summary: "Most recently selected model", Kind: daconfig.KindString, Default: "", Persist: true},
	daconfig.Option{Key: "models.approval", Group: "Models", Summary: "Optional separate model used for automatic approval reviews; unset reuses the main model", Kind: daconfig.KindString, Environment: "DACODE_APPROVAL_MODEL", Persist: true},
	daconfig.Option{Key: "models.base_url", Group: "Models", Summary: "OpenAI-compatible API base URL", Kind: daconfig.KindString, Environment: "OPENAI_BASE_URL", Persist: true},
	daconfig.Option{Key: "credentials.openai", Group: "Credentials", Summary: "OpenAI-compatible API credential", Kind: daconfig.KindString, Environment: "OPENAI_API_KEY", Redacted: true},
	daconfig.Option{Key: "runtime.recursion_limit", Group: "Runtime", Summary: "Maximum graph steps per agent turn", Kind: daconfig.KindInt, Environment: "DACODE_RECURSION_LIMIT", Default: defaultRecursionLimit, Minimum: 1, Maximum: 100000, Persist: true},
	daconfig.Option{Key: "runtime.state_dir", Group: "Runtime", Summary: "Directory for private local state", Kind: daconfig.KindString, Environment: "DACODE_STATE_DIR", Persist: true},
	daconfig.Option{Key: "threads.compact_on_resume_threshold", Group: "Threads", Summary: "Prompt to compact resumed threads above this context-token count; zero disables", Kind: daconfig.KindInt, Default: defaultCompactResumeThreshold, Minimum: 0, Maximum: 10_000_000, Persist: true},
	daconfig.Option{Key: "memory.auto_save", Group: "Memory", Summary: "Allow the agent to update durable memory", Kind: daconfig.KindBool, Environment: "MEMORY_AUTO_SAVE", Default: true, Persist: true},
	daconfig.Option{Key: "security.shell_allow_list", Group: "Security", Summary: "Comma-separated inline shell allow-list", Kind: daconfig.KindString, Environment: "DACODE_SHELL_ALLOW_LIST", Persist: true},
	daconfig.Option{Key: "startup.onboarding", Group: "Startup", Summary: "Force or suppress first-run onboarding; unset follows the completion marker", Kind: daconfig.KindBool, Environment: onboardingEnvironment, Persist: true},
	daconfig.Option{Key: "startup.command", Group: "Startup", Summary: "Local command run before the first prompt", Kind: daconfig.KindString, Environment: "DACODE_STARTUP_COMMAND", Persist: true},
	daconfig.Option{Key: "sandboxes.default", Group: "Sandboxes", Summary: "Provider selected by an explicit bare --sandbox flag", Kind: daconfig.KindString, Environment: "DACODE_SANDBOX_DEFAULT", Persist: true},
	daconfig.Option{Key: "agents.default", Group: "Agents", Summary: "Named agent selected for new runs", Kind: daconfig.KindString, Environment: "DACODE_DEFAULT_AGENT", Persist: true},
	daconfig.Option{Key: "agents.recent", Group: "Agents", Summary: "Most recently selected named agent", Kind: daconfig.KindString, Environment: "DACODE_RECENT_AGENT", Persist: true},
)

type resolvedCLIConfig struct {
	snapshot daconfig.Snapshot
	store    *daconfig.Store
}

func defaultConfigPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	return filepath.Join(directory, "dacode", defaultConfigFilename), nil
}

func configuredPath(explicit string) (string, error) {
	path := explicit
	if path == "" {
		for _, name := range []string{"DACODE_CONFIG", daconfig.CodePrefix + "DACODE_CONFIG", daconfig.CLIPrefix + "DACODE_CONFIG"} {
			if value, ok := os.LookupEnv(name); ok {
				path = value
			}
		}
	}
	if path == "" {
		return defaultConfigPath()
	}
	if len(path) > 4096 || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("config path is invalid")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	return absolute, nil
}

func resolveCLIConfig(ctx context.Context, explicitPath string) (resolvedCLIConfig, error) {
	path, err := configuredPath(explicitPath)
	if err != nil {
		return resolvedCLIConfig{}, err
	}
	store := daconfig.NewStore(cliConfigManifest, path, daconfig.StoreOptions{})
	layer, err := store.Load(ctx)
	if err != nil {
		return resolvedCLIConfig{}, err
	}
	resolver := daconfig.NewResolver(cliConfigManifest, os.LookupEnv, daconfig.ResolverOptions{})
	snapshot, err := resolver.Resolve([]daconfig.Layer{layer}, daconfig.Layer{})
	if err != nil {
		return resolvedCLIConfig{}, err
	}
	return resolvedCLIConfig{snapshot: snapshot, store: store}, nil
}

func applyResolvedCLIConfig(options *cliOptions, rawShellAllowList *string, explicit map[string]bool, config resolvedCLIConfig) {
	setString := func(target *string, key string, flags ...string) {
		for _, flag := range flags {
			if explicit[flag] {
				return
			}
		}
		*target = config.snapshot.String(key)
	}
	if !explicit["model"] && !explicit["M"] {
		defaultEntries := config.snapshot.Select("models.default")
		if len(defaultEntries) == 1 && defaultEntries[0].Set {
			options.model = config.snapshot.String("models.default")
		} else if recent := config.snapshot.String("models.recent"); recent != "" {
			options.model = recent
		}
	}
	setString(&options.approvalModel, "models.approval", "approval-model")
	setString(&options.baseURL, "models.base_url")
	setString(&options.apiKey, "credentials.openai")
	setString(&options.stateDir, "runtime.state_dir", "state-dir")
	setString(&options.startupCommand, "startup.command", "startup-cmd")
	setString(&options.sandboxDefault, "sandboxes.default")
	setString(&options.defaultAgent, "agents.default")
	setString(&options.recentAgent, "agents.recent")
	setString(rawShellAllowList, "security.shell_allow_list", "shell-allow-list", "S")
	if !explicit["recursion-limit"] {
		options.recursionLimit = config.snapshot.Int("runtime.recursion_limit")
	}
	if !explicit["memory-auto-save"] {
		options.memoryAutoSave = config.snapshot.Bool("memory.auto_save")
	}
}

func runConfigCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	options, positionals, err := parseConfigArguments(arguments)
	if err != nil {
		return err
	}
	config, err := resolveCLIConfig(ctx, options.path)
	if err != nil {
		return err
	}
	command := "show"
	if len(positionals) > 0 {
		command = strings.ToLower(positionals[0])
		positionals = positionals[1:]
	}
	switch command {
	case "show":
		if len(positionals) != 0 {
			return fmt.Errorf("config show accepts no arguments")
		}
		return writeConfigEntries(stdout, "config", config.snapshot.Entries(), options, false)
	case "get":
		if len(positionals) != 1 {
			return &commandExitError{code: 2, err: errors.New("config get requires one option key or section")}
		}
		entries := config.snapshot.Select(positionals[0])
		if len(entries) == 0 {
			return fmt.Errorf("unknown config option or section %q", positionals[0])
		}
		_, exact := cliConfigManifest.Option(positionals[0])
		return writeConfigEntries(stdout, "config get", entries, options, exact)
	case "path":
		if len(positionals) != 0 {
			return fmt.Errorf("config path accepts no arguments")
		}
		_, statErr := os.Lstat(config.store.Path())
		exists := statErr == nil
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect config path: %w", statErr)
		}
		if options.json {
			return writeConfigJSON(stdout, "config path", map[string]any{"path": config.store.Path(), "exists": exists})
		}
		_, err := fmt.Fprintf(stdout, "%s\t%t\n", unicodesecurity.RenderTerminalSafe(config.store.Path()), exists)
		return err
	case "set":
		if len(positionals) != 2 {
			return &commandExitError{code: 2, err: errors.New("config set requires an option key and value")}
		}
		if err := config.store.Set(ctx, strings.ToLower(positionals[0]), positionals[1]); err != nil {
			return err
		}
		if options.json {
			return writeConfigJSON(stdout, "config set", map[string]any{"key": strings.ToLower(positionals[0]), "updated": true})
		}
		_, err := fmt.Fprintf(stdout, "Updated %s\n", unicodesecurity.RenderTerminalSafe(strings.ToLower(positionals[0])))
		return err
	case "unset":
		if len(positionals) != 1 {
			return &commandExitError{code: 2, err: errors.New("config unset requires one option key")}
		}
		removed, err := config.store.Unset(ctx, strings.ToLower(positionals[0]))
		if err != nil {
			return err
		}
		if options.json {
			return writeConfigJSON(stdout, "config unset", map[string]any{"key": strings.ToLower(positionals[0]), "removed": removed})
		}
		_, err = fmt.Fprintf(stdout, "Unset %s\t%t\n", unicodesecurity.RenderTerminalSafe(strings.ToLower(positionals[0])), removed)
		return err
	default:
		return fmt.Errorf("unknown config command %q", command)
	}
}

type configCommandOptions struct {
	path    string
	json    bool
	verbose bool
}

func parseConfigArguments(arguments []string) (configCommandOptions, []string, error) {
	var options configCommandOptions
	positionals := []string{}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--json":
			options.json = true
		case argument == "--verbose" || argument == "--all" || argument == "-v":
			options.verbose = true
		case argument == "--config":
			index++
			if index >= len(arguments) || options.path != "" {
				return configCommandOptions{}, nil, errors.New("--config requires one path and may be specified once")
			}
			options.path = arguments[index]
		case strings.HasPrefix(argument, "--config="):
			if options.path != "" || strings.TrimPrefix(argument, "--config=") == "" {
				return configCommandOptions{}, nil, errors.New("--config requires one path and may be specified once")
			}
			options.path = strings.TrimPrefix(argument, "--config=")
		case strings.HasPrefix(argument, "-"):
			return configCommandOptions{}, nil, fmt.Errorf("unknown config option %q", argument)
		default:
			positionals = append(positionals, argument)
		}
	}
	return options, positionals, nil
}

func writeConfigEntries(output io.Writer, command string, entries []daconfig.Entry, options configCommandOptions, single bool) error {
	if options.json {
		rows := make([]map[string]any, len(entries))
		for index, entry := range entries {
			rows[index] = configJSONRow(entry, options.verbose)
		}
		if single {
			return writeConfigJSON(output, command, rows[0])
		}
		return writeConfigJSON(output, command, rows)
	}
	groups := map[string][]daconfig.Entry{}
	var order []string
	for _, entry := range entries {
		if _, exists := groups[entry.Group]; !exists {
			order = append(order, entry.Group)
		}
		groups[entry.Group] = append(groups[entry.Group], entry)
	}
	for _, group := range order {
		if _, err := fmt.Fprintln(output, group); err != nil {
			return err
		}
		for _, entry := range groups[group] {
			value := configDisplayValue(entry)
			if _, err := fmt.Fprintf(output, "  %s = %s\t(%s)\n", entry.Key, value, unicodesecurity.RenderTerminalSafe(entry.Source)); err != nil {
				return err
			}
			if options.verbose {
				if _, err := fmt.Fprintf(output, "    %s\n", unicodesecurity.RenderTerminalSafe(entry.Summary)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func configDisplayValue(entry daconfig.Entry) string {
	if entry.Redacted {
		if entry.Set {
			return "<set>"
		}
		return "<unset>"
	}
	return unicodesecurity.RenderTerminalSafe(fmt.Sprint(entry.Value))
}

func configJSONRow(entry daconfig.Entry, verbose bool) map[string]any {
	row := map[string]any{"key": entry.Key, "source": entry.Source, "set": entry.Set, "redacted": entry.Redacted, "value": entry.Value}
	if verbose {
		row["group"], row["kind"], row["summary"] = entry.Group, entry.Kind, entry.Summary
	}
	return row
}

func writeConfigJSON(output io.Writer, command string, data any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(true)
	return encoder.Encode(map[string]any{"version": 1, "command": command, "data": data})
}

func serverConfigFor(options cliOptions, interactive bool) daconfig.ServerConfig {
	return daconfig.ServerConfig{
		Model: options.model, WorkingDirectory: options.workingDir, StateDirectory: options.stateDir,
		RecursionLimit: options.recursionLimit, MemoryReadOnly: !options.memoryAutoSave,
		NonInteractive: !interactive, ShellAllowList: append([]string(nil), options.shellAllowList.commands...),
	}
}
