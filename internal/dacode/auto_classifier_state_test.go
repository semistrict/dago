package dacode

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestAutoClassifierStatePreservesStartupSessionAndExplicitClear(t *testing.T) {
	state := newAutoClassifierState("openai:main", "anthropic:startup")
	if got := state.reviewSpec(); got != "anthropic:startup" || state.source() != autoClassifierStartup || !state.distinctFromMain() {
		t.Fatalf("startup state = spec %q, source %d, distinct %t", got, state.source(), state.distinctFromMain())
	}
	if context := state.contextValue(); context.Set || context.Inherit || context.Spec != "" {
		t.Fatalf("startup context override = %#v", context)
	}
	action := state.clear()
	if action.Kind != autoClassifierApply || !action.Context.Set || !action.Context.Inherit || action.Context.Spec != "" {
		t.Fatalf("clear action = %#v", action)
	}
	if got := state.reviewSpec(); got != "openai:main" || state.source() != autoClassifierMainModel || state.distinctFromMain() {
		t.Fatalf("cleared state = spec %q, source %d, distinct %t", got, state.source(), state.distinctFromMain())
	}
	if action := state.clear(); action.Kind != autoClassifierNoAction || !strings.Contains(state.notice, "already uses") {
		t.Fatalf("repeated clear = %#v, notice=%q", action, state.notice)
	}
	if action := state.beginSelection("anthropic:startup", false); action.Revalidate {
		t.Fatalf("cleared startup classifier was treated as active: %#v", action)
	}
}

func TestAutoClassifierZeroAndMalformedSeedsUseMainFallback(t *testing.T) {
	state := newAutoClassifierState(" malformed ", "UPPER:model")
	if state.mainModel != "" || state.startupModel != "" || state.reviewSpec() != "" || state.selectorCurrent() != "" {
		t.Fatalf("malformed seeds = %#v", state)
	}
	var zero autoClassifierState
	if got := zero.render(80, 8, true); !strings.Contains(got, "not resolved") || strings.Contains(got, "✓") {
		t.Fatalf("zero render = %q", got)
	}
	if context := zero.contextValue(); context.Set {
		t.Fatalf("zero context = %#v", context)
	}
}

func TestAutoClassifierBareMainModelUsesImplicitOpenAIProvider(t *testing.T) {
	state := newAutoClassifierState("gpt-5.6-terra", "")
	if got := state.reviewSpec(); got != "openai:gpt-5.6-terra" || state.source() != autoClassifierMainModel {
		t.Fatalf("bare main model = spec %q, source %d", got, state.source())
	}
}

func TestAutoClassifierCandidatesMatchPinnedFastReviewModels(t *testing.T) {
	candidates := autoClassifierCandidates()
	want := []string{
		"anthropic:claude-haiku-4-5",
		"google_genai:gemini-3.6-flash",
		"openai:gpt-5.6-luna",
	}
	if len(candidates) != len(want) {
		t.Fatalf("candidate count = %d", len(candidates))
	}
	for index, spec := range want {
		if candidates[index].Spec != spec || candidates[index].Label == "" {
			t.Fatalf("candidate %d = %#v", index, candidates[index])
		}
	}
	candidates[0].Spec = "mutated:model"
	if autoClassifierCandidates()[0].Spec != want[0] {
		t.Fatal("candidate registry was not defensively copied")
	}
}

func TestAutoClassifierCommandSemantics(t *testing.T) {
	state := newAutoClassifierState("openai:main", "")
	if action := state.handleCommand("/auto"); action.Kind != autoClassifierActivateAuto {
		t.Fatalf("bare command action = %#v", action)
	}
	if action := state.handleCommand(" /AUTO\t model "); action.Kind != autoClassifierOpenSelector {
		t.Fatalf("selector action = %#v", action)
	}
	action := state.handleCommand("/auto model anthropic:reviewer")
	if action.Kind != autoClassifierValidate || action.Spec != "anthropic:reviewer" || action.Persist {
		t.Fatalf("model action = %#v", action)
	}
	if action := state.handleCommand("/auto model clear extra"); action.Kind != autoClassifierShowUsage {
		t.Fatalf("extra argument action = %#v", action)
	}
	if action := state.handleCommand("/auto unknown"); action.Kind != autoClassifierShowUsage {
		t.Fatalf("unknown subcommand action = %#v", action)
	}
	if action := state.handleCommand("/manual"); action.Kind != autoClassifierShowUsage {
		t.Fatalf("foreign command action = %#v", action)
	}
	for _, command := range []string{"/auto\nmodel", "/auto model provider:", "/auto model :model", "/auto model Provider:model"} {
		if action := state.handleCommand(command); action.Kind == autoClassifierValidate {
			t.Fatalf("malformed command %q produced validation: %#v", command, action)
		}
	}
	if action := state.handleCommand(strings.Repeat("x", maxAutoClassifierCommandLen+1)); action.Kind != autoClassifierShowUsage {
		t.Fatalf("oversized command action = %#v", action)
	}
}

func TestAutoClassifierSelectionDoesNotMutateBeforeValidation(t *testing.T) {
	state := newAutoClassifierState("openai:main", "anthropic:startup")
	action := state.beginSelection("openai:reviewer", true)
	if action.Kind != autoClassifierValidate || !action.Persist || action.Revalidate || state.reviewSpec() != "anthropic:startup" {
		t.Fatalf("pending selection = %#v, review=%q", action, state.reviewSpec())
	}
	if state.pendingModel != "openai:reviewer" || !state.pendingPersist {
		t.Fatalf("pending state = %#v", state)
	}
	stale := state.completeValidation("openai:other", autoClassifierValidation{ModelAvailable: true, CredentialsAvailable: true})
	if stale.Kind != autoClassifierNoAction || state.reviewSpec() != "anthropic:startup" || state.pendingModel != "openai:reviewer" {
		t.Fatalf("stale validation = %#v, state=%#v", stale, state)
	}
}

func TestAutoClassifierValidationFailsClosedWithoutChangingReviewer(t *testing.T) {
	tests := []struct {
		name       string
		validation autoClassifierValidation
		wantNotice string
	}{
		{name: "zero", validation: autoClassifierValidation{}, wantNotice: "model is unavailable"},
		{name: "credentials", validation: autoClassifierValidation{ModelAvailable: true}, wantNotice: "Credentials for openai are unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newAutoClassifierState("openai:main", "anthropic:startup")
			state.beginSelection("openai:reviewer", false)
			action := state.completeValidation("openai:reviewer", test.validation)
			if action.Kind != autoClassifierNoAction || state.reviewSpec() != "anthropic:startup" || !state.warning || !strings.Contains(state.notice, test.wantNotice) {
				t.Fatalf("failed validation action=%#v state=%#v", action, state)
			}
			if state.pendingModel != "" || state.pendingPersist {
				t.Fatalf("failed validation retained pending state: %#v", state)
			}
		})
	}
}

func TestAutoClassifierValidationAppliesContextPersistenceAndWarnings(t *testing.T) {
	state := newAutoClassifierState("openai:main", "anthropic:startup")
	state.beginSelection("openai:reviewer", true)
	action := state.completeValidation("openai:reviewer", autoClassifierValidation{
		ModelAvailable: true, CredentialsAvailable: true, StructuredOutput: autoClassifierStructuredUnsupported,
	})
	if action.Kind != autoClassifierApply || action.Spec != "openai:reviewer" || !action.Persist ||
		!action.Context.Set || action.Context.Inherit || action.Context.Spec != "openai:reviewer" {
		t.Fatalf("applied action = %#v", action)
	}
	if state.reviewSpec() != "openai:reviewer" || state.source() != autoClassifierSession || !state.warning || !strings.Contains(state.notice, "tool calling") {
		t.Fatalf("applied state = %#v", state)
	}
	revalidate := state.beginSelection("openai:reviewer", false)
	if !revalidate.Revalidate {
		t.Fatalf("same-spec selection did not request revalidation: %#v", revalidate)
	}
	validated := state.completeValidation("openai:reviewer", autoClassifierValidation{
		ModelAvailable: true, CredentialsAvailable: true, StructuredOutput: autoClassifierStructuredSupported,
	})
	if validated.Kind != autoClassifierApply || state.warning || !strings.Contains(state.notice, "next turn") {
		t.Fatalf("revalidated result = %#v, state=%#v", validated, state)
	}
}

func TestAutoClassifierSpecValidationIsBoundedAndAdversarial(t *testing.T) {
	valid := []string{
		"openai:model", "openai_oauth:model:tag", "provider:model/name", "p1:model with internal space",
	}
	for _, value := range valid {
		if got, ok := normalizeAutoClassifierSpec(value); !ok || got != value {
			t.Fatalf("valid spec %q = %q, %t", value, got, ok)
		}
	}
	invalid := []string{
		"", " provider:model", "provider:model ", "Provider:model", "1provider:model", "provider-name:model",
		"provider:", ":model", "provider:model\nnext", "provider:model\x00tail", string([]byte{0xff}),
		"provider:" + strings.Repeat("x", maxAutoClassifierSpecBytes+1),
	}
	for _, value := range invalid {
		if got, ok := normalizeAutoClassifierSpec(value); ok || got != "" {
			t.Fatalf("invalid spec %q = %q, %t", value, got, ok)
		}
	}
}

func TestAutoClassifierRenderingAndActionsAreTerminalSafeBoundedAndASCII(t *testing.T) {
	state := newAutoClassifierState("openai:main", "anthropic:startup")
	state.setNotice("unsafe\x1b[31m\nmessage"+strings.Repeat("long", 300), true)
	view := state.render(42, 8, true)
	if strings.ContainsRune(view, '\x1b') || !strings.Contains(view, "<U+001B CONTROL>") {
		t.Fatalf("terminal-unsafe view = %q", view)
	}
	for _, forbidden := range []string{"✓", "•", "…"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("ASCII view contains %q: %q", forbidden, view)
		}
	}
	assertAutoClassifierBounds(t, view, 42, 8)
	action := autoClassifierAction{Kind: autoClassifierApply, Spec: "openai:model", Context: autoClassifierContext{Set: true, Spec: "openai:model"}}
	for _, rendered := range []string{fmt.Sprint(action), fmt.Sprintf("%#v", action), fmt.Sprint(action.Context)} {
		if strings.ContainsRune(rendered, '\x1b') || strings.ContainsRune(rendered, '\n') {
			t.Fatalf("unsafe action formatting = %q", rendered)
		}
	}
	usage := renderAutoClassifierUsage(state, 80, true)
	if !strings.Contains(usage, "/auto model clear") || strings.Contains(usage, "•") {
		t.Fatalf("ASCII usage = %q", usage)
	}
	assertAutoClassifierBounds(t, usage, 80, 10)
}

func TestAutoApprovalEligibilityFailsClosed(t *testing.T) {
	if autoApprovalEligible(autoApprovalCapabilities{}) {
		t.Fatal("zero capabilities were eligible")
	}
	if autoApprovalEligible(autoApprovalCapabilities{Interactive: true, ReviewerAvailable: true, RemoteSandbox: true}) {
		t.Fatal("remote sandbox was eligible")
	}
	if autoApprovalEligible(autoApprovalCapabilities{Interactive: true}) {
		t.Fatal("missing reviewer was eligible")
	}
	if !autoApprovalEligible(autoApprovalCapabilities{Interactive: true, ReviewerAvailable: true}) {
		t.Fatal("local interactive reviewer was ineligible")
	}
}

func TestAutoApprovalRoutingPredicates(t *testing.T) {
	if shouldRunAutoClassifier(approvalManual, true, false) || shouldRunAutoClassifier(approvalYOLO, true, false) || shouldRunAutoClassifier(approvalAuto, false, false) || shouldRunAutoClassifier(approvalAuto, true, true) {
		t.Fatal("classifier ran outside gated non-deterministic Auto")
	}
	if !shouldRunAutoClassifier(approvalAuto, true, false) {
		t.Fatal("classifier did not run for gated Auto")
	}
	tests := []struct {
		name          string
		mode          approvalMode
		gated         bool
		deterministic bool
		outcome       autoClassifierReviewOutcome
		want          autoApprovalDisposition
	}{
		{name: "ungated", mode: approvalManual, outcome: autoClassifierReviewNotRun, want: autoApprovalAllow},
		{name: "manual", mode: approvalManual, gated: true, want: autoApprovalRequireHuman},
		{name: "unrestricted", mode: approvalYOLO, gated: true, want: autoApprovalAllow},
		{name: "deterministic", mode: approvalAuto, gated: true, deterministic: true, want: autoApprovalAllow},
		{name: "pending", mode: approvalAuto, gated: true, outcome: autoClassifierReviewPending, want: autoApprovalRunClassifier},
		{name: "allowed", mode: approvalAuto, gated: true, outcome: autoClassifierReviewAllowed, want: autoApprovalAllow},
		{name: "denied", mode: approvalAuto, gated: true, outcome: autoClassifierReviewDenied, want: autoApprovalDeny},
		{name: "unavailable", mode: approvalAuto, gated: true, outcome: autoClassifierReviewUnavailable, want: autoApprovalDeny},
		{name: "not-run", mode: approvalAuto, gated: true, outcome: autoClassifierReviewNotRun, want: autoApprovalRequireHuman},
		{name: "invalid-mode", mode: approvalMode(99), gated: true, outcome: autoClassifierReviewAllowed, want: autoApprovalRequireHuman},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := autoApprovalDispositionFor(test.mode, test.gated, test.deterministic, test.outcome); got != test.want {
				t.Fatalf("disposition = %d, want %d", got, test.want)
			}
		})
	}
}

func assertAutoClassifierBounds(t *testing.T, rendered string, width, height int) {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	if len(lines) > height {
		t.Fatalf("rendered lines = %d, want <= %d", len(lines), height)
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) > width || ansi.StringWidth(line) > width {
			t.Fatalf("line exceeds width %d: %q", width, line)
		}
	}
}
