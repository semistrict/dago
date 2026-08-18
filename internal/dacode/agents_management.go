package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/internal/unicodesecurity"
)

type agentPreference struct {
	filename string
	label    string
}

var recentAgentPreference = agentPreference{filename: recentAgentFilename, label: "recent agent"}

func readAgentPreference(stateDir string, preference agentPreference) (string, error) {
	root, err := os.OpenRoot(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open agent settings: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(preference.filename)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", preference.label, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s setting is not a regular file", preference.label)
	}
	file, err := root.Open(preference.filename)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", preference.label, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		if err == nil {
			err = errors.New("setting changed while opening")
		}
		return "", fmt.Errorf("inspect opened %s: %w", preference.label, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDefaultAgentBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", preference.label, err)
	}
	if len(data) > maxDefaultAgentBytes {
		return "", fmt.Errorf("%s setting exceeds %d bytes", preference.label, maxDefaultAgentBytes)
	}
	name := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if err := validateAgentName(name); err != nil {
		return "", fmt.Errorf("%s setting: %w", preference.label, err)
	}
	return name, nil
}

func writeAgentPreference(stateDir, name string, preference agentPreference) error {
	if err := validateAgentName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create agent settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(stateDir, ".agent-preference-*")
	if err != nil {
		return fmt.Errorf("create %s setting: %w", preference.label, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure %s setting: %w", preference.label, err)
	}
	if _, err := io.WriteString(temporary, name+"\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("write %s setting: %w", preference.label, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync %s setting: %w", preference.label, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s setting: %w", preference.label, err)
	}
	if err := replaceFileDurably(temporaryPath, filepath.Join(stateDir, preference.filename)); err != nil {
		return fmt.Errorf("replace %s setting: %w", preference.label, err)
	}
	return nil
}

func configuredRecentAgent(stateDir string) (string, error) {
	return readAgentPreference(stateDir, recentAgentPreference)
}

func agentAvailable(ctx context.Context, stateDir, name string) (bool, error) {
	agents, err := discoverAgents(ctx, stateDir, "")
	if err != nil {
		return false, err
	}
	for _, agent := range agents {
		if agent.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func resolveInitialAgent(ctx context.Context, stateDir, explicit string) (string, error) {
	return resolveInitialAgentConfigured(ctx, stateDir, explicit, "", "")
}

func resolveInitialAgentConfigured(ctx context.Context, stateDir, explicit, configuredDefault, configuredRecent string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if explicit != "" {
		if err := ensureAgentMemoryFile(stateDir, explicit); err != nil {
			return "", fmt.Errorf("select agent: %w", err)
		}
		if err := writeAgentPreference(stateDir, explicit, recentAgentPreference); err != nil {
			return "", err
		}
		return explicit, nil
	}
	candidates := []struct {
		name string
		load func(string) (string, error)
	}{
		{name: configuredDefault},
		{load: configuredDefaultAgent},
		{name: configuredRecent},
		{load: configuredRecentAgent},
	}
	for _, candidate := range candidates {
		name, err := candidate.name, error(nil)
		if candidate.load != nil {
			name, err = candidate.load(stateDir)
		}
		if err != nil {
			return "", err
		}
		if name == "" {
			continue
		}
		if err := validateAgentName(name); err != nil {
			return "", fmt.Errorf("configured agent: %w", err)
		}
		available, err := agentAvailable(ctx, stateDir, name)
		if err != nil {
			return "", err
		}
		if available {
			if err := writeAgentPreference(stateDir, name, recentAgentPreference); err != nil {
				return "", err
			}
			return name, nil
		}
	}
	if err := ensureAgentMemoryFile(stateDir, defaultAgentName); err != nil {
		return "", err
	}
	if err := writeAgentPreference(stateDir, defaultAgentName, recentAgentPreference); err != nil {
		return "", err
	}
	return defaultAgentName, nil
}

type agentResetResult struct {
	Agent  string `json:"agent"`
	Source string `json:"reset_to"`
	Path   string `json:"path"`
	DryRun bool   `json:"dry_run,omitempty"`
}

func resetAgentProfile(ctx context.Context, stateDir, name, source string, dryRun bool) (agentResetResult, error) {
	if err := ctx.Err(); err != nil {
		return agentResetResult{}, err
	}
	if err := validateAgentName(name); err != nil {
		return agentResetResult{}, err
	}
	content := ""
	sourceLabel := "default"
	if source != "" {
		if source == name {
			return agentResetResult{}, errors.New("reset source must differ from the destination agent")
		}
		var err error
		content, err = readAgentPromptForReset(ctx, stateDir, source)
		if err != nil {
			return agentResetResult{}, fmt.Errorf("load source agent: %w", err)
		}
		sourceLabel = source
	}
	result := agentResetResult{Agent: name, Source: sourceLabel, Path: filepath.Join(stateDir, name), DryRun: dryRun}
	if dryRun {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return agentResetResult{}, err
	}
	generation, err := newThreadID()
	if err != nil {
		return agentResetResult{}, fmt.Errorf("generate agent session namespace: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return agentResetResult{}, fmt.Errorf("create agent storage: %w", err)
	}
	staging, err := os.MkdirTemp(stateDir, ".agent-reset-*")
	if err != nil {
		return agentResetResult{}, fmt.Errorf("stage agent reset: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o700); err != nil {
		return agentResetResult{}, fmt.Errorf("secure staged agent: %w", err)
	}
	for _, child := range []string{agentSkillsDirectory, agentSessionsDirectory} {
		if err := os.Mkdir(filepath.Join(staging, child), 0o700); err != nil {
			return agentResetResult{}, fmt.Errorf("stage agent %s: %w", child, err)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, agentSessionsDirectory, agentSessionGeneration), []byte(generation+"\n"), 0o600); err != nil {
		return agentResetResult{}, fmt.Errorf("stage agent session namespace: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, agentInstructionsFilename), []byte(content), 0o600); err != nil {
		return agentResetResult{}, fmt.Errorf("stage agent instructions: %w", err)
	}
	target := filepath.Join(stateDir, name)
	if info, statErr := os.Lstat(target); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return agentResetResult{}, fmt.Errorf("agent %q path is not a confined directory", name)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return agentResetResult{}, fmt.Errorf("inspect agent %q: %w", name, statErr)
	}
	backup := filepath.Join(stateDir, ".agent-reset-backup-"+name)
	if _, err := os.Lstat(backup); err == nil {
		return agentResetResult{}, fmt.Errorf("agent reset backup already exists for %q", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return agentResetResult{}, fmt.Errorf("inspect agent reset backup: %w", err)
	}
	hadTarget := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return agentResetResult{}, fmt.Errorf("preserve existing agent: %w", err)
		}
		hadTarget = true
	}
	if err := os.Rename(staging, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		return agentResetResult{}, fmt.Errorf("activate reset agent: %w", err)
	}
	if hadTarget {
		if err := os.RemoveAll(backup); err != nil {
			return agentResetResult{}, fmt.Errorf("remove reset agent backup: %w", err)
		}
	}
	return result, nil
}

func readAgentGeneration(ctx context.Context, stateDir, name string) (string, error) {
	if err := validateAgentName(name); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return "", fmt.Errorf("open agent storage: %w", err)
	}
	defer root.Close()
	filename := filepath.Join(name, agentSessionsDirectory, agentSessionGeneration)
	info, err := root.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect agent %q session namespace: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("agent %q session namespace is not a regular file", name)
	}
	file, err := root.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open agent %q session namespace: %w", name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		if err == nil {
			err = errors.New("session namespace changed while opening")
		}
		return "", fmt.Errorf("inspect opened agent %q session namespace: %w", name, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAgentGenerationBytes+1))
	if err != nil {
		return "", fmt.Errorf("read agent %q session namespace: %w", name, err)
	}
	if len(data) > maxAgentGenerationBytes {
		return "", fmt.Errorf("agent %q session namespace exceeds %d bytes", name, maxAgentGenerationBytes)
	}
	value := strings.TrimSuffix(string(data), "\n")
	if len(value) != 24 {
		return "", fmt.Errorf("agent %q session namespace is invalid", name)
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return "", fmt.Errorf("agent %q session namespace is invalid", name)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return value, nil
}

func readAgentPromptForReset(ctx context.Context, stateDir, name string) (string, error) {
	if _, err := loadAgentInstructions(ctx, stateDir, name); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return "", fmt.Errorf("open agent storage: %w", err)
	}
	defer root.Close()
	filename := filepath.Join(name, agentInstructionsFilename)
	info, err := root.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("prompt is not a regular file")
		}
		return "", fmt.Errorf("inspect agent %q prompt: %w", name, err)
	}
	file, err := root.Open(filename)
	if err != nil {
		return "", fmt.Errorf("open agent %q prompt: %w", name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		if err == nil {
			err = errors.New("prompt changed while opening")
		}
		return "", fmt.Errorf("inspect opened agent %q prompt: %w", name, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAgentInstructionsBytes+1))
	if err != nil {
		return "", fmt.Errorf("read agent %q prompt: %w", name, err)
	}
	if len(data) > maxAgentInstructionsBytes {
		return "", fmt.Errorf("agent %q prompt exceeds %d bytes", name, maxAgentInstructionsBytes)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("agent %q prompt is not valid UTF-8", name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(data), nil
}

func runAgentsCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	command := "list"
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		command, arguments = arguments[0], arguments[1:]
	}
	var stateDir, configPath, name, source string
	var jsonOutput, dryRun bool
	flags := flag.NewFlagSet("dacode agents "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&stateDir, "state-dir", "", "state directory")
	flags.StringVar(&configPath, "config", "", "layered configuration file")
	flags.BoolVar(&jsonOutput, "json", false, "write versioned JSON")
	flags.BoolVar(&dryRun, "dry-run", false, "show reset without changing files")
	flags.StringVar(&name, "agent", "", "agent to reset")
	flags.StringVar(&source, "target", "", "agent prompt to copy")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	positionals := flags.Args()
	config, err := resolveCLIConfig(ctx, configPath)
	if err != nil {
		return err
	}
	if stateDir == "" {
		stateDir = config.snapshot.String("runtime.state_dir")
	}
	if stateDir == "" {
		stateDir = filepath.Dir(config.store.Path())
	}
	switch command {
	case "list", "ls":
		if len(positionals) != 0 || dryRun || name != "" || source != "" {
			return errors.New("agents list accepts no reset arguments")
		}
		agents, err := discoverAgents(ctx, stateDir, "")
		if err != nil {
			return err
		}
		defaultName := config.snapshot.String("agents.default")
		if defaultName != "" {
			if err := validateAgentName(defaultName); err != nil {
				return fmt.Errorf("configured default agent: %w", err)
			}
		}
		if defaultName == "" {
			defaultName, err = configuredDefaultAgent(stateDir)
			if err != nil {
				return err
			}
		}
		recent := config.snapshot.String("agents.recent")
		if recent != "" {
			if err := validateAgentName(recent); err != nil {
				return fmt.Errorf("configured recent agent: %w", err)
			}
		}
		if recent == "" {
			recent, err = configuredRecentAgent(stateDir)
			if err != nil {
				return err
			}
		}
		for index := range agents {
			agents[index].Default = agents[index].Name == defaultName
		}
		if jsonOutput {
			rows := make([]map[string]any, len(agents))
			for index, agent := range agents {
				rows[index] = map[string]any{"name": agent.Name, "path": filepath.Join(stateDir, agent.Name), "is_default": agent.Default, "is_recent": agent.Name == recent}
			}
			return json.NewEncoder(stdout).Encode(map[string]any{"version": 1, "command": "agents list", "result": rows})
		}
		if len(agents) == 0 {
			_, err = fmt.Fprintln(stdout, "No agents found.")
			return err
		}
		for _, agent := range agents {
			labels := ""
			if agent.Default {
				labels += " default"
			}
			if agent.Name == recent {
				labels += " recent"
			}
			if _, err := fmt.Fprintf(stdout, "%s\t%s%s\n", unicodesecurity.RenderTerminalSafe(agent.Name), unicodesecurity.RenderTerminalSafe(filepath.Join(stateDir, agent.Name)), labels); err != nil {
				return err
			}
		}
		return nil
	case "reset":
		if len(positionals) != 0 || name == "" {
			return errors.New("agents reset requires --agent NAME")
		}
		result, err := resetAgentProfile(ctx, stateDir, name, source, dryRun)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(map[string]any{"version": 1, "command": "agents reset", "result": result})
		}
		verb := "Reset"
		if dryRun {
			verb = "Would reset"
		}
		_, err = fmt.Fprintf(stdout, "%s agent %s to %s at %s\n", verb, unicodesecurity.RenderTerminalSafe(name), unicodesecurity.RenderTerminalSafe(result.Source), unicodesecurity.RenderTerminalSafe(result.Path))
		return err
	default:
		return fmt.Errorf("unknown agents command %q", command)
	}
}
