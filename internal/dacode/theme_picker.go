package dacode

import (
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type themePickerState struct {
	names                  []string
	selected               int
	original               string
	terminalDefault        string
	sessionTerminalDefault string
	showKeys               bool
}

func newThemePicker(registry themeRegistry, current, terminalDefault string) *themePickerState {
	names := registry.names()
	selected := 0
	for index, name := range names {
		if name == current {
			selected = index
			break
		}
	}
	return &themePickerState{names: names, selected: selected, original: current, terminalDefault: terminalDefault}
}

func (model *tuiModel) configureTheme(path, terminal string) []string {
	registry, diagnostics := loadThemeRegistry(path)
	name, terminalDefault, preferenceDiagnostics := loadThemePreference(path, terminal, os.LookupEnv)
	glyphs, charset, charsetDiagnostics := resolveUIGlyphs(os.LookupEnv, os.Getenv("LC_CTYPE"))
	kitty, kittyDiagnostics := supportsKittyKeyboard(os.LookupEnv, true, true)
	cursor, cursorDiagnostics := loadCursorPreference(path, os.LookupEnv)
	model.themeRegistry = registry
	model.themeStore = newThemePreferenceStore(path)
	model.notificationStore = newNotificationPreferenceStore(path)
	model.terminalTheme = terminalDefault
	model.glyphs, model.charset = glyphs, charset
	model.kittyKeyboard, model.newlineLabel, model.cursor = kitty, newlineShortcut(kitty), cursor
	model.spinner.Spinner = spinner.Spinner{Frames: append([]string(nil), glyphs.SpinnerFrames...), FPS: time.Second / 10}
	model.applyCursorPreference(true)
	model.setTheme(name)
	seen := map[string]bool{}
	allDiagnostics := append(append(append(append(diagnostics, preferenceDiagnostics...), charsetDiagnostics...), kittyDiagnostics...), cursorDiagnostics...)
	unique := make([]string, 0, len(allDiagnostics))
	for _, diagnostic := range allDiagnostics {
		if diagnostic != "" && !seen[diagnostic] {
			seen[diagnostic] = true
			unique = append(unique, diagnostic)
		}
	}
	return unique
}

func (model *tuiModel) applyCursorPreference(active bool) {
	if !active {
		model.composer.Cursor.Style = lipgloss.NewStyle().Foreground(colorMuted)
		model.composer.Cursor.SetMode(cursor.CursorStatic)
		return
	}
	if model.cursor.Style == cursorUnderline {
		model.composer.Cursor.Style = lipgloss.NewStyle().Foreground(colorPrimary).Underline(true)
	} else {
		model.composer.Cursor.Style = lipgloss.NewStyle().Background(colorPrimary).Foreground(colorBackground)
	}
	mode := cursor.CursorStatic
	if model.cursor.Blink {
		mode = cursor.CursorBlink
	}
	model.composer.Cursor.SetMode(mode)
}

func (model *tuiModel) setTheme(name string) bool {
	entry, ok := model.themeRegistry[name]
	if !ok {
		return false
	}
	applyThemePalette(entry.Palette)
	if entry.ANSI || entry.Palette.Background == "" {
		model.themeSequence = terminalBackgroundResetSequence()
	} else {
		model.themeSequence = terminalBackgroundSequence(entry.Palette.Background)
	}
	model.themeName = name
	model.composer.FocusedStyle.Base = lipgloss.NewStyle().Foreground(colorBody)
	model.composer.FocusedStyle.CursorLine = lipgloss.NewStyle()
	model.composer.FocusedStyle.Text = lipgloss.NewStyle().Foreground(colorBody)
	model.composer.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
	model.composer.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colorMuted)
	model.composer.BlurredStyle = model.composer.FocusedStyle
	model.spinner.Style = lipgloss.NewStyle().Foreground(colorPrimary)
	model.relayout()
	model.refreshTranscript()
	return true
}

func (model *tuiModel) handleThemeKey(message tea.KeyMsg) (tea.Cmd, bool) {
	picker := model.themePicker
	if picker == nil {
		return nil, false
	}
	if len(picker.names) == 0 {
		model.themePicker = nil
		return nil, true
	}
	preview := func() {
		model.setTheme(picker.names[picker.selected])
	}
	switch message.String() {
	case "up", "shift+tab":
		picker.selected = (picker.selected - 1 + len(picker.names)) % len(picker.names)
		preview()
	case "down", "tab":
		picker.selected = (picker.selected + 1) % len(picker.names)
		preview()
	case "n", "N":
		picker.showKeys = !picker.showKeys
	case "t", "T":
		terminal := strings.TrimSpace(os.Getenv("TERM_PROGRAM"))
		if terminal == "" {
			model.appendItem(transcriptItem{kind: itemError, text: "TERM_PROGRAM is unset; this terminal cannot have its own theme default."})
			model.refreshTranscript()
			return nil, true
		}
		name := picker.names[picker.selected]
		original := picker.original
		model.setTheme(name)
		return func() tea.Msg {
			return themePreferenceSavedMsg{name: name, terminal: terminal, original: original, err: model.themeStore.saveTerminal(model.themeRegistry, terminal, name)}
		}, true
	case "enter":
		name := picker.names[picker.selected]
		model.setTheme(name)
		model.themePicker = nil
		return func() tea.Msg {
			return themePreferenceSavedMsg{name: name, err: model.themeStore.saveGlobal(model.themeRegistry, name)}
		}, true
	case "esc":
		target := picker.original
		if picker.sessionTerminalDefault != "" {
			target = picker.sessionTerminalDefault
		}
		model.setTheme(target)
		model.themePicker = nil
	default:
		return nil, true
	}
	return nil, true
}

func (model *tuiModel) renderThemePicker() string {
	picker := model.themePicker
	if picker == nil {
		return ""
	}
	if model.width < 40 || model.height < 14 {
		entry := model.themeRegistry[picker.names[picker.selected]]
		label := entry.Label
		if picker.showKeys {
			label = entry.Name
		}
		label = ansi.Truncate(model.glyphs.Cursor+" "+label, max(model.width-6, 1), model.glyphs.Ellipsis)
		if model.height <= 10 {
			return lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorPrimary).
				Width(max(model.width-4, 1)).Render(lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(label))
		}
		lines := []string{
			lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Select Theme"),
			lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(label),
			lipgloss.NewStyle().Foreground(colorMuted).Render(model.glyphs.ArrowUp + "/" + model.glyphs.ArrowDown + " Enter Esc"),
		}
		panel := lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorPrimary).
			Padding(0, 1).Width(max(model.width-4, 1)).Render(strings.Join(lines, "\n"))
		return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
	}
	contentWidth := min(max(model.width-8, 12), 76)
	lines := []string{lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Select Theme"), ""}
	visible := min(len(picker.names), max(model.height-9, 1))
	start := max(picker.selected-visible/2, 0)
	start = min(start, max(len(picker.names)-visible, 0))
	for index := start; index < start+visible; index++ {
		name := picker.names[index]
		entry := model.themeRegistry[name]
		label := entry.Label
		if picker.showKeys {
			label = name
		}
		suffixes := []string{}
		if name == model.themeName {
			suffixes = append(suffixes, "current")
		}
		if name == picker.terminalDefault {
			suffixes = append(suffixes, "default")
		}
		if len(suffixes) > 0 {
			label += " (" + strings.Join(suffixes, ", ") + ")"
		}
		prefix := "  "
		style := lipgloss.NewStyle().Foreground(colorBody).Padding(0, 1).Width(contentWidth)
		if index == picker.selected {
			prefix = model.glyphs.Cursor + " "
			style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
		}
		lines = append(lines, style.Render(prefix+label))
	}
	separator := "  " + model.glyphs.Bullet + "  "
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(model.glyphs.ArrowUp+"/"+model.glyphs.ArrowDown+" or Tab preview"+separator+"Enter save"+separator+"Esc cancel"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("N labels/keys"+separator+"T set for this terminal"))
	panel := lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorPrimary).
		Padding(1, 2).Width(min(max(model.width-6, 1), 80)).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
}
