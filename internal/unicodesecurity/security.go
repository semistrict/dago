// Package unicodesecurity detects text and URLs that may render deceptively.
package unicodesecurity

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

// Issue describes an invisible or directional Unicode character in source text.
// Position is a zero-based rune index, rather than a byte offset.
type Issue struct {
	Position  int
	Character rune
	Codepoint string
	Name      string
}

// URLResult is the result of checking a URL for deceptive text.
type URLResult struct {
	Safe          bool
	DecodedDomain string
	Warnings      []string
	Issues        []Issue
}

var dangerousNames = map[rune]string{
	0x00ad: "SOFT HYPHEN",
	0x034f: "COMBINING GRAPHEME JOINER",
	0x115f: "HANGUL CHOSEONG FILLER",
	0x1160: "HANGUL JUNGSEONG FILLER",
	0x200b: "ZERO WIDTH SPACE",
	0x200c: "ZERO WIDTH NON-JOINER",
	0x200d: "ZERO WIDTH JOINER",
	0x200e: "LEFT-TO-RIGHT MARK",
	0x200f: "RIGHT-TO-LEFT MARK",
	0x202a: "LEFT-TO-RIGHT EMBEDDING",
	0x202b: "RIGHT-TO-LEFT EMBEDDING",
	0x202c: "POP DIRECTIONAL FORMATTING",
	0x202d: "LEFT-TO-RIGHT OVERRIDE",
	0x202e: "RIGHT-TO-LEFT OVERRIDE",
	0x2060: "WORD JOINER",
	0x2066: "LEFT-TO-RIGHT ISOLATE",
	0x2067: "RIGHT-TO-LEFT ISOLATE",
	0x2068: "FIRST STRONG ISOLATE",
	0x2069: "POP DIRECTIONAL ISOLATE",
	0xfeff: "ZERO WIDTH NO-BREAK SPACE",
}

var confusables = map[rune]rune{
	// Cyrillic.
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x',
	'н': 'h', 'і': 'i', 'ј': 'j', 'к': 'k', 'ѕ': 's',
	// Greek.
	'α': 'a', 'ε': 'e', 'ο': 'o', 'ρ': 'p', 'χ': 'x', 'κ': 'k', 'ν': 'v', 'τ': 't',
	// Armenian.
	'հ': 'h', 'ո': 'n', 'ս': 'u',
	// Fullwidth Latin.
	'ａ': 'a', 'ｅ': 'e', 'ｏ': 'o',
}

var urlKeys = map[string]struct{}{
	"url": {}, "uri": {}, "href": {}, "link": {}, "base_url": {}, "endpoint": {},
}

// Detect returns deceptive or invisible Unicode characters in source order.
func Detect(text string) []Issue {
	var issues []Issue
	position := 0
	for _, character := range text {
		if name, dangerous := dangerousNames[character]; dangerous {
			issues = append(issues, Issue{
				Position:  position,
				Character: character,
				Codepoint: fmt.Sprintf("U+%04X", character),
				Name:      name,
			})
		}
		position++
	}
	return issues
}

// Strip removes known deceptive or invisible Unicode characters.
func Strip(text string) string {
	return strings.Map(func(character rune) rune {
		if _, dangerous := dangerousNames[character]; dangerous {
			return -1
		}
		return character
	}, text)
}

// RenderMarkers replaces deceptive or invisible characters with visible markers.
func RenderMarkers(text string) string {
	var rendered strings.Builder
	for _, character := range text {
		name, dangerous := dangerousNames[character]
		if !dangerous {
			rendered.WriteRune(character)
			continue
		}
		fmt.Fprintf(&rendered, "<U+%04X %s>", character, name)
	}
	return rendered.String()
}

// RenderTerminalSafe preserves printable text and newlines while making terminal
// control characters inert. Tabs become spaces so their visual separation is
// retained without allowing cursor movement.
func RenderTerminalSafe(text string) string {
	var rendered strings.Builder
	for _, character := range text {
		switch {
		case character == '\n':
			rendered.WriteRune(character)
		case character == '\t':
			rendered.WriteString("    ")
		case unicode.IsControl(character):
			fmt.Fprintf(&rendered, "<U+%04X CONTROL>", character)
		default:
			rendered.WriteRune(character)
		}
	}
	return RenderMarkers(rendered.String())
}

// Summarize describes the unique code points in issues, capped at maxItems.
func Summarize(issues []Issue, maxItems int) string {
	if maxItems < 0 {
		maxItems = 0
	}
	entries := make([]string, 0, len(issues))
	seen := make(map[string]struct{}, len(issues))
	for _, issue := range issues {
		entry := issue.Codepoint + " " + issue.Name
		if _, exists := seen[entry]; exists {
			continue
		}
		seen[entry] = struct{}{}
		entries = append(entries, entry)
	}
	if len(entries) <= maxItems {
		return strings.Join(entries, ", ")
	}
	remainder := len(entries) - maxItems
	noun := "entries"
	if remainder == 1 {
		noun = "entry"
	}
	parts := append([]string(nil), entries[:maxItems]...)
	parts = append(parts, fmt.Sprintf("+%d more %s", remainder, noun))
	return strings.Join(parts, ", ")
}

// CheckURL detects hidden text and mixed-script or confusable domain labels.
func CheckURL(value string) URLResult {
	result := URLResult{Safe: true, Issues: Detect(value)}
	if len(result.Issues) > 0 {
		result.Safe = false
		result.Warnings = append(result.Warnings,
			"URL contains hidden Unicode characters ("+Summarize(result.Issues, 3)+")")
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return result
	}
	hostname := parsed.Hostname()
	decoded, failed := decodeHostname(hostname)
	if decoded != hostname {
		result.DecodedDomain = decoded
		result.Warnings = append(result.Warnings, "Punycode domain decodes to '"+decoded+"'")
	}
	if len(failed) > 0 {
		result.Safe = false
		result.Warnings = append(result.Warnings,
			"Punycode label(s) could not be decoded: "+strings.Join(failed, ", "))
	}
	if localOrIP(decoded) {
		return result
	}

	for _, label := range splitLabels(decoded) {
		scripts := scriptsInLabel(label)
		if len(scripts) > 1 {
			result.Safe = false
			names := make([]string, 0, len(scripts))
			for script := range scripts {
				names = append(names, script)
			}
			sort.Strings(names)
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Domain label '%s' mixes scripts (%s)", label, strings.Join(names, ", ")))
		}
		if len(scripts) > 1 && containsConfusable(label) {
			result.Safe = false
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Domain label '%s' contains confusable Unicode characters", label))
		}
	}
	return result
}

// LooksLikeURLKey reports whether the leaf of an argument path commonly holds a URL.
func LooksLikeURLKey(path string) bool {
	if index := strings.LastIndex(path, "."); index >= 0 {
		path = path[index+1:]
	}
	if index := strings.Index(path, "["); index >= 0 {
		path = path[:index]
	}
	_, ok := urlKeys[strings.ToLower(path)]
	return ok
}

// ScanArguments recursively scans string leaves in decoded tool arguments.
// Returned warnings include the tool and argument path so approval surfaces can
// show the deceptive value in context.
func ScanArguments(tool string, arguments any) []string {
	var warnings []string
	walkStrings(arguments, "", func(path, value string) {
		issues := Detect(value)
		if len(issues) > 0 {
			warnings = append(warnings, fmt.Sprintf("%s.%s: hidden Unicode (%s)",
				tool, path, Summarize(issues, 3)))
		}
		if !LooksLikeURLKey(path) {
			return
		}
		checked := CheckURL(value)
		if checked.Safe {
			return
		}
		detail := warningDetail(checked.Warnings, 2)
		if checked.DecodedDomain != "" {
			detail += "; decoded host: " + checked.DecodedDomain
		}
		warnings = append(warnings, fmt.Sprintf("%s.%s: %s", tool, path, detail))
	})
	return warnings
}

func walkStrings(value any, path string, visit func(path, value string)) {
	switch typed := value.(type) {
	case string:
		visit(path, typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := typed[key]
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			walkStrings(child, childPath, visit)
		}
	case []any:
		for index, child := range typed {
			walkStrings(child, fmt.Sprintf("%s[%d]", path, index), visit)
		}
	}
}

func warningDetail(warnings []string, maxShown int) string {
	shown := warnings
	if len(shown) > maxShown {
		shown = shown[:maxShown]
	}
	detail := strings.Join(shown, "; ")
	if remaining := len(warnings) - len(shown); remaining > 0 {
		detail += fmt.Sprintf("; +%d more", remaining)
	}
	return detail
}

func decodeHostname(hostname string) (string, []string) {
	labels := splitLabels(hostname)
	failed := make([]string, 0)
	for index, label := range labels {
		if !strings.HasPrefix(strings.ToLower(label), "xn--") {
			continue
		}
		decoded, err := idna.Lookup.ToUnicode(label)
		if err != nil {
			failed = append(failed, label)
			continue
		}
		labels[index] = decoded
	}
	return strings.Join(labels, "."), failed
}

func splitLabels(hostname string) []string {
	parts := strings.Split(hostname, ".")
	labels := parts[:0]
	for _, part := range parts {
		if part != "" {
			labels = append(labels, part)
		}
	}
	return labels
}

func localOrIP(hostname string) bool {
	hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	return strings.EqualFold(hostname, "localhost") || net.ParseIP(hostname) != nil
}

func scriptsInLabel(label string) map[string]struct{} {
	scripts := make(map[string]struct{})
	for _, character := range label {
		script := runeScript(character)
		if script != "Common" && script != "Inherited" {
			scripts[script] = struct{}{}
		}
	}
	return scripts
}

func containsConfusable(label string) bool {
	for _, character := range label {
		if _, ok := confusables[character]; ok {
			return true
		}
	}
	return false
}

func runeScript(character rune) string {
	switch {
	case character >= 0xff41 && character <= 0xff5a:
		return "Fullwidth"
	case unicode.In(character, unicode.Latin):
		return "Latin"
	case unicode.In(character, unicode.Cyrillic):
		return "Cyrillic"
	case unicode.In(character, unicode.Greek):
		return "Greek"
	case unicode.In(character, unicode.Armenian):
		return "Armenian"
	case unicode.In(character, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Bopomofo):
		return "EastAsian"
	case unicode.IsMark(character):
		return "Inherited"
	case unicode.IsNumber(character), unicode.IsPunct(character), unicode.IsSymbol(character),
		unicode.IsSpace(character), unicode.Is(unicode.Other, character), character == utf8.RuneError:
		return "Common"
	default:
		return "Other"
	}
}
