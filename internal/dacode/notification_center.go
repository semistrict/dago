package dacode

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type notificationActionRequest struct {
	Key string
	ID  notificationActionID
}

type notificationCenterState struct {
	entries  []pendingNotification
	selected int
	detail   bool
	action   int
}

func newNotificationCenter(entries []pendingNotification) *notificationCenterState {
	cloned := make([]pendingNotification, len(entries))
	for index, entry := range entries {
		validatePendingNotification(entry)
		cloned[index] = clonePendingNotification(entry)
	}
	return &notificationCenterState{entries: cloned}
}

func (center *notificationCenterState) handleKey(message tea.KeyMsg) (*notificationActionRequest, bool) {
	center.requireInitialized()
	if len(center.entries) == 0 {
		return nil, true
	}
	if key.Matches(message, key.NewBinding(key.WithKeys("esc"))) {
		if center.detail {
			center.detail = false
			center.action = 0
			return nil, false
		}
		return nil, true
	}
	move := 0
	if key.Matches(message, key.NewBinding(key.WithKeys("up", "k", "shift+tab"))) {
		move = -1
	} else if key.Matches(message, key.NewBinding(key.WithKeys("down", "j", "tab"))) {
		move = 1
	}
	if center.detail {
		actions := center.entries[center.selected].Actions
		if move != 0 {
			center.action = wrapIndex(center.action+move, len(actions))
			return nil, false
		}
		if key.Matches(message, key.NewBinding(key.WithKeys("enter"))) {
			action := actions[center.action]
			request := &notificationActionRequest{Key: center.entries[center.selected].Key, ID: action.ID}
			keepOpen := action.ID == notificationSuppress || action.ID == notificationEnterAPIKey || action.ID == notificationChangelog
			return request, !keepOpen
		}
		return nil, false
	}
	if move != 0 {
		center.selected = wrapIndex(center.selected+move, len(center.entries))
		return nil, false
	}
	if key.Matches(message, key.NewBinding(key.WithKeys("enter"))) {
		center.detail = true
		center.action = primaryNotificationAction(center.entries[center.selected].Actions)
	}
	return nil, false
}

func (center *notificationCenterState) reload(entries []pendingNotification) bool {
	center.requireInitialized()
	selectedKey := ""
	previousSelection := center.selected
	if len(center.entries) > 0 && center.selected < len(center.entries) {
		selectedKey = center.entries[center.selected].Key
	}
	replacement := newNotificationCenter(entries)
	center.entries = replacement.entries
	center.detail = false
	center.action = 0
	center.selected = min(previousSelection, max(len(center.entries)-1, 0))
	for index, entry := range center.entries {
		if entry.Key == selectedKey {
			center.selected = index
			break
		}
	}
	return len(center.entries) != 0
}

func (center *notificationCenterState) render(width, height int, glyphs uiGlyphs) string {
	center.requireInitialized()
	if center.detail && len(center.entries) != 0 {
		return center.renderDetail(width, height, glyphs)
	}
	contentWidth := min(max(width-12, 40), 76)
	lines := []string{modalTitle("Notifications"), ""}
	if len(center.entries) == 0 {
		lines = append(lines, mutedModalText("No pending notifications."))
	} else {
		visible := min(len(center.entries), max(height-8, 4))
		start := boundedWindowStart(center.selected, visible, len(center.entries))
		for index := start; index < start+visible; index++ {
			label := unicodesecurity.RenderTerminalSafe(center.entries[index].Title)
			lines = append(lines, modalRow(label, index == center.selected, contentWidth, glyphs))
		}
	}
	lines = append(lines, "", mutedModalText(glyphs.ArrowUp+"/"+glyphs.ArrowDown+", j/k, or Tab navigate  "+glyphs.Bullet+"  Enter open  "+glyphs.Bullet+"  Esc close"))
	return placeModal(lines, contentWidth, width, height, glyphs)
}

func (center *notificationCenterState) renderDetail(width, height int, glyphs uiGlyphs) string {
	entry := center.entries[center.selected]
	contentWidth := min(max(width-12, 40), 76)
	body := boundedNotificationBody(entry.Body, max(height-len(entry.Actions)-11, 3), contentWidth)
	lines := []string{modalTitle(entry.Title), "", lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth).Render(body), ""}
	for index, action := range entry.Actions {
		label := unicodesecurity.RenderTerminalSafe(action.Label)
		if action.Primary {
			label += " (recommended)"
		}
		lines = append(lines, modalRow(label, index == center.action, contentWidth, glyphs))
	}
	lines = append(lines, "", mutedModalText(glyphs.ArrowUp+"/"+glyphs.ArrowDown+" or Tab navigate  "+glyphs.Bullet+"  Enter choose  "+glyphs.Bullet+"  Esc back"))
	return placeModal(lines, contentWidth, width, height, glyphs)
}

func primaryNotificationAction(actions []notificationAction) int {
	for index, action := range actions {
		if action.Primary {
			return index
		}
	}
	return 0
}

func (center *notificationCenterState) requireInitialized() {
	if center == nil || center.selected < 0 || center.action < 0 ||
		(len(center.entries) == 0 && (center.selected != 0 || center.detail)) ||
		(len(center.entries) != 0 && center.selected >= len(center.entries)) ||
		(center.detail && center.action >= len(center.entries[center.selected].Actions)) {
		panic("dacode: initialized notification center is required")
	}
}

type warningToggle struct {
	Key   string
	Label string
}

var notificationWarningToggles = []warningToggle{
	{Key: warningRipgrep, Label: "Warn when ripgrep is not installed"},
	{Key: warningTavily, Label: "Warn when the web-search API key is not set"},
	{Key: warningYOLO, Label: "Warn when unrestricted mode is active"},
}

type notificationSettingsState struct {
	suppressed map[string]bool
	selected   int
}

func newNotificationSettings(suppressed map[string]bool) *notificationSettingsState {
	cloned := make(map[string]bool, len(suppressed))
	for key, value := range suppressed {
		if value && validWarningKey(key) {
			cloned[key] = true
		}
	}
	return &notificationSettingsState{suppressed: cloned}
}

func (settings *notificationSettingsState) handleKey(message tea.KeyMsg) (key string, enabled bool, changed bool, close bool) {
	settings.requireInitialized()
	switch message.String() {
	case "esc":
		return "", false, false, true
	case "up", "k", "shift+tab":
		settings.selected = wrapIndex(settings.selected-1, len(notificationWarningToggles))
	case "down", "j", "tab":
		settings.selected = wrapIndex(settings.selected+1, len(notificationWarningToggles))
	case "enter", " ":
		selected := notificationWarningToggles[settings.selected].Key
		if settings.suppressed[selected] {
			delete(settings.suppressed, selected)
			return selected, true, true, false
		}
		settings.suppressed[selected] = true
		return selected, false, true, false
	}
	return "", false, false, false
}

func (settings *notificationSettingsState) rollback(key string, enabled bool) {
	settings.requireInitialized()
	if !validWarningKey(key) {
		panic("dacode: notification warning is unavailable")
	}
	if enabled {
		delete(settings.suppressed, key)
	} else {
		settings.suppressed[key] = true
	}
}

func (settings *notificationSettingsState) render(width, height int, glyphs uiGlyphs) string {
	settings.requireInitialized()
	contentWidth := min(max(width-12, 40), 76)
	lines := []string{modalTitle("Notification Settings"), ""}
	for index, toggle := range notificationWarningToggles {
		mark := "[x]"
		if settings.suppressed[toggle.Key] {
			mark = "[ ]"
		}
		lines = append(lines, modalRow(mark+" "+toggle.Label, index == settings.selected, contentWidth, glyphs))
	}
	lines = append(lines, "", mutedModalText(glyphs.ArrowUp+"/"+glyphs.ArrowDown+" or Tab navigate  "+glyphs.Bullet+"  Space/Enter toggle  "+glyphs.Bullet+"  Esc close"))
	return placeModal(lines, contentWidth, width, height, glyphs)
}

func (settings *notificationSettingsState) requireInitialized() {
	if settings == nil || settings.suppressed == nil || settings.selected < 0 || settings.selected >= len(notificationWarningToggles) {
		panic("dacode: initialized notification settings are required")
	}
}

func wrapIndex(index, length int) int {
	if length < 1 {
		panic("dacode: selection requires at least one item")
	}
	return (index%length + length) % length
}

func boundedWindowStart(selected, visible, length int) int {
	start := max(selected-visible/2, 0)
	return min(start, max(length-visible, 0))
}

func modalTitle(value string) string {
	return lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(unicodesecurity.RenderTerminalSafe(value))
}

func mutedModalText(value string) string {
	return lipgloss.NewStyle().Foreground(colorMuted).Render(value)
}

func modalRow(label string, selected bool, width int, glyphs uiGlyphs) string {
	prefix := "  "
	style := lipgloss.NewStyle().Foreground(colorBody).Width(width)
	if selected {
		prefix = glyphs.Cursor + " "
		style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
	}
	return style.Render(prefix + label)
}

func placeModal(lines []string, contentWidth, width, height int, glyphs uiGlyphs) string {
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func boundedNotificationBody(value string, maximumLines, maximumRunes int) string {
	safe := unicodesecurity.RenderTerminalSafe(value)
	lines := strings.Split(safe, "\n")
	truncated := len(lines) > maximumLines
	if truncated {
		lines = lines[:maximumLines]
	}
	for index, line := range lines {
		runes := []rune(line)
		if len(runes) > maximumRunes {
			lines[index] = string(runes[:max(maximumRunes-3, 1)]) + "..."
		}
	}
	if truncated {
		lines[len(lines)-1] = "..."
	}
	return strings.Join(lines, "\n")
}

func (request notificationActionRequest) String() string {
	return fmt.Sprintf("%s:%s", request.Key, request.ID)
}
