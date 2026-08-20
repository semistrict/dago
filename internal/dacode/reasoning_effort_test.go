package dacode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
)

func TestReasoningEffortManagerUsesProfileLevelsDefaultAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), reasoningEffortFilename)
	profile := damodel.Profile{
		Provider: "openai", Model: "reasoner", SupportsReasoning: true,
		ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningLevel: "medium",
	}
	manager := newReasoningEffortManager(profile, path)
	context := manager.Context()
	if context.ModelSpec != "openai:reasoner" || context.Current != "" || context.Default != "medium" {
		t.Fatalf("initial context = %#v", context)
	}
	if err := manager.Set("HIGH"); err != nil {
		t.Fatal(err)
	}
	if got := manager.Context().Current; got != "high" {
		t.Fatalf("current = %q", got)
	}
	restored := newReasoningEffortManager(profile, path)
	if got := restored.Context().Current; got != "high" {
		t.Fatalf("restored current = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preference mode = %o", info.Mode().Perm())
	}
	if err := restored.Set(""); err != nil {
		t.Fatal(err)
	}
	if got := newReasoningEffortManager(profile, path).Context().Current; got != "" {
		t.Fatalf("cleared current = %q", got)
	}
}

func TestReasoningEffortManagerRejectsUnsupportedWithoutChangingCurrent(t *testing.T) {
	manager := newReasoningEffortManager(damodel.Profile{
		Provider: "openai", Model: "reasoner", ReasoningLevels: []string{"low", "high"},
	}, filepath.Join(t.TempDir(), reasoningEffortFilename))
	if err := manager.Set("low"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Set("maximum"); err == nil {
		t.Fatal("unsupported effort was accepted")
	}
	if got := manager.Context().Current; got != "low" {
		t.Fatalf("current changed to %q", got)
	}
}

func TestReasoningEffortManagerTreatsNullPreferencesAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), reasoningEffortFilename)
	if err := os.WriteFile(path, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := newReasoningEffortManager(damodel.Profile{
		Provider: "openai", Model: "reasoner", ReasoningLevels: []string{"medium"},
	}, path)
	if err := manager.Set("medium"); err != nil {
		t.Fatal(err)
	}
	if got := manager.Context().Current; got != "medium" {
		t.Fatalf("current = %q", got)
	}
}

func TestReasoningEffortMiddlewareOverridesEffortAndPreservesSummary(t *testing.T) {
	manager := newReasoningEffortManager(damodel.Profile{
		Provider: "openai", Model: "reasoner", ReasoningLevels: []string{"high"},
	}, filepath.Join(t.TempDir(), reasoningEffortFilename))
	if err := manager.Set("high"); err != nil {
		t.Fatal(err)
	}
	request := dagent.ModelRequest{Reasoning: &damodel.Reasoning{Effort: "low", Summary: "auto"}}
	var received *damodel.Reasoning
	_, err := manager.Middleware().WrapModelCall(t.Context(), request, func(_ context.Context, request dagent.ModelRequest) (dagent.ModelResponse, error) {
		received = request.Reasoning
		return dagent.ModelResponse{Update: dastate.Values{}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if received == nil || received.Effort != "high" || received.Summary != "auto" {
		t.Fatalf("reasoning = %#v", received)
	}
	if request.Reasoning.Effort != "low" {
		t.Fatalf("middleware mutated caller request: %#v", request.Reasoning)
	}
}

func TestEffortPickerUsesCurrentThenDefaultAndAppliesSelection(t *testing.T) {
	runner := &fakeRunner{profile: damodel.Profile{
		Provider: "openai", Model: "reasoner", ContextWindow: 128_000, SupportsReasoning: true,
		ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningLevel: "medium",
	}}
	model := newTUIModel(t.Context(), runner, "/work", "reasoner", "thread", false, false, "")
	model.resize(100, 30)
	if command, handled := model.slashCommand("/effort"); !handled || command != nil || model.effortPicker == nil {
		t.Fatalf("handled = %t, command = %v, picker = %#v", handled, command, model.effortPicker)
	}
	if model.effortPicker.selected != 1 {
		t.Fatalf("selected = %d, want default index 1", model.effortPicker.selected)
	}
	plain := ansi.Strip(model.View())
	for _, expected := range []string{"Select Reasoning Effort", "openai:reasoner", "medium (default)", "Enter select", "Esc cancel"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("picker missing %q:\n%s", expected, plain)
		}
	}
	command, handled := model.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if !handled || command != nil || model.effortPicker.selected != 2 {
		t.Fatalf("Tab handled = %t, command = %v, selected = %d", handled, command, model.effortPicker.selected)
	}
	command, handled = model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !handled || command == nil || model.effortPicker != nil {
		t.Fatalf("Enter handled = %t, command nil = %t, picker = %#v", handled, command == nil, model.effortPicker)
	}
	model.Update(command())
	if runner.effort != "high" || !strings.Contains(model.items[len(model.items)-1].text, "set to high") {
		t.Fatalf("effort = %q, items = %#v", runner.effort, model.items)
	}
	if status := ansi.Strip(model.renderStatus()); !strings.Contains(status, "openai:reasoner high") {
		t.Fatalf("status = %q", status)
	}
}

func TestEffortCommandSupportsCaseInsensitiveLevelClearAndErrors(t *testing.T) {
	runner := &fakeRunner{profile: damodel.Profile{
		Provider: "openai", Model: "reasoner", ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningLevel: "medium",
	}}
	model := newTUIModel(t.Context(), runner, "/work", "reasoner", "thread", false, false, "")
	command, handled := model.slashCommand("/effort HIGH")
	if !handled || command == nil {
		t.Fatalf("set handled = %t, command = %v", handled, command)
	}
	model.Update(command())
	if runner.effort != "high" {
		t.Fatalf("effort = %q", runner.effort)
	}

	command, handled = model.slashCommand("/effort clear")
	if !handled || command == nil {
		t.Fatalf("clear handled = %t, command = %v", handled, command)
	}
	model.Update(command())
	if runner.effort != "" || !strings.Contains(model.items[len(model.items)-1].text, "override cleared") {
		t.Fatalf("effort = %q, items = %#v", runner.effort, model.items)
	}

	if command, handled = model.slashCommand("/effort impossible"); !handled || command != nil {
		t.Fatalf("invalid handled = %t, command = %v", handled, command)
	}
	last := model.items[len(model.items)-1]
	if last.kind != itemError || !strings.Contains(last.text, "Supported efforts: low, medium, high") {
		t.Fatalf("invalid item = %#v", last)
	}
}

func TestEffortCommandExplainsUnsupportedModelAndPickerCancellation(t *testing.T) {
	unsupported := newTUIModel(t.Context(), &fakeRunner{}, "/work", "main-model", "thread", false, false, "")
	if command, handled := unsupported.slashCommand("/effort"); !handled || command != nil {
		t.Fatalf("unsupported handled = %t, command = %v", handled, command)
	}
	if got := unsupported.items[len(unsupported.items)-1].text; got != "Reasoning effort is not configurable for openai:main-model." {
		t.Fatalf("unsupported notice = %q", got)
	}

	runner := &fakeRunner{profile: damodel.Profile{
		Provider: "openai", Model: "reasoner", ReasoningLevels: []string{"low", "high"},
	}}
	model := newTUIModel(t.Context(), runner, "/work", "reasoner", "thread", false, false, "")
	model.slashCommand("/effort")
	if command, handled := model.handleKey(tea.KeyPressMsg{Code: tea.KeyEsc}); !handled || command != nil || model.effortPicker != nil || runner.effort != "" {
		t.Fatalf("cancel handled = %t, command = %v, picker = %#v, effort = %q", handled, command, model.effortPicker, runner.effort)
	}
}
