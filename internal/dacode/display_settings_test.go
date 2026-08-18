package dacode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/damessage"
)

func TestDisplaySettingsRoundTripAndUsefulDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", displaySettingsFilename)
	defaults, err := loadDisplaySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.ShowMessageTimestamps || defaults.ShowScrollbar {
		t.Fatalf("defaults = %#v", defaults)
	}
	if !defaults.ShowDiffLineNumbers {
		t.Fatalf("diff line numbers should be useful by default: %#v", defaults)
	}
	want := displaySettings{ShowMessageTimestamps: true, ShowScrollbar: true, ShowDiffLineNumbers: false}
	if err := saveDisplaySettings(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadDisplaySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("settings = %#v, want %#v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestDisplaySettingsRejectUnboundedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), displaySettingsFilename)
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxDisplaySettingsBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, err := loadDisplaySettings(path)
	if err == nil || settings != (displaySettings{}) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("settings = %#v, error = %v", settings, err)
	}
}

func TestDisplaySettingToggleRegistryIsCompleteAndFailClosed(t *testing.T) {
	settings := displaySettings{ShowDiffLineNumbers: true}
	for _, name := range []displaySettingName{displayMessageTimestamps, displayChatScrollbar, displayDiffLineNumbers} {
		var ok bool
		settings, ok = toggleDisplaySetting(settings, name)
		if !ok {
			t.Fatalf("toggle %q was not registered", name)
		}
	}
	if !settings.ShowMessageTimestamps || !settings.ShowScrollbar || settings.ShowDiffLineNumbers {
		t.Fatalf("settings = %#v", settings)
	}
	before := settings
	if after, ok := toggleDisplaySetting(settings, "unknown"); ok || after != before {
		t.Fatalf("unknown toggle = %#v, %v", after, ok)
	}
}

func TestDiffLineNumberToggleOnlyAffectsNewDiffsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), displaySettingsFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	if err := model.configureDisplaySettings(path); err != nil {
		t.Fatal(err)
	}
	model.addToolCall(damessage.ToolCall{ID: "old", Name: "edit_file", Arguments: []byte(`{"file_path":"main.go","old_string":"old","new_string":"new"}`)})
	if !model.items[0].lineNums {
		t.Fatal("new diff did not capture the useful line-number default")
	}
	command, handled := model.slashCommand("/line-numbers")
	if !handled || command == nil || model.showLineNumbers {
		t.Fatalf("toggle: handled = %t, command = %v, shown = %t", handled, command, model.showLineNumbers)
	}
	model.addToolCall(damessage.ToolCall{ID: "new", Name: "edit_file", Arguments: []byte(`{"file_path":"other.go","old_string":"before","new_string":"after"}`)})
	if !model.items[0].lineNums || model.items[1].lineNums {
		t.Fatalf("captured preferences = old:%t new:%t", model.items[0].lineNums, model.items[1].lineNums)
	}
	model.Update(command())
	settings, err := loadDisplaySettings(path)
	if err != nil || settings.ShowDiffLineNumbers {
		t.Fatalf("settings = %#v, err = %v", settings, err)
	}
}

func TestTimestampToggleUpdatesExistingMessagesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), displaySettingsFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	if err := model.configureDisplaySettings(path); err != nil {
		t.Fatal(err)
	}
	model.resize(80, 20)
	model.items = []transcriptItem{{
		kind: itemAssistant, text: "finished response", timestamp: time.Date(2026, time.August, 16, 13, 14, 15, 0, time.Local), done: true,
	}}
	model.refreshTranscript()
	if plain := ansi.Strip(model.View()); strings.Contains(plain, "13:14:15") {
		t.Fatalf("timestamp was visible by default:\n%s", plain)
	}
	command, handled := model.slashCommand("/timestamps")
	if !handled || command == nil || !model.showTimestamps {
		t.Fatalf("handled = %t, command = %v, shown = %t", handled, command, model.showTimestamps)
	}
	plain := ansi.Strip(model.View())
	for _, expected := range []string{"finished response", "13:14:15", "Message timestamps shown."} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("render missing %q:\n%s", expected, plain)
		}
	}
	model.Update(command())
	settings, err := loadDisplaySettings(path)
	if err != nil || !settings.ShowMessageTimestamps {
		t.Fatalf("settings = %#v, err = %v", settings, err)
	}
	command, handled = model.slashCommand("/timestamps")
	if !handled || command == nil || model.showTimestamps {
		t.Fatalf("second toggle: handled = %t, command = %v, shown = %t", handled, command, model.showTimestamps)
	}
	if plain := ansi.Strip(model.View()); strings.Contains(plain, "13:14:15") || !strings.Contains(plain, "Message timestamps hidden.") {
		t.Fatalf("hidden render:\n%s", plain)
	}
}

func TestScrollbarToggleRendersOverflowAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), displaySettingsFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	if err := model.configureDisplaySettings(path); err != nil {
		t.Fatal(err)
	}
	model.resize(80, 12)
	for range 20 {
		model.appendItem(transcriptItem{kind: itemNotice, text: "overflow row"})
	}
	model.refreshTranscript()
	if plain := ansi.Strip(model.View()); strings.Contains(plain, "█") {
		t.Fatalf("scrollbar was visible by default:\n%s", plain)
	}
	command, handled := model.slashCommand("/scrollbar")
	if !handled || command == nil || !model.showScrollbar {
		t.Fatalf("handled = %t, command = %v, shown = %t", handled, command, model.showScrollbar)
	}
	plain := ansi.Strip(model.View())
	if !strings.Contains(plain, "█") || !strings.Contains(plain, "Chat scrollbar shown.") {
		t.Fatalf("shown render:\n%s", plain)
	}
	model.Update(command())
	settings, err := loadDisplaySettings(path)
	if err != nil || !settings.ShowScrollbar {
		t.Fatalf("settings = %#v, err = %v", settings, err)
	}
	command, handled = model.slashCommand("/scrollbar")
	if !handled || command == nil || model.showScrollbar {
		t.Fatalf("second toggle: handled = %t, command = %v, shown = %t", handled, command, model.showScrollbar)
	}
	if plain := ansi.Strip(model.View()); strings.Contains(plain, "█") || !strings.Contains(plain, "Chat scrollbar hidden.") {
		t.Fatalf("hidden render:\n%s", plain)
	}
}

func TestDisplaySettingsWritesSerializeRapidToggles(t *testing.T) {
	path := filepath.Join(t.TempDir(), displaySettingsFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	if err := model.configureDisplaySettings(path); err != nil {
		t.Fatal(err)
	}
	first, handled := model.slashCommand("/timestamps")
	if !handled || first == nil {
		t.Fatalf("first toggle: handled = %t, command = %v", handled, first)
	}
	second, handled := model.slashCommand("/scrollbar")
	if !handled || second != nil {
		t.Fatalf("queued toggle: handled = %t, command = %v", handled, second)
	}
	_, followup := model.Update(first())
	if followup == nil {
		t.Fatal("queued settings were not saved after the first write")
	}
	model.Update(followup())
	settings, err := loadDisplaySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ShowMessageTimestamps || !settings.ShowScrollbar {
		t.Fatalf("settings = %#v", settings)
	}
}
