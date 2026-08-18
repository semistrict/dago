package dacode

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestModelSelectorCatalogKeepsRecentFirstAndDeduplicates(t *testing.T) {
	entries := modelSelectorCatalog([]string{
		"openrouter:z-ai/glm-5.2",
		"custom_provider:org/model",
		"openrouter:z-ai/glm-5.2",
		" padded:model",
	})
	if len(entries) < 3 || entries[0].Spec != "openrouter:z-ai/glm-5.2" || !entries[0].Recent || entries[1].Spec != "custom_provider:org/model" || !entries[1].Recent {
		t.Fatalf("recent entries = %+v", entries[:min(len(entries), 3)])
	}
	count := 0
	for _, entry := range entries {
		if entry.Spec == "openrouter:z-ai/glm-5.2" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("recommended recent model occurred %d times", count)
	}
	providers := sortedModelSelectorProviders(entries)
	if !slices.IsSorted(providers) || !slices.Contains(providers, "custom_provider") {
		t.Fatalf("providers = %v", providers)
	}
}

func TestModelSelectorKeyHandlingAndRendering(t *testing.T) {
	state := newModelSelector([]modelSelectorEntry{
		{Spec: "first:model-a", Label: "Model A", Recent: true, Recommended: true},
		{Spec: "second:model-b", Label: "Model B", Recommended: true},
	}, "second:model-b", "first:model-a")
	state.handleKey("down", 5)
	state.handleKey("ctrl+n", 5)
	view := ansi.Strip(renderModelSelector(state, 80, 24, unicodeUIGlyphs))
	for _, wanted := range []string{"Select Model", "Recent", "first:model-a", "default", "current", "Ctrl+N names"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
	state.setQuery("")
	state.handleKey("c", 5)
	state.handleKey("u", 5)
	state.handleKey("s", 5)
	state.handleKey("t", 5)
	state.handleKey("o", 5)
	state.handleKey("m", 5)
	state.handleKey("_", 5)
	state.handleKey("p", 5)
	state.handleKey("r", 5)
	state.handleKey("o", 5)
	state.handleKey("v", 5)
	state.handleKey("i", 5)
	state.handleKey("d", 5)
	state.handleKey("e", 5)
	state.handleKey("r", 5)
	state.handleKey(":", 5)
	state.handleKey("m", 5)
	state.handleKey("o", 5)
	state.handleKey("d", 5)
	state.handleKey("e", 5)
	state.handleKey("l", 5)
	result := state.handleKey("enter", 5)
	if result.Err != nil || result.Action != modelSelectorSelect || result.Spec != "custom_provider:model" {
		t.Fatalf("typed result = %#v", result)
	}
	result = state.handleKey("esc", 5)
	if result.Action != modelSelectorCancel {
		t.Fatalf("cancel result = %#v", result)
	}
}

func TestModelSelectorRendersASCIIUI(t *testing.T) {
	state := newModelSelector([]modelSelectorEntry{{Spec: "openai:gpt-test", Label: "GPT Test", Recommended: true}}, "", "")
	view := ansi.Strip(renderModelSelector(state, 80, 24, asciiUIGlyphs))
	for _, wanted := range []string{"Select Model", "Showing recommended models - Ctrl+R for all", "^/v navigate", "> GPT Test"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎▌›↑↓—") {
		t.Fatalf("ASCII model selector contains Unicode UI glyphs:\n%s", view)
	}
}

func TestModelSelectorSearchesAllAndPreservesSelection(t *testing.T) {
	state := newModelSelector([]modelSelectorEntry{
		{Spec: "provider:recommended", Label: "Recommended", Recommended: true},
		{Spec: "provider:hidden-model", Label: "Hidden Friendly"},
		{Spec: "other:second", Label: "Second", Recommended: true},
	}, "other:second", "provider:recommended")
	if entry, ok := state.selectedEntry(); !ok || entry.Spec != "other:second" {
		t.Fatalf("initial selection = %+v, %v", entry, ok)
	}
	state.setQuery("hidden friendly")
	if state.info() != "Searching all models" || len(state.visible) != 1 {
		t.Fatalf("filtered = %v, info = %q", state.visible, state.info())
	}
	if spec, err := state.selection(); err != nil || spec != "provider:hidden-model" {
		t.Fatalf("selection = %q, %v", spec, err)
	}
	state.setQuery("")
	if len(state.visible) != 2 {
		t.Fatalf("recommended visible = %v", state.visible)
	}
	state.toggleRecommended()
	if len(state.visible) != 3 || state.info() != "Showing all models — Ctrl+R for recommended" {
		t.Fatalf("all visible = %v, info = %q", state.visible, state.info())
	}
}

func TestModelSelectorNavigationAutocompleteNamesAndDefault(t *testing.T) {
	state := newModelSelector([]modelSelectorEntry{
		{Spec: "first:model-a", Label: "Model A", Recommended: true},
		{Spec: "second:model-b", Label: "Model B", Recommended: true},
	}, "", "first:model-a")
	state.move(-1)
	entry, ok := state.selectedEntry()
	if !ok || entry.Spec != "second:model-b" {
		t.Fatalf("wrapped selection = %+v, %v", entry, ok)
	}
	if got := state.autocomplete(); got != "second:model-b" || state.query != got {
		t.Fatalf("autocomplete = %q, query = %q", got, state.query)
	}
	state.toggleNames()
	if state.label(entry) != entry.Spec {
		t.Fatalf("raw label = %q", state.label(entry))
	}
	state.toggleNames()
	if state.label(entry) != "Model B" {
		t.Fatalf("friendly label = %q", state.label(entry))
	}
	result := state.handleKey("ctrl+s", 5)
	if result.Err != nil || result.Action != modelSelectorSetDefault || result.Spec != "second:model-b" || state.defaultSpec != "first:model-a" {
		t.Fatalf("set-default request = %#v; stored %q", result, state.defaultSpec)
	}
	write, accepted := state.beginPreferenceWrite(result.Request)
	if !accepted || state.defaultSpec != "second:model-b" || !state.finishPreferenceWrite(write, nil) {
		t.Fatalf("set-default write = %#v accepted=%v stored=%q", write, accepted, state.defaultSpec)
	}
	result = state.handleKey("ctrl+s", 5)
	write, accepted = state.beginPreferenceWrite(result.Request)
	if result.Action != modelSelectorClearDefault || !accepted || state.defaultSpec != "" || !state.finishPreferenceWrite(write, nil) {
		t.Fatalf("clear-default write result=%#v write=%#v accepted=%v stored=%q", result, write, accepted, state.defaultSpec)
	}
}

func TestModelSelectorAcceptsTypedSpecAndRejectsUnsafeInput(t *testing.T) {
	state := newModelSelector(nil, "", "")
	state.setQuery("custom_provider:org/new-model")
	if spec, err := state.selection(); err != nil || spec != "custom_provider:org/new-model" {
		t.Fatalf("typed selection = %q, %v", spec, err)
	}
	for _, value := range []string{"Provider:model", "provider:", ":model", "provider: padded", "provider:model\nunsafe"} {
		state.setQuery(value)
		if spec, err := state.selection(); err == nil || spec != "" {
			t.Fatalf("unsafe %q selection = %q, %v", value, spec, err)
		}
	}
}

func TestModelSelectorBoundsAndSanitizesEntries(t *testing.T) {
	entries := make([]modelSelectorEntry, maxModelSelectorEntries+10)
	for index := range entries {
		entries[index] = modelSelectorEntry{Spec: "provider:model-" + string(rune('a'+index%26)), Label: "unsafe\x1b[31m", Recommended: true}
	}
	state := newModelSelector(entries, "", "")
	if len(state.entries) > maxModelSelectorEntries || len(state.entries) != 26 {
		t.Fatalf("entry count = %d", len(state.entries))
	}
	for _, entry := range state.entries {
		if entry.Label == "unsafe\x1b[31m" || entry.Label == "" {
			t.Fatalf("unsafe label survived: %q", entry.Label)
		}
	}
	state.setQuery(string(make([]byte, 600)))
	if len([]rune(state.query)) > 512 {
		t.Fatalf("query length = %d", len([]rune(state.query)))
	}
}

func TestModelSelectorBoundsRecentInputBeforeBuildingCatalog(t *testing.T) {
	recent := make([]string, 100_000)
	for index := range recent {
		recent[index] = "provider:model-" + string(rune('a'+index%26))
	}
	entries := modelSelectorCatalog(recent)
	recentCount := 0
	for _, entry := range entries {
		if entry.Recent {
			recentCount++
		}
		if len([]rune(entry.Label)) > 160 {
			t.Fatalf("unbounded label length = %d", len([]rune(entry.Label)))
		}
	}
	if recentCount > maxRecentModelEntries {
		t.Fatalf("recent entries = %d", recentCount)
	}
}

func TestModelSelectorProviderAvailabilityAndDeterministicGrouping(t *testing.T) {
	state := newModelSelectorWithOptions([]modelSelectorEntry{
		{Spec: "zeta:model-z", Label: "Zeta", Recommended: true},
		{Spec: "alpha:model-b", Label: "Alpha B", Recommended: true},
		{Spec: "recent:model", Label: "Recent", Recent: true},
		{Spec: "alpha:model-a", Label: "Alpha A", Recommended: true},
	}, "", "", modelSelectorOptions{ProviderAvailability: map[string]modelProviderAvailability{
		"zeta":  {Install: modelRequirementMissing, Credentials: modelRequirementMissing},
		"alpha": {Install: modelRequirementNotRequired, Credentials: modelRequirementReady},
	}})
	got := make([]string, len(state.visible))
	for index, entryIndex := range state.visible {
		got[index] = state.entries[entryIndex].Spec
	}
	want := []string{"recent:model", "alpha:model-a", "alpha:model-b", "zeta:model-z"}
	if !slices.Equal(got, want) {
		t.Fatalf("grouped order = %#v, want %#v", got, want)
	}
	view := ansi.Strip(renderModelSelector(state, 80, 24, asciiUIGlyphs))
	for _, expected := range []string{"Recent", "alpha", "zeta", "[available]", "[install required; credentials required]"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q:\n%s", expected, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 80 {
			t.Fatalf("line width = %d: %q", ansi.StringWidth(line), line)
		}
	}
	if got := renderModelSelector(state, 20, 24, asciiUIGlyphs); got != "" {
		t.Fatalf("unsafe narrow render = %q", got)
	}
	if got := renderModelSelector(state, 80, 11, asciiUIGlyphs); got != "" {
		t.Fatalf("unsafe short render = %q", got)
	}
}

func TestModelSelectorPreferenceWriteRollbackAndStaleRejection(t *testing.T) {
	state := newModelSelector([]modelSelectorEntry{
		{Spec: "provider:old", Recommended: true},
		{Spec: "provider:new", Recommended: true},
	}, "provider:new", "provider:old")
	state.selectSpec("provider:new")
	result := state.handleKey("ctrl+s", 5)
	if state.defaultSpec != "provider:old" {
		t.Fatalf("request mutated default: %q", state.defaultSpec)
	}
	deferred, ok := result.deferredAction()
	if !ok || deferred.Request != result.Request || deferred.Request.PriorDefault != "provider:old" {
		t.Fatalf("deferred action = %#v, %v", deferred, ok)
	}
	write, ok := state.beginPreferenceWrite(result.Request)
	if !ok || state.defaultSpec != "provider:new" {
		t.Fatalf("begin write = %#v, %v default=%q", write, ok, state.defaultSpec)
	}
	stale := write
	stale.Generation++
	if state.finishPreferenceWrite(stale, errors.New("stale")) {
		t.Fatal("stale write completion was accepted")
	}
	if state.defaultSpec != "provider:new" || state.pendingWrite == nil {
		t.Fatalf("stale completion changed state: default=%q pending=%#v", state.defaultSpec, state.pendingWrite)
	}
	if !state.finishPreferenceWrite(write, errors.New("disk full")) || state.defaultSpec != "provider:old" || state.pendingWrite != nil {
		t.Fatalf("failed write did not roll back: default=%q pending=%#v", state.defaultSpec, state.pendingWrite)
	}
	if state.finishPreferenceWrite(write, nil) {
		t.Fatal("duplicate completion was accepted")
	}

	result = state.handleKey("ctrl+s", 5)
	write, ok = state.beginPreferenceWrite(result.Request)
	state.replaceDefault("provider:old")
	if !ok || state.finishPreferenceWrite(write, nil) || state.defaultSpec != "provider:old" {
		t.Fatalf("replacement did not invalidate write: accepted=%v default=%q", ok, state.defaultSpec)
	}
}

func TestModelSelectorDeferredCustomSelectionDoesNotMutateLiveState(t *testing.T) {
	state := newModelSelector(nil, "provider:live", "provider:default")
	state.setQuery("custom_provider:org/arbitrary-model")
	result := state.handleKey("enter", 5)
	action, ok := result.deferredAction()
	if !ok || action.Request.Spec != "custom_provider:org/arbitrary-model" || action.Request.Action != modelSelectorSelect {
		t.Fatalf("deferred custom selection = %#v, %v", action, ok)
	}
	if state.current != "provider:live" || state.defaultSpec != "provider:default" || state.query != "custom_provider:org/arbitrary-model" {
		t.Fatalf("deferred request mutated state: %#v", state)
	}
}
