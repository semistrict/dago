package dacode

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/semistrict/dago/daplugin"
)

type pluginManagerPlugin struct {
	ID          string
	Name        string
	Description string
	Version     string
	Marketplace string
	Enabled     bool
	Installed   bool
	Loaded      bool
	Pending     bool
	Skills      int
	MCP         int
	Hooks       int
	Unsupported []string
}

type pluginManagerMarketplace struct {
	Name           string
	Source         string
	PluginCount    int
	InstalledCount int
	Error          bool
}

type pluginManagerSnapshot struct {
	Available    []pluginManagerPlugin
	Installed    []pluginManagerPlugin
	Marketplaces []pluginManagerMarketplace
	Warnings     []string
}

type pluginManagerService struct {
	store   *daplugin.Store
	manager *daplugin.Manager
}

// newPluginManagerService constructs the interactive store boundary without
// filesystem or network I/O. Remote authority is an explicit positional
// dependency; nil intentionally supports local marketplaces only.
func newPluginManagerService(storeRoot string, materializer daplugin.Materializer) *pluginManagerService {
	store := daplugin.NewStore(storeRoot, daplugin.StoreOptions{})
	return &pluginManagerService{store: store, manager: daplugin.NewManager(store, materializer, daplugin.ManagerOptions{})}
}

func (service *pluginManagerService) Snapshot(ctx context.Context, loadedIDs []string) (pluginManagerSnapshot, error) {
	if service == nil || service.store == nil || service.manager == nil {
		panic("dacode: plugin manager service is required")
	}
	state, err := service.store.Load(ctx)
	if err != nil {
		return pluginManagerSnapshot{}, err
	}
	installed, warnings, err := service.manager.Discover(ctx)
	if err != nil {
		return pluginManagerSnapshot{}, err
	}
	loaded := make(map[string]bool, len(loadedIDs))
	for _, id := range loadedIDs {
		loaded[id] = true
	}
	installedByID := make(map[string]pluginManagerPlugin, len(installed))
	for _, plugin := range installed {
		row := pluginManagerPlugin{
			ID: plugin.ID, Name: plugin.Name, Version: plugin.Version, Marketplace: plugin.Marketplace,
			Enabled: plugin.Enabled, Installed: true, Loaded: loaded[plugin.ID],
			Skills: len(plugin.Inventory.Skills), MCP: len(plugin.Inventory.MCPFiles), Hooks: len(plugin.Inventory.HookFiles),
			Unsupported: append([]string(nil), plugin.Inventory.Unsupported...),
		}
		if plugin.Manifest != nil {
			if plugin.Manifest.DisplayName != "" {
				row.Name = plugin.Manifest.DisplayName
			}
			row.MCP += len(plugin.Manifest.InlineMCP)
			if len(plugin.Manifest.InlineHooks) > 0 {
				row.Hooks++
			}
		}
		row.Pending = row.Enabled != row.Loaded
		installedByID[row.ID] = row
	}
	result := pluginManagerSnapshot{Warnings: stablePluginManagerWarnings(warnings)}
	for _, name := range sortedMarketplaceNames(state.Marketplaces) {
		if err := ctx.Err(); err != nil {
			return pluginManagerSnapshot{}, err
		}
		record := state.Marketplaces[name]
		marketplace, loadErr := daplugin.LoadMarketplace(record.InstallLocation)
		marketRow := pluginManagerMarketplace{Name: name, Source: string(record.Source.Type), Error: loadErr != nil}
		if loadErr != nil {
			result.Warnings = append(result.Warnings, "configured marketplace could not be loaded")
			result.Marketplaces = append(result.Marketplaces, marketRow)
			continue
		}
		marketRow.PluginCount = len(marketplace.Plugins)
		for _, entry := range marketplace.Plugins {
			id := entry.Name + "@" + marketplace.Name
			if installedRow, ok := installedByID[id]; ok {
				marketRow.InstalledCount++
				if installedRow.Description == "" {
					installedRow.Description = entry.Description
				}
				installedByID[id] = installedRow
				continue
			}
			label := entry.DisplayName
			if label == "" {
				label = entry.Name
			}
			result.Available = append(result.Available, pluginManagerPlugin{
				ID: id, Name: label, Description: entry.Description, Marketplace: marketplace.Name,
			})
		}
		result.Marketplaces = append(result.Marketplaces, marketRow)
	}
	for _, row := range installedByID {
		result.Installed = append(result.Installed, row)
	}
	sort.Slice(result.Available, func(i, j int) bool { return result.Available[i].ID < result.Available[j].ID })
	sort.Slice(result.Installed, func(i, j int) bool { return result.Installed[i].ID < result.Installed[j].ID })
	sort.Slice(result.Marketplaces, func(i, j int) bool { return result.Marketplaces[i].Name < result.Marketplaces[j].Name })
	result.Warnings = stablePluginManagerWarnings(result.Warnings)
	return result, nil
}

func (service *pluginManagerService) Install(ctx context.Context, id string) error {
	_, err := service.manager.Install(ctx, id)
	return err
}

func (service *pluginManagerService) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if enabled {
		return service.manager.Enable(ctx, id)
	}
	return service.manager.Disable(ctx, id)
}

func (service *pluginManagerService) Uninstall(ctx context.Context, id string) error {
	return service.manager.Uninstall(ctx, id)
}

func (service *pluginManagerService) AddMarketplace(ctx context.Context, source string) error {
	parsed, err := daplugin.ParseMarketplaceSource(source)
	if err != nil {
		return err
	}
	_, err = service.manager.AddMarketplace(ctx, parsed)
	return err
}

func (service *pluginManagerService) RemoveMarketplace(ctx context.Context, name string) error {
	return service.manager.RemoveMarketplace(ctx, name)
}

func stablePluginManagerWarnings(input []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(input))
	for _, warning := range input {
		warning = strings.TrimSpace(warning)
		if warning == "" {
			continue
		}
		// Discovery warnings are already bounded and never include component
		// contents. Reduce path- or URL-bearing variants to a stable class before
		// they reach terminal scrollback.
		if strings.ContainsAny(warning, `/\\`) || strings.Contains(warning, "://") {
			warning = "plugin discovery skipped an unsafe or invalid entry"
		}
		if !seen[warning] {
			seen[warning] = true
			result = append(result, warning)
		}
	}
	sort.Strings(result)
	return result
}

func pluginManagerDisplayError(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Plugin action cancelled."
	}
	return "Plugin action failed. Check the marketplace or plugin configuration."
}

func sortedMarketplaceNames(records map[string]daplugin.MarketplaceRecord) []string {
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
