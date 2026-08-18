package dacode

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/daworkflow"
)

const workflowRefreshInterval = 200 * time.Millisecond

type workflowPanelState struct {
	runs     []daworkflow.Status
	selected int
	loading  bool
	polling  bool
	err      error
}

type activeWorkflowAgent struct {
	call      int
	label     string
	phase     string
	startedAt time.Time
	tokens    int64
}

type workflowsLoadedMsg struct {
	runs []daworkflow.Status
}

type workflowStartedMsg struct {
	status daworkflow.Status
	runs   []daworkflow.Status
	err    error
}

type workflowCancelledMsg struct {
	runID     string
	cancelled bool
	runs      []daworkflow.Status
}

type workflowTickMsg struct {
	panel *workflowPanelState
}

type workflowCompletedMsg struct {
	status daworkflow.Status
}

type workflowRunner interface {
	StartWorkflow(context.Context, daworkflow.StartRequest) (daworkflow.Status, error)
	Workflows() []daworkflow.Status
	RunningWorkflows() int
	CancelWorkflow(string) bool
	WaitWorkflowCompletion(context.Context) (daworkflow.Status, bool)
}

func workflowRuntime(runner agentRunner) (workflowRunner, bool) {
	runtime, ok := runner.(workflowRunner)
	return runtime, ok
}

func waitForWorkflowCompletion(ctx context.Context, runner agentRunner) tea.Cmd {
	return func() tea.Msg {
		runtime, available := workflowRuntime(runner)
		if !available {
			return nil
		}
		if status, ok := runtime.WaitWorkflowCompletion(ctx); ok {
			return workflowCompletedMsg{status: status}
		}
		return nil
	}
}

func workflowCompletionMessage(statuses []daworkflow.Status) damessage.Message {
	payload, _ := json.Marshal(statuses)
	body := string(payload)
	if len(payload) > 16_000 {
		body = string(payload[:16_000]) + "\n<notification truncated; use check_workflow>"
	}
	message := damessage.Human(`<workflow_notification>
One or more background workflows completed. This is trusted host-generated state. Inspect the statuses and results below, explain failures or summarize useful results to the user, and use check_workflow if more detail is needed. Do not start another workflow unless the user asks.

` + body + `
</workflow_notification>`)
	message.Metadata = map[string]json.RawMessage{"dago_workflow_control": json.RawMessage(`true`)}
	return message
}

func startWorkflow(ctx context.Context, runner agentRunner, reference string) tea.Cmd {
	return func() tea.Msg {
		runtime, available := workflowRuntime(runner)
		if !available {
			return workflowStartedMsg{err: fmt.Errorf("workflow runtime is unavailable")}
		}
		status, err := runtime.StartWorkflow(ctx, daworkflow.StartRequest{ScriptPath: reference})
		return workflowStartedMsg{status: status, runs: runtime.Workflows(), err: err}
	}
}

func loadWorkflows(runner agentRunner) tea.Cmd {
	return func() tea.Msg {
		runtime, available := workflowRuntime(runner)
		if !available {
			return workflowsLoadedMsg{}
		}
		return workflowsLoadedMsg{runs: runtime.Workflows()}
	}
}

func cancelWorkflow(runner agentRunner, runID string) tea.Cmd {
	return func() tea.Msg {
		runtime, available := workflowRuntime(runner)
		if !available {
			return workflowCancelledMsg{runID: runID}
		}
		cancelled := runtime.CancelWorkflow(runID)
		return workflowCancelledMsg{runID: runID, cancelled: cancelled, runs: runtime.Workflows()}
	}
}

func pollWorkflows(panel *workflowPanelState) tea.Cmd {
	if panel == nil || panel.polling {
		return nil
	}
	panel.polling = true
	return tea.Tick(workflowRefreshInterval, func(time.Time) tea.Msg { return workflowTickMsg{panel: panel} })
}

func (model *tuiModel) showWorkflows() tea.Cmd {
	model.workflowPanel = &workflowPanelState{loading: true}
	return loadWorkflows(model.runner)
}

func (model *tuiModel) finishWorkflowCompletion(status daworkflow.Status) tea.Cmd {
	kind := itemNotice
	text := fmt.Sprintf("Workflow %s (%s) completed: %s", status.Name, status.RunID, strings.ToUpper(status.Status))
	if status.Error != "" {
		kind = itemError
		text += " " + model.glyphs.Dash + " " + status.Error
	}
	model.appendItem(transcriptItem{kind: kind, text: text})
	model.pendingWorkflows = append(model.pendingWorkflows, status)
	if model.workflowPanel != nil {
		if runtime, available := workflowRuntime(model.runner); available {
			model.updateWorkflowRuns(runtime.Workflows(), status.RunID)
		}
	}
	model.refreshTranscript()
	wait := waitForWorkflowCompletion(model.ctx, model.runner)
	if model.running || model.approval != nil {
		return wait
	}
	return tea.Batch(wait, model.startWorkflowContinuation())
}

func (model *tuiModel) startWorkflowContinuation() tea.Cmd {
	if len(model.pendingWorkflows) == 0 {
		return nil
	}
	statuses := append([]daworkflow.Status(nil), model.pendingWorkflows...)
	model.pendingWorkflows = nil
	model.currentAssistant = -1
	model.status = "Reviewing workflow result"
	return model.startStream(dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: model.threadID},
		Messages: []damessage.Message{workflowCompletionMessage(statuses)}, SkipValueEvents: true,
	})
}

func (model *tuiModel) workflowCommand(reference string) tea.Cmd {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		model.appendItem(transcriptItem{kind: itemError, text: "Usage: /workflow <saved-name-or-script-path>"})
		model.refreshTranscript()
		return nil
	}
	model.appendItem(transcriptItem{kind: itemUser, text: "/workflow " + reference})
	model.running = true
	model.status = "Starting workflow"
	model.refreshTranscript()
	return startWorkflow(model.ctx, model.runner, reference)
}

func (model *tuiModel) finishWorkflowStart(message workflowStartedMsg) tea.Cmd {
	model.running = false
	if message.err != nil {
		model.status = "Workflow error"
		model.appendItem(transcriptItem{kind: itemError, text: message.err.Error()})
		model.refreshTranscript()
		return nil
	}
	model.status = "Ready"
	model.workflowPanel = &workflowPanelState{}
	model.updateWorkflowRuns(message.runs, message.status.RunID)
	if message.status.Status == "running" {
		return pollWorkflows(model.workflowPanel)
	}
	return nil
}

func (model *tuiModel) finishWorkflowLoad(message workflowsLoadedMsg) tea.Cmd {
	if model.workflowPanel == nil {
		return nil
	}
	selected := ""
	if run := model.selectedWorkflow(); run != nil {
		selected = run.RunID
	}
	model.workflowPanel.loading = false
	model.updateWorkflowRuns(message.runs, selected)
	if workflowsRunning(model.workflowPanel.runs) {
		return pollWorkflows(model.workflowPanel)
	}
	return nil
}

func (model *tuiModel) finishWorkflowCancel(message workflowCancelledMsg) tea.Cmd {
	if model.workflowPanel == nil {
		return nil
	}
	if !message.cancelled {
		model.workflowPanel.err = fmt.Errorf("workflow %s is not running", message.runID)
	} else {
		model.workflowPanel.err = nil
	}
	model.updateWorkflowRuns(message.runs, message.runID)
	return pollWorkflows(model.workflowPanel)
}

func (model *tuiModel) updateWorkflowRuns(runs []daworkflow.Status, selected string) {
	if model.workflowPanel == nil {
		return
	}
	model.workflowPanel.runs = append([]daworkflow.Status(nil), runs...)
	sort.SliceStable(model.workflowPanel.runs, func(left, right int) bool {
		if model.workflowPanel.runs[left].CreatedAt == model.workflowPanel.runs[right].CreatedAt {
			return model.workflowPanel.runs[left].RunID > model.workflowPanel.runs[right].RunID
		}
		return model.workflowPanel.runs[left].CreatedAt > model.workflowPanel.runs[right].CreatedAt
	})
	model.workflowPanel.selected = min(model.workflowPanel.selected, max(len(model.workflowPanel.runs)-1, 0))
	for index := range model.workflowPanel.runs {
		if model.workflowPanel.runs[index].RunID == selected {
			model.workflowPanel.selected = index
			break
		}
	}
}

func (model *tuiModel) selectedWorkflow() *daworkflow.Status {
	if model.workflowPanel == nil || len(model.workflowPanel.runs) == 0 {
		return nil
	}
	index := min(max(model.workflowPanel.selected, 0), len(model.workflowPanel.runs)-1)
	return &model.workflowPanel.runs[index]
}

func (model *tuiModel) handleWorkflowKey(message tea.KeyMsg) (tea.Cmd, bool) {
	panel := model.workflowPanel
	if panel == nil {
		return nil, false
	}
	switch message.String() {
	case "esc", "q":
		model.workflowPanel = nil
		model.refreshTranscript()
		return nil, true
	case "up", "k":
		panel.selected = max(panel.selected-1, 0)
		panel.err = nil
		return nil, true
	case "down", "j":
		panel.selected = min(panel.selected+1, max(len(panel.runs)-1, 0))
		panel.err = nil
		return nil, true
	case "r":
		panel.loading = true
		panel.err = nil
		return loadWorkflows(model.runner), true
	case "c":
		run := model.selectedWorkflow()
		if run == nil || run.Status != "running" {
			panel.err = fmt.Errorf("select a running workflow to cancel")
			return nil, true
		}
		return cancelWorkflow(model.runner, run.RunID), true
	}
	return nil, true
}

func workflowsRunning(runs []daworkflow.Status) bool {
	for _, run := range runs {
		if run.Status == "running" {
			return true
		}
	}
	return false
}

func (model *tuiModel) renderWorkflowPanel() string {
	panel := model.workflowPanel
	if panel == nil {
		return ""
	}
	width := max(model.width-6, 26)
	title := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("WORKFLOW CONTROL")
	subtitle := lipgloss.NewStyle().Foreground(colorMuted).Render("deterministic background orchestration")
	active, complete, failed := workflowStatusCounts(panel.runs)
	separator := "  " + model.glyphs.Separator + "  "
	summary := fmt.Sprintf("%d RUNS%s%d ACTIVE%s%d COMPLETE", len(panel.runs), separator, active, separator, complete)
	if failed > 0 {
		summary += fmt.Sprintf("%s%d ATTENTION", separator, failed)
	}
	lines := []string{title + "  " + subtitle, lipgloss.NewStyle().Foreground(colorMuted).Render(summary), ""}
	switch {
	case panel.loading && len(panel.runs) == 0:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(model.spinner.View()+" Reading workflow journal"+model.glyphs.Ellipsis))
	case len(panel.runs) == 0:
		lines = append(lines,
			lipgloss.NewStyle().Foreground(colorBody).Bold(true).Render("No workflow runs yet."),
			lipgloss.NewStyle().Foreground(colorMuted).Render("Launch one with /workflow <saved-name-or-script-path> or ask the agent to use a workflow."),
		)
	default:
		pageSize := max((model.height-18)/2, 1)
		start := max(panel.selected-pageSize+1, 0)
		end := min(start+pageSize, len(panel.runs))
		for index := start; index < end; index++ {
			lines = append(lines, renderWorkflowRow(panel.runs[index], index == panel.selected, width, model.glyphs))
		}
		if selected := model.selectedWorkflow(); selected != nil {
			lines = append(lines, "", renderWorkflowDetailWithGlyphs(*selected, width, model.glyphs))
		}
	}
	if panel.err != nil {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorError).Render(panel.err.Error()))
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(
		model.glyphs.ArrowUp+model.glyphs.ArrowDown+" select"+separator+"c cancel"+separator+"r refresh"+separator+"esc return",
	))
	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Foreground(colorBody).Border(uiBorder(model.glyphs)).
		BorderForeground(colorPrimary).Padding(1, 2).Width(max(model.width-2, 1)).Render(body)
}

func renderWorkflowRow(run daworkflow.Status, selected bool, width int, glyphs uiGlyphs) string {
	icon, color := workflowStatusStyle(run.Status, glyphs)
	name := run.Name
	if name == "" {
		name = "unnamed workflow"
	}
	header := lipgloss.NewStyle().Foreground(color).Bold(true).Render(icon+" "+strings.ToUpper(run.Status)) + "  " +
		lipgloss.NewStyle().Foreground(colorBody).Bold(true).Render(truncate(name, max(width-18, 8)))
	separator := "  " + glyphs.Separator + "  "
	metadata := run.RunID + separator + workflowAge(run.UpdatedAt) + separator + workflowProgressText(run, glyphs)
	body := header + "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(metadata, width-5))
	style := lipgloss.NewStyle().BorderLeft(true).BorderStyle(uiBorder(glyphs)).BorderForeground(colorPanel).
		Padding(0, 1).Width(max(width-2, 1))
	if selected {
		style = style.BorderForeground(colorPrimary).Background(colorSurface)
	}
	return style.Render(body)
}

func renderWorkflowDetail(run daworkflow.Status, width int) string {
	return renderWorkflowDetailWithGlyphs(run, width, unicodeUIGlyphs)
}

func renderWorkflowDetailWithGlyphs(run daworkflow.Status, width int, glyphs uiGlyphs) string {
	title := lipgloss.NewStyle().Foreground(colorSecondary).Bold(true).Render("SELECTED RUN")
	lines := []string{title}
	if run.Description != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorBody).Render(truncate(run.Description, width-5)))
	}
	if len(run.Phases) > 0 {
		current := currentWorkflowPhase(run.Events)
		phases := make([]string, 0, len(run.Phases))
		for _, phase := range run.Phases {
			marker := glyphs.CircleEmpty
			style := lipgloss.NewStyle().Foreground(colorMuted)
			if phase.Title == current {
				marker = glyphs.CircleFilled
				style = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
			}
			phases = append(phases, style.Render(marker+" "+phase.Title))
		}
		lines = append(lines, strings.Join(phases, "  "+glyphs.BoxHorizontal+"  "))
	}
	if event := latestWorkflowEvent(run.Events); event != nil {
		activity := event.Label
		if event.Kind == "agent_failed" && event.Message != "" {
			activity = event.Message
		}
		if activity == "" {
			activity = event.Message
		}
		if activity == "" {
			activity = strings.ReplaceAll(event.Kind, "_", " ")
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("latest  "+truncate(activity, width-13)))
	}
	if agents := activeWorkflowAgents(run); len(agents) > 0 {
		lines = append(lines, "", renderActiveWorkflowAgents(agents, width, time.Now(), glyphs))
	}
	if run.Error != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Render(truncate(run.Error, width-5)))
	}
	return lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorPanel).
		Padding(0, 1).Width(max(width-2, 1)).Render(strings.Join(lines, "\n"))
}

func activeWorkflowAgents(run daworkflow.Status) []activeWorkflowAgent {
	active := map[int]activeWorkflowAgent{}
	fallback, _ := time.Parse(time.RFC3339, run.CreatedAt)
	for _, event := range run.Events {
		switch event.Kind {
		case "agent_started":
			startedAt, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil {
				startedAt = fallback
			}
			active[event.Call] = activeWorkflowAgent{
				call: event.Call, label: event.Label, phase: event.Phase, startedAt: startedAt, tokens: event.Tokens,
			}
		case "agent_progress":
			agent, exists := active[event.Call]
			if !exists {
				continue
			}
			agent.tokens = max(agent.tokens, event.Tokens)
			active[event.Call] = agent
		case "agent_finished", "agent_failed":
			delete(active, event.Call)
		}
	}
	result := make([]activeWorkflowAgent, 0, len(active))
	for _, agent := range active {
		if agent.label == "" {
			agent.label = fmt.Sprintf("agent-%d", agent.call)
		}
		result = append(result, agent)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].call < result[right].call })
	return result
}

func renderActiveWorkflowAgents(agents []activeWorkflowAgent, width int, now time.Time, glyphs uiGlyphs) string {
	title := lipgloss.NewStyle().Foreground(colorSecondary).Bold(true).Render(fmt.Sprintf("RUNNING AGENTS  %d", len(agents)))
	lines := []string{title}
	for _, agent := range agents {
		elapsed := now.Sub(agent.startedAt)
		if agent.startedAt.IsZero() || elapsed < 0 {
			elapsed = 0
		}
		metadata := formatWorkflowElapsed(elapsed) + "  " + glyphs.Separator + "  " + formatWorkflowTokens(agent.tokens, glyphs)
		phase := ""
		if agent.phase != "" {
			phase = "  " + agent.phase
		}
		available := max(width-lipgloss.Width(metadata)-lipgloss.Width(phase)-12, 8)
		label := truncate(agent.label, available)
		left := lipgloss.NewStyle().Foreground(colorPrimary).Render(glyphs.CircleFilled) + " " +
			lipgloss.NewStyle().Foreground(colorBody).Bold(true).Render(label) +
			lipgloss.NewStyle().Foreground(colorMuted).Render(phase)
		gap := max(width-lipgloss.Width(left)-lipgloss.Width(metadata)-5, 1)
		lines = append(lines, left+strings.Repeat(" ", gap)+lipgloss.NewStyle().Foreground(colorMuted).Render(metadata))
	}
	return strings.Join(lines, "\n")
}

func formatWorkflowElapsed(elapsed time.Duration) string {
	seconds := max(int(elapsed/time.Second), 0)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	seconds %= 60
	if minutes < 60 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}

func formatWorkflowTokens(tokens int64, glyphs uiGlyphs) string {
	if tokens <= 0 {
		return "counting" + glyphs.Ellipsis
	}
	if tokens < 1_000 {
		return fmt.Sprintf("~%d tok", tokens)
	}
	return fmt.Sprintf("~%.1fk tok", float64(tokens)/1_000)
}

func workflowStatusStyle(status string, glyphs uiGlyphs) (string, lipgloss.Color) {
	switch status {
	case "running":
		return glyphs.Cursor, colorPrimary
	case "success":
		return glyphs.Checkmark, colorSuccess
	case "cancelled":
		return glyphs.Warning, colorWarning
	default:
		return "!", colorError
	}
}

func workflowStatusCounts(runs []daworkflow.Status) (active, complete, failed int) {
	for _, run := range runs {
		switch run.Status {
		case "running":
			active++
		case "success":
			complete++
		case "error", "cancelled":
			failed++
		}
	}
	return active, complete, failed
}

func workflowProgressText(run daworkflow.Status, glyphs uiGlyphs) string {
	started, finished, failed := 0, 0, 0
	for _, event := range run.Events {
		switch event.Kind {
		case "agent_started":
			started++
		case "agent_finished":
			finished++
		case "agent_failed":
			failed++
		}
	}
	if started == 0 {
		if run.Status == "running" {
			return "initializing"
		}
		return "no agent calls"
	}
	active := max(started-finished-failed, 0)
	separator := " " + glyphs.Separator + " "
	return fmt.Sprintf("%d done%s%d active%s%d failed", finished, separator, active, separator, failed)
}

func currentWorkflowPhase(events []daworkflow.Event) string {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Phase != "" {
			return events[index].Phase
		}
		if events[index].Kind == "phase" && events[index].Message != "" {
			return events[index].Message
		}
	}
	return ""
}

func latestWorkflowEvent(events []daworkflow.Event) *daworkflow.Event {
	if len(events) == 0 {
		return nil
	}
	return &events[len(events)-1]
}

func workflowAge(value string) string {
	updated, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "just now"
	}
	age := time.Since(updated)
	if age < time.Minute {
		return "just now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(age.Hours()))
}
