package llm

import (
	"strings"
	"unicode/utf8"
)

// Truncate clips s to at most n runes, appending an ellipsis when truncated.
// Whitespace runs are collapsed to single spaces so it renders well in a
// single-line log field. n counts runes (not bytes) and the cut always lands
// on a rune boundary, so multi-byte UTF-8 sequences are never split.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, count := 0, 0
	for i < len(s) && count < n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i] + "…"
}
