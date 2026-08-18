package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPluginManagerServiceSnapshotsMutationsAndPendingReload(t *testing.T) {
	storeRoot, catalog := t.TempDir(), t.TempDir()
	writePluginCLIFile(t, filepath.Join(catalog, "plugins", "alpha"), "plugin.json", `{"name":"alpha","displayName":"Alpha","version":"1","skills":"./skills","hooks":{"SessionStart":[]}}`)
	writePluginCLIFile(t, filepath.Join(catalog, "plugins", "alpha"), "skills/check/SKILL.md", "---\nname: check\ndescription: Check.\n---\nCheck.\n")
	writePluginCLIFile(t, filepath.Join(catalog, "plugins", "beta"), "plugin.json", `{"name":"beta","version":"2"}`)
	writePluginCLIFile(t, catalog, ".agents/plugins/marketplace.json", `{"name":"local","plugins":[{"name":"alpha","description":"Alpha plugin","source":"./plugins/alpha"},{"name":"beta","description":"Beta plugin","source":"./plugins/beta"}]}`)

	service := newPluginManagerService(storeRoot, nil)
	if err := service.AddMarketplace(t.Context(), catalog); err != nil {
		t.Fatal(err)
	}
	if err := service.Install(t.Context(), "alpha@local"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(t.Context(), []string{"alpha@local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Available) != 1 || snapshot.Available[0].ID != "beta@local" || len(snapshot.Installed) != 1 || snapshot.Installed[0].Name != "Alpha" || snapshot.Installed[0].Pending || snapshot.Installed[0].Skills != 1 || snapshot.Installed[0].Hooks != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if len(snapshot.Marketplaces) != 1 || snapshot.Marketplaces[0].PluginCount != 2 || snapshot.Marketplaces[0].InstalledCount != 1 {
		t.Fatalf("marketplaces = %#v", snapshot.Marketplaces)
	}
	if err := service.SetEnabled(t.Context(), "alpha@local", false); err != nil {
		t.Fatal(err)
	}
	snapshot, err = service.Snapshot(t.Context(), []string{"alpha@local"})
	if err != nil || !snapshot.Installed[0].Pending || snapshot.Installed[0].Enabled || !snapshot.Installed[0].Loaded {
		t.Fatalf("disabled pending snapshot = %#v, %v", snapshot, err)
	}
	if err := service.SetEnabled(t.Context(), "alpha@local", true); err != nil {
		t.Fatal(err)
	}
	if err := service.Install(t.Context(), "beta@local"); err != nil {
		t.Fatal(err)
	}
	if err := service.Uninstall(t.Context(), "beta@local"); err != nil {
		t.Fatal(err)
	}
	if err := service.RemoveMarketplace(t.Context(), "local"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = service.Snapshot(t.Context(), nil)
	if err != nil || len(snapshot.Available)+len(snapshot.Installed)+len(snapshot.Marketplaces) != 0 {
		t.Fatalf("removed snapshot = %#v, %v", snapshot, err)
	}
}

func TestPluginManagerServiceCancellationAndStableErrors(t *testing.T) {
	service := newPluginManagerService(t.TempDir(), nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Snapshot(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot = %v", err)
	}
	if got := pluginManagerDisplayError(context.Canceled); got != "Plugin action cancelled." {
		t.Fatalf("cancel display = %q", got)
	}
	if got := pluginManagerDisplayError(errors.New("https://user:" + "password@example.test")); got != "Plugin action failed. Check the marketplace or plugin configuration." {
		t.Fatalf("unsafe display = %q", got)
	}
	assertPluginManagerPanics(t, func() { newPluginManagerService("", nil) })
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		root := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "plugins")); err != nil {
			t.Fatal(err)
		}
		linked := newPluginManagerService(filepath.Join(root, "plugins"), nil)
		if _, err := linked.Snapshot(t.Context(), nil); err == nil {
			t.Fatal("linked store root accepted")
		}
	}
}

func assertPluginManagerPanics(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	call()
}
