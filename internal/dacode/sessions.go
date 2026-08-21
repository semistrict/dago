package dacode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

type sessionInfo struct {
	ThreadID       string
	CheckpointID   string
	ThreadRevision string
	Preview        string
	Directory      string
	Agent          string
	Branch         string
	ContextTokens  int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	MessageCount   int
}

type sessionPickerState struct {
	sessions     []sessionInfo
	selected     int
	loading      bool
	resuming     bool
	startup      bool
	err          error
	selector     *threadSelectorState
	directResume bool
	requestID    uint64
}

var nextSessionPickerRequestID atomic.Uint64

type sessionsLoadedMsg struct {
	sessions  []sessionInfo
	err       error
	requestID uint64
}

type sessionLoadedMsg struct {
	session    sessionInfo
	messages   []damessage.Message
	decision   sessionResumeDecision
	err        error
	generation uint64
}

type sessionResumePreparedMsg struct {
	controller *sessionResumeController
	err        error
	generation uint64
}

type sessionDirectorySwitcher interface {
	SwitchSessionDirectory(context.Context, string) error
}

func listSessions(ctx context.Context, runner agentRunner) tea.Cmd {
	return listSessionsForPicker(ctx, runner, 0)
}

func listSessionsForPicker(ctx context.Context, runner agentRunner, requestID uint64) tea.Cmd {
	return func() tea.Msg {
		sessions, err := runner.ListSessions(ctx)
		return sessionsLoadedMsg{sessions: sessions, err: err, requestID: requestID}
	}
}

func loadSession(ctx context.Context, runner agentRunner, session sessionInfo) tea.Cmd {
	return func() tea.Msg {
		messages, err := runner.LoadSession(ctx, session.ThreadID)
		return sessionLoadedMsg{session: session, messages: messages, err: err}
	}
}

func prepareSession(ctx context.Context, runner agentRunner, threadID string, options sessionResumeOptions) tea.Cmd {
	return prepareSessionGeneration(ctx, runner, threadID, options, 0)
}

func prepareSessionGeneration(ctx context.Context, runner agentRunner, threadID string, options sessionResumeOptions, generation uint64) tea.Cmd {
	return func() tea.Msg {
		controller, err := prepareSessionResume(ctx, runner, threadID, options)
		return sessionResumePreparedMsg{controller: controller, err: err, generation: generation}
	}
}

func continueSessionResume(ctx context.Context, runner agentRunner, controller *sessionResumeController) tea.Cmd {
	return continueSessionResumeGeneration(ctx, runner, controller, 0)
}

func continueSessionResumeGeneration(ctx context.Context, runner agentRunner, controller *sessionResumeController, generation uint64) tea.Cmd {
	return func() tea.Msg {
		decision, ready := controller.Decision()
		if !ready {
			return sessionLoadedMsg{err: fmt.Errorf("session resume decisions are incomplete"), generation: generation}
		}
		var switcher sessionDirectorySwitcher
		if decision.SwitchDirectory {
			var ok bool
			switcher, ok = runner.(sessionDirectorySwitcher)
			if !ok {
				return sessionLoadedMsg{decision: decision, err: fmt.Errorf("switch session directory: runtime restart is unavailable"), generation: generation}
			}
		}
		agentAttempted := false
		directoryAttempted := false
		rollback := func(cause error) error {
			rollbackContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			failures := []error{cause}
			if directoryAttempted {
				if err := switcher.SwitchSessionDirectory(rollbackContext, controller.options.CurrentDirectory); err != nil {
					failures = append(failures, fmt.Errorf("restore session directory: %w", err))
				}
			}
			if agentAttempted {
				if err := runner.SwitchAgent(rollbackContext, controller.currentAgent); err != nil {
					failures = append(failures, fmt.Errorf("restore session agent: %w", err))
				}
			}
			return errors.Join(failures...)
		}
		if decision.SwitchAgent {
			agentAttempted = true
			if err := runner.SwitchAgent(ctx, decision.Agent); err != nil {
				return sessionLoadedMsg{decision: decision, err: rollback(fmt.Errorf("switch session agent: %w", err)), generation: generation}
			}
		}
		if decision.SwitchDirectory {
			directoryAttempted = true
			if err := switcher.SwitchSessionDirectory(ctx, decision.Directory); err != nil {
				return sessionLoadedMsg{decision: decision, err: rollback(fmt.Errorf("switch session directory: %w", err)), generation: generation}
			}
		}
		session, messages, err := loadPreparedSession(ctx, runner, controller)
		if err != nil {
			err = rollback(err)
		}
		return sessionLoadedMsg{session: session, messages: messages, decision: decision, err: err, generation: generation}
	}
}

func (model *tuiModel) finishSessionList(message sessionsLoadedMsg) tea.Cmd {
	if model.sessionPicker == nil {
		return nil
	}
	if message.requestID != 0 && message.requestID != model.sessionPicker.requestID {
		return nil
	}
	if model.sessionPicker.directResume && message.err == nil {
		model.sessionPicker.directResume = false
		if len(message.sessions) == 0 {
			model.sessionPicker.err = fmt.Errorf("no resumable threads were found")
			return nil
		}
		model.sessionPicker.resuming = true
		options := model.resumeOptions
		options.AbortMode = cwdResumeAbortThreadSwitch
		return prepareSessionGeneration(model.ctx, model.runner, message.sessions[0].ThreadID, options, model.operationGeneration)
	}
	model.sessionPicker.loading = false
	model.sessionPicker.sessions = message.sessions
	model.sessionPicker.err = message.err
	if message.err == nil {
		options := defaultThreadSelectorOptions()
		options.Preferences = model.threadSelectorPreferences
		if model.sessionPicker.selector == nil {
			model.sessionPicker.selector = newThreadSelectorStateWithOptions(message.sessions, model.threadID, options)
		} else {
			model.sessionPicker.selector.replaceSessions(message.sessions)
		}
	}
	for index, session := range message.sessions {
		if session.ThreadID == model.threadID {
			model.sessionPicker.selected = index
			break
		}
	}
	return nil
}

func (model *tuiModel) threadsCommand(arguments string) tea.Cmd {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		model.sessionPicker = &sessionPickerState{loading: true, requestID: nextSessionPickerRequestID.Add(1)}
		return listSessionsForPicker(model.ctx, model.runner, model.sessionPicker.requestID)
	}
	fields := strings.Fields(arguments)
	if len(fields) == 0 || fields[0] != "-r" || len(fields) > 2 {
		return model.notify("Usage: /threads [-r [ID]]", toastWarning, "")
	}
	if len(fields) == 1 {
		model.sessionPicker = &sessionPickerState{loading: true, directResume: true, requestID: nextSessionPickerRequestID.Add(1)}
		return listSessionsForPicker(model.ctx, model.runner, model.sessionPicker.requestID)
	}
	threadID := validThreadSelectorID(fields[1])
	if threadID == "" {
		return model.notify("Enter a valid thread ID.", toastWarning, "")
	}
	model.sessionPicker = &sessionPickerState{resuming: true}
	options := model.resumeOptions
	options.AbortMode = cwdResumeAbortThreadSwitch
	return prepareSessionGeneration(model.ctx, model.runner, threadID, options, model.operationGeneration)
}

func (model *tuiModel) finishSessionLoad(message sessionLoadedMsg) tea.Cmd {
	if model.sessionPicker == nil {
		return nil
	}
	if message.err != nil {
		model.sessionPicker.resuming = false
		model.sessionPicker.err = message.err
		model.resumeController = nil
		return nil
	}
	model.operationGeneration++
	model.threadID = message.session.ThreadID
	model.threadHasCheckpoint = true
	if message.decision.SwitchDirectory {
		model.workingDir = message.decision.Directory
		model.resumeOptions.CurrentDirectory = message.decision.Directory
	}
	if message.decision.SwitchAgent {
		model.agentName = message.decision.Agent
	}
	approvalModeErr := model.restoreApprovalMode(model.threadID)
	model.goal = nil
	model.restoreTranscript(message.messages)
	if approvalModeErr != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not restore approval mode; using Manual: " + approvalModeErr.Error()})
	}
	model.showStartupTranscript()
	model.sessionPicker = nil
	model.resumeController = nil
	model.status = "Resumed session"
	model.refreshTranscript()
	if message.decision.Compact {
		model.compactionCheckpointID = message.session.CheckpointID
		if strings.TrimSpace(model.initial) != "" {
			model.inputQueue = append(model.inputQueue, queuedInput{mode: inputNormal, value: model.initial, display: model.initial})
			model.initial = ""
		}
		return model.startCompaction()
	}
	if model.autoModeNotice || model.yoloModeNotice {
		model.approvalNoticeDeferred = true
		return model.requestCostReport(false)
	}
	if strings.TrimSpace(model.initial) != "" {
		return tea.Batch(model.submitPrompt(model.initial), model.requestCostReport(false))
	}
	return tea.Batch(loadGoalGeneration(model.ctx, model.runner, model.threadID, model.operationGeneration), model.requestCostReport(false))
}

func (model *tuiModel) finishSessionResumePreparation(message sessionResumePreparedMsg) tea.Cmd {
	if model.sessionPicker == nil {
		return nil
	}
	if message.err != nil {
		model.sessionPicker.resuming = false
		model.sessionPicker.err = message.err
		return nil
	}
	model.resumeController = message.controller
	if _, ready := message.controller.Decision(); ready {
		return continueSessionResumeGeneration(model.ctx, model.runner, message.controller, model.operationGeneration)
	}
	return nil
}

func (model *tuiModel) handleSessionResumeKey(message tea.KeyPressMsg) tea.Cmd {
	controller := model.resumeController
	if controller == nil {
		return nil
	}
	prompt := controller.Prompt()
	action := resumePromptNoAction
	switch {
	case prompt.Agent != nil:
		action = prompt.Agent.handleKey(message.String())
	case prompt.CWD != nil:
		action = prompt.CWD.handleKey(message.String())
	case prompt.Compact != nil:
		action = prompt.Compact.handleKey(message.String())
	}
	if action == resumePromptNoAction {
		return nil
	}
	if err := controller.Apply(action); err != nil {
		model.sessionPicker.resuming = false
		model.sessionPicker.err = err
		model.resumeController = nil
		return nil
	}
	if controller.Canceled() {
		model.resumeController = nil
		if model.sessionPicker.startup {
			threadID, err := newThreadID()
			if err != nil {
				model.sessionPicker.err = err
				model.sessionPicker.resuming = false
				return nil
			}
			model.threadID = threadID
			model.sessionPicker = nil
			model.status = "Resume canceled; started a new session"
			return model.initialSessionCommand()
		}
		model.sessionPicker.resuming = false
		return model.finishDeferredAsync(deferredThreadSwitch, model.deferredThreadWaiting(), nil)
	}
	if _, ready := controller.Decision(); ready {
		return continueSessionResumeGeneration(model.ctx, model.runner, controller, model.operationGeneration)
	}
	return nil
}

func (model *tuiModel) handleSessionKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	picker := model.sessionPicker
	if picker == nil {
		return nil, false
	}
	if picker.selector != nil && !picker.loading && !picker.resuming && picker.err == nil {
		if message.String() == "ctrl+c" {
			if picker.startup {
				return tea.Quit, true
			}
			model.sessionPicker = nil
			return nil, true
		}
		if message.String() == "ctrl+d" && picker.selector.confirmingDelete != nil {
			model.confirmations.intervene(confirmDelete)
			if model.confirmations.press(confirmDelete, time.Now()) {
				return tea.Quit, true
			}
			return model.notify("Press Ctrl+D again to quit; Enter confirms deletion.", toastWarning, ""), true
		}
		if picker.startup && message.String() == "q" {
			return tea.Quit, true
		}
		result := picker.selector.handleKey(message.String(), model.sessionPageSize())
		switch result.Action {
		case threadSelectorCancel:
			if picker.startup {
				return tea.Quit, true
			}
			model.sessionPicker = nil
		case threadSelectorResume:
			if model.interactionBusy() {
				return model.deferThreadResume(result.Session), true
			}
			picker.resuming = true
			options := model.resumeOptions
			options.AbortMode = cwdResumeAbortThreadSwitch
			return prepareSessionGeneration(model.ctx, model.runner, result.Session.ThreadID, options, model.operationGeneration), true
		case threadSelectorDelete:
			deleter, _ := model.runner.(threadSessionDeleter)
			return deleteSelectedThread(model.ctx, deleter, result.Authorization), true
		case threadSelectorConfirmDelete:
			model.confirmations.intervene(confirmDelete)
		case threadSelectorPreferencesChanged:
			model.threadSelectorPreferences = result.Preferences
			model.displayDirty = true
			return model.startDisplaySettingsSave(), true
		}
		return nil, true
	}
	switch message.String() {
	case "esc", "q", "ctrl+c":
		if picker.startup {
			return tea.Quit, true
		}
		model.sessionPicker = nil
		return nil, true
	case "up", "k":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = (picker.selected - 1 + len(picker.sessions)) % len(picker.sessions)
		}
		return nil, true
	case "down", "j":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = (picker.selected + 1) % len(picker.sessions)
		}
		return nil, true
	case "pgup":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = max(picker.selected-model.sessionPageSize(), 0)
		}
		return nil, true
	case "pgdown":
		if !picker.loading && !picker.resuming && len(picker.sessions) > 0 {
			picker.selected = min(picker.selected+model.sessionPageSize(), len(picker.sessions)-1)
		}
		return nil, true
	case "enter":
		if !picker.loading && !picker.resuming && picker.err == nil && len(picker.sessions) > 0 {
			picker.resuming = true
			options := model.resumeOptions
			options.AbortMode = cwdResumeAbortThreadSwitch
			return prepareSessionGeneration(model.ctx, model.runner, picker.sessions[picker.selected].ThreadID, options, model.operationGeneration), true
		}
		return nil, true
	}
	return nil, true
}

func (model *tuiModel) restoreTranscript(messages []damessage.Message) {
	model.items = nil
	model.toolItems = map[string]int{}
	model.transparentToolParents = map[string]struct{}{}
	model.currentAssistant = -1
	model.transcriptStart = -1
	model.restoreUsage(messages)
	for _, message := range messages {
		switch message.Role {
		case damessage.RoleHuman:
			if text := message.TextContent(); text != "" {
				model.appendItem(transcriptItem{kind: itemUser, text: text, restored: true})
			}
		case damessage.RoleAssistant:
			if transparency, ok := damessage.MetadataAs[datool.PTCTransparencyMetadata](message.Metadata, datool.PTCTransparencyMetadataKey); ok {
				for _, callID := range transparency.ParentCallIDs {
					if callID != "" {
						model.transparentToolParents[callID] = struct{}{}
					}
				}
			}
			if text := message.TextContent(); text != "" {
				model.appendItem(transcriptItem{kind: itemAssistant, text: text, restored: true, done: true})
			}
			for _, block := range message.Content {
				if block.Type == damessage.BlockServerTool {
					model.addServerToolCall(block, true)
				}
			}
			for _, call := range message.ToolCalls {
				if _, hidden := model.transparentToolParents[call.ID]; hidden {
					continue
				}
				model.appendItem(transcriptItem{
					kind: itemTool, callID: call.ID, name: call.Name, args: compactJSON(call.Arguments), restored: true,
					lifecycle: toolRunning, lineNums: model.showLineNumbers,
				})
				model.toolItems[call.ID] = len(model.items) - 1
			}
		case damessage.RoleTool:
			if _, hidden := model.transparentToolParents[message.ToolCallID]; hidden {
				if artifact, ok := datool.ParsePTCTranscriptArtifact(message.Artifact); ok {
					for _, call := range artifact.Calls {
						if call.CallID == "" || call.Name == "" {
							continue
						}
						status := call.Status
						if status != damessage.ToolStatusError {
							status = damessage.ToolStatusSuccess
						}
						lifecycle := toolSuccess
						if status == damessage.ToolStatusError {
							lifecycle = toolError
						}
						model.appendItem(transcriptItem{
							kind: itemTool, callID: call.CallID, name: call.Name, args: compactJSON(call.Arguments),
							text: call.Output, restored: true, done: true, failed: status == damessage.ToolStatusError,
							lifecycle: lifecycle, lineNums: model.showLineNumbers,
						})
						model.toolItems[call.CallID] = len(model.items) - 1
					}
				}
				continue
			}
			index, exists := model.toolItems[message.ToolCallID]
			if !exists {
				model.appendItem(transcriptItem{
					kind: itemTool, callID: message.ToolCallID, name: message.Name, restored: true,
					lifecycle: toolRunning, lineNums: model.showLineNumbers,
				})
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
			if item.failed {
				item.lifecycle = toolError
			} else {
				item.lifecycle = toolSuccess
			}
		case damessage.RoleSystem:
		}
	}
}

func (model *tuiModel) renderSessionPicker() string {
	picker := model.sessionPicker
	if picker == nil {
		return ""
	}
	if picker.selector != nil && !picker.loading && picker.err == nil {
		return renderThreadSelector(picker.selector, model.width, model.height, model.glyphs)
	}
	width := max(model.width-4, 20)
	title := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Previous sessions")
	subtitle := lipgloss.NewStyle().Foreground(colorMuted).Render("Choose a session to continue")
	lines := []string{title, subtitle, ""}
	switch {
	case picker.loading:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(model.spinner.View()+" Loading sessions"+model.glyphs.Ellipsis))
	case picker.err != nil:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Render(picker.err.Error()))
	case len(picker.sessions) == 0:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No previous sessions yet."))
	default:
		pageSize := model.sessionPageSize()
		start := max(picker.selected-pageSize+1, 0)
		end := min(start+pageSize, len(picker.sessions))
		directoryWidth := min(max(width/4, 12), 28)
		header := fmt.Sprintf("  %-9s  %-9s  %-*s  %7s  %s", "UPDATED", "SESSION", directoryWidth, "DIRECTORY", "MESSAGES", "PROMPT")
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(truncate(header, width-4)))
		for index := start; index < end; index++ {
			session := picker.sessions[index]
			marker := "  "
			if index == picker.selected {
				marker = model.glyphs.Cursor + " "
			}
			current := ""
			if session.ThreadID == model.threadID {
				current = " (current)"
			}
			preview := strings.ReplaceAll(strings.TrimSpace(session.Preview), "\n", " ")
			if preview == "" {
				preview = "(no user prompt)"
			}
			directory := "-"
			if session.Directory != "" {
				directory = truncate(shortPath(session.Directory), directoryWidth)
			}
			metadata := fmt.Sprintf("%s%-9s  %-9s  %-*s  %3d msg  ", marker, sessionAge(session.UpdatedAt), truncate(session.ThreadID, 9), directoryWidth, directory, session.MessageCount)
			line := truncate(metadata+preview+current, width-4)
			style := lipgloss.NewStyle().Foreground(colorBody).Padding(0, 1).Width(max(width-2, 1))
			if index == picker.selected {
				style = style.Background(colorPrimary).Foreground(colorBackground).Bold(true)
			}
			lines = append(lines, style.Render(line))
		}
	}
	lines = append(lines, "")
	separator := "  " + model.glyphs.Bullet + "  "
	navigation := model.glyphs.ArrowUp + model.glyphs.ArrowDown + " navigate"
	hint := navigation + separator + "Enter resume" + separator + "Esc cancel"
	if picker.startup {
		hint = navigation + separator + "Enter resume" + separator + "q quit"
	}
	if picker.resuming {
		hint = model.spinner.View() + " Resuming session" + model.glyphs.Ellipsis
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(hint))
	body := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Foreground(colorBody).Border(model.uiBorder()).
		BorderForeground(colorPrimary).Padding(1, 2).Width(max(model.width-2, 1)).Render(body)
}

func (model *tuiModel) sessionPageSize() int {
	return max(model.height-10, 1)
}

func sessionAge(updated time.Time) string {
	if updated.IsZero() {
		return "unknown"
	}
	difference := time.Since(updated)
	if difference < time.Minute {
		return "now"
	}
	if difference < time.Hour {
		return fmt.Sprintf("%dm ago", int(difference.Minutes()))
	}
	if difference < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(difference.Hours()))
	}
	if difference < 7*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(difference.Hours()/24))
	}
	return updated.Local().Format("Jan 2")
}
