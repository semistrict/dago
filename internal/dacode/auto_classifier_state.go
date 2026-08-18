package dacode

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	maxAutoClassifierSpecRunes  = 1024
	maxAutoClassifierSpecBytes  = 4096
	maxAutoClassifierCommandLen = 8192
	maxAutoClassifierNotice     = 512
)

type autoClassifierCandidate struct {
	Spec  string
	Label string
}

var pinnedAutoClassifierCandidates = []autoClassifierCandidate{
	{Spec: "anthropic:claude-haiku-4-5", Label: "Claude Haiku 4.5"},
	{Spec: "google_genai:gemini-3.6-flash", Label: "Gemini 3.6 Flash"},
	{Spec: "openai:gpt-5.6-luna", Label: "GPT-5.6 Luna"},
}

func autoClassifierCandidates() []autoClassifierCandidate {
	return append([]autoClassifierCandidate(nil), pinnedAutoClassifierCandidates...)
}

type autoClassifierSource uint8

const (
	autoClassifierMainModel autoClassifierSource = iota
	autoClassifierStartup
	autoClassifierSession
)

type autoClassifierContext struct {
	Set     bool
	Inherit bool
	Spec    string
}

func (value autoClassifierContext) String() string {
	return fmt.Sprintf("autoClassifierContext(set=%t,inherit=%t,spec=%s)", value.Set, value.Inherit, singleLineAutoSafe(value.Spec))
}

func (value autoClassifierContext) GoString() string { return value.String() }

type autoClassifierActionKind uint8

const (
	autoClassifierNoAction autoClassifierActionKind = iota
	autoClassifierActivateAuto
	autoClassifierOpenSelector
	autoClassifierValidate
	autoClassifierApply
	autoClassifierShowUsage
)

type autoClassifierAction struct {
	Kind       autoClassifierActionKind
	Spec       string
	Context    autoClassifierContext
	Persist    bool
	Revalidate bool
}

func (action autoClassifierAction) String() string {
	return fmt.Sprintf("autoClassifierAction(kind=%d,spec=%s,persist=%t,revalidate=%t,context=%s)",
		action.Kind, singleLineAutoSafe(action.Spec), action.Persist, action.Revalidate, action.Context)
}

func (action autoClassifierAction) GoString() string { return action.String() }

type autoClassifierStructuredSupport uint8

const (
	autoClassifierStructuredUnknown autoClassifierStructuredSupport = iota
	autoClassifierStructuredSupported
	autoClassifierStructuredUnsupported
)

// autoClassifierValidation is the secret-free result supplied by the runtime
// after resolving a candidate. Zero values fail closed.
type autoClassifierValidation struct {
	ModelAvailable       bool
	CredentialsAvailable bool
	StructuredOutput     autoClassifierStructuredSupport
}

type autoClassifierState struct {
	mainModel      string
	startupModel   string
	sessionModel   string
	cleared        bool
	pendingModel   string
	pendingPersist bool
	notice         string
	warning        bool
}

// newAutoClassifierState constructs classifier-model management without I/O.
// The main and startup model identities are required positional inputs; either
// may be empty when the caller has not resolved it yet.
func newAutoClassifierState(mainModel, startupModel string) *autoClassifierState {
	return &autoClassifierState{
		mainModel:    normalizeAutoClassifierSeed(mainModel),
		startupModel: normalizeAutoClassifierSeed(startupModel),
	}
}

func normalizeAutoClassifierSeed(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed != value {
		return ""
	}
	value = trimmed
	if !strings.ContainsRune(value, ':') {
		value = "openai:" + value
	}
	normalized, ok := normalizeAutoClassifierSpec(value)
	if !ok {
		return ""
	}
	return normalized
}

func normalizeAutoClassifierSpec(value string) (string, bool) {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) ||
		len(value) > maxAutoClassifierSpecBytes || utf8.RuneCountInString(value) > maxAutoClassifierSpecRunes {
		return "", false
	}
	for _, character := range value {
		if unicode.IsControl(character) || character == unicode.ReplacementChar {
			return "", false
		}
	}
	provider, model, found := strings.Cut(value, ":")
	if !found || !validAutoClassifierProvider(provider) || model == "" || strings.TrimSpace(model) != model {
		return "", false
	}
	return provider + ":" + model, true
}

func validAutoClassifierProvider(provider string) bool {
	if provider == "" {
		return false
	}
	for index, character := range provider {
		if character >= 'a' && character <= 'z' || index > 0 && (character == '_' || character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func (state *autoClassifierState) handleCommand(command string) autoClassifierAction {
	if state == nil {
		return autoClassifierAction{Kind: autoClassifierShowUsage}
	}
	if len(command) > maxAutoClassifierCommandLen || !utf8.ValidString(command) || hasAutoClassifierControl(command) {
		state.setNotice("The Auto command is malformed.", true)
		return autoClassifierAction{Kind: autoClassifierShowUsage}
	}
	fields := strings.Fields(command)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "/auto") {
		state.setNotice("Use /auto or /auto model.", true)
		return autoClassifierAction{Kind: autoClassifierShowUsage}
	}
	if len(fields) == 1 {
		state.notice, state.warning = "", false
		return autoClassifierAction{Kind: autoClassifierActivateAuto}
	}
	if !strings.EqualFold(fields[1], "model") {
		state.setNotice("Unknown /auto subcommand.", true)
		return autoClassifierAction{Kind: autoClassifierShowUsage}
	}
	if len(fields) == 2 {
		state.notice, state.warning = "", false
		return autoClassifierAction{Kind: autoClassifierOpenSelector}
	}
	if len(fields) > 3 {
		state.setNotice("Usage: /auto model [provider:model|clear]", true)
		return autoClassifierAction{Kind: autoClassifierShowUsage}
	}
	if strings.EqualFold(fields[2], "clear") {
		return state.clear()
	}
	return state.beginSelection(fields[2], false)
}

// beginSelection asks the caller to validate one model. The active classifier
// is unchanged until completeValidation accepts the matching result.
func (state *autoClassifierState) beginSelection(spec string, persist bool) autoClassifierAction {
	if state == nil {
		return autoClassifierAction{}
	}
	normalized, ok := normalizeAutoClassifierSpec(strings.TrimPrefix(spec, ":"))
	if !ok {
		state.pendingModel, state.pendingPersist = "", false
		state.setNotice("Enter a valid provider:model classifier, or clear it.", true)
		return autoClassifierAction{}
	}
	state.pendingModel, state.pendingPersist = normalized, persist
	state.notice, state.warning = "Validating classifier model...", false
	return autoClassifierAction{
		Kind: autoClassifierValidate, Spec: normalized, Persist: persist,
		Revalidate: normalized == state.reviewSpec(),
	}
}

func (state *autoClassifierState) completeValidation(spec string, validation autoClassifierValidation) autoClassifierAction {
	if state == nil {
		return autoClassifierAction{}
	}
	normalized, ok := normalizeAutoClassifierSpec(spec)
	if !ok || normalized == "" || normalized != state.pendingModel {
		state.setNotice("Ignored a stale classifier validation result.", true)
		return autoClassifierAction{}
	}
	persist := state.pendingPersist
	state.pendingModel, state.pendingPersist = "", false
	if !validation.ModelAvailable {
		state.setNotice("Classifier model is unavailable; the active reviewer was not changed.", true)
		return autoClassifierAction{}
	}
	if !validation.CredentialsAvailable {
		provider, _, _ := strings.Cut(normalized, ":")
		state.setNotice("Credentials for "+provider+" are unavailable; the active reviewer was not changed.", true)
		return autoClassifierAction{}
	}
	state.sessionModel, state.cleared = normalized, false
	if validation.StructuredOutput == autoClassifierStructuredUnsupported {
		state.setNotice("This model does not advertise the tool calling required for structured reviews; failed reviews require a user decision.", true)
	} else {
		state.setNotice("Classifier model set to "+normalized+" for the next turn.", false)
	}
	return autoClassifierAction{
		Kind: autoClassifierApply, Spec: normalized, Persist: persist,
		Context: autoClassifierContext{Set: true, Spec: normalized},
	}
}

func (state *autoClassifierState) clear() autoClassifierAction {
	if state == nil {
		return autoClassifierAction{}
	}
	state.pendingModel, state.pendingPersist = "", false
	if state.cleared && state.sessionModel == "" {
		state.setNotice("Classifier already uses the main agent model.", false)
		return autoClassifierAction{}
	}
	state.sessionModel, state.cleared = "", true
	state.setNotice("Classifier cleared for this session; reviews use the main agent model.", false)
	return autoClassifierAction{Kind: autoClassifierApply, Context: autoClassifierContext{Set: true, Inherit: true}}
}

func (state *autoClassifierState) reviewSpec() string {
	if state == nil {
		return ""
	}
	if state.sessionModel != "" {
		return state.sessionModel
	}
	if state.cleared {
		return state.mainModel
	}
	if state.startupModel != "" {
		return state.startupModel
	}
	return state.mainModel
}

func (state *autoClassifierState) source() autoClassifierSource {
	if state == nil {
		return autoClassifierMainModel
	}
	if state.sessionModel != "" {
		return autoClassifierSession
	}
	if !state.cleared && state.startupModel != "" {
		return autoClassifierStartup
	}
	return autoClassifierMainModel
}

func (state *autoClassifierState) distinctFromMain() bool {
	return state != nil && state.reviewSpec() != "" && state.mainModel != "" && state.reviewSpec() != state.mainModel
}

// contextValue preserves the distinction between no per-run override and an
// explicit clear that must override a server-side startup classifier.
func (state *autoClassifierState) contextValue() autoClassifierContext {
	if state == nil {
		return autoClassifierContext{}
	}
	if state.sessionModel != "" {
		return autoClassifierContext{Set: true, Spec: state.sessionModel}
	}
	if state.cleared {
		return autoClassifierContext{Set: true, Inherit: true}
	}
	return autoClassifierContext{}
}

func (state *autoClassifierState) selectorCurrent() string { return state.reviewSpec() }

func (state *autoClassifierState) setNotice(value string, warning bool) {
	value = truncateAutoText(singleLineAutoSafe(value), maxAutoClassifierNotice, false)
	state.notice, state.warning = value, warning
}

func (state *autoClassifierState) render(width, height int, ascii bool) string {
	if state == nil {
		return ""
	}
	width = min(max(width, 24), 160)
	height = min(max(height, 6), 20)
	reviewer := state.reviewSpec()
	if reviewer == "" {
		reviewer = "main agent model (not resolved)"
	}
	source := "main model"
	switch state.source() {
	case autoClassifierStartup:
		source = "startup classifier"
	case autoClassifierSession:
		source = "session classifier"
	}
	status := "same model as the agent"
	if state.distinctFromMain() {
		status = "separate from the agent model"
	}
	glyphs := uiGlyphsForASCII(ascii)
	separator, marker := " "+glyphs.Bullet+" ", glyphs.Checkmark
	lines := []string{
		"Auto classifier model",
		"",
		marker + " Reviewer: " + reviewer,
		"Source: " + source + separator + status,
	}
	if state.pendingModel != "" {
		lines = append(lines, "Pending validation: "+state.pendingModel)
	}
	if state.notice != "" {
		prefix := "Notice: "
		if state.warning {
			prefix = "Warning: "
		}
		lines = append(lines, "", prefix+state.notice)
	}
	lines = append(lines, "", "/auto model <spec>  "+separator+"/auto model clear")
	for index := range lines {
		lines[index] = truncateAutoText(singleLineAutoSafe(lines[index]), width, ascii)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func renderAutoClassifierUsage(state *autoClassifierState, width int, ascii bool) string {
	width = min(max(width, 24), 160)
	current := "the main agent model"
	if state != nil && state.reviewSpec() != "" {
		current = state.reviewSpec()
	}
	glyphs := uiGlyphsForASCII(ascii)
	separator := " " + glyphs.Bullet + " "
	usage := "Usage:\n" +
		"  /auto" + separator + "switch to Auto approval mode\n" +
		"  /auto model" + separator + "choose the classifier model\n" +
		"  /auto model <provider:model>" + separator + "validate and use a classifier\n" +
		"  /auto model clear" + separator + "reuse the main agent model\n\n" +
		"Current reviewer: " + singleLineAutoSafe(current)
	lines := strings.Split(usage, "\n")
	for index := range lines {
		lines[index] = truncateAutoText(singleLineAutoSafe(lines[index]), width, ascii)
	}
	return strings.Join(lines, "\n")
}

func hasAutoClassifierControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) && character != '\t' {
			return true
		}
	}
	return false
}

func singleLineAutoSafe(value string) string {
	return strings.ReplaceAll(unicodesecurity.RenderTerminalSafe(value), "\n", " ")
}

func truncateAutoText(value string, limit int, ascii bool) string {
	if limit < 1 {
		return ""
	}
	runes := []rune(value)
	if ansi.StringWidth(value) <= limit && len(runes) <= limit {
		return value
	}
	tail := uiGlyphsForASCII(ascii).Ellipsis
	tailWidth := ansi.StringWidth(tail)
	if limit <= tailWidth {
		return strings.Repeat(".", limit)
	}
	if len(runes) > limit {
		value = string(runes[:limit-tailWidth]) + tail
	}
	return ansi.Truncate(value, limit, tail)
}

type autoApprovalCapabilities struct {
	Interactive       bool
	RemoteSandbox     bool
	ReviewerAvailable bool
}

func autoApprovalEligible(capabilities autoApprovalCapabilities) bool {
	return capabilities.Interactive && !capabilities.RemoteSandbox && capabilities.ReviewerAvailable
}

type autoClassifierReviewOutcome uint8

const (
	autoClassifierReviewNotRun autoClassifierReviewOutcome = iota
	autoClassifierReviewPending
	autoClassifierReviewAllowed
	autoClassifierReviewDenied
	autoClassifierReviewUnavailable
)

type autoApprovalDisposition uint8

const (
	autoApprovalRequireHuman autoApprovalDisposition = iota
	autoApprovalRunClassifier
	autoApprovalAllow
	autoApprovalDeny
)

func shouldRunAutoClassifier(mode approvalMode, gated, deterministicAllowed bool) bool {
	return mode == approvalAuto && gated && !deterministicAllowed
}

// autoApprovalDispositionFor is execution-safe by default: missing or failed
// classifier review requires a human and never authorizes the action.
func autoApprovalDispositionFor(mode approvalMode, gated, deterministicAllowed bool, outcome autoClassifierReviewOutcome) autoApprovalDisposition {
	if !gated || mode == approvalYOLO || mode == approvalAuto && deterministicAllowed {
		return autoApprovalAllow
	}
	if mode != approvalAuto {
		return autoApprovalRequireHuman
	}
	switch outcome {
	case autoClassifierReviewPending:
		return autoApprovalRunClassifier
	case autoClassifierReviewAllowed:
		return autoApprovalAllow
	case autoClassifierReviewDenied:
		return autoApprovalDeny
	case autoClassifierReviewUnavailable:
		// The current batch fails closed without executing. A caller-owned
		// counter/latch decides when later batches escalate to a human.
		return autoApprovalDeny
	default:
		return autoApprovalRequireHuman
	}
}
