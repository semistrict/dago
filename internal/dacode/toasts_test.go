package dacode

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestToastQueueDefaultsBoundsAndExpires(t *testing.T) {
	queue := newToastQueue(0)
	now := time.Unix(100, 0)
	first := queue.add("Copied", toastInfo, 0, "", now)
	second := queue.add("Update available", toastWarning, time.Second, "update", now)
	items := queue.list(now)
	if first == 0 || second <= first || len(items) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].ExpiresAt != now.Add(defaultToastDuration) || items[1].ActionKey != "update" {
		t.Fatalf("toast defaults = %#v", items)
	}
	items = queue.list(now.Add(2 * time.Second))
	if len(items) != 1 || items[0].ID != first {
		t.Fatalf("expired items = %#v", items)
	}
	if expiry, exists := queue.nextExpiry(); !exists || expiry != now.Add(defaultToastDuration) {
		t.Fatalf("next expiry = %v, %v", expiry, exists)
	}
	if dismissed, exists := queue.dismiss(first); !exists || dismissed.Text != "Copied" {
		t.Fatalf("dismissed = %#v, %v", dismissed, exists)
	}
	if _, exists := queue.nextExpiry(); exists {
		t.Fatal("empty queue has an expiry")
	}
}

func TestToastRenderingIsBoundedSafeActionableAndASCII(t *testing.T) {
	queue := newToastQueue(8)
	now := time.Unix(100, 0)
	for index, text := range []string{"old", "info", "warn", "unsafe\x1b[31m", strings.Repeat("x", 1000)} {
		severity := toastInfo
		if index == 2 {
			severity = toastWarning
		} else if index == 3 {
			severity = toastError
		}
		action := ""
		if index == 3 {
			action = "update"
		}
		queue.add(text, severity, 0, action, now)
	}
	view := ansi.Strip(renderToasts(queue, 80, asciiUIGlyphs, now))
	if strings.Contains(view, "old") || strings.Contains(view, "info") || !strings.Contains(view, "Ctrl+N for details") || !strings.Contains(view, "<U+001B CONTROL>") {
		t.Fatalf("toast view:\n%s", view)
	}
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎↑↓›") {
		t.Fatalf("ASCII toast view contains Unicode UI glyphs:\n%s", view)
	}
	if got := renderToasts(queue, 80, asciiUIGlyphs, now.Add(defaultToastDuration)); got != "" {
		t.Fatalf("expired toasts rendered: %q", got)
	}
}

func TestToastQueueEvictsOldestAtCapacity(t *testing.T) {
	queue := newToastQueue(2)
	now := time.Unix(100, 0)
	queue.add("one", toastInfo, 0, "", now)
	second := queue.add("two", toastInfo, 0, "", now)
	third := queue.add("three", toastError, 0, "", now)
	items := queue.list(now)
	if len(items) != 2 || items[0].ID != second || items[1].ID != third {
		t.Fatalf("items = %#v", items)
	}
}

func TestToastQueueDismissesOnlyActionableNotices(t *testing.T) {
	queue := newToastQueue(0)
	now := time.Unix(100, 0)
	info := queue.add("Saved", toastInfo, 0, "", now)
	first := queue.add("Update available", toastWarning, 0, "update", now)
	second := queue.add("Missing dependency", toastWarning, 0, "dependency", now)
	dismissed := queue.dismissActionable()
	if len(dismissed) != 2 || dismissed[0].ID != first || dismissed[1].ID != second {
		t.Fatalf("dismissed = %#v", dismissed)
	}
	items := queue.list(now)
	if len(items) != 1 || items[0].ID != info {
		t.Fatalf("remaining = %#v", items)
	}
}

func TestToastQueueRejectsInvalidStaticConfiguration(t *testing.T) {
	now := time.Unix(100, 0)
	tests := []func(){
		func() { newToastQueue(-1) },
		func() { newToastQueue(maximumToastCapacity + 1) },
		func() { newToastQueue(1).add("", toastInfo, 0, "", now) },
		func() { newToastQueue(1).add("text", "unknown", 0, "", now) },
		func() { newToastQueue(1).add("text", toastInfo, maximumToastDuration+time.Second, "", now) },
		func() { newToastQueue(1).add("text", toastInfo, 0, "bad\nkey", now) },
	}
	for index, test := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			test()
		})
	}
}
