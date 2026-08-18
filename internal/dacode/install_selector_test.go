package dacode

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dainstall"
)

func TestInstallSelectorUsesClosedCatalogAndSearch(t *testing.T) {
	state := newInstallSelector()
	if len(state.entries) == 0 {
		t.Fatal("install catalog is empty")
	}
	for _, entry := range state.entries {
		if !entry.BuiltIn || entry.Kind != dainstall.Extra {
			t.Fatalf("unexpected install entry: %#v", entry)
		}
	}
	state.setQuery("ollama")
	entry, ok := state.selectedEntry()
	if !ok || entry.Name != "ollama" {
		t.Fatalf("filtered entry = %#v, %v", entry, ok)
	}
	result := state.handleKey("enter", 8)
	if result.Action != installSelectorAlreadyAvailable || result.Entry.Name != "ollama" {
		t.Fatalf("included result = %#v", result)
	}
}

func TestInstallSelectorRequiresConfirmationForExternalCode(t *testing.T) {
	state := newInstallSelectorWithEntries([]installSelectorEntry{{Name: "safe-extra", Kind: dainstall.Extra, Description: "optional", BuiltIn: false}})
	result := state.handleKey("enter", 5)
	if result.Action != installSelectorConfirm || state.confirmation == nil {
		t.Fatalf("confirmation = %#v, %v", result, state.confirmation)
	}
	result = state.handleKey("n", 5)
	if result.Action != installSelectorNoAction || state.confirmation != nil {
		t.Fatalf("cancel confirmation = %#v, %v", result, state.confirmation)
	}
	state.handleKey("enter", 5)
	result = state.handleKey("y", 5)
	if result.Action != installSelectorInstall || result.Entry.Name != "safe-extra" || state.confirmation != nil {
		t.Fatalf("install result = %#v, %v", result, state.confirmation)
	}
}

func TestInstallSelectorSanitizesAndRendersASCII(t *testing.T) {
	state := newInstallSelectorWithEntries([]installSelectorEntry{
		{Name: "unsafe\x1b", Kind: dainstall.Extra},
		{Name: "safe", Kind: dainstall.Extra, Description: "description", BuiltIn: true},
		{Name: "safe", Kind: dainstall.Extra, Description: "duplicate"},
	})
	if len(state.entries) != 1 || state.entries[0].Name != "safe" {
		t.Fatalf("entries = %#v", state.entries)
	}
	view := ansi.Strip(renderInstallSelector(state, 80, 24, asciiUIGlyphs))
	for _, wanted := range []string{"Install optional integration", "safe", "included", "^/v navigate"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
		t.Fatalf("ASCII install selector contains Unicode UI glyphs:\n%s", view)
	}
}

func TestInstallSelectorNavigationAndCancel(t *testing.T) {
	state := newInstallSelectorWithEntries([]installSelectorEntry{
		{Name: "a", Kind: dainstall.Extra, BuiltIn: true}, {Name: "b", Kind: dainstall.Extra, BuiltIn: true},
	})
	state.handleKey("up", 5)
	entry, _ := state.selectedEntry()
	if entry.Name != "b" {
		t.Fatalf("wrapped entry = %#v", entry)
	}
	if result := state.handleKey("esc", 5); result.Action != installSelectorCancel {
		t.Fatalf("cancel = %#v", result)
	}
}

func TestInstallSelectorParsesForceSeparatelyAndOnlyBypassesConfirmation(t *testing.T) {
	arguments, err := parseInstallSelectorArguments("external-helper --force")
	if err != nil || arguments.Query != "external-helper" || !arguments.Force {
		t.Fatalf("arguments = %#v, %v", arguments, err)
	}
	state := newInstallSelectorWithEntries([]installSelectorEntry{
		{Name: "external-helper", Kind: dainstall.Package, Description: "test fixture"},
	})
	state.applyArguments(arguments)
	result := state.handleKey("enter", 5)
	if result.Action != installSelectorInstall || result.Request.Name != "external-helper" || !result.Request.Force || state.confirmation != nil {
		t.Fatalf("forced result = %#v confirmation=%#v", result, state.confirmation)
	}
	if _, allowed := state.entryForRequest(result.Request); !allowed {
		t.Fatal("exact forced request was not allowlisted")
	}
	forged := result.Request
	forged.Name = "other-helper"
	if _, allowed := state.entryForRequest(forged); allowed {
		t.Fatal("force authorized a non-catalog name")
	}
}

func TestInstallSelectorArgumentParsingAndStartupRecoveryClassification(t *testing.T) {
	for _, value := range []string{"", "external-helper", "external-helper --force", "--force external-helper"} {
		if !installStartupRecoveryBypassCapable(value) {
			t.Errorf("valid recovery arguments rejected: %q", value)
		}
	}
	for _, value := range []string{"one two", "--yes external-helper", "--force=true", "external-helper\nunsafe"} {
		if installStartupRecoveryBypassCapable(value) {
			t.Errorf("invalid recovery arguments accepted: %q", value)
		}
	}
	arguments, err := parseInstallSelectorArguments("unknown-name --force")
	if err != nil {
		t.Fatal(err)
	}
	state := newInstallSelectorWithEntries([]installSelectorEntry{{Name: "external-helper", Kind: dainstall.Package}})
	state.applyArguments(arguments)
	if len(state.visible) != 0 || state.handleKey("enter", 5).Action != installSelectorNoAction {
		t.Fatalf("unknown force target escaped allowlist: visible=%#v", state.visible)
	}
}

type deterministicInstallController struct {
	mu      sync.Mutex
	entries map[dainstall.Kind][]dainstall.Entry
	result  dainstall.Result
	err     error
	calls   []installControllerCall
}

type installControllerCall struct {
	Kind          dainstall.Kind
	Name          string
	Authorization dainstall.Authorization
}

func newDeterministicInstallController() *deterministicInstallController {
	return &deterministicInstallController{
		entries: map[dainstall.Kind][]dainstall.Entry{
			dainstall.Package: {{Name: "external-helper", Kind: dainstall.Package, Description: "deterministic test fixture"}},
		},
		result: dainstall.Result{Name: "external-helper", Kind: dainstall.Package, Status: dainstall.Installed},
	}
}

func (controller *deterministicInstallController) Available(kind dainstall.Kind) []dainstall.Entry {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return slices.Clone(controller.entries[kind])
}

func (controller *deterministicInstallController) Install(_ context.Context, kind dainstall.Kind, name string, authorization dainstall.Authorization) (dainstall.Result, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if authorization != dainstall.AuthorizationGranted {
		return dainstall.Result{}, dainstall.ErrAuthorization
	}
	allowed := false
	for _, entry := range controller.entries[kind] {
		if entry.Name == name {
			allowed = true
			break
		}
	}
	if !allowed {
		return dainstall.Result{}, dainstall.ErrUnknownDependency
	}
	controller.calls = append(controller.calls, installControllerCall{Kind: kind, Name: name, Authorization: authorization})
	return controller.result, controller.err
}

func (controller *deterministicInstallController) callSnapshot() []installControllerCall {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return slices.Clone(controller.calls)
}

func TestInstallSelectorControllerRunsOffUILoopAndReturnsRestartAction(t *testing.T) {
	controller := newDeterministicInstallController()
	model := &tuiModel{ctx: context.Background(), installController: controller, height: 24}
	model.openInstallSelector("external-helper --force")
	if len(controller.callSnapshot()) != 0 {
		t.Fatal("opening selector executed installer")
	}
	command := model.handleInstallSelectorKey("enter")
	if command == nil || len(controller.callSnapshot()) != 0 {
		t.Fatalf("controller ran on UI loop: command=%v calls=%#v", command != nil, controller.callSnapshot())
	}
	message := command().(installCompletedMsg)
	if message.err != nil || message.action != installCompletionRestartRequired || message.result.Status != dainstall.Installed {
		t.Fatalf("completion = %#v", message)
	}
	want := []installControllerCall{{Kind: dainstall.Package, Name: "external-helper", Authorization: dainstall.AuthorizationGranted}}
	if calls := controller.callSnapshot(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestInstallSelectorCancellationMakesZeroControllerCalls(t *testing.T) {
	controller := newDeterministicInstallController()
	model := &tuiModel{ctx: context.Background(), installController: controller, height: 24}
	model.openInstallSelector("external-helper")
	if command := model.handleInstallSelectorKey("enter"); command != nil || model.installSelector.confirmation == nil {
		t.Fatalf("confirmation command=%v state=%#v", command != nil, model.installSelector.confirmation)
	}
	if command := model.handleInstallSelectorKey("esc"); command != nil || model.installSelector.confirmation != nil {
		t.Fatalf("confirmation cancel command=%v state=%#v", command != nil, model.installSelector.confirmation)
	}
	if command := model.handleInstallSelectorKey("esc"); command != nil || model.installSelector != nil {
		t.Fatalf("selector cancel command=%v selector=%#v", command != nil, model.installSelector)
	}
	if calls := controller.callSnapshot(); len(calls) != 0 {
		t.Fatalf("canceled selector calls = %#v", calls)
	}
}

func TestInstallCompletionBoundsSanitizesAndPreservesFailureIdentity(t *testing.T) {
	controller := newDeterministicInstallController()
	cause := errors.New("failed\x1b[31m\n" + strings.Repeat("secret", 100))
	controller.err = cause
	request := installRequest{SelectorID: 1, Generation: 1, Kind: dainstall.Package, Name: "external-helper"}
	message := executeInstallRequest(context.Background(), controller, request)().(installCompletedMsg)
	if message.err == nil || !errors.Is(message.err, cause) || message.action != installCompletionNoAction {
		t.Fatalf("failure completion = %#v", message)
	}
	if text := message.err.Error(); len([]rune(text)) > 320 || strings.ContainsAny(text, "\x1b\n\r") {
		t.Fatalf("unsafe failure = %q", text)
	}
}

func TestDeterministicExternalControllerRejectsNamesOutsideExactCatalog(t *testing.T) {
	controller := newDeterministicInstallController()
	request := installRequest{SelectorID: 1, Generation: 1, Kind: dainstall.Package, Name: "other-helper", Force: true}
	message := executeInstallRequest(context.Background(), controller, request)().(installCompletedMsg)
	if !errors.Is(message.err, dainstall.ErrUnknownDependency) || len(controller.callSnapshot()) != 0 {
		t.Fatalf("forged completion=%#v calls=%#v", message, controller.callSnapshot())
	}
}
