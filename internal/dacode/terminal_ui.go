package dacode

import (
	"errors"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type charsetMode string

const (
	charsetUnicode charsetMode = "unicode"
	charsetASCII   charsetMode = "ascii"
	charsetAuto    charsetMode = "auto"
)

type uiGlyphs struct {
	ToolPrefix         string
	Ellipsis           string
	Checkmark          string
	Error              string
	CircleEmpty        string
	CircleFilled       string
	OutputPrefix       string
	SpinnerFrames      []string
	Pause              string
	Newline            string
	Warning            string
	Question           string
	Hourglass          string
	Retry              string
	ArrowUp            string
	ArrowDown          string
	ArrowLeft          string
	ArrowRight         string
	Bullet             string
	Separator          string
	Dash               string
	Cursor             string
	DisclosureClosed   string
	DisclosureExpanded string
	BoxHorizontal      string
	BoxVertical        string
	BoxTopLeft         string
	BoxTopRight        string
	BoxBottomLeft      string
	BoxBottomRight     string
	BoxHeavy           string
	HunkBreak          string
	GitBranch          string
}

var unicodeUIGlyphs = uiGlyphs{
	ToolPrefix: "⏺", Ellipsis: "…", Checkmark: "✓", Error: "✗", CircleEmpty: "○", CircleFilled: "●", OutputPrefix: "⎿",
	SpinnerFrames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	Pause:         "⏸", Newline: "⏎", Warning: "⚠", Question: "?", Hourglass: "⏳", Retry: "↻",
	ArrowUp: "↑", ArrowDown: "↓", ArrowLeft: "←", ArrowRight: "→", Bullet: "•", Separator: "•", Dash: "—", Cursor: "›", DisclosureClosed: "▸", DisclosureExpanded: "▾",
	BoxHorizontal: "─", BoxVertical: "│", BoxTopLeft: "╭", BoxTopRight: "╮", BoxBottomLeft: "╰", BoxBottomRight: "╯",
	BoxHeavy: "━", HunkBreak: "⋮", GitBranch: "↗",
}

var asciiUIGlyphs = uiGlyphs{
	ToolPrefix: "(*)", Ellipsis: "...", Checkmark: "[OK]", Error: "[X]", CircleEmpty: "[ ]", CircleFilled: "[*]", OutputPrefix: "L",
	SpinnerFrames: []string{"(-)", "(\\)", "(|)", "(/)"},
	Pause:         "[||]", Newline: "\\n", Warning: "[!]", Question: "[?]", Hourglass: "[~]", Retry: "[R]",
	ArrowUp: "^", ArrowDown: "v", ArrowLeft: "<", ArrowRight: ">", Bullet: "-", Separator: "-", Dash: "-", Cursor: ">", DisclosureClosed: ">", DisclosureExpanded: "v",
	BoxHorizontal: "-", BoxVertical: "|", BoxTopLeft: "+", BoxTopRight: "+", BoxBottomLeft: "+", BoxBottomRight: "+",
	BoxHeavy: "=", HunkBreak: ":", GitBranch: "git:",
}

func uiGlyphsForASCII(ascii bool) uiGlyphs {
	if ascii {
		return cloneUIGlyphs(asciiUIGlyphs)
	}
	return cloneUIGlyphs(unicodeUIGlyphs)
}

func uiBorder(glyphs uiGlyphs) lipgloss.Border {
	return lipgloss.Border{
		Top: glyphs.BoxHorizontal, Bottom: glyphs.BoxHorizontal,
		Left: glyphs.BoxVertical, Right: glyphs.BoxVertical,
		TopLeft: glyphs.BoxTopLeft, TopRight: glyphs.BoxTopRight,
		BottomLeft: glyphs.BoxBottomLeft, BottomRight: glyphs.BoxBottomRight,
	}
}

func resolveUIGlyphs(lookup func(string) (string, bool), outputEncoding string) (uiGlyphs, charsetMode, []string) {
	mode := charsetAuto
	diagnostics := []string{}
	if lookup != nil {
		if raw, exists := lookup("DEEPAGENTS_CODE_UI_CHARSET_MODE"); exists {
			switch charsetMode(strings.ToLower(strings.TrimSpace(raw))) {
			case charsetUnicode:
				mode = charsetUnicode
			case charsetASCII:
				mode = charsetASCII
			case charsetAuto, "":
				mode = charsetAuto
			default:
				diagnostics = append(diagnostics, "unknown terminal character-set mode; using automatic detection")
			}
		}
	}
	resolved := mode
	if resolved == charsetAuto {
		resolved = charsetASCII
		if strings.Contains(strings.ToLower(outputEncoding), "utf") || environmentUsesUTF8(lookup) {
			resolved = charsetUnicode
		}
	}
	if resolved == charsetASCII {
		return cloneUIGlyphs(asciiUIGlyphs), resolved, diagnostics
	}
	return cloneUIGlyphs(unicodeUIGlyphs), resolved, diagnostics
}

func environmentUsesUTF8(lookup func(string) (string, bool)) bool {
	if lookup == nil {
		return false
	}
	for _, key := range []string{"LANG", "LC_ALL"} {
		if value, exists := lookup(key); exists && strings.Contains(strings.ToLower(value), "utf") {
			return true
		}
	}
	return false
}

func cloneUIGlyphs(glyphs uiGlyphs) uiGlyphs {
	glyphs.SpinnerFrames = append([]string(nil), glyphs.SpinnerFrames...)
	return glyphs
}

func supportsKittyKeyboard(lookup func(string) (string, bool), stdinTTY, stdoutTTY bool) (bool, []string) {
	if runtime.GOOS == "windows" || !stdinTTY || !stdoutTTY {
		return false, nil
	}
	if lookup == nil {
		return false, nil
	}
	if raw, exists := lookup("DEEPAGENTS_CODE_KITTY_KEYBOARD"); exists {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "1", "true", "yes", "on":
			return true, nil
		case "0", "false", "no", "off":
			return false, nil
		case "", "auto":
		default:
			return terminalIdentitySupportsKitty(lookup), []string{"invalid kitty-keyboard override; using terminal detection"}
		}
	}
	return terminalIdentitySupportsKitty(lookup), nil
}

func terminalIdentitySupportsKitty(lookup func(string) (string, bool)) bool {
	if value, exists := lookup("KITTY_WINDOW_ID"); exists && strings.TrimSpace(value) != "" {
		return true
	}
	term, _ := lookup("TERM")
	return term == "xterm-kitty" || term == "xterm-ghostty"
}

func newlineShortcut(kitty bool) string {
	if kitty {
		return "Shift+Enter"
	}
	if runtime.GOOS == "darwin" {
		return "Option+Enter"
	}
	return "Ctrl+J"
}

type cursorStyle string

const (
	cursorBlock     cursorStyle = "block"
	cursorUnderline cursorStyle = "underline"
)

type cursorPreference struct {
	Style cursorStyle
	Blink bool
}

func loadCursorPreference(path string, lookup func(string) (string, bool)) (cursorPreference, []string) {
	preference := cursorPreference{Style: cursorBlock, Blink: true}
	diagnostics := []string{}
	document, err := readThemeDocument(path)
	if err == nil {
		ui, _ := document["ui"].(map[string]any)
		if value, exists := ui["cursor_blink"]; exists {
			if enabled, ok := value.(bool); ok {
				preference.Blink = enabled
			} else {
				diagnostics = append(diagnostics, "cursor blink preference is malformed; using enabled")
			}
		}
		if value, exists := ui["cursor_style"]; exists {
			if style, ok := parseCursorStyle(value); ok {
				preference.Style = style
			} else {
				diagnostics = append(diagnostics, "cursor style preference is unknown; using block")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		diagnostics = append(diagnostics, "cursor preferences are unavailable; using safe defaults")
	}
	if lookup != nil {
		if value, exists := lookup("DEEPAGENTS_CODE_CURSOR_STYLE"); exists {
			if style, ok := parseCursorStyle(value); ok {
				preference.Style = style
			} else {
				diagnostics = append(diagnostics, "cursor style override is unknown; using saved preference")
			}
		}
	}
	return preference, diagnostics
}

func parseCursorStyle(value any) (cursorStyle, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	switch cursorStyle(strings.ToLower(strings.TrimSpace(text))) {
	case cursorBlock:
		return cursorBlock, true
	case cursorUnderline:
		return cursorUnderline, true
	default:
		return "", false
	}
}
