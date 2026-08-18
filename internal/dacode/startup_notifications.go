package dacode

import "time"

func (model *tuiModel) configureStartupNotifications(suppressed map[string]bool, ripgrepAvailable, webSearchConfigured, unrestricted bool) {
	if model.notifications == nil || model.toasts == nil {
		panic("dacode: initialized notification surfaces are required")
	}
	if !ripgrepAvailable && !suppressed[warningRipgrep] {
		model.addStartupNotification(newPendingNotification(
			"warning:ripgrep", "ripgrep is not installed",
			"Fast workspace search is limited until ripgrep is installed.",
			missingDependencyNotification{Tool: warningRipgrep, URL: "https://github.com/BurntSushi/ripgrep#installation"},
			notificationAction{ID: notificationOpenWebsite, Label: "Open installation guide", Primary: true},
			notificationAction{ID: notificationSuppress, Label: "Hide this warning"},
		))
	}
	if !webSearchConfigured && !suppressed[warningTavily] {
		model.addStartupNotification(newPendingNotification(
			"warning:tavily", "Web search is not configured",
			"Add a Tavily API key to enable the web_search tool.",
			missingDependencyNotification{Tool: warningTavily, URL: "https://app.tavily.com/"},
			notificationAction{ID: notificationEnterAPIKey, Label: "Enter API key", Primary: true},
			notificationAction{ID: notificationOpenWebsite, Label: "Open Tavily"},
			notificationAction{ID: notificationSuppress, Label: "Hide this warning"},
		))
	}
	if unrestricted && !suppressed[warningYOLO] {
		model.addStartupNotification(newPendingNotification(
			"warning:yolo", "Unrestricted mode is active",
			"Local actions may run without confirmation in this thread.",
			missingDependencyNotification{Tool: warningYOLO, URL: "https://docs.langchain.com/oss/python/deepagents/cli/overview"},
			notificationAction{ID: notificationOpenWebsite, Label: "Review safety guidance", Primary: true},
			notificationAction{ID: notificationSuppress, Label: "Hide this warning"},
		))
	}
}

func (model *tuiModel) addStartupNotification(notification pendingNotification) {
	model.notifications.add(notification)
	id := model.toasts.add(notification.Title, toastWarning, 0, notification.Key, time.Now())
	model.notifications.bindToast(notification.Key, notificationToastIdentity(id))
}
