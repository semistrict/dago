package dacode

import (
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maximumPendingNotifications = 256
	maximumNotificationActions  = 16
	maximumNotificationText     = 16 << 10
	maximumNotificationTitle    = 512
	maximumNotificationLabel    = 256
)

type notificationActionID string

const (
	notificationSuppress    notificationActionID = "suppress"
	notificationCopyInstall notificationActionID = "copy_install"
	notificationOpenWebsite notificationActionID = "open_website"
	notificationEnterAPIKey notificationActionID = "enter_api_key"
	notificationInstall     notificationActionID = "install"
	notificationSkipOnce    notificationActionID = "skip_once"
	notificationSkipVersion notificationActionID = "skip_version"
	notificationChangelog   notificationActionID = "changelog"
)

type notificationAction struct {
	ID      notificationActionID
	Label   string
	Primary bool
}

type notificationPayload interface{ notificationPayload() }

type missingDependencyNotification struct {
	Tool           string
	InstallCommand string
	URL            string
}

func (missingDependencyNotification) notificationPayload() {}

type updateAvailableNotification struct {
	Latest      string
	UpgradeArgs []string
}

func (updateAvailableNotification) notificationPayload() {}

type pendingNotification struct {
	Key     string
	Title   string
	Body    string
	Actions []notificationAction
	Payload notificationPayload
}

func newPendingNotification(key, title, body string, payload notificationPayload, actions ...notificationAction) pendingNotification {
	notification := pendingNotification{
		Key: key, Title: title, Body: body, Payload: payload,
		Actions: append([]notificationAction(nil), actions...),
	}
	validatePendingNotification(notification)
	return notification
}

func validatePendingNotification(notification pendingNotification) {
	if !validNotificationKey(notification.Key) {
		panic("dacode: notification key is invalid")
	}
	if !validBoundedNotificationText(notification.Title, maximumNotificationTitle) || !validNotificationText(notification.Body) {
		panic("dacode: notification text is invalid")
	}
	if notification.Payload == nil {
		panic("dacode: notification payload is required")
	}
	if len(notification.Actions) == 0 || len(notification.Actions) > maximumNotificationActions {
		panic("dacode: notification actions are required and bounded")
	}
	primary := 0
	seen := make(map[notificationActionID]struct{}, len(notification.Actions))
	for _, action := range notification.Actions {
		if !validNotificationAction(action.ID) || !validBoundedNotificationText(action.Label, maximumNotificationLabel) {
			panic("dacode: notification action is invalid")
		}
		if _, exists := seen[action.ID]; exists {
			panic("dacode: notification action is duplicated")
		}
		seen[action.ID] = struct{}{}
		if action.Primary {
			primary++
		}
	}
	if primary > 1 {
		panic("dacode: notification has multiple primary actions")
	}
	validateNotificationPayload(notification.Payload)
	validateNotificationActions(notification.Payload, notification.Actions)
}

func validateNotificationActions(payload notificationPayload, actions []notificationAction) {
	for _, action := range actions {
		switch typed := payload.(type) {
		case missingDependencyNotification:
			switch action.ID {
			case notificationSuppress, notificationEnterAPIKey:
			case notificationCopyInstall:
				if typed.InstallCommand == "" {
					panic("dacode: copy-install notification requires an install command")
				}
			case notificationOpenWebsite:
				if typed.URL == "" {
					panic("dacode: open-website notification requires a URL")
				}
			default:
				panic("dacode: notification action is incompatible with its payload")
			}
		case updateAvailableNotification:
			switch action.ID {
			case notificationInstall, notificationSkipOnce, notificationSkipVersion, notificationChangelog:
			default:
				panic("dacode: notification action is incompatible with its payload")
			}
		}
	}
}

func validateNotificationPayload(payload notificationPayload) {
	switch typed := payload.(type) {
	case missingDependencyNotification:
		if !validNotificationText(typed.Tool) || (typed.InstallCommand == "" && typed.URL == "") ||
			!validOptionalNotificationText(typed.InstallCommand) || !validOptionalNotificationText(typed.URL) {
			panic("dacode: missing-dependency notification payload is invalid")
		}
		if typed.URL != "" && !validNotificationURL(typed.URL) {
			panic("dacode: missing-dependency notification URL is invalid")
		}
	case updateAvailableNotification:
		if !validNotificationText(typed.Latest) || len(typed.UpgradeArgs) == 0 || len(typed.UpgradeArgs) > 64 {
			panic("dacode: update notification payload is invalid")
		}
		for _, argument := range typed.UpgradeArgs {
			if !validNotificationText(argument) {
				panic("dacode: update notification argument is invalid")
			}
		}
	default:
		panic("dacode: notification payload type is unsupported")
	}
}

func validNotificationAction(action notificationActionID) bool {
	switch action {
	case notificationSuppress, notificationCopyInstall, notificationOpenWebsite, notificationEnterAPIKey,
		notificationInstall, notificationSkipOnce, notificationSkipVersion, notificationChangelog:
		return true
	default:
		return false
	}
}

func validNotificationKey(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validNotificationText(value string) bool {
	return value != "" && validOptionalNotificationText(value)
}

func validBoundedNotificationText(value string, maximum int) bool {
	return len(value) <= maximum && validNotificationText(value)
}

func validOptionalNotificationText(value string) bool {
	return len(value) <= maximumNotificationText && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validNotificationURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	return len(value) <= 4096 && !strings.ContainsAny(value, "\x00\r\n")
}

func clonePendingNotification(notification pendingNotification) pendingNotification {
	result := notification
	result.Actions = append([]notificationAction(nil), notification.Actions...)
	if payload, ok := notification.Payload.(updateAvailableNotification); ok {
		payload.UpgradeArgs = append([]string(nil), payload.UpgradeArgs...)
		result.Payload = payload
	}
	return result
}

type notificationRegistry struct {
	mu sync.RWMutex

	entries    map[string]pendingNotification
	order      []string
	keyToToast map[string]string
	toastToKey map[string]string
}

func newNotificationRegistry() *notificationRegistry {
	return &notificationRegistry{
		entries: make(map[string]pendingNotification), keyToToast: make(map[string]string), toastToKey: make(map[string]string),
	}
}

func (registry *notificationRegistry) add(notification pendingNotification) {
	registry.requireInitialized()
	validatePendingNotification(notification)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.entries[notification.Key]; !exists {
		if len(registry.entries) >= maximumPendingNotifications {
			panic("dacode: pending notification limit exceeded")
		}
		registry.order = append(registry.order, notification.Key)
	}
	registry.unbindKeyLocked(notification.Key)
	registry.entries[notification.Key] = clonePendingNotification(notification)
}

func (registry *notificationRegistry) remove(key string) (pendingNotification, bool) {
	registry.requireInitialized()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	notification, exists := registry.entries[key]
	if !exists {
		return pendingNotification{}, false
	}
	delete(registry.entries, key)
	registry.unbindKeyLocked(key)
	for index, ordered := range registry.order {
		if ordered == key {
			registry.order = append(registry.order[:index], registry.order[index+1:]...)
			break
		}
	}
	return clonePendingNotification(notification), true
}

func (registry *notificationRegistry) get(key string) (pendingNotification, bool) {
	registry.requireInitialized()
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	notification, exists := registry.entries[key]
	return clonePendingNotification(notification), exists
}

func (registry *notificationRegistry) list() []pendingNotification {
	registry.requireInitialized()
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	result := make([]pendingNotification, 0, len(registry.entries))
	for _, key := range registry.order {
		if notification, exists := registry.entries[key]; exists {
			result = append(result, clonePendingNotification(notification))
		}
	}
	return result
}

func (registry *notificationRegistry) bindToast(key, identity string) bool {
	registry.requireInitialized()
	if !validNotificationKey(identity) {
		panic("dacode: toast identity is invalid")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.entries[key]; !exists {
		return false
	}
	registry.unbindKeyLocked(key)
	if previousKey, exists := registry.toastToKey[identity]; exists {
		delete(registry.keyToToast, previousKey)
	}
	registry.keyToToast[key] = identity
	registry.toastToKey[identity] = key
	return true
}

func (registry *notificationRegistry) unbindToast(identity string) {
	registry.requireInitialized()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if key, exists := registry.toastToKey[identity]; exists {
		delete(registry.toastToKey, identity)
		delete(registry.keyToToast, key)
	}
}

func (registry *notificationRegistry) keyForToast(identity string) (string, bool) {
	registry.requireInitialized()
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	key, exists := registry.toastToKey[identity]
	return key, exists
}

func (registry *notificationRegistry) toastForKey(key string) (string, bool) {
	registry.requireInitialized()
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	identity, exists := registry.keyToToast[key]
	return identity, exists
}

func (registry *notificationRegistry) clear() {
	registry.requireInitialized()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	clear(registry.entries)
	clear(registry.keyToToast)
	clear(registry.toastToKey)
	registry.order = nil
}

func (registry *notificationRegistry) unbindKeyLocked(key string) {
	if identity, exists := registry.keyToToast[key]; exists {
		delete(registry.keyToToast, key)
		delete(registry.toastToKey, identity)
	}
}

func (registry *notificationRegistry) requireInitialized() {
	if registry == nil || registry.entries == nil || registry.keyToToast == nil || registry.toastToKey == nil {
		panic("dacode: initialized notification registry is required")
	}
}
