package dacode

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type onboardingStep uint8

const (
	onboardingName onboardingStep = iota
	onboardingDependencies
	onboardingModel
	onboardingWebSearch
	onboardingGoalCriteria
	onboardingComplete
	onboardingSkipped
)

type onboardingDependency struct {
	Name        string
	Category    string
	Description string
	Installed   bool
}

type onboardingResult struct {
	Name                   string
	Model                  string
	ConfigureWebSearch     bool
	AutoAcceptGoalCriteria bool
	Skipped                bool
}

type onboardingState struct {
	step         onboardingStep
	nameInput    string
	dependencies []onboardingDependency
	dependencyAt int
	model        *modelSelectorState
	choice       int
	result       onboardingResult
}

func newOnboardingState(dependencies []onboardingDependency, models []modelSelectorEntry, currentModel string) *onboardingState {
	return &onboardingState{
		step:         onboardingName,
		dependencies: normalizeOnboardingDependencies(dependencies),
		model:        newModelSelector(models, currentModel, ""),
	}
}

func normalizeOnboardingDependencies(dependencies []onboardingDependency) []onboardingDependency {
	if len(dependencies) > 256 {
		dependencies = dependencies[:256]
	}
	result := make([]onboardingDependency, 0, len(dependencies))
	seen := map[string]bool{}
	for _, dependency := range dependencies {
		dependency.Name = strings.TrimSpace(dependency.Name)
		dependency.Category = strings.TrimSpace(dependency.Category)
		dependency.Description = strings.TrimSpace(dependency.Description)
		if dependency.Name == "" || utf8.RuneCountInString(dependency.Name) > 128 || hasModelSelectorControl(dependency.Name) || seen[dependency.Name] {
			continue
		}
		seen[dependency.Name] = true
		if dependency.Category == "" || utf8.RuneCountInString(dependency.Category) > 80 || hasModelSelectorControl(dependency.Category) {
			dependency.Category = "Other"
		}
		if utf8.RuneCountInString(dependency.Description) > 240 || hasModelSelectorControl(dependency.Description) {
			dependency.Description = ""
		}
		result = append(result, dependency)
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Installed != result[right].Installed {
			return !result[left].Installed
		}
		if result[left].Category != result[right].Category {
			return result[left].Category < result[right].Category
		}
		return result[left].Name < result[right].Name
	})
	return result
}

func (state *onboardingState) handleKey(key string, pageHeight int) {
	if state == nil || state.done() {
		return
	}
	switch state.step {
	case onboardingName:
		state.handleNameKey(key)
	case onboardingDependencies:
		state.handleDependenciesKey(key, pageHeight)
	case onboardingModel:
		state.handleModelKey(key, pageHeight)
	case onboardingWebSearch:
		state.handleChoiceKey(key, func(selected int) {
			state.result.ConfigureWebSearch = selected == 1
			state.choice = 0
			state.step = onboardingGoalCriteria
		})
	case onboardingGoalCriteria:
		state.handleChoiceKey(key, func(selected int) {
			state.result.AutoAcceptGoalCriteria = selected == 1
			state.step = onboardingComplete
		})
	}
}

func (state *onboardingState) handleNameKey(key string) {
	switch key {
	case "esc", "ctrl+c":
		state.result.Skipped = true
		state.step = onboardingSkipped
	case "enter":
		state.result.Name = normalizeOnboardingName(state.nameInput)
		state.step = onboardingDependencies
	case "backspace":
		characters := []rune(state.nameInput)
		if len(characters) > 0 {
			state.nameInput = string(characters[:len(characters)-1])
		}
	default:
		characters := []rune(key)
		if len(characters) == 1 && unicode.IsPrint(characters[0]) && utf8.RuneCountInString(state.nameInput) < 80 {
			state.nameInput += key
		}
	}
}

func (state *onboardingState) handleDependenciesKey(key string, pageHeight int) {
	switch key {
	case "esc", "ctrl+c":
		state.result.Skipped = true
		state.step = onboardingSkipped
	case "enter":
		state.step = onboardingModel
	case "up":
		state.dependencyAt = max(state.dependencyAt-1, 0)
	case "down":
		state.dependencyAt = min(state.dependencyAt+1, max(len(state.dependencies)-1, 0))
	case "pgup":
		state.dependencyAt = max(state.dependencyAt-max(pageHeight, 1), 0)
	case "pgdown":
		state.dependencyAt = min(state.dependencyAt+max(pageHeight, 1), max(len(state.dependencies)-1, 0))
	}
}

func (state *onboardingState) handleModelKey(key string, pageHeight int) {
	result := state.model.handleKey(key, pageHeight)
	if result.Action == modelSelectorCancel {
		state.result.Skipped = true
		state.step = onboardingSkipped
		return
	}
	if result.Action == modelSelectorSelect && result.Err == nil {
		state.result.Model = result.Spec
		state.choice = 0
		state.step = onboardingWebSearch
	}
}

func (state *onboardingState) handleChoiceKey(key string, finish func(int)) {
	switch key {
	case "up", "shift+tab":
		state.choice = wrapIndex(state.choice-1, 2)
	case "down", "tab":
		state.choice = wrapIndex(state.choice+1, 2)
	case "esc", "ctrl+c":
		finish(0)
	case "enter":
		finish(state.choice)
	}
}

func (state *onboardingState) done() bool {
	return state == nil || state.step == onboardingComplete || state.step == onboardingSkipped
}

func (state *onboardingState) value() (onboardingResult, bool) {
	if state == nil || !state.done() {
		return onboardingResult{}, false
	}
	return state.result, true
}

func normalizeOnboardingName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || utf8.RuneCountInString(value) > 80 || hasModelSelectorControl(value) {
		return ""
	}
	allLower := true
	for _, character := range value {
		if unicode.IsLetter(character) && !unicode.IsLower(character) {
			allLower = false
			break
		}
	}
	if allLower {
		return strings.Title(value) //nolint:staticcheck // pinned onboarding title-cases all-lower names.
	}
	return value
}

func renderOnboarding(state *onboardingState, width, height int, glyphs uiGlyphs) string {
	if state == nil || state.done() {
		return ""
	}
	if state.step == onboardingModel {
		return renderModelSelector(state.model, width, height, glyphs)
	}
	contentWidth := min(max(width-12, 40), 72)
	var lines []string
	switch state.step {
	case onboardingName:
		lines = []string{
			lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Welcome to dacode"),
			"What should the agent call you?",
			lipgloss.NewStyle().Foreground(colorPrimary).Render(unicodesecurity.RenderTerminalSafe(state.nameInput) + glyphs.Cursor),
			"",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Enter continue  " + glyphs.Bullet + "  Esc skip setup"),
		}
	case onboardingDependencies:
		lines = renderOnboardingDependencies(state, contentWidth, height, glyphs)
	case onboardingWebSearch:
		lines = renderOnboardingChoice("Enable web search?", "A web-search key can be added now or later with /auth.",
			[]string{"Skip for now", "Configure web search"}, state.choice, glyphs)
	case onboardingGoalCriteria:
		lines = renderOnboardingChoice("How should Auto mode handle goal criteria?", "New goals draft acceptance criteria before work starts.",
			[]string{"Review before applying (recommended)", "Apply automatically in Auto mode"}, state.choice, glyphs)
	}
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, contentWidth+8), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

func renderOnboardingDependencies(state *onboardingState, width, height int, glyphs uiGlyphs) []string {
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Optional integrations"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("Review what is installed. You can add integrations later with /install."),
		"",
	}
	if len(state.dependencies) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No optional integrations were detected."))
	} else {
		visible := min(max(height-10, 3), 16)
		start := max(state.dependencyAt-visible/2, 0)
		start = min(start, max(len(state.dependencies)-visible, 0))
		previousCategory := ""
		for index := start; index < min(start+visible, len(state.dependencies)); index++ {
			dependency := state.dependencies[index]
			if dependency.Category != previousCategory {
				lines = append(lines, lipgloss.NewStyle().Foreground(colorSecondary).Bold(true).Render(unicodesecurity.RenderTerminalSafe(dependency.Category)))
				previousCategory = dependency.Category
			}
			status := glyphs.Error + " not installed"
			if dependency.Installed {
				status = glyphs.Checkmark + " installed"
			}
			label := dependency.Name + " " + glyphs.Dash + " " + status
			if dependency.Description != "" {
				label += " " + glyphs.Dash + " " + dependency.Description
			}
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(colorBody).Width(width)
			if index == state.dependencyAt {
				prefix = glyphs.Cursor + " "
				style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
			}
			lines = append(lines, style.Render(prefix+unicodesecurity.RenderTerminalSafe(label)))
		}
	}
	return append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render("Enter continue  "+glyphs.Bullet+"  Esc skip setup"))
}

func renderOnboardingChoice(title, description string, choices []string, selected int, glyphs uiGlyphs) []string {
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(title),
		lipgloss.NewStyle().Foreground(colorMuted).Render(description),
		"",
	}
	for index, choice := range choices {
		prefix := "  "
		style := lipgloss.NewStyle().Foreground(colorBody)
		if index == selected {
			prefix = glyphs.Cursor + " "
			style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true).PaddingRight(1)
		}
		lines = append(lines, style.Render(prefix+choice))
	}
	return append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(glyphs.ArrowUp+"/"+glyphs.ArrowDown+" or Tab switch  "+glyphs.Bullet+"  Enter select  "+glyphs.Bullet+"  Esc safe default"))
}
