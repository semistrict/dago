package dacode

import (
	"context"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/dacredential"
)

const authTUIOperationTimeout = 30 * time.Second

type authTUIController struct {
	manager           *authManager
	oauthPath         string
	login             authSubscriptionLogin
	openURL           func(string) error
	flow              *authSubscriptionFlow
	open              bool
	refreshGeneration uint64
}

func newAuthTUIController(manager *authManager, oauthPath string, login authSubscriptionLogin, openURL func(string) error) *authTUIController {
	if manager == nil || strings.TrimSpace(oauthPath) == "" || len(oauthPath) > 4096 || strings.ContainsRune(oauthPath, 0) || login == nil || openURL == nil {
		panic("dacode: complete auth TUI dependencies are required")
	}
	return &authTUIController{manager: manager, oauthPath: oauthPath, login: login, openURL: openURL}
}

type authRefreshMsg struct {
	generation uint64
	configured bool
	notice     string
	state      authManagerState
	err        error
}

type authMutationMsg struct {
	notice string
	err    error
}

type authSubscriptionEventMsg struct {
	flow   *authSubscriptionFlow
	event  authSubscriptionEvent
	closed bool
}

func (model *tuiModel) openAuthManager() tea.Cmd {
	if model.authManager == nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Credential management is unavailable."})
		model.refreshTranscript()
		return nil
	}
	model.authManager.open = true
	model.authManager.manager.state.setNotice("Loading credential status...")
	return model.refreshAuthManager()
}

func (model *tuiModel) refreshAuthManager() tea.Cmd {
	return model.refreshAuthManagerWithNotice("")
}

func (model *tuiModel) refreshAuthManagerWithNotice(notice string) tea.Cmd {
	controller := model.authManager
	if controller == nil {
		return nil
	}
	controller.refreshGeneration++
	generation := controller.refreshGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(model.ctx, authTUIOperationTimeout)
		defer cancel()
		configured, err := storedOAuthSession(controller.oauthPath)
		fresh := newAuthManager(controller.manager.store, controller.manager.lookup)
		if err == nil {
			err = fresh.refresh(ctx, configured)
		}
		return authRefreshMsg{generation: generation, configured: configured, notice: notice, state: fresh.state, err: err}
	}
}

func (model *tuiModel) handleAuthRefresh(message authRefreshMsg) tea.Cmd {
	if model.authManager == nil || !model.authManager.open {
		return nil
	}
	if message.generation != model.authManager.refreshGeneration {
		return nil
	}
	if message.err != nil {
		model.authManager.manager.failRefresh()
		model.authManager.manager.state.setNotice("Credential status is unavailable.")
	} else {
		model.authManager.manager.state = message.state
		model.refreshModelCredentialAvailability(message.state.rows)
		if message.notice != "" {
			model.authManager.manager.state.setNotice(message.notice)
		}
		if target := model.notificationAuthTarget; target != "" {
			model.notificationAuthTarget = ""
			state := &model.authManager.manager.state
			found := false
			for index := range state.rows {
				if state.rows[index].provider == target {
					state.selected = index
					found = true
					break
				}
			}
			if found {
				return model.applyAuthManagerAction(state.beginSelected())
			}
			state.setNotice("The requested credential is unavailable.")
		}
	}
	return nil
}

func (model *tuiModel) handleAuthManagerKey(message tea.KeyMsg) tea.Cmd {
	controller := model.authManager
	if controller == nil || !controller.open {
		return nil
	}
	state := &controller.manager.state
	key := message.String()
	if key == "ctrl+c" {
		state.cancel()
		if controller.flow != nil {
			controller.flow.Cancel()
			controller.flow = nil
		}
		controller.open = false
		return model.drainInputQueue()
	}
	if key == "ctrl+d" && state.mode == authManagerList {
		state.beginRemoval()
		return nil
	}
	if state.mode == authManagerSubscription {
		switch key {
		case "c", "C":
			if target, ok := state.subscriptionAuthorizeURL(); ok {
				return model.stageTerminalSequences(osc52ClipboardSequence(target), "")
			}
		case "enter":
			if state.subscriptionPhase == authSubscriptionSucceeded || state.subscriptionPhase == authSubscriptionCancelled || state.subscriptionPhase == authSubscriptionFailed {
				state.cancel()
				return model.refreshAuthManager()
			}
		case "esc":
			state.cancel()
			if controller.flow != nil {
				controller.flow.Cancel()
				controller.flow = nil
			}
			return nil
		}
		return nil
	}
	if state.mode == authManagerAPIKey {
		switch key {
		case "enter":
			return model.applyAuthManagerAction(state.submitAPIKey())
		case "backspace", "delete":
			state.backspaceAPIKey()
		case "esc":
			state.cancel()
		default:
			if message.Type == tea.KeyRunes && len(message.Runes) > 0 {
				_ = state.appendAPIKey(string(message.Runes))
			}
		}
		return nil
	}
	if state.mode == authManagerRemoval {
		switch key {
		case "enter":
			return model.applyAuthManagerAction(state.confirmRemoval())
		case "esc":
			state.cancel()
		}
		return nil
	}
	switch key {
	case "up", "shift+tab":
		state.move(-1)
	case "down", "tab":
		state.move(1)
	case "enter":
		return model.applyAuthManagerAction(state.beginSelected())
	case "delete", "backspace":
		state.beginRemoval()
	case "esc":
		controller.open = false
		state.cancel()
		return model.drainInputQueue()
	}
	return nil
}

func (model *tuiModel) applyAuthManagerAction(action authManagerAction) tea.Cmd {
	controller := model.authManager
	if controller == nil {
		return nil
	}
	switch action.kind {
	case authManagerSaveAPIKey:
		secret := action.consumeSecret()
		if secret == "" {
			return nil
		}
		provider := action.provider
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(model.ctx, authTUIOperationTimeout)
			defer cancel()
			err := controller.manager.store.SetAPIKey(ctx, provider, secret, "", "")
			return authMutationMsg{notice: "Credential saved. Restart or reload to apply it to active models.", err: err}
		}
	case authManagerRemoveCredential:
		provider := action.provider
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(model.ctx, authTUIOperationTimeout)
			defer cancel()
			err := removeAuthManagerCredential(ctx, controller.manager.store, controller.oauthPath, provider)
			return authMutationMsg{notice: "Stored credential removed. Restart or reload to apply the change.", err: err}
		}
	case authManagerStartSubscription:
		if controller.flow != nil {
			controller.flow.Cancel()
		}
		controller.flow = startAuthSubscriptionFlow(model.ctx, action.generation, controller.oauthPath, controller.login, authSubscriptionFlowOptions{OpenURL: controller.openURL})
		return waitAuthSubscriptionEvent(controller.flow)
	default:
		return nil
	}
}

func removeAuthManagerCredential(ctx context.Context, store *dacredential.Store, oauthPath, provider string) error {
	if store == nil {
		panic("dacode: credential store is required")
	}
	if _, err := store.Remove(ctx, provider); err != nil {
		return err
	}
	if provider != "openai_oauth" {
		return nil
	}
	stored, err := storedOAuthSession(oauthPath)
	if err != nil || !stored {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Remove(oauthPath)
}

func waitAuthSubscriptionEvent(flow *authSubscriptionFlow) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-flow.Events
		return authSubscriptionEventMsg{flow: flow, event: event, closed: !ok}
	}
}

func (model *tuiModel) handleAuthSubscriptionEvent(message authSubscriptionEventMsg) tea.Cmd {
	controller := model.authManager
	if controller == nil || !controller.open || controller.flow != message.flow {
		return nil
	}
	if message.closed {
		controller.flow = nil
		return nil
	}
	controller.manager.state.applySubscriptionEvent(message.event)
	return waitAuthSubscriptionEvent(message.flow)
}

func (model *tuiModel) handleAuthMutation(message authMutationMsg) tea.Cmd {
	if model.authManager == nil || !model.authManager.open {
		return nil
	}
	if message.err != nil {
		model.authManager.manager.state.setNotice("Credential update failed.")
		return nil
	}
	return model.refreshAuthManagerWithNotice(message.notice)
}

func (model *tuiModel) renderAuthManager() string {
	if model.authManager == nil || !model.authManager.open {
		return ""
	}
	return model.authManager.manager.state.render(model.width, model.height)
}
