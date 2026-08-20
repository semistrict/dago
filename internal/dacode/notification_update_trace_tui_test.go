package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/daupdate"
)

func configuredUpdateTestModel(t *testing.T, service updateService, preference autoUpdatePreference, state updatePersistentState) *tuiModel {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, directory, "model", "thread", false, false, "")
	model.configureUpdates(
		&tuiUpdateProfile{service: service, current: "v1", target: filepath.Join(directory, "replacement")},
		newAutoUpdatePreferenceStore(filepath.Join(directory, "config.toml")),
		newUpdateStateStore(filepath.Join(directory, "update-state.json")),
		preference,
		state,
	)
	return model
}

func TestTUIUpdateRequiresTwoConfirmationsAndAppliesOnce(t *testing.T) {
	result := daupdate.Result{Status: daupdate.UpdateAvailable, CurrentVersion: "v1", LatestVersion: "v2", Channel: "stable", Applied: true}
	service := &fakeUpdateService{result: result}
	model := configuredUpdateTestModel(t, service, autoUpdatePreference{Enabled: false, Explicit: true, Source: "config"}, newUpdatePersistentState())

	check := model.startUpdateCommand()
	model.handleUpdateResult(check().(updateTUIResultMsg))
	if service.checks != 1 || service.applies != 0 || model.updateModal.phase != updateModalAvailable {
		t.Fatalf("after check: service=%#v modal=%#v", service, model.updateModal)
	}
	if command := model.handleUpdateModalKey("enter"); command != nil || model.updateModal.phase != updateModalConfirm {
		t.Fatalf("first confirmation started work: command=%v phase=%d", command != nil, model.updateModal.phase)
	}
	if command := model.handleUpdateModalKey("n"); command != nil || model.updateModal.phase != updateModalAvailable {
		t.Fatalf("backout = command %v phase %d", command != nil, model.updateModal.phase)
	}
	model.handleUpdateModalKey("enter")
	apply := model.handleUpdateModalKey("enter")
	if apply == nil || service.applies != 0 {
		t.Fatal("second confirmation did not stage exactly one apply")
	}
	next := model.handleUpdateResult(apply().(updateTUIResultMsg))
	saved := next().(updateStateSavedMsg)
	if saved.err != nil {
		t.Fatal(saved.err)
	}
	model.handleUpdateStateSaved(saved)
	if service.applies != 1 || model.updateModal == nil || model.updateModal.phase != updateModalComplete {
		t.Fatalf("apply result: service=%#v modal=%#v", service, model.updateModal)
	}
}

func TestTUIUpdateCancelIgnoresLateCheckButReportsCompletedApply(t *testing.T) {
	result := daupdate.Result{Status: daupdate.UpdateAvailable, CurrentVersion: "v1", LatestVersion: "v2", Applied: true}
	service := &fakeUpdateService{result: result}
	model := configuredUpdateTestModel(t, service, autoUpdatePreference{}, newUpdatePersistentState())
	check := model.startUpdateCommand()
	model.handleUpdateModalKey("esc")
	model.handleUpdateResult(check().(updateTUIResultMsg))
	if model.updateModal != nil {
		t.Fatalf("late check reopened modal: %#v", model.updateModal)
	}

	model.updateModal = newUpdateModal()
	model.updateModal.finishCheck(result, nil)
	model.handleUpdateModalKey("enter")
	apply := model.handleUpdateModalKey("enter")
	model.handleUpdateModalKey("ctrl+c")
	next := model.handleUpdateResult(apply().(updateTUIResultMsg))
	runTUITestCommand(model, next)
	if model.updateModal == nil || model.updateModal.phase != updateModalComplete || service.applies != 1 {
		t.Fatalf("completed activation was lost: service=%#v modal=%#v", service, model.updateModal)
	}
}

func TestTUICompletedStaleApplyDoesNotOverwriteNewerModal(t *testing.T) {
	result := daupdate.Result{Status: daupdate.UpdateAvailable, CurrentVersion: "v1", LatestVersion: "v2", Applied: true}
	model := configuredUpdateTestModel(t, &fakeUpdateService{result: result}, autoUpdatePreference{}, newUpdatePersistentState())
	model.updateGeneration = 2
	model.updateModal = newUpdateModal()
	command := model.handleUpdateResult(updateTUIResultMsg{generation: 1, operation: updateTUIApply, result: result})
	if command == nil || model.updateModal.phase != updateModalChecking {
		t.Fatalf("stale success overwrote newer modal: %#v", model.updateModal)
	}
	if len(model.toasts.items) == 0 || !strings.Contains(model.toasts.items[len(model.toasts.items)-1].Text, "previously started update finished") {
		t.Fatalf("stale completed activation was not surfaced: %#v", model.toasts.items)
	}
}

func TestTUIUpdateFailureIsRedactedRetryableAndPersistsCooldown(t *testing.T) {
	result := daupdate.Result{Status: daupdate.UpdateAvailable, LatestVersion: "v2"}
	service := &fakeUpdateService{result: result, err: errors.New("secret=do-not-render")}
	model := configuredUpdateTestModel(t, service, autoUpdatePreference{}, newUpdatePersistentState())
	model.updateModal = newUpdateModal()
	model.updateModal.finishCheck(result, nil)
	model.handleUpdateModalKey("enter")
	apply := model.handleUpdateModalKey("enter")
	next := model.handleUpdateResult(apply().(updateTUIResultMsg))
	saved := next().(updateStateSavedMsg)
	if saved.err != nil {
		t.Fatal(saved.err)
	}
	model.handleUpdateStateSaved(saved)
	if model.updateModal.err == "" || strings.Contains(model.updateModal.err, "do-not-render") {
		t.Fatalf("unsafe failure = %q", model.updateModal.err)
	}
	if model.updateState.CooldownVersion != "v2" || !model.updateState.CooldownUntil.After(time.Now()) {
		t.Fatalf("cooldown = %#v", model.updateState)
	}
	service.err = nil
	retry := model.handleUpdateModalKey("r")
	model.handleUpdateResult(retry().(updateTUIResultMsg))
	if model.updateModal.phase != updateModalAvailable || service.checks != 1 {
		t.Fatalf("retry = phase %d checks %d", model.updateModal.phase, service.checks)
	}
}

func TestTUIStartupUpdateNoticesThenAppliesAndHonorsFallbacks(t *testing.T) {
	result := daupdate.Result{Status: daupdate.UpdateAvailable, LatestVersion: "v2", Applied: true}
	service := &fakeUpdateService{result: result}
	model := configuredUpdateTestModel(t, service, autoUpdatePreference{Enabled: true, Source: "default"}, newUpdatePersistentState())
	if command := model.handleStartupUpdateCheck(result, nil); command == nil {
		t.Fatal("first launch did not create notice work")
	}
	saved := model.persistUpdateNotice("v2", true)().(updateStateSavedMsg)
	if saved.err != nil {
		t.Fatal(saved.err)
	}
	model.handleUpdateStateSaved(saved)
	if service.applies != 0 || len(model.notifications.list()) != 1 || !model.updateState.ImplicitDefaultAcknowledged || !model.updateState.AutoUpdateConsent {
		t.Fatalf("first launch = applies %d notifications %#v state %#v", service.applies, model.notifications.list(), model.updateState)
	}
	apply := model.handleStartupUpdateCheck(result, nil)
	if apply == nil {
		t.Fatal("acknowledged launch did not stage apply")
	}
	runTUITestCommand(model, apply)
	if service.applies != 1 || model.updateState.RestartVersion != "v2" || model.updateState.RestartAttempts != 1 {
		t.Fatalf("second launch = applies %d state %#v", service.applies, model.updateState)
	}

	model.updateState = updatePersistentState{Version: updateStateVersion, CooldownVersion: "v2", CooldownUntil: time.Now().Add(time.Hour)}
	if command := model.handleStartupUpdateCheck(result, nil); command != nil {
		t.Fatal("cooldown still scheduled update work")
	}
	model.updateState = updatePersistentState{Version: updateStateVersion, SkipVersion: "v2"}
	if command := model.handleStartupUpdateCheck(result, nil); command != nil {
		t.Fatal("version skip still scheduled update work")
	}
}

func TestTUIStartupUpdateWindowsRunningTargetFallsBackToManualNotice(t *testing.T) {
	result := daupdate.Result{Status: daupdate.UpdateAvailable, LatestVersion: "v2"}
	service := &fakeUpdateService{result: result}
	model := configuredUpdateTestModel(t, service, autoUpdatePreference{Enabled: true, Explicit: true, Source: "config"}, updatePersistentState{Version: updateStateVersion, AutoUpdateConsent: true})
	model.updateProfile.target = ""
	model.updateProfile.operatingSystem = "windows"
	if command := model.handleStartupUpdateCheck(result, nil); command == nil {
		t.Fatal("Windows fallback did not stage a manual notification")
	}
	if service.applies != 0 || len(model.notifications.list()) != 1 || model.notificationCenter == nil {
		t.Fatalf("Windows fallback = applies %d notifications %d center %#v", service.applies, len(model.notifications.list()), model.notificationCenter)
	}
}

func TestTUIUpdateWithoutLaunchProfileFailsBeforeNetwork(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, t.TempDir(), "model", "thread", false, false, "")
	model.configureUpdates(nil, newAutoUpdatePreferenceStore(filepath.Join(t.TempDir(), "config.toml")), newUpdateStateStore(filepath.Join(t.TempDir(), "state.json")), autoUpdatePreference{}, newUpdatePersistentState())
	if command := model.startUpdateCommand(); command != nil || model.updateModal == nil || model.updateModal.phase != updateModalFailed {
		t.Fatalf("unconfigured update = command %v modal %#v", command != nil, model.updateModal)
	}
	if !strings.Contains(model.updateModal.err, "explicit launch-time update profile") {
		t.Fatalf("unconfigured error = %q", model.updateModal.err)
	}
}

func TestTUIAutoUpdateSaveFailureKeepsDisabledAndGenerationWins(t *testing.T) {
	directory := t.TempDir()
	badPath := filepath.Join(directory, "directory")
	model := newTUIModel(t.Context(), &fakeRunner{}, directory, "model", "thread", false, false, "")
	model.configureUpdates(nil, newAutoUpdatePreferenceStore(badPath), newUpdateStateStore(filepath.Join(directory, "state.json")), autoUpdatePreference{Enabled: false, Explicit: true, Source: "config"}, newUpdatePersistentState())
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	command := model.toggleAutoUpdate()
	message := command().(autoUpdateSavedMsg)
	model.handleAutoUpdateSaved(message)
	if model.autoUpdate.preference.Enabled || len(model.toasts.items) == 0 || !strings.Contains(model.toasts.items[len(model.toasts.items)-1].Text, "remain disabled") {
		t.Fatalf("failed save mutated preference or omitted warning: %#v %#v", model.autoUpdate.preference, model.toasts.items)
	}
	old := autoUpdateSavedMsg{generation: message.generation, enabled: true}
	model.autoUpdate.begin(false)
	model.handleAutoUpdateSaved(old)
	if model.autoUpdate.preference.Enabled {
		t.Fatal("stale auto-update result won")
	}
}

func TestTUIUpdateStateNewestIntentWinsBeforeDiskMutation(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store := newUpdateStateStore(filepath.Join(directory, "state.json"))
	controller := newUpdateStateWriteController(store)
	older := controller.begin()
	newer := controller.begin()
	if _, stale, err := controller.update(t.Context(), older, func(state *updatePersistentState) error {
		state.SkipVersion = "v-old"
		return nil
	}); err != nil || !stale {
		t.Fatalf("older write = stale %t err %v", stale, err)
	}
	if _, stale, err := controller.update(t.Context(), newer, func(state *updatePersistentState) error {
		state.SkipVersion = "v-new"
		return nil
	}); err != nil || stale {
		t.Fatalf("newer write = stale %t err %v", stale, err)
	}
	state, err := store.Load(t.Context())
	if err != nil || state.SkipVersion != "v-new" {
		t.Fatalf("stored state = %#v, %v", state, err)
	}
}

func TestTUITraceDefersTranscriptDuringTurnButOpensImmediately(t *testing.T) {
	opened := ""
	resolver := traceResolverFunc(func(context.Context, string, string) (string, error) {
		return "https://smith.example/projects/p/r/thread", nil
	})
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.browserLinks = false
	model.openURL = func(value string) error { opened = value; return nil }
	model.configureTrace(newTraceCommand(resolver), "project")
	model.running = true
	message := model.startTraceCommand()().(traceResolvedMsg)
	open := model.handleTraceResolved(message)
	if open == nil || open().(traceBrowserOpenedMsg).failed || opened == "" || len(model.items) != 0 || len(model.deferredTrace) != 1 {
		t.Fatalf("busy trace = opened %q items %d deferred %d", opened, len(model.items), len(model.deferredTrace))
	}
	model.running = false
	model.flushDeferredTrace()
	if len(model.items) != 2 || model.items[0].text != "/trace" || strings.Count(model.items[1].text, "trace will be empty") != 1 {
		t.Fatalf("flushed trace = %#v", model.items)
	}
}

func TestTUIModalToastsStayBoundedAndASCII(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.glyphs = cloneUIGlyphs(asciiUIGlyphs)
	model.resize(52, 18)
	model.notificationSettings = newNotificationSettings(nil)
	for _, text := range []string{"one", "two", "three"} {
		model.toasts.add(text, toastWarning, maximumToastDuration, "", time.Now())
	}
	view := model.View()
	if height := lipgloss.Height(view); height != 18 {
		t.Fatalf("height = %d, want exact terminal height\n%s", height, view)
	}
	for index, line := range strings.Split(view, "\n") {
		if width := lipgloss.Width(line); width > 52 {
			t.Fatalf("line %d width = %d\n%s", index, width, view)
		}
	}
	plain := ansi.Strip(view)
	for _, expected := range []string{"Notification Settings", "What would you like to build?", "openai:model"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("view missing %q\n%s", expected, view)
		}
	}
	if lines := strings.Split(view, "\n"); !strings.Contains(lines[len(lines)-1], "Ready") {
		t.Fatalf("status is not on final physical row: %q", lines[len(lines)-1])
	}
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
		t.Fatalf("ASCII view has Unicode UI glyphs:\n%s", view)
	}
}

func TestTUIDraftDoesNotRenderMouseActionRow(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(80, 24)
	model.composer.SetValue("clear button draft")
	model.relayout()
	view := model.View()
	if strings.Contains(view, "[ X ] clear") || strings.Contains(view, "[ COPY ] copy") {
		t.Fatalf("removed draft actions are still rendered:\n%s", view)
	}
}

func TestTUIBrowserLayoutKeepsStatusOnPhysicalBottomRow(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.browserLinks = true
	model.resize(80, 24)
	if height := lipgloss.Height(model.View()); height != 24 {
		t.Fatalf("browser frame height = %d, want exact 24-row frame", height)
	}
	model.composer.SetValue("clear")
	model.toasts.add("actionable", toastWarning, maximumToastDuration, "", time.Now())
	model.relayout()
	if height := lipgloss.Height(model.View()); height != 24 {
		t.Fatalf("browser frame with toast height = %d, want exact 24-row frame (viewport=%d composer=%d toast=%d)\n%s", height, model.viewport.Height(), model.composer.Height(), model.toastHeight, model.View())
	}
	if strings.Contains(model.View(), "[ X ] clear") || strings.Contains(model.View(), "[ COPY ] copy") {
		t.Fatal("browser layout restored removed draft actions")
	}
	model.toasts.expire(time.Now().Add(maximumToastDuration + time.Second))
	model.relayout()
	if height := lipgloss.Height(model.View()); height != 24 {
		t.Fatalf("browser frame after toast height = %d, want exact 24-row frame", height)
	}
}

func TestTUIToastExpiryCannotChangeFrameBetweenLayoutAndRender(t *testing.T) {
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "model", "thread", false, false, "")
	model.resize(80, 24)
	now := time.Now()
	model.toasts.add("expiring", toastWarning, maximumToastDuration, "", now)
	model.toasts.add("remaining", toastWarning, maximumToastDuration, "", now)
	model.relayout()
	if len(model.toasts.items) != 2 {
		t.Fatalf("toast count = %d, want 2", len(model.toasts.items))
	}
	model.toasts.items[0].ExpiresAt = now.Add(-time.Second)
	view := model.View()
	if height := lipgloss.Height(view); height != 24 {
		t.Fatalf("frame changed after a toast crossed its deadline between layout and render: height=%d\n%s", height, view)
	}
	if !strings.Contains(view, "expiring") || !strings.Contains(view, "remaining") {
		t.Fatalf("layout snapshot was not rendered atomically:\n%s", view)
	}
	model.Update(toastExpiryMsg(time.Now()))
	view = model.View()
	if height := lipgloss.Height(view); height != 24 {
		t.Fatalf("frame after scheduled expiry = %d, want 24\n%s", height, view)
	}
	if strings.Contains(view, "expiring") || !strings.Contains(view, "remaining") {
		t.Fatalf("scheduled expiry did not replace the layout snapshot:\n%s", view)
	}
}

func runTUITestCommand(model *tuiModel, command tea.Cmd) {
	if command == nil {
		return
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, nested := range batch {
			runTUITestCommand(model, nested)
		}
		return
	}
	if _, tick := message.(toastExpiryMsg); tick {
		return
	}
	_, next := model.Update(message)
	runTUITestCommand(model, next)
}
