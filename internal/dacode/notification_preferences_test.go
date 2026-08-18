package dacode

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestNotificationPreferencesUseSafeUsefulDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config.toml")
	suppressed, diagnostics := loadSuppressedWarnings(path)
	if len(suppressed) != 0 || len(diagnostics) != 0 {
		t.Fatalf("missing preferences = %#v, %#v", suppressed, diagnostics)
	}
	if err := newNotificationPreferenceStore(path).setWarningEnabled(warningTavily, false); err != nil {
		t.Fatal(err)
	}
	suppressed, diagnostics = loadSuppressedWarnings(path)
	if !suppressed[warningTavily] || len(diagnostics) != 0 {
		t.Fatalf("saved preferences = %#v, %#v", suppressed, diagnostics)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
}

func TestNotificationPreferenceStoreRequiresPath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	newNotificationPreferenceStore(" \t")
}

func TestNotificationPreferencesPreserveUnrelatedConfigurationAndSort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[ui]\ntheme = \"nord\"\n\n[custom]\nvalue = 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newNotificationPreferenceStore(path)
	if err := store.setWarningEnabled(warningYOLO, false); err != nil {
		t.Fatal(err)
	}
	if err := store.setWarningEnabled(warningRipgrep, false); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if _, err := toml.Decode(string(payload), &document); err != nil {
		t.Fatal(err)
	}
	if document["custom"].(map[string]any)["value"] != int64(7) || document["ui"].(map[string]any)["theme"] != "nord" {
		t.Fatalf("unrelated configuration changed: %s", payload)
	}
	if !strings.Contains(string(payload), "suppress = [\"ripgrep\", \"yolo\"]") {
		t.Fatalf("suppression list is not deterministic: %s", payload)
	}
	if err := store.setWarningEnabled(warningRipgrep, true); err != nil {
		t.Fatal(err)
	}
	suppressed, diagnostics := loadSuppressedWarnings(path)
	if suppressed[warningRipgrep] || !suppressed[warningYOLO] || len(diagnostics) != 0 {
		t.Fatalf("enabled warning = %#v, %#v", suppressed, diagnostics)
	}
}

func TestNotificationPreferencesFailSafeForMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[warnings]\nsuppress = \"yolo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	suppressed, diagnostics := loadSuppressedWarnings(path)
	if len(suppressed) != 0 || len(diagnostics) != 1 {
		t.Fatalf("malformed preferences = %#v, %#v", suppressed, diagnostics)
	}
	if err := newNotificationPreferenceStore(path).setWarningEnabled(warningYOLO, false); err == nil {
		t.Fatal("malformed preferences were overwritten")
	}
}

func TestNotificationPreferencesRejectSymlinkConfiguration(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.toml")
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(target, []byte("[warnings]\nsuppress = [\"yolo\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	suppressed, diagnostics := loadSuppressedWarnings(path)
	if len(suppressed) != 0 || len(diagnostics) != 1 {
		t.Fatalf("symlink preferences = %#v, %#v", suppressed, diagnostics)
	}
	if err := newNotificationPreferenceStore(path).setWarningEnabled(warningYOLO, false); err == nil {
		t.Fatal("symlink preferences were overwritten")
	}
}

func TestThemeAndNotificationPreferenceWritesAreSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	registry := builtinThemeRegistry()
	themeStore := newThemePreferenceStore(path)
	notificationStore := newNotificationPreferenceStore(path)
	for attempt := 0; attempt < 20; attempt++ {
		var group sync.WaitGroup
		group.Go(func() {
			if err := themeStore.saveGlobal(registry, "nord"); err != nil {
				t.Errorf("save theme: %v", err)
			}
		})
		group.Go(func() {
			if err := notificationStore.setWarningEnabled(warningYOLO, false); err != nil {
				t.Errorf("save notification: %v", err)
			}
		})
		group.Wait()
		name, _, diagnostics := loadThemePreference(path, "", nil)
		suppressed, warningDiagnostics := loadSuppressedWarnings(path)
		if name != "nord" || !suppressed[warningYOLO] || len(diagnostics) != 0 || len(warningDiagnostics) != 0 {
			t.Fatalf("attempt %d lost a preference: theme=%q suppressed=%#v diagnostics=%#v %#v", attempt, name, suppressed, diagnostics, warningDiagnostics)
		}
	}
}

func TestNotificationPreferenceWritesPreserveLatestIntentAndResultGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	store := newNotificationPreferenceStore(path)
	older := store.beginWarningEnabled(warningYOLO, false)
	newer := store.beginWarningEnabled(warningYOLO, true)
	if older.Generation == 0 || newer.Generation <= older.Generation || store.currentWarningWrite(older) || !store.currentWarningWrite(newer) {
		t.Fatalf("write generations = old %#v new %#v", older, newer)
	}
	if err := store.saveWarningEnabled(newer); err != nil {
		t.Fatal(err)
	}
	if err := store.saveWarningEnabled(older); err != nil {
		t.Fatal(err)
	}
	suppressed, diagnostics := loadSuppressedWarnings(path)
	if suppressed[warningYOLO] || len(diagnostics) != 0 {
		t.Fatalf("late write replaced current intent: %#v, %#v", suppressed, diagnostics)
	}
}

func TestNotificationPreferenceWritesAreRaceSafeAcrossKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	store := newNotificationPreferenceStore(path)
	for attempt := 0; attempt < 20; attempt++ {
		var group sync.WaitGroup
		for _, key := range []string{warningRipgrep, warningTavily, warningYOLO} {
			key := key
			older := store.beginWarningEnabled(key, false)
			newer := store.beginWarningEnabled(key, true)
			group.Go(func() { _ = store.saveWarningEnabled(older) })
			group.Go(func() { _ = store.saveWarningEnabled(newer) })
		}
		group.Wait()
	}
	suppressed, diagnostics := loadSuppressedWarnings(path)
	if len(suppressed) != 0 || len(diagnostics) != 0 {
		t.Fatalf("latest warning intent was lost: %#v, %#v", suppressed, diagnostics)
	}
}
