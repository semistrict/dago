package dacode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/datool"
)

func TestApprovalModeStoreHashesThreadIDsAndKeepsModesIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	store := newApprovalModeStore(path)
	if err := store.Save("thread-one", approvalYOLO); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("thread-two", approvalAuto); err != nil {
		t.Fatal(err)
	}
	for threadID, want := range map[string]approvalMode{"thread-one": approvalYOLO, "thread-two": approvalAuto, "missing": approvalAuto} {
		got, err := store.Load(threadID)
		if err != nil || got != want {
			t.Errorf("Load(%q) = %v, %v; want %v", threadID, got, err, want)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "thread-one") || strings.Contains(string(data), "thread-two") {
		t.Fatalf("raw thread ID leaked into preferences: %s", data)
	}
	if key := approvalModeKey("thread-one"); len(key) != 64 || !strings.Contains(string(data), key) {
		t.Fatalf("hashed key %q was not persisted: %s", key, data)
	}
}

func TestApprovalModeStoreFailsClosedOnCorruptOrInvalidRecords(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "corrupt JSON", data: "not-json"},
		{name: "invalid container", data: `{"thread_approval_modes":[]}`},
		{name: "invalid record", data: `{"thread_approval_modes":{"KEY":{"mode":"unsafe"}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
			data := strings.ReplaceAll(test.data, "KEY", approvalModeKey("thread"))
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			mode, err := newApprovalModeStore(path).Load("thread")
			if err == nil || mode != approvalManual {
				t.Fatalf("Load = %v, %v; want Manual error", mode, err)
			}
		})
	}
}

func TestApprovalModeStoreConcurrentWritesPreserveThreads(t *testing.T) {
	store := newApprovalModeStore(filepath.Join(t.TempDir(), approvalPreferencesFilename))
	var group sync.WaitGroup
	for index := range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := store.Save(fmt.Sprintf("thread-%d", index), approvalAuto); err != nil {
				t.Errorf("Save: %v", err)
			}
		}()
	}
	group.Wait()
	for index := range 20 {
		mode, err := store.Load(fmt.Sprintf("thread-%d", index))
		if err != nil || mode != approvalAuto {
			t.Fatalf("thread %d = %v, %v", index, mode, err)
		}
	}
}

func TestApprovalModeStoreRejectsStaleOverlappingGeneration(t *testing.T) {
	store := newApprovalModeStore(filepath.Join(t.TempDir(), "approval.json"))
	store.registerGeneration("thread-1", 1)
	store.registerGeneration("thread-1", 2)
	if err := store.saveGeneration("thread-1", approvalManual, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.saveGeneration("thread-1", approvalAuto, 1); err != nil {
		t.Fatal(err)
	}
	mode, err := store.Load("thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if mode != approvalManual {
		t.Fatalf("mode = %v, want Manual after stale Auto save", mode)
	}
}

func TestApprovalRulesReadLiveThreadModeAndFailClosed(t *testing.T) {
	store := newApprovalModeStore(filepath.Join(t.TempDir(), approvalPreferencesFilename))
	rules := approvalRulesForThreadModes([]dagent.ApprovalRule{{Pattern: "execute"}}, store)
	request := dagent.ToolCallRequest{Runtime: dagent.Runtime{Config: dacheckpoint.Config{ThreadID: "thread"}}}
	request.Call.Name = "execute"
	if applies, err := rules[0].Applies(request); err != nil || !applies {
		t.Fatalf("missing mode applies = %t, %v", applies, err)
	}
	if err := store.Save("thread", approvalYOLO); err != nil {
		t.Fatal(err)
	}
	if applies, err := rules[0].Applies(request); err != nil || applies {
		t.Fatalf("YOLO applies = %t, %v", applies, err)
	}
	if err := store.Save("thread", approvalAuto); err != nil {
		t.Fatal(err)
	}
	if applies, err := rules[0].Applies(request); err != nil || !applies {
		t.Fatalf("Auto applies = %t, %v", applies, err)
	}
	request.Runtime.Config.ThreadID = "missing"
	if applies, err := rules[0].Applies(request); err != nil || !applies {
		t.Fatalf("missing thread applies = %t, %v", applies, err)
	}
}

func TestApprovalModeIsEnforcedInsideDelegatedSubagents(t *testing.T) {
	for _, test := range []struct {
		mode          approvalMode
		wantInterrupt bool
	}{
		{mode: approvalManual, wantInterrupt: true},
		{mode: approvalAuto, wantInterrupt: true},
		{mode: approvalYOLO, wantInterrupt: false},
	} {
		t.Run(test.mode.String(), func(t *testing.T) {
			store := newApprovalModeStore(filepath.Join(t.TempDir(), approvalPreferencesFilename))
			if err := store.Save("delegated-thread", test.mode); err != nil {
				t.Fatal(err)
			}
			var executions atomic.Int32
			danger := datool.MustNew("danger", "Perform a gated action.", func(context.Context, struct{}) (string, error) {
				executions.Add(1)
				return "executed", nil
			})
			child := modeltest.New(damodel.Profile{ToolCalling: true},
				modeltest.Step{Response: damodel.Response{Message: damessage.Message{
					Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{ID: "child-danger", Name: "danger", Arguments: json.RawMessage(`{}`)}},
				}}},
				modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("child done")}},
			)
			parent := modeltest.New(damodel.Profile{ToolCalling: true},
				modeltest.Step{Response: damodel.Response{Message: damessage.Message{
					Role: damessage.RoleAssistant, ToolCalls: []damessage.ToolCall{{
						ID: "delegate", Name: "task", Arguments: json.RawMessage(`{"description":"run danger","subagent_type":"operator"}`),
					}},
				}}},
				modeltest.Step{Response: damodel.Response{Message: damessage.Assistant("parent done")}},
			)
			rules := approvalRulesForThreadModes([]dagent.ApprovalRule{{Pattern: "danger"}}, store)
			agent := dago.NewAgent(
				parent,
				dago.WithTools(danger),
				dago.WithSaver(dacheckpoint.NewMemorySaver()),
				dago.WithoutSummary(),
				dago.WithApprovalRules(rules...),
				dago.WithSubagents(dago.NewSubagent(
					"operator", "Runs delegated actions.", child,
					dago.WithSystemMessage(damessage.System("Run the delegated action.")),
				)),
			)
			result, err := agent.Invoke(t.Context(), dagent.Input{
				Config: dacheckpoint.Config{ThreadID: "delegated-thread"}, Messages: []damessage.Message{damessage.Human("delegate")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(result.Interrupts) > 0; got != test.wantInterrupt {
				t.Fatalf("interrupt = %t, want %t; result = %#v", got, test.wantInterrupt, result)
			}
			wantExecutions := int32(1)
			if test.wantInterrupt {
				wantExecutions = 0
			}
			if executions.Load() != wantExecutions {
				t.Fatalf("executions = %d, want %d", executions.Load(), wantExecutions)
			}
		})
	}
}

func TestTUILiveApprovalModePersistsAndRestoresActiveThread(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	if err := saveAutoModeNotice(path); err != nil {
		t.Fatal(err)
	}
	if err := saveYoloAcknowledgement(path); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread", false, false, "")
	if err := model.configureApprovalState(path, "review-model", false); err != nil {
		t.Fatal(err)
	}
	command := model.setApprovalMode(approvalAuto)
	if command == nil || model.approvalMode != approvalManual {
		t.Fatalf("before save: command=%v mode=%v", command, model.approvalMode)
	}
	model.Update(command())
	if model.approvalMode != approvalAuto {
		t.Fatalf("after save mode = %v", model.approvalMode)
	}

	restarted := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread", false, false, "")
	if err := restarted.configureApprovalState(path, "review-model", true); err != nil {
		t.Fatal(err)
	}
	if restarted.approvalMode != approvalAuto {
		t.Fatalf("restored mode = %v", restarted.approvalMode)
	}

	stale := restarted.setApprovalMode(approvalYOLO)
	restarted.threadID = "other-thread"
	if err := restarted.startNewApprovalThread(restarted.threadID); err != nil {
		t.Fatal(err)
	}
	restarted.Update(stale())
	if restarted.approvalMode != approvalAuto {
		t.Fatalf("stale save changed active thread mode = %v", restarted.approvalMode)
	}
}

func TestManualPersistenceFailureBlocksNewRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	if err := saveAutoModeNotice(path); err != nil {
		t.Fatal(err)
	}
	if err := saveYoloAcknowledgement(path); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread", false, false, "")
	if err := model.configureApprovalState(path, "review-model", false); err != nil {
		t.Fatal(err)
	}
	runAutoNoticeCommand(model, model.setApprovalMode(approvalYOLO))
	if model.approvalMode != approvalYOLO {
		t.Fatalf("mode = %v", model.approvalMode)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	runAutoNoticeCommand(model, model.setApprovalMode(approvalManual))
	if model.approvalMode != approvalManual || !model.approvalModeBlocked {
		t.Fatalf("mode=%v blocked=%t", model.approvalMode, model.approvalModeBlocked)
	}
	if command := model.submitPrompt("must not run"); command != nil || len(runner.inputs) != 0 {
		t.Fatalf("command=%v inputs=%d", command, len(runner.inputs))
	}
	if len(model.items) == 0 || !strings.Contains(model.items[len(model.items)-1].text, "blocked") {
		t.Fatalf("items = %#v", model.items)
	}
}

func TestAutoModeNoticePreferencesAreVersionedPrivateAndMergeSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", approvalPreferencesFilename)
	if shown, err := hasAutoModeNotice(path); err != nil || shown {
		t.Fatalf("missing notice = %t, %v", shown, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"policy_version":"keep-me","acknowledged":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveAutoModeNotice(path); err != nil {
		t.Fatal(err)
	}
	shown, err := hasAutoModeNotice(path)
	if err != nil || !shown {
		t.Fatalf("saved notice = %t, %v", shown, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var preferences map[string]any
	if err := json.Unmarshal(data, &preferences); err != nil {
		t.Fatal(err)
	}
	if preferences["policy_version"] != "keep-me" || preferences["acknowledged"] != true {
		t.Fatalf("unrelated preferences were lost: %#v", preferences)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}

	preferences["auto_notice_version"] = "outdated"
	data, err = json.Marshal(preferences)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if shown, err := hasAutoModeNotice(path); err != nil || shown {
		t.Fatalf("outdated notice = %t, %v", shown, err)
	}
}

func TestYoloAcknowledgementIsPolicyVersionedAndPreservesAutoNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	if err := saveAutoModeNotice(path); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := hasYoloAcknowledgement(path); err != nil || acknowledged {
		t.Fatalf("missing acknowledgement = %t, %v", acknowledged, err)
	}
	if err := saveYoloAcknowledgement(path); err != nil {
		t.Fatal(err)
	}
	acknowledged, err := hasYoloAcknowledgement(path)
	if err != nil || !acknowledged {
		t.Fatalf("saved acknowledgement = %t, %v", acknowledged, err)
	}
	if shown, err := hasAutoModeNotice(path); err != nil || !shown {
		t.Fatalf("Auto notice was lost = %t, %v", shown, err)
	}
	preferences, err := loadApprovalPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	preferences["policy_version"] = "outdated"
	if err := saveApprovalPreferences(path, preferences); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := hasYoloAcknowledgement(path); err != nil || acknowledged {
		t.Fatalf("outdated acknowledgement = %t, %v", acknowledged, err)
	}
}

func TestAutoModeStartsInteractiveWithoutNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	if err := model.configureApprovalNotices(path, "review-model"); err != nil {
		t.Fatal(err)
	}
	model.resize(90, 28)
	plain := ansi.Strip(model.View())
	if model.approvalMode != approvalAuto || model.autoModeNotice {
		t.Fatalf("mode=%v notice=%t", model.approvalMode, model.autoModeNotice)
	}
	for _, removed := range []string{"Auto mode", "Enter to keep Auto", "Esc for Manual"} {
		if strings.Contains(plain, removed) {
			t.Fatalf("removed notice text %q is still visible:\n%s", removed, plain)
		}
	}
	if _, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("build now")}); !handled {
		t.Fatal("startup typing was not handled")
	}
	if got := model.composer.Value(); got != "build now" {
		t.Fatalf("composer value = %q", got)
	}
}

func TestApprovalPreferencesMalformedStateFailsClosedWithoutNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	if err := model.configureApprovalNotices(path, "review-model"); err == nil {
		t.Fatal("malformed approval preferences were accepted")
	}
	if model.approvalMode != approvalManual || model.autoModeNotice || !model.autoModeNoticeAcknowledged {
		t.Fatalf("mode=%v notice=%t acknowledged=%t", model.approvalMode, model.autoModeNotice, model.autoModeNoticeAcknowledged)
	}
	restored := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, true, "")
	restored.approvalModeStore = newApprovalModeStore(path)
	if err := restored.restoreApprovalMode("thread-1"); err == nil {
		t.Fatal("malformed restored approval mode was accepted")
	}
	if restored.approvalMode != approvalManual || restored.autoModeNotice {
		t.Fatalf("restored mode=%v notice=%t", restored.approvalMode, restored.autoModeNotice)
	}
}

func TestAutoModeStartsSessionLoadWithoutDecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	runner := &fakeRunner{}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "saved", false, true, "")
	model.sessionPicker = &sessionPickerState{sessions: []sessionInfo{{ThreadID: "saved"}}, resuming: true, startup: true}
	if err := model.configureApprovalNotices(path, "review-model"); err != nil {
		t.Fatal(err)
	}
	command := model.initialSessionCommand()
	if command == nil || model.approvalNoticeDeferred {
		t.Fatalf("command=%v deferred=%t", command, model.approvalNoticeDeferred)
	}
}

func TestYoloNoticeGatesModeUntilPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	if err := model.configureApprovalNotices(path, "review-model"); err != nil {
		t.Fatal(err)
	}
	model.resize(90, 28)
	if command := model.setApprovalMode(approvalYOLO); command != nil || !model.yoloModeNotice || model.approvalMode != approvalManual {
		t.Fatalf("initial gate: command=%v notice=%t mode=%v", command, model.yoloModeNotice, model.approvalMode)
	}
	plain := ansi.Strip(model.View())
	for _, expected := range []string{"YOLO mode", "without asking you first", "Enter to enable YOLO", "m for Manual", "Esc to keep current mode"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("notice missing %q:\n%s", expected, plain)
		}
	}
	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled || command == nil || model.yoloModeNotice || model.approvalMode != approvalManual {
		t.Fatalf("Esc: handled=%t command=%v notice=%t mode=%v", handled, command, model.yoloModeNotice, model.approvalMode)
	}
	runAutoNoticeCommand(model, command)
	if acknowledged, err := hasYoloAcknowledgement(path); err != nil || acknowledged {
		t.Fatalf("Esc persisted acknowledgement = %t, %v", acknowledged, err)
	}

	model.setApprovalMode(approvalYOLO)
	command, handled = model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if !handled || command == nil || model.yoloModeNotice || model.approvalMode != approvalManual {
		t.Fatalf("m: handled=%t command=%v notice=%t mode=%v", handled, command, model.yoloModeNotice, model.approvalMode)
	}
	runAutoNoticeCommand(model, command)
	model.setApprovalMode(approvalYOLO)
	command, handled = model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil || !model.yoloModeNoticeSaving || model.approvalMode != approvalManual {
		t.Fatalf("Enter before save: handled=%t command=%v saving=%t mode=%v", handled, command, model.yoloModeNoticeSaving, model.approvalMode)
	}
	runAutoNoticeCommand(model, command)
	if model.yoloModeNotice || model.yoloModeNoticeSaving || model.approvalMode != approvalYOLO {
		t.Fatalf("after save: notice=%t saving=%t mode=%v", model.yoloModeNotice, model.yoloModeNoticeSaving, model.approvalMode)
	}
	if acknowledged, err := hasYoloAcknowledgement(path); err != nil || !acknowledged {
		t.Fatalf("Enter persisted acknowledgement = %t, %v", acknowledged, err)
	}
	runAutoNoticeCommand(model, model.setApprovalMode(approvalManual))
	runAutoNoticeCommand(model, model.setApprovalMode(approvalYOLO))
	if model.yoloModeNotice || model.approvalMode != approvalYOLO {
		t.Fatal("acknowledged YOLO was gated again")
	}
}

func TestYoloNoticeSaveFailureStaysManual(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, approvalPreferencesFilename)
	model := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread-1", false, false, "")
	if err := model.configureApprovalNotices(path, "review-model"); err == nil {
		t.Fatal("invalid approval path was accepted")
	}
	model.setApprovalMode(approvalYOLO)
	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatalf("handled=%t command=%v", handled, command)
	}
	runAutoNoticeCommand(model, command)
	if model.approvalMode != approvalManual || model.yoloModeAcknowledged || model.yoloModeNotice {
		t.Fatalf("mode=%v acknowledged=%t notice=%t", model.approvalMode, model.yoloModeAcknowledged, model.yoloModeNotice)
	}
	if len(model.items) == 0 || !strings.Contains(model.items[len(model.items)-1].text, "staying in the current approval mode") {
		t.Fatalf("items = %#v", model.items)
	}
}

func TestStartupYoloDefersInitialPromptAndEscUsesManual(t *testing.T) {
	path := filepath.Join(t.TempDir(), approvalPreferencesFilename)
	runner := &fakeRunner{streams: []eventStream{&fakeEventStream{}}}
	model := newTUIModel(t.Context(), runner, "/work", "main-model", "thread-1", true, false, "initial work")
	if err := model.configureApprovalNotices(path, "review-model"); err != nil {
		t.Fatal(err)
	}
	model.Init()
	if !model.yoloModeNotice || model.approvalMode != approvalManual || len(runner.inputs) != 0 || !model.approvalNoticeDeferred {
		t.Fatalf("notice=%t mode=%v inputs=%d deferred=%t", model.yoloModeNotice, model.approvalMode, len(runner.inputs), model.approvalNoticeDeferred)
	}
	command, handled := model.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled || command == nil {
		t.Fatalf("handled=%t command=%v", handled, command)
	}
	runAutoNoticeCommand(model, command)
	if model.approvalMode != approvalManual || len(runner.inputs) != 1 || runner.inputs[0].Messages[0].TextContent() != "initial work" {
		t.Fatalf("mode=%v inputs=%#v", model.approvalMode, runner.inputs)
	}
}

func runAutoNoticeCommand(model *tuiModel, command tea.Cmd) {
	if command == nil {
		return
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, nested := range batch {
			runAutoNoticeCommand(model, nested)
		}
		return
	}
	_, next := model.Update(message)
	runAutoNoticeCommand(model, next)
}
