package dacode

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestNotificationShortcutUsesEmptyToastAndDefersToModelSelector(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/tmp", "test", "thread", false, false, "")
	command, handled := model.handleNotificationShortcut(tea.KeyMsg{Type: tea.KeyCtrlN})
	if !handled || command == nil || model.notificationCenter != nil {
		t.Fatalf("empty shortcut = handled %v command %v center %#v", handled, command != nil, model.notificationCenter)
	}
	_ = command()
	if view := renderToasts(model.toasts, 80, unicodeUIGlyphs, model.toasts.items[0].ExpiresAt.Add(-1)); !strings.Contains(view, "No pending notifications") {
		t.Fatalf("empty toast = %q", view)
	}
	model.modelSelector = &modelSelectorState{}
	if command, handled := model.handleNotificationShortcut(tea.KeyMsg{Type: tea.KeyCtrlN}); handled || command != nil {
		t.Fatalf("selector shortcut was intercepted: handled %v command %v", handled, command != nil)
	}
}

func TestNotificationShortcutDismissesOnlyActionableToasts(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/tmp", "test", "thread", false, false, "")
	notification := testUpdateNotification("update", "v2")
	model.notifications.add(notification)
	model.toasts.add("generic", toastInfo, 0, "", testClock())
	id := model.toasts.add("actionable", toastWarning, 0, notification.Key, testClock())
	model.notifications.bindToast(notification.Key, notificationToastIdentity(id))
	if _, handled := model.handleNotificationShortcut(tea.KeyMsg{Type: tea.KeyCtrlN}); !handled || model.notificationCenter == nil {
		t.Fatal("notification center did not open")
	}
	items := model.toasts.items
	if len(items) != 1 || items[0].Text != "generic" {
		t.Fatalf("remaining toasts = %#v", items)
	}
}

func TestNotificationShortcutPromptsToCloseOtherModal(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/tmp", "test", "thread", false, false, "")
	model.notificationSettings = newNotificationSettings(nil)
	command, handled := model.handleNotificationShortcut(tea.KeyMsg{Type: tea.KeyCtrlN})
	if !handled || command == nil || model.notificationCenter != nil {
		t.Fatalf("blocked shortcut = handled %v command %v center %#v", handled, command != nil, model.notificationCenter)
	}
	_ = command()
	if !strings.Contains(model.toasts.items[0].Text, "Close the current dialog") {
		t.Fatalf("toast = %#v", model.toasts.items)
	}
}

func TestToastExpiryUnbindsActionableSurfaceOnly(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/tmp", "test", "thread", false, false, "")
	notification := testUpdateNotification("update", "v2")
	model.notifications.add(notification)
	now := testClock()
	id := model.toasts.add("actionable", toastWarning, time.Second, notification.Key, now)
	identity := notificationToastIdentity(id)
	model.notifications.bindToast(notification.Key, identity)
	model.Update(toastExpiryMsg(now.Add(2 * time.Second)))
	if _, exists := model.notifications.keyForToast(identity); exists {
		t.Fatal("expired actionable toast retained a registry binding")
	}
	if _, exists := model.notifications.get(notification.Key); !exists {
		t.Fatal("toast expiry removed the pending notification")
	}
}

func TestModalRenderingEmitsStagedBrowserSequence(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/tmp", "test", "thread", false, false, "")
	model.resize(80, 24)
	model.browserSequence = "\x1b]777;open;https://example.test\x07"
	view := model.renderModalWithToasts("Dialog")
	if !strings.HasSuffix(view, model.browserSequence) || !strings.Contains(view, "Dialog") {
		t.Fatalf("modal omitted staged browser sequence: %q", view)
	}
}

func testClock() time.Time { return time.Unix(1_700_000_000, 0) }
