package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daworkflow"
)

var (
	colorBackground = lipgloss.Color("#11121D")
	colorSurface    = lipgloss.Color("#1A1B2E")
	colorPanel      = lipgloss.Color("#25283B")
	colorBody       = lipgloss.Color("#C0CAF5")
	colorPrimary    = lipgloss.Color("#7AA2F7")
	colorSecondary  = lipgloss.Color("#BB9AF7")
	colorSuccess    = lipgloss.Color("#9ECE6A")
	colorWarning    = lipgloss.Color("#EB8B46")
	colorError      = lipgloss.Color("#F7768E")
	colorMuted      = lipgloss.Color("#545C7E")
)

type itemKind int

const (
	itemUser itemKind = iota
	itemAssistant
	itemTool
	itemNotice
	itemError
)

type transcriptItem struct {
	kind   itemKind
	text   string
	callID string
	name   string
	args   string
	done   bool
	failed bool
}

type approvalState struct {
	requests []dagent.ApprovalRequest
	ready    bool
}

type streamEventMsg struct {
	event dagent.Event
}

type streamDoneMsg struct {
	result dagent.Result
	err    error
}

type cancelDoneMsg struct {
	err error
}

type reviewDoneMsg struct {
	result approvalReviewResult
	err    error
}

type initialPromptMsg string

type goalLoadedMsg struct {
	goal *dagoal.Goal
	err  error
}

type goalActionMsg struct {
	action       string
	goal         *dagoal.Goal
	cleared      bool
	continueWork bool
	err          error
}

type tuiModel struct {
	ctx        context.Context
	runner     agentRunner
	workingDir string
	modelName  string
	threadID   string
	yolo       bool
	autoReview bool
	initial    string

	width  int
	height int
	ready  bool

	viewport viewport.Model
	composer textarea.Model
	spinner  spinner.Model

	items            []transcriptItem
	toolItems        map[string]int
	currentAssistant int
	stream           eventStream
	turnCancel       context.CancelFunc
	running          bool
	cancelling       bool
	approval         *approvalState
	sessionPicker    *sessionPickerState
	status           string
	totalTokens      int
	goal             *dagoal.Goal
	workflowPanel    *workflowPanelState
	pendingWorkflows []daworkflow.Status
}

func newTUIModel(ctx context.Context, runner agentRunner, workingDir, modelName, threadID string, yolo, autoReview bool, initial string) *tuiModel {
	composer := textarea.New()
	composer.Placeholder = "What would you like to build?"
	composer.Prompt = "> "
	composer.ShowLineNumbers = false
	composer.CharLimit = 0
	composer.SetHeight(1)
	composer.MaxHeight = 8
	composer.FocusedStyle.Base = lipgloss.NewStyle().Foreground(colorBody)
	composer.FocusedStyle.CursorLine = lipgloss.NewStyle()
	composer.FocusedStyle.Text = lipgloss.NewStyle().Foreground(colorBody)
	composer.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
	composer.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colorMuted)
	composer.BlurredStyle = composer.FocusedStyle
	composer.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j"))
	composer.Focus()

	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = lipgloss.NewStyle().Foreground(colorPrimary)

	return &tuiModel{
		ctx: ctx, runner: runner, workingDir: workingDir, modelName: modelName,
		threadID: threadID, yolo: yolo, autoReview: autoReview, initial: initial, composer: composer,
		spinner: spin, currentAssistant: -1, toolItems: map[string]int{}, status: "Ready",
	}
}

func (model *tuiModel) Init() tea.Cmd {
	commands := []tea.Cmd{textarea.Blink, model.spinner.Tick, waitForWorkflowCompletion(model.ctx, model.runner)}
	if model.sessionPicker != nil && model.sessionPicker.loading {
		commands = append(commands, listSessions(model.ctx, model.runner))
	} else if model.sessionPicker != nil && model.sessionPicker.resuming && len(model.sessionPicker.sessions) > 0 {
		commands = append(commands, loadSession(model.ctx, model.runner, model.sessionPicker.sessions[0]))
	}
	if model.sessionPicker == nil {
		if strings.TrimSpace(model.initial) != "" {
			commands = append(commands, func() tea.Msg { return initialPromptMsg(model.initial) })
		} else {
			commands = append(commands, loadGoal(model.ctx, model.runner, model.threadID))
		}
	}
	return tea.Batch(commands...)
}

func (model *tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := message.(type) {
	case tea.WindowSizeMsg:
		model.resize(typed.Width, typed.Height)
		return model, nil
	case initialPromptMsg:
		if !model.running {
			return model, model.submitPrompt(string(typed))
		}
	case goalLoadedMsg:
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not restore goal: " + typed.err.Error()})
			model.refreshTranscript()
			return model, nil
		}
		model.goal = typed.goal
		if model.goal != nil && model.goal.Actionable() && !model.running {
			return model, model.startGoalContinuation()
		}
	case goalActionMsg:
		return model.finishGoalAction(typed)
	case workflowStartedMsg:
		return model, model.finishWorkflowStart(typed)
	case workflowsLoadedMsg:
		return model, model.finishWorkflowLoad(typed)
	case workflowCancelledMsg:
		return model, model.finishWorkflowCancel(typed)
	case workflowTickMsg:
		if model.workflowPanel != nil && model.workflowPanel == typed.panel {
			typed.panel.polling = false
			return model, loadWorkflows(model.runner)
		}
		return model, nil
	case workflowCompletedMsg:
		return model, model.finishWorkflowCompletion(typed.status)
	case spinner.TickMsg:
		var command tea.Cmd
		model.spinner, command = model.spinner.Update(typed)
		model.refreshSpinner()
		return model, command
	case streamEventMsg:
		model.applyEvent(typed.event)
		model.refreshTranscript()
		return model, waitForStream(model.ctx, model.stream)
	case streamDoneMsg:
		return model.finishStream(typed)
	case cancelDoneMsg:
		model.cancelling = false
		model.running = false
		model.stream = nil
		model.turnCancel = nil
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not finalize cancellation: " + typed.err.Error()})
			model.status = "Cancellation failed"
		} else {
			model.appendItem(transcriptItem{kind: itemNotice, text: "Operation cancelled."})
			model.status = "Ready"
		}
		model.refreshTranscript()
		return model, nil
	case reviewDoneMsg:
		return model.finishReview(typed)
	case sessionsLoadedMsg:
		model.finishSessionList(typed)
		return model, nil
	case sessionLoadedMsg:
		return model, model.finishSessionLoad(typed)
	case tea.MouseMsg:
		var command tea.Cmd
		model.viewport, command = model.viewport.Update(typed)
		return model, command
	case tea.KeyMsg:
		if command, handled := model.handleKey(typed); handled {
			return model, command
		}
	}

	if !model.running && model.approval == nil {
		var command tea.Cmd
		model.composer, command = model.composer.Update(message)
		model.relayout()
		model.refreshTranscript()
		return model, command
	}
	var command tea.Cmd
	model.viewport, command = model.viewport.Update(message)
	return model, command
}

func (model *tuiModel) handleKey(message tea.KeyMsg) (tea.Cmd, bool) {
	if model.workflowPanel != nil {
		return model.handleWorkflowKey(message)
	}
	if model.sessionPicker != nil {
		return model.handleSessionKey(message)
	}
	switch message.String() {
	case "ctrl+c":
		if model.running && !model.cancelling {
			model.cancelling = true
			model.status = "Cancelling"
			if model.turnCancel != nil {
				model.turnCancel()
			}
			return nil, true
		}
		if strings.TrimSpace(model.composer.Value()) != "" {
			model.composer.Reset()
			model.relayout()
			model.refreshTranscript()
			return nil, true
		}
		return tea.Quit, true
	case "ctrl+d":
		if !model.running && model.composer.Value() == "" {
			return tea.Quit, true
		}
	case "ctrl+j":
		if model.approval == nil && !model.running {
			model.composer.InsertString("\n")
			model.relayout()
			model.refreshTranscript()
			return nil, true
		}
	case "enter":
		if model.approval == nil && !model.running {
			prompt := strings.TrimSpace(model.composer.Value())
			if prompt != "" {
				model.composer.Reset()
				model.relayout()
				if command, ok := model.slashCommand(prompt); ok {
					return command, true
				}
				return model.submitPrompt(prompt), true
			}
		}
	case "y", "Y":
		if model.manualApprovalVisible() && model.approval.ready {
			return model.resolveApproval(true), true
		}
	case "n", "N", "esc":
		if model.manualApprovalVisible() && model.approval.ready {
			return model.resolveApproval(false), true
		}
	case "pgup", "pgdown":
		var command tea.Cmd
		model.viewport, command = model.viewport.Update(message)
		return command, true
	case "ctrl+_":
		model.viewport.LineUp(model.viewport.MouseWheelDelta)
		return nil, true
	case "ctrl+^":
		model.viewport.LineDown(model.viewport.MouseWheelDelta)
		return nil, true
	}
	return nil, false
}

func (model *tuiModel) slashCommand(prompt string) (tea.Cmd, bool) {
	command := strings.TrimSpace(strings.TrimPrefix(prompt, "/"))
	if command == prompt {
		return nil, false
	}
	if command == "goal" || strings.HasPrefix(command, "goal ") {
		return model.goalCommand(strings.TrimSpace(strings.TrimPrefix(command, "goal"))), true
	}
	if command == "workflow" || strings.HasPrefix(command, "workflow ") {
		return model.workflowCommand(strings.TrimSpace(strings.TrimPrefix(command, "workflow"))), true
	}
	switch command {
	case "help":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Commands: /help  /clear  /new  /threads  /model  /goal  /workflow  /workflows  /quit"})
		model.refreshTranscript()
		return nil, true
	case "clear":
		model.items = nil
		model.toolItems = map[string]int{}
		model.currentAssistant = -1
		model.refreshTranscript()
		return nil, true
	case "new":
		threadID, err := newThreadID()
		if err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: err.Error()})
		} else {
			model.threadID = threadID
			model.goal = nil
			model.items = nil
			model.toolItems = map[string]int{}
			model.currentAssistant = -1
			model.status = "New thread"
		}
		model.refreshTranscript()
		return nil, true
	case "threads", "resume":
		model.sessionPicker = &sessionPickerState{loading: true}
		return listSessions(model.ctx, model.runner), true
	case "workflows":
		return model.showWorkflows(), true
	case "model":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Model: openai:" + model.modelName})
		model.refreshTranscript()
		return nil, true
	case "quit", "exit":
		return tea.Quit, true
	default:
		model.appendItem(transcriptItem{kind: itemError, text: "Unknown command: /" + command})
		model.refreshTranscript()
		return nil, true
	}
}

func (model *tuiModel) goalCommand(arguments string) tea.Cmd {
	text := "/goal"
	if arguments != "" {
		text += " " + arguments
	}
	model.appendItem(transcriptItem{kind: itemUser, text: text})
	model.currentAssistant = -1
	model.running = true
	model.status = "Updating goal"
	model.refreshTranscript()

	action := "set"
	request := dagoal.SetRequest{}
	continueWork := false
	switch {
	case arguments == "" || arguments == "show" || arguments == "status":
		action = "show"
	case arguments == "pause":
		action = "pause"
		status := dagoal.StatusPaused
		request.Status = &status
	case arguments == "resume":
		action = "resume"
		status := dagoal.StatusActive
		request.Status = &status
		continueWork = true
	case arguments == "clear":
		action = "clear"
	case strings.HasPrefix(arguments, "budget "):
		action = "budget"
		value := strings.TrimSpace(strings.TrimPrefix(arguments, "budget "))
		if value == "clear" {
			request.Budget = dagoal.ClearBudget()
			break
		}
		budget, err := strconv.ParseInt(value, 10, 64)
		if err != nil || budget <= 0 {
			model.running = false
			model.status = "Ready"
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /goal budget <positive tokens|clear>"})
			model.refreshTranscript()
			return nil
		}
		request.Budget = dagoal.SetBudget(budget)
	default:
		objective := arguments
		status := dagoal.StatusActive
		request.Objective = &objective
		request.Status = &status
		continueWork = true
	}
	return runGoalAction(model.ctx, model.runner, model.threadID, action, request, continueWork)
}

func loadGoal(ctx context.Context, runner agentRunner, threadID string) tea.Cmd {
	return func() tea.Msg {
		goal, err := runner.Goal(ctx, threadID)
		return goalLoadedMsg{goal: goal, err: err}
	}
}

func runGoalAction(ctx context.Context, runner agentRunner, threadID, action string, request dagoal.SetRequest, continueWork bool) tea.Cmd {
	return func() tea.Msg {
		if action == "show" {
			goal, err := runner.Goal(ctx, threadID)
			return goalActionMsg{action: action, goal: goal, err: err}
		}
		if action == "clear" {
			cleared, err := runner.ClearGoal(ctx, threadID)
			return goalActionMsg{action: action, cleared: cleared, err: err}
		}
		goal, err := runner.SetGoal(ctx, threadID, request)
		return goalActionMsg{action: action, goal: goal, continueWork: continueWork, err: err}
	}
}

func (model *tuiModel) finishGoalAction(message goalActionMsg) (tea.Model, tea.Cmd) {
	model.running = false
	if message.err != nil {
		model.status = "Goal error"
		model.appendItem(transcriptItem{kind: itemError, text: message.err.Error()})
		model.refreshTranscript()
		return model, nil
	}
	if message.action == "clear" {
		model.goal = nil
		model.status = "Ready"
		text := "No goal was set."
		if message.cleared {
			text = "Goal cleared."
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: text})
		model.refreshTranscript()
		return model, nil
	}
	model.goal = message.goal
	model.status = "Ready"
	switch message.action {
	case "show", "budget":
		model.appendItem(transcriptItem{kind: itemNotice, text: formatGoal(message.goal)})
	case "pause":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal paused."})
	case "resume":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal resumed."})
	default:
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal set. " + formatGoal(message.goal)})
	}
	model.refreshTranscript()
	if message.continueWork && message.goal != nil && message.goal.Actionable() {
		return model, model.startGoalContinuation()
	}
	return model, nil
}

func formatGoal(goal *dagoal.Goal) string {
	if goal == nil {
		return "No goal set. Usage: /goal <objective>"
	}
	budget := "unbounded"
	if goal.TokenBudget != nil {
		budget = fmt.Sprintf("%d/%d tokens", goal.TokensUsed, *goal.TokenBudget)
	} else if goal.TokensUsed > 0 {
		budget = fmt.Sprintf("%d tokens used", goal.TokensUsed)
	}
	return fmt.Sprintf("%s\nStatus: %s · %s · %ds", goal.Objective, goal.Status, budget, goal.TimeUsedSeconds)
}

func (model *tuiModel) submitPrompt(prompt string) tea.Cmd {
	model.appendItem(transcriptItem{kind: itemUser, text: prompt})
	model.currentAssistant = -1
	return model.startStream(dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: model.threadID},
		Messages: []damessage.Message{damessage.Human(prompt)}, SkipValueEvents: true,
	})
}

func (model *tuiModel) startGoalContinuation() tea.Cmd {
	if model.goal == nil || !model.goal.Actionable() {
		return nil
	}
	model.currentAssistant = -1
	command := model.startStream(dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: model.threadID},
		Messages: []damessage.Message{dagoal.ContinuationMessage(*model.goal)}, SkipValueEvents: true,
	})
	model.status = "Continuing goal"
	return command
}

func (model *tuiModel) startStream(input dagent.Input) tea.Cmd {
	turnContext, cancel := context.WithCancel(model.ctx)
	model.turnCancel = cancel
	model.stream = model.runner.Start(turnContext, input)
	model.running = true
	model.cancelling = false
	model.status = "Thinking"
	model.refreshTranscript()
	return waitForStream(turnContext, model.stream)
}

func waitForStream(ctx context.Context, stream eventStream) tea.Cmd {
	return func() tea.Msg {
		if stream == nil {
			return streamDoneMsg{err: fmt.Errorf("agent stream is unavailable")}
		}
		event, err := stream.Next(ctx)
		if err == nil {
			return streamEventMsg{event: event}
		}
		defer stream.Close()
		if !errors.Is(err, io.EOF) {
			return streamDoneMsg{err: err}
		}
		result, resultErr := stream.Result(ctx)
		return streamDoneMsg{result: result, err: resultErr}
	}
}

func (model *tuiModel) applyEvent(event dagent.Event) {
	switch event.Mode {
	case dagent.EventToken:
		if event.Chunk == nil {
			return
		}
		text := event.Chunk.MessageDelta.TextContent()
		if text != "" {
			model.appendAssistant(text)
			model.status = "Responding"
		} else {
			for _, block := range event.Chunk.MessageDelta.Content {
				if block.Type == damessage.BlockReasoning && block.Reasoning != "" {
					model.status = "Reasoning"
				}
			}
		}
		for _, call := range event.Chunk.MessageDelta.ToolCalls {
			model.addToolCall(call)
		}
		if usage := event.Chunk.MessageDelta.Usage; usage != nil {
			model.totalTokens = usage.TotalTokens
		}
	case dagent.EventUpdate:
		messages, ok := event.Update[dagent.MessagesKey].([]damessage.Message)
		if !ok {
			return
		}
		for _, message := range messages {
			switch message.Role {
			case damessage.RoleAssistant:
				text := message.TextContent()
				if text != "" && (model.currentAssistant < 0 || model.items[model.currentAssistant].text == "") {
					model.appendAssistant(text)
				}
				for _, call := range message.ToolCalls {
					model.addToolCall(call)
				}
				if message.Usage != nil {
					model.totalTokens = message.Usage.TotalTokens
				}
			case damessage.RoleTool:
				model.completeTool(message)
			}
		}
	case dagent.EventToolProgress:
		if event.ToolProgress != nil {
			model.updateToolProgress(*event.ToolProgress)
		}
	case dagent.EventInterrupt:
		if event.Interrupt == nil {
			return
		}
		requests, err := decodeApprovalRequests(event.Interrupt.Value)
		if err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Cannot display approval request: " + err.Error()})
			return
		}
		model.approval = &approvalState{requests: requests}
		model.status = "Waiting for review"
	}
}

func (model *tuiModel) appendAssistant(text string) {
	if model.currentAssistant >= 0 && model.currentAssistant < len(model.items) && model.items[model.currentAssistant].kind == itemAssistant {
		model.items[model.currentAssistant].text += text
		return
	}
	model.appendItem(transcriptItem{kind: itemAssistant, text: text})
	model.currentAssistant = len(model.items) - 1
}

func (model *tuiModel) addToolCall(call damessage.ToolCall) {
	if call.ID == "" {
		return
	}
	if _, exists := model.toolItems[call.ID]; exists {
		return
	}
	model.appendItem(transcriptItem{kind: itemTool, callID: call.ID, name: call.Name, args: compactJSON(call.Arguments)})
	model.toolItems[call.ID] = len(model.items) - 1
	model.currentAssistant = -1
	model.status = "Using " + call.Name
}

func (model *tuiModel) completeTool(message damessage.Message) {
	index, exists := model.toolItems[message.ToolCallID]
	if !exists {
		model.appendItem(transcriptItem{kind: itemTool, callID: message.ToolCallID, name: message.Name})
		index = len(model.items) - 1
		model.toolItems[message.ToolCallID] = index
	}
	item := &model.items[index]
	if message.Name != "" {
		item.name = message.Name
	}
	item.text = message.TextContent()
	item.done = true
	item.failed = message.ToolStatus == damessage.ToolStatusError
	model.currentAssistant = -1
}

func (model *tuiModel) updateToolProgress(progress datool.Progress) {
	index, exists := model.toolItems[progress.CallID]
	if !exists {
		model.appendItem(transcriptItem{kind: itemTool, callID: progress.CallID, name: progress.Name})
		index = len(model.items) - 1
		model.toolItems[progress.CallID] = index
	}
	item := &model.items[index]
	if progress.Name != "" {
		item.name = progress.Name
	}
	item.text = progress.Output
	if progress.Status != "" {
		item.done = true
		item.failed = progress.Status == damessage.ToolStatusError
		model.currentAssistant = -1
	}
}

func (model *tuiModel) finishStream(message streamDoneMsg) (tea.Model, tea.Cmd) {
	if model.turnCancel != nil {
		model.turnCancel()
	}
	model.turnCancel = nil
	model.stream = nil
	if model.cancelling || errors.Is(message.err, context.Canceled) {
		model.status = "Finalizing cancellation"
		return model, cancelRun(model.runner, model.threadID)
	}
	model.running = false
	if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: message.err.Error()})
		model.status = "Error"
		model.refreshTranscript()
		return model, nil
	}
	if goal, present := dagoal.FromState(message.result.State); present {
		model.goal = goal
	}
	if model.approval == nil && len(message.result.Interrupts) > 0 {
		requests, err := decodeApprovalRequests(message.result.Interrupts[0].Value)
		if err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Cannot display approval request: " + err.Error()})
		} else {
			model.approval = &approvalState{requests: requests}
		}
	}
	if model.approval != nil {
		model.approval.ready = true
		if model.autoReview {
			model.running = true
			model.status = "Reviewing action"
			model.refreshTranscript()
			return model, reviewApproval(model.ctx, model.runner, approvalReviewRequest{
				WorkingDir: model.workingDir, Transcript: model.reviewTranscript(), Requests: model.approval.requests,
			})
		}
		model.status = "Review action"
	} else {
		if len(model.pendingWorkflows) > 0 {
			model.refreshTranscript()
			return model, model.startWorkflowContinuation()
		}
		if model.goal != nil && model.goal.Actionable() {
			model.refreshTranscript()
			return model, model.startGoalContinuation()
		}
		model.status = "Ready"
	}
	model.refreshTranscript()
	return model, nil
}

func cancelRun(runner agentRunner, threadID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return cancelDoneMsg{err: runner.Cancel(ctx, threadID)}
	}
}

func reviewApproval(ctx context.Context, runner agentRunner, request approvalReviewRequest) tea.Cmd {
	return func() tea.Msg {
		result, err := runner.Review(ctx, request)
		return reviewDoneMsg{result: result, err: err}
	}
}

func (model *tuiModel) finishReview(message reviewDoneMsg) (tea.Model, tea.Cmd) {
	model.running = false
	if model.approval == nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Automatic review completed without a pending action."})
		model.status = "Review error"
		model.refreshTranscript()
		return model, nil
	}
	if message.err != nil {
		return model.rejectAutomaticApproval("Automatic review failed; action denied: " + message.err.Error())
	}
	decisions := make(map[string]dagent.ApprovalChoice, len(model.approval.requests))
	for _, request := range model.approval.requests {
		assessment, ok := message.result.Assessments[request.Call.ID]
		if !ok {
			return model.rejectAutomaticApproval("Automatic review omitted " + request.Call.Name + "; action denied.")
		}
		decision := dagent.ApprovalApprove
		if !assessment.approved() {
			decision = dagent.ApprovalReject
		}
		decisions[request.Call.ID] = dagent.ApprovalChoice{
			Decision: decision, Reason: assessment.Rationale, Message: assessment.Rationale,
		}
		if !assessment.approved() {
			model.appendItem(transcriptItem{kind: itemNotice, text: fmt.Sprintf(
				"Automatic review denied %s (risk: %s, authorization: %s): %s",
				request.Call.Name, assessment.RiskLevel, assessment.UserAuthorization, assessment.Rationale,
			)})
		}
	}
	return model, model.resumeApproval(decisions)
}

func (model *tuiModel) rejectAutomaticApproval(reason string) (tea.Model, tea.Cmd) {
	decisions := make(map[string]dagent.ApprovalChoice, len(model.approval.requests))
	for _, request := range model.approval.requests {
		decisions[request.Call.ID] = dagent.ApprovalChoice{
			Decision: dagent.ApprovalReject,
			Reason:   reason,
			Message:  reason,
		}
	}
	model.appendItem(transcriptItem{kind: itemError, text: reason})
	return model, model.resumeApproval(decisions)
}

func (model *tuiModel) resolveApproval(approve bool) tea.Cmd {
	decisions := make(map[string]dagent.ApprovalChoice, len(model.approval.requests))
	names := make([]string, 0, len(model.approval.requests))
	for _, request := range model.approval.requests {
		choice := dagent.ApprovalChoice{Decision: dagent.ApprovalApprove}
		if !approve {
			choice.Decision = dagent.ApprovalReject
			choice.Reason = "Rejected by user."
		}
		decisions[request.Call.ID] = choice
		names = append(names, request.Call.Name)
	}
	sort.Strings(names)
	verb := "Approved"
	if !approve {
		verb = "Rejected"
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: verb + ": " + strings.Join(names, ", ")})
	return model.resumeApproval(decisions)
}

func (model *tuiModel) resumeApproval(decisions map[string]dagent.ApprovalChoice) tea.Cmd {
	model.approval = nil
	model.currentAssistant = -1
	return model.startStream(dagent.Input{
		Config: dacheckpoint.Config{ThreadID: model.threadID},
		Resume: dagent.ApprovalResponse{Decisions: decisions}, SkipValueEvents: true,
	})
}

func (model *tuiModel) reviewTranscript() string {
	var transcript strings.Builder
	for _, item := range model.items {
		switch item.kind {
		case itemUser:
			fmt.Fprintf(&transcript, "[user, trusted]\n%s\n\n", item.text)
		case itemAssistant:
			fmt.Fprintf(&transcript, "[assistant, untrusted]\n%s\n\n", item.text)
		case itemTool:
			fmt.Fprintf(&transcript, "[tool %s, untrusted]\narguments: %s\nresult: %s\n\n", item.name, item.args, item.text)
		}
	}
	return transcript.String()
}

func decodeApprovalRequests(value any) ([]dagent.ApprovalRequest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var requests []dagent.ApprovalRequest
	if err := json.Unmarshal(encoded, &requests); err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("approval request is empty")
	}
	for _, request := range requests {
		if request.Call.ID == "" || request.Call.Name == "" {
			return nil, fmt.Errorf("approval request has an incomplete tool call")
		}
	}
	return requests, nil
}

func (model *tuiModel) appendItem(item transcriptItem) {
	model.items = append(model.items, item)
}

func (model *tuiModel) resize(width, height int) {
	model.width = max(width, 20)
	model.height = max(height, 10)
	model.relayout()
	model.refreshTranscript()
}

func (model *tuiModel) relayout() {
	if model.width == 0 || model.height == 0 {
		return
	}
	composerWidth := max(model.width-4, 10)
	model.composer.SetWidth(composerWidth)
	model.composer.SetHeight(composerContentHeight(model.composer.Value(), max(composerWidth-2, 1)))
	viewportHeight := max(model.height-model.composer.Height()-4, 3)
	if !model.ready {
		model.viewport = viewport.New(model.width, viewportHeight)
		model.ready = true
	} else {
		model.viewport.Width = model.width
		model.viewport.Height = viewportHeight
	}
}

func composerContentHeight(value string, width int) int {
	height := 0
	for _, line := range strings.Split(value, "\n") {
		lineWidth := lipgloss.Width(line)
		height += max((lineWidth+width-1)/width, 1)
	}
	return min(height, 8)
}

func (model *tuiModel) refreshTranscript() {
	if !model.ready {
		return
	}
	followBottom := model.viewport.AtBottom()
	model.viewport.SetContent(model.renderTranscript())
	if followBottom {
		model.viewport.GotoBottom()
	}
}

func (model *tuiModel) refreshSpinner() {
	if !model.ready || !model.running {
		return
	}
	model.viewport.SetContent(model.renderTranscript())
}

func (model *tuiModel) View() string {
	if !model.ready {
		return "Starting dacode…"
	}
	if model.sessionPicker != nil {
		return model.renderSessionPicker()
	}
	if model.workflowPanel != nil {
		return model.renderWorkflowPanel()
	}
	composer := model.composer
	if model.running {
		composer.Placeholder = "Agent is working…"
	}
	input := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colorPrimary).
		Padding(0, 1).Width(max(model.width-2, 1)).
		Render(composer.View())
	return lipgloss.NewStyle().Foreground(colorBody).Render(
		model.viewport.View() + "\n" + input + "\n" + model.renderStatus(),
	)
}

func (model *tuiModel) renderTranscript() string {
	width := max(model.width-4, 20)
	sections := []string{model.renderWelcome(width)}
	for _, item := range model.items {
		sections = append(sections, renderItem(item, width))
	}
	if model.running {
		sections = append(sections, lipgloss.NewStyle().Foreground(colorMuted).PaddingLeft(1).Render(model.spinner.View()+" "+model.status+"…"))
	}
	if model.manualApprovalVisible() {
		sections = append(sections, renderApproval(model.approval, width))
	}
	return strings.Join(sections, "\n\n")
}

func (model *tuiModel) manualApprovalVisible() bool {
	return model.approval != nil && !model.autoReview
}

func (model *tuiModel) renderWelcome(width int) string {
	title := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("dacode")
	subtitle := lipgloss.NewStyle().Foreground(colorSecondary).Render("  Go coding agent")
	mode := "manual review"
	if model.autoReview {
		mode = "auto review"
	}
	if model.yolo {
		mode = "yolo"
	}
	metadata := "openai:" + model.modelName + "  •  " + shortPath(model.workingDir) + "  •  " + mode
	metadataWidth := max(width-4, 16)
	if lipgloss.Width(metadata) > metadataWidth {
		metadata = "openai:" + model.modelName + "  •  " + mode + "\n" + truncate(shortPath(model.workingDir), metadataWidth)
	}
	lines := []string{title + subtitle}
	for _, line := range strings.Split(metadata, "\n") {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(line))
	}
	lines = append(lines,
		lipgloss.NewStyle().Foreground(colorBody).Render("Ready to code. What would you like to build?"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("Enter send  •  Ctrl+J newline  •  / commands"),
	)
	for index, line := range lines {
		lines[index] = line + strings.Repeat(" ", max(metadataWidth-lipgloss.Width(line), 0))
	}
	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorPrimary).
		Padding(0, 2).Width(max(width, 1)).Render(body)
}

func renderItem(item transcriptItem, width int) string {
	contentWidth := max(width-4, 10)
	switch item.kind {
	case itemUser:
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorPrimary).
			PaddingLeft(1).Width(contentWidth).Render(lipgloss.NewStyle().Foreground(colorBody).Render("> ") + item.text)
	case itemAssistant:
		return lipgloss.NewStyle().Foreground(colorBody).PaddingLeft(1).Width(contentWidth).Render(item.text)
	case itemTool:
		icon := "○"
		color := colorWarning
		if item.done {
			icon = "✓"
			color = colorSuccess
		}
		if item.failed {
			icon = "✗"
			color = colorError
		}
		header := lipgloss.NewStyle().Foreground(color).Bold(true).Render(icon+" "+item.name) + " " +
			lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(item.args, max(contentWidth-len(item.name)-6, 16)))
		body := header
		if item.text != "" {
			body += "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render(collapseLines(item.text, 8))
		}
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorPanel).
			PaddingLeft(1).Width(contentWidth).Render(body)
	case itemError:
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorError).
			Foreground(colorError).PaddingLeft(1).Width(contentWidth).Render(item.text)
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).PaddingLeft(1).Width(contentWidth).Render(item.text)
	}
}

func renderApproval(state *approvalState, width int) string {
	lines := []string{lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("Review requested")}
	for _, request := range state.requests {
		line := lipgloss.NewStyle().Foreground(colorBody).Render(request.Call.Name) + " " +
			lipgloss.NewStyle().Foreground(colorMuted).Render(compactJSON(request.Call.Arguments))
		if request.Description != "" {
			line += "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render(request.Description)
		}
		lines = append(lines, line)
	}
	hint := "Pausing…"
	if state.ready {
		hint = "y approve  •  n reject"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(hint))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorWarning).
		Padding(0, 1).Width(max(width, 1)).Render(strings.Join(lines, "\n"))
}

func (model *tuiModel) renderStatus() string {
	mode := "manual"
	modeColor := colorSuccess
	if model.autoReview {
		mode = "auto review"
		modeColor = colorPrimary
	}
	if model.yolo {
		mode = "yolo"
		modeColor = colorError
	}
	hint := "  ctrl+c cancel  •  ctrl+d quit"
	rightText := fmt.Sprintf("%d tokens  •  openai:%s", model.totalTokens, model.modelName)
	badgeWidth := lipgloss.Width(mode) + 2
	if badgeWidth+lipgloss.Width(hint)+1+lipgloss.Width(rightText) > model.width {
		hint = "  ctrl+d quit"
		rightText = fmt.Sprintf("%d tok", model.totalTokens)
	}
	if badgeWidth+lipgloss.Width(hint)+1+lipgloss.Width(rightText) > model.width {
		hint = ""
	}
	left := lipgloss.NewStyle().Background(modeColor).Foreground(colorBackground).Bold(true).Padding(0, 1).Render(mode)
	left += lipgloss.NewStyle().Foreground(colorMuted).Render(hint)
	right := lipgloss.NewStyle().Foreground(colorMuted).Render(rightText)
	space := max(model.width-lipgloss.Width(left)-lipgloss.Width(right), 0)
	first := left + strings.Repeat(" ", space) + right
	statusWidth := max(model.width-1, 1)
	id := truncate(model.threadID, 8)
	goalStatus := ""
	if model.goal != nil {
		goalStatus = "  •  goal:" + string(model.goal.Status)
	}
	workflowStatus := ""
	if active := model.runner.RunningWorkflows(); active > 0 {
		workflowStatus = fmt.Sprintf("  •  wf:%d", active)
	}
	suffix := goalStatus + workflowStatus + "  •  " + id
	pathWidth := max(statusWidth-lipgloss.Width(suffix), 1)
	secondText := truncate(shortPath(model.workingDir), pathWidth) + suffix
	second := lipgloss.NewStyle().Background(colorSurface).Foreground(colorMuted).Width(statusWidth).
		Render(secondText)
	return first + "\n" + second
}

func compactJSON(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	var compact bytes.Buffer
	if json.Compact(&compact, value) == nil {
		return compact.String()
	}
	return string(value)
}

func shortPath(value string) string {
	clean := filepath.Clean(value)
	home, err := os.UserHomeDir()
	if err != nil {
		return clean
	}
	home = filepath.Clean(home)
	if clean == home {
		return "~"
	}
	if relative, err := filepath.Rel(home, clean); err == nil && relative != "." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != ".." {
		return filepath.Join("~", relative)
	}
	return clean
}

func collapseLines(value string, maximum int) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	if len(lines) <= maximum {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:maximum], "\n") + fmt.Sprintf("\n… %d more lines", len(lines)-maximum)
}

func truncate(value string, width int) string {
	if width <= 0 || len(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return value[:width-1] + "…"
}
