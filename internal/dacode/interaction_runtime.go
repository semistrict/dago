package dacode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	dagoapi "github.com/semistrict/dago"
	"github.com/semistrict/dago/damessage"
)

type queuedInputDispatchMsg struct{ Input queuedInput }
type deferredThreadResumeMsg struct{ ThreadID string }
type deferredAgentSwitchMsg struct {
	Name     string
	ThreadID string
}
type deferredMCPReconnectMsg struct{}

type forceClearPending struct {
	generation uint64
	threadID   string
	stream     eventStream
	warned     bool
	retries    uint8
	scheduled  bool
	reading    bool
	finalizing bool
}

type forceClearFinalizedMsg struct {
	generation uint64
	err        error
}

type forceClearDrainTimeoutMsg struct{ generation uint64 }
type forceClearRetryMsg struct{ generation uint64 }

const (
	maximumForceClearQuarantines = 16
	maximumForceClearRetries     = 6
	initialForceClearRetryDelay  = 250 * time.Millisecond
	maximumForceClearRetryDelay  = 4 * time.Second
)

type composerAttachmentUndo struct {
	pasteBindings map[string]string
	inputMedia    map[string]damessage.ContentBlock
	imageSequence int
	videoSequence int
	ready         bool
}

type deferredDrainProgress struct {
	actions        []deferredActionCompletedMsg
	prompt         tea.Msg
	promptFailed   bool
	waiting        deferredActionKind
	active         deferredActionCompletedMsg
	waitGeneration uint64
	compacting     bool
}

func (model *tuiModel) interactionBusy() bool {
	return (model.deferredDrain != nil && !model.applyingDeferred) || model.running || model.shellRunning || model.approval != nil || model.askUser != nil ||
		model.goalReview != nil || model.skillTrust != nil || model.pluginReloading
}

func (model *tuiModel) deferThreadResume(session sessionInfo) tea.Cmd {
	threadID := validThreadSelectorID(session.ThreadID)
	if threadID == "" {
		return model.notify("The selected thread is no longer available.", toastWarning, "")
	}
	model.sessionPicker = nil
	model.deferredActions.deferAction(deferredAction{
		Kind:    deferredThreadSwitch,
		Payload: deferredActionPayload{Identity: threadID},
		ExecutePayload: func(payload deferredActionPayload) tea.Msg {
			return deferredThreadResumeMsg{ThreadID: payload.Identity}
		},
	})
	return model.notify("Thread switch queued for the next idle point.", toastInfo, "")
}

func (model *tuiModel) applyDeferredThreadResume(message deferredThreadResumeMsg) tea.Cmd {
	if validThreadSelectorID(message.ThreadID) != message.ThreadID {
		return model.notify("The queued thread switch expired.", toastWarning, "")
	}
	model.sessionPicker = &sessionPickerState{resuming: true}
	options := model.resumeOptions
	options.AbortMode = cwdResumeAbortThreadSwitch
	return prepareSessionGeneration(model.ctx, model.runner, message.ThreadID, options, model.operationGeneration)
}

func (model *tuiModel) deferAgentSwitch(name, threadID string) tea.Cmd {
	if validThreadSelectorID(threadID) != threadID || strings.TrimSpace(name) == "" {
		return model.notify("The selected agent is no longer available.", toastWarning, "")
	}
	model.agentPicker = nil
	model.deferredActions.deferAction(deferredAction{
		Kind:    deferredAgentSwitch,
		Payload: deferredActionPayload{Identity: name, Arguments: []string{threadID}},
		ExecutePayload: func(payload deferredActionPayload) tea.Msg {
			return deferredAgentSwitchMsg{Name: payload.Identity, ThreadID: payload.Arguments[0]}
		},
	})
	return model.notify("Agent switch queued for the next idle point.", toastInfo, "")
}

func (model *tuiModel) applyDeferredAgentSwitch(message deferredAgentSwitchMsg) tea.Cmd {
	model.status = "Switching agent"
	return switchAgentGeneration(model.ctx, model.runner, message.Name, message.ThreadID, model.operationGeneration)
}

func (model *tuiModel) deferMCPReconnect() tea.Cmd {
	model.mcpReconnectPrompt = nil
	model.deferredActions.deferAction(deferredAction{
		Kind:           deferredMCPReconnect,
		Payload:        deferredActionPayload{},
		ExecutePayload: func(deferredActionPayload) tea.Msg { return deferredMCPReconnectMsg{} },
	})
	return model.notify("MCP reconnect queued for the next idle point.", toastInfo, "")
}

func (model *tuiModel) composerRuneOffset() int {
	lines := strings.Split(model.composer.Value(), "\n")
	line := model.composer.Line()
	if line < 0 || line >= len(lines) {
		return -1
	}
	lineInfo := model.composer.LineInfo()
	offset := lineInfo.StartColumn + lineInfo.ColumnOffset
	for index := 0; index < line; index++ {
		offset += len([]rune(lines[index])) + 1
	}
	return offset
}

func (model *tuiModel) setComposerRuneOffset(offset int) {
	lines := strings.Split(model.composer.Value(), "\n")
	if len(lines) == 0 {
		return
	}
	offset = max(offset, 0)
	targetLine, targetColumn := len(lines)-1, len([]rune(lines[len(lines)-1]))
	remaining := offset
	for index, line := range lines {
		length := len([]rune(line))
		if remaining <= length {
			targetLine, targetColumn = index, remaining
			break
		}
		remaining -= length + 1
	}
	for model.composer.Line() > targetLine {
		model.composer.CursorUp()
	}
	for model.composer.Line() < targetLine {
		model.composer.CursorDown()
	}
	model.composer.SetCursorColumn(targetColumn)
}

func (model *tuiModel) clearComposerWithUndo() bool {
	attachments, ok := boundedComposerAttachmentSnapshot(model.composer.Value(), model.pasteBindings, model.inputMedia)
	if !ok {
		return false
	}
	cleared, ok := model.draftUndo.clear(model.composer.Value())
	if !ok {
		return false
	}
	attachments.imageSequence = model.imageSequence
	attachments.videoSequence = model.videoSequence
	attachments.ready = true
	model.draftAttachmentUndo = attachments
	model.composer.SetValue(cleared)
	model.pasteBindings = map[string]string{}
	model.inputMedia = map[string]damessage.ContentBlock{}
	model.imageSequence = 0
	model.videoSequence = 0
	model.inputCompletion = completionState{}
	model.setInputMode(inputNormal)
	model.relayout()
	model.refreshTranscript()
	return true
}

func boundedComposerAttachmentSnapshot(draft string, pasteBindings map[string]string, inputMedia map[string]damessage.ContentBlock) (composerAttachmentUndo, bool) {
	total := len(draft)
	if total > maximumDraftUndoBytes {
		return composerAttachmentUndo{}, false
	}
	add := func(size int) bool {
		if size < 0 || size > maximumDraftUndoBytes-total {
			return false
		}
		total += size
		return true
	}
	snapshot := composerAttachmentUndo{
		pasteBindings: make(map[string]string, len(pasteBindings)),
		inputMedia:    make(map[string]damessage.ContentBlock, len(inputMedia)),
	}
	for placeholder, value := range pasteBindings {
		if !add(16 + len(placeholder) + len(value)) {
			return composerAttachmentUndo{}, false
		}
		snapshot.pasteBindings[strings.Clone(placeholder)] = strings.Clone(value)
	}
	for placeholder, block := range inputMedia {
		if !add(32 + len(placeholder) + len(block.Type) + len(block.ID) + len(block.Text) + len(block.Reasoning) + len(block.URL) + len(block.Data) +
			len(block.MIMEType) + len(block.Name) + len(block.NonStandard)) {
			return composerAttachmentUndo{}, false
		}
		cloned := block
		cloned.Data = append([]byte(nil), block.Data...)
		cloned.NonStandard = append([]byte(nil), block.NonStandard...)
		if block.Index != nil {
			index := *block.Index
			cloned.Index = &index
		}
		cloned.Citations = make([]damessage.Citation, len(block.Citations))
		for index, citation := range block.Citations {
			if !add(32 + len(citation.ID) + len(citation.URL) + len(citation.Title) + len(citation.CitedText)) {
				return composerAttachmentUndo{}, false
			}
			cloned.Citations[index] = citation
			if citation.StartIndex != nil {
				value := *citation.StartIndex
				cloned.Citations[index].StartIndex = &value
			}
			if citation.EndIndex != nil {
				value := *citation.EndIndex
				cloned.Citations[index].EndIndex = &value
			}
		}
		cloned.Extra = make(map[string]json.RawMessage, len(block.Extra))
		for key, value := range block.Extra {
			if !add(16 + len(key) + len(value)) {
				return composerAttachmentUndo{}, false
			}
			cloned.Extra[strings.Clone(key)] = append(json.RawMessage(nil), value...)
		}
		snapshot.inputMedia[strings.Clone(placeholder)] = cloned
	}
	return snapshot, true
}

func (model *tuiModel) undoComposerClear() bool {
	value, ok := model.draftUndo.undo()
	if !ok {
		return false
	}
	model.composer.SetValue(value)
	if model.draftAttachmentUndo.ready {
		model.pasteBindings = model.draftAttachmentUndo.pasteBindings
		model.inputMedia = model.draftAttachmentUndo.inputMedia
		model.imageSequence = model.draftAttachmentUndo.imageSequence
		model.videoSequence = model.draftAttachmentUndo.videoSequence
	}
	model.draftAttachmentUndo = composerAttachmentUndo{}
	model.updateInputModeFromValue()
	model.updateInputCompletion()
	model.relayout()
	model.refreshTranscript()
	return true
}

func (model *tuiModel) discardDraftUndo() {
	model.draftUndo.discard()
	model.draftAttachmentUndo = composerAttachmentUndo{}
}

func (model *tuiModel) forwardDeleteComposer() bool {
	if model.deletePastePlaceholder(false) {
		return true
	}
	runes := []rune(model.composer.Value())
	offset := model.composerRuneOffset()
	if offset < 0 || offset >= len(runes) {
		return false
	}
	model.composer.SetValue(string(append(runes[:offset], runes[offset+1:]...)))
	model.setComposerRuneOffset(offset)
	model.updateInputModeFromValue()
	model.updateInputCompletion()
	model.relayout()
	model.refreshTranscript()
	return true
}

func (model *tuiModel) applyDeferredCompletion(completion deferredActionCompletedMsg) tea.Cmd {
	if completion.Failed || completion.Message == nil {
		return model.notify("A queued change could not be applied.", toastError, "")
	}
	updated, command := model.Update(completion.Message)
	if updated != model {
		*model = *updated
	}
	return command
}

func (model *tuiModel) finishDeferredDrain(message deferredDrainCompletedMsg) tea.Cmd {
	if model.deferredDrain != nil {
		return model.notify("Queued changes are already being applied.", toastWarning, "")
	}
	model.deferredDrain = &deferredDrainProgress{
		actions: append([]deferredActionCompletedMsg(nil), message.Actions...),
		prompt:  message.Prompt, promptFailed: message.PromptFailed,
	}
	return model.advanceDeferredDrain()
}

func (model *tuiModel) advanceDeferredDrain() tea.Cmd {
	progress := model.deferredDrain
	if progress == nil || progress.waiting != "" {
		return nil
	}
	var notices []tea.Cmd
	for len(progress.actions) > 0 {
		completion := progress.actions[0]
		progress.actions = progress.actions[1:]
		if completion.Failed || completion.Message == nil {
			notices = append(notices, model.notify("A queued change could not be applied.", toastError, ""))
			continue
		}
		progress.active = completion
		progress.waitGeneration = model.operationGeneration
		model.applyingDeferred = true
		updated, command := model.Update(completion.Message)
		model.applyingDeferred = false
		if updated != model {
			*model = *updated
		}
		if command == nil {
			progress.active = deferredActionCompletedMsg{}
			continue
		}
		switch completion.Kind {
		case deferredModelSwitch, deferredThreadSwitch, deferredAgentSwitch, deferredMCPReconnect:
			progress.waiting = completion.Kind
			return tea.Sequence(append(notices, command)...)
		default:
			notices = append(notices, command)
			progress.active = deferredActionCompletedMsg{}
		}
	}
	model.deferredDrain = nil
	if progress.promptFailed {
		notices = append(notices, model.notify("The queued prompt could not be started.", toastError, ""))
	} else if progress.prompt != nil {
		updated, command := model.Update(progress.prompt)
		if updated != model {
			*model = *updated
		}
		if command != nil {
			notices = append(notices, command)
		}
	} else if command := model.drainInputQueue(); command != nil {
		notices = append(notices, command)
	}
	return tea.Sequence(notices...)
}

func (model *tuiModel) finishDeferredAsync(kind deferredActionKind, matches bool, command tea.Cmd) tea.Cmd {
	progress := model.deferredDrain
	if progress == nil || progress.waiting != kind || !matches {
		return command
	}
	progress.waiting = ""
	progress.active = deferredActionCompletedMsg{}
	next := model.advanceDeferredDrain()
	if command == nil {
		return next
	}
	if next == nil {
		return command
	}
	return tea.Sequence(command, next)
}

func (model *tuiModel) deferredModelPreferenceMatches(message modelPreferenceMsg) bool {
	progress := model.deferredDrain
	if progress == nil || progress.waiting != deferredModelSwitch || len(progress.active.Payload.Arguments) < 1 {
		return false
	}
	actionValue, actionErr := strconv.Atoi(progress.active.Payload.Arguments[0])
	if actionErr == nil && modelSelectorAction(actionValue) == modelSelectorSelect {
		return len(progress.active.Payload.Arguments) >= 2 && message.action == "recent" &&
			message.spec == progress.active.Payload.Arguments[1] && message.recentGeneration == model.modelRecentGeneration
	}
	selectorID, err := strconv.ParseUint(progress.active.Payload.Identity, 10, 64)
	return err == nil && message.write.SelectorID == selectorID && message.write.Generation == progress.active.Payload.Generation
}

func (model *tuiModel) deferredGenerationMatches(kind deferredActionKind, generation uint64) bool {
	progress := model.deferredDrain
	return progress != nil && progress.waiting == kind && generation == progress.waitGeneration
}

func (model *tuiModel) deferredThreadResultMatches(message sessionLoadedMsg) bool {
	progress := model.deferredDrain
	if !model.deferredGenerationMatches(deferredThreadSwitch, message.generation) {
		return false
	}
	return message.session.ThreadID == "" || message.session.ThreadID == progress.active.Payload.Identity
}

func (model *tuiModel) deferredAgentResultMatches(message agentSwitchedMsg) bool {
	progress := model.deferredDrain
	return model.deferredGenerationMatches(deferredAgentSwitch, message.generation) &&
		message.name == progress.active.Payload.Identity && len(progress.active.Payload.Arguments) == 1 &&
		message.threadID == progress.active.Payload.Arguments[0]
}

func (model *tuiModel) deferredMCPResultMatches(message mcpRuntimeMsg) bool {
	return message.action == "reconnect" && model.deferredGenerationMatches(deferredMCPReconnect, message.generation)
}

func (model *tuiModel) deferredThreadWaiting() bool {
	return model.deferredDrain != nil && model.deferredDrain.waiting == deferredThreadSwitch
}

func (model *tuiModel) deferredCompactionWaiting() bool {
	return model.deferredDrain != nil && model.deferredDrain.waiting == deferredThreadSwitch && model.deferredDrain.compacting
}

func (model *tuiModel) deferredQueueNotice() string {
	count := model.deferredActions.length()
	if count == 0 {
		return ""
	}
	return fmt.Sprintf("%d queued change(s)", count)
}

func (model *tuiModel) applyClearCommand(force bool) tea.Cmd {
	if force && !model.running && !model.shellRunning && len(model.forceClearPending) > 0 {
		commands := []tea.Cmd{model.notify("Retrying quarantined cleanup before another force clear.", toastWarning, "")}
		for generation, pending := range model.forceClearPending {
			if pending == nil || pending.scheduled || pending.reading || pending.finalizing {
				continue
			}
			pending.retries = 0
			commands = append(commands, model.retryForceClear(generation))
		}
		return tea.Batch(commands...)
	}
	if force && len(model.forceClearPending) >= maximumForceClearQuarantines {
		return model.notify("Force clear is unavailable until earlier quarantined cleanup completes.", toastError, "")
	}
	plan := planClearCommand(force, clearCommandState{
		AgentRunning: model.running || model.deferredDrain != nil, ShellRunning: model.shellRunning, ApprovalPending: model.approval != nil,
		QuestionPending: model.askUser != nil, GoalPending: model.goalReview != nil,
		QueuedMessages: len(model.inputQueue), DeferredActions: model.deferredActions.length(),
		PreviousThread: model.threadID, HasCheckpoint: model.threadHasCheckpoint,
	})
	if plan.QueueUntilIdle {
		model.inputQueue = append(model.inputQueue, queuedInput{mode: inputCommand, value: "clear", display: "clear"})
		model.status = fmt.Sprintf("Queued (%d)", len(model.inputQueue))
		return model.notify("Clear queued until active work finishes.", toastInfo, "")
	}
	threadID, err := model.createThreadID()
	if err != nil {
		return model.notify("A new thread could not be created; the current thread was left unchanged.", toastError, "")
	}
	oldThreadID := model.threadID
	oldGeneration := model.operationGeneration
	oldStream := model.stream
	hadPendingInterrupt := model.approval != nil || model.askUser != nil
	model.operationGeneration++
	if model.turnCancel != nil {
		model.turnCancel()
	}
	if model.shellCancel != nil {
		model.shellCancel()
	}
	model.threadID = threadID
	model.threadHasCheckpoint = false
	approvalModeErr := model.startNewApprovalThread(threadID)
	model.running = false
	model.cancelling = false
	model.stream = nil
	model.turnCancel = nil
	model.shellRunning = false
	model.shellCancel = nil
	model.shellContext = nil
	model.approval = nil
	model.askUser = nil
	model.goalReview = nil
	model.resumeController = nil
	model.sessionPicker = nil
	if model.modelSelector != nil && model.modelSelector.pendingWrite != nil {
		model.deferredModelSelector = model.modelSelector
	} else if model.deferredModelSelector == nil || model.deferredModelSelector.pendingWrite == nil {
		model.deferredModelSelector = nil
	}
	model.modelSelector = nil
	model.installSelector = nil
	model.installPending = nil
	model.inputQueue = nil
	model.deferredDrain = nil
	if force {
		model.deferredActions.discardFor(deferredDiscardForceClear)
	} else {
		model.deferredActions.discard()
	}
	model.items = nil
	model.toolItems = map[string]int{}
	model.transparentToolParents = map[string]struct{}{}
	model.currentAssistant = -1
	model.transcriptStart = -1
	model.goal = nil
	model.rubric = dagoapi.RubricSnapshot{}
	model.nextRubric = ""
	model.oneShotRubric = false
	model.oneShotPreviousRubric = ""
	model.autoClassifierPendingResults = nil
	model.autoClassifierTurnID = ""
	model.autoClassifierReset = true
	model.resetUsage()
	model.composer.Reset()
	model.pasteBindings = map[string]string{}
	model.inputMedia = map[string]damessage.ContentBlock{}
	model.imageSequence = 0
	model.videoSequence = 0
	model.inputCompletion = completionState{}
	model.setInputMode(inputNormal)
	model.discardDraftUndo()
	model.subagentPanel = newSubagentPanelState(time.Now, subagentPanelOptions{})
	model.status = "New thread"
	if plan.PreviousThread != "" {
		model.appendItem(transcriptItem{kind: itemNotice, text: "Previous thread remains resumable with /threads -r " + plan.PreviousThread + "."})
	}
	model.relayout()
	model.refreshTranscript()
	if approvalModeErr != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not initialize thread approval mode; using Manual: " + boundedLifecycleError(approvalModeErr)})
		model.refreshTranscript()
	}
	if force && oldThreadID != "" {
		if model.forceClearPending == nil {
			model.forceClearPending = make(map[uint64]*forceClearPending)
		}
		if oldStream != nil {
			model.forceClearPending[oldGeneration] = &forceClearPending{generation: oldGeneration, threadID: oldThreadID, stream: oldStream, reading: true}
			// The invocation's existing Next command owns the stream read. Its
			// stale generation result advances this drain after turnCancel is
			// observed, avoiding concurrent readers on one event stream.
			return model.notify("Previous work is stopping in the background.", toastInfo, "")
		}
		if hadPendingInterrupt {
			model.forceClearPending[oldGeneration] = &forceClearPending{generation: oldGeneration, threadID: oldThreadID, finalizing: true}
			return finalizeForceClear(model.runner, oldThreadID, oldGeneration)
		}
	}
	return nil
}

func waitForForceClearStream(ctx context.Context, stream eventStream, generation uint64, timeout time.Duration) tea.Cmd {
	return func() tea.Msg {
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		waitContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if stream == nil {
			return streamDoneMsg{generation: generation, err: errors.New("agent stream is unavailable")}
		}
		event, err := stream.Next(waitContext)
		if err == nil {
			return streamEventMsg{event: event, generation: generation}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return forceClearDrainTimeoutMsg{generation: generation}
		}
		defer stream.Close()
		if !errors.Is(err, io.EOF) {
			return streamDoneMsg{generation: generation, err: err}
		}
		result, resultErr := stream.Result(waitContext)
		return streamDoneMsg{result: result, err: resultErr, generation: generation}
	}
}

func finalizeForceClear(runner agentRunner, threadID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return forceClearFinalizedMsg{generation: generation, err: runner.Cancel(ctx, threadID)}
	}
}

func (model *tuiModel) continueForceClearDrain(generation uint64) tea.Cmd {
	pending := model.forceClearPending[generation]
	if pending == nil || pending.stream == nil || pending.reading {
		return nil
	}
	pending.reading = true
	return waitForForceClearStream(model.ctx, pending.stream, generation, model.forceClearTimeout)
}

func (model *tuiModel) finishForceClearStream(generation uint64) tea.Cmd {
	pending := model.forceClearPending[generation]
	if pending == nil {
		return nil
	}
	pending.stream = nil
	pending.reading = false
	if pending.finalizing {
		return nil
	}
	pending.finalizing = true
	return finalizeForceClear(model.runner, pending.threadID, generation)
}

func (model *tuiModel) retryForceClear(generation uint64) tea.Cmd {
	pending := model.forceClearPending[generation]
	if pending == nil {
		return nil
	}
	if pending.stream != nil {
		if pending.reading {
			return nil
		}
		pending.reading = true
		return waitForForceClearStream(model.ctx, pending.stream, generation, model.forceClearTimeout)
	}
	if pending.finalizing {
		return nil
	}
	pending.finalizing = true
	return finalizeForceClear(model.runner, pending.threadID, generation)
}

func (model *tuiModel) scheduleForceClearRetry(generation uint64) tea.Cmd {
	pending := model.forceClearPending[generation]
	if pending == nil || pending.scheduled || pending.retries >= maximumForceClearRetries {
		return nil
	}
	delay := initialForceClearRetryDelay << pending.retries
	if delay > maximumForceClearRetryDelay {
		delay = maximumForceClearRetryDelay
	}
	pending.retries++
	pending.scheduled = true
	return tea.Tick(delay, func(time.Time) tea.Msg { return forceClearRetryMsg{generation: generation} })
}
