package llm

import (
	"strings"
)

// The ChatGPT subscription backend (used by the gpt-5.6-* variants and other
// ChatGPT-hosted models) embeds inline web-search citations in the assistant
// text using Unicode Private-Use-Area delimiter characters, e.g.
//
//	U+E200 "cite" U+E202 "turn1search0" U+E202 "turn1search2" U+E201
//
// These are an internal ChatGPT rendering protocol: the real citation data
// also arrives out-of-band as url_citation annotations (which we already turn
// into llm.Content.Citations). The inline PUA markup is pure noise for us, and
// worse, clients that don't strip it render the PUA code points as fallback
// glyphs — on iOS they show up as stray "emoji" (❓🚢🚶). Strip them.
//
// Marker code points:
//   - U+E200 START of a citation group
//   - U+E201 END of a citation group
//   - U+E202 internal delimiter between fields
//   - U+E203/U+E204 bracket the span of prose a following citation covers
//
// Only U+E200 opens a group. U+E203 and U+E204 wrap ordinary assistant text —
// real exports contain spans like
//
//	U+E203 "A model optimized for speed." U+E204 U+E200 "cite" U+E202 "turn0search3" U+E201
//
// so they are dropped as bare runes and their contents kept; treating either
// as a group delimiter would delete visible prose.
//
// isInlineCitationMarker deliberately treats the whole U+E200–U+E20F block as
// markup rather than just the code points above: the protocol is undocumented
// and has grown (U+E206 turns up in third-party ChatGPT export converters), and
// a nearby code point we haven't seen yet is far likelier to be another
// delimiter than legitimate assistant prose.
//
// The group payload lives between START and END, so removing the whole span
// discards it entirely. The leading keyword names the widget the group renders
// as — "cite", "navlist", "entity", "i" (image carousel), "filecite",
// "filenavlist", "forecast", "finance", "schedule", "standing" — and none of
// them are text the user should see.
const (
	citeMarkerStart = '\uE200'
	citeMarkerEnd   = '\uE201'

	// Across 90 citation groups in real ChatGPT conversation exports the
	// largest was 129 bytes (a navlist: one list title plus up to ten
	// comma-separated turn refs); a plain cite runs 18–52. The shape with the
	// most room to grow is filenavlist, which carries a free-text description
	// per item for up to ten items.
	//
	// The bound only has to keep a missing END marker from making the stream
	// filter retain unbounded data and from suppressing an arbitrary amount of
	// assistant text, so it sits well clear of any well-formed group:
	// exceeding it means the framing is broken, and the payload is emitted
	// rather than swallowed.
	inlineCitationMaxBytes = 4096

	// Every marker code point (U+E200–U+E20F) encodes to a 3-byte UTF-8
	// sequence sharing this prefix, so a plain byte search rules out the
	// overwhelmingly common marker-free text ~50x faster than decoding
	// runes. Matches fall through to the exact per-rune scan.
	inlineCitationMarkerPrefix = "\xee\x88"
)

func isInlineCitationMarker(r rune) bool {
	return r >= '\uE200' && r <= '\uE20F'
}

func writeWithoutInlineCitationMarkers(b *strings.Builder, s string) {
	for _, r := range s {
		if !isInlineCitationMarker(r) {
			b.WriteRune(r)
		}
	}
}

// inlineCitationFilter removes complete citation groups while retaining at
// most inlineCitationMaxBytes for a group split across chunks. Malformed or
// unexpectedly large groups fail open: their payload is emitted with the PUA
// marker runes removed, preserving assistant prose at the cost of showing the
// internal citation reference.
type inlineCitationFilter struct {
	pending strings.Builder
	inGroup bool
}

func (f *inlineCitationFilter) filter(s string) string {
	if !f.inGroup && !hasInlineCitationMarker(s) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if f.inGroup {
			switch r {
			case citeMarkerEnd:
				f.pending.Reset()
				f.inGroup = false
				continue
			case citeMarkerStart:
				// A second START means the group we were accumulating was
				// never terminated. Fail it open rather than absorbing it as
				// payload, which would delete real prose along with it.
				writeWithoutInlineCitationMarkers(&b, f.pending.String())
				f.pending.Reset()
				continue
			}
			f.pending.WriteRune(r)
			if f.pending.Len() > inlineCitationMaxBytes {
				writeWithoutInlineCitationMarkers(&b, f.pending.String())
				f.pending.Reset()
				f.inGroup = false
			}
			continue
		}

		switch {
		case r == citeMarkerStart:
			f.inGroup = true
		case isInlineCitationMarker(r):
			// Drop stray marker runes outside a citation group.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// finish fails open an unterminated citation group and resets the filter.
func (f *inlineCitationFilter) finish() string {
	if !f.inGroup {
		return ""
	}
	var b strings.Builder
	b.Grow(f.pending.Len())
	writeWithoutInlineCitationMarkers(&b, f.pending.String())
	f.pending.Reset()
	f.inGroup = false
	return b.String()
}

func hasInlineCitationMarker(s string) bool {
	if !strings.Contains(s, inlineCitationMarkerPrefix) {
		return false
	}
	for _, r := range s {
		if isInlineCitationMarker(r) {
			return true
		}
	}
	return false
}

// StripInlineCitationMarkers removes ChatGPT inline citation markup from a
// complete text block. Complete groups are discarded. Unterminated or
// unexpectedly large groups fail open with only their PUA marker runes
// removed, so malformed markup cannot hide assistant prose.
//
// Text containing no markers is returned unchanged. Text that does contain a
// marker is re-encoded, which replaces any invalid UTF-8 with U+FFFD; callers
// that slice the result at byte offsets should strip before slicing, not
// after, so a cut cannot land inside a marker's 3-byte sequence.
func StripInlineCitationMarkers(s string) string {
	if !hasInlineCitationMarker(s) {
		return s
	}
	var f inlineCitationFilter
	return f.filter(s) + f.finish()
}
