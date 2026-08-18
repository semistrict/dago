package dacode

import (
	"fmt"
	"sync"
	"testing"
)

func testUpdateNotification(key, latest string) pendingNotification {
	return newPendingNotification(
		key, "Update available", "A verified update is ready.",
		updateAvailableNotification{Latest: latest, UpgradeArgs: []string{"update", "--version", latest}},
		notificationAction{ID: notificationInstall, Label: "Install", Primary: true},
		notificationAction{ID: notificationSkipOnce, Label: "Later"},
	)
}

func TestNotificationRegistryDeduplicatesAndPreservesInsertionOrder(t *testing.T) {
	registry := newNotificationRegistry()
	registry.add(testUpdateNotification("update", "v1.2.3"))
	registry.add(newPendingNotification(
		"missing-tool", "Missing tool", "Install the optional tool.",
		missingDependencyNotification{Tool: "search", InstallCommand: "install search"},
		notificationAction{ID: notificationCopyInstall, Label: "Copy"},
	))
	if !registry.bindToast("update", "toast-old") {
		t.Fatal("expected toast binding")
	}
	registry.add(testUpdateNotification("update", "v1.2.4"))

	entries := registry.list()
	if len(entries) != 2 || entries[0].Key != "update" || entries[1].Key != "missing-tool" {
		t.Fatalf("entries = %#v", entries)
	}
	if payload := entries[0].Payload.(updateAvailableNotification); payload.Latest != "v1.2.4" {
		t.Fatalf("replacement payload = %#v", payload)
	}
	if _, exists := registry.keyForToast("toast-old"); exists {
		t.Fatal("replacement retained stale toast binding")
	}
}

func TestNotificationRegistryOwnsCopiesAndBindings(t *testing.T) {
	registry := newNotificationRegistry()
	notification := testUpdateNotification("update", "v2.0.0")
	registry.add(notification)
	notification.Actions[0].Label = "mutated"
	payload := notification.Payload.(updateAvailableNotification)
	payload.UpgradeArgs[0] = "mutated"

	stored, exists := registry.get("update")
	if !exists || stored.Actions[0].Label != "Install" || stored.Payload.(updateAvailableNotification).UpgradeArgs[0] != "update" {
		t.Fatalf("stored notification was mutated: %#v", stored)
	}
	if !registry.bindToast("update", "toast-1") {
		t.Fatal("bind failed")
	}
	if !registry.bindToast("update", "toast-2") {
		t.Fatal("replacement bind failed")
	}
	if _, exists := registry.keyForToast("toast-1"); exists {
		t.Fatal("old toast still routes")
	}
	if key, exists := registry.keyForToast("toast-2"); !exists || key != "update" {
		t.Fatalf("toast route = %q, %v", key, exists)
	}
	registry.unbindToast("toast-2")
	if _, exists := registry.toastForKey("update"); exists {
		t.Fatal("toast remained bound")
	}
}

func TestNotificationRegistryRemoveClearAndUnknownOperations(t *testing.T) {
	registry := newNotificationRegistry()
	if registry.bindToast("missing", "toast") {
		t.Fatal("unknown notification accepted a toast")
	}
	registry.add(testUpdateNotification("update", "v1.0.0"))
	removed, exists := registry.remove("update")
	if !exists || removed.Key != "update" {
		t.Fatalf("removed = %#v, %v", removed, exists)
	}
	if _, exists := registry.remove("update"); exists {
		t.Fatal("removed notification twice")
	}
	registry.add(testUpdateNotification("update", "v1.0.0"))
	registry.clear()
	if entries := registry.list(); len(entries) != 0 {
		t.Fatalf("entries after clear = %#v", entries)
	}
}

func TestNotificationRegistryIsConcurrent(t *testing.T) {
	registry := newNotificationRegistry()
	var group sync.WaitGroup
	for index := 0; index < 64; index++ {
		index := index
		group.Go(func() {
			key := fmt.Sprintf("update-%02d", index)
			registry.add(testUpdateNotification(key, "v1.0.0"))
			if !registry.bindToast(key, "toast-"+key) {
				t.Errorf("bind %q failed", key)
			}
		})
	}
	group.Wait()
	if entries := registry.list(); len(entries) != 64 {
		t.Fatalf("entries = %d", len(entries))
	}
}

func TestPendingNotificationRejectsInvalidStaticConfiguration(t *testing.T) {
	tests := []struct {
		name  string
		build func()
	}{
		{"empty key", func() {
			newPendingNotification("", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://example.test"}, notificationAction{ID: notificationOpenWebsite, Label: "Open"})
		}},
		{"no actions", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://example.test"})
		}},
		{"two primaries", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://example.test"}, notificationAction{ID: notificationOpenWebsite, Label: "Open", Primary: true}, notificationAction{ID: notificationSuppress, Label: "Hide", Primary: true})
		}},
		{"duplicate action", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://example.test"}, notificationAction{ID: notificationSuppress, Label: "Hide"}, notificationAction{ID: notificationSuppress, Label: "Hide again"})
		}},
		{"credential URL", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://user:" + "pass@example.test"}, notificationAction{ID: notificationOpenWebsite, Label: "Open"})
		}},
		{"unsupported payload", func() {
			newPendingNotification("key", "title", "body", fakeNotificationPayload{}, notificationAction{ID: notificationSuppress, Label: "Hide"})
		}},
		{"copy without command", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://example.test"}, notificationAction{ID: notificationCopyInstall, Label: "Copy"})
		}},
		{"open without URL", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", InstallCommand: "install x"}, notificationAction{ID: notificationOpenWebsite, Label: "Open"})
		}},
		{"update suppression", func() {
			newPendingNotification("key", "title", "body", updateAvailableNotification{Latest: "v2", UpgradeArgs: []string{"update"}}, notificationAction{ID: notificationSuppress, Label: "Hide"})
		}},
		{"URL without hostname", func() {
			newPendingNotification("key", "title", "body", missingDependencyNotification{Tool: "x", URL: "https://:443"}, notificationAction{ID: notificationOpenWebsite, Label: "Open"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			test.build()
		})
	}
}

type fakeNotificationPayload struct{}

func (fakeNotificationPayload) notificationPayload() {}
