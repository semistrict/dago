package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/semistrict/dago/daupdate"
)

const (
	updateTUIOperationTimeout = 2 * time.Minute
	updateFailureCooldown     = time.Hour
)

type updateTUIOperation uint8

const (
	updateTUICheck updateTUIOperation = iota
	updateTUIApply
)

type updateTUIResultMsg struct {
	generation uint64
	operation  updateTUIOperation
	startup    bool
	result     daupdate.Result
	err        error
}

type updateStateSavedMsg struct {
	generation      uint64
	notice          string
	state           updatePersistentState
	notificationKey string
	err             error
}

type autoUpdateSavedMsg struct {
	generation uint64
	enabled    bool
	stale      bool
	err        error
}

type autoUpdateController struct {
	mu         sync.Mutex
	store      *autoUpdatePreferenceStore
	preference autoUpdatePreference
	generation uint64
}

var errStaleUpdateStateWrite = errors.New("stale update state write")

type updateStateWriteController struct {
	store  *updateStateStore
	latest atomic.Uint64
}

func newUpdateStateWriteController(store *updateStateStore) *updateStateWriteController {
	if store == nil {
		panic("dacode: update state store is required")
	}
	return &updateStateWriteController{store: store}
}

func (controller *updateStateWriteController) begin() uint64 {
	if controller == nil || controller.store == nil {
		panic("dacode: update state write controller is required")
	}
	generation := controller.latest.Add(1)
	if generation == 0 {
		panic("dacode: update state generation exhausted")
	}
	return generation
}

func (controller *updateStateWriteController) update(ctx context.Context, generation uint64, mutate func(*updatePersistentState) error) (updatePersistentState, bool, error) {
	if controller == nil || controller.store == nil || mutate == nil || generation == 0 {
		panic("dacode: update state write dependencies are required")
	}
	if controller.latest.Load() != generation {
		return updatePersistentState{}, true, nil
	}
	state, err := controller.store.Update(ctx, func(state *updatePersistentState) error {
		if controller.latest.Load() != generation {
			return errStaleUpdateStateWrite
		}
		return mutate(state)
	})
	if errors.Is(err, errStaleUpdateStateWrite) || controller.latest.Load() != generation {
		return updatePersistentState{}, true, nil
	}
	return state, false, err
}

func newAutoUpdateController(store *autoUpdatePreferenceStore, preference autoUpdatePreference) *autoUpdateController {
	if store == nil {
		panic("dacode: auto-update preference store is required")
	}
	return &autoUpdateController{store: store, preference: preference}
}

func (controller *autoUpdateController) begin(enabled bool) uint64 {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.generation++
	if controller.generation == 0 {
		controller.generation = 1
	}
	return controller.generation
}

func (controller *autoUpdateController) save(generation uint64, enabled bool) (bool, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if generation == 0 || generation != controller.generation {
		return true, nil
	}
	return false, controller.store.set(enabled)
}

func (controller *autoUpdateController) apply(generation uint64, enabled bool) bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if generation != controller.generation {
		return false
	}
	controller.preference = autoUpdatePreference{Enabled: enabled, Explicit: true, Source: "config"}
	return true
}

func (controller *autoUpdateController) current(generation uint64) bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return generation != 0 && generation == controller.generation
}

func (model *tuiModel) configureUpdates(profile *tuiUpdateProfile, preferences *autoUpdatePreferenceStore, state *updateStateStore, preference autoUpdatePreference, persistent updatePersistentState) {
	if preferences == nil || state == nil {
		panic("dacode: update TUI stores are required")
	}
	model.updateProfile = profile
	model.autoUpdate = newAutoUpdateController(preferences, preference)
	model.updateStateStore = state
	model.updateStateWrites = newUpdateStateWriteController(state)
	model.updateState = persistent
}

func (model *tuiModel) startUpdateCommand() tea.Cmd {
	if model.updateProfile == nil {
		model.updateModal = newUpdateModal()
		model.updateModal.phase = updateModalFailed
		model.updateModal.err = "Interactive updates require an explicit launch-time update profile."
		return nil
	}
	model.updateModal = newUpdateModal()
	return model.beginUpdateOperation(updateTUICheck, false)
}

func (model *tuiModel) beginUpdateOperation(operation updateTUIOperation, startup bool) tea.Cmd {
	if model.updateProfile == nil {
		return nil
	}
	if model.updateCancel != nil {
		model.updateCancel()
	}
	model.updateGeneration++
	if model.updateGeneration == 0 {
		model.updateGeneration = 1
	}
	generation := model.updateGeneration
	operationContext, cancel := context.WithTimeout(model.ctx, updateTUIOperationTimeout)
	model.updateCancel = cancel
	profile := model.updateProfile
	return func() tea.Msg {
		var result daupdate.Result
		var err error
		if operation == updateTUICheck {
			result, err = profile.service.Check(operationContext, profile.current)
		} else {
			target := profile.target
			if target == "" {
				target, err = os.Executable()
				if err == nil {
					target, err = filepath.Abs(target)
				}
			}
			if err == nil && profile.platform() == "windows" && sameExecutablePath(target) {
				err = daupdate.ErrApplyFailed
			} else if err == nil {
				result, err = profile.service.Apply(operationContext, profile.current, target, daupdate.AuthorizationGranted)
			}
		}
		cancel()
		return updateTUIResultMsg{generation: generation, operation: operation, startup: startup, result: result, err: err}
	}
}

func (model *tuiModel) handleUpdateResult(message updateTUIResultMsg) tea.Cmd {
	if message.generation != model.updateGeneration {
		if message.operation == updateTUIApply && message.err == nil && message.result.Applied {
			persist := model.persistAppliedUpdate(message.result)
			if model.updateModal == nil {
				model.updateModal = newUpdateModal()
				model.updateModal.finishApply(message.result, nil)
				return persist
			}
			return tea.Batch(persist, model.notify("A previously started update finished; restart to use the installed release.", toastWarning, ""))
		}
		return nil
	}
	model.updateCancel = nil
	if message.operation == updateTUICheck && message.startup {
		return model.handleStartupUpdateCheck(message.result, message.err)
	}
	if model.updateModal == nil {
		if message.operation == updateTUIApply && message.err == nil && message.result.Applied {
			model.updateModal = newUpdateModal()
			model.updateModal.finishApply(message.result, nil)
			return model.persistAppliedUpdate(message.result)
		}
		if message.operation == updateTUIApply && message.err != nil {
			version := message.result.LatestVersion
			if version == "" {
				version = model.updateState.NotifiedVersion
			}
			return tea.Batch(model.persistUpdateCooldown(version), model.notify("Automatic update failed; this version will not be retried until the cooldown expires.", toastError, ""))
		}
		return nil
	}
	if message.operation == updateTUICheck {
		model.updateModal.finishCheck(message.result, message.err)
		return nil
	}
	if errors.Is(message.err, context.DeadlineExceeded) {
		model.updateModal.phase = updateModalFailed
		model.updateModal.err = "Another update is already running or the update lock timed out."
	} else {
		model.updateModal.finishApply(message.result, message.err)
	}
	if message.err == nil && message.result.Applied {
		return model.persistAppliedUpdate(message.result)
	}
	if message.result.LatestVersion != "" {
		return model.persistUpdateCooldown(message.result.LatestVersion)
	}
	return nil
}

func (model *tuiModel) handleUpdateModalKey(key string) tea.Cmd {
	if model.updateModal == nil {
		return nil
	}
	phase := model.updateModal.phase
	action := model.updateModal.handleKey(key)
	switch action {
	case updateModalCancel:
		if model.updateCancel != nil {
			model.updateCancel()
			model.updateCancel = nil
		}
		model.updateModal = nil
		if phase == updateModalApplying || phase == updateModalChecking {
			// Keep the generation so a completed activation can still report
			// success; all other late completions are ignored while closed.
			return nil
		}
		return model.drainInputQueue()
	case updateModalApply:
		return model.beginUpdateOperation(updateTUIApply, false)
	case updateModalRetry:
		return model.beginUpdateOperation(updateTUICheck, false)
	default:
		return nil
	}
}

func (model *tuiModel) toggleAutoUpdate() tea.Cmd {
	if model.autoUpdate == nil {
		return model.notify("Automatic update preferences are unavailable.", toastError, "")
	}
	model.autoUpdate.mu.Lock()
	preference := model.autoUpdate.preference
	model.autoUpdate.mu.Unlock()
	if preference.Source == "environment" {
		return model.notify("Automatic updates are controlled by DEEPAGENTS_CODE_AUTO_UPDATE for this launch.", toastWarning, "")
	}
	enabled := !preference.Enabled
	generation := model.autoUpdate.begin(enabled)
	controller := model.autoUpdate
	return func() tea.Msg {
		stale, err := controller.save(generation, enabled)
		return autoUpdateSavedMsg{generation: generation, enabled: enabled, stale: stale, err: err}
	}
}

func (model *tuiModel) handleAutoUpdateSaved(message autoUpdateSavedMsg) tea.Cmd {
	if model.autoUpdate == nil || message.stale || !model.autoUpdate.current(message.generation) {
		return nil
	}
	if message.err != nil {
		return model.notify("Automatic update preference could not be saved; automatic updates remain disabled.", toastError, "")
	}
	if !model.autoUpdate.apply(message.generation, message.enabled) {
		return nil
	}
	notice := "Automatic updates disabled."
	if message.enabled {
		notice = "Automatic updates enabled."
		if model.updateProfile == nil {
			notice += " No update profile is configured for this launch."
		}
	}
	return tea.Batch(model.persistAutoUpdateConsent(message.enabled), model.notify(notice, toastInfo, ""))
}

func (model *tuiModel) persistAutoUpdateConsent(enabled bool) tea.Cmd {
	if model.updateStateStore == nil {
		return nil
	}
	generation := model.nextUpdateStateGeneration()
	return func() tea.Msg {
		return model.runUpdateStateWrite(generation, "", "", func(state *updatePersistentState) error {
			state.AutoUpdateConsent = enabled
			return nil
		})
	}
}

func (model *tuiModel) startStartupUpdate() tea.Cmd {
	if model.updateProfile == nil || model.autoUpdate == nil {
		return nil
	}
	model.autoUpdate.mu.Lock()
	preference := model.autoUpdate.preference
	model.autoUpdate.mu.Unlock()
	if !preference.Enabled {
		return nil
	}
	return model.beginUpdateOperation(updateTUICheck, true)
}

func (model *tuiModel) handleStartupUpdateCheck(result daupdate.Result, err error) tea.Cmd {
	if err != nil || result.Status != daupdate.UpdateAvailable || result.LatestVersion == "" {
		return nil
	}
	now := time.Now().UTC()
	state := model.updateState
	if state.SkipVersion == result.LatestVersion || state.CooldownVersion == result.LatestVersion && state.CooldownUntil.After(now) {
		return nil
	}
	if state.RestartVersion == result.LatestVersion && state.RestartAttempts > 0 {
		return model.notify("An update was installed; restart this process to use "+result.LatestVersion+".", toastWarning, "")
	}
	model.autoUpdate.mu.Lock()
	preference := model.autoUpdate.preference
	model.autoUpdate.mu.Unlock()
	consented := preference.Explicit || state.AutoUpdateConsent
	if state.SkipOnceVersion == result.LatestVersion {
		consented = false
	}
	if preference.Source == "default" && !state.ImplicitDefaultAcknowledged {
		consented = false
	}
	if consented {
		if model.updateProfile.platform() == "windows" && model.updateProfile.target == "" {
			return tea.Batch(model.addUpdateNotification(result), model.persistUpdateNotice(result.LatestVersion, true))
		}
		return model.beginUpdateOperation(updateTUIApply, true)
	}
	return tea.Batch(model.addUpdateNotification(result), model.persistUpdateNotice(result.LatestVersion, preference.Source == "default"))
}

func (model *tuiModel) addUpdateNotification(result daupdate.Result) tea.Cmd {
	key := "update:" + result.LatestVersion
	if !validNotificationKey(key) {
		return nil
	}
	notification := newPendingNotification(
		key, "Update "+result.LatestVersion+" available",
		"A signed update is available. Installation still requires explicit confirmation.",
		updateAvailableNotification{Latest: result.LatestVersion, UpgradeArgs: []string{"update"}},
		notificationAction{ID: notificationInstall, Label: "Install", Primary: true},
		notificationAction{ID: notificationSkipOnce, Label: "Remind me later"},
		notificationAction{ID: notificationSkipVersion, Label: "Skip this version"},
		notificationAction{ID: notificationChangelog, Label: "Open changelog"},
	)
	model.notifications.add(notification)
	model.notificationCenter = newNotificationCenter([]pendingNotification{notification})
	model.notificationCenter.detail = true
	model.notificationCenter.action = primaryNotificationAction(notification.Actions)
	return model.notify("Update "+result.LatestVersion+" is available.", toastWarning, key)
}

func (model *tuiModel) persistUpdateNotice(version string, implicit bool) tea.Cmd {
	if model.updateStateStore == nil {
		return nil
	}
	generation := model.nextUpdateStateGeneration()
	return func() tea.Msg {
		return model.runUpdateStateWrite(generation, "update notice", "", func(state *updatePersistentState) error {
			state.NotifiedVersion = version
			state.NotifiedAt = time.Now().UTC()
			if state.SkipOnceVersion == version {
				state.SkipOnceVersion = ""
			}
			if implicit {
				state.ImplicitDefaultAcknowledged = true
				state.AutoUpdateConsent = true
			}
			return nil
		})
	}
}

func (model *tuiModel) persistAppliedUpdate(result daupdate.Result) tea.Cmd {
	if model.updateStateStore == nil || result.LatestVersion == "" {
		return nil
	}
	generation := model.nextUpdateStateGeneration()
	return func() tea.Msg {
		return model.runUpdateStateWrite(generation, "update installed", "", func(state *updatePersistentState) error {
			state.RestartVersion = result.LatestVersion
			state.RestartAttempts = min(state.RestartAttempts+1, 8)
			state.RestartedAt = time.Now().UTC()
			state.CooldownVersion = ""
			state.CooldownUntil = time.Time{}
			return nil
		})
	}
}

func (model *tuiModel) persistUpdateCooldown(version string) tea.Cmd {
	if model.updateStateStore == nil || strings.TrimSpace(version) == "" {
		return nil
	}
	generation := model.nextUpdateStateGeneration()
	return func() tea.Msg {
		return model.runUpdateStateWrite(generation, "update cooldown", "", func(state *updatePersistentState) error {
			state.CooldownVersion = version
			state.CooldownUntil = time.Now().UTC().Add(updateFailureCooldown)
			return nil
		})
	}
}

func (model *tuiModel) persistUpdateChoice(key string, skipVersion bool) tea.Cmd {
	notification, ok := model.notifications.get(key)
	if !ok {
		return nil
	}
	payload, ok := notification.Payload.(updateAvailableNotification)
	if !ok || model.updateStateStore == nil {
		return nil
	}
	generation := model.nextUpdateStateGeneration()
	return func() tea.Msg {
		notice := "reminder saved"
		if skipVersion {
			notice = "version skipped"
		}
		return model.runUpdateStateWrite(generation, notice, key, func(state *updatePersistentState) error {
			state.AutoUpdateConsent = false
			if skipVersion {
				state.SkipVersion = payload.Latest
			} else {
				state.SkipOnceVersion = payload.Latest
			}
			return nil
		})
	}
}

func (model *tuiModel) nextUpdateStateGeneration() uint64 {
	if model.updateStateWrites == nil {
		panic("dacode: update state write controller is required")
	}
	model.updateStateGeneration = model.updateStateWrites.begin()
	return model.updateStateGeneration
}

func (model *tuiModel) runUpdateStateWrite(generation uint64, notice, notificationKey string, mutate func(*updatePersistentState) error) tea.Msg {
	state, stale, err := model.updateStateWrites.update(model.ctx, generation, mutate)
	if stale {
		return updateStateSavedMsg{}
	}
	return updateStateSavedMsg{generation: generation, notice: notice, state: state, notificationKey: notificationKey, err: err}
}

func (model *tuiModel) handleUpdateStateSaved(message updateStateSavedMsg) tea.Cmd {
	if message.generation != model.updateStateGeneration {
		return nil
	}
	if message.err != nil {
		return model.notify("Update preference state could not be saved; the pending notification was retained.", toastError, "")
	}
	model.updateState = message.state
	if message.notificationKey != "" {
		model.notifications.remove(message.notificationKey)
		if model.notificationCenter != nil && !model.notificationCenter.reload(model.notifications.list()) {
			model.notificationCenter = nil
		}
	}
	switch message.notice {
	case "reminder saved":
		return model.notify("Update reminder deferred until the next launch.", toastInfo, "")
	case "version skipped":
		return model.notify("This update version will be skipped.", toastInfo, "")
	}
	return nil
}
