package dacode

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestOnboardingFlowCapturesChoices(t *testing.T) {
	state := newOnboardingState([]onboardingDependency{
		{Name: "Tavily", Category: "Services", Description: "web search", Installed: false},
		{Name: "Fireworks", Category: "Models", Installed: true},
	}, []modelSelectorEntry{{Spec: "openrouter:gpt-test", Label: "GPT Test", Recommended: true}}, "")
	for _, key := range []string{"r", "a", "m", "o", "n", "enter"} {
		state.handleKey(key, 8)
	}
	if state.step != onboardingDependencies || state.result.Name != "Ramon" {
		t.Fatalf("name step = %d, result = %#v", state.step, state.result)
	}
	state.handleKey("enter", 8)
	if state.step != onboardingModel {
		t.Fatalf("dependency step = %d", state.step)
	}
	state.handleKey("enter", 8)
	if state.step != onboardingWebSearch || state.result.Model != "openrouter:gpt-test" {
		t.Fatalf("model step = %d, result = %#v", state.step, state.result)
	}
	state.handleKey("down", 8)
	state.handleKey("enter", 8)
	state.handleKey("down", 8)
	state.handleKey("enter", 8)
	result, done := state.value()
	if !done || result.Skipped || !result.ConfigureWebSearch || !result.AutoAcceptGoalCriteria {
		t.Fatalf("completed result = %#v, %v", result, done)
	}
}

func TestOnboardingEscapeSemanticsFailSafe(t *testing.T) {
	state := newOnboardingState(nil, modelSelectorCatalog(nil), "")
	state.handleKey("esc", 5)
	result, done := state.value()
	if !done || !result.Skipped || state.step != onboardingSkipped {
		t.Fatalf("name escape = %#v, %v, step %d", result, done, state.step)
	}
	state = newOnboardingState(nil, modelSelectorCatalog(nil), "")
	state.handleKey("enter", 5)
	state.handleKey("enter", 5)
	state.handleKey("esc", 5)
	result, done = state.value()
	if !done || !result.Skipped {
		t.Fatalf("model escape = %#v, %v", result, done)
	}
	state = newOnboardingState(nil, []modelSelectorEntry{{Spec: "openrouter:gpt-test", Recommended: true}}, "")
	state.handleKey("enter", 5)
	state.handleKey("enter", 5)
	state.handleKey("enter", 5)
	state.handleKey("esc", 5)
	if state.step != onboardingGoalCriteria || state.result.ConfigureWebSearch {
		t.Fatalf("web-search escape = step %d, result %#v", state.step, state.result)
	}
	state.handleKey("esc", 5)
	result, done = state.value()
	if !done || result.AutoAcceptGoalCriteria {
		t.Fatalf("criteria escape = %#v, %v", result, done)
	}
}

func TestOnboardingSkipsSearchKeyForProviderHostedModels(t *testing.T) {
	for _, spec := range []string{"anthropic:claude-test", "openai:gpt-test", "openai_oauth:gpt-test"} {
		state := newOnboardingState(nil, []modelSelectorEntry{{Spec: spec, Recommended: true}}, "")
		state.step = onboardingModel
		state.handleKey("enter", 5)
		if state.step != onboardingGoalCriteria || state.result.Model != spec || state.result.ConfigureWebSearch {
			t.Fatalf("model %q step = %d, result = %#v", spec, state.step, state.result)
		}
	}
}

func TestOnboardingNormalizesDependenciesAndUnsafeInput(t *testing.T) {
	dependencies := normalizeOnboardingDependencies([]onboardingDependency{
		{Name: "same", Category: "Z", Installed: true},
		{Name: "same", Category: "A"},
		{Name: "missing", Category: "A", Installed: false},
		{Name: "unsafe\x1b", Category: "A"},
	})
	if len(dependencies) != 2 || dependencies[0].Name != "missing" || dependencies[1].Name != "same" {
		t.Fatalf("dependencies = %#v", dependencies)
	}
	if got := normalizeOnboardingName("  ada lovelace "); got != "Ada Lovelace" {
		t.Fatalf("normalized name = %q", got)
	}
	if got := normalizeOnboardingName("unsafe\nname"); got != "" {
		t.Fatalf("unsafe name = %q", got)
	}
}

func TestOnboardingRenderUsesASCIIAndBounds(t *testing.T) {
	state := newOnboardingState([]onboardingDependency{{Name: "Provider", Category: "Models", Description: strings.Repeat("x", 200), Installed: true}}, modelSelectorCatalog(nil), "")
	state.handleKey("enter", 5)
	view := ansi.Strip(renderOnboarding(state, 80, 24, asciiUIGlyphs))
	for _, wanted := range []string{"Optional integrations", "Models", "Provider", "[OK] installed", "Enter continue"} {
		if !strings.Contains(view, wanted) {
			t.Fatalf("view missing %q:\n%s", wanted, view)
		}
	}
	if strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎") {
		t.Fatalf("ASCII onboarding contains Unicode UI glyphs:\n%s", view)
	}
}

func TestOnboardingModelStepUsesASCII(t *testing.T) {
	state := newOnboardingState(nil, []modelSelectorEntry{{Spec: "openai:gpt-test", Label: "GPT Test", Recommended: true}}, "")
	state.handleKey("enter", 5)
	state.handleKey("enter", 5)
	view := ansi.Strip(renderOnboarding(state, 80, 24, asciiUIGlyphs))
	if !strings.Contains(view, "Select Model") || strings.ContainsAny(view, "╭╮╰╯│─…✓✗⏳•⏎▌›↑↓—") {
		t.Fatalf("ASCII onboarding model step contains Unicode UI glyphs:\n%s", view)
	}
}
