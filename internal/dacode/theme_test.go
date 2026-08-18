package dacode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBuiltinThemeRegistryContainsPinnedCatalog(t *testing.T) {
	registry := builtinThemeRegistry()
	want := []string{
		"langchain", "langchain-light", "textual-dark", "textual-light", "nord", "gruvbox",
		"catppuccin-mocha", "dracula", "tokyo-night", "monokai", "flexoki", "catppuccin-latte",
		"catppuccin-frappe", "catppuccin-macchiato", "solarized-light", "solarized-dark",
		"rose-pine", "rose-pine-moon", "rose-pine-dawn", "atom-one-dark", "atom-one-light",
		"ansi-dark", "ansi-light",
	}
	for _, name := range want {
		entry, ok := registry[name]
		if !ok {
			t.Errorf("missing built-in theme %q", name)
			continue
		}
		if entry.Name != name || strings.TrimSpace(entry.Label) == "" {
			t.Errorf("theme %q metadata = %#v", name, entry)
		}
	}
	if !registry["ansi-dark"].ANSI || !registry["ansi-light"].ANSI {
		t.Fatal("ANSI themes are not marked as terminal-palette themes")
	}
	if got := registry[defaultThemeName].Palette.Background; got != "#11121D" {
		t.Fatalf("default background = %q", got)
	}
}

func TestLoadThemeRegistryMergesCustomThemesAndBuiltinOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	payload := `[unrelated]
value = "preserved"

[themes.langchain]
primary = "#123456"

[themes.paper]
label = "Paper"
primary = "#010203"

[themes.midnight]
label = "Midnight"
dark = true
surface = "#202020"
`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, diagnostics := loadThemeRegistry(path)
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if entry := registry["langchain"]; entry.Label != "LangChain Dark" || entry.Palette.Primary != "#123456" || entry.Palette.Background != "#11121D" {
		t.Fatalf("built-in override = %#v", entry)
	}
	if entry := registry["paper"]; entry.Dark || entry.Palette.Primary != "#010203" || entry.Palette.Background != "#F5F5F7" {
		t.Fatalf("light custom theme = %#v", entry)
	}
	if entry := registry["midnight"]; !entry.Dark || entry.Palette.Surface != "#202020" || entry.Palette.Background != "#11121D" {
		t.Fatalf("dark custom theme = %#v", entry)
	}
}

func TestLoadThemeRegistrySkipsInvalidEntriesWithoutDiscardingValidOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	payload := `[themes.good]
label = "Good"
warning = "#ABCDEF"

[themes.missing-label]
dark = true

[themes.bad-color]
label = "Bad"
primary = "red"

[themes.unknown-field]
label = "Unknown"
border = "#010203"
`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, diagnostics := loadThemeRegistry(path)
	if _, ok := registry["good"]; !ok {
		t.Fatal("valid theme was discarded")
	}
	for _, name := range []string{"missing-label", "bad-color", "unknown-field"} {
		if _, ok := registry[name]; ok {
			t.Errorf("invalid theme %q was loaded", name)
		}
	}
	if len(diagnostics) != 3 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if strings.Contains(strings.Join(diagnostics, " "), "#010203") {
		t.Fatalf("diagnostics leaked configuration values: %v", diagnostics)
	}
}

func TestLoadThemeRegistryUsesDefaultsForMissingMalformedAndOversizedFiles(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T) string
		warn    bool
	}{
		{name: "missing", prepare: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.toml") }},
		{name: "malformed", warn: true, prepare: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[themes.bad"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "oversized", warn: true, prepare: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, make([]byte, maxThemeConfigSize+1), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry, diagnostics := loadThemeRegistry(test.prepare(t))
			if _, ok := registry[defaultThemeName]; !ok {
				t.Fatal("default theme unavailable")
			}
			if (len(diagnostics) > 0) != test.warn {
				t.Fatalf("diagnostics = %v", diagnostics)
			}
		})
	}
}

func TestThemeNamesAreDeterministicAndDetached(t *testing.T) {
	registry := builtinThemeRegistry()
	first := registry.names()
	second := registry.names()
	if strings.Join(first, "\x00") != strings.Join(second, "\x00") {
		t.Fatalf("theme names are not deterministic: %v vs %v", first, second)
	}
	first[0] = "changed"
	if second[0] == "changed" {
		t.Fatal("theme names alias caller state")
	}
}

func TestThemePreferenceResolutionPrecedenceAndAliases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	payload := `[ui]
theme = "LangChain Light"

[ui.terminal_themes]
WarpTerminal = "Tokyo Night"
`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, terminal, environment, want, terminalDefault string
	}{
		{name: "global", want: "langchain-light"},
		{name: "terminal", terminal: "WarpTerminal", want: "tokyo-night", terminalDefault: "tokyo-night"},
		{name: "environment label", terminal: "WarpTerminal", environment: "Nord", want: "nord"},
		{name: "legacy environment", environment: "textual-ansi", want: "ansi-light"},
		{name: "invalid environment fails to default", terminal: "WarpTerminal", environment: "missing", want: defaultThemeName},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(string) (string, bool) { return test.environment, test.environment != "" }
			got, terminalDefault, _ := loadThemePreference(path, test.terminal, lookup)
			if got != test.want || terminalDefault != test.terminalDefault {
				t.Fatalf("preference = %q, %q; want %q, %q", got, terminalDefault, test.want, test.terminalDefault)
			}
		})
	}
}

func TestThemePreferenceStorePreservesConfigurationAndWritesPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[mcp]\ndisabled_project_servers = [\"tools\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := builtinThemeRegistry()
	store := newThemePreferenceStore(path)
	if err := store.saveGlobal(registry, "nord"); err != nil {
		t.Fatal(err)
	}
	if err := store.saveTerminal(registry, "WarpTerminal", "ansi-dark"); err != nil {
		t.Fatal(err)
	}
	document, err := readThemeDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if document["mcp"] == nil {
		t.Fatal("unrelated configuration was discarded")
	}
	ui := document["ui"].(map[string]any)
	if ui["theme"] != "nord" || ui["terminal_themes"].(map[string]any)["WarpTerminal"] != "ansi-dark" {
		t.Fatalf("saved UI configuration = %#v", ui)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := store.saveGlobal(registry, "missing"); err == nil {
		t.Fatal("unknown theme was saved")
	}
	if err := store.saveTerminal(registry, "", "nord"); err == nil {
		t.Fatal("empty terminal name was saved")
	}
}

func TestThemePreferenceStoreSerializesOverlappingWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	store := newThemePreferenceStore(path)
	registry := builtinThemeRegistry()
	errors := make(chan error, 2)
	go func() { errors <- store.saveGlobal(registry, "nord") }()
	go func() { errors <- store.saveTerminal(registry, "Terminal", "ansi-light") }()
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	document, err := readThemeDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	ui := document["ui"].(map[string]any)
	if ui["theme"] != "nord" || ui["terminal_themes"].(map[string]any)["Terminal"] != "ansi-light" {
		t.Fatalf("overlapping writes lost data: %#v", ui)
	}
}

func TestThemePickerPreviewsCancelsAndPersistsSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.configureTheme(path, "")
	model.resize(100, 30)
	if _, ok := model.slashCommand("/theme"); !ok || model.themePicker == nil {
		t.Fatal("/theme did not open the picker")
	}
	original := model.themeName
	command, handled := model.handleThemeKey(tea.KeyMsg{Type: tea.KeyDown})
	if !handled || command != nil || model.themeName == original {
		t.Fatalf("down did not preview: handled=%v command=%v theme=%q", handled, command != nil, model.themeName)
	}
	if _, handled := model.handleThemeKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled || model.themeName != original || model.themePicker != nil {
		t.Fatalf("Esc did not restore %q", original)
	}

	model.themePicker = newThemePicker(model.themeRegistry, model.themeName, model.terminalTheme)
	model.themePicker.selected = indexTheme(model.themePicker.names, "nord")
	command, handled = model.handleThemeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil || model.themeName != "nord" || model.themePicker != nil {
		t.Fatalf("Enter did not commit Nord: handled=%v command=%v theme=%q", handled, command != nil, model.themeName)
	}
	message, ok := command().(themePreferenceSavedMsg)
	if !ok || message.err != nil {
		t.Fatalf("save message = %#v", message)
	}
	restarted := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	restarted.configureTheme(path, "")
	if restarted.themeName != "nord" {
		t.Fatalf("restored theme = %q", restarted.themeName)
	}
}

func TestThemePickerTerminalDefaultAndLabels(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ThemeTestTerminal")
	path := filepath.Join(t.TempDir(), "config.toml")
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.configureTheme(path, "ThemeTestTerminal")
	model.resize(100, 30)
	model.themePicker = newThemePicker(model.themeRegistry, model.themeName, model.terminalTheme)
	model.themePicker.selected = indexTheme(model.themePicker.names, "ansi-dark")
	if _, handled := model.handleThemeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}); !handled || !model.themePicker.showKeys {
		t.Fatal("n did not toggle canonical keys")
	}
	command, handled := model.handleThemeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if !handled || command == nil || model.themeName != "ansi-dark" {
		t.Fatalf("t did not select terminal theme: handled=%v command=%v theme=%q", handled, command != nil, model.themeName)
	}
	message := command().(themePreferenceSavedMsg)
	updated, _ := model.Update(message)
	model = updated.(*tuiModel)
	if model.terminalTheme != "ansi-dark" || !strings.Contains(model.renderThemePicker(), "ansi-dark (current, default)") {
		t.Fatalf("terminal default not reflected: %q\n%s", model.terminalTheme, model.renderThemePicker())
	}
	if _, handled := model.handleThemeKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled || model.themeName != "ansi-dark" {
		t.Fatal("Esc reverted a terminal default chosen in this picker")
	}
}

func indexTheme(names []string, target string) int {
	for index, name := range names {
		if name == target {
			return index
		}
	}
	return -1
}
