package dacode

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago"
)

func TestGoalReviewNavigationEditRejectAndCancel(t *testing.T) {
	review := newGoalReview("ship it", "- tests pass\n- behavior works", false)
	review.resize(80)

	decision, _ := review.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	if decision != nil || review.mode != goalReviewEdit || review.input.Value() != review.proposal.Criteria {
		t.Fatalf("edit state = %#v, decision = %#v", review, decision)
	}
	review.setEditedValue("   ")
	decision, _ = review.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if decision != nil || !strings.Contains(review.warning, "criteria") {
		t.Fatalf("blank edit decision = %#v, warning = %q", decision, review.warning)
	}
	review.setEditedValue("- focused test passes")
	decision, _ = review.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if decision == nil || decision.kind != goalReviewEdited || decision.criteria != "- focused test passes" {
		t.Fatalf("edited decision = %#v", decision)
	}

	review = newGoalReview("ship it", "- tests pass\n- behavior works", true)
	decision, _ = review.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if decision != nil || review.mode != goalReviewReject {
		t.Fatalf("reject mode = %d, decision = %#v", review.mode, decision)
	}
	review.setEditedValue("Keep the existing API")
	decision, _ = review.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if decision == nil || decision.kind != goalReviewRejected || decision.feedback != "Keep the existing API" {
		t.Fatalf("rejected decision = %#v", decision)
	}

	review = newGoalReview("ship it", "- tests pass\n- behavior works", false)
	decision, _ = review.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if decision == nil || decision.kind != goalReviewCancelled {
		t.Fatalf("cancel decision = %#v", decision)
	}
}

func TestGoalReviewBoundsAndTerminalSafeRendering(t *testing.T) {
	review := newGoalReview("ship \x1b]52;c;forged\a", "- safe\n- bounded", false)
	review.startInput(goalReviewReject, strings.Repeat("é", maxGoalReviewCharacters+20))
	review.clampInput()
	if got := len([]rune(review.input.Value())); got != maxGoalReviewCharacters {
		t.Fatalf("review input runes = %d", got)
	}
	rendered := renderGoalReview(review, 80, "vim")
	if strings.Contains(rendered, "\x1b]52;c;forged") || !strings.Contains(rendered, "Ctrl+X vim") {
		t.Fatalf("unsafe or incomplete render: %q", rendered)
	}
}

func TestFormatRubricSnapshotIncludesLatestCriterionResults(t *testing.T) {
	text := formatRubricSnapshot(dago.RubricSnapshot{
		Criteria: "- tests pass",
		Status:   dago.RubricNeedsRevision,
		Evaluations: []dago.RubricEvaluation{{Explanation: "one gap", Criteria: []dago.RubricCriterionEvaluation{
			{Name: "unit tests", Passed: true},
			{Name: "browser", Passed: false, Gap: "missing restart case"},
		}}},
	})
	for _, want := range []string{"Latest result: needs_revision", "✓ unit tests", "✗ browser: missing restart case"} {
		if !strings.Contains(text, want) {
			t.Fatalf("format missing %q: %s", want, text)
		}
	}
}
