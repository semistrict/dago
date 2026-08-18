package dacode

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	maxModelSelectorEntries = 1024
	maxRecentModelEntries   = 16
)

type modelSelectorEntry struct {
	Spec             string
	Provider         string
	Label            string
	Recent           bool
	Recommended      bool
	InstallStatus    modelRequirementStatus
	CredentialStatus modelRequirementStatus
}

type modelRequirementStatus uint8

const (
	modelRequirementUnknown modelRequirementStatus = iota
	modelRequirementReady
	modelRequirementMissing
	modelRequirementNotRequired
)

type modelProviderAvailability struct {
	Install     modelRequirementStatus
	Credentials modelRequirementStatus
}

type modelSelectorOptions struct {
	ProviderAvailability map[string]modelProviderAvailability
}

type modelSelectorState struct {
	entries         []modelSelectorEntry
	visible         []int
	selected        int
	query           string
	current         string
	defaultSpec     string
	recommendedOnly bool
	showSpecs       bool
	title           string
	selectorID      uint64
	generation      uint64
	pendingWrite    *modelPreferenceWrite
}

type modelSelectorAction uint8

const (
	modelSelectorNoAction modelSelectorAction = iota
	modelSelectorCancel
	modelSelectorSelect
	modelSelectorSetDefault
	modelSelectorClearDefault
)

type modelSelectorResult struct {
	Action  modelSelectorAction
	Spec    string
	Request modelSelectorRequest
	Err     error
}

type modelSelectorRequest struct {
	Action       modelSelectorAction
	Spec         string
	PriorDefault string
	SelectorID   uint64
	Generation   uint64
}

type deferredModelSelectorAction struct {
	Request modelSelectorRequest
}

type modelPreferenceWrite struct {
	SelectorID uint64
	Generation uint64
	Previous   string
	Next       string
	Clear      bool
}

var nextModelSelectorID atomic.Uint64

func newModelSelector(entries []modelSelectorEntry, current, defaultSpec string) *modelSelectorState {
	return newModelSelectorWithOptions(entries, current, defaultSpec, modelSelectorOptions{})
}

func newModelSelectorWithOptions(entries []modelSelectorEntry, current, defaultSpec string, options modelSelectorOptions) *modelSelectorState {
	return newNamedModelSelectorWithOptions(entries, current, defaultSpec, "Select Model", options)
}

func newNamedModelSelector(entries []modelSelectorEntry, current, defaultSpec, title string) *modelSelectorState {
	return newNamedModelSelectorWithOptions(entries, current, defaultSpec, title, modelSelectorOptions{})
}

func newNamedModelSelectorWithOptions(entries []modelSelectorEntry, current, defaultSpec, title string, options modelSelectorOptions) *modelSelectorState {
	normalized := normalizeModelSelectorEntries(entries)
	applyModelProviderAvailability(normalized, options.ProviderAvailability)
	state := &modelSelectorState{
		entries:         normalized,
		current:         validModelSelectorSpec(current),
		defaultSpec:     validModelSelectorSpec(defaultSpec),
		recommendedOnly: true,
		title:           boundedMCPSingleLine(title, 80, "Select Model"),
		selectorID:      nextModelSelectorID.Add(1),
		generation:      1,
	}
	state.refilter()
	state.selectSpec(state.current)
	return state
}

func normalizeModelSelectorEntries(entries []modelSelectorEntry) []modelSelectorEntry {
	result := make([]modelSelectorEntry, 0, min(len(entries), maxModelSelectorEntries))
	seen := make(map[string]bool, min(len(entries), maxModelSelectorEntries))
	for _, entry := range entries {
		if len(result) == maxModelSelectorEntries {
			break
		}
		entry.Spec = validModelSelectorSpec(entry.Spec)
		if entry.Spec == "" || seen[entry.Spec] {
			continue
		}
		seen[entry.Spec] = true
		entry.Provider, _, _ = strings.Cut(entry.Spec, ":")
		entry.Label = strings.TrimSpace(entry.Label)
		if entry.Label == "" || utf8.RuneCountInString(entry.Label) > 160 || hasModelSelectorControl(entry.Label) {
			entry.Label = modelSelectorDisplayName(entry.Spec)
		}
		entry.InstallStatus = validModelRequirementStatus(entry.InstallStatus)
		entry.CredentialStatus = validModelRequirementStatus(entry.CredentialStatus)
		result = append(result, entry)
	}
	return result
}

func validModelRequirementStatus(status modelRequirementStatus) modelRequirementStatus {
	if status > modelRequirementNotRequired {
		return modelRequirementUnknown
	}
	return status
}

func applyModelProviderAvailability(entries []modelSelectorEntry, availability map[string]modelProviderAvailability) {
	for index := range entries {
		providerStatus, exists := availability[entries[index].Provider]
		if !exists {
			continue
		}
		if entries[index].InstallStatus == modelRequirementUnknown {
			entries[index].InstallStatus = validModelRequirementStatus(providerStatus.Install)
		}
		if entries[index].CredentialStatus == modelRequirementUnknown {
			entries[index].CredentialStatus = validModelRequirementStatus(providerStatus.Credentials)
		}
	}
}

func modelSelectorCatalog(recent []string) []modelSelectorEntry {
	if len(recent) > maxRecentModelEntries {
		recent = recent[:maxRecentModelEntries]
	}
	recommended := []modelSelectorEntry{
		{Spec: "anthropic:claude-opus-4-8", Label: "Claude Opus 4.8"},
		{Spec: "anthropic:claude-opus-5", Label: "Claude Opus 5"},
		{Spec: "anthropic:claude-sonnet-5", Label: "Claude Sonnet 5"},
		{Spec: "baseten:deepseek-ai/DeepSeek-V4-Flash-0731", Label: "DeepSeek V4 Flash 0731"},
		{Spec: "baseten:deepseek-ai/DeepSeek-V4-Pro", Label: "DeepSeek V4 Pro"},
		{Spec: "baseten:moonshotai/Kimi-K3", Label: "Kimi K3"},
		{Spec: "baseten:nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B", Label: "Nemotron 3 Ultra 550B A55B"},
		{Spec: "baseten:zai-org/GLM-5.2", Label: "GLM 5.2"},
		{Spec: "baseten:zai-org/GLM-5.2-Fast", Label: "GLM 5.2 Fast"},
		{Spec: "fireworks:accounts/fireworks/models/deepseek-v4-flash-0731", Label: "DeepSeek V4 Flash 0731"},
		{Spec: "fireworks:accounts/fireworks/models/deepseek-v4-pro", Label: "DeepSeek V4 Pro"},
		{Spec: "fireworks:accounts/fireworks/models/glm-5p2", Label: "GLM 5.2"},
		{Spec: "fireworks:accounts/fireworks/models/kimi-k3", Label: "Kimi K3"},
		{Spec: "fireworks:accounts/fireworks/models/minimax-m3", Label: "MiniMax-M3"},
		{Spec: "fireworks:accounts/fireworks/models/qwen3p7-plus", Label: "Qwen 3.7 Plus"},
		{Spec: "google_genai:gemini-3.6-flash", Label: "Gemini 3.6 Flash"},
		{Spec: "meta:muse-spark-1.1", Label: "Muse Spark 1.1"},
		{Spec: "meta:muse-spark-1.2", Label: "Muse Spark 1.2"},
		{Spec: "ollama:deepseek-v4-flash:cloud", Label: "DeepSeek V4 Flash"},
		{Spec: "ollama:deepseek-v4-pro:cloud", Label: "DeepSeek V4 Pro"},
		{Spec: "ollama:glm-5.2:cloud", Label: "GLM 5.2"},
		{Spec: "ollama:minimax-m3:cloud", Label: "MiniMax-M3"},
		{Spec: "openai:gpt-5.6-luna", Label: "GPT-5.6 Luna"},
		{Spec: "openai:gpt-5.6-sol", Label: "GPT-5.6 Sol"},
		{Spec: "openai:gpt-5.6-terra", Label: "GPT-5.6 Terra"},
		{Spec: "openai_oauth:gpt-5.6-luna", Label: "GPT-5.6 Luna"},
		{Spec: "openai_oauth:gpt-5.6-sol", Label: "GPT-5.6 Sol"},
		{Spec: "openai_oauth:gpt-5.6-terra", Label: "GPT-5.6 Terra"},
		{Spec: "openrouter:anthropic/claude-opus-4.8", Label: "Claude Opus 4.8"},
		{Spec: "openrouter:anthropic/claude-sonnet-5", Label: "Claude Sonnet 5"},
		{Spec: "openrouter:deepseek/deepseek-v4-flash-0731", Label: "DeepSeek V4 Flash 0731"},
		{Spec: "openrouter:deepseek/deepseek-v4-flash:free", Label: "DeepSeek V4 Flash (free)"},
		{Spec: "openrouter:deepseek/deepseek-v4-pro", Label: "DeepSeek V4 Pro"},
		{Spec: "openrouter:google/gemini-3.6-flash", Label: "Gemini 3.6 Flash"},
		{Spec: "openrouter:moonshotai/kimi-k3", Label: "Kimi K3"},
		{Spec: "openrouter:nvidia/nemotron-3-ultra-550b-a55b", Label: "Nemotron 3 Ultra 550B A55B"},
		{Spec: "openrouter:openrouter/fusion", Label: "OpenRouter Fusion"},
		{Spec: "openrouter:qwen/qwen3.7-plus", Label: "Qwen 3.7 Plus"},
		{Spec: "openrouter:z-ai/glm-5.2", Label: "GLM 5.2"},
		{Spec: "xai:grok-4.5", Label: "Grok 4.5"},
	}
	for index := range recommended {
		recommended[index].Recommended = true
	}
	bySpec := make(map[string]modelSelectorEntry, len(recommended))
	for _, entry := range recommended {
		bySpec[entry.Spec] = entry
	}
	result := make([]modelSelectorEntry, 0, len(recent)+len(recommended))
	seen := make(map[string]bool, len(recent)+len(recommended))
	for _, spec := range recent {
		spec = validModelSelectorSpec(spec)
		if spec == "" || seen[spec] {
			continue
		}
		entry, exists := bySpec[spec]
		if !exists {
			entry = modelSelectorEntry{Spec: spec, Label: modelSelectorDisplayName(spec)}
		}
		entry.Recent = true
		seen[spec] = true
		result = append(result, entry)
	}
	for _, entry := range recommended {
		if !seen[entry.Spec] {
			result = append(result, entry)
		}
	}
	return normalizeModelSelectorEntries(result)
}

func (state *modelSelectorState) setQuery(value string) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > 512 {
		value = string([]rune(value)[:512])
	}
	state.query = value
	state.refilter()
}

func (state *modelSelectorState) toggleRecommended() {
	state.recommendedOnly = !state.recommendedOnly
	state.refilter()
}

func (state *modelSelectorState) toggleNames() { state.showSpecs = !state.showSpecs }

func (state *modelSelectorState) move(delta int) {
	if len(state.visible) == 0 {
		state.selected = 0
		return
	}
	state.selected = wrapIndex(state.selected+delta, len(state.visible))
}

func (state *modelSelectorState) page(delta, height int) {
	if height < 1 {
		height = 1
	}
	state.move(delta * height)
}

func (state *modelSelectorState) selectedEntry() (modelSelectorEntry, bool) {
	if state.selected < 0 || state.selected >= len(state.visible) {
		return modelSelectorEntry{}, false
	}
	return state.entries[state.visible[state.selected]], true
}

func (state *modelSelectorState) autocomplete() string {
	entry, ok := state.selectedEntry()
	if !ok {
		return state.query
	}
	state.setQuery(entry.Spec)
	return state.query
}

func (state *modelSelectorState) selection() (string, error) {
	if typed := validModelSelectorSpec(state.query); typed != "" && strings.Contains(state.query, ":") {
		return typed, nil
	}
	if entry, ok := state.selectedEntry(); ok {
		return entry.Spec, nil
	}
	return "", fmt.Errorf("enter a provider:model selection")
}

func (state *modelSelectorState) toggleDefault() (spec string, clear bool, err error) {
	spec, err = state.selection()
	if err != nil {
		return "", false, err
	}
	return spec, spec == state.defaultSpec, nil
}

func (state *modelSelectorState) request(action modelSelectorAction, spec string) modelSelectorRequest {
	return modelSelectorRequest{
		Action: action, Spec: spec, PriorDefault: state.defaultSpec,
		SelectorID: state.selectorID, Generation: state.generation,
	}
}

func (result modelSelectorResult) deferredAction() (deferredModelSelectorAction, bool) {
	switch result.Action {
	case modelSelectorSelect, modelSelectorSetDefault, modelSelectorClearDefault:
		return deferredModelSelectorAction{Request: result.Request}, result.Err == nil
	default:
		return deferredModelSelectorAction{}, false
	}
}

func (state *modelSelectorState) beginPreferenceWrite(request modelSelectorRequest) (modelPreferenceWrite, bool) {
	if state == nil || state.pendingWrite != nil || request.SelectorID != state.selectorID || request.Generation != state.generation ||
		request.PriorDefault != state.defaultSpec || validModelSelectorSpec(request.Spec) != request.Spec {
		return modelPreferenceWrite{}, false
	}
	next := request.Spec
	clear := request.Action == modelSelectorClearDefault
	if clear {
		next = ""
	} else if request.Action != modelSelectorSetDefault {
		return modelPreferenceWrite{}, false
	}
	if clear != (request.Spec == state.defaultSpec) || !clear && request.Spec == state.defaultSpec {
		return modelPreferenceWrite{}, false
	}
	state.defaultSpec = next
	state.generation++
	write := modelPreferenceWrite{
		SelectorID: state.selectorID, Generation: state.generation,
		Previous: request.PriorDefault, Next: next, Clear: clear,
	}
	state.pendingWrite = &write
	return write, true
}

// finishPreferenceWrite accepts only the exact in-flight write. A failed write
// restores the prior default, while stale completions cannot affect newer state.
func (state *modelSelectorState) finishPreferenceWrite(write modelPreferenceWrite, err error) bool {
	if state == nil || state.pendingWrite == nil || *state.pendingWrite != write {
		return false
	}
	state.pendingWrite = nil
	if err != nil {
		state.defaultSpec = write.Previous
	}
	state.generation++
	return true
}

func (state *modelSelectorState) replaceDefault(spec string) {
	state.defaultSpec = validModelSelectorSpec(spec)
	state.pendingWrite = nil
	state.generation++
}

func (state *modelSelectorState) label(entry modelSelectorEntry) string {
	if state.showSpecs {
		return entry.Spec
	}
	return entry.Label
}

func (state *modelSelectorState) handleKey(key string, pageHeight int) modelSelectorResult {
	switch key {
	case "esc", "ctrl+c":
		return modelSelectorResult{Action: modelSelectorCancel}
	case "up":
		state.move(-1)
	case "down":
		state.move(1)
	case "pgup":
		state.page(-1, pageHeight)
	case "pgdown":
		state.page(1, pageHeight)
	case "tab":
		state.autocomplete()
	case "ctrl+r":
		state.toggleRecommended()
	case "ctrl+n":
		state.toggleNames()
	case "ctrl+s":
		if state.pendingWrite != nil {
			return modelSelectorResult{Err: fmt.Errorf("default model preference is still being saved")}
		}
		spec, clear, err := state.toggleDefault()
		if err != nil {
			return modelSelectorResult{Err: err}
		}
		action := modelSelectorSetDefault
		if clear {
			action = modelSelectorClearDefault
		}
		return modelSelectorResult{Action: action, Spec: spec, Request: state.request(action, spec)}
	case "enter":
		spec, err := state.selection()
		return modelSelectorResult{Action: modelSelectorSelect, Spec: spec, Request: state.request(modelSelectorSelect, spec), Err: err}
	case "backspace":
		characters := []rune(state.query)
		if len(characters) > 0 {
			state.setQuery(string(characters[:len(characters)-1]))
		}
	case "ctrl+w":
		state.setQuery(strings.TrimRightFunc(strings.TrimRightFunc(state.query, unicode.IsSpace), func(character rune) bool {
			return !unicode.IsSpace(character)
		}))
	default:
		characters := []rune(key)
		if len(characters) == 1 && unicode.IsPrint(characters[0]) {
			state.setQuery(state.query + key)
		}
	}
	return modelSelectorResult{}
}

func renderModelSelector(state *modelSelectorState, width, height int, glyphs uiGlyphs) string {
	if state == nil || width < 24 || height < 12 {
		return ""
	}
	contentWidth := min(max(width-8, 16), 76)
	listHeight := min(max(height-11, 1), 16)
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(modelSelectorFit(state.title, contentWidth, glyphs.Ellipsis)),
		lipgloss.NewStyle().Foreground(colorMuted).Render(modelSelectorFit(unicodesecurity.RenderTerminalSafe(modelSelectorInfo(state, glyphs)), contentWidth, glyphs.Ellipsis)),
		lipgloss.NewStyle().Foreground(colorBody).Render("Filter: ") + lipgloss.NewStyle().Foreground(colorPrimary).Render(
			modelSelectorFit(unicodesecurity.RenderTerminalSafe(state.query)+glyphs.Cursor, max(contentWidth-8, 1), glyphs.Ellipsis)),
		"",
	}
	if len(state.visible) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
			modelSelectorFit("No matching catalog models. Enter a full provider:model value.", contentWidth, glyphs.Ellipsis)))
	} else {
		rows, selectedRow := state.renderRows()
		start := max(selectedRow-listHeight/2, 0)
		start = min(start, max(len(rows)-listHeight, 0))
		for _, row := range rows[start:min(start+listHeight, len(rows))] {
			if row.heading != "" {
				lines = append(lines, lipgloss.NewStyle().Foreground(colorSecondary).Bold(true).Render(
					modelSelectorFit(unicodesecurity.RenderTerminalSafe(row.heading), contentWidth, glyphs.Ellipsis)))
				continue
			}
			visibleIndex := row.visibleIndex
			entry := state.entries[state.visible[visibleIndex]]
			labels := make([]string, 0, 2)
			if entry.Spec == state.current {
				labels = append(labels, "current")
			}
			if entry.Spec == state.defaultSpec {
				labels = append(labels, "default")
			}
			label := state.label(entry)
			if entry.Recent && !state.showSpecs {
				label += " (" + entry.Provider + ")"
			}
			if len(labels) > 0 {
				label += " (" + strings.Join(labels, ", ") + ")"
			}
			if availability := modelSelectorAvailabilityLabel(entry); availability != "" {
				label += " [" + availability + "]"
			}
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth)
			if visibleIndex == state.selected {
				prefix = glyphs.Cursor + " "
				style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
			}
			lines = append(lines, style.Render(modelSelectorFit(prefix+unicodesecurity.RenderTerminalSafe(label), contentWidth, glyphs.Ellipsis)))
		}
	}
	footer := glyphs.ArrowUp + "/" + glyphs.ArrowDown + " navigate  " + glyphs.Bullet + "  Tab autocomplete  " + glyphs.Bullet + "  Enter select  " + glyphs.Bullet + "  Ctrl+S set default"
	footer += "\nCtrl+R recommended  " + glyphs.Bullet + "  Ctrl+N "
	if state.showSpecs {
		footer += "names"
	} else {
		footer += "IDs"
	}
	footer += "  " + glyphs.Bullet + "  Esc cancel"
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(modelSelectorFitLines(footer, contentWidth, glyphs.Ellipsis)))
	panel := lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, lipgloss.Width(panel)), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

type modelSelectorRenderRow struct {
	heading      string
	visibleIndex int
}

func (state *modelSelectorState) renderRows() ([]modelSelectorRenderRow, int) {
	rows := make([]modelSelectorRenderRow, 0, len(state.visible)*2)
	selectedRow, previousSection := 0, ""
	for visibleIndex, entryIndex := range state.visible {
		entry := state.entries[entryIndex]
		section := entry.Provider
		if entry.Recent {
			section = "Recent"
		}
		if section != previousSection {
			rows = append(rows, modelSelectorRenderRow{heading: section})
			previousSection = section
		}
		if visibleIndex == state.selected {
			selectedRow = len(rows)
		}
		rows = append(rows, modelSelectorRenderRow{visibleIndex: visibleIndex})
	}
	return rows, selectedRow
}

func modelSelectorAvailabilityLabel(entry modelSelectorEntry) string {
	missing := make([]string, 0, 2)
	if entry.InstallStatus == modelRequirementMissing {
		missing = append(missing, "install required")
	}
	if entry.CredentialStatus == modelRequirementMissing {
		missing = append(missing, "credentials required")
	}
	if len(missing) != 0 {
		return strings.Join(missing, "; ")
	}
	installKnown := entry.InstallStatus == modelRequirementReady || entry.InstallStatus == modelRequirementNotRequired
	credentialsKnown := entry.CredentialStatus == modelRequirementReady || entry.CredentialStatus == modelRequirementNotRequired
	if installKnown && credentialsKnown {
		return "available"
	}
	if entry.InstallStatus == modelRequirementReady {
		return "installed"
	}
	if entry.CredentialStatus == modelRequirementReady {
		return "credentials ready"
	}
	return ""
}

func modelSelectorFit(value string, width int, ellipsis string) string {
	if width < 1 {
		return ""
	}
	return ansi.Truncate(value, width, ellipsis)
}

func modelSelectorFitLines(value string, width int, ellipsis string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = modelSelectorFit(lines[index], width, ellipsis)
	}
	return strings.Join(lines, "\n")
}

func modelSelectorInfo(state *modelSelectorState, glyphs uiGlyphs) string {
	info := state.info()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		return strings.ReplaceAll(info, " "+unicodeUIGlyphs.Dash+" ", " "+glyphs.Dash+" ")
	}
	return info
}

func (state *modelSelectorState) info() string {
	if state.query != "" {
		return "Searching all models"
	}
	if state.recommendedOnly {
		return "Showing recommended models " + unicodeUIGlyphs.Dash + " Ctrl+R for all"
	}
	return "Showing all models " + unicodeUIGlyphs.Dash + " Ctrl+R for recommended"
}

func (state *modelSelectorState) refilter() {
	selectedSpec := ""
	if entry, ok := state.selectedEntry(); ok {
		selectedSpec = entry.Spec
	}
	query := strings.ToLower(state.query)
	state.visible = state.visible[:0]
	for index, entry := range state.entries {
		if query == "" && state.recommendedOnly && !entry.Recommended && !entry.Recent {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(entry.Spec), query) &&
			!strings.Contains(strings.ToLower(entry.Label), query) &&
			!strings.Contains(strings.ToLower(entry.Provider), query) {
			continue
		}
		state.visible = append(state.visible, index)
	}
	sort.SliceStable(state.visible, func(left, right int) bool {
		leftEntry, rightEntry := state.entries[state.visible[left]], state.entries[state.visible[right]]
		if leftEntry.Recent != rightEntry.Recent {
			return leftEntry.Recent
		}
		if leftEntry.Recent {
			return false
		}
		leftProvider, rightProvider := strings.ToLower(leftEntry.Provider), strings.ToLower(rightEntry.Provider)
		if leftProvider != rightProvider {
			return leftProvider < rightProvider
		}
		return strings.ToLower(leftEntry.Spec) < strings.ToLower(rightEntry.Spec)
	})
	state.selected = 0
	state.selectSpec(selectedSpec)
}

func (state *modelSelectorState) selectSpec(spec string) {
	for index, entryIndex := range state.visible {
		if state.entries[entryIndex].Spec == spec {
			state.selected = index
			return
		}
	}
}

func validModelSelectorSpec(value string) string {
	if value == "" || strings.TrimSpace(value) != value || utf8.RuneCountInString(value) > 1024 || hasModelSelectorControl(value) {
		return ""
	}
	provider, model, found := strings.Cut(value, ":")
	if !found || provider == "" || model == "" || strings.TrimSpace(model) != model {
		return ""
	}
	for index, character := range provider {
		if character >= 'a' && character <= 'z' || index > 0 && (character == '_' || character >= '0' && character <= '9') {
			continue
		}
		return ""
	}
	return value
}

func hasModelSelectorControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) || character == unicode.ReplacementChar {
			return true
		}
	}
	return false
}

func modelSelectorDisplayName(spec string) string {
	_, model, _ := strings.Cut(spec, ":")
	model = strings.TrimPrefix(model, "accounts/fireworks/models/")
	parts := strings.FieldsFunc(model, func(character rune) bool {
		return character == '-' || character == '_' || character == '/'
	})
	for index, part := range parts {
		if part == "" {
			continue
		}
		characters := []rune(part)
		parts[index] = string(unicode.ToUpper(characters[0])) + string(characters[1:])
	}
	if len(parts) == 0 {
		return truncateModelSelectorLabel(spec)
	}
	return truncateModelSelectorLabel(strings.Join(parts, " "))
}

func truncateModelSelectorLabel(value string) string {
	characters := []rune(value)
	if len(characters) <= 160 {
		return value
	}
	return string(characters[:159]) + unicodeUIGlyphs.Ellipsis
}

func sortedModelSelectorProviders(entries []modelSelectorEntry) []string {
	seen := map[string]bool{}
	providers := make([]string, 0)
	for _, entry := range entries {
		if entry.Provider != "" && !seen[entry.Provider] {
			seen[entry.Provider] = true
			providers = append(providers, entry.Provider)
		}
	}
	sort.Strings(providers)
	return providers
}
