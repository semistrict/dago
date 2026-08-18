package daplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Materializer grants explicit remote fetch authority. Implementations return
// local, immutable directories or files; Manager still validates every result.
type Materializer interface {
	Marketplace(context.Context, MarketplaceSource, string) (string, error)
	Plugin(context.Context, Marketplace, MarketplaceEntry, string) (MaterializedPlugin, error)
	Cleanup(context.Context, string) error
}

// ManagerOptions is retained for source compatibility. Manager currently has
// no optional construction policy, so callers may omit it.
type ManagerOptions struct{}
type Manager struct {
	store        *Store
	materializer Materializer
}

// NewManager constructs a manager without discovery, network, or filesystem I/O.
func NewManager(store *Store, materializer Materializer, compatibility ...ManagerOptions) *Manager {
	if store == nil {
		panic("daplugin: store is required")
	}
	if len(compatibility) > 1 {
		panic("daplugin: at most one compatibility options value is accepted")
	}
	if nilMaterializer(materializer) {
		materializer = nil
	}
	return &Manager{store: store, materializer: materializer}
}

func nilMaterializer(materializer Materializer) bool {
	if materializer == nil {
		return true
	}
	value := reflect.ValueOf(materializer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (manager *Manager) AddMarketplace(ctx context.Context, source MarketplaceSource) (Marketplace, error) {
	var location string
	remote := false
	switch source.Type {
	case SourceDirectory, SourceFile:
		location = source.Value
	default:
		remote = true
		if manager.materializer == nil {
			return Marketplace{}, errors.New("remote marketplace requires an explicit materializer")
		}
		var err error
		location, err = manager.materializer.Marketplace(ctx, source, filepath.Join(manager.store.root, "marketplaces"))
		if err != nil {
			return Marketplace{}, err
		}
	}
	marketplace, err := LoadMarketplace(location)
	if err != nil {
		if remote {
			_ = manager.materializer.Cleanup(context.Background(), location)
		}
		return Marketplace{}, err
	}
	state, err := manager.store.Load(ctx)
	if err != nil {
		if remote {
			_ = manager.materializer.Cleanup(context.Background(), location)
		}
		return Marketplace{}, err
	}
	previous, hadPrevious := state.Marketplaces[marketplace.Name]
	if err := manager.store.AddMarketplace(ctx, MarketplaceRecord{Name: marketplace.Name, Source: source, InstallLocation: location}); err != nil {
		if remote {
			_ = manager.materializer.Cleanup(context.Background(), location)
		}
		return Marketplace{}, err
	}
	if hadPrevious && previous.InstallLocation != location && isRemoteMarketplace(previous.Source) {
		_ = manager.materializer.Cleanup(context.Background(), previous.InstallLocation)
	}
	return marketplace, nil
}

func (manager *Manager) RemoveMarketplace(ctx context.Context, name string) error {
	state, loadErr := manager.store.Load(ctx)
	if loadErr != nil {
		return loadErr
	}
	if record, exists := state.Marketplaces[name]; exists && isRemoteMarketplace(record.Source) && manager.materializer == nil {
		return errors.New("remote marketplace cleanup requires its materializer")
	}
	record, removed, err := manager.store.RemoveMarketplaceCascade(ctx, name)
	if err != nil {
		return err
	}
	if removed && isRemoteMarketplace(record.Source) {
		_ = manager.materializer.Cleanup(context.Background(), record.InstallLocation)
	}
	return nil
}

func (manager *Manager) Install(ctx context.Context, pluginID string) (Plugin, error) {
	name, marketName, err := SplitID(pluginID)
	if err != nil {
		return Plugin{}, err
	}
	state, err := manager.store.Load(ctx)
	if err != nil {
		return Plugin{}, err
	}
	record, ok := state.Marketplaces[marketName]
	if !ok {
		return Plugin{}, errors.New("marketplace is not configured")
	}
	market, err := LoadMarketplace(record.InstallLocation)
	if err != nil {
		return Plugin{}, err
	}
	var entry *MarketplaceEntry
	for index := range market.Plugins {
		if market.Plugins[index].Name == name {
			entry = &market.Plugins[index]
			break
		}
	}
	if entry == nil {
		return Plugin{}, errors.New("plugin is not present in marketplace")
	}
	var source string
	if entry.Source.Type == SourceDirectory || entry.Source.Type == SourceLocal {
		base := market.Root
		if raw, ok := market.Metadata["pluginRoot"].(string); ok && strings.HasPrefix(raw, "./") {
			if resolved, resolveErr := containedPath(market.Root, raw); resolveErr == nil {
				base = resolved
			}
		}
		source, err = containedPath(base, entry.Source.Path)
	} else {
		if manager.materializer == nil {
			return Plugin{}, errors.New("remote plugin requires an explicit materializer")
		}
		materialized, materializeErr := manager.materializer.Plugin(ctx, market, *entry, filepath.Join(manager.store.root, "sources"))
		if materializeErr != nil {
			return Plugin{}, materializeErr
		}
		source = materialized.Root
		defer manager.materializer.Cleanup(context.Background(), materialized.CleanupRoot)
	}
	if err != nil {
		return Plugin{}, err
	}
	manifest, inventory, err := LoadManifest(source, name)
	if err != nil {
		return Plugin{}, err
	}
	version := ""
	if manifest != nil {
		version = manifest.Version
	}
	installed, err := manager.store.install(ctx, pluginID, source, version, &record)
	if err != nil {
		return Plugin{}, err
	}
	inventory = rebaseInventory(inventory, source, installed.InstallPath)
	manifest = rebaseManifest(manifest, source, installed.InstallPath)
	return Plugin{ID: pluginID, Name: name, Marketplace: marketName, Version: version, Root: installed.InstallPath, DataDir: filepath.Join(manager.store.root, "data", safeID(pluginID)), Enabled: true, Manifest: manifest, Inventory: inventory}, nil
}

func isRemoteMarketplace(source MarketplaceSource) bool {
	return source.Type != SourceDirectory && source.Type != SourceFile
}

func rebaseInventory(inventory Inventory, from, to string) Inventory {
	rebase := func(paths []string) []string {
		result := make([]string, 0, len(paths))
		for _, path := range paths {
			relative, err := filepath.Rel(from, path)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				result = append(result, filepath.Join(to, relative))
			}
		}
		return result
	}
	inventory.Skills = rebase(inventory.Skills)
	inventory.MCPFiles = rebase(inventory.MCPFiles)
	inventory.HookFiles = rebase(inventory.HookFiles)
	return inventory
}
func rebaseManifest(manifest *Manifest, from, to string) *Manifest {
	if manifest == nil {
		return nil
	}
	copyManifest := *manifest
	copyManifest.ComponentPaths = map[string][]string{}
	for field, paths := range manifest.ComponentPaths {
		copyManifest.ComponentPaths[field] = rebaseInventory(Inventory{Skills: paths}, from, to).Skills
	}
	copyManifest.InlineMCP = cloneRaw(manifest.InlineMCP)
	copyManifest.InlineHooks = append([]byte(nil), manifest.InlineHooks...)
	return &copyManifest
}

func (manager *Manager) Enable(ctx context.Context, id string) error {
	return manager.store.SetEnabled(ctx, id, true)
}
func (manager *Manager) Disable(ctx context.Context, id string) error {
	return manager.store.SetEnabled(ctx, id, false)
}
func (manager *Manager) Uninstall(ctx context.Context, id string) error {
	return manager.store.Uninstall(ctx, id)
}

func (manager *Manager) Discover(ctx context.Context) ([]Plugin, []string, error) {
	state, err := manager.store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	var plugins []Plugin
	var warnings []string
	cacheRoot := filepath.Join(manager.store.root, "cache")
	canonicalCache, cacheErr := filepath.EvalSymlinks(cacheRoot)
	if cacheErr != nil && len(state.Installed) > 0 {
		return nil, nil, fmt.Errorf("resolve plugin cache: %w", cacheErr)
	}
	canonicalStore, storeErr := filepath.EvalSymlinks(manager.store.root)
	if storeErr != nil && len(state.Installed) > 0 {
		return nil, nil, fmt.Errorf("resolve plugin store: %w", storeErr)
	}
	for _, id := range sortedKeys(state.Installed) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		entry := state.Installed[id]
		name, market, splitErr := SplitID(id)
		if splitErr != nil {
			warnings = append(warnings, "skipping invalid installed plugin id")
			continue
		}
		absolute, absErr := filepath.Abs(entry.InstallPath)
		resolved, resolveErr := canonicalRoot(absolute)
		rel, relErr := filepath.Rel(canonicalCache, resolved)
		if absErr != nil || resolveErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			warnings = append(warnings, "skipping plugin with install path outside cache: "+id)
			continue
		}
		manifest, inventory, loadErr := LoadManifest(resolved, name)
		if loadErr != nil {
			warnings = append(warnings, "skipping invalid plugin "+id+": "+loadErr.Error())
			continue
		}
		version := entry.Version
		if manifest != nil {
			version = manifest.Version
		}
		plugins = append(plugins, Plugin{ID: id, Name: name, Marketplace: market, Version: version, Root: resolved, DataDir: filepath.Join(canonicalStore, "data", safeID(id)), Enabled: state.Enabled[id], Manifest: manifest, Inventory: inventory})
	}
	return plugins, warnings, nil
}

func safeID(id string) string {
	digest := sha256Sum(id)
	prefix := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
	if len(prefix) > 48 {
		prefix = prefix[:48]
	}
	return prefix + "-" + digest[:16]
}
func sha256Sum(value string) string { sum := sha256Bytes([]byte(value)); return fmt.Sprintf("%x", sum) }

// Compose returns enabled, confined component sources without executing them.
func (manager *Manager) Compose(ctx context.Context, projectDir string) (Components, []string, error) {
	plugins, warnings, err := manager.Discover(ctx)
	if err != nil {
		return Components{}, nil, err
	}
	components := Components{}
	for _, plugin := range plugins {
		if !plugin.Enabled {
			continue
		}
		environment := map[string]string{"CLAUDE_PLUGIN_ROOT": plugin.Root, "PLUGIN_DATA": plugin.DataDir}
		if projectDir != "" {
			if absolute, absErr := filepath.Abs(projectDir); absErr == nil {
				environment["CLAUDE_PROJECT_DIR"] = absolute
			}
		}
		for _, path := range plugin.Inventory.Skills {
			components.Skills = append(components.Skills, SkillSource{PluginID: plugin.ID, Root: plugin.Root, Path: path, Namespace: strings.ToLower(plugin.ID)})
		}
		for _, path := range plugin.Inventory.HookFiles {
			components.Hooks = append(components.Hooks, HookSource{PluginID: plugin.ID, Path: path, Environment: cloneStrings(environment)})
		}
		if plugin.Manifest != nil && len(plugin.Manifest.InlineHooks) > 0 {
			components.Hooks = append(components.Hooks, HookSource{PluginID: plugin.ID, Inline: append([]byte(nil), plugin.Manifest.InlineHooks...), Environment: cloneStrings(environment)})
		}
		for _, path := range plugin.Inventory.MCPFiles {
			components.MCP = append(components.MCP, MCPSource{PluginID: plugin.ID, Path: path, Environment: cloneStrings(environment)})
		}
		if plugin.Manifest != nil && len(plugin.Manifest.InlineMCP) > 0 {
			components.MCP = append(components.MCP, MCPSource{PluginID: plugin.ID, Inline: cloneRaw(plugin.Manifest.InlineMCP), Environment: cloneStrings(environment)})
		}
		if len(plugin.Inventory.Unsupported) > 0 {
			warnings = append(warnings, "plugin "+plugin.ID+" contains unsupported components: "+strings.Join(plugin.Inventory.Unsupported, ", "))
		}
	}
	sort.Slice(components.Skills, func(i, j int) bool {
		return components.Skills[i].PluginID+components.Skills[i].Path < components.Skills[j].PluginID+components.Skills[j].Path
	})
	return components, warnings, nil
}

func cloneStrings(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
func cloneRaw(input map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(input))
	for key, value := range input {
		result[key] = append([]byte(nil), value...)
	}
	return result
}
