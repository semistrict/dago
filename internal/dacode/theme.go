package dacode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
)

const (
	defaultThemeName   = "langchain"
	maxThemeConfigSize = 1 << 20
)

var (
	themeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	hexColorPattern  = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
)

type themePalette struct {
	Primary       string
	Secondary     string
	Accent        string
	Panel         string
	Success       string
	Warning       string
	Error         string
	Muted         string
	ModeBash      string
	ModeCommand   string
	ModeIncognito string
	Skill         string
	SkillHover    string
	Tool          string
	ToolHover     string
	Foreground    string
	Background    string
	Surface       string
}

type themeEntry struct {
	Name    string
	Label   string
	Dark    bool
	ANSI    bool
	Custom  bool
	Palette themePalette
}

type themeRegistry map[string]themeEntry

type themePreferenceStore struct {
	mu   sync.Mutex
	path string
}

type builtinTheme struct {
	name, label                                                       string
	dark, ansi                                                        bool
	primary, secondary, accent, warning, failure, success, foreground string
	background, surface, panel                                        string
}

func darkThemePalette() themePalette {
	return themePalette{
		Primary: "#7AA2F7", Secondary: "#BB9AF7", Accent: "#9ECE6A",
		Panel: "#25283B", Success: "#9ECE6A", Warning: "#EB8B46", Error: "#F7768E",
		Muted: "#545C7E", ModeBash: "#F7768E", ModeCommand: "#BB9AF7", ModeIncognito: "#2DD4BF",
		Skill: "#A78BFA", SkillHover: "#C4B5FD", Tool: "#EB8B46", ToolHover: "#FFCB91",
		Foreground: "#C0CAF5", Background: "#11121D", Surface: "#1A1B2E",
	}
}

func lightThemePalette() themePalette {
	return themePalette{
		Primary: "#2E5EAA", Secondary: "#7C3AED", Accent: "#3A7D0A",
		Panel: "#E0E1E6", Success: "#3A7D0A", Warning: "#B45309", Error: "#BE185D",
		Muted: "#6B7280", ModeBash: "#BE185D", ModeCommand: "#7C3AED", ModeIncognito: "#0F766E",
		Skill: "#7C3AED", SkillHover: "#6D28D9", Tool: "#B45309", ToolHover: "#78350F",
		Foreground: "#24283B", Background: "#F5F5F7", Surface: "#EAEAEE",
	}
}

func builtinThemeRegistry() themeRegistry {
	registry := themeRegistry{
		"langchain":       {Name: "langchain", Label: "LangChain Dark", Dark: true, Custom: true, Palette: darkThemePalette()},
		"langchain-light": {Name: "langchain-light", Label: "LangChain Light", Custom: true, Palette: lightThemePalette()},
	}
	builtins := []builtinTheme{
		{name: "textual-dark", label: "Textual Dark", dark: true, primary: "#0178D4", secondary: "#004578", accent: "#FFA62B", warning: "#FFA62B", failure: "#BA3C5B", success: "#4EBF71", foreground: "#E0E0E0"},
		{name: "textual-light", label: "Textual Light", primary: "#004578", secondary: "#0178D4", accent: "#FFA62B", warning: "#FFA62B", failure: "#BA3C5B", success: "#4EBF71", foreground: "#24283B", background: "#E0E0E0", surface: "#D8D8D8", panel: "#D0D0D0"},
		{name: "nord", label: "Nord", dark: true, primary: "#88C0D0", secondary: "#81A1C1", accent: "#B48EAD", warning: "#EBCB8B", failure: "#BF616A", success: "#A3BE8C", foreground: "#D8DEE9", background: "#2E3440", surface: "#3B4252", panel: "#434C5E"},
		{name: "gruvbox", label: "Gruvbox", dark: true, primary: "#85A598", secondary: "#A89A85", accent: "#FABD2F", warning: "#FE8019", failure: "#FB4934", success: "#B8BB26", foreground: "#FBF1C7", background: "#282828", surface: "#3C3836", panel: "#504945"},
		{name: "catppuccin-mocha", label: "Catppuccin Mocha", dark: true, primary: "#F5C2E7", secondary: "#CBA6F7", accent: "#FAB387", warning: "#FAE3B0", failure: "#F28FAD", success: "#ABE9B3", foreground: "#CDD6F4", background: "#181825", surface: "#313244", panel: "#45475A"},
		{name: "dracula", label: "Dracula", dark: true, primary: "#BD93F9", secondary: "#6272A4", accent: "#FF79C6", warning: "#FFB86C", failure: "#FF5555", success: "#50FA7B", foreground: "#F8F8F2", background: "#282A36", surface: "#2B2E3B", panel: "#313442"},
		{name: "tokyo-night", label: "Tokyo Night", dark: true, primary: "#BB9AF7", secondary: "#7AA2F7", accent: "#FF9E64", warning: "#E0AF68", failure: "#F7768E", success: "#9ECE6A", foreground: "#A9B1D6", background: "#1A1B26", surface: "#24283B", panel: "#414868"},
		{name: "monokai", label: "Monokai", dark: true, primary: "#AE81FF", secondary: "#F92672", accent: "#66D9EF", warning: "#FD971F", failure: "#F92672", success: "#A6E22E", foreground: "#D6D6D6", background: "#272822", surface: "#2E2E2E", panel: "#3E3D32"},
		{name: "flexoki", label: "Flexoki", dark: true, primary: "#205EA6", secondary: "#24837B", accent: "#9B76C8", warning: "#AD8301", failure: "#AF3029", success: "#66800B", foreground: "#FFFCF0", background: "#100F0F", surface: "#1C1B1A", panel: "#282726"},
		{name: "catppuccin-latte", label: "Catppuccin Latte", primary: "#8839EF", secondary: "#DC8A78", accent: "#FE640B", warning: "#DF8E1D", failure: "#D20F39", success: "#40A02B", foreground: "#4C4F69", background: "#EFF1F5", surface: "#E6E9EF", panel: "#CCD0DA"},
		{name: "catppuccin-frappe", label: "Catppuccin Frappé", dark: true, primary: "#CA9EE6", secondary: "#EF9F76", accent: "#F4B8E4", warning: "#E5C890", failure: "#E78284", success: "#A6D189", foreground: "#C6D0F5", background: "#303446", surface: "#414559", panel: "#51576D"},
		{name: "catppuccin-macchiato", label: "Catppuccin Macchiato", dark: true, primary: "#C6A0F6", secondary: "#F5A97F", accent: "#F5BDE6", warning: "#EED49F", failure: "#ED8796", success: "#A6DA95", foreground: "#CAD3F5", background: "#24273A", surface: "#363A4F", panel: "#494D64"},
		{name: "solarized-light", label: "Solarized Light", primary: "#268BD2", secondary: "#2AA198", accent: "#6C71C4", warning: "#CB4B16", failure: "#DC322F", success: "#859900", foreground: "#586E75", background: "#FDF6E3", surface: "#EEE8D5", panel: "#EEE8D5"},
		{name: "solarized-dark", label: "Solarized Dark", dark: true, primary: "#268BD2", secondary: "#2AA198", accent: "#6C71C4", warning: "#CB4B16", failure: "#DC322F", success: "#859900", foreground: "#839496", background: "#002B36", surface: "#073642", panel: "#073642"},
		{name: "rose-pine", label: "Rosé Pine", dark: true, primary: "#C4A7E7", secondary: "#31748F", accent: "#EBBCBA", warning: "#F6C177", failure: "#EB6F92", success: "#9CCFD8", foreground: "#E0DEF4", background: "#191724", surface: "#1F1D2E", panel: "#26233A"},
		{name: "rose-pine-moon", label: "Rosé Pine Moon", dark: true, primary: "#C4A7E7", secondary: "#3E8FB0", accent: "#EA9A97", warning: "#F6C177", failure: "#EB6F92", success: "#9CCFD8", foreground: "#E0DEF4", background: "#232136", surface: "#2A273F", panel: "#393552"},
		{name: "rose-pine-dawn", label: "Rosé Pine Dawn", primary: "#907AA9", secondary: "#286983", accent: "#D7827E", warning: "#EA9D34", failure: "#B4637A", success: "#56949F", foreground: "#575279", background: "#FAF4ED", surface: "#FFFAF3", panel: "#F2E9E1"},
		{name: "atom-one-dark", label: "Atom One Dark", dark: true, primary: "#61AFEF", secondary: "#C678DD", accent: "#A378C2", warning: "#DEB25B", failure: "#F06262", success: "#62F062", foreground: "#ABB2BF", background: "#282C34", surface: "#3B414D", panel: "#4F5666"},
		{name: "atom-one-light", label: "Atom One Light", primary: "#4078F2", secondary: "#A626A4", accent: "#BF9232", warning: "#D8D938", failure: "#F23F3F", success: "#6CF23F", foreground: "#383A42", background: "#FAFAFA", surface: "#E0E0E0", panel: "#CCCCCC"},
	}
	for _, builtin := range builtins {
		registry[builtin.name] = themeEntry{Name: builtin.name, Label: builtin.label, Dark: builtin.dark, Palette: paletteFromBuiltin(builtin)}
	}
	registry["ansi-dark"] = themeEntry{Name: "ansi-dark", Label: "Terminal ANSI Dark", Dark: true, ANSI: true, Palette: ansiThemePalette(true)}
	registry["ansi-light"] = themeEntry{Name: "ansi-light", Label: "Terminal ANSI Light", ANSI: true, Palette: ansiThemePalette(false)}
	return registry
}

func paletteFromBuiltin(theme builtinTheme) themePalette {
	palette := lightThemePalette()
	if theme.dark {
		palette = darkThemePalette()
	}
	set := func(target *string, value string) {
		if value != "" {
			*target = strings.ToUpper(value)
		}
	}
	set(&palette.Primary, theme.primary)
	set(&palette.Secondary, theme.secondary)
	set(&palette.Accent, theme.accent)
	set(&palette.Warning, theme.warning)
	set(&palette.Error, theme.failure)
	set(&palette.Success, theme.success)
	set(&palette.Foreground, theme.foreground)
	set(&palette.Background, theme.background)
	set(&palette.Surface, theme.surface)
	set(&palette.Panel, theme.panel)
	palette.ModeBash, palette.ModeCommand, palette.ModeIncognito = palette.Error, palette.Secondary, palette.Accent
	palette.Skill, palette.SkillHover = palette.Secondary, palette.Primary
	palette.Tool, palette.ToolHover = palette.Warning, palette.Accent
	return palette
}

func ansiThemePalette(dark bool) themePalette {
	palette := themePalette{
		Primary: "4", Secondary: "6", Accent: "2", Panel: "", Success: "2", Warning: "3", Error: "1",
		Muted: "8", ModeBash: "1", ModeCommand: "5", ModeIncognito: "6", Skill: "5", SkillHover: "13",
		Tool: "3", ToolHover: "11", Foreground: "", Background: "", Surface: "",
	}
	if !dark {
		palette.Warning = "9"
	}
	return palette
}

func loadThemeRegistry(path string) (themeRegistry, []string) {
	registry := builtinThemeRegistry()
	if strings.TrimSpace(path) == "" {
		return registry, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return registry, nil
		}
		return registry, []string{"theme configuration is unreadable"}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxThemeConfigSize {
		return registry, []string{"theme configuration must be a bounded regular file"}
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxThemeConfigSize+1))
	if err != nil || len(payload) > maxThemeConfigSize {
		return registry, []string{"theme configuration could not be read safely"}
	}
	var document struct {
		Themes map[string]map[string]any `toml:"themes"`
	}
	if _, err := toml.Decode(string(payload), &document); err != nil {
		return registry, []string{"theme configuration is malformed"}
	}
	names := make([]string, 0, len(document.Themes))
	for name := range document.Themes {
		names = append(names, name)
	}
	sort.Strings(names)
	diagnostics := []string{}
	for _, name := range names {
		entry, ok, reason := mergeUserTheme(registry, name, document.Themes[name])
		if !ok {
			diagnostics = append(diagnostics, fmt.Sprintf("theme %q ignored: %s", name, reason))
			continue
		}
		registry[name] = entry
	}
	return registry, diagnostics
}

func resolveThemeName(registry themeRegistry, value any) (string, bool) {
	name, ok := value.(string)
	if !ok {
		return "", false
	}
	name = strings.TrimSpace(name)
	if name == "textual-ansi" {
		name = "ansi-light"
	}
	if _, ok := registry[name]; ok {
		return name, true
	}
	for registered, entry := range registry {
		if strings.EqualFold(registered, name) || strings.EqualFold(entry.Label, name) {
			return registered, true
		}
	}
	return "", false
}

func loadThemePreference(path, terminal string, lookup func(string) (string, bool)) (string, string, []string) {
	registry, diagnostics := loadThemeRegistry(path)
	if lookup != nil {
		if value, exists := lookup("DEEPAGENTS_CODE_THEME"); exists {
			if name, ok := resolveThemeName(registry, value); ok {
				return name, "", diagnostics
			}
			return defaultThemeName, "", append(diagnostics, "configured theme override is unknown")
		}
	}
	document, err := readThemeDocument(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			diagnostics = append(diagnostics, "theme preference is unavailable")
		}
		return defaultThemeName, "", diagnostics
	}
	ui, _ := document["ui"].(map[string]any)
	terminal = strings.TrimSpace(terminal)
	terminalDefault := ""
	if mappings, ok := ui["terminal_themes"].(map[string]any); ok && terminal != "" {
		if name, ok := resolveThemeName(registry, mappings[terminal]); ok {
			terminalDefault = name
			return name, terminalDefault, diagnostics
		}
	}
	if name, ok := resolveThemeName(registry, ui["theme"]); ok {
		return name, terminalDefault, diagnostics
	}
	return defaultThemeName, terminalDefault, diagnostics
}

func newThemePreferenceStore(path string) *themePreferenceStore {
	return &themePreferenceStore{path: path}
}

func (store *themePreferenceStore) saveGlobal(registry themeRegistry, name string) error {
	return store.update(registry, name, "")
}

func (store *themePreferenceStore) saveTerminal(registry themeRegistry, terminal, name string) error {
	terminal = strings.TrimSpace(terminal)
	if terminal == "" || utf8.RuneCountInString(terminal) > 128 || strings.ContainsAny(terminal, "\x00\r\n") {
		return errors.New("terminal name is unavailable")
	}
	return store.update(registry, name, terminal)
}

func (store *themePreferenceStore) update(registry themeRegistry, name, terminal string) error {
	if store == nil || strings.TrimSpace(store.path) == "" {
		return errors.New("theme preference path is unavailable")
	}
	if _, ok := registry[name]; !ok {
		return errors.New("theme is unavailable")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	configurationPreferenceWriteMu.Lock()
	defer configurationPreferenceWriteMu.Unlock()
	document, err := readThemeDocument(store.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("read theme preferences")
	}
	if document == nil {
		document = map[string]any{}
	}
	ui, ok := document["ui"].(map[string]any)
	if !ok {
		ui = map[string]any{}
		document["ui"] = ui
	}
	if terminal == "" {
		ui["theme"] = name
	} else {
		mappings, ok := ui["terminal_themes"].(map[string]any)
		if !ok {
			mappings = map[string]any{}
			ui["terminal_themes"] = mappings
		}
		mappings[terminal] = name
	}
	var payload bytes.Buffer
	if err := toml.NewEncoder(&payload).Encode(document); err != nil || payload.Len() > maxThemeConfigSize {
		return errors.New("encode theme preferences")
	}
	return replaceThemeConfig(store.path, payload.Bytes())
}

func readThemeDocument(path string) (map[string]any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, os.ErrNotExist
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 || !linkInfo.Mode().IsRegular() || linkInfo.Size() > maxThemeConfigSize {
		return nil, errors.New("theme configuration is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxThemeConfigSize || !os.SameFile(linkInfo, info) {
		return nil, errors.New("theme configuration is not a bounded regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxThemeConfigSize+1))
	if err != nil || len(payload) > maxThemeConfigSize {
		return nil, errors.New("theme configuration is too large")
	}
	document := map[string]any{}
	if _, err := toml.Decode(string(payload), &document); err != nil {
		return nil, errors.New("theme configuration is malformed")
	}
	return document, nil
}

func replaceThemeConfig(path string, payload []byte) (err error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".theme-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func mergeUserTheme(registry themeRegistry, name string, section map[string]any) (themeEntry, bool, string) {
	if !themeNamePattern.MatchString(name) {
		return themeEntry{}, false, "invalid name"
	}
	existing, builtin := registry[name]
	dark := false
	label := ""
	if builtin {
		dark, label = existing.Dark, existing.Label
	} else {
		value, exists := section["label"]
		var valid bool
		label, valid = value.(string)
		if !exists || !valid || strings.TrimSpace(label) == "" || utf8.RuneCountInString(label) > 80 || strings.ContainsAny(label, "\x00\r\n") {
			return themeEntry{}, false, "label is required"
		}
		if value, exists := section["dark"]; exists {
			var valid bool
			dark, valid = value.(bool)
			if !valid {
				return themeEntry{}, false, "dark must be true or false"
			}
		}
	}
	palette := lightThemePalette()
	if dark {
		palette = darkThemePalette()
	}
	if builtin {
		palette = existing.Palette
	}
	for key, value := range section {
		if key == "label" || key == "dark" {
			continue
		}
		color, valid := value.(string)
		if !valid {
			return themeEntry{}, false, "color fields must be strings"
		}
		if !hexColorPattern.MatchString(color) {
			return themeEntry{}, false, "colors must use #RRGGBB"
		}
		if !setThemeColor(&palette, key, strings.ToUpper(color)) {
			return themeEntry{}, false, "unknown color field"
		}
	}
	return themeEntry{Name: name, Label: strings.TrimSpace(label), Dark: dark, Custom: true, Palette: palette}, true, ""
}

func setThemeColor(palette *themePalette, name, value string) bool {
	targets := map[string]*string{
		"primary": &palette.Primary, "secondary": &palette.Secondary, "accent": &palette.Accent,
		"panel": &palette.Panel, "success": &palette.Success, "warning": &palette.Warning,
		"error": &palette.Error, "muted": &palette.Muted, "mode_bash": &palette.ModeBash,
		"mode_command": &palette.ModeCommand, "mode_incognito": &palette.ModeIncognito,
		"skill": &palette.Skill, "skill_hover": &palette.SkillHover, "tool": &palette.Tool,
		"tool_hover": &palette.ToolHover, "foreground": &palette.Foreground,
		"background": &palette.Background, "surface": &palette.Surface,
	}
	target := targets[name]
	if target == nil {
		return false
	}
	*target = value
	return true
}

func (registry themeRegistry) names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Slice(names, func(left, right int) bool {
		if registry[names[left]].Label == registry[names[right]].Label {
			return names[left] < names[right]
		}
		return registry[names[left]].Label < registry[names[right]].Label
	})
	return names
}

func applyThemePalette(palette themePalette) {
	colorBackground = lipgloss.Color(palette.Background)
	colorSurface = lipgloss.Color(palette.Surface)
	colorPanel = lipgloss.Color(palette.Panel)
	colorBody = lipgloss.Color(palette.Foreground)
	colorPrimary = lipgloss.Color(palette.Primary)
	colorSecondary = lipgloss.Color(palette.Secondary)
	colorSuccess = lipgloss.Color(palette.Success)
	colorWarning = lipgloss.Color(palette.Warning)
	colorError = lipgloss.Color(palette.Error)
	colorMuted = lipgloss.Color(palette.Muted)
}
