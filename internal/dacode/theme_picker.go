package dacode

import (
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	lipgloss2 "charm.land/lipgloss/v2"
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
	styles := model.composer.Styles()
	if !active {
		styles.Cursor.Color = lipgloss2.Color(string(colorMuted))
		styles.Cursor.Blink = false
		model.composer.SetStyles(styles)
		return
	}
	if model.cursor.Style == cursorUnderline {
		styles.Cursor.Shape = tea.CursorUnderline
	} else {
		styles.Cursor.Shape = tea.CursorBlock
	}
	styles.Cursor.Color = lipgloss2.Color(string(colorPrimary))
	styles.Cursor.Blink = model.cursor.Blink
	model.composer.SetStyles(styles)
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
	styleComposer(&model.composer)
	model.spinner.Style = lipgloss2.NewStyle().Foreground(lipgloss2.Color(string(colorPrimary)))
	model.relayout()
	model.refreshTranscript()
	return true
}

func styleComposer(composer *textarea.Model) {
	styles := composer.Styles()
	styles.Focused.Base = lipgloss2.NewStyle().Foreground(lipgloss2.Color(string(colorBody)))
	styles.Focused.CursorLine = lipgloss2.NewStyle()
	styles.Focused.Text = lipgloss2.NewStyle().Foreground(lipgloss2.Color(string(colorBody)))
	styles.Focused.Prompt = lipgloss2.NewStyle().Foreground(lipgloss2.Color(string(colorPrimary))).Bold(true)
	styles.Focused.Placeholder = lipgloss2.NewStyle().Foreground(lipgloss2.Color(string(colorMuted)))
	styles.Blurred = styles.Focused
	composer.SetStyles(styles)
}

func (model *tuiModel) handleThemeKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	picker := model.themePicker
	if picker == nil {
		return nil, false
	}
	if len(picker.names) == 0 {
		model.themePicker = nil
		return nil, true
	}
	var themeCommand tea.Cmd
	preview := func() {
		model.setTheme(picker.names[picker.selected])
		themeCommand = tea.Raw(model.themeSequence)
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
		save := func() tea.Msg {
			return themePreferenceSavedMsg{name: name, terminal: terminal, original: original, err: model.themeStore.saveTerminal(model.themeRegistry, terminal, name)}
		}
		return tea.Batch(tea.Raw(model.themeSequence), save), true
	case "enter":
		name := picker.names[picker.selected]
		model.setTheme(name)
		model.themePicker = nil
		save := func() tea.Msg {
			return themePreferenceSavedMsg{name: name, err: model.themeStore.saveGlobal(model.themeRegistry, name)}
		}
		return tea.Batch(tea.Raw(model.themeSequence), save), true
	case "esc":
		target := picker.original
		if picker.sessionTerminalDefault != "" {
			target = picker.sessionTerminalDefault
		}
		model.setTheme(target)
		model.themePicker = nil
		themeCommand = tea.Raw(model.themeSequence)
	default:
		return nil, true
	}
	return themeCommand, true
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
