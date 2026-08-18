package dacode

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	defaultToastDuration = 4 * time.Second
	maximumToastDuration = 30 * time.Second
	defaultToastCapacity = 8
	maximumToastCapacity = 32
)

type toastSeverity string

const (
	toastInfo    toastSeverity = "info"
	toastWarning toastSeverity = "warning"
	toastError   toastSeverity = "error"
)

type toastNotice struct {
	ID        uint64
	Text      string
	Severity  toastSeverity
	ActionKey string
	ExpiresAt time.Time
}

type toastQueue struct {
	items    []toastNotice
	nextID   uint64
	capacity int
}

func newToastQueue(capacity int) *toastQueue {
	if capacity < 0 || capacity > maximumToastCapacity {
		panic("dacode: toast capacity exceeds its finite limit")
	}
	if capacity == 0 {
		capacity = defaultToastCapacity
	}
	return &toastQueue{capacity: capacity}
}

func (queue *toastQueue) add(text string, severity toastSeverity, duration time.Duration, actionKey string, now time.Time) uint64 {
	queue.requireInitialized()
	if !validToastText(text) || !validToastSeverity(severity) ||
		(actionKey != "" && !validNotificationKey(actionKey)) || now.IsZero() {
		panic("dacode: toast is invalid")
	}
	if duration < 0 || duration > maximumToastDuration {
		panic("dacode: toast duration exceeds its finite limit")
	}
	if duration == 0 {
		duration = defaultToastDuration
	}
	queue.nextID++
	if queue.nextID == 0 {
		panic("dacode: toast identity space exhausted")
	}
	item := toastNotice{ID: queue.nextID, Text: text, Severity: severity, ActionKey: actionKey, ExpiresAt: now.Add(duration)}
	if len(queue.items) == queue.capacity {
		copy(queue.items, queue.items[1:])
		queue.items[len(queue.items)-1] = item
	} else {
		queue.items = append(queue.items, item)
	}
	return item.ID
}

func (queue *toastQueue) list(now time.Time) []toastNotice {
	queue.requireInitialized()
	queue.expire(now)
	return append([]toastNotice(nil), queue.items...)
}

func (queue *toastQueue) dismiss(id uint64) (toastNotice, bool) {
	queue.requireInitialized()
	for index, item := range queue.items {
		if item.ID == id {
			queue.items = append(queue.items[:index], queue.items[index+1:]...)
			return item, true
		}
	}
	return toastNotice{}, false
}

func (queue *toastQueue) dismissActionable() []toastNotice {
	queue.requireInitialized()
	dismissed := make([]toastNotice, 0, len(queue.items))
	kept := queue.items[:0]
	for _, item := range queue.items {
		if item.ActionKey != "" {
			dismissed = append(dismissed, item)
			continue
		}
		kept = append(kept, item)
	}
	clear(queue.items[len(kept):])
	queue.items = kept
	return dismissed
}

func (queue *toastQueue) expire(now time.Time) []toastNotice {
	queue.requireInitialized()
	if now.IsZero() {
		panic("dacode: toast clock is required")
	}
	expired := make([]toastNotice, 0, len(queue.items))
	kept := queue.items[:0]
	for _, item := range queue.items {
		if now.Before(item.ExpiresAt) {
			kept = append(kept, item)
		} else {
			expired = append(expired, item)
		}
	}
	clear(queue.items[len(kept):])
	queue.items = kept
	return expired
}

func (queue *toastQueue) nextExpiry() (time.Time, bool) {
	queue.requireInitialized()
	if len(queue.items) == 0 {
		return time.Time{}, false
	}
	next := queue.items[0].ExpiresAt
	for _, item := range queue.items[1:] {
		if item.ExpiresAt.Before(next) {
			next = item.ExpiresAt
		}
	}
	return next, true
}

func (queue *toastQueue) requireInitialized() {
	if queue == nil || queue.capacity < 1 || queue.capacity > maximumToastCapacity {
		panic("dacode: initialized toast queue is required")
	}
}

func validToastText(value string) bool {
	return value != "" && len(value) <= maximumNotificationText && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validToastSeverity(severity toastSeverity) bool {
	switch severity {
	case toastInfo, toastWarning, toastError:
		return true
	default:
		return false
	}
}

func renderToasts(queue *toastQueue, width int, glyphs uiGlyphs, now time.Time) string {
	return renderToastsWithin(queue, width, int(^uint(0)>>1), glyphs, now)
}

func renderToastsWithin(queue *toastQueue, width, maximumHeight int, glyphs uiGlyphs, now time.Time) string {
	if queue == nil {
		return ""
	}
	if maximumHeight < 1 {
		return ""
	}
	items := queue.list(now)
	if len(items) == 0 {
		return ""
	}
	const maximumVisibleToasts = 3
	if len(items) > maximumVisibleToasts {
		items = items[len(items)-maximumVisibleToasts:]
	}
	contentWidth := min(max(width/2, 28), 72)
	panels := make([]string, 0, len(items))
	usedHeight := 0
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		body := boundedNotificationBody(item.Text, 2, max(contentWidth-5, 8))
		lines := []string{toastSeverityGlyph(item.Severity, glyphs) + " " + unicodesecurity.RenderTerminalSafe(body)}
		if item.ActionKey != "" {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Ctrl+N for details"))
		}
		border := lipgloss.RoundedBorder()
		if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
			border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
		}
		color := colorBody
		switch item.Severity {
		case toastWarning:
			color = colorWarning
		case toastError:
			color = colorError
		}
		panel := lipgloss.NewStyle().Border(border).BorderForeground(color).Padding(0, 1).Width(contentWidth).Render(strings.Join(lines, "\n"))
		panelHeight := lipgloss.Height(panel)
		separator := 0
		if len(panels) != 0 {
			separator = 1
		}
		if usedHeight+separator+panelHeight > maximumHeight {
			continue
		}
		panels = append([]string{panel}, panels...)
		usedHeight += separator + panelHeight
	}
	return strings.Join(panels, "\n")
}

func toastSeverityGlyph(severity toastSeverity, glyphs uiGlyphs) string {
	switch severity {
	case toastWarning:
		return glyphs.Warning
	case toastError:
		return glyphs.Error
	default:
		return glyphs.CircleFilled
	}
}
