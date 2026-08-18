package dacode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/daaskuser"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	askUserAnsweredSummary = "User answered"
	askUserFailedSummary   = "Question failed"
	askUserMissingAnswer   = "Please provide an answer to all questions before continuing."
	askUserOtherChoice     = "Other (type your answer)"
)

var askUserTrailingAnnotation = regexp.MustCompile(`(?i)\s*(?:[-–—]\s*(?:optional|required)|\((?:optional|required)[.!?]?\)|\[(?:optional|required)[.!?]?\])[.!?]*\s*$`)

type askUserQuestionState struct {
	question  daaskuser.Question
	input     textarea.Model
	selected  int
	confirmed bool
}

type askUserState struct {
	request   daaskuser.AskRequest
	questions []askUserQuestionState
	current   int
	ready     bool
	warning   string
}

type askUserCancelledMsg struct {
	err        error
	generation uint64
}

func newAskUserState(request daaskuser.AskRequest) *askUserState {
	state := &askUserState{request: request, questions: make([]askUserQuestionState, len(request.Questions))}
	for index, question := range request.Questions {
		input := textarea.New()
		input.Placeholder = "Type your answer"
		input.Prompt = "> "
		input.ShowLineNumbers = false
		input.CharLimit = 0
		input.SetHeight(1)
		input.MaxHeight = 6
		input.FocusedStyle.Base = lipgloss.NewStyle().Foreground(colorBody)
		input.FocusedStyle.CursorLine = lipgloss.NewStyle()
		input.FocusedStyle.Text = lipgloss.NewStyle().Foreground(colorBody)
		input.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
		input.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colorMuted)
		input.BlurredStyle = input.FocusedStyle
		input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "shift+enter"))
		state.questions[index] = askUserQuestionState{question: question, input: input}
	}
	state.focusCurrent()
	return state
}

func (state *askUserState) focusCurrent() {
	for index := range state.questions {
		state.questions[index].input.Blur()
	}
	if input := state.activeInput(); input != nil {
		input.Focus()
	}
}

func (state *askUserState) activeInput() *textarea.Model {
	if state == nil || state.current < 0 || state.current >= len(state.questions) {
		return nil
	}
	question := &state.questions[state.current]
	if question.question.Type == daaskuser.QuestionText || question.selected == len(question.question.Choices) {
		return &question.input
	}
	return nil
}

func (state *askUserState) currentAnswer(index int) string {
	if index < 0 || index >= len(state.questions) {
		return ""
	}
	question := &state.questions[index]
	if question.question.Type == daaskuser.QuestionText || question.selected == len(question.question.Choices) {
		return question.input.Value()
	}
	if question.selected >= 0 && question.selected < len(question.question.Choices) {
		return question.question.Choices[question.selected].Value
	}
	return ""
}

func (state *askUserState) answers() []string {
	answers := make([]string, len(state.questions))
	for index := range state.questions {
		answers[index] = state.currentAnswer(index)
	}
	return answers
}

func (state *askUserState) setCurrent(index int) {
	if index < 0 || index >= len(state.questions) {
		return
	}
	state.current = index
	state.warning = ""
	state.focusCurrent()
}

func (state *askUserState) resize(width int) {
	for index := range state.questions {
		state.questions[index].input.SetWidth(max(width-8, 12))
		state.questions[index].input.SetHeight(composerContentHeight(state.questions[index].input.Value(), max(width-12, 1)))
	}
}

func decodeAskUserRequest(interrupt dagent.Interrupt) (daaskuser.AskRequest, error) {
	request, ok := dagent.InterruptAs[daaskuser.AskRequest](interrupt)
	if !ok {
		return daaskuser.AskRequest{}, errors.New("invalid ask_user request payload")
	}
	if interrupt.ID != daaskuser.InterruptID || request.Type != daaskuser.ToolName {
		return daaskuser.AskRequest{}, errors.New("invalid ask_user request type")
	}
	if strings.TrimSpace(request.ToolCallID) == "" {
		return daaskuser.AskRequest{}, errors.New("ask_user request has no tool call ID")
	}
	if err := daaskuser.ValidateQuestions(request.Questions); err != nil {
		return daaskuser.AskRequest{}, err
	}
	return request, nil
}

func isAskUserInterrupt(interrupt dagent.Interrupt) bool {
	if interrupt.ID == daaskuser.InterruptID {
		return true
	}
	request, ok := dagent.InterruptAs[daaskuser.AskRequest](interrupt)
	return ok && request.Type == daaskuser.ToolName
}

func isAskUserTool(name string) bool { return name == daaskuser.ToolName }

func (model *tuiModel) presentAskUser(interrupt dagent.Interrupt) error {
	request, err := decodeAskUserRequest(interrupt)
	if err != nil {
		return err
	}
	if model.askUser != nil && model.askUser.request.ToolCallID == request.ToolCallID {
		return nil
	}
	model.askUser = newAskUserState(request)
	model.status = "Waiting for your answer"
	model.relayout()
	return nil
}

func (model *tuiModel) handleAskUserKey(message tea.KeyMsg) (tea.Cmd, bool) {
	state := model.askUser
	if state == nil {
		return nil, false
	}
	if !state.ready {
		if message.String() == "esc" {
			return model.cancelAskUser(), true
		}
		return nil, true
	}
	question := &state.questions[state.current]
	switch message.String() {
	case "esc":
		return model.cancelAskUser(), true
	case "tab":
		if state.current < len(state.questions)-1 {
			state.setCurrent(state.current + 1)
			model.relayout()
			model.refreshTranscript()
		}
		return nil, true
	case "shift+tab":
		if state.current > 0 {
			state.setCurrent(state.current - 1)
			model.relayout()
			model.refreshTranscript()
		}
		return nil, true
	case "ctrl+x":
		if state.activeInput() != nil {
			return model.openAskUserEditor(), true
		}
		return nil, true
	case "ctrl+j", "shift+enter":
		if input := state.activeInput(); input != nil {
			input.InsertString("\n")
			model.relayout()
			model.refreshTranscript()
		}
		return nil, true
	case "enter":
		if question.question.Type == daaskuser.QuestionMultipleChoice && question.selected == len(question.question.Choices) && state.activeInput() == nil {
			state.focusCurrent()
			return nil, true
		}
		return model.confirmAskUserAnswer(), true
	}

	if question.question.Type == daaskuser.QuestionMultipleChoice {
		otherSelected := question.selected == len(question.question.Choices)
		if !otherSelected {
			switch message.String() {
			case "up", "k":
				question.selected = max(question.selected-1, 0)
				state.focusCurrent()
				model.refreshTranscript()
				return nil, true
			case "down", "j":
				question.selected = min(question.selected+1, len(question.question.Choices))
				state.focusCurrent()
				model.relayout()
				model.refreshTranscript()
				return nil, true
			default:
				return nil, true
			}
		}
		if message.String() == "up" && question.input.Line() == 0 {
			question.selected = max(len(question.question.Choices)-1, 0)
			state.focusCurrent()
			model.relayout()
			model.refreshTranscript()
			return nil, true
		}
	}
	return nil, false
}

func (model *tuiModel) confirmAskUserAnswer() tea.Cmd {
	state := model.askUser
	if state == nil || !state.ready {
		return nil
	}
	current := &state.questions[state.current]
	answer := state.currentAnswer(state.current)
	if current.question.IsRequired() && strings.TrimSpace(answer) == "" {
		state.warning = askUserMissingAnswer
		model.relayout()
		model.refreshTranscript()
		return nil
	}
	current.confirmed = true
	state.warning = ""
	for index := state.current + 1; index < len(state.questions); index++ {
		if !state.questions[index].confirmed {
			state.setCurrent(index)
			model.relayout()
			model.refreshTranscript()
			return nil
		}
	}
	answers := state.answers()
	for index, question := range state.questions {
		if question.question.IsRequired() && strings.TrimSpace(answers[index]) == "" {
			state.questions[index].confirmed = false
			state.setCurrent(index)
			state.warning = askUserMissingAnswer
			model.relayout()
			model.refreshTranscript()
			return nil
		}
	}
	return model.answerAskUser(answers)
}

func (model *tuiModel) answerAskUser(answers []string) tea.Cmd {
	state := model.askUser
	if state == nil {
		return nil
	}
	if index, ok := model.toolItems[state.request.ToolCallID]; ok {
		item := &model.items[index]
		item.text = askUserAnsweredSummary
		item.done = true
		item.failed = false
		item.lifecycle = toolSuccess
	}
	model.askUser = nil
	model.editorAskUserCallID = ""
	model.currentAssistant = -1
	model.status = "Sending answers"
	model.relayout()
	model.refreshTranscript()
	portableAnswers := make([]any, len(answers))
	for index, answer := range answers {
		portableAnswers[index] = answer
	}
	return model.startStream(dagent.Input{
		Config:          dacheckpoint.Config{ThreadID: model.threadID},
		Resume:          map[string]any{"status": string(daaskuser.AnswerAnswered), "answers": portableAnswers},
		SkipValueEvents: true,
	})
}

func (model *tuiModel) cancelAskUser() tea.Cmd {
	state := model.askUser
	if state == nil {
		return nil
	}
	if index, ok := model.toolItems[state.request.ToolCallID]; ok {
		item := &model.items[index]
		item.text = ""
		item.done = true
		item.failed = true
		item.lifecycle = toolSkipped
	}
	model.askUser = nil
	model.editorAskUserCallID = ""
	model.status = "Cancelling question"
	model.appendItem(transcriptItem{kind: itemNotice, text: "Question dismissed."})
	model.relayout()
	model.refreshTranscript()
	return cancelAskUserRunGeneration(model.runner, model.threadID, model.operationGeneration)
}

func cancelAskUserRun(runner agentRunner, threadID string) tea.Cmd {
	return cancelAskUserRunGeneration(runner, threadID, 0)
}

func cancelAskUserRunGeneration(runner agentRunner, threadID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return askUserCancelledMsg{err: runner.Cancel(ctx, threadID), generation: generation}
	}
}

func (model *tuiModel) openAskUserEditor() tea.Cmd {
	state := model.askUser
	if state == nil || state.activeInput() == nil {
		return nil
	}
	command, err := model.editDraft(state.activeInput().Value())
	if err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "External editor failed. Check $VISUAL/$EDITOR."})
		model.refreshTranscript()
		return nil
	}
	model.editorAskUserCallID = state.request.ToolCallID
	model.editorAskUserQuestion = state.current
	return command
}

func (model *tuiModel) finishAskUserEditor(message editorFinishedMsg) bool {
	if model.editorAskUserCallID == "" {
		return false
	}
	callID, questionIndex := model.editorAskUserCallID, model.editorAskUserQuestion
	model.editorAskUserCallID = ""
	if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "External editor failed. Check $VISUAL/$EDITOR."})
		model.refreshTranscript()
		return true
	}
	state := model.askUser
	if message.cancelled || state == nil || state.request.ToolCallID != callID || questionIndex < 0 || questionIndex >= len(state.questions) {
		return true
	}
	state.questions[questionIndex].input.SetValue(message.text)
	state.questions[questionIndex].input.CursorEnd()
	state.setCurrent(questionIndex)
	model.relayout()
	model.refreshTranscript()
	return true
}

func (model *tuiModel) renderAskUser() string {
	state := model.askUser
	if state == nil {
		return ""
	}
	count := len(state.questions)
	title := "Agent has a question for you"
	if count != 1 {
		title = fmt.Sprintf("Agent has %d Questions for you", count)
	}
	lines := []string{lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render(model.glyphs.Cursor + " " + title), ""}
	for index := range state.questions {
		question := &state.questions[index]
		marker := "  "
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if index == state.current {
			marker = model.glyphs.Cursor + " "
			style = lipgloss.NewStyle().Foreground(colorBody)
		}
		prefix := ""
		if count > 1 {
			prefix = fmt.Sprintf("%d. ", index+1)
		}
		questionText := askUserTrailingAnnotation.ReplaceAllString(strings.TrimSpace(question.question.Question), "")
		suffix := ""
		if question.question.IsRequired() {
			suffix = " (required)"
		}
		lines = append(lines, style.Render(marker+prefix+unicodesecurity.RenderTerminalSafe(questionText)+suffix))
		if question.question.Type == daaskuser.QuestionMultipleChoice {
			for choiceIndex, choice := range question.question.Choices {
				cursor := "  " + model.glyphs.CircleEmpty + " "
				choiceStyle := lipgloss.NewStyle().Foreground(colorMuted)
				if choiceIndex == question.selected {
					cursor = "  " + model.glyphs.CircleFilled + " "
					choiceStyle = lipgloss.NewStyle().Foreground(colorPrimary).Bold(index == state.current)
				}
				lines = append(lines, choiceStyle.Render(cursor+unicodesecurity.RenderTerminalSafe(choice.Value)))
			}
			otherIndex := len(question.question.Choices)
			cursor := "  " + model.glyphs.CircleEmpty + " "
			otherStyle := lipgloss.NewStyle().Foreground(colorMuted)
			if question.selected == otherIndex {
				cursor = "  " + model.glyphs.CircleFilled + " "
				otherStyle = lipgloss.NewStyle().Foreground(colorPrimary).Bold(index == state.current)
			}
			lines = append(lines, otherStyle.Render(cursor+askUserOtherChoice))
			if question.selected == otherIndex {
				lines = append(lines, question.input.View())
			}
		} else {
			lines = append(lines, question.input.View())
		}
		if index != len(state.questions)-1 {
			lines = append(lines, "")
		}
	}
	if state.warning != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorWarning).Render(state.warning))
	}
	separator := "  " + model.glyphs.Bullet + "  "
	help := model.glyphs.ArrowUp + "/" + model.glyphs.ArrowDown + " Select" + separator + "Enter to continue" + separator + "Ctrl+J newline"
	if state.activeInput() != nil {
		editor := model.externalEditorName
		if editor == "" {
			editor = "external editor"
		}
		help += separator + "Ctrl+X edit in " + unicodesecurity.RenderTerminalSafe(editor)
	}
	if count > 1 {
		help += separator + "Tab/Shift+Tab switch question"
	}
	help += separator + "Esc to cancel"
	if !state.ready {
		help = "Preparing question" + model.glyphs.Ellipsis + separator + "Esc to cancel"
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(help))
	return lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorWarning).
		Padding(0, 1).Width(max(model.width-2, 1)).Render(strings.Join(lines, "\n"))
}

func askUserAuditResult(item transcriptItem) string {
	if item.failed {
		return askUserFailedSummary
	}
	return askUserAnsweredSummary
}
