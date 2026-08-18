package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	dagoapi "github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dahook"
	"github.com/semistrict/dago/damcp"
	"github.com/semistrict/dago/daplugin"
	"github.com/semistrict/dago/daskill"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	pluginSkillMount      = "/plugin-skills"
	maxPluginRuntimeFile  = int64(4 << 20)
	maxPluginSkillEntries = 4096
	maxPluginMCPServers   = 256
)

type pluginRuntimeComponents struct {
	SkillRoutes  map[string]dabackend.Backend
	SkillCatalog []dagoapi.Skill
	Hooks        []dahook.Plugin
	MCP          []damcp.Connection
	Warnings     []string
	LoadedIDs    []string
}

// loadRuntimePlugins composes only already-installed plugins. The required
// store and project roots are positional; this path never materializes remote
// content or performs network discovery.
func loadRuntimePlugins(ctx context.Context, storeRoot, projectRoot string, lookup damcp.LookupEnv) (pluginRuntimeComponents, error) {
	if ctx == nil || lookup == nil {
		panic("dacode: plugin runtime context and environment lookup are required")
	}
	if !filepath.IsAbs(storeRoot) || !filepath.IsAbs(projectRoot) {
		panic("dacode: plugin runtime roots must be absolute")
	}
	manager := daplugin.NewManager(daplugin.NewStore(storeRoot, daplugin.StoreOptions{}), nil, daplugin.ManagerOptions{})
	discovered, _, err := manager.Discover(ctx)
	if err != nil {
		return pluginRuntimeComponents{}, fmt.Errorf("discover installed plugins: %w", err)
	}
	composed, warnings, err := manager.Compose(ctx, projectRoot)
	if err != nil {
		return pluginRuntimeComponents{}, fmt.Errorf("compose installed plugins: %w", err)
	}
	result := pluginRuntimeComponents{SkillRoutes: map[string]dabackend.Backend{}, Warnings: append([]string(nil), warnings...)}
	for _, plugin := range discovered {
		if plugin.Enabled {
			result.LoadedIDs = append(result.LoadedIDs, plugin.ID)
		}
	}
	sort.Strings(result.LoadedIDs)
	if err := result.composeSkills(ctx, composed.Skills); err != nil {
		return pluginRuntimeComponents{}, err
	}
	for _, source := range composed.Hooks {
		if err := ensurePluginDataDirectory(storeRoot, source.Environment["PLUGIN_DATA"]); err != nil {
			result.Warnings = append(result.Warnings, "enabled plugin hook data directory is unavailable")
			continue
		}
		result.Hooks = append(result.Hooks, dahook.Plugin{
			ID: source.PluginID, Path: source.Path, Inline: append(json.RawMessage(nil), source.Inline...),
			Enabled: true, Environment: clonePluginEnvironment(source.Environment),
		})
	}
	result.composeMCP(ctx, storeRoot, composed.MCP, lookup)
	sort.Strings(result.Warnings)
	return result, nil
}

func (result *pluginRuntimeComponents) composeSkills(ctx context.Context, sources []daplugin.SkillSource) error {
	mounted := map[string]string{}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		mount, exists := mounted[source.PluginID]
		if !exists {
			files, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: source.Root, MaxFileSize: 1 << 20, MaxResults: 1_000})
			if err != nil {
				result.Warnings = append(result.Warnings, "enabled plugin skill root is unavailable")
				continue
			}
			mount = pluginSkillMount + "/" + pluginRuntimeID(source.PluginID)
			result.SkillRoutes[mount] = readOnlySkillBackend{Backend: files}
			mounted[source.PluginID] = mount
		}
		relative, err := filepath.Rel(source.Root, source.Path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			result.Warnings = append(result.Warnings, "enabled plugin skill path is not confined")
			continue
		}
		info, err := os.Lstat(source.Path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			result.Warnings = append(result.Warnings, "enabled plugin skill path is unavailable")
			continue
		}
		virtual := mount + "/" + strings.TrimPrefix(filepath.ToSlash(relative), "./")
		if info.Mode().IsRegular() {
			skill, warned, loadErr := readPluginSkill(ctx, source.Path)
			if loadErr != nil {
				result.Warnings = append(result.Warnings, "enabled plugin skill document is invalid")
				continue
			}
			if warned {
				result.Warnings = append(result.Warnings, "enabled plugin skill document has compatibility warnings")
			}
			skill.Name = daplugin.NamespacedSkillName(source.Namespace, skill.Name)
			skill.Path = virtual
			skill.Body = ""
			result.SkillCatalog = append(result.SkillCatalog, skill)
			continue
		}
		if !info.IsDir() {
			result.Warnings = append(result.Warnings, "enabled plugin skill path is not a regular file or directory")
			continue
		}
		tree, invalid, walkErr := readPluginSkillTree(ctx, source, mount)
		if walkErr != nil {
			result.Warnings = append(result.Warnings, "enabled plugin skill tree is invalid or exceeds limits")
			continue
		}
		if invalid {
			result.Warnings = append(result.Warnings, "enabled plugin skill tree contains skipped or compatibility-warning documents")
		}
		result.SkillCatalog = append(result.SkillCatalog, tree...)
	}
	return nil
}

func readPluginSkillTree(ctx context.Context, source daplugin.SkillSource, mount string) ([]dagoapi.Skill, bool, error) {
	var skills []dagoapi.Skill
	invalid := false
	count := 0
	err := filepath.WalkDir(source.Path, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > maxPluginSkillEntries {
			return errors.New("plugin skill tree exceeds entry limit")
		}
		if entry.IsDir() && path != source.Path {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
		}
		if !entry.Type().IsRegular() || !strings.EqualFold(entry.Name(), "SKILL.md") {
			return nil
		}
		skill, warned, err := readPluginSkill(ctx, path)
		if err != nil {
			invalid = true
			return nil
		}
		invalid = invalid || warned
		relativeDir, err := filepath.Rel(source.Path, filepath.Dir(path))
		if err != nil || relativeDir == ".." || strings.HasPrefix(relativeDir, ".."+string(filepath.Separator)) {
			return errors.New("plugin skill document escapes source root")
		}
		var folders []string
		if relativeDir != "." {
			parts := strings.Split(filepath.ToSlash(relativeDir), "/")
			if len(parts) > 1 {
				folders = parts[:len(parts)-1]
			}
		}
		skill.Name = daplugin.NamespacedSkillName(source.Namespace, skill.Name, folders...)
		relativeFile, err := filepath.Rel(source.Root, path)
		if err != nil || relativeFile == ".." || strings.HasPrefix(relativeFile, ".."+string(filepath.Separator)) {
			return errors.New("plugin skill document escapes plugin root")
		}
		skill.Path = mount + "/" + strings.TrimPrefix(filepath.ToSlash(relativeFile), "./")
		skill.Body = ""
		skills = append(skills, skill)
		return nil
	})
	if err != nil {
		return nil, invalid, err
	}
	return skills, invalid, nil
}

func readPluginSkill(ctx context.Context, path string) (daskill.Skill, bool, error) {
	if err := ctx.Err(); err != nil {
		return daskill.Skill{}, false, err
	}
	file, err := os.Open(path)
	if err != nil {
		return daskill.Skill{}, false, err
	}
	defer file.Close()
	limited := &io.LimitedReader{R: file, N: 1<<20 + 1}
	raw, err := io.ReadAll(limited)
	if err != nil || limited.N == 0 {
		return daskill.Skill{}, false, errors.New("skill file exceeds limit")
	}
	skill, warnings, err := daskill.ParseContent(string(raw), filepath.ToSlash(path))
	if err != nil {
		return daskill.Skill{}, false, errors.New("invalid skill metadata")
	}
	return skill, len(warnings) > 0, nil
}

func (result *pluginRuntimeComponents) composeMCP(ctx context.Context, storeRoot string, sources []daplugin.MCPSource, lookup damcp.LookupEnv) {
	type definitions struct {
		environment map[string]string
		servers     map[string]json.RawMessage
	}
	byPlugin := map[string]*definitions{}
	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		if err := ensurePluginDataDirectory(storeRoot, source.Environment["PLUGIN_DATA"]); err != nil {
			result.Warnings = append(result.Warnings, "enabled plugin MCP data directory is unavailable")
			continue
		}
		current := byPlugin[source.PluginID]
		if current == nil {
			current = &definitions{environment: clonePluginEnvironment(source.Environment), servers: map[string]json.RawMessage{}}
			byPlugin[source.PluginID] = current
		}
		servers := source.Inline
		if source.Path != "" {
			var err error
			servers, err = readPluginMCPFile(ctx, source.Path)
			if err != nil {
				result.Warnings = append(result.Warnings, "enabled plugin MCP document is invalid")
				continue
			}
		}
		for name, raw := range servers {
			current.servers[name] = append(json.RawMessage(nil), raw...)
		}
	}
	pluginIDs := make([]string, 0, len(byPlugin))
	serverCount := 0
	for id := range byPlugin {
		pluginIDs = append(pluginIDs, id)
		serverCount += len(byPlugin[id].servers)
	}
	if serverCount > maxPluginMCPServers {
		result.Warnings = append(result.Warnings, "enabled plugin MCP declarations exceed the server limit")
		return
	}
	sort.Strings(pluginIDs)
	for _, id := range pluginIDs {
		current := byPlugin[id]
		names := make([]string, 0, len(current.servers))
		for name := range current.servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				result.Warnings = append(result.Warnings, "enabled plugin MCP server has an invalid name")
				continue
			}
			raw, err := normalizePluginMCPDefinition(current.servers[name], current.environment)
			if err != nil {
				result.Warnings = append(result.Warnings, "enabled plugin MCP server definition is invalid")
				continue
			}
			configured := damcp.ConfiguredServer{Name: daplugin.ScopedMCPName(id, name), Definition: raw, Source: "enabled plugin", Scope: damcp.UserConfigScope}
			connection, err := damcp.ResolveConnection(configured, overlayPluginEnvironment(current.environment, lookup))
			if err != nil {
				result.Warnings = append(result.Warnings, "enabled plugin MCP server definition is invalid or unresolved")
				continue
			}
			result.MCP = append(result.MCP, connection)
		}
	}
	sort.Slice(result.MCP, func(i, j int) bool { return result.MCP[i].Name < result.MCP[j].Name })
}

func readPluginMCPFile(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limited := &io.LimitedReader{R: file, N: maxPluginRuntimeFile + 1}
	raw, err := io.ReadAll(limited)
	if err != nil || limited.N == 0 {
		return nil, errors.New("MCP config exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var document map[string]json.RawMessage
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("MCP config has trailing data")
	}
	for _, wrapper := range []string{"mcpServers", "mcp_servers"} {
		if value, ok := document[wrapper]; ok {
			var servers map[string]json.RawMessage
			if err := json.Unmarshal(value, &servers); err != nil || servers == nil {
				return nil, errors.New("MCP server wrapper is invalid")
			}
			return servers, nil
		}
	}
	return document, nil
}

func normalizePluginMCPDefinition(raw json.RawMessage, environment map[string]string) (json.RawMessage, error) {
	var definition map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&definition); err != nil || definition == nil {
		return nil, errors.New("MCP server definition must be an object")
	}
	declared := map[string]any{}
	if value, ok := definition["env"]; ok {
		var valid bool
		declared, valid = value.(map[string]any)
		if !valid {
			return nil, errors.New("MCP environment must be an object")
		}
	}
	merged := make(map[string]any, len(environment)+len(declared))
	for key, value := range environment {
		merged[key] = value
	}
	for key, value := range declared {
		if _, ok := value.(string); !ok {
			return nil, errors.New("MCP environment values must be strings")
		}
		merged[key] = value
	}
	definition["env"] = merged
	if cwd, ok := definition["cwd"].(string); ok && cwd != "" && !filepath.IsAbs(cwd) {
		root := environment["CLAUDE_PLUGIN_ROOT"]
		candidate := filepath.Clean(filepath.Join(root, filepath.FromSlash(cwd)))
		relative, err := filepath.Rel(root, candidate)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("MCP working directory escapes plugin root")
		}
		definition["cwd"] = candidate
	}
	return json.Marshal(definition)
}

func ensurePluginDataDirectory(storeRoot, dataDir string) error {
	if dataDir == "" {
		return errors.New("plugin data directory is empty")
	}
	root, err := filepath.Abs(storeRoot)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, dataDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("plugin data directory escapes store")
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootHandle.Close()
	if err := rootHandle.MkdirAll(relative, 0o700); err != nil {
		return err
	}
	info, err := rootHandle.Stat(relative)
	if err != nil || !info.IsDir() {
		return errors.New("plugin data directory is not a real directory")
	}
	return os.Chmod(dataDir, 0o700)
}

func overlayPluginEnvironment(plugin map[string]string, fallback damcp.LookupEnv) damcp.LookupEnv {
	return func(key string) (string, bool) {
		if value, ok := plugin[key]; ok {
			return value, true
		}
		return fallback(key)
	}
}

func clonePluginEnvironment(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func pluginRuntimeID(id string) string {
	return strings.TrimPrefix(daplugin.ScopedMCPName(id, "root"), "plugin__")
}

func writePluginRuntimeWarnings(output io.Writer, warnings []string) {
	if output == nil {
		return
	}
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(output, "Plugin: %s.\n", unicodesecurity.RenderTerminalSafe(warning))
	}
}

func mergePluginMCP(resolution mcpConfigResolution, plugin []damcp.Connection) mcpConfigResolution {
	winning := make(map[string]damcp.Connection, len(plugin)+len(resolution.Connections)+len(resolution.OAuth))
	for _, connection := range plugin {
		winning[connection.Name] = connection
	}
	for _, connection := range resolution.OAuth {
		winning[connection.Name] = connection
	}
	for _, connection := range resolution.Connections {
		winning[connection.Name] = connection
	}
	resolution.Connections = resolution.Connections[:0]
	resolution.OAuth = resolution.OAuth[:0]
	names := make([]string, 0, len(winning))
	for name := range winning {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		connection := winning[name]
		if connection.Auth == "oauth" {
			resolution.OAuth = append(resolution.OAuth, connection)
		} else {
			resolution.Connections = append(resolution.Connections, connection)
		}
	}
	return resolution
}
