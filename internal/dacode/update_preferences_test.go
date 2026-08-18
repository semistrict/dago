package dacode

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAutoUpdatePreferenceUsesOptOutDefaultAndPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	preference, diagnostics := loadAutoUpdatePreference(path, mapLookup(map[string]string{}))
	if !preference.Enabled || preference.Explicit || preference.Source != "default" || len(diagnostics) != 0 {
		t.Fatalf("default = %#v, %v", preference, diagnostics)
	}
	store := newAutoUpdatePreferenceStore(path)
	if err := store.set(false); err != nil {
		t.Fatal(err)
	}
	preference, diagnostics = loadAutoUpdatePreference(path, mapLookup(map[string]string{}))
	if preference.Enabled || !preference.Explicit || preference.Source != "config" || len(diagnostics) != 0 {
		t.Fatalf("saved = %#v, %v", preference, diagnostics)
	}
	preference, diagnostics = loadAutoUpdatePreference(path, mapLookup(map[string]string{autoUpdateEnvironment: "yes"}))
	if !preference.Enabled || preference.Source != "environment" || len(diagnostics) != 0 {
		t.Fatalf("environment = %#v, %v", preference, diagnostics)
	}
	preference, diagnostics = loadAutoUpdatePreference(path, mapLookup(map[string]string{autoUpdateEnvironment: "maybe"}))
	if preference.Enabled || preference.Source != "config" || len(diagnostics) != 1 {
		t.Fatalf("invalid environment = %#v, %v", preference, diagnostics)
	}
	preference, diagnostics = loadAutoUpdatePreference(path, mapLookup(map[string]string{autoUpdateEnvironment: " \t"}))
	if preference.Enabled || !preference.Explicit || preference.Source != "environment" || len(diagnostics) != 0 {
		t.Fatalf("empty environment = %#v, %v", preference, diagnostics)
	}
}

func TestAutoUpdatePreferenceFailsClosedAndPreservesConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[ui]\ntheme = \"nord\"\n\n[update]\nauto_update = \"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preference, diagnostics := loadAutoUpdatePreference(path, nil)
	if preference.Enabled || preference.Source != "unavailable" || len(diagnostics) != 1 {
		t.Fatalf("malformed = %#v, %v", preference, diagnostics)
	}
	if err := newAutoUpdatePreferenceStore(path).set(true); err != nil {
		t.Fatal(err)
	}
	document, err := readThemeDocument(path)
	if err != nil || document["ui"].(map[string]any)["theme"] != "nord" || document["update"].(map[string]any)["auto_update"] != true {
		t.Fatalf("document = %#v, %v", document, err)
	}
}

func TestAutoUpdatePreferenceRejectsUnsafeConfig(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.toml")
	path := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(target, []byte("[update]\nauto_update = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	preference, diagnostics := loadAutoUpdatePreference(path, nil)
	if preference.Enabled || len(diagnostics) != 1 {
		t.Fatalf("symlink = %#v, %v", preference, diagnostics)
	}
	if err := newAutoUpdatePreferenceStore(path).set(true); err == nil {
		t.Fatal("symlink configuration was overwritten")
	}
}

func TestAutoUpdateAndOtherPreferenceWritesAreSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	updates := newAutoUpdatePreferenceStore(path)
	notifications := newNotificationPreferenceStore(path)
	for attempt := 0; attempt < 20; attempt++ {
		var group sync.WaitGroup
		group.Go(func() { _ = updates.set(false) })
		group.Go(func() { _ = notifications.setWarningEnabled(warningYOLO, false) })
		group.Wait()
		preference, updateDiagnostics := loadAutoUpdatePreference(path, nil)
		suppressed, warningDiagnostics := loadSuppressedWarnings(path)
		if preference.Enabled || !suppressed[warningYOLO] || len(updateDiagnostics) != 0 || len(warningDiagnostics) != 0 {
			t.Fatalf("attempt %d lost preference: %#v %#v %v %v", attempt, preference, suppressed, updateDiagnostics, warningDiagnostics)
		}
	}
}
