package unicodesecurity

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
)

func TestDetectStripAndRenderDangerousUnicode(t *testing.T) {
	text := "a\u202eb\u200bc"
	issues := Detect(text)
	if len(issues) != 2 {
		t.Fatalf("Detect() returned %d issues, want 2", len(issues))
	}
	if issues[0].Position != 1 || issues[0].Codepoint != "U+202E" || issues[0].Name != "RIGHT-TO-LEFT OVERRIDE" {
		t.Fatalf("first issue = %#v", issues[0])
	}
	if got := Strip("ap\u200bple"); got != "apple" {
		t.Fatalf("Strip() = %q, want apple", got)
	}
	rendered := RenderMarkers(text)
	for _, want := range []string{"<U+202E RIGHT-TO-LEFT OVERRIDE>", "<U+200B ZERO WIDTH SPACE>"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("RenderMarkers() = %q, missing %q", rendered, want)
		}
	}
}

func TestDetectPositionUsesRuneIndex(t *testing.T) {
	issues := Detect("世\u200b界")
	if len(issues) != 1 || issues[0].Position != 1 {
		t.Fatalf("issues = %#v, want dangerous rune at position 1", issues)
	}
}

func TestRenderTerminalSafeMakesControlSequencesInert(t *testing.T) {
	input := "first\n\tsecond\x1b]52;c;ZXZpbA==\a\rthird\u009b31m"
	got := RenderTerminalSafe(input)
	want := "first\n    second<U+001B CONTROL>]52;c;ZXZpbA==<U+0007 CONTROL><U+000D CONTROL>third<U+009B CONTROL>31m"
	if got != want {
		t.Fatalf("RenderTerminalSafe() = %q, want %q", got, want)
	}
	for _, character := range got {
		if character != '\n' && unicode.IsControl(character) {
			t.Fatalf("RenderTerminalSafe() retained control U+%04X", character)
		}
	}
}

func TestSummarizeDeduplicatesAndBoundsOutput(t *testing.T) {
	issues := Detect("\u200b\u200b\u200c\u200d")
	if got := Summarize(issues, 2); got != "U+200B ZERO WIDTH SPACE, U+200C ZERO WIDTH NON-JOINER, +1 more entry" {
		t.Fatalf("Summarize() = %q", got)
	}
}

func TestCheckURLSafety(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		safe        bool
		decoded     string
		warningPart string
	}{
		{name: "ASCII", url: "https://apple.com", safe: true},
		{name: "mixed Cyrillic", url: "https://аpple.com", warningPart: "mixes scripts"},
		{name: "punycode homograph", url: "https://xn--pple-43d.com", decoded: "аpple.com", warningPart: "mixes scripts"},
		{name: "single non-Latin script", url: "https://例え.jp", safe: true},
		{name: "localhost", url: "https://localhost:8080", safe: true},
		{name: "IPv4", url: "https://192.168.1.1", safe: true},
		{name: "IPv6", url: "https://[::1]", safe: true},
		{name: "hidden path character", url: "https://example.com/\u200badmin", warningPart: "hidden Unicode"},
		{name: "data URI hidden character", url: "data:text/html,\u200bhello", warningPart: "hidden Unicode"},
		{name: "invalid punycode", url: "https://xn--invalid!!!.com", warningPart: "could not be decoded"},
		{name: "fullwidth single script", url: "https://ａｅｏ.com", safe: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := CheckURL(test.url)
			if result.Safe != test.safe {
				t.Fatalf("CheckURL(%q).Safe = %v, warnings: %v", test.url, result.Safe, result.Warnings)
			}
			if result.DecodedDomain != test.decoded {
				t.Fatalf("CheckURL(%q).DecodedDomain = %q, want %q", test.url, result.DecodedDomain, test.decoded)
			}
			if test.warningPart != "" && !strings.Contains(strings.Join(result.Warnings, "\n"), test.warningPart) {
				t.Fatalf("CheckURL(%q).Warnings = %v, missing %q", test.url, result.Warnings, test.warningPart)
			}
		})
	}
}

func TestLooksLikeURLKey(t *testing.T) {
	for _, path := range []string{"url", "URL", "Base_URL", "nested.url", "items[0].endpoint"} {
		if !LooksLikeURLKey(path) {
			t.Errorf("LooksLikeURLKey(%q) = false", path)
		}
	}
	for _, path := range []string{"command", "urls[0]", "link_text"} {
		if LooksLikeURLKey(path) {
			t.Errorf("LooksLikeURLKey(%q) = true", path)
		}
	}
}

func TestScanArgumentsRecursesAndNamesSource(t *testing.T) {
	warnings := ScanArguments("task", map[string]any{
		"description": "review\u202ethis",
		"targets": []any{
			map[string]any{"url": "https://xn--pple-43d.com"},
		},
	})
	if len(warnings) != 2 {
		t.Fatalf("ScanArguments() = %#v, want 2 warnings", warnings)
	}
	if !strings.Contains(warnings[0], "task.description: hidden Unicode") {
		t.Fatalf("first warning = %q", warnings[0])
	}
	if !strings.Contains(warnings[1], "task.targets[0].url:") || !strings.Contains(warnings[1], "decoded host: аpple.com") {
		t.Fatalf("URL warning = %q", warnings[1])
	}
}

func TestScanArgumentsLeavesSafeArgumentsAlone(t *testing.T) {
	if got := ScanArguments("fetch_url", map[string]any{"url": "https://example.com", "limit": 3}); !reflect.DeepEqual(got, []string(nil)) {
		t.Fatalf("ScanArguments() = %#v, want nil", got)
	}
}

func TestScanArgumentsCoversApprovalGatedToolShapes(t *testing.T) {
	tests := []struct {
		tool      string
		arguments map[string]any
		want      string
	}{
		{tool: "execute", arguments: map[string]any{"command": "git status\u202e"}, want: "execute.command: hidden Unicode"},
		{tool: "fetch_url", arguments: map[string]any{"url": "https://аpple.com"}, want: "fetch_url.url: Domain label"},
		{tool: "write_file", arguments: map[string]any{"content": "safe\u200btext"}, want: "write_file.content: hidden Unicode"},
		{tool: "edit_file", arguments: map[string]any{"new_string": "safe\u2066text"}, want: "edit_file.new_string: hidden Unicode"},
		{tool: "task", arguments: map[string]any{"description": "inspect\u200fthis"}, want: "task.description: hidden Unicode"},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			warnings := ScanArguments(test.tool, test.arguments)
			if !strings.Contains(strings.Join(warnings, "\n"), test.want) {
				t.Fatalf("ScanArguments() = %#v, missing %q", warnings, test.want)
			}
		})
	}
}
