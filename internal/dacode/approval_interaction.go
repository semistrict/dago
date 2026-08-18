package dacode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

func newApprovalState(requests []dagent.ApprovalRequest) *approvalState {
	reason := textarea.New()
	reason.Placeholder = "Reason (Enter to submit, Esc to cancel)"
	reason.Prompt = "> "
	reason.ShowLineNumbers = false
	reason.CharLimit = maxApprovalReasonCharacters
	reason.SetHeight(1)
	reason.MaxHeight = 1
	reason.FocusedStyle.Base = lipgloss.NewStyle().Foreground(colorBody)
	reason.FocusedStyle.CursorLine = lipgloss.NewStyle()
	reason.FocusedStyle.Text = lipgloss.NewStyle().Foreground(colorBody)
	reason.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(colorError).Bold(true)
	reason.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colorMuted)
	reason.BlurredStyle = reason.FocusedStyle
	return &approvalState{requests: requests, reason: reason}
}

func (model *tuiModel) presentApproval(requests []dagent.ApprovalRequest) *approvalState {
	model.redactSensitiveApprovalTranscript(requests)
	state := newApprovalState(requests)
	state.previews = approvalWorkspacePreviews(model.ctx, model.workingDir, requests)
	now := time.Now()
	mode := model.effectiveApprovalMode()
	if mode == approvalManual && model.userIsTyping(now) {
		state.deferred = true
		state.typingProtected = true
		state.deferredAt = now
		state.deferGeneration = 1
		model.status = "Waiting for typing to finish"
	} else if mode == approvalAuto {
		state.preparingReview = true
	}
	model.approval = state
	return state
}

func (model *tuiModel) redactSensitiveApprovalTranscript(requests []dagent.ApprovalRequest) {
	for _, request := range requests {
		arguments := approvalArgumentMap(request.Call.Arguments)
		path, _ := arguments["file_path"].(string)
		if path == "" {
			path, _ = arguments["path"].(string)
		}
		if !sensitiveApprovalPath(path) {
			continue
		}
		index, exists := model.toolItems[request.Call.ID]
		if !exists || index < 0 || index >= len(model.items) {
			continue
		}
		encoded, err := json.Marshal(map[string]string{"file_path": path, "contents": "hidden"})
		if err == nil {
			model.items[index].args = string(encoded)
		}
	}
}

func (model *tuiModel) handleApprovalMenuKey(message tea.KeyMsg) (tea.Cmd, bool) {
	state := model.approval
	if state == nil || !model.manualApprovalVisible() || !state.ready || state.deferred || state.typingProtected || state.preparingReview || state.reviewing {
		return nil, false
	}
	options := approvalOptions(state)
	selectOption := func(index int) tea.Cmd {
		if index < 0 || index >= len(options) {
			return nil
		}
		state.selected = index
		switch options[index].kind {
		case "approve":
			return model.resolveApproval(true)
		case "reject":
			return model.resolveApproval(false)
		case "manual":
			return model.setApprovalMode(approvalManual)
		default:
			return model.enableAutoForApproval()
		}
	}
	switch message.String() {
	case "up", "k":
		state.selected = (state.selected + len(options) - 1) % len(options)
		model.refreshTranscript()
		return nil, true
	case "down", "j":
		state.selected = (state.selected + 1) % len(options)
		model.refreshTranscript()
		return nil, true
	case "enter":
		return selectOption(state.selected), true
	case "1", "y", "Y":
		return selectOption(0), true
	case "2", "a", "A":
		return selectOption(1), true
	case "3", "n", "N", "esc":
		return selectOption(2), true
	case "tab":
		model.startApprovalReason()
		return nil, true
	case "e", "E":
		for _, request := range state.requests {
			arguments := approvalArgumentMap(request.Call.Arguments)
			command, _ := arguments["command"].(string)
			if request.Call.Name == "execute" && approvalCommandExpandable(command) {
				state.commandExpanded = !state.commandExpanded
				model.refreshTranscript()
				break
			}
		}
		return nil, true
	default:
		return nil, false
	}
}

func (model *tuiModel) enableAutoForApproval() tea.Cmd {
	if model.approval == nil {
		return nil
	}
	model.approvalAutoApproveAfterNotice = true
	if model.approvalModeStore == nil {
		_ = model.applyApprovalMode(approvalAuto)
		if model.autoModeNotice {
			return nil
		}
		model.approvalAutoApproveAfterNotice = false
		return model.resolveApproval(true)
	}
	store := model.approvalModeStore
	threadID := model.threadID
	model.approvalModeGeneration++
	generation := model.approvalModeGeneration
	model.approvalModePending = approvalAuto
	model.approvalModePendingSet = true
	store.registerGeneration(threadID, generation)
	return func() tea.Msg {
		return approvalModeSavedMsg{
			threadID: threadID, mode: approvalAuto, generation: generation, approvePending: true,
			err: store.saveGeneration(threadID, approvalAuto, generation),
		}
	}
}

func (model *tuiModel) userIsTyping(now time.Time) bool {
	return !model.lastTypedAt.IsZero() && now.Sub(model.lastTypedAt) < approvalTypingIdleDuration
}

func isComposerTypingKey(message tea.KeyMsg) bool {
	if message.Paste || message.Type == tea.KeyRunes {
		return true
	}
	switch message.String() {
	case "backspace", "delete", "ctrl+backspace", "alt+backspace", "ctrl+w", "ctrl+j", "shift+enter", "alt+enter", "ctrl+enter":
		return true
	default:
		return false
	}
}

func (model *tuiModel) noteComposerTyping(message tea.KeyMsg) tea.Cmd {
	if !isComposerTypingKey(message) || model.askUser != nil || model.sessionPicker != nil || model.agentPicker != nil || model.effortPicker != nil || model.contextScreen {
		return nil
	}
	if model.approval != nil && !model.approval.typingProtected && !model.approval.preparingReview && !model.approval.reviewing {
		return nil
	}
	model.lastTypedAt = time.Now()
	if model.approval == nil || !model.approval.typingProtected {
		return nil
	}
	model.approval.deferGeneration++
	return model.scheduleDeferredApproval()
}

func (model *tuiModel) scheduleDeferredApproval() tea.Cmd {
	state := model.approval
	if state == nil || !state.typingProtected {
		return nil
	}
	now := time.Now()
	idleRemaining := approvalTypingIdleDuration - now.Sub(model.lastTypedAt)
	delay := idleRemaining
	if state.deferred {
		deadlineRemaining := approvalDeferralTimeout - now.Sub(state.deferredAt)
		delay = min(idleRemaining, deadlineRemaining)
	}
	if delay < 0 {
		delay = 0
	}
	generation := state.deferGeneration
	return tea.Tick(delay, func(time.Time) tea.Msg { return approvalDeferredTickMsg{generation: generation} })
}

func (model *tuiModel) finishDeferredApproval(message approvalDeferredTickMsg) tea.Cmd {
	state := model.approval
	if state == nil || !state.typingProtected || message.generation != state.deferGeneration {
		return nil
	}
	now := time.Now()
	if model.userIsTyping(now) {
		if now.Sub(state.deferredAt) >= approvalDeferralTimeout {
			state.deferred = false
			if state.ready {
				model.status = "Review action"
			}
			model.relayout()
			model.refreshTranscript()
		}
		state.deferGeneration++
		return model.scheduleDeferredApproval()
	}
	state.deferred = false
	state.typingProtected = false
	if state.ready {
		model.status = "Review action"
	} else {
		model.status = "Pausing for review"
	}
	model.relayout()
	model.refreshTranscript()
	return nil
}

func (model *tuiModel) deferApprovalAfterReviewFailure() tea.Cmd {
	state := model.approval
	if state == nil || state.deferred || !model.userIsTyping(time.Now()) {
		return nil
	}
	state.deferred = true
	state.typingProtected = true
	state.deferredAt = time.Now()
	state.deferGeneration++
	model.status = "Waiting for typing to finish"
	model.relayout()
	model.refreshTranscript()
	return model.scheduleDeferredApproval()
}

func (model *tuiModel) startApprovalReason() {
	state := model.approval
	if state == nil || !state.ready || state.deferred || state.reasonMode {
		return
	}
	state.reasonMode = true
	state.reasonWarning = ""
	state.reason.Reset()
	state.reason.Focus()
	model.status = "Describe rejection"
	model.relayout()
	model.refreshTranscript()
}

func (model *tuiModel) closeApprovalReason() {
	state := model.approval
	if state == nil || !state.reasonMode {
		return
	}
	state.reasonMode = false
	state.reasonWarning = ""
	state.reason.Blur()
	model.status = "Review action"
	model.relayout()
	model.refreshTranscript()
}

func (model *tuiModel) handleApprovalReasonKey(message tea.KeyMsg) (tea.Cmd, bool) {
	state := model.approval
	if state == nil || !state.reasonMode {
		return nil, false
	}
	switch message.String() {
	case "enter":
		reason := strings.TrimSpace(state.reason.Value())
		if utf8.RuneCountInString(reason) > maxApprovalReasonCharacters || len(reason) > maxApprovalReasonBytes {
			state.reasonWarning = fmt.Sprintf("Reason must be at most %d characters and %d bytes.", maxApprovalReasonCharacters, maxApprovalReasonBytes)
			model.refreshTranscript()
			return nil, true
		}
		state.reasonMode = false
		return model.resolveApprovalWithReason(false, reason), true
	case "esc", "n", "N":
		model.closeApprovalReason()
		return nil, true
	case "tab":
		return nil, true
	case "ctrl+j", "shift+enter", "alt+enter", "ctrl+enter":
		return nil, true
	}
	return nil, false
}

func (model *tuiModel) sanitizeApprovalReasonInput() {
	state := model.approval
	if state == nil || !state.reasonMode {
		return
	}
	value := state.reason.Value()
	safe := unicodesecurity.RenderTerminalSafe(strings.ReplaceAll(value, "\n", " "))
	if safe == value {
		return
	}
	state.reason.SetValue(safe)
	state.reason.CursorEnd()
}

func frameApprovalRejectReason(reason string) string {
	return "User rejected the tool call with reason: " + reason
}

func sanitizeApprovalRejectReason(reason string) string {
	singleLine := strings.ReplaceAll(strings.TrimSpace(reason), "\n", " ")
	safe := unicodesecurity.RenderTerminalSafe(singleLine)
	var bounded strings.Builder
	bounded.Grow(min(len(safe), maxApprovalReasonBytes))
	characters := 0
	bytes := 0
	for _, character := range safe {
		size := utf8.RuneLen(character)
		if characters == maxApprovalReasonCharacters || bytes+size > maxApprovalReasonBytes {
			break
		}
		bounded.WriteRune(character)
		characters++
		bytes += size
	}
	return bounded.String()
}
