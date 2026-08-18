package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semistrict/dago/daplugin"
	"github.com/semistrict/dago/daweb"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type pluginCLIOptions struct {
	json  bool
	store string
}

func runPluginCommand(ctx context.Context, arguments []string, stdout io.Writer) error {
	options, positionals, err := parsePluginArguments(arguments)
	if err != nil {
		return err
	}
	if options.store == "" {
		configDir, configErr := os.UserConfigDir()
		if configErr != nil {
			return configErr
		}
		options.store = filepath.Join(configDir, "dacode", "plugins")
	}
	store := daplugin.NewStore(options.store, daplugin.StoreOptions{})
	materializer := daplugin.NewSecureMaterializer(daweb.NewClient(daweb.Options{}), "", daplugin.MaterializerOptions{})
	manager := daplugin.NewManager(store, materializer, daplugin.ManagerOptions{})
	if len(positionals) == 0 {
		return errors.New("plugin requires list, install, uninstall, enable, disable, or marketplace")
	}
	command := positionals[0]
	positionals = positionals[1:]
	switch command {
	case "list", "ls":
		if len(positionals) != 0 {
			return errors.New("plugin list accepts no arguments")
		}
		plugins, warnings, err := manager.Discover(ctx)
		if err != nil {
			return err
		}
		if options.json {
			return writePluginJSON(stdout, map[string]any{"plugins": plugins, "warnings": warnings})
		}
		for _, plugin := range plugins {
			status := "disabled"
			if plugin.Enabled {
				status = "enabled"
			}
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%s\n", unicodesecurity.RenderTerminalSafe(plugin.ID), status, unicodesecurity.RenderTerminalSafe(plugin.Version)); err != nil {
				return err
			}
		}
		return nil
	case "install":
		if len(positionals) != 1 {
			return errors.New("plugin install requires name@marketplace")
		}
		plugin, err := manager.Install(ctx, positionals[0])
		if err != nil {
			return err
		}
		if options.json {
			return writePluginJSON(stdout, map[string]any{"installed": plugin, "reload_required": true})
		}
		_, err = fmt.Fprintf(stdout, "Installed %s. Reload required.\n", unicodesecurity.RenderTerminalSafe(plugin.ID))
		return err
	case "uninstall":
		if len(positionals) != 1 {
			return errors.New("plugin uninstall requires name@marketplace")
		}
		if err := manager.Uninstall(ctx, positionals[0]); err != nil {
			return err
		}
		return writePluginMutation(stdout, options.json, "uninstalled", positionals[0])
	case "enable":
		if len(positionals) != 1 {
			return errors.New("plugin enable requires name@marketplace")
		}
		if err := manager.Enable(ctx, positionals[0]); err != nil {
			return err
		}
		return writePluginMutation(stdout, options.json, "enabled", positionals[0])
	case "disable":
		if len(positionals) != 1 {
			return errors.New("plugin disable requires name@marketplace")
		}
		if err := manager.Disable(ctx, positionals[0]); err != nil {
			return err
		}
		return writePluginMutation(stdout, options.json, "disabled", positionals[0])
	case "marketplace":
		return runPluginMarketplace(ctx, manager, store, positionals, options, stdout)
	default:
		return fmt.Errorf("unknown plugin command %q", command)
	}
}

func parsePluginArguments(arguments []string) (pluginCLIOptions, []string, error) {
	var options pluginCLIOptions
	var positionals []string
	for index := 0; index < len(arguments); index++ {
		switch arguments[index] {
		case "--json":
			options.json = true
		case "--store":
			index++
			if index >= len(arguments) || options.store != "" {
				return pluginCLIOptions{}, nil, errors.New("--store requires one path")
			}
			options.store = arguments[index]
		default:
			if strings.HasPrefix(arguments[index], "-") {
				return pluginCLIOptions{}, nil, fmt.Errorf("unknown plugin option %q", arguments[index])
			}
			positionals = append(positionals, arguments[index])
		}
	}
	return options, positionals, nil
}

func runPluginMarketplace(ctx context.Context, manager *daplugin.Manager, store *daplugin.Store, args []string, options pluginCLIOptions, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("plugin marketplace requires list, add, or remove")
	}
	switch args[0] {
	case "list", "ls":
		if len(args) != 1 {
			return errors.New("plugin marketplace list accepts no arguments")
		}
		state, err := store.Load(ctx)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(state.Marketplaces))
		for name := range state.Marketplaces {
			names = append(names, name)
		}
		sort.Strings(names)
		if options.json {
			return writePluginJSON(stdout, map[string]any{"marketplaces": state.Marketplaces})
		}
		for _, name := range names {
			record := state.Marketplaces[name]
			if _, err := fmt.Fprintf(stdout, "%s\t%s\n", unicodesecurity.RenderTerminalSafe(name), record.Source.Type); err != nil {
				return err
			}
		}
		return nil
	case "add":
		if len(args) != 2 {
			return errors.New("plugin marketplace add requires one source")
		}
		source, err := daplugin.ParseMarketplaceSource(args[1])
		if err != nil {
			return err
		}
		market, err := manager.AddMarketplace(ctx, source)
		if err != nil {
			return err
		}
		if options.json {
			return writePluginJSON(stdout, map[string]any{"added": market})
		}
		_, err = fmt.Fprintf(stdout, "Added marketplace %s (%d plugins).\n", unicodesecurity.RenderTerminalSafe(market.Name), len(market.Plugins))
		return err
	case "remove":
		if len(args) != 2 {
			return errors.New("plugin marketplace remove requires one name")
		}
		if err := manager.RemoveMarketplace(ctx, args[1]); err != nil {
			return err
		}
		return writePluginMutation(stdout, options.json, "marketplace_removed", args[1])
	default:
		return fmt.Errorf("unknown plugin marketplace command %q", args[0])
	}
}

func writePluginMutation(output io.Writer, jsonOutput bool, action, id string) error {
	if jsonOutput {
		return writePluginJSON(output, map[string]any{"action": action, "id": id, "reload_required": true})
	}
	_, err := fmt.Fprintf(output, "%s %s. Reload required.\n", strings.ReplaceAll(action, "_", " "), unicodesecurity.RenderTerminalSafe(id))
	return err
}
func writePluginJSON(output io.Writer, data any) error {
	return json.NewEncoder(output).Encode(map[string]any{"version": 1, "command": "plugin", "data": data})
}
