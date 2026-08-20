package dacode

import (
	"context"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dagit"
)

const chatHydrationThreshold = 2

type startupReadyMsg struct{}

type statusBranchMsg struct {
	workingDirectory string
	branch           string
}

type welcomeMCPSnapshotMsg struct {
	servers []mcpViewerServer
	pending bool
	err     error
}

func resolveStatusBranch(ctx context.Context, workingDirectory string) tea.Cmd {
	workingDirectory = strings.TrimSpace(workingDirectory)
	if workingDirectory == "" {
		return nil
	}
	return func() tea.Msg {
		return statusBranchMsg{workingDirectory: workingDirectory, branch: dagit.ResolveBranch(ctx, workingDirectory)}
	}
}

func markStartupReady() tea.Cmd {
	return func() tea.Msg { return startupReadyMsg{} }
}

func (model *tuiModel) startStatusSpinner() tea.Cmd {
	if model == nil {
		panic("dacode: initialized TUI model is required")
	}
	if model.statusSpinnerRunning || !statusSpinnerActive(model.projectStatusBarState()) {
		return nil
	}
	model.statusSpinnerRunning = true
	return model.spinner.Tick
}

func (model *tuiModel) projectStatusBarState() statusBarState {
	state := newStatusBarState()
	switch model.inputMode {
	case inputShell:
		state.InputMode = "shell"
	case inputIncognitoShell:
		state.InputMode = "shell_incognito"
	case inputCommand:
		state.InputMode = "command"
	}
	state.ApprovalMode = model.effectiveApprovalMode().String()
	switch {
	case model.restarting:
		state.Connection = "reconnecting"
	case model.sessionPicker != nil && model.sessionPicker.resuming:
		state.Connection = "resuming"
	case !model.startupReady:
		state.Connection = "connecting"
	}
	state.AgentStatus = model.status
	if state.AgentStatus == "Ready" || strings.HasPrefix(state.AgentStatus, "Queued (") {
		editor := strings.TrimSpace(model.externalEditorName)
		if editor == "" {
			editor = "editor"
		}
		state.AgentStatus = "Ready " + model.glyphs.Bullet + " ctrl+x " + editor +
			" " + model.glyphs.Bullet + " ctrl+c cancel " + model.glyphs.Bullet + " ctrl+d quit"
	}
	state.HookStatus = model.hookStatus
	if model.hookStatus == "" &&
		(model.running || model.shellRunning || model.restarting || model.pluginReloading || model.cancelling) &&
		model.status != "Ready" && !strings.HasPrefix(model.status, "Queued (") {
		state.BusyStatus = model.status
	}
	state.Spinner = state.BusyStatus != "" || state.HookStatus != ""
	state.WorkingDirectory = model.workingDir
	state.Branch = model.statusBranch
	if model.rubric.Criteria != "" {
		state.Rubric = "active"
	}
	state.Model = displayModelName(model.modelName)
	state.Effort = effectiveReasoningEffort(model.runner.ReasoningEffort())
	state.Queued = len(model.inputQueue)
	state.Tokens = int64(max(model.totalTokens, 0))
	state.ContextLimit = int64(max(model.contextWindow, 0))
	state.Approximate = model.cancelling
	if model.costStats.loaded {
		if model.status == "Ready" || strings.HasPrefix(model.status, "Queued (") {
			state.AgentStatus = "Ready"
		}
		state.CacheInput = model.costStats.report.InputTokens
		state.CacheRead = model.costStats.report.CacheReadTokens
		state.CacheWrite = model.costStats.report.CacheWriteTokens
		if model.costStats.report.PricedRequestCount > 0 {
			state.CostUSD = model.costStats.report.CostUSD
		}
	}
	return state
}

func (model *tuiModel) prepareStartupTip() {
	if model.sessionPicker == nil || !model.sessionPicker.startup {
		return
	}
	model.startupTip = newStartupTipState(startupTipResumed, model.externalEditorName, true, uint64(time.Now().UnixNano()), false)
}

func (model *tuiModel) restoreFallbackStartupTip() {
	if model.startupTip.Dismissed {
		return
	}
	model.startupTip = newStartupTipState(startupTipFallback, model.externalEditorName, true, uint64(time.Now().UnixNano()), startupTipsVisible(os.LookupEnv))
}

func (model *tuiModel) suppressStartupTipAfterResume() {
	model.startupTip.Visible = false
	model.startupTip.Dismissed = true
}

func (model *tuiModel) dismissStartupTip() {
	if model.startupTip.dismissOnFirstSubmit() {
		model.relayout()
		model.refreshTranscript()
	}
}

func (model *tuiModel) renderStartupTip() string {
	if !model.startupTip.Visible || model.startupTip.Dismissed || model.startupTip.Text == "" {
		return ""
	}
	rendered := lipgloss.NewStyle().Foreground(colorMuted).Italic(true).PaddingLeft(1).Render("Tip: " + model.startupTip.Text)
	return truncateDisplayLine(rendered, max(model.width, 1), model.glyphs.Ellipsis)
}

func (model *tuiModel) scrollbarVisible() bool {
	return model.showScrollbar && model.charset != charsetASCII
}

func (model *tuiModel) refreshWelcomeMCP() tea.Cmd {
	controller := model.mcpController
	if controller == nil {
		return nil
	}
	return func() tea.Msg {
		servers, pending, err := controller.SnapshotMCP()
		return welcomeMCPSnapshotMsg{servers: servers, pending: pending, err: err}
	}
}

func (model *tuiModel) applyWelcomeMCPSnapshot(message welcomeMCPSnapshotMsg) {
	if message.err != nil {
		return
	}
	model.welcomeMCPServers = append(model.welcomeMCPServers[:0], message.servers...)
	model.welcomeMCPPending = message.pending
}

func (model *tuiModel) configureWelcomeProject(label, projectURL string) bool {
	label = boundedTraceValue(label, 256)
	projectURL = strings.TrimSpace(projectURL)
	if label == "" || !validTraceLink(projectURL) {
		model.welcomeProjectLabel = ""
		model.welcomeProjectURL = ""
		return false
	}
	model.welcomeProjectLabel = label
	model.welcomeProjectURL = projectURL
	return true
}

func (model *tuiModel) projectWelcomeBannerState() welcomeBannerState {
	state := welcomeBannerState{
		Version: buildVersion(), Model: model.modelName, WorkingDirectory: model.workingDir,
		ProjectLabel: model.welcomeProjectLabel, ProjectURL: model.welcomeProjectURL,
		Agent: model.agentName, ApprovalMode: approvalModeDisplay(model.effectiveApprovalMode()), ThreadID: model.threadID,
		ShowVersion: true, ShowModel: true, ShowWorkingDirectory: true, ShowProject: model.welcomeProjectURL != "", ShowThreadID: true,
	}
	for _, server := range model.welcomeMCPServers {
		state.MCPTools += len(server.Tools)
		switch server.Status {
		case mcpViewerUnauthenticated:
			state.MCPLoginRequired++
		case mcpViewerError:
			state.MCPErrors++
		case mcpViewerAwaitingReconnect:
			state.MCPAwaitingReconnect++
		}
		if server.PendingReconnect && server.Status != mcpViewerAwaitingReconnect {
			state.MCPAwaitingReconnect++
		}
	}
	if model.welcomeMCPPending && state.MCPAwaitingReconnect == 0 {
		state.MCPAwaitingReconnect = 1
	}
	starting := !model.startupReady || model.restarting || (model.sessionPicker != nil && model.sessionPicker.resuming)
	if model.startupFailed {
		state.StartupError = "The agent could not start. Review the error below and retry."
	} else {
		state.Ready = !starting
	}
	return state
}

func approvalModeDisplay(mode approvalMode) string {
	switch mode {
	case approvalAuto:
		return "Auto"
	case approvalYOLO:
		return "YOLO"
	default:
		return "Manual"
	}
}

func (model *tuiModel) refreshTranscriptWithAnchor(insertedAbove bool) {
	if !model.ready {
		return
	}
	oldLines := model.viewport.TotalLineCount()
	model.chatScroll.updateLayout(oldLines, model.viewport.Height())
	var anchor transcriptScrollAnchor
	anchored := false
	if !model.chatScroll.FollowBottom {
		anchor, anchored = transcriptAnchorAt(model.transcriptLayout, model.chatScroll.Offset)
	}
	rendered := model.renderTranscript()
	model.viewport.SetContent(rendered)
	newLines := model.viewport.TotalLineCount()
	inserted := 0
	if insertedAbove {
		inserted = max(newLines-oldLines, 0)
	}
	model.chatScroll.updateLayoutPreservingAnchor(newLines, model.viewport.Height(), inserted)
	if anchored && !insertedAbove {
		if offset, ok := transcriptOffsetForAnchor(model.transcriptLayout, anchor); ok {
			model.chatScroll.userScrolled(offset)
		}
	}
	model.viewport.SetYOffset(model.chatScroll.Offset)
}

func transcriptAnchorAt(layout []transcriptBlockLayout, offset int) (transcriptScrollAnchor, bool) {
	if len(layout) == 0 || offset < 0 {
		return transcriptScrollAnchor{}, false
	}
	selected := -1
	for index := range layout {
		if layout[index].start > offset {
			break
		}
		selected = index
	}
	if selected < 0 {
		return transcriptScrollAnchor{}, false
	}
	block := layout[selected]
	return transcriptScrollAnchor{id: block.id, line: min(max(offset-block.start, 0), block.lines)}, true
}

func transcriptOffsetForAnchor(layout []transcriptBlockLayout, anchor transcriptScrollAnchor) (int, bool) {
	for _, block := range layout {
		if block.id == anchor.id {
			return block.start + min(max(anchor.line, 0), block.lines), true
		}
	}
	return 0, false
}

func (model *tuiModel) hydrateChatHistory() bool {
	if !model.hydrateOlderTranscript() {
		return false
	}
	model.refreshTranscriptWithAnchor(true)
	return true
}

func (model *tuiModel) handleChatWheel(message tea.Mouse) bool {
	direction := 0
	switch message.Button {
	case tea.MouseWheelUp:
		direction = -1
	case tea.MouseWheelDown:
		direction = 1
	default:
		return false
	}
	model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height())
	model.chatScroll.userScrolled(model.viewport.YOffset())
	model.viewport.SetYOffset(model.chatScroll.wheel(direction))
	if model.chatScroll.shouldHydrateOlder(chatHydrationThreshold) {
		model.hydrateChatHistory()
	}
	return true
}

func (model *tuiModel) pageChat(direction int) {
	model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height())
	model.chatScroll.userScrolled(model.viewport.YOffset())
	if direction < 0 {
		offset, hydrate := model.chatScroll.pageUp(chatHydrationThreshold)
		model.viewport.SetYOffset(offset)
		if hydrate {
			model.hydrateChatHistory()
		}
		return
	}
	model.viewport.SetYOffset(model.chatScroll.scrollLines(max(model.viewport.Height()-1, 1)))
}

func (model *tuiModel) handleWelcomeMouse(message tea.Mouse) (tea.Cmd, bool) {
	if message.Button != tea.MouseLeft || model.welcomeInteractionBlocked() {
		return nil, false
	}
	for _, target := range model.welcomeScreenHitTargets {
		if message.Y != target.Y || message.X < target.X || message.X >= target.X+target.Width {
			continue
		}
		switch target.Kind {
		case welcomeHitVersion:
			return tea.Batch(
				model.stageTerminalSequences(osc52ClipboardSequence(target.Value), ""),
				model.notify("Version copied.", toastInfo, ""),
			), true
		case welcomeHitThread:
			return tea.Batch(
				model.stageTerminalSequences(osc52ClipboardSequence(model.threadID), ""),
				model.notify("Thread ID copied.", toastInfo, ""),
			), true
		case welcomeHitMCP:
			return model.openMCPViewer(), true
		case welcomeHitProject:
			return model.openTraceURL(model.welcomeProjectURL), true
		}
	}
	return nil, false
}

func (model *tuiModel) cacheWelcomeScreenTargets(view string) {
	model.welcomeScreenHitTargets = model.welcomeScreenHitTargets[:0]
	clip := max(lipgloss.Height(view)-model.height, 0)
	for _, target := range model.welcomeHitTargets {
		target.Y = target.Y - model.viewport.YOffset() - clip
		if target.Y >= 0 && target.Y < min(model.viewport.Height(), model.height) && target.Width > 0 {
			model.welcomeScreenHitTargets = append(model.welcomeScreenHitTargets, target)
		}
	}
}

func (model *tuiModel) welcomeInteractionBlocked() bool {
	return model.debugConsole != nil || model.onboarding != nil || model.updateModal != nil || model.notificationCenter != nil ||
		model.notificationSettings != nil || model.authManager != nil && model.authManager.open || model.mcpViewer != nil ||
		model.mcpLogin != nil || model.mcpReconnectPrompt != nil || model.mcpErrorDetail != "" || model.restartPrompt != nil ||
		model.sessionPicker != nil || model.modelSelector != nil || model.themePicker != nil || model.effortPicker != nil || model.agentPicker != nil ||
		model.skillTrust != nil || model.contextScreen || model.goalReview != nil || model.askUser != nil || model.approval != nil ||
		model.autoModeNotice || model.yoloModeNotice || model.pluginManager != nil || model.pluginReloadPrompt || model.pluginReloading ||
		model.restarting || model.installSelector != nil
}
