package dacode

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestITermCursorGuideDisablesAndRestoresOnlyEnabledActiveProfile(t *testing.T) {
	t.Parallel()
	preferences := `<?xml version="1.0"?><plist><dict>
<key>Default Bookmark Guid</key><string>default</string>
<key>New Bookmarks</key><array>
<dict><key>Name</key><string>Default</string><key>Guid</key><string>default</string><key>Use Cursor Guide</key><false/></dict>
<dict><key>Name</key><string>Work</string><key>Guid</key><string>work</string><key>Use Cursor Guide (Dark)</key><true/></dict>
</array></dict></plist>`
	lookup := mapLookup(map[string]string{"TERM_PROGRAM": "iTerm.app", "ITERM_PROFILE": "Work"})
	guide := resolveITermCursorGuide(lookup, true, strings.NewReader(preferences))
	if guide.Disable != itermCursorGuideOff || guide.Restore != itermCursorGuideOn {
		t.Fatalf("guide = %#v", guide)
	}
}

func TestSuspendITermCursorGuidePairsDisableAndRestore(t *testing.T) {
	var output bytes.Buffer
	restore := suspendITermCursorGuide(&output, itermCursorGuide{Disable: itermCursorGuideOff, Restore: itermCursorGuideOn})
	if output.String() != itermCursorGuideOff {
		t.Fatalf("disable output = %q", output.String())
	}
	restore()
	if output.String() != itermCursorGuideOff+itermCursorGuideOn {
		t.Fatalf("paired output = %q", output.String())
	}

	failed := &failingCursorGuideWriter{}
	suspendITermCursorGuide(failed, itermCursorGuide{Disable: itermCursorGuideOff, Restore: itermCursorGuideOn})()
	if failed.calls != 1 {
		t.Fatalf("zero-byte failure attempted restore: calls=%d", failed.calls)
	}
}

type failingCursorGuideWriter struct{ calls int }

func (writer *failingCursorGuideWriter) Write([]byte) (int, error) {
	writer.calls++
	return 0, errors.New("closed")
}

func TestITermCursorGuideUsesDefaultProfileAndFailsClosed(t *testing.T) {
	t.Parallel()
	enabled := `<plist><dict><key>Default Bookmark Guid</key><string>default</string><key>New Bookmarks</key><array><dict><key>Guid</key><string>default</string><key>Use Cursor Guide</key><integer>1</integer></dict></array></dict></plist>`
	lookup := mapLookup(map[string]string{"LC_TERMINAL": "iTerm2"})
	if guide := resolveITermCursorGuide(lookup, true, strings.NewReader(enabled)); guide.Disable == "" || guide.Restore == "" {
		t.Fatalf("default profile guide = %#v", guide)
	}
	for name, reader := range map[string]*strings.Reader{
		"not a terminal": strings.NewReader(enabled),
		"malformed":      strings.NewReader(`<plist><dict>`),
		"disabled":       strings.NewReader(strings.Replace(enabled, `<integer>1</integer>`, `<false/>`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			stderrTTY := name != "not a terminal"
			if guide := resolveITermCursorGuide(lookup, stderrTTY, reader); guide != (itermCursorGuide{}) {
				t.Fatalf("guide = %#v", guide)
			}
		})
	}
}

func TestITermCursorGuideRequiresExactTerminalIdentity(t *testing.T) {
	t.Parallel()
	preferences := strings.NewReader(`<plist><dict><key>Use Cursor Guide</key><true/></dict></plist>`)
	if guide := resolveITermCursorGuide(mapLookup(map[string]string{"TERM_PROGRAM": "iTerm"}), true, preferences); guide != (itermCursorGuide{}) {
		t.Fatalf("guide = %#v", guide)
	}
}
