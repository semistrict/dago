package llm

import (
	"strings"
	"testing"
)

func TestStripInlineCitationMarkers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"none", "plain text", "plain text"},
		{
			"single group",
			"JavaScript.\ue200cite\ue202turn1search0\ue202turn1search2\ue201\nNext line.",
			"JavaScript.\nNext line.",
		},
		{
			"mid sentence",
			"gemini-2.5-flash\ue200cite\ue202turn2search0\ue201 is good.",
			"gemini-2.5-flash is good.",
		},
		{
			"stray markers no group",
			"a\ue202b\ue203c",
			"abc",
		},
		{
			"unterminated group fails open",
			"keep\ue200cite\ue202turn1search0",
			"keepciteturn1search0",
		},
		{
			// U+E203/U+E204 bracket the prose a citation covers, so the text
			// between them must survive with only the markers removed.
			"cited-span markers keep their prose",
			"\ue203A model optimized for speed.\ue204 \ue200cite\ue202turn0search3\ue201",
			"A model optimized for speed. ",
		},
		{
			"multiple groups",
			"\ue200cite\ue202turn0search0\ue201x\ue200cite\ue202turn0search1\ue201y",
			"xy",
		},
		{
			"restarted group fails the abandoned one open",
			"keep\ue200broken prose\ue200cite\ue202turn1search0\ue201after",
			"keepbroken proseafter",
		},
		{
			"over-cap group fails open",
			"a\ue200cite\ue202" + strings.Repeat("x", inlineCitationMaxBytes) + "\ue201b",
			"acite" + strings.Repeat("x", inlineCitationMaxBytes) + "b",
		},
		{
			"navlist-sized group is stripped whole",
			"before\ue200navlist\ue202" + strings.Repeat("Some Long News Headline\ue202turn0news14\ue202", 20) + "\ue201after",
			"beforeafter",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripInlineCitationMarkers(tt.in); got != tt.want {
				t.Errorf("StripInlineCitationMarkers(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
