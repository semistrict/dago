package daplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifestPinnedCompositionOrderAndInlineNormalization(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "skills/base/SKILL.md", "---\nname: base\ndescription: Base.\n---\n")
	writeTestFile(t, root, "extra/skill/SKILL.md", "---\nname: skill\ndescription: Extra.\n---\n")
	writeTestFile(t, root, ".mcp.json", `{"mcpServers":{"base":{}}}`)
	writeTestFile(t, root, "extra-mcp.json", `{"mcpServers":{"extra":{}}}`)
	writeTestFile(t, root, "hooks/hooks.json", `{"hooks":{}}`)
	writeTestFile(t, root, "extra-hooks/hooks.json", `{"hooks":{}}`)
	writeTestFile(t, root, "agents/ignored", "x")
	writeTestFile(t, root, "plugin.json", `{"name":"plug","version":"1","skills":[7,"./extra"],"mcpServers":[{"one":{}},{"two":{}}],"hooks":{"Stop":[]},"extensions":{"wrong":{"autoUpdate":true},"com.langchain.deepagents.code":{"autoUpdate":true}}}`)
	manifest, inventory, err := LoadManifest(root, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "plug" || !manifest.AutoUpdate || len(manifest.InlineMCP) != 2 || !strings.Contains(string(manifest.InlineHooks), `"hooks"`) {
		t.Fatalf("manifest=%#v", manifest)
	}
	if len(inventory.Skills) != 2 || !strings.HasSuffix(inventory.Skills[0], "skills") || !strings.HasSuffix(inventory.Skills[1], "extra") {
		t.Fatalf("skills=%#v", inventory.Skills)
	}
	if len(inventory.MCPFiles) != 1 || len(inventory.HookFiles) != 1 || !reflect.DeepEqual(inventory.Unsupported, []string{"agents"}) {
		t.Fatalf("inventory=%#v", inventory)
	}
	if len(inventory.Warnings) != 1 {
		t.Fatalf("warnings=%#v", inventory.Warnings)
	}
}

func TestLoadManifestRootSkillFallbackAndEscapeRejection(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "SKILL.md", "---\nname: plug\ndescription: Root.\n---\n")
	manifest, inventory, err := LoadManifest(root, "plug")
	if err != nil || manifest != nil || len(inventory.Skills) != 1 {
		t.Fatalf("result=%#v %#v %v", manifest, inventory, err)
	}
	outside := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, root, "plugin.json", `{"name":"plug","skills":"./escape"}`)
		_, got, err := LoadManifest(root, "plug")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Skills) != 1 || len(got.Warnings) == 0 {
			t.Fatalf("escaping symlink loaded: %#v", got)
		}
	}
}

func TestMarketplaceSourceAndManifestValidation(t *testing.T) {
	for _, test := range []struct {
		raw  string
		kind SourceType
		fail bool
	}{{"owner/repo#main", SourceGitHub, false}, {"https://example.com/catalog.json", SourceURL, false}, {"http://example.com/x", "", true}, {"", "", true}} {
		got, err := ParseMarketplaceSource(test.raw)
		if test.fail {
			if err == nil {
				t.Fatalf("%q accepted", test.raw)
			}
		} else if err != nil || got.Type != test.kind {
			t.Fatalf("Parse(%q)=%#v,%v", test.raw, got, err)
		}
	}
	root := t.TempDir()
	writeTestFile(t, root, ".agents/plugins/marketplace.json", `{"name":"market","plugins":[{"name":"good","source":"./plugins/good"},{"name":"bad name","source":7}]}`)
	market, err := LoadMarketplace(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(market.Plugins) != 1 || len(market.Warnings) != 1 {
		t.Fatalf("market=%#v", market)
	}
}

func TestManagerLifecycleCompositionAndCorruptState(t *testing.T) {
	catalog := t.TempDir()
	pluginRoot := filepath.Join(catalog, "plugins", "review")
	writeTestFile(t, pluginRoot, "plugin.json", `{"name":"review","version":"1.0","skills":"./skills","mcpServers":{"server":{"command":"tool"}},"hooks":{"Stop":[]}}`)
	writeTestFile(t, pluginRoot, "skills/review/SKILL.md", "---\nname: review\ndescription: Review.\n---\n")
	writeTestFile(t, pluginRoot, "commands/ignored", "x")
	writeTestFile(t, catalog, ".agents/plugins/marketplace.json", `{"name":"market","plugins":[{"name":"review","source":"./plugins/review"}]}`)
	store := NewStore(t.TempDir(), StoreOptions{})
	manager := NewManager(store, nil, ManagerOptions{})
	source, err := ParseMarketplaceSource(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddMarketplace(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	plugin, err := manager.Install(context.Background(), "review@market")
	if err != nil {
		t.Fatal(err)
	}
	if !plugin.Enabled || plugin.Version != "1.0" {
		t.Fatalf("plugin=%#v", plugin)
	}
	components, warnings, err := manager.Compose(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(components.Skills) != 1 || components.Skills[0].Namespace != "review@market" || len(components.Hooks) != 1 || len(components.MCP) != 1 || len(warnings) == 0 {
		t.Fatalf("components=%#v warnings=%#v", components, warnings)
	}
	if err := manager.Disable(context.Background(), plugin.ID); err != nil {
		t.Fatal(err)
	}
	components, _, _ = manager.Compose(context.Background(), "")
	if len(components.Skills) != 0 {
		t.Fatal("disabled plugin remained composed")
	}
	data := plugin.DataDir
	writeTestFile(t, data, "state", "keep")
	if err := manager.Uninstall(context.Background(), plugin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); err != nil {
		t.Fatalf("data removed: %v", err)
	}
	if err := os.WriteFile(store.statePath, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Enable(context.Background(), plugin.ID); err == nil {
		t.Fatal("corrupt future state was overwritten")
	}
	raw, _ := os.ReadFile(store.statePath)
	if !strings.Contains(string(raw), `99`) {
		t.Fatal("corrupt state changed")
	}
}

func TestInstallRejectsIdentityMismatchLinksAndBounds(t *testing.T) {
	store := NewStore(t.TempDir(), StoreOptions{MaxCopyFiles: 1, MaxCopyBytes: 8})
	source := t.TempDir()
	writeTestFile(t, source, "plugin.json", `{"name":"other"}`)
	if _, err := store.Install(context.Background(), "wanted@market", source, ""); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	source = t.TempDir()
	writeTestFile(t, source, "plugin.json", `{"name":"wanted"}`)
	writeTestFile(t, source, "extra", "123456789")
	if _, err := store.Install(context.Background(), "wanted@market", source, ""); err == nil {
		t.Fatal("copy bounds ignored")
	}
	if runtime.GOOS != "windows" {
		source = t.TempDir()
		writeTestFile(t, source, "plugin.json", `{"name":"wanted"}`)
		if err := os.Symlink(filepath.Join(source, "plugin.json"), filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Install(context.Background(), "wanted@market", source, ""); err == nil {
			t.Fatal("symlink accepted")
		}
	}
}

func TestSplitIDUsesLastSeparator(t *testing.T) {
	name, market, err := SplitID("review@team@market")
	if err != nil || name != "review@team" || market != "market" {
		t.Fatalf("SplitID=%q,%q,%v", name, market, err)
	}
	if _, _, err := SplitID("missing"); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestStateJSONIsStable(t *testing.T) {
	state := emptyState()
	raw, err := json.Marshal(state)
	if err != nil || !strings.Contains(string(raw), `"version":1`) {
		t.Fatalf("state=%s %v", raw, err)
	}
}

func TestComponentNamesAreNamespacedAndCollisionResistant(t *testing.T) {
	if got := NamespacedSkillName("Review@Market", "Check", "Nested"); got != "review@market:nested:check" {
		t.Fatalf("skill name=%q", got)
	}
	first := ScopedMCPName("tools@example.com", "server")
	second := ScopedMCPName("tools-example.com", "server")
	if first == second || !strings.HasPrefix(first, "plugin__") {
		t.Fatalf("scoped names=%q %q", first, second)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("empty plugin identity did not panic")
		}
	}()
	_ = ScopedMCPName("", "server")
}

func TestUninstallCannotFollowReplacedCacheAndCopyPreservesExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and symlink contract")
	}
	store := NewStore(t.TempDir(), StoreOptions{})
	source := t.TempDir()
	writeTestFile(t, source, "plugin.json", `{"name":"tool"}`)
	script := writeTestFile(t, source, "run.sh", "#!/bin/sh\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Install(context.Background(), "tool@market", source, "")
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.Stat(filepath.Join(entry.InstallPath, "run.sh"))
	if err != nil || installed.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable mode=%v err=%v", installed.Mode(), err)
	}
	outside := t.TempDir()
	marker := writeTestFile(t, outside, "keep", "x")
	cache := filepath.Join(store.root, "cache")
	if err := os.Rename(cache, cache+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, cache); err != nil {
		t.Fatal(err)
	}
	if err := store.Uninstall(context.Background(), "tool@market"); err == nil {
		t.Fatal("symlinked cache ancestor was accepted")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("outside marker removed: %v", err)
	}
}

func TestRepositoryMaterializationLimits(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "one", "1234")
	writeTestFile(t, root, "two", "5678")
	if err := treeWithinLimits(root, 1, 100); err == nil {
		t.Fatal("entry limit ignored")
	}
	if err := treeWithinLimits(root, 10, 4); err == nil {
		t.Fatal("byte limit ignored")
	}
}

func TestStateMutationRejectsUnreadableNextStateWithoutReplacingCurrent(t *testing.T) {
	store := NewStore(t.TempDir(), StoreOptions{})
	if err := store.AddMarketplace(context.Background(), MarketplaceRecord{Name: "first", Source: MarketplaceSource{Type: SourceDirectory, Value: "/catalog"}, InstallLocation: "/catalog"}); err != nil {
		t.Fatal(err)
	}
	err := store.mutate(context.Background(), func(state *State) error {
		for index := 0; index < MaxPlugins; index++ {
			name := fmt.Sprintf("market-%03d", index)
			state.Marketplaces[name] = MarketplaceRecord{Name: name, InstallLocation: "/x"}
		}
		return nil
	})
	if err == nil {
		t.Fatal("oversized state committed")
	}
	state, loadErr := store.Load(context.Background())
	if loadErr != nil || len(state.Marketplaces) != 1 {
		t.Fatalf("prior state lost: %#v %v", state, loadErr)
	}
}
