package dacode

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	maxGoalReviewCharacters = 16_000
	maxGoalReviewBytes      = 64_000
)

type goalReviewMode uint8

const (
	goalReviewMenu goalReviewMode = iota
	goalReviewEdit
	goalReviewReject
)

type goalReviewDecisionKind string

const (
	goalReviewAccepted  goalReviewDecisionKind = "accepted"
	goalReviewEdited    goalReviewDecisionKind = "edited"
	goalReviewRejected  goalReviewDecisionKind = "rejected"
	goalReviewCancelled goalReviewDecisionKind = "cancelled"
)

type goalReviewDecision struct {
	kind     goalReviewDecisionKind
	criteria string
	feedback string
}

type goalReviewState struct {
	proposal  dagoal.CriteriaProposal
	amendment bool
	selected  int
	mode      goalReviewMode
	input     textarea.Model
	warning   string
}

func newGoalReview(objective, criteria string, amendment bool) *goalReviewState {
	objective = strings.TrimSpace(objective)
	criteria = strings.TrimSpace(criteria)
	if objective == "" || criteria == "" {
		panic("goal review requires objective and criteria")
	}
	input := textarea.New()
	input.Placeholder = "Edit acceptance criteria"
	input.Prompt = "> "
	input.CharLimit = maxGoalReviewCharacters
	input.ShowLineNumbers = false
	input.SetHeight(5)
	input.MaxHeight = 8
	input.KeyMap.InsertNewline.SetKeys("ctrl+j")
	return &goalReviewState{
		proposal:  dagoal.CriteriaProposal{Objective: objective, Criteria: criteria},
		amendment: amendment,
		input:     input,
	}
}

func (state *goalReviewState) resize(width int) {
	state.input.SetWidth(max(width-6, 12))
}

func (state *goalReviewState) handleKey(message tea.KeyMsg) (*goalReviewDecision, tea.Cmd) {
	if state.mode != goalReviewMenu {
		switch message.String() {
		case "esc":
			state.mode = goalReviewMenu
			state.warning = ""
			state.input.Blur()
			return nil, nil
		case "enter":
			value := boundedGoalReviewText(state.input.Value())
			if value == "" {
				if state.mode == goalReviewEdit {
					state.warning = "Enter some criteria, or press Esc to go back."
				} else {
					state.warning = "Enter some feedback, or press Esc to go back."
				}
				return nil, nil
			}
			if state.mode == goalReviewEdit {
				return &goalReviewDecision{kind: goalReviewEdited, criteria: value}, nil
			}
			return &goalReviewDecision{kind: goalReviewRejected, feedback: value}, nil
		case "ctrl+x":
			return nil, nil
		}
		var command tea.Cmd
		state.input, command = state.input.Update(message)
		state.clampInput()
		state.warning = ""
		return nil, command
	}

	switch message.String() {
	case "up", "k":
		state.selected = (state.selected + 3) % 4
	case "down", "j":
		state.selected = (state.selected + 1) % 4
	case "1", "y", "Y":
		return &goalReviewDecision{kind: goalReviewAccepted, criteria: state.proposal.Criteria}, nil
	case "2", "e", "E":
		state.startInput(goalReviewEdit, state.proposal.Criteria)
	case "3", "r", "R":
		state.startInput(goalReviewReject, "")
	case "4", "n", "N", "esc":
		return &goalReviewDecision{kind: goalReviewCancelled}, nil
	case "enter":
		switch state.selected {
		case 0:
			return &goalReviewDecision{kind: goalReviewAccepted, criteria: state.proposal.Criteria}, nil
		case 1:
			state.startInput(goalReviewEdit, state.proposal.Criteria)
		case 2:
			state.startInput(goalReviewReject, "")
		default:
			return &goalReviewDecision{kind: goalReviewCancelled}, nil
		}
	}
	return nil, nil
}

func (state *goalReviewState) startInput(mode goalReviewMode, value string) {
	state.mode = mode
	state.warning = ""
	state.input.SetValue(value)
	state.input.CursorEnd()
	state.input.Focus()
}

func (state *goalReviewState) setEditedValue(value string) {
	state.input.SetValue(boundedGoalReviewText(value))
	state.input.CursorEnd()
	state.clampInput()
}

func (state *goalReviewState) clampInput() {
	value := state.input.Value()
	bounded := boundedGoalReviewText(value)
	if bounded != strings.TrimSpace(value) {
		state.input.SetValue(bounded)
		state.input.CursorEnd()
	}
}

func boundedGoalReviewText(value string) string {
	value = strings.TrimSpace(value)
	for len(value) > maxGoalReviewBytes {
		_, size := lastRune(value)
		value = value[:len(value)-size]
	}
	runes := []rune(value)
	if len(runes) > maxGoalReviewCharacters {
		value = string(runes[:maxGoalReviewCharacters])
	}
	return strings.TrimSpace(value)
}

func lastRune(value string) (rune, int) {
	runes := []rune(value)
	if len(runes) == 0 {
		return 0, 0
	}
	last := runes[len(runes)-1]
	return last, len(string(last))
}

func renderGoalReview(state *goalReviewState, width int, editor string) string {
	return renderGoalReviewWithGlyphs(state, width, editor, unicodeUIGlyphs)
}

func renderGoalReviewWithGlyphs(state *goalReviewState, width int, editor string, glyphs uiGlyphs) string {
	title := "Review goal criteria"
	if state.amendment {
		title = "Review goal amendment"
	}
	lines := []string{lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(title)}
	if state.amendment {
		lines = append(lines, "", lipgloss.NewStyle().Bold(true).Render("Proposed objective"), unicodesecurity.RenderTerminalSafe(state.proposal.Objective))
	}
	lines = append(lines, "", lipgloss.NewStyle().Bold(true).Render("Proposed criteria"), unicodesecurity.RenderTerminalSafe(state.proposal.Criteria), "")
	labels := []string{"1. Accept proposed criteria (y)", "2. Edit criteria (e)", "3. Reject with message (r)", "4. Cancel (n)"}
	for index, label := range labels {
		prefix := "  "
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if state.mode == goalReviewMenu && index == state.selected {
			prefix = glyphs.Cursor + " "
			style = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
		}
		lines = append(lines, style.Render(prefix+label))
	}
	separator := "  " + glyphs.Bullet + "  "
	hint := glyphs.ArrowUp + "/" + glyphs.ArrowDown + " navigate" + separator + "Enter select" + separator + "y/e/r/n quick keys" + separator + "Esc cancel"
	if state.mode != goalReviewMenu {
		lines = append(lines, "", state.input.View())
		action := "save edits"
		if state.mode == goalReviewReject {
			action = "regenerate"
		}
		if editor == "" {
			editor = "external editor"
		}
		hint = "Enter " + action + separator + "Ctrl+J newline" + separator + "Ctrl+X " + editor + separator + "Esc back"
	}
	if state.warning != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(state.warning))
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(hint))
	return lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorPrimary).
		Padding(0, 1).Width(max(width, 1)).Render(strings.Join(lines, "\n"))
}

func formatRubricSnapshot(snapshot dago.RubricSnapshot) string {
	return formatRubricSnapshotWithGlyphs(snapshot, unicodeUIGlyphs)
}

func formatRubricSnapshotWithGlyphs(snapshot dago.RubricSnapshot, glyphs uiGlyphs) string {
	if snapshot.Criteria == "" {
		return "No rubric set."
	}
	lines := []string{"Rubric:", snapshot.Criteria}
	if snapshot.Status != "" {
		lines = append(lines, "", "Latest result: "+string(snapshot.Status))
	}
	if len(snapshot.Evaluations) > 0 {
		latest := snapshot.Evaluations[len(snapshot.Evaluations)-1]
		if strings.TrimSpace(latest.Explanation) != "" {
			lines = append(lines, latest.Explanation)
		}
		for _, criterion := range latest.Criteria {
			mark := glyphs.Checkmark
			if !criterion.Passed {
				mark = glyphs.Error
			}
			line := mark + " " + criterion.Name
			if gap := strings.TrimSpace(criterion.Gap); gap != "" {
				line += ": " + gap
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
