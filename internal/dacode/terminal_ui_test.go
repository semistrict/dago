package dacode

import (
	"os"
	"path/filepath"
	"testing"
)

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, exists := values[key]
		return value, exists
	}
}

func TestUIGlyphsResolveExplicitAndAutomaticModes(t *testing.T) {
	tests := []struct {
		name        string
		environment map[string]string
		encoding    string
		want        charsetMode
		ellipsis    string
		diagnostic  bool
	}{
		{"explicit ASCII", map[string]string{"DEEPAGENTS_CODE_UI_CHARSET_MODE": "ascii", "LANG": "en_US.UTF-8"}, "utf-8", charsetASCII, "...", false},
		{"explicit Unicode", map[string]string{"DEEPAGENTS_CODE_UI_CHARSET_MODE": "unicode"}, "ascii", charsetUnicode, "…", false},
		{"encoding auto", nil, "UTF-8", charsetUnicode, "…", false},
		{"locale auto", map[string]string{"LANG": "C.UTF-8"}, "ascii", charsetUnicode, "…", false},
		{"ASCII auto", map[string]string{"LANG": "C"}, "ascii", charsetASCII, "...", false},
		{"invalid", map[string]string{"DEEPAGENTS_CODE_UI_CHARSET_MODE": "broken"}, "ascii", charsetASCII, "...", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			glyphs, mode, diagnostics := resolveUIGlyphs(mapLookup(test.environment), test.encoding)
			if mode != test.want || glyphs.Ellipsis != test.ellipsis || (len(diagnostics) != 0) != test.diagnostic {
				t.Fatalf("resolved = %#v, %q, %#v", glyphs, mode, diagnostics)
			}
			glyphs.SpinnerFrames[0] = "mutated"
			again, _, _ := resolveUIGlyphs(mapLookup(test.environment), test.encoding)
			if again.SpinnerFrames[0] == "mutated" {
				t.Fatal("glyph catalog was mutated")
			}
		})
	}
}

func TestKittyKeyboardDetectionIsConservativeAndOverrideable(t *testing.T) {
	if supported, _ := supportsKittyKeyboard(mapLookup(map[string]string{"TERM": "xterm-kitty"}), false, true); supported {
		t.Fatal("non-TTY input enabled kitty keyboard")
	}
	if supported, _ := supportsKittyKeyboard(mapLookup(map[string]string{"TERM": "xterm-kitty"}), true, true); !supported {
		t.Fatal("known terminal was not detected")
	}
	if supported, _ := supportsKittyKeyboard(mapLookup(map[string]string{"TERM": "xterm-kitty", "DEEPAGENTS_CODE_KITTY_KEYBOARD": "off"}), true, true); supported {
		t.Fatal("explicit disable was ignored")
	}
	if supported, diagnostics := supportsKittyKeyboard(mapLookup(map[string]string{"TERM": "xterm-ghostty", "DEEPAGENTS_CODE_KITTY_KEYBOARD": "invalid"}), true, true); !supported || len(diagnostics) != 1 {
		t.Fatalf("invalid override = %v, %#v", supported, diagnostics)
	}
}

func TestCursorPreferencesUseDefaultsConfigAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	preference, diagnostics := loadCursorPreference(path, nil)
	if preference != (cursorPreference{Style: cursorBlock, Blink: true}) || len(diagnostics) != 0 {
		t.Fatalf("defaults = %#v, %#v", preference, diagnostics)
	}
	if err := os.WriteFile(path, []byte("[ui]\ncursor_style = \"underline\"\ncursor_blink = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preference, diagnostics = loadCursorPreference(path, mapLookup(map[string]string{"DEEPAGENTS_CODE_CURSOR_STYLE": "block"}))
	if preference != (cursorPreference{Style: cursorBlock, Blink: false}) || len(diagnostics) != 0 {
		t.Fatalf("resolved = %#v, %#v", preference, diagnostics)
	}
}
