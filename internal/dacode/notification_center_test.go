package dacode

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestNotificationCenterNavigatesDetailsAndActions(t *testing.T) {
	center := newNotificationCenter([]pendingNotification{
		testUpdateNotification("update", "v1.2.3"),
		newPendingNotification("tool", "Missing tool", "Install it.", missingDependencyNotification{Tool: "search", InstallCommand: "install search"},
			notificationAction{ID: notificationCopyInstall, Label: "Copy"}, notificationAction{ID: notificationSuppress, Label: "Hide"}),
	})
	if request, close := center.handleKey(tea.KeyPressMsg{Code: tea.KeyDown}); request != nil || close || center.selected != 1 {
		t.Fatalf("down = %#v, %v, selected %d", request, close, center.selected)
	}
	center.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !center.detail || center.action != 0 {
		t.Fatalf("detail = %v action = %d", center.detail, center.action)
	}
	center.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	request, close := center.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if request == nil || request.Key != "tool" || request.ID != notificationSuppress || close {
		t.Fatalf("request = %#v close = %v", request, close)
	}
	center.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if center.detail {
		t.Fatal("escape did not return to list")
	}
	if request, close := center.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape}); request != nil || !close {
		t.Fatalf("close = %#v, %v", request, close)
	}
}

func TestNotificationCenterStartsOnPrimaryAndReloadsByKey(t *testing.T) {
	center := newNotificationCenter([]pendingNotification{
		testUpdateNotification("update", "v1.2.3"),
		newPendingNotification("tool", "Missing tool", "Install it.", missingDependencyNotification{Tool: "search", InstallCommand: "install search"}, notificationAction{ID: notificationCopyInstall, Label: "Copy"}),
	})
	center.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if center.action != 0 {
		t.Fatalf("primary action = %d", center.action)
	}
	center.detail = false
	center.selected = 1
	if !center.reload([]pendingNotification{center.entries[1], center.entries[0]}) || center.selected != 0 {
		t.Fatalf("reload selection = %d", center.selected)
	}
	if center.reload(nil) {
		t.Fatal("empty reload remained open")
	}
}

func TestNotificationCenterReloadClampsMissingSelection(t *testing.T) {
	center := newNotificationCenter([]pendingNotification{
		testUpdateNotification("one", "v1.0.0"),
		testUpdateNotification("two", "v1.0.0"),
		testUpdateNotification("three", "v1.0.0"),
	})
	center.selected = 2
	if !center.reload([]pendingNotification{testUpdateNotification("one", "v1.0.0"), testUpdateNotification("two", "v1.0.0")}) {
		t.Fatal("non-empty reload closed")
	}
	if center.selected != 1 {
		t.Fatalf("selection = %d, want 1", center.selected)
	}
}

func TestNotificationCenterRendersExternalTextSafely(t *testing.T) {
	center := newNotificationCenter([]pendingNotification{newPendingNotification(
		"tool", "Unsafe\x1b[31m", "Body\rtext", missingDependencyNotification{Tool: "search", URL: "https://example.test"},
		notificationAction{ID: notificationOpenWebsite, Label: "Open\x1b[2J"},
	)})
	rendered := center.render(100, 30, unicodeUIGlyphs)
	if strings.Contains(rendered, "\x1b[31m") || !strings.Contains(rendered, "U+001B") {
		t.Fatalf("unsafe list render = %q", rendered)
	}
	center.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	rendered = center.render(100, 30, unicodeUIGlyphs)
	if strings.Contains(rendered, "\x1b[2J") || !strings.Contains(rendered, "U+001B") {
		t.Fatalf("unsafe detail render = %q", rendered)
	}
}

func TestNotificationSettingsToggleAndRollback(t *testing.T) {
	settings := newNotificationSettings(map[string]bool{warningTavily: true, "unknown": true})
	if !strings.Contains(settings.render(100, 30, unicodeUIGlyphs), "[ ] Warn when the web-search API key") {
		t.Fatalf("render = %q", settings.render(100, 30, unicodeUIGlyphs))
	}
	settings.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	key, enabled, changed, close := settings.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if key != warningTavily || !enabled || !changed || close || settings.suppressed[warningTavily] {
		t.Fatalf("toggle = %q %v %v %v %#v", key, enabled, changed, close, settings.suppressed)
	}
	settings.rollback(key, !enabled)
	if !settings.suppressed[warningTavily] {
		t.Fatal("rollback did not restore suppression")
	}
	if _, _, _, close := settings.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape}); !close {
		t.Fatal("escape did not close settings")
	}
	settings = newNotificationSettings(nil)
	key, enabled, changed, close = settings.handleKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if key != warningRipgrep || enabled || !changed || close || !settings.suppressed[warningRipgrep] {
		t.Fatalf("space toggle = %q %v %v %v %#v", key, enabled, changed, close, settings.suppressed)
	}
}

func TestNotificationScreensRenderASCIIUI(t *testing.T) {
	center := newNotificationCenter([]pendingNotification{testUpdateNotification("update", "v1.2.3")})
	views := []string{center.render(80, 24, asciiUIGlyphs)}
	center.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	views = append(views, center.render(80, 24, asciiUIGlyphs), newNotificationSettings(nil).render(80, 24, asciiUIGlyphs))
	for _, rendered := range views {
		if strings.ContainsAny(rendered, "╭╮╰╯│─…✓✗⏳•⏎↑↓›") {
			t.Fatalf("ASCII notification screen contains Unicode UI glyphs:\n%s", rendered)
		}
	}
}
