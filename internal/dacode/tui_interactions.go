package dacode

import (
	"errors"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dacredential"
)

var errNotificationPreferenceUnavailable = errors.New("notification preference path is unavailable")

type toastExpiryMsg time.Time

type notificationPreferenceSavedMsg struct {
	write           notificationPreferenceWrite
	notificationKey string
	err             error
}

type notificationURLOpenedMsg struct{ failed bool }

func (model *tuiModel) notify(text string, severity toastSeverity, actionKey string) tea.Cmd {
	if model.toasts == nil {
		model.toasts = newToastQueue(0)
	}
	now := time.Now()
	id := model.toasts.add(text, severity, 0, actionKey, now)
	if actionKey != "" && model.notifications != nil {
		model.notifications.bindToast(actionKey, notificationToastIdentity(id))
	}
	model.relayout()
	model.refreshTranscript()
	expires, ok := model.toasts.nextExpiry()
	if !ok {
		return nil
	}
	delay := max(time.Until(expires), time.Millisecond)
	return tea.Tick(delay, func(at time.Time) tea.Msg { return toastExpiryMsg(at) })
}

func (model *tuiModel) openNotificationSettings() {
	suppressed := map[string]bool{}
	if model.notificationStore != nil {
		loaded, diagnostics := loadSuppressedWarnings(model.notificationStore.path)
		suppressed = loaded
		for _, diagnostic := range diagnostics {
			model.appendItem(transcriptItem{kind: itemError, text: diagnostic})
		}
	}
	model.notificationSettings = newNotificationSettings(suppressed)
}

func (model *tuiModel) saveNotificationPreference(key string, enabled bool) tea.Cmd {
	store := model.notificationStore
	if store == nil {
		return func() tea.Msg {
			return notificationPreferenceSavedMsg{write: notificationPreferenceWrite{Key: key, Enabled: enabled, Generation: 1}, err: errNotificationPreferenceUnavailable}
		}
	}
	write := store.beginWarningEnabled(key, enabled)
	return func() tea.Msg {
		return notificationPreferenceSavedMsg{write: write, err: store.saveWarningEnabled(write)}
	}
}

func (model *tuiModel) handleNotificationAction(request notificationActionRequest) tea.Cmd {
	notification, ok := model.notifications.get(request.Key)
	if !ok {
		return model.notify("That notification is no longer pending.", toastWarning, "")
	}
	switch request.ID {
	case notificationSuppress:
		payload, ok := notification.Payload.(missingDependencyNotification)
		if !ok || model.notificationStore == nil {
			return model.notify("That warning cannot be hidden persistently.", toastWarning, "")
		}
		warning := strings.ToLower(strings.TrimSpace(payload.Tool))
		if !validWarningKey(warning) {
			return model.notify("That warning cannot be hidden persistently.", toastWarning, "")
		}
		write := model.notificationStore.beginWarningEnabled(warning, false)
		store := model.notificationStore
		return func() tea.Msg {
			return notificationPreferenceSavedMsg{write: write, notificationKey: request.Key, err: store.saveWarningEnabled(write)}
		}
	case notificationSkipOnce:
		return model.persistUpdateChoice(request.Key, false)
	case notificationSkipVersion:
		return model.persistUpdateChoice(request.Key, true)
	case notificationInstall:
		model.notifications.remove(request.Key)
		model.notificationCenter = nil
		return model.startUpdateCommand()
	case notificationChangelog:
		return model.openNotificationURL(changelogURL)
	case notificationCopyInstall:
		payload, ok := notification.Payload.(missingDependencyNotification)
		if !ok || payload.InstallCommand == "" {
			return model.notify("No install command is available.", toastWarning, "")
		}
		return tea.Batch(model.stageTerminalSequences(osc52ClipboardSequence(payload.InstallCommand), ""), model.notify("Install command copied.", toastInfo, ""))
	case notificationOpenWebsite:
		payload, ok := notification.Payload.(missingDependencyNotification)
		if !ok || payload.URL == "" {
			return model.notify("No website is available.", toastWarning, "")
		}
		return model.openNotificationURL(payload.URL)
	case notificationEnterAPIKey:
		payload, ok := notification.Payload.(missingDependencyNotification)
		if !ok {
			return model.notify("Credential management is unavailable for that notification.", toastWarning, "")
		}
		provider := strings.ToLower(strings.TrimSpace(payload.Tool))
		if _, known := dacredential.ProviderByName(provider); !known {
			return model.notify("Credential management is unavailable for that notification.", toastWarning, "")
		}
		model.notificationAuthTarget = provider
		return model.openAuthManager()
	default:
		return model.notify("That notification action is unavailable.", toastWarning, "")
	}
}

func (model *tuiModel) openNotificationURL(value string) tea.Cmd {
	if !validNotificationURL(value) {
		return model.notify("That website address is unsafe.", toastWarning, "")
	}
	if model.browserLinks {
		return model.stageTerminalSequences("", browserOpenURLSequence(value))
	}
	opener := model.openURL
	return func() tea.Msg {
		return notificationURLOpenedMsg{failed: opener == nil || opener(value) != nil}
	}
}
