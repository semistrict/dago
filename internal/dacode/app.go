package dacode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	dagoapi "github.com/semistrict/dago"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dagoal"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/daworkflow"
	"github.com/semistrict/dago/internal/unicodesecurity"
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

const (
	docsURL      = "https://github.com/semistrict/dago#readme"
	changelogURL = "https://github.com/semistrict/dago/releases"
	feedbackURL  = "https://github.com/semistrict/dago/issues/new/choose"
	// Retain terminal controls across several renderer frames before clearing.
	// Bubble Tea may coalesce an immediate follow-up model update before the
	// renderer writes the preceding View result.
	terminalSequenceDisplayDuration = 100 * time.Millisecond
	debugConsoleRefreshInterval     = 500 * time.Millisecond
	approvalTypingIdleDuration      = 2 * time.Second
	approvalDeferralTimeout         = 30 * time.Second
	maxApprovalReasonCharacters     = 4_000
	maxApprovalReasonBytes          = 16_000
)

type itemKind int

const (
	itemUser itemKind = iota
	itemAssistant
	itemTool
	itemSkill
	itemNotice
	itemError
)

type transcriptItem struct {
	kind      itemKind
	text      string
	callID    string
	name      string
	args      string
	timestamp time.Time
	restored  bool
	done      bool
	streaming bool
	failed    bool
	expanded  bool
	cancelled bool
	lifecycle toolLifecycle
	startedAt time.Time
	source    string
	detail    string
	request   string
	lineNums  bool
}

type transcriptBlockKind uint8

const (
	transcriptBlockWelcome transcriptBlockKind = iota
	transcriptBlockVirtualized
	transcriptBlockToolGroup
	transcriptBlockItem
	transcriptBlockRunning
	transcriptBlockApproval
)

type transcriptBlockID struct {
	kind  transcriptBlockKind
	index int
	key   string
}

type transcriptBlockLayout struct {
	id    transcriptBlockID
	start int
	lines int
}

type transcriptScrollAnchor struct {
	id   transcriptBlockID
	line int
}

type approvalState struct {
	requests        []dagent.ApprovalRequest
	ready           bool
	deferred        bool
	typingProtected bool
	deferredAt      time.Time
	deferGeneration uint64
	preparingReview bool
	reviewing       bool
	freezeReview    bool
	reasonMode      bool
	reason          textarea.Model
	reasonWarning   string
	selected        int
	autoFallback    bool
	commandExpanded bool
	previews        map[string][]string
}

type approvalMode int

const (
	approvalManual approvalMode = iota
	approvalAuto
	approvalYOLO
)

func initialApprovalMode(yolo, autoReview bool) approvalMode {
	if yolo {
		return approvalYOLO
	}
	if autoReview {
		return approvalAuto
	}
	return approvalManual
}

func parseApprovalMode(value any) (approvalMode, bool) {
	text, ok := value.(string)
	if !ok {
		return approvalManual, false
	}
	switch text {
	case "manual":
		return approvalManual, true
	case "auto":
		return approvalAuto, true
	case "yolo":
		return approvalYOLO, true
	default:
		return approvalManual, false
	}
}

func (mode approvalMode) valid() bool {
	return mode >= approvalManual && mode <= approvalYOLO
}

func (mode approvalMode) String() string {
	switch mode {
	case approvalAuto:
		return "auto"
	case approvalYOLO:
		return "yolo"
	default:
		return "manual"
	}
}

func (mode approvalMode) next() approvalMode {
	return (mode + 1) % 3
}

type streamEventMsg struct {
	event      dagent.Event
	generation uint64
}

type streamDoneMsg struct {
	result     dagent.Result
	err        error
	generation uint64
}

type displaySettingsSavedMsg struct {
	generation uint64
	err        error
}

type autoModeNoticeSavedMsg struct {
	err error
}

type yoloModeNoticeSavedMsg struct {
	err error
}

type themePreferenceSavedMsg struct {
	name     string
	terminal string
	original string
	err      error
}

type approvalModeSavedMsg struct {
	threadID       string
	mode           approvalMode
	startAfter     bool
	approvePending bool
	generation     uint64
	err            error
}

type externalURLOpenedMsg struct {
	url string
	err error
}

type cancelDoneMsg struct {
	err        error
	generation uint64
}

type reviewDoneMsg struct {
	result     approvalReviewResult
	err        error
	generation uint64
}

type initialPromptMsg string
type initialGoalMsg string
type terminalSequencesFlushedMsg struct{ generation uint64 }
type approvalDeferredTickMsg struct{ generation uint64 }
type debugConsoleTickMsg struct{ generation uint64 }

type onboardingSavedMsg struct {
	result onboardingResult
	err    error
}

type restartFinishedMsg struct{ err error }

type hookStatusMsg struct {
	update hookStatusUpdate
	err    error
}

type goalLoadedMsg struct {
	goal       *dagoal.Goal
	rubric     dagoapi.RubricSnapshot
	err        error
	generation uint64
}

type goalActionMsg struct {
	action       string
	goal         *dagoal.Goal
	cleared      bool
	continueWork bool
	err          error
	generation   uint64
}

type goalCriteriaMsg struct {
	proposal   dagoal.CriteriaProposal
	amendment  bool
	err        error
	generation uint64
}

type rubricActionMsg struct {
	action     string
	rubric     dagoapi.RubricSnapshot
	cleared    bool
	err        error
	generation uint64
}

type tuiModel struct {
	ctx                            context.Context
	runner                         agentRunner
	workingDir                     string
	modelName                      string
	threadID                       string
	threadHasCheckpoint            bool
	approvalMode                   approvalMode
	approvalModeStore              *approvalModeStore
	approvalModeBlocked            bool
	approvalModeGeneration         uint64
	approvalModePending            approvalMode
	approvalModePendingSet         bool
	approvalAutoApproveAfterNotice bool
	initial                        string
	initialGoal                    string
	startupTranscript              string
	externalEditorName             string
	editDraft                      func(string) (tea.Cmd, error)
	createThreadID                 func() (string, error)

	width  int
	height int
	ready  bool

	viewport                viewport.Model
	composer                textarea.Model
	spinner                 spinner.Model
	inputMode               inputMode
	inputHistory            *inputHistory
	inputFiles              []string
	inputCompletion         completionState
	pasteBindings           map[string]string
	inputMedia              map[string]damessage.ContentBlock
	pasteSequence           int
	imageSequence           int
	videoSequence           int
	inputQueue              []queuedInput
	shellRunning            bool
	shellCancel             context.CancelFunc
	shellContext            []string
	terminalBlurred         bool
	refocusedAt             time.Time
	lastTypedAt             time.Time
	lastControlCAt          time.Time
	glyphs                  uiGlyphs
	charset                 charsetMode
	kittyKeyboard           bool
	newlineLabel            string
	cursor                  cursorPreference
	confirmations           *confirmationArms
	draftUndo               draftUndoBuffer
	draftAttachmentUndo     composerAttachmentUndo
	toasts                  *toastQueue
	toastHeight             int
	notifications           *notificationRegistry
	notificationCenter      *notificationCenterState
	notificationSettings    *notificationSettingsState
	notificationStore       *notificationPreferenceStore
	notificationAuthTarget  string
	traceCommand            *traceCommand
	traceProject            string
	traceGeneration         uint64
	deferredTrace           []traceCommandResult
	updateProfile           *tuiUpdateProfile
	updateModal             *updateModalState
	updateCancel            context.CancelFunc
	updateGeneration        uint64
	updateStateGeneration   uint64
	updateStateStore        *updateStateStore
	updateStateWrites       *updateStateWriteController
	updateState             updatePersistentState
	autoUpdate              *autoUpdateController
	deferredActions         deferredActionQueue
	deferredDrain           *deferredDrainProgress
	applyingDeferred        bool
	operationGeneration     uint64
	forceClearPending       map[uint64]*forceClearPending
	forceClearTimeout       time.Duration
	startupFailed           bool
	startupReady            bool
	startupTip              startupTipState
	statusBranch            string
	statusSpinnerRunning    bool
	welcomeHitTargets       []welcomeHitTarget
	welcomeScreenHitTargets []welcomeHitTarget
	welcomeMCPServers       []mcpViewerServer
	welcomeMCPPending       bool
	welcomeProjectLabel     string
	welcomeProjectURL       string
	chatScroll              chatScrollState
	transcriptLayout        []transcriptBlockLayout

	items                              []transcriptItem
	toolItems                          map[string]int
	currentAssistant                   int
	stream                             eventStream
	turnCancel                         context.CancelFunc
	running                            bool
	cancelling                         bool
	approval                           *approvalState
	askUser                            *askUserState
	editorAskUserCallID                string
	editorAskUserQuestion              int
	editorGoalReview                   bool
	agentPicker                        *agentPickerState
	skillTrust                         *skillTrustState
	effortPicker                       *effortPickerState
	themePicker                        *themePickerState
	pluginManager                      *pluginManagerState
	pluginReloadPrompt                 bool
	pluginReloading                    bool
	modelSelector                      *modelSelectorState
	deferredModelSelector              *modelSelectorState
	modelProviderAvailability          map[string]modelProviderAvailability
	modelPreferences                   modelPreferenceController
	modelPreferenceSequence            *modelPreferenceSequencer
	modelDefaultSpec                   string
	modelRecentSpecs                   []string
	modelRecentGeneration              uint64
	installSelector                    *installSelectorState
	installController                  installController
	installPending                     *installRequest
	onboarding                         *onboardingState
	onboardingStateDirectory           string
	restartPrompt                      *restartPromptState
	restartController                  restartController
	restarting                         bool
	resumeController                   *sessionResumeController
	resumeOptions                      sessionResumeOptions
	compactionCheckpointID             string
	mcpViewer                          *mcpViewerState
	mcpController                      mcpRuntimeController
	mcpReconnectPrompt                 *mcpReconnectPromptState
	mcpErrorServer                     string
	mcpErrorDetail                     string
	mcpLogin                           *mcpLoginState
	mcpLoginGeneration                 uint64
	authManager                        *authTUIController
	autoClassifier                     *autoClassifierState
	autoClassifierPreferences          autoClassifierPreferenceController
	autoClassifierSelector             bool
	autoClassifierValidationGeneration uint64
	autoClassifierTurnID               string
	autoClassifierReset                bool
	autoClassifierPendingResults       map[string]struct{}
	subagentPanel                      *subagentPanelState
	debugConsoleBuffer                 *debugConsoleBuffer
	debugConsole                       *debugConsoleOverlay
	debugConsoleClearedUpto            uint64
	debugConsoleClickToCopy            bool
	debugConsoleGeneration             uint64
	sessionPicker                      *sessionPickerState
	agentName                          string
	status                             string
	hookStatus                         string
	totalTokens                        int
	contextWindow                      int
	lastUsage                          damessage.Usage
	threadUsage                        damessage.Usage
	usageRequests                      int
	costStats                          sessionCostState
	contextScreen                      bool
	autoModeNotice                     bool
	autoModeNoticeConfigured           bool
	autoModeNoticeAcknowledged         bool
	approvalNoticeDeferred             bool
	autoModeNoticePath                 string
	autoModeReviewer                   string
	yoloModeNotice                     bool
	yoloModeNoticeSaving               bool
	yoloModeAcknowledged               bool
	clipboardSequence                  string
	browserSequence                    string
	themeSequence                      string
	terminalSequenceGeneration         uint64
	browserLinks                       bool
	openURL                            func(string) error
	showTimestamps                     bool
	showScrollbar                      bool
	showLineNumbers                    bool
	transcriptStart                    int
	toolGroupExpanded                  map[string]bool
	themeRegistry                      themeRegistry
	themeName                          string
	terminalTheme                      string
	themeStore                         *themePreferenceStore
	displaySettings                    string
	displaySaving                      bool
	displayDirty                       bool
	displayGeneration                  uint64
	displayActiveGeneration            uint64
	displaySaveMu                      *sync.Mutex
	threadSelectorPreferences          threadSelectorPreferences
	goal                               *dagoal.Goal
	goalReview                         *goalReviewState
	rubric                             dagoapi.RubricSnapshot
	nextRubric                         string
	oneShotRubric                      bool
	oneShotPreviousRubric              string
	workflowPanel                      *workflowPanelState
	pendingWorkflows                   []daworkflow.Status
}

func newTUIModel(ctx context.Context, runner agentRunner, workingDir, modelName, threadID string, yolo, autoReview bool, initial string) *tuiModel {
	themes := builtinThemeRegistry()
	applyThemePalette(themes[defaultThemeName].Palette)
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

	profile := runner.Profile()
	debugBuffer := newDebugConsoleBuffer(0)
	debugBuffer.append(debugConsoleRecord{
		Time: time.Now(), Level: "INFO", LevelNumber: debugConsoleLevelNumbers["INFO"],
		Logger: "dacode.tui", Message: "Debug console ready",
	})
	return &tuiModel{
		ctx: ctx, runner: runner, workingDir: workingDir, modelName: modelName,
		threadID: threadID, approvalMode: initialApprovalMode(yolo, autoReview), initial: initial, composer: composer,
		spinner: spin, currentAssistant: -1, toolItems: map[string]int{}, status: "Ready", contextWindow: profile.ContextWindow, startupReady: true,
		pasteBindings: map[string]string{}, inputMedia: map[string]damessage.ContentBlock{},
		showLineNumbers: true, toolGroupExpanded: map[string]bool{}, transcriptStart: -1,
		agentName: runner.AgentName(), externalEditorName: editorDisplayName(),
		themeRegistry: themes, themeName: defaultThemeName, themeStore: newThemePreferenceStore(""),
		editDraft: func(draft string) (tea.Cmd, error) {
			return prepareEditorTeaCommand(ctx, draft)
		},
		createThreadID: newThreadID,
		browserLinks:   os.Getenv("DACODE_XTERMJS") == "1", openURL: openExternalURL,
		glyphs: cloneUIGlyphs(unicodeUIGlyphs), charset: charsetUnicode, newlineLabel: newlineShortcut(false),
		cursor: cursorPreference{Style: cursorBlock, Blink: true}, confirmations: newConfirmationArms(),
		operationGeneration:     1,
		modelPreferenceSequence: newModelPreferenceSequencer(),
		toasts:                  newToastQueue(0), notifications: newNotificationRegistry(), subagentPanel: newSubagentPanelState(time.Now, subagentPanelOptions{}),
		debugConsoleBuffer:        debugBuffer,
		resumeOptions:             defaultSessionResumeOptions(workingDir),
		startupTip:                newStartupTipState(startupTipFresh, editorDisplayName(), true, uint64(time.Now().UnixNano()), startupTipsVisible(os.LookupEnv)),
		chatScroll:                newChatScrollState(),
		displaySaveMu:             &sync.Mutex{},
		threadSelectorPreferences: defaultThreadSelectorOptions().Preferences,
	}
}

func (model *tuiModel) Init() tea.Cmd {
	model.startupReady = false
	model.prepareStartupTip()
	commands := []tea.Cmd{
		textarea.Blink,
		model.startStatusSpinner(),
		markStartupReady(),
		resolveStatusBranch(model.ctx, model.workingDir),
		model.refreshWelcomeMCP(),
		waitForWorkflowCompletion(model.ctx, model.runner),
	}
	if model.toasts != nil {
		if expires, ok := model.toasts.nextExpiry(); ok {
			commands = append(commands, tea.Tick(max(time.Until(expires), time.Millisecond), func(at time.Time) tea.Msg { return toastExpiryMsg(at) }))
		}
	}
	if command := model.startStartupUpdate(); command != nil {
		commands = append(commands, command)
	}
	if command := model.requestCostReport(false); command != nil {
		commands = append(commands, command)
	}
	if source, ok := model.runner.(hookStatusSource); ok {
		commands = append(commands, waitForHookStatus(model.ctx, source))
	}
	if model.sessionPicker == nil {
		model.showStartupTranscript()
	}
	if model.autoModeNotice || model.yoloModeNotice {
		model.approvalNoticeDeferred = true
	} else if command := model.initialSessionCommand(); command != nil {
		commands = append(commands, command)
	}
	return tea.Batch(commands...)
}

func (model *tuiModel) showStartupTranscript() {
	if model.startupTranscript == "" {
		return
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: model.startupTranscript})
	model.startupTranscript = ""
}

func (model *tuiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var typingCommand tea.Cmd
	switch typed := message.(type) {
	case tea.WindowSizeMsg:
		model.resize(typed.Width, typed.Height)
		return model, nil
	case startupReadyMsg:
		model.startupReady = true
		model.refreshTranscript()
		return model, nil
	case statusBranchMsg:
		if typed.workingDirectory == model.workingDir {
			model.statusBranch = typed.branch
		}
		return model, nil
	case welcomeMCPSnapshotMsg:
		model.applyWelcomeMCPSnapshot(typed)
		model.refreshTranscript()
		return model, nil
	case initialPromptMsg:
		if !model.running {
			model.dismissStartupTip()
			return model, model.submitPrompt(string(typed))
		}
	case initialGoalMsg:
		if !model.running {
			model.dismissStartupTip()
			return model, model.goalCommand(string(typed))
		}
	case queuedInputDispatchMsg:
		input := typed.Input
		return model, model.dispatchInput(input.mode, input.value, input.display, input.attachments)
	case deferredActionCompletedMsg:
		return model, model.applyDeferredCompletion(typed)
	case deferredDrainCompletedMsg:
		return model, model.finishDeferredDrain(typed)
	case deferredModelSelectionMsg:
		return model, model.applyDeferredModelSelectorAction(typed.Action)
	case deferredThreadResumeMsg:
		return model, model.applyDeferredThreadResume(typed)
	case deferredAgentSwitchMsg:
		return model, model.applyDeferredAgentSwitch(typed)
	case deferredMCPReconnectMsg:
		return model, model.reconnectMCP()
	case hookStatusMsg:
		if typed.err != nil {
			if !errors.Is(typed.err, context.Canceled) {
				model.hookStatus = ""
			}
			return model, nil
		}
		model.hookStatus = typed.update.Status
		if source, ok := model.runner.(hookStatusSource); ok {
			return model, tea.Batch(waitForHookStatus(model.ctx, source), model.startStatusSpinner())
		}
		return model, nil
	case pluginManagerSnapshotMsg:
		if model.pluginManager != nil {
			model.pluginManager.applySnapshot(typed)
		}
		return model, nil
	case pluginManagerMutationMsg:
		if model.pluginManager == nil {
			return model, nil
		}
		model.pluginManager.applyMutation(typed)
		if typed.err != nil {
			return model, nil
		}
		controller, ok := model.runner.(pluginRuntimeController)
		if !ok {
			model.pluginManager.error = "Plugin runtime is unavailable."
			model.pluginManager.loading = false
			return model, nil
		}
		return model, loadPluginManager(model.ctx, controller)
	case pluginReloadMsg:
		model.pluginReloading = false
		model.pluginReloadPrompt = false
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: pluginManagerDisplayError(typed.err)})
		} else {
			model.appendItem(transcriptItem{kind: itemNotice, text: pluginReloadSummary(typed.result)})
			if model.pluginManager != nil {
				model.pluginManager.dirty = false
				model.pluginManager.status = "Reload applied."
			}
		}
		model.refreshTranscript()
		return model, model.drainInputQueue()
	case goalLoadedMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not restore goal: " + typed.err.Error()})
			model.refreshTranscript()
			return model, nil
		}
		model.goal = typed.goal
		model.rubric = typed.rubric
		if model.goal != nil && model.goal.Actionable() && !model.running {
			return model, model.startGoalContinuation()
		}
	case goalActionMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
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
	case goalCriteriaMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		model.running = false
		if typed.err != nil {
			model.status = "Goal error"
			model.appendItem(transcriptItem{kind: itemError, text: "Could not draft goal criteria: " + typed.err.Error()})
			model.refreshTranscript()
			return model, model.drainInputQueue()
		}
		model.goalReview = newGoalReview(typed.proposal.Objective, typed.proposal.Criteria, typed.amendment)
		model.status = "Review goal"
		model.relayout()
		model.refreshTranscript()
		return model, nil
	case rubricActionMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		return model.finishRubricAction(typed)
	case spinner.TickMsg:
		model.statusSpinnerRunning = false
		var command tea.Cmd
		model.spinner, command = model.spinner.Update(typed)
		if model.subagentPanel != nil {
			model.subagentPanel.tick()
		}
		model.refreshSpinner()
		if !statusSpinnerActive(model.projectStatusBarState()) {
			return model, nil
		}
		model.statusSpinnerRunning = true
		return model, command
	case debugConsoleTickMsg:
		if model.debugConsole == nil || typed.generation != model.debugConsoleGeneration {
			return model, nil
		}
		model.debugConsole.updateSnapshot(model.debugConsoleSnapshotFields())
		model.debugConsole.poll()
		return model, scheduleDebugConsoleTick(typed.generation)
	case streamEventMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			if pending := model.forceClearPending[typed.generation]; pending != nil {
				pending.reading = false
				return model, model.continueForceClearDrain(typed.generation)
			}
			return model, nil
		}
		model.applyEvent(typed.event)
		model.refreshTranscript()
		return model, tea.Batch(waitForStreamGeneration(model.ctx, model.stream, model.operationGeneration), model.scheduleDeferredApproval())
	case streamDoneMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			if pending := model.forceClearPending[typed.generation]; pending != nil {
				pending.reading = false
				return model, model.finishForceClearStream(typed.generation)
			}
			return model, nil
		}
		updated, command := model.finishStream(typed)
		return updated, tea.Batch(command, model.requestCostReport(false))
	case forceClearFinalizedMsg:
		pending := model.forceClearPending[typed.generation]
		if pending == nil {
			return model, nil
		}
		pending.finalizing = false
		if typed.err == nil {
			delete(model.forceClearPending, typed.generation)
			return model, nil
		}
		command := model.scheduleForceClearRetry(typed.generation)
		if pending.warned {
			return model, command
		}
		pending.warned = true
		return model, tea.Batch(model.notify("The previous thread could not be finalized after cancellation; cleanup will retry.", toastError, ""), command)
	case forceClearDrainTimeoutMsg:
		pending := model.forceClearPending[typed.generation]
		if pending == nil {
			return model, nil
		}
		pending.reading = false
		command := model.scheduleForceClearRetry(typed.generation)
		if pending.warned {
			return model, command
		}
		pending.warned = true
		return model, tea.Batch(model.notify("The previous thread did not stop in time; cleanup remains quarantined and will retry.", toastWarning, ""), command)
	case forceClearRetryMsg:
		if pending := model.forceClearPending[typed.generation]; pending != nil {
			pending.scheduled = false
		}
		return model, model.retryForceClear(typed.generation)
	case sessionCostMsg:
		model.applyCostReport(typed)
		return model, nil
	case toastExpiryMsg:
		if model.toasts != nil {
			for _, expired := range model.toasts.expire(time.Time(typed)) {
				if expired.ActionKey != "" && model.notifications != nil {
					model.notifications.unbindToast(notificationToastIdentity(expired.ID))
				}
			}
			model.relayout()
			model.refreshTranscript()
			if expires, ok := model.toasts.nextExpiry(); ok {
				return model, tea.Tick(max(time.Until(expires), time.Millisecond), func(at time.Time) tea.Msg { return toastExpiryMsg(at) })
			}
		}
		return model, nil
	case notificationPreferenceSavedMsg:
		if model.notificationStore != nil && !model.notificationStore.currentWarningWrite(typed.write) {
			return model, nil
		}
		if typed.err != nil {
			if model.notificationSettings != nil {
				model.notificationSettings.rollback(typed.write.Key, !typed.write.Enabled)
			}
			return model, model.notify("Notification preference could not be saved.", toastError, "")
		}
		if typed.notificationKey != "" {
			model.notifications.remove(typed.notificationKey)
			if model.notificationCenter != nil && !model.notificationCenter.reload(model.notifications.list()) {
				model.notificationCenter = nil
			}
			return model, model.notify("Warning notification hidden.", toastInfo, "")
		}
		return model, model.notify("Notification preference saved.", toastInfo, "")
	case traceResolvedMsg:
		if typed.result.URL != "" {
			model.configureWelcomeProject(model.traceProject, typed.result.URL)
		}
		return model, model.handleTraceResolved(typed)
	case traceBrowserOpenedMsg:
		if typed.failed {
			return model, model.notify("The trace link could not be opened automatically; copy it from the transcript.", toastWarning, "")
		}
		return model, nil
	case notificationURLOpenedMsg:
		if typed.failed {
			return model, model.notify("The website could not be opened automatically.", toastWarning, "")
		}
		return model, nil
	case updateTUIResultMsg:
		return model, model.handleUpdateResult(typed)
	case updateStateSavedMsg:
		return model, model.handleUpdateStateSaved(typed)
	case autoUpdateSavedMsg:
		return model, model.handleAutoUpdateSaved(typed)
	case modelPreferenceMsg:
		matches := model.deferredModelPreferenceMatches(typed)
		command := model.finishModelPreference(typed)
		return model, model.finishDeferredAsync(deferredModelSwitch, matches, command)
	case authRefreshMsg:
		return model, model.handleAuthRefresh(typed)
	case authMutationMsg:
		return model, model.handleAuthMutation(typed)
	case authSubscriptionEventMsg:
		return model, model.handleAuthSubscriptionEvent(typed)
	case mcpLoginEventMsg:
		return model, model.handleMCPLoginEvent(typed)
	case mcpRuntimeMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		matches := model.deferredMCPResultMatches(typed)
		model.applyWelcomeMCPSnapshot(welcomeMCPSnapshotMsg{servers: typed.servers, pending: typed.pending, err: typed.err})
		command := model.handleMCPRuntime(typed)
		return model, model.finishDeferredAsync(deferredMCPReconnect, matches, command)
	case autoClassifierValidatedMsg:
		return model, model.finishAutoClassifierValidation(typed)
	case autoClassifierPreferenceMsg:
		if typed.err != nil {
			return model, model.notify("Classifier preference could not be saved.", toastError, "")
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: typed.notice})
		model.refreshTranscript()
		return model, nil
	case installCompletedMsg:
		if model.installPending == nil || *model.installPending != typed.request {
			return model, nil
		}
		model.installPending = nil
		if typed.err != nil {
			return model, model.notify("Integration installation failed.", toastError, "")
		}
		installedName := boundedTerminalText(typed.result.Name, 64)
		if installedName == "" {
			installedName = typed.request.Name
		}
		if typed.action == installCompletionRestartRequired {
			return model, model.notify("Installed "+installedName+". Restart required to load it.", toastInfo, "")
		}
		return model, model.notify(installedName+" is already available.", toastInfo, "")
	case onboardingSavedMsg:
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not save onboarding state; setup may run again next launch: " + boundedLifecycleError(typed.err)})
		} else if typed.result.Name != "" {
			model.appendItem(transcriptItem{kind: itemNotice, text: "Welcome, " + unicodesecurity.RenderTerminalSafe(typed.result.Name) + "."})
		}
		if typed.result.ConfigureWebSearch {
			model.appendItem(transcriptItem{kind: itemNotice, text: "Web search can be configured with /auth."})
		}
		model.refreshTranscript()
		return model, nil
	case restartFinishedMsg:
		model.restarting = false
		if typed.err != nil {
			model.status = "Restart failed"
			model.appendItem(transcriptItem{kind: itemError, text: "Local agent server restart failed: " + boundedLifecycleError(typed.err)})
		} else {
			model.status = "Ready"
			model.appendItem(transcriptItem{kind: itemNotice, text: "Local agent server restarted."})
		}
		model.refreshTranscript()
		return model, model.drainInputQueue()
	case compactionFinishedMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		deferredCompaction := model.deferredCompactionWaiting()
		model.running = false
		model.applyCursorPreference(!model.terminalBlurred)
		output := boundedResumePromptText(typed.result.Output, 4_000)
		if typed.err != nil {
			output = "Compaction failed: " + boundedLifecycleError(typed.err)
		}
		if output == "" {
			output = "Nothing to compact yet — the conversation is already compact."
		}
		model.appendItem(transcriptItem{
			kind: itemTool, name: "compact_conversation", text: output, done: true,
			failed: typed.err != nil || typed.result.Failed,
			lifecycle: func() toolLifecycle {
				if typed.err != nil || typed.result.Failed {
					return toolError
				}
				return toolSuccess
			}(),
		})
		model.status = "Ready"
		model.refreshTranscript()
		command := model.drainInputQueue()
		return model, model.finishDeferredAsync(deferredThreadSwitch, deferredCompaction, command)
	case displaySettingsSavedMsg:
		if typed.generation == 0 || typed.generation != model.displayActiveGeneration {
			return model, nil
		}
		model.displaySaving = false
		model.displayActiveGeneration = 0
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Display preference changed for this session but could not be saved: " + typed.err.Error()})
			model.refreshTranscript()
		}
		return model, model.startDisplaySettingsSave()
	case autoModeNoticeSavedMsg:
		if typed.err != nil {
			model.autoModeNoticeAcknowledged = false
			model.appendItem(transcriptItem{kind: itemError, text: "Auto mode remains enabled, but its notice could not be saved: " + typed.err.Error()})
			model.refreshTranscript()
		}
		if model.approvalAutoApproveAfterNotice && model.approval != nil {
			model.approvalAutoApproveAfterNotice = false
			return model, model.resolveApproval(true)
		}
		return model, nil
	case yoloModeNoticeSavedMsg:
		model.yoloModeNotice = false
		model.yoloModeNoticeSaving = false
		if typed.err != nil {
			model.yoloModeAcknowledged = false
			model.appendItem(transcriptItem{kind: itemError, text: "YOLO acknowledgement could not be saved; staying in the current approval mode: " + typed.err.Error()})
			model.refreshTranscript()
			return model, model.startAfterApprovalNotice()
		}
		model.yoloModeAcknowledged = true
		return model, model.persistApprovalMode(approvalYOLO, true)
	case themePreferenceSavedMsg:
		if typed.err != nil {
			if typed.terminal != "" && model.themeName == typed.name {
				model.setTheme(typed.original)
				if model.themePicker != nil && model.themePicker.sessionTerminalDefault == typed.name {
					model.themePicker.sessionTerminalDefault = ""
				}
			}
			model.appendItem(transcriptItem{kind: itemError, text: "Theme changed for this session but its preference could not be saved."})
			model.refreshTranscript()
			return model, nil
		}
		if typed.terminal != "" {
			model.terminalTheme = typed.name
			if model.themePicker != nil {
				model.themePicker.terminalDefault = typed.name
				model.themePicker.sessionTerminalDefault = typed.name
			}
			model.appendItem(transcriptItem{kind: itemNotice, text: "Terminal theme default saved."})
			model.refreshTranscript()
			return model, nil
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: "Theme preference saved."})
		model.refreshTranscript()
		return model, nil
	case approvalModeSavedMsg:
		if typed.threadID != model.threadID || typed.generation != model.approvalModeGeneration {
			return model, nil
		}
		model.approvalModePendingSet = false
		if typed.err != nil {
			if typed.approvePending {
				model.approvalAutoApproveAfterNotice = false
			}
			if typed.mode == approvalManual {
				model.approvalMode = approvalManual
				model.approvalModeBlocked = true
				model.appendItem(transcriptItem{kind: itemError, text: "Manual approval mode could not be persisted; active work was cancelled and new runs are blocked: " + typed.err.Error()})
				if model.running {
					model.cancelling = true
				}
				if model.turnCancel != nil {
					model.turnCancel()
				}
				if model.approval != nil {
					model.approval.freezeReview = false
					if !model.running {
						model.approval = nil
					}
				}
			} else {
				model.appendItem(transcriptItem{kind: itemError, text: strings.ToUpper(typed.mode.String()) + " approval mode could not be persisted; remaining in " + model.approvalMode.String() + ": " + typed.err.Error()})
			}
			model.relayout()
			model.refreshTranscript()
			if typed.startAfter && typed.mode != approvalManual {
				return model, model.startAfterApprovalNotice()
			}
			return model, nil
		}
		model.approvalModeBlocked = false
		command := model.applyApprovalMode(typed.mode)
		if typed.approvePending && model.approval != nil && !model.autoModeNotice {
			model.approvalAutoApproveAfterNotice = false
			return model, model.resolveApproval(true)
		}
		if typed.startAfter {
			return model, tea.Batch(command, model.startAfterApprovalNotice())
		}
		return model, command
	case externalURLOpenedMsg:
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not open " + typed.url + ": " + typed.err.Error()})
			model.refreshTranscript()
		}
		return model, nil
	case editorFinishedMsg:
		if model.editorGoalReview && model.goalReview != nil {
			model.editorGoalReview = false
			if typed.err != nil {
				model.goalReview.warning = "External editor failed. Check $VISUAL/$EDITOR."
			} else if !typed.cancelled {
				model.goalReview.setEditedValue(typed.text)
			}
			model.relayout()
			model.refreshTranscript()
			return model, nil
		}
		if model.finishAskUserEditor(typed) {
			return model, nil
		}
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "External editor failed. Check $VISUAL/$EDITOR."})
			model.refreshTranscript()
			return model, nil
		}
		if !typed.cancelled {
			model.composer.SetValue(typed.text)
			model.relayout()
			model.refreshTranscript()
		}
		return model, nil
	case shellDoneMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		return model.finishShell(typed)
	case askUserCancelledMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		if typed.err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not cancel question: " + typed.err.Error()})
			model.status = "Question cancellation failed"
		} else {
			model.status = "Ready"
		}
		model.refreshTranscript()
		return model, model.drainInputQueue()
	case cancelDoneMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		model.cancelling = false
		model.running = false
		model.applyCursorPreference(!model.terminalBlurred)
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
		return model, model.drainInputQueue()
	case reviewDoneMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		return model.finishReview(typed)
	case sessionsLoadedMsg:
		return model, model.finishSessionList(typed)
	case sessionResumePreparedMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		command := model.finishSessionResumePreparation(typed)
		if typed.err != nil {
			command = model.finishDeferredAsync(deferredThreadSwitch, model.deferredGenerationMatches(deferredThreadSwitch, typed.generation), command)
		}
		return model, command
	case sessionLoadedMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		startupResume := model.sessionPicker != nil && model.sessionPicker.startup
		if startupResume && typed.err != nil {
			model.restoreFallbackStartupTip()
		}
		matches := model.deferredThreadResultMatches(typed)
		command := model.finishSessionLoad(typed)
		if typed.err == nil {
			model.suppressStartupTipAfterResume()
			model.statusBranch = typed.session.Branch
		}
		if matches && typed.decision.Compact && model.running && command != nil && model.deferredDrain != nil {
			model.deferredDrain.compacting = true
			return model, command
		}
		return model, model.finishDeferredAsync(deferredThreadSwitch, matches, command)
	case threadDeleteCompletedMsg:
		if model.sessionPicker != nil && model.sessionPicker.selector != nil && model.sessionPicker.selector.finishDelete(typed.Authorization, typed.Err) {
			model.sessionPicker.sessions = append([]sessionInfo(nil), model.sessionPicker.selector.sessions...)
		}
		return model, nil
	case agentsLoadedMsg:
		model.finishAgentList(typed)
		return model, nil
	case agentSwitchedMsg:
		if typed.generation != 0 && typed.generation != model.operationGeneration {
			return model, nil
		}
		matches := model.deferredAgentResultMatches(typed)
		model.finishAgentSwitch(typed)
		return model, model.finishDeferredAsync(deferredAgentSwitch, matches, nil)
	case defaultAgentSavedMsg:
		model.finishDefaultAgentSave(typed)
		return model, nil
	case reasoningEffortChangedMsg:
		model.finishReasoningEffortChange(typed)
		return model, nil
	case skillsLoadedMsg:
		model.finishSkillsList(typed)
		return model, nil
	case skillLoadedMsg:
		return model, model.finishSkillLoad(typed)
	case skillTrustedMsg:
		return model, model.finishSkillTrust(typed)
	case terminalSequencesFlushedMsg:
		if typed.generation == model.terminalSequenceGeneration {
			model.clipboardSequence = ""
			model.browserSequence = ""
		}
		return model, nil
	case approvalDeferredTickMsg:
		return model, model.finishDeferredApproval(typed)
	case tea.BlurMsg:
		model.terminalBlurred = true
		model.applyCursorPreference(false)
		return model, nil
	case tea.FocusMsg:
		if model.terminalBlurred {
			model.refocusedAt = time.Now()
		}
		model.terminalBlurred = false
		model.applyCursorPreference(!model.running && !model.shellRunning)
		return model, nil
	case tea.MouseMsg:
		if !model.refocusedAt.IsZero() && time.Since(model.refocusedAt) <= 300*time.Millisecond && typed.Button == tea.MouseButtonLeft && typed.Action == tea.MouseActionPress {
			model.refocusedAt = time.Time{}
			return model, nil
		}
		if model.debugConsole != nil {
			return model, model.handleDebugConsoleMouse(typed)
		}
		if command, handled := model.handleWelcomeMouse(typed); handled {
			return model, command
		}
		if model.handleChatWheel(typed) {
			return model, nil
		}
		var command tea.Cmd
		model.viewport, command = model.viewport.Update(typed)
		model.chatScroll.userScrolled(model.viewport.YOffset)
		return model, command
	case tea.KeyMsg:
		if model.approval != nil && !model.manualApprovalVisible() && model.approval.ready && !model.approval.preparingReview && !model.approval.reviewing {
			command, _ := model.handleKey(typed)
			return model, command
		}
		typingCommand = model.noteComposerTyping(typed)
		if command, handled := model.handleKey(typed); handled {
			return model, tea.Batch(command, typingCommand)
		}
	}

	if model.askUser != nil {
		if input := model.askUser.activeInput(); input != nil {
			var command tea.Cmd
			*input, command = input.Update(message)
			model.relayout()
			model.refreshTranscript()
			return model, command
		}
		return model, nil
	}
	if model.approval != nil && model.approval.reasonMode {
		var command tea.Cmd
		model.approval.reason, command = model.approval.reason.Update(message)
		model.sanitizeApprovalReasonInput()
		model.relayout()
		model.refreshTranscript()
		return model, command
	}
	if model.approval == nil || model.approval.typingProtected || model.approval.preparingReview || model.approval.reviewing {
		var command tea.Cmd
		model.composer, command = model.composer.Update(message)
		model.updateInputModeFromValue()
		model.updateInputCompletion()
		model.relayout()
		model.refreshTranscript()
		return model, tea.Batch(command, typingCommand)
	}
	var command tea.Cmd
	model.viewport, command = model.viewport.Update(message)
	model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height)
	model.chatScroll.userScrolled(model.viewport.YOffset)
	return model, command
}

func (model *tuiModel) handleKey(message tea.KeyMsg) (tea.Cmd, bool) {
	if model.debugConsole != nil {
		return model.handleDebugConsoleKey(message), true
	}
	if message.String() == "ctrl+\\" || message.Type == tea.KeyCtrlBackslash {
		return model.toggleDebugConsole(), true
	}
	if model.workflowPanel != nil {
		return model.handleWorkflowKey(message)
	}
	if model.updateModal != nil {
		if message.String() == "ctrl+n" {
			return model.notify("Close the current dialog before opening notifications.", toastInfo, ""), true
		}
		return model.handleUpdateModalKey(message.String()), true
	}
	if command, handled := model.handleNotificationShortcut(message); handled {
		return command, true
	}
	if (message.String() == "ctrl+c" || message.String() == "ctrl+d") && model.managementModalOpen() && !(model.authManager != nil && model.authManager.open) {
		model.cancelManagementModals()
		if message.String() == "ctrl+d" {
			return tea.Quit, true
		}
		return nil, true
	}
	if model.mcpLogin != nil {
		return model.handleMCPLoginKey(message), true
	}
	if model.mcpReconnectPrompt != nil {
		return model.handleMCPReconnectKey(message), true
	}
	if model.mcpErrorDetail != "" {
		return model.handleMCPErrorKey(message), true
	}
	if model.mcpViewer != nil {
		return model.handleMCPViewerKey(message), true
	}
	if model.authManager != nil && model.authManager.open {
		return model.handleAuthManagerKey(message), true
	}
	if model.onboarding != nil {
		model.onboarding.handleKey(message.String(), max(model.height-11, 3))
		if result, done := model.onboarding.value(); done {
			model.onboarding = nil
			commands := []tea.Cmd{model.persistOnboarding(result)}
			if result.Model != "" {
				commands = append(commands, model.selectRuntimeModel(result.Model))
			}
			model.relayout()
			model.refreshTranscript()
			return tea.Batch(commands...), true
		}
		return nil, true
	}
	if model.restartPrompt != nil || model.restarting {
		if model.restarting {
			return nil, true
		}
		switch model.restartPrompt.handleKey(message.String()) {
		case restartPromptRestart:
			model.restartPrompt = nil
			model.restarting = true
			model.status = "Restarting local agent server"
			return restartLocalAgentServer(model.ctx, model.restartController), true
		case restartPromptLater:
			model.restartPrompt = nil
			return model.drainInputQueue(), true
		default:
			return nil, true
		}
	}
	if model.resumeController != nil {
		return model.handleSessionResumeKey(message), true
	}
	if model.yoloModeNotice {
		if model.yoloModeNoticeSaving {
			return nil, true
		}
		switch message.String() {
		case "enter":
			model.yoloModeNoticeSaving = true
			path := model.autoModeNoticePath
			return func() tea.Msg { return yoloModeNoticeSavedMsg{err: saveYoloAcknowledgement(path)} }, true
		case "m":
			model.yoloModeNotice = false
			return model.persistApprovalMode(approvalManual, true), true
		case "esc":
			model.yoloModeNotice = false
			model.relayout()
			model.refreshTranscript()
			return model.persistApprovalMode(approvalManual, true), true
		default:
			return nil, true
		}
	}
	if model.pluginReloadPrompt || model.pluginReloading {
		if model.pluginReloading {
			return nil, true
		}
		switch message.String() {
		case "enter":
			if model.running || model.shellRunning || model.approval != nil || model.askUser != nil || model.skillTrust != nil {
				model.pluginReloadPrompt = false
				model.inputQueue = append(model.inputQueue, queuedInput{mode: inputCommand, value: "reload", display: "reload"})
				model.status = fmt.Sprintf("Queued (%d)", len(model.inputQueue))
				model.appendItem(transcriptItem{kind: itemNotice, text: fmt.Sprintf("Queued input #%d.", len(model.inputQueue))})
				model.refreshTranscript()
				return nil, true
			}
			controller, ok := model.runner.(pluginRuntimeController)
			if !ok {
				model.pluginReloadPrompt = false
				model.appendItem(transcriptItem{kind: itemError, text: "Plugin runtime is unavailable."})
				model.refreshTranscript()
				return nil, true
			}
			model.pluginReloading = true
			return reloadPluginRuntime(model.ctx, controller), true
		case "esc":
			model.pluginReloadPrompt = false
			return model.drainInputQueue(), true
		default:
			return nil, true
		}
	}
	if model.pluginManager != nil {
		controller, ok := model.runner.(pluginRuntimeController)
		if !ok {
			model.pluginManager = nil
			return nil, true
		}
		command, close := model.pluginManager.handleKey(model.ctx, controller, message)
		if close {
			dirty := model.pluginManager.dirty
			model.pluginManager = nil
			if dirty {
				model.pluginReloadPrompt = true
				return nil, true
			}
			return model.drainInputQueue(), true
		}
		return command, true
	}
	if model.notificationCenter != nil {
		request, close := model.notificationCenter.handleKey(message)
		if close {
			model.notificationCenter = nil
		}
		if request != nil {
			return model.handleNotificationAction(*request), true
		}
		return nil, true
	}
	if model.notificationSettings != nil {
		key, enabled, changed, close := model.notificationSettings.handleKey(message)
		if close {
			model.notificationSettings = nil
			return nil, true
		}
		if changed {
			return model.saveNotificationPreference(key, enabled), true
		}
		return nil, true
	}
	if model.modelSelector != nil {
		if model.autoClassifierSelector {
			return model.handleAutoClassifierSelectorKey(message.String()), true
		}
		return model.handleModelSelectorKey(message.String()), true
	}
	if model.installSelector != nil {
		return model.handleInstallSelectorKey(message.String()), true
	}
	if model.themePicker != nil {
		return model.handleThemeKey(message)
	}
	if model.effortPicker != nil {
		return model.handleEffortKey(message)
	}
	if model.agentPicker != nil {
		return model.handleAgentKey(message)
	}
	if model.skillTrust != nil {
		return model.handleSkillTrustKey(message)
	}
	if model.sessionPicker != nil {
		return model.handleSessionKey(message)
	}
	if model.contextScreen {
		if message.String() == "esc" {
			model.contextScreen = false
		}
		return nil, true
	}
	if model.goalReview != nil {
		if message.String() == "ctrl+c" {
			return model.resolveGoalReview(goalReviewDecision{kind: goalReviewCancelled}), true
		}
		if message.String() == "ctrl+x" && model.goalReview.mode != goalReviewMenu {
			return model.openGoalReviewEditor(), true
		}
		decision, command := model.goalReview.handleKey(message)
		if decision == nil {
			model.relayout()
			model.refreshTranscript()
			return command, true
		}
		return model.resolveGoalReview(*decision), true
	}
	if model.askUser != nil {
		if message.String() == "ctrl+c" {
			return model.cancelAskUser(), true
		}
		return model.handleAskUserKey(message)
	}
	if model.approval != nil && model.approval.reasonMode {
		return model.handleApprovalReasonKey(message)
	}
	if command, handled := model.handleApprovalMenuKey(message); handled {
		return command, true
	}
	if model.approval == nil || model.approval.typingProtected || model.approval.preparingReview || model.approval.reviewing {
		if command, handled := model.handleComposerKey(message); handled {
			return command, true
		}
	}
	switch message.String() {
	case "ctrl+g":
		if model.subagentPanel != nil && model.subagentPanel.handleKey("ctrl+g") {
			model.relayout()
			model.refreshTranscript()
			return nil, true
		}
	case "shift+tab", "ctrl+t":
		return model.setApprovalMode(model.approvalMode.next()), true
	case "ctrl+o":
		model.toggleLatestTranscriptUnit()
		model.refreshTranscript()
		return nil, true
	case "ctrl+c":
		now := time.Now()
		rapid := !model.lastControlCAt.IsZero() && now.Sub(model.lastControlCAt) >= 0 && now.Sub(model.lastControlCAt) < time.Second
		model.lastControlCAt = now
		if model.shellRunning && model.shellCancel != nil {
			model.shellCancel()
			model.status = "Cancelling shell command"
			return nil, true
		}
		if model.approval != nil {
			return model.resolveApproval(false), true
		}
		if model.running && !model.cancelling {
			model.cancelling = true
			model.status = "Cancelling"
			if model.turnCancel != nil {
				model.turnCancel()
			}
			return nil, true
		}
		if strings.TrimSpace(model.composer.Value()) != "" && !rapid {
			return model.stageTerminalSequences(osc52ClipboardSequence(model.expandedComposerValue()), ""), true
		}
		if model.confirmations.press(confirmQuit, now) {
			return tea.Quit, true
		}
		return model.notify("Press Ctrl+C again to quit.", toastWarning, ""), true
	case "ctrl+d":
		if model.forwardDeleteComposer() {
			return nil, true
		}
		return tea.Quit, true
	case "ctrl+z":
		if model.undoComposerClear() {
			return model.notify("Input restored.", toastInfo, ""), true
		}
		return nil, true
	case "ctrl+j":
		if model.approval == nil || model.approval.typingProtected || model.approval.preparingReview || model.approval.reviewing {
			model.composer.InsertString("\n")
			model.relayout()
			model.refreshTranscript()
			return nil, true
		}
	case "ctrl+x":
		if (model.approval == nil || model.approval.typingProtected || model.approval.preparingReview || model.approval.reviewing) && !model.running && !model.shellRunning {
			return model.openEditor(), true
		}
	case "tab":
		if model.manualApprovalVisible() && model.approval.ready && !model.approval.typingProtected && !model.approval.preparingReview && !model.approval.reviewing {
			model.startApprovalReason()
			return nil, true
		}
	case "enter":
		if model.approval == nil || model.approval.typingProtected || model.approval.preparingReview || model.approval.reviewing {
			return model.submitComposer(), true
		}
	case "y", "Y":
		if model.manualApprovalVisible() && model.approval.ready && !model.approval.typingProtected && !model.approval.preparingReview && !model.approval.reviewing {
			return model.resolveApproval(true), true
		}
	case "esc":
		if model.manualApprovalVisible() && model.approval.ready && !model.approval.typingProtected && !model.approval.preparingReview && !model.approval.reviewing {
			return model.resolveApproval(false), true
		}
		if model.inputMode != inputNormal {
			model.setInputMode(inputNormal)
			return nil, true
		}
		if model.shellRunning && model.shellCancel != nil {
			model.shellCancel()
			model.status = "Cancelling shell command"
			return nil, true
		}
		if len(model.inputQueue) > 0 || model.deferredActions.length() > 0 {
			model.inputQueue = model.inputQueue[:len(model.inputQueue)-1]
			model.status = "Ready"
			return model.notify("Newest queued message removed.", toastInfo, ""), true
		}
		if model.running && !model.cancelling {
			model.cancelling = true
			model.status = "Cancelling"
			if model.turnCancel != nil {
				model.turnCancel()
			}
			return nil, true
		}
		if strings.TrimSpace(model.composer.Value()) != "" {
			if !model.confirmations.press(confirmClearInput, time.Now()) {
				return model.notify("Press Esc again to clear input.", toastWarning, ""), true
			}
			if !model.clearComposerWithUndo() {
				return model.notify("Input is too large to clear safely.", toastWarning, ""), true
			}
			return model.notify("Input cleared.", toastInfo, ""), true
		}
	case "n", "N":
		if model.manualApprovalVisible() && model.approval.ready && !model.approval.typingProtected && !model.approval.preparingReview && !model.approval.reviewing {
			return model.resolveApproval(false), true
		}
	case "pgup":
		model.pageChat(-1)
		return nil, true
	case "pgdown":
		model.pageChat(1)
		return nil, true
	case "ctrl+_":
		model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height)
		model.chatScroll.userScrolled(model.viewport.YOffset)
		model.viewport.SetYOffset(model.chatScroll.wheel(-1))
		if model.chatScroll.shouldHydrateOlder(chatHydrationThreshold) {
			model.hydrateChatHistory()
		}
		return nil, true
	case "ctrl+^":
		model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height)
		model.chatScroll.userScrolled(model.viewport.YOffset)
		model.viewport.SetYOffset(model.chatScroll.wheel(1))
		return nil, true
	case "end":
		if strings.TrimSpace(model.composer.Value()) != "" {
			return nil, false
		}
		model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height)
		model.chatScroll.userScrolled(model.chatScroll.MaxOffset)
		model.viewport.SetYOffset(model.chatScroll.Offset)
		return nil, true
	case "ctrl+end":
		model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height)
		model.chatScroll.userScrolled(model.chatScroll.MaxOffset)
		model.viewport.SetYOffset(model.chatScroll.Offset)
		return nil, true
	}
	if model.approval != nil && !model.manualApprovalVisible() && model.approval.ready && !model.approval.preparingReview && !model.approval.reviewing {
		return nil, true
	}
	return nil, false
}

func (model *tuiModel) managementModalOpen() bool {
	return model.authManager != nil && model.authManager.open || model.mcpLogin != nil || model.mcpReconnectPrompt != nil ||
		model.mcpErrorDetail != "" || model.mcpViewer != nil || model.notificationCenter != nil || model.notificationSettings != nil
}

func (model *tuiModel) cancelManagementModals() {
	if model.authManager != nil {
		if model.authManager.flow != nil {
			model.authManager.flow.Cancel()
			model.authManager.flow = nil
		}
		model.authManager.manager.state.cancel()
		model.authManager.open = false
	}
	if model.mcpLogin != nil {
		if model.mcpLogin.flow != nil {
			model.mcpLogin.flow.Cancel()
		}
		clear(model.mcpLogin.input)
	}
	model.mcpLogin = nil
	model.mcpReconnectPrompt = nil
	model.mcpErrorServer, model.mcpErrorDetail = "", ""
	model.mcpViewer = nil
	model.notificationCenter = nil
	model.notificationSettings = nil
}

func (model *tuiModel) configureInput(path string) error {
	history, err := loadInputHistory(path)
	if err != nil {
		model.inputHistory = &inputHistory{path: path, index: -1}
	} else {
		model.inputHistory = history
	}
	model.inputFiles = workspaceFiles(model.workingDir, 5_000)
	return err
}

func (model *tuiModel) setInputMode(mode inputMode) {
	model.inputMode = mode
	model.composer.Prompt = mode.prompt()
	if model.width > 0 {
		model.composer.SetWidth(max(model.width-4, 10))
	}
	model.updateInputCompletion()
}

func (model *tuiModel) updateInputModeFromValue() {
	if model.composer.Value() != "" || model.inputMode != inputNormal {
		return
	}
	model.setInputMode(inputNormal)
}

func (model *tuiModel) handleComposerKey(message tea.KeyMsg) (tea.Cmd, bool) {
	if message.Paste {
		model.insertPaste(string(message.Runes))
		return nil, true
	}
	// Legacy terminals can deliver a paste as one unmarked run of runes.
	if message.Type == tea.KeyRunes && len(message.Runes) >= legacyPasteMinimumRunes {
		model.insertPaste(string(message.Runes))
		return nil, true
	}
	if message.String() == "backspace" || message.String() == "delete" {
		if model.deletePastePlaceholder(message.String() == "backspace") {
			return nil, true
		}
	}
	if model.inputCompletion.kind != completionNone && len(model.inputCompletion.items) > 0 {
		switch message.String() {
		case "up":
			model.inputCompletion.selected = (model.inputCompletion.selected - 1 + len(model.inputCompletion.items)) % len(model.inputCompletion.items)
			return nil, true
		case "down":
			model.inputCompletion.selected = (model.inputCompletion.selected + 1) % len(model.inputCompletion.items)
			return nil, true
		case "tab":
			model.applyInputCompletion()
			return nil, true
		case "enter":
			kind := model.inputCompletion.kind
			model.applyInputCompletion()
			if kind == completionSlash {
				return model.submitComposer(), true
			}
			return nil, true
		case "esc":
			model.inputCompletion = completionState{}
			model.relayout()
			return nil, true
		}
	}
	if model.composer.Value() == "" {
		switch message.String() {
		case "!":
			if model.inputMode == inputShell {
				model.setInputMode(inputIncognitoShell)
			} else if model.inputMode == inputNormal {
				model.setInputMode(inputShell)
			} else {
				return nil, false
			}
			model.relayout()
			return nil, true
		case "/":
			if model.inputMode == inputNormal {
				model.setInputMode(inputCommand)
				model.relayout()
				return nil, true
			}
		case "backspace":
			if model.inputMode != inputNormal {
				model.setInputMode(inputNormal)
				model.relayout()
				return nil, true
			}
		case "up":
			return model.navigateInputHistory(true), true
		case "down":
			return model.navigateInputHistory(false), true
		}
	}
	if model.inputHistory != nil && model.composer.LineCount() == 1 {
		switch message.String() {
		case "up":
			return model.navigateInputHistory(true), true
		case "down":
			return model.navigateInputHistory(false), true
		}
	}
	return nil, false
}

func (model *tuiModel) deletePastePlaceholder(backward bool) bool {
	value := model.composer.Value()
	cursor := model.composerRuneOffset()
	if cursor < 0 {
		return false
	}
	runes := []rune(value)
	for placeholder := range model.pasteBindings {
		placeholderRunes := []rune(placeholder)
		for start := 0; start+len(placeholderRunes) <= len(runes); start++ {
			if string(runes[start:start+len(placeholderRunes)]) != placeholder {
				continue
			}
			if backward && cursor != start+len(placeholderRunes) || !backward && cursor != start {
				continue
			}
			model.composer.SetValue(string(append(runes[:start], runes[start+len(placeholderRunes):]...)))
			model.setComposerRuneOffset(start)
			delete(model.pasteBindings, placeholder)
			model.updateInputCompletion()
			model.relayout()
			model.refreshTranscript()
			return true
		}
	}
	for placeholder := range model.inputMedia {
		placeholderRunes := []rune(placeholder)
		for start := 0; start+len(placeholderRunes) <= len(runes); start++ {
			if string(runes[start:start+len(placeholderRunes)]) != placeholder {
				continue
			}
			if backward && cursor != start+len(placeholderRunes) || !backward && cursor != start {
				continue
			}
			model.composer.SetValue(string(append(runes[:start], runes[start+len(placeholderRunes):]...)))
			model.setComposerRuneOffset(start)
			delete(model.inputMedia, placeholder)
			model.updateInputCompletion()
			model.relayout()
			model.refreshTranscript()
			return true
		}
	}
	return false
}

func (model *tuiModel) navigateInputHistory(previous bool) tea.Cmd {
	if model.inputHistory == nil {
		return nil
	}
	var value string
	var ok bool
	if previous {
		value, ok = model.inputHistory.previous(model.inputMode.prefix() + model.composer.Value())
	} else {
		value, ok = model.inputHistory.next()
	}
	if !ok {
		return nil
	}
	model.setRawComposerValue(value)
	model.relayout()
	model.refreshTranscript()
	return nil
}

func (model *tuiModel) setRawComposerValue(value string) {
	mode := inputNormal
	switch {
	case strings.HasPrefix(value, "!!"):
		mode, value = inputIncognitoShell, strings.TrimPrefix(value, "!!")
	case strings.HasPrefix(value, "!"):
		mode, value = inputShell, strings.TrimPrefix(value, "!")
	case strings.HasPrefix(value, "/"):
		mode, value = inputCommand, strings.TrimPrefix(value, "/")
	}
	model.setInputMode(mode)
	model.composer.SetValue(value)
	model.composer.CursorEnd()
	model.updateInputCompletion()
}

func (model *tuiModel) updateInputCompletion() {
	value := model.composer.Value()
	if model.inputMode == inputCommand && !strings.ContainsAny(value, " \t\n") {
		model.inputCompletion = completionState{kind: completionSlash, items: rankedCompletions(slashCommandNames, "/"+value, 10)}
		return
	}
	marker := strings.LastIndex(value, "@")
	if marker >= 0 && !strings.ContainsAny(value[marker+1:], " \t\n") {
		model.inputCompletion = completionState{kind: completionFile, items: rankedCompletions(model.inputFiles, value[marker+1:], 10)}
		return
	}
	model.inputCompletion = completionState{}
}

func (model *tuiModel) applyInputCompletion() {
	if len(model.inputCompletion.items) == 0 {
		return
	}
	selected := model.inputCompletion.items[model.inputCompletion.selected]
	if model.inputCompletion.kind == completionSlash {
		model.composer.SetValue(strings.TrimPrefix(selected, "/"))
	} else {
		value := model.composer.Value()
		marker := strings.LastIndex(value, "@")
		if marker >= 0 {
			model.composer.SetValue(value[:marker] + "@" + selected + " ")
		}
	}
	model.composer.CursorEnd()
	model.inputCompletion = completionState{}
	model.relayout()
	model.refreshTranscript()
}

func (model *tuiModel) insertPaste(value string) {
	if paths := parsePastedPaths(model.workingDir, value); len(paths) > 0 {
		mentions := make([]string, len(paths))
		for index, path := range paths {
			if block, ok := loadMediaInput(model.workingDir, path); ok {
				var placeholder string
				if block.Type == damessage.BlockImage {
					model.imageSequence++
					placeholder = fmt.Sprintf("[image %d]", model.imageSequence)
				} else {
					model.videoSequence++
					placeholder = fmt.Sprintf("[video %d]", model.videoSequence)
				}
				model.inputMedia[placeholder] = block
				mentions[index] = placeholder
			} else {
				mentions[index] = "@" + path
			}
		}
		model.composer.InsertString(strings.Join(mentions, " ") + " ")
	} else if len([]rune(value)) > largePasteCharacters || strings.Count(value, "\n") > largePasteNewlines {
		model.pasteSequence++
		lines := strings.Count(value, "\n") + 1
		placeholder := fmt.Sprintf("[Pasted text #%d]", model.pasteSequence)
		if lines > 1 {
			placeholder = fmt.Sprintf("[Pasted text #%d +%d lines]", model.pasteSequence, lines-1)
		}
		model.pasteBindings[placeholder] = value
		model.composer.InsertString(placeholder)
	} else {
		model.composer.InsertString(value)
	}
	model.updateInputCompletion()
	model.relayout()
	model.refreshTranscript()
}

func (model *tuiModel) expandedComposerValue() string {
	value := model.composer.Value()
	for placeholder, pasted := range model.pasteBindings {
		value = strings.ReplaceAll(value, placeholder, pasted)
	}
	return value
}

func (model *tuiModel) submitComposer() tea.Cmd {
	value := strings.TrimSpace(model.expandedComposerValue())
	if value == "" {
		return nil
	}
	model.dismissStartupTip()
	mode := model.inputMode
	if mode == inputNormal {
		switch {
		case strings.HasPrefix(value, "!!"):
			mode, value = inputIncognitoShell, strings.TrimSpace(strings.TrimPrefix(value, "!!"))
		case strings.HasPrefix(value, "!"):
			mode, value = inputShell, strings.TrimSpace(strings.TrimPrefix(value, "!"))
		case strings.HasPrefix(value, "/"):
			mode, value = inputCommand, strings.TrimSpace(strings.TrimPrefix(value, "/"))
		}
	}
	displayValue := value
	attachments := make([]damessage.ContentBlock, 0, len(model.inputMedia))
	placeholders := make([]string, 0, len(model.inputMedia))
	for placeholder := range model.inputMedia {
		placeholders = append(placeholders, placeholder)
	}
	sort.Strings(placeholders)
	for _, placeholder := range placeholders {
		if strings.Contains(value, placeholder) {
			value = strings.TrimSpace(strings.Replace(value, placeholder, "", 1))
			attachments = append(attachments, model.inputMedia[placeholder])
		}
	}
	raw := mode.prefix() + displayValue
	if model.inputHistory != nil {
		if err := model.inputHistory.add(raw); err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not save input history: " + err.Error()})
		}
		model.inputHistory.resetNavigation()
	}
	model.discardDraftUndo()
	model.confirmations.disarm(confirmClearInput)
	model.composer.Reset()
	model.pasteBindings = map[string]string{}
	model.inputMedia = map[string]damessage.ContentBlock{}
	model.imageSequence = 0
	model.videoSequence = 0
	model.inputCompletion = completionState{}
	model.setInputMode(inputNormal)
	model.relayout()
	busy := model.deferredDrain != nil || model.running || model.shellRunning || model.approval != nil || model.askUser != nil || model.skillTrust != nil || model.pluginManager != nil || model.pluginReloadPrompt || model.pluginReloading
	bypass := false
	if mode == inputCommand {
		bypass = canBypassCommandQueue("/"+value, commandQueueState{
			AgentRunning: model.running || model.deferredDrain != nil, ShellRunning: model.shellRunning,
			ModalRunning:  model.approval != nil || model.askUser != nil || model.skillTrust != nil || model.pluginManager != nil || model.pluginReloadPrompt || model.pluginReloading,
			StartupFailed: model.startupFailed,
			Switching:     model.resumeController != nil || model.restarting,
		})
	}
	if busy && !bypass {
		model.inputQueue = append(model.inputQueue, queuedInput{mode: mode, value: value, display: displayValue, attachments: attachments})
		model.status = fmt.Sprintf("Queued (%d)", len(model.inputQueue))
		model.appendItem(transcriptItem{kind: itemNotice, text: fmt.Sprintf("Queued input #%d.", len(model.inputQueue))})
		model.refreshTranscript()
		return nil
	}
	return model.dispatchInput(mode, value, displayValue, attachments)
}

func (model *tuiModel) dispatchInput(mode inputMode, value, display string, attachments []damessage.ContentBlock) tea.Cmd {
	switch mode {
	case inputShell, inputIncognitoShell:
		if value == "" {
			return nil
		}
		shellContext, cancel := context.WithCancel(model.ctx)
		model.shellCancel = cancel
		model.shellRunning = true
		model.applyCursorPreference(false)
		model.status = "Running shell command"
		if mode == inputShell {
			model.appendItem(transcriptItem{kind: itemNotice, text: "$ " + value})
		}
		model.refreshTranscript()
		return tea.Batch(
			executeShellCommandGeneration(shellContext, model.workingDir, value, mode == inputIncognitoShell, model.operationGeneration),
			model.startStatusSpinner(),
		)
	case inputCommand:
		command := "/" + value
		if result, ok := model.slashCommand(command); ok {
			return result
		}
		return nil
	default:
		if len(model.shellContext) == 0 && len(attachments) == 0 {
			return model.submitPrompt(value)
		}
		messageText := value
		if len(model.shellContext) > 0 {
			messageText = "Local shell context:\n" + strings.Join(model.shellContext, "\n\n") + "\n\nUser request:\n" + value
		}
		model.shellContext = nil
		model.appendItem(transcriptItem{kind: itemUser, text: display})
		model.currentAssistant = -1
		content := make([]damessage.ContentBlock, 0, len(attachments)+1)
		if messageText != "" {
			content = append(content, damessage.ContentBlock{Type: damessage.BlockText, Text: messageText})
		}
		content = append(content, attachments...)
		return model.startStream(dagent.Input{
			Config:   dacheckpoint.Config{ThreadID: model.threadID},
			Messages: []damessage.Message{{Role: damessage.RoleHuman, Content: content}}, SkipValueEvents: true,
		})
	}
}

func (model *tuiModel) finishShell(message shellDoneMsg) (tea.Model, tea.Cmd) {
	model.shellRunning = false
	model.shellCancel = nil
	model.applyCursorPreference(!model.terminalBlurred)
	if !message.incognito {
		entry := "$ " + message.command
		if message.output != "" {
			entry += "\n" + message.output
		}
		model.shellContext = append(model.shellContext, entry)
	}
	if message.output != "" {
		model.appendItem(transcriptItem{kind: itemNotice, text: message.output})
	}
	if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Shell command failed: " + message.err.Error()})
		model.status = "Shell error"
	} else {
		model.status = "Ready"
	}
	model.refreshTranscript()
	return model, model.drainInputQueue()
}

func (model *tuiModel) drainInputQueue() tea.Cmd {
	if model.deferredDrain != nil || model.running || model.shellRunning || model.approval != nil || model.askUser != nil || model.skillTrust != nil || model.pluginManager != nil || model.pluginReloadPrompt || model.pluginReloading {
		return nil
	}
	var next *queuedInput
	var prompt tea.Cmd
	if len(model.inputQueue) > 0 {
		value := model.inputQueue[0]
		next = &value
		model.inputQueue = model.inputQueue[1:]
		prompt = func() tea.Msg { return queuedInputDispatchMsg{Input: value} }
	}
	if command, draining := model.deferredActions.drainBeforePrompt(prompt); draining {
		return command
	}
	if next != nil {
		return model.dispatchInput(next.mode, next.value, next.display, next.attachments)
	}
	return nil
}

func (model *tuiModel) slashCommand(prompt string) (tea.Cmd, bool) {
	command := strings.TrimSpace(strings.TrimPrefix(prompt, "/"))
	if command == prompt {
		return nil, false
	}
	if strings.EqualFold(command, "debug") {
		return model.toggleDebugConsole(), true
	}
	if injection := resolveDebugConsoleHiddenCommand("/" + command); injection.Action == debugConsoleInjectErrorAction {
		if model.debugConsoleBuffer != nil {
			injection.apply(model.debugConsoleBuffer)
		}
		model.appendItem(transcriptItem{kind: itemError, text: injection.VisibleError})
		model.refreshTranscript()
		return nil, true
	}
	if canonical, exists := canonicalSlashInput("/" + command); exists {
		command = strings.TrimSpace(strings.TrimPrefix(canonical, "/"))
	}
	if command == "goal" || strings.HasPrefix(command, "goal ") {
		return model.goalCommand(strings.TrimSpace(strings.TrimPrefix(command, "goal"))), true
	}
	if command == "workflow" || strings.HasPrefix(command, "workflow ") {
		return model.workflowCommand(strings.TrimSpace(strings.TrimPrefix(command, "workflow"))), true
	}
	if command == "rubric" || command == "criteria" || strings.HasPrefix(command, "rubric ") || strings.HasPrefix(command, "criteria ") {
		name := "rubric"
		if command == "criteria" || strings.HasPrefix(command, "criteria ") {
			name = "criteria"
		}
		return model.rubricCommand(name, strings.TrimSpace(strings.TrimPrefix(command, name))), true
	}
	if command == "effort" || strings.HasPrefix(command, "effort ") {
		return model.effortCommand(strings.TrimSpace(strings.TrimPrefix(command, "effort"))), true
	}
	if command == "model" || strings.HasPrefix(command, "model ") {
		selection := strings.TrimSpace(strings.TrimPrefix(command, "model"))
		if selection == "" {
			model.openModelSelector()
			return nil, true
		}
		return model.selectRuntimeModel(selection), true
	}
	if command == "auto" || strings.HasPrefix(command, "auto ") {
		return model.autoClassifierCommand("/" + command), true
	}
	if command == "trace" {
		return model.startTraceCommand(), true
	}
	if command == "update" {
		return model.startUpdateCommand(), true
	}
	if strings.HasPrefix(command, "update ") {
		model.appendItem(transcriptItem{kind: itemError, text: "Usage: /update"})
		model.refreshTranscript()
		return nil, true
	}
	if command == "auto-update" {
		return model.toggleAutoUpdate(), true
	}
	if strings.HasPrefix(command, "auto-update ") {
		model.appendItem(transcriptItem{kind: itemError, text: "Usage: /auto-update"})
		model.refreshTranscript()
		return nil, true
	}
	if strings.HasPrefix(command, "trace ") {
		model.appendItem(transcriptItem{kind: itemError, text: "Usage: /trace"})
		model.refreshTranscript()
		return nil, true
	}
	if command == "install" || strings.HasPrefix(command, "install ") {
		model.openInstallSelector(strings.TrimSpace(strings.TrimPrefix(command, "install")))
		return nil, true
	}
	if command == "threads" || strings.HasPrefix(command, "threads ") {
		return model.threadsCommand(strings.TrimSpace(strings.TrimPrefix(command, "threads"))), true
	}
	if command == "auth" || command == "connect" {
		return model.openAuthManager(), true
	}
	if command == "mcp" {
		return model.openMCPViewer(), true
	}
	if strings.HasPrefix(command, "mcp ") {
		arguments := strings.Fields(strings.TrimSpace(strings.TrimPrefix(command, "mcp")))
		switch {
		case len(arguments) == 2 && arguments[0] == "login":
			return model.startMCPLogin(arguments[1]), true
		case len(arguments) == 1 && arguments[0] == "reconnect":
			model.mcpReconnectPrompt = newMCPReconnectPrompt(mcpReconnectApplyChanges, nil, model.charset == charsetASCII)
			return nil, true
		default:
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /mcp [login SERVER|reconnect]"})
			model.refreshTranscript()
			return nil, true
		}
	}
	if command == "remember" || strings.HasPrefix(command, "remember ") {
		request := strings.TrimSpace(strings.TrimPrefix(command, "remember"))
		return loadSkillCommand(model.ctx, model.runner, "remember", request, "/"+command), true
	}
	if command == "skill-creator" || strings.HasPrefix(command, "skill-creator ") {
		request := strings.TrimSpace(strings.TrimPrefix(command, "skill-creator"))
		return loadSkillCommand(model.ctx, model.runner, "skill-creator", request, "/"+command), true
	}
	if strings.HasPrefix(command, "skill:") {
		invocation := strings.TrimSpace(strings.TrimPrefix(command, "skill:"))
		parts := strings.SplitN(invocation, " ", 2)
		name := parts[0]
		request := ""
		if len(parts) == 2 {
			request = strings.TrimSpace(parts[1])
		}
		return loadSkillCommand(model.ctx, model.runner, name, request, "/"+command), true
	}
	switch command {
	case "help":
		model.appendItem(transcriptItem{kind: itemNotice, text: slashCommandHelp(model.externalEditorName, model.glyphs)})
		model.refreshTranscript()
		return nil, true
	case "agents":
		model.agentPicker = &agentPickerState{loading: true}
		return listAgents(model.ctx, model.runner), true
	case "plugins":
		controller, ok := model.runner.(pluginRuntimeController)
		if !ok {
			model.appendItem(transcriptItem{kind: itemError, text: "Plugin runtime is unavailable."})
			model.refreshTranscript()
			return nil, true
		}
		model.pluginManager = newPluginManagerState()
		return loadPluginManager(model.ctx, controller), true
	case "workflows":
		return model.showWorkflows(), true
	case "reload":
		controller, ok := model.runner.(pluginRuntimeController)
		if !ok {
			model.appendItem(transcriptItem{kind: itemError, text: "Plugin runtime is unavailable."})
			model.refreshTranscript()
			return nil, true
		}
		model.appendItem(transcriptItem{kind: itemUser, text: "/reload"})
		model.pluginReloading = true
		model.refreshTranscript()
		return reloadPluginRuntime(model.ctx, controller), true
	case "restart":
		if model.restartController == nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Restart is unavailable because this session does not own a local agent server."})
			model.refreshTranscript()
			return nil, true
		}
		model.restartPrompt = newRestartPrompt("local agent server", "Restart")
		return nil, true
	case "offload", "compact":
		return model.startCompaction(), true
	case "changelog":
		return model.openExternalURLCommand("/changelog", changelogURL), true
	case "docs":
		return model.openExternalURLCommand("/docs", docsURL), true
	case "editor":
		return model.openEditor(), true
	case "feedback":
		return model.openExternalURLCommand("/feedback", feedbackURL), true
	case "theme":
		model.themePicker = newThemePicker(model.themeRegistry, model.themeName, model.terminalTheme)
		return nil, true
	case "version", "about":
		model.appendItem(transcriptItem{kind: itemUser, text: "/" + command})
		model.appendItem(transcriptItem{kind: itemNotice, text: versionSummary()})
		model.refreshTranscript()
		return nil, true
	case "copy":
		model.appendItem(transcriptItem{kind: itemUser, text: "/copy"})
		content, streamingPending := model.latestFinishedAssistant()
		if content == "" {
			text := "No message to copy yet."
			if streamingPending {
				text = "Latest assistant message is still streaming; try again in a moment."
			}
			model.appendItem(transcriptItem{kind: itemNotice, text: text})
			model.refreshTranscript()
			return nil, true
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: "Copied latest response to clipboard."})
		model.refreshTranscript()
		return model.stageTerminalSequences(osc52ClipboardSequence(content), ""), true
	case "manual":
		return model.setApprovalMode(approvalManual), true
	case "yolo":
		return model.setApprovalMode(approvalYOLO), true
	case "scrollbar":
		model.showScrollbar = !model.showScrollbar
		model.relayout()
		label := "hidden"
		if model.scrollbarVisible() {
			label = "shown"
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: "Chat scrollbar " + label + "."})
		model.refreshTranscript()
		return model.persistDisplaySettings(), true
	case "timestamps":
		model.showTimestamps = !model.showTimestamps
		label := "hidden"
		if model.showTimestamps {
			label = "shown"
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: "Message timestamps " + label + "."})
		model.refreshTranscript()
		return model.persistDisplaySettings(), true
	case "line-numbers":
		model.showLineNumbers = !model.showLineNumbers
		label := "hidden"
		if model.showLineNumbers {
			label = "shown"
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: "Diff line numbers " + label + " for new diffs."})
		model.refreshTranscript()
		return model.persistDisplaySettings(), true
	case "tools":
		definitions := model.runner.Tools()
		lines := make([]string, 0, len(definitions)+1)
		lines = append(lines, "Available tools:")
		for _, definition := range definitions {
			lines = append(lines, "- "+unicodesecurity.RenderMarkers(definition.Name)+" — "+unicodesecurity.RenderMarkers(definition.Description))
		}
		if len(definitions) == 0 {
			lines = append(lines, "(none)")
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: strings.Join(lines, "\n")})
		model.refreshTranscript()
		return nil, true
	case "skills":
		return listSkillsCommand(model.ctx, model.runner), true
	case "context":
		model.contextScreen = true
		return nil, true
	case "notifications":
		model.openNotificationSettings()
		return nil, true
	case "tokens":
		model.appendItem(transcriptItem{kind: itemUser, text: "/tokens"})
		model.appendItem(transcriptItem{kind: itemNotice, text: model.tokenUsageSummary()})
		model.refreshTranscript()
		return nil, true
	case "cost":
		model.appendItem(transcriptItem{kind: itemUser, text: "/cost"})
		if command := model.requestCostReport(true); command != nil {
			model.refreshTranscript()
			return command, true
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: model.costSummary()})
		model.refreshTranscript()
		return nil, true
	case "clear":
		return model.applyClearCommand(false), true
	case "force-clear":
		return model.applyClearCommand(true), true
	case "quit":
		return tea.Quit, true
	default:
		model.appendItem(transcriptItem{kind: itemError, text: "Unknown command: /" + command})
		model.refreshTranscript()
		return nil, true
	}
}

func (model *tuiModel) openEditor() tea.Cmd {
	command, err := model.editDraft(model.composer.Value())
	if err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "External editor failed. Check $VISUAL/$EDITOR."})
		model.refreshTranscript()
		return nil
	}
	return command
}

func (model *tuiModel) openGoalReviewEditor() tea.Cmd {
	if model.goalReview == nil || model.goalReview.mode == goalReviewMenu {
		return nil
	}
	command, err := model.editDraft(model.goalReview.input.Value())
	if err != nil {
		model.goalReview.warning = "External editor failed. Check $VISUAL/$EDITOR."
		model.refreshTranscript()
		return nil
	}
	model.editorGoalReview = true
	return command
}

func (model *tuiModel) setApprovalMode(mode approvalMode) tea.Cmd {
	if mode == approvalYOLO && model.autoModeNoticeConfigured && !model.yoloModeAcknowledged {
		model.yoloModeNotice = true
		model.relayout()
		model.refreshTranscript()
		return nil
	}
	if mode == approvalManual && model.approval != nil && (model.approval.preparingReview || model.approval.reviewing) {
		model.approval.freezeReview = true
		model.approval.preparingReview = false
		if model.approval.reviewing && model.turnCancel != nil {
			model.turnCancel()
		}
		model.status = "Switching to Manual"
		model.refreshTranscript()
	}
	return model.persistApprovalMode(mode, false)
}

func (model *tuiModel) persistApprovalMode(mode approvalMode, startAfter bool) tea.Cmd {
	if model.approvalModeStore == nil {
		command := model.applyApprovalMode(mode)
		if startAfter {
			return tea.Batch(command, model.startAfterApprovalNotice())
		}
		return command
	}
	store := model.approvalModeStore
	threadID := model.threadID
	model.approvalModeGeneration++
	generation := model.approvalModeGeneration
	model.approvalModePending = mode
	model.approvalModePendingSet = true
	store.registerGeneration(threadID, generation)
	return func() tea.Msg {
		return approvalModeSavedMsg{threadID: threadID, mode: mode, startAfter: startAfter, generation: generation, err: store.saveGeneration(threadID, mode, generation)}
	}
}

func (model *tuiModel) applyApprovalMode(mode approvalMode) tea.Cmd {
	if model.approvalMode != mode {
		model.autoClassifierReset = true
	}
	model.approvalMode = mode
	model.autoModeNotice = false
	model.yoloModeNotice = false
	manualFrozen := false
	if model.approval != nil {
		model.approval.autoFallback = false
		manualFrozen = mode == approvalManual && model.approval.freezeReview
		model.approval.freezeReview = false
		if mode != approvalAuto {
			model.approval.preparingReview = false
			if model.approval.reviewing && model.turnCancel != nil {
				model.turnCancel()
			}
		} else if model.running && !model.approval.reviewing {
			model.approval.preparingReview = true
		}
	}
	model.relayout()
	model.refreshTranscript()
	if manualFrozen && model.approval != nil && model.approval.ready && !model.approval.reviewing {
		if model.userIsTyping(time.Now()) {
			model.approval.deferred = true
			model.approval.typingProtected = true
			model.approval.deferredAt = time.Now()
			model.approval.deferGeneration++
			model.status = "Waiting for typing to finish"
		} else {
			model.status = "Review action"
		}
		model.refreshTranscript()
		return model.scheduleDeferredApproval()
	}
	if mode == approvalYOLO && !model.running && model.approval != nil && model.approval.ready {
		return model.resolveApproval(true)
	}
	if mode == approvalAuto && !model.autoModeNotice && !model.running && model.approval != nil && model.approval.ready {
		return model.beginAutomaticApprovalReview()
	}
	return nil
}

func (model *tuiModel) goalCommand(arguments string) tea.Cmd {
	text := "/goal"
	if arguments != "" {
		text += " " + arguments
	}
	model.appendItem(transcriptItem{kind: itemUser, text: text})
	action := "set"
	request := dagoal.SetRequest{}
	continueWork := false
	parts := strings.Fields(arguments)
	first := ""
	if len(parts) > 0 {
		first = strings.ToLower(parts[0])
	}
	switch {
	case arguments == "" || arguments == "show" || arguments == "status":
		action = "show"
	case first == "model" && len(parts) <= 2:
		if len(parts) > 2 {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /goal model [provider:model|clear]"})
			model.refreshTranscript()
			return nil
		}
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		return model.setRubricModelCommand("goal", value)
	case (first == "max-iterations" || first == "max_iterations") && len(parts) <= 2:
		if len(parts) != 2 {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /goal max-iterations <positive integer|clear>"})
			model.refreshTranscript()
			return nil
		}
		return model.setRubricIterationsCommand("goal", parts[1])
	case first == "amend":
		feedback := strings.TrimSpace(strings.TrimPrefix(arguments, parts[0]))
		if model.goal == nil || model.goal.Status == dagoal.StatusComplete {
			model.appendItem(transcriptItem{kind: itemNotice, text: "No active goal to amend. Use /goal <objective> to create one."})
			model.refreshTranscript()
			return nil
		}
		if feedback == "" {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /goal amend <feedback>"})
			model.refreshTranscript()
			return nil
		}
		model.running = true
		model.status = "Drafting goal amendment"
		model.refreshTranscript()
		return draftGoalCriteriaGeneration(model.ctx, model.runner, model.threadID, dagoal.CriteriaRequest{
			Objective: model.goal.Objective, ExistingCriteria: model.goal.Criteria, Feedback: feedback, Amendment: true,
		}, model.operationGeneration)
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
		model.running = true
		model.status = "Drafting goal criteria"
		model.refreshTranscript()
		return draftGoalCriteriaGeneration(model.ctx, model.runner, model.threadID, dagoal.CriteriaRequest{Objective: arguments}, model.operationGeneration)
	}
	model.currentAssistant = -1
	model.running = true
	model.status = "Updating goal"
	model.refreshTranscript()
	return runGoalActionGeneration(model.ctx, model.runner, model.threadID, action, request, continueWork, model.operationGeneration)
}

func draftGoalCriteria(ctx context.Context, runner agentRunner, threadID string, request dagoal.CriteriaRequest) tea.Cmd {
	return draftGoalCriteriaGeneration(ctx, runner, threadID, request, 0)
}

func draftGoalCriteriaGeneration(ctx context.Context, runner agentRunner, threadID string, request dagoal.CriteriaRequest, generation uint64) tea.Cmd {
	return func() tea.Msg {
		proposal, err := runner.DraftGoalCriteria(ctx, threadID, request)
		return goalCriteriaMsg{proposal: proposal, amendment: request.Amendment, err: err, generation: generation}
	}
}

func (model *tuiModel) resolveGoalReview(decision goalReviewDecision) tea.Cmd {
	review := model.goalReview
	model.goalReview = nil
	model.editorGoalReview = false
	model.relayout()
	if review == nil {
		return nil
	}
	switch decision.kind {
	case goalReviewCancelled:
		model.status = "Ready"
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal proposal cancelled."})
		model.refreshTranscript()
		return model.drainInputQueue()
	case goalReviewRejected:
		model.running = true
		model.status = "Regenerating goal criteria"
		model.refreshTranscript()
		return draftGoalCriteriaGeneration(model.ctx, model.runner, model.threadID, dagoal.CriteriaRequest{
			Objective: review.proposal.Objective, ExistingCriteria: review.proposal.Criteria,
			Feedback: decision.feedback, Amendment: review.amendment,
		}, model.operationGeneration)
	case goalReviewAccepted, goalReviewEdited:
		criteria := decision.criteria
		objective := review.proposal.Objective
		status := dagoal.StatusActive
		model.running = true
		model.status = "Applying goal"
		model.refreshTranscript()
		action := "set"
		if review.amendment {
			action = "amend"
		}
		return runGoalActionGeneration(model.ctx, model.runner, model.threadID, action, dagoal.SetRequest{
			Objective: &objective, Criteria: &criteria, Status: &status,
		}, true, model.operationGeneration)
	default:
		return nil
	}
}

func loadGoal(ctx context.Context, runner agentRunner, threadID string) tea.Cmd {
	return loadGoalGeneration(ctx, runner, threadID, 0)
}

func loadGoalGeneration(ctx context.Context, runner agentRunner, threadID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		goal, err := runner.Goal(ctx, threadID)
		if err != nil {
			return goalLoadedMsg{err: err, generation: generation}
		}
		rubric, err := runner.Rubric(ctx, threadID)
		return goalLoadedMsg{goal: goal, rubric: rubric, err: err, generation: generation}
	}
}

func runGoalAction(ctx context.Context, runner agentRunner, threadID, action string, request dagoal.SetRequest, continueWork bool) tea.Cmd {
	return runGoalActionGeneration(ctx, runner, threadID, action, request, continueWork, 0)
}

func runGoalActionGeneration(ctx context.Context, runner agentRunner, threadID, action string, request dagoal.SetRequest, continueWork bool, generation uint64) tea.Cmd {
	return func() tea.Msg {
		if action == "show" {
			goal, err := runner.Goal(ctx, threadID)
			return goalActionMsg{action: action, goal: goal, err: err, generation: generation}
		}
		if action == "clear" {
			cleared, err := runner.ClearGoal(ctx, threadID)
			return goalActionMsg{action: action, cleared: cleared, err: err, generation: generation}
		}
		goal, err := runner.SetGoal(ctx, threadID, request)
		return goalActionMsg{action: action, goal: goal, continueWork: continueWork, err: err, generation: generation}
	}
}

func (model *tuiModel) finishGoalAction(message goalActionMsg) (tea.Model, tea.Cmd) {
	model.running = false
	if message.err != nil {
		model.status = "Goal error"
		model.appendItem(transcriptItem{kind: itemError, text: message.err.Error()})
		model.refreshTranscript()
		return model, model.drainInputQueue()
	}
	if message.action == "clear" {
		model.goal = nil
		model.rubric = dagoapi.RubricSnapshot{}
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
	if message.goal != nil && message.goal.Actionable() && message.goal.Criteria != "" {
		model.rubric.Criteria = message.goal.Criteria
		model.rubric.Status = ""
	} else if message.action != "show" && message.action != "budget" {
		model.rubric = dagoapi.RubricSnapshot{}
	}
	model.status = "Ready"
	switch message.action {
	case "show", "budget":
		model.appendItem(transcriptItem{kind: itemNotice, text: formatGoal(message.goal)})
	case "pause":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal paused."})
	case "resume":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal resumed."})
	case "amend":
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal amended. " + formatGoal(message.goal)})
	default:
		model.appendItem(transcriptItem{kind: itemNotice, text: "Goal accepted. " + formatGoal(message.goal)})
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
	text := fmt.Sprintf("%s\nStatus: %s · %s · %ds", goal.Objective, goal.Status, budget, goal.TimeUsedSeconds)
	if goal.Criteria != "" {
		text += "\nCriteria:\n" + goal.Criteria
	}
	return text
}

func (model *tuiModel) rubricCommand(name, arguments string) tea.Cmd {
	command := "/" + name
	if arguments != "" {
		command += " " + arguments
	}
	model.appendItem(transcriptItem{kind: itemUser, text: command})
	parts := strings.SplitN(arguments, " ", 2)
	subcommand := ""
	value := ""
	if len(parts) > 0 {
		subcommand = strings.ToLower(strings.TrimSpace(parts[0]))
	}
	if len(parts) == 2 {
		value = strings.TrimSpace(parts[1])
	}
	if arguments == "" {
		model.appendItem(transcriptItem{kind: itemNotice, text: rubricUsage()})
		model.refreshTranscript()
		return nil
	}
	switch subcommand {
	case "show", "status":
		text := formatRubricSnapshot(model.rubric)
		if model.nextRubric != "" {
			text += "\n\nNext-turn rubric:\n" + model.nextRubric
		}
		grader, iterations := model.runner.RubricSettings()
		grader = displayModelName(grader)
		text += fmt.Sprintf("\n\nGrader: %s · max iterations: %d", grader, iterations)
		model.appendItem(transcriptItem{kind: itemNotice, text: text})
		model.refreshTranscript()
		return nil
	case "set":
		if value == "" {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /rubric set <criteria>"})
			model.refreshTranscript()
			return nil
		}
		model.running = true
		model.status = "Setting rubric"
		model.refreshTranscript()
		return runRubricActionGeneration(model.ctx, model.runner, model.threadID, "set", value, model.operationGeneration)
	case "next":
		if value == "" {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /rubric next <criteria>"})
		} else {
			model.nextRubric = value
			model.appendItem(transcriptItem{kind: itemNotice, text: "Rubric set for next turn."})
		}
		model.refreshTranscript()
		return nil
	case "file":
		if value == "" {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /rubric file <path>"})
			model.refreshTranscript()
			return nil
		}
		criteria, err := resolveRubricText(rubricFileReference(value), model.workingDir)
		if err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: err.Error()})
			model.refreshTranscript()
			return nil
		}
		model.running = true
		model.status = "Setting rubric"
		model.refreshTranscript()
		return runRubricActionGeneration(model.ctx, model.runner, model.threadID, "file", criteria, model.operationGeneration)
	case "clear":
		model.running = true
		model.status = "Clearing rubric"
		model.refreshTranscript()
		return runRubricActionGeneration(model.ctx, model.runner, model.threadID, "clear", "", model.operationGeneration)
	case "model":
		return model.setRubricModelCommand("rubric", value)
	case "max-iterations", "max_iterations":
		return model.setRubricIterationsCommand("rubric", value)
	default:
		model.appendItem(transcriptItem{kind: itemNotice, text: rubricUsage()})
		model.refreshTranscript()
		return nil
	}
}

func rubricUsage() string {
	return "Usage:\n  /rubric set <criteria>\n  /rubric next <criteria>\n  /rubric file <path>\n  /rubric show\n  /rubric clear\n  /rubric model [provider:model|clear]\n  /rubric max-iterations <positive integer|clear>"
}

func (model *tuiModel) setRubricModelCommand(source, value string) tea.Cmd {
	if value == "" {
		grader, _ := model.runner.RubricSettings()
		label := source
		if label != "" {
			label = strings.ToUpper(label[:1]) + label[1:]
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: label + " grader model: " + grader})
		model.refreshTranscript()
		return nil
	}
	model.running = true
	model.status = "Changing grader model"
	model.refreshTranscript()
	generation := model.operationGeneration
	return func() tea.Msg {
		err := model.runner.SetRubricModel(model.ctx, value)
		return rubricActionMsg{action: source + " model", err: err, generation: generation}
	}
}

func (model *tuiModel) setRubricIterationsCommand(source, value string) tea.Cmd {
	iterations := 0
	if !strings.EqualFold(strings.TrimSpace(value), "clear") {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed < 1 {
			model.appendItem(transcriptItem{kind: itemError, text: "Usage: /" + source + " max-iterations <positive integer|clear>"})
			model.refreshTranscript()
			return nil
		}
		iterations = parsed
	}
	model.running = true
	model.status = "Changing grader iterations"
	model.refreshTranscript()
	generation := model.operationGeneration
	return func() tea.Msg {
		err := model.runner.SetRubricMaxIterations(iterations)
		return rubricActionMsg{action: source + " max-iterations", err: err, generation: generation}
	}
}

func runRubricAction(ctx context.Context, runner agentRunner, threadID, action, criteria string) tea.Cmd {
	return runRubricActionGeneration(ctx, runner, threadID, action, criteria, 0)
}

func runRubricActionGeneration(ctx context.Context, runner agentRunner, threadID, action, criteria string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		if action == "clear" || action == "one-shot-clear" {
			cleared, err := runner.ClearRubric(ctx, threadID)
			return rubricActionMsg{action: action, cleared: cleared, err: err, generation: generation}
		}
		rubric, err := runner.SetRubric(ctx, threadID, criteria)
		return rubricActionMsg{action: action, rubric: rubric, err: err, generation: generation}
	}
}

func (model *tuiModel) finishRubricAction(message rubricActionMsg) (tea.Model, tea.Cmd) {
	model.running = false
	model.status = "Ready"
	if message.err != nil {
		model.appendItem(transcriptItem{kind: itemError, text: message.err.Error()})
		model.refreshTranscript()
		return model, model.drainInputQueue()
	}
	if message.action == "one-shot-clear" || message.action == "one-shot-restore" {
		model.rubric = message.rubric
		model.oneShotRubric = false
		model.oneShotPreviousRubric = ""
		model.refreshTranscript()
		return model, model.drainInputQueue()
	}
	if strings.HasSuffix(message.action, " model") {
		grader, _ := model.runner.RubricSettings()
		model.appendItem(transcriptItem{kind: itemNotice, text: "Grader model set to " + grader + "."})
	} else if strings.HasSuffix(message.action, " max-iterations") {
		_, iterations := model.runner.RubricSettings()
		model.appendItem(transcriptItem{kind: itemNotice, text: fmt.Sprintf("Grader max iterations set to %d.", iterations)})
	} else if message.action == "clear" {
		model.rubric = dagoapi.RubricSnapshot{}
		model.nextRubric = ""
		model.oneShotPreviousRubric = ""
		if message.cleared {
			model.appendItem(transcriptItem{kind: itemNotice, text: "Rubric cleared."})
		} else {
			model.appendItem(transcriptItem{kind: itemNotice, text: "No rubric set. Nothing to clear."})
		}
	} else {
		model.rubric = message.rubric
		label := "Rubric set."
		if message.action == "file" {
			label = "Rubric set from file."
		}
		model.appendItem(transcriptItem{kind: itemNotice, text: label})
	}
	model.refreshTranscript()
	return model, model.drainInputQueue()
}

func (model *tuiModel) submitPrompt(prompt string) tea.Cmd {
	model.appendItem(transcriptItem{kind: itemUser, text: prompt})
	model.autoClassifierTurnID = approvalTurnID(model.threadID, len(model.items), prompt)
	model.currentAssistant = -1
	input := dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: model.threadID},
		Messages: []damessage.Message{damessage.Human(prompt)}, SkipValueEvents: true,
	}
	if model.nextRubric != "" {
		model.oneShotPreviousRubric = model.rubric.Criteria
		input.State = dastate.Values{dagoapi.RubricKey: model.nextRubric}
		model.rubric = dagoapi.RubricSnapshot{Criteria: model.nextRubric}
		model.nextRubric = ""
		model.oneShotRubric = true
	}
	return model.startStream(input)
}

func (model *tuiModel) startGoalContinuation() tea.Cmd {
	if model.goal == nil || !model.goal.Actionable() {
		return nil
	}
	model.currentAssistant = -1
	model.autoClassifierTurnID = approvalTurnID(model.threadID, len(model.items), model.goal.Objective)
	command := model.startStream(dagent.Input{
		Config:   dacheckpoint.Config{ThreadID: model.threadID},
		Messages: []damessage.Message{dagoal.ContinuationMessage(*model.goal)}, SkipValueEvents: true,
	})
	model.status = "Continuing goal"
	return command
}

func (model *tuiModel) startStream(input dagent.Input) tea.Cmd {
	if model.approvalModeBlocked {
		model.appendItem(transcriptItem{kind: itemError, text: "New runs are blocked until Manual approval mode can be persisted."})
		model.status = "Approval mode unavailable"
		model.refreshTranscript()
		return nil
	}
	turnContext, cancel := context.WithCancel(model.ctx)
	configurable := make(map[string]any, len(input.Configurable)+1)
	for key, value := range input.Configurable {
		configurable[key] = value
	}
	configurable[dagent.RuntimeModelConfigKey] = model.modelName
	input.Configurable = configurable
	model.turnCancel = cancel
	model.stream = model.runner.Start(turnContext, input)
	model.running = true
	model.cancelling = false
	model.applyCursorPreference(false)
	model.status = "Thinking"
	if model.subagentPanel != nil {
		model.subagentPanel.prepareTurn(displayModelName(model.modelName))
	}
	model.appendDebugLog("INFO", "Agent run started",
		debugConsoleAttribute{Key: "thread_id", Value: model.threadID},
		debugConsoleAttribute{Key: "model", Value: displayModelName(model.modelName)},
	)
	model.relayout()
	model.refreshTranscript()
	return tea.Batch(
		waitForStreamGeneration(turnContext, model.stream, model.operationGeneration),
		model.startStatusSpinner(),
	)
}

func waitForStream(ctx context.Context, stream eventStream) tea.Cmd {
	return waitForStreamGeneration(ctx, stream, 0)
}

func waitForStreamGeneration(ctx context.Context, stream eventStream, generation uint64) tea.Cmd {
	return func() tea.Msg {
		if stream == nil {
			return streamDoneMsg{err: fmt.Errorf("agent stream is unavailable"), generation: generation}
		}
		event, err := stream.Next(ctx)
		if err == nil {
			return streamEventMsg{event: event, generation: generation}
		}
		defer stream.Close()
		if !errors.Is(err, io.EOF) {
			return streamDoneMsg{err: err, generation: generation}
		}
		result, resultErr := stream.Result(ctx)
		return streamDoneMsg{result: result, err: resultErr, generation: generation}
	}
}

func (model *tuiModel) applyEvent(event dagent.Event) {
	if panelEvent, ok := subagentPanelEventFromAgentEvent(event); ok && model.subagentPanel != nil {
		if model.subagentPanel.apply(panelEvent) {
			model.appendDebugLog("INFO", "Subagent lifecycle changed",
				debugConsoleAttribute{Key: "subagent", Value: panelEvent.Label},
				debugConsoleAttribute{Key: "phase", Value: fmt.Sprint(panelEvent.Phase)},
			)
			model.relayout()
		}
	}
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
			model.setLastUsage(*usage)
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
					model.setLastUsage(*message.Usage)
				}
			case damessage.RoleTool:
				model.completeTool(message)
			}
		}
	case dagent.EventToolProgress:
		if event.ToolProgress != nil {
			model.updateToolProgress(*event.ToolProgress)
		}
	case dagent.EventCustom:
		var payload struct {
			Type        string                              `json:"type"`
			GradingRun  string                              `json:"grading_run_id"`
			Iteration   int                                 `json:"iteration"`
			Result      dagoapi.RubricResult                `json:"result"`
			Explanation string                              `json:"explanation"`
			Criteria    []dagoapi.RubricCriterionEvaluation `json:"criteria"`
		}
		if json.Unmarshal(event.Custom, &payload) != nil {
			return
		}
		switch payload.Type {
		case "rubric_evaluation_start":
			model.status = "Checking acceptance criteria"
		case "rubric_evaluation_end":
			evaluation := dagoapi.RubricEvaluation{
				GradingRunID: payload.GradingRun, Iteration: payload.Iteration, Result: payload.Result,
				Explanation: payload.Explanation, Criteria: payload.Criteria,
			}
			model.rubric.Status = payload.Result
			model.rubric.Iterations = payload.Iteration + 1
			model.rubric.Evaluations = append(model.rubric.Evaluations, evaluation)
			model.appendItem(transcriptItem{kind: itemNotice, text: formatRubricSnapshot(model.rubric)})
		}
	case dagent.EventInterrupt:
		if event.Interrupt == nil {
			return
		}
		if isAskUserInterrupt(*event.Interrupt) {
			if err := model.presentAskUser(*event.Interrupt); err != nil {
				model.appendItem(transcriptItem{kind: itemError, text: "Cannot display question: " + err.Error()})
			}
			return
		}
		requests, err := decodeApprovalRequests(event.Interrupt.Value)
		if err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Cannot display approval request: " + err.Error()})
			return
		}
		state := model.presentApproval(requests)
		if !state.deferred {
			model.status = "Waiting for review"
		}
	}
}

func (model *tuiModel) appendAssistant(text string) {
	if model.currentAssistant >= 0 && model.currentAssistant < len(model.items) && model.items[model.currentAssistant].kind == itemAssistant {
		model.items[model.currentAssistant].text += text
		return
	}
	model.appendItem(transcriptItem{kind: itemAssistant, text: text, streaming: true})
	model.currentAssistant = len(model.items) - 1
}

func (model *tuiModel) finishCurrentAssistant() {
	if model.currentAssistant < 0 || model.currentAssistant >= len(model.items) {
		return
	}
	item := &model.items[model.currentAssistant]
	if item.kind == itemAssistant {
		item.done = true
		item.streaming = false
	}
	model.currentAssistant = -1
}

func (model *tuiModel) abandonCurrentAssistant() {
	if model.currentAssistant < 0 || model.currentAssistant >= len(model.items) {
		return
	}
	item := &model.items[model.currentAssistant]
	if item.kind == itemAssistant {
		item.streaming = false
	}
	model.currentAssistant = -1
}

func (model *tuiModel) latestFinishedAssistant() (string, bool) {
	streamingPending := false
	for index := len(model.items) - 1; index >= 0; index-- {
		item := model.items[index]
		if item.kind != itemAssistant || strings.TrimSpace(item.text) == "" {
			continue
		}
		if item.streaming {
			streamingPending = true
			continue
		}
		if item.done {
			return item.text, streamingPending
		}
	}
	return "", streamingPending
}

func osc52ClipboardSequence(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

func browserOpenURLSequence(url string) string {
	return "\x1b]777;dago-open-url;" + base64.StdEncoding.EncodeToString([]byte(url)) + "\x07"
}

func terminalBackgroundSequence(background string) string {
	if !hexColorPattern.MatchString(background) {
		return terminalBackgroundResetSequence()
	}
	return terminalBackgroundResetSequence() + "\x1b]11;" + strings.ToUpper(background) + "\x07"
}

func terminalBackgroundResetSequence() string { return "\x1b]111\x07" }

func (model *tuiModel) terminalSequences() string {
	if model == nil {
		return ""
	}
	return model.clipboardSequence + model.browserSequence
}

func flushTerminalSequences(generation uint64) tea.Cmd {
	return tea.Tick(terminalSequenceDisplayDuration, func(time.Time) tea.Msg {
		return terminalSequencesFlushedMsg{generation: generation}
	})
}

func scheduleDebugConsoleTick(generation uint64) tea.Cmd {
	return tea.Tick(debugConsoleRefreshInterval, func(time.Time) tea.Msg {
		return debugConsoleTickMsg{generation: generation}
	})
}

func (model *tuiModel) stageTerminalSequences(clipboard, browser string) tea.Cmd {
	model.clipboardSequence = clipboard
	model.browserSequence = browser
	model.terminalSequenceGeneration++
	return flushTerminalSequences(model.terminalSequenceGeneration)
}

func (model *tuiModel) toggleDebugConsole() tea.Cmd {
	if model.debugConsole != nil {
		model.debugConsole = nil
		model.debugConsoleGeneration++
		model.relayout()
		model.refreshTranscript()
		return nil
	}
	if model.debugConsoleBuffer == nil {
		model.debugConsoleBuffer = newDebugConsoleBuffer(0)
	}
	model.debugConsole = newDebugConsoleOverlay(model.debugConsoleBuffer, debugConsoleOverlayOptions{
		ClearedUpto: model.debugConsoleClearedUpto,
		ClickToCopy: model.debugConsoleClickToCopy,
	})
	model.debugConsole.updateSnapshot(model.debugConsoleSnapshotFields())
	model.debugConsole.poll()
	model.debugConsoleGeneration++
	return scheduleDebugConsoleTick(model.debugConsoleGeneration)
}

func (model *tuiModel) handleDebugConsoleKey(message tea.KeyMsg) tea.Cmd {
	if model.debugConsole == nil {
		return nil
	}
	interaction := model.debugConsole.handleKey(message.String())
	switch interaction.Action {
	case debugKeyClose:
		return model.toggleDebugConsole()
	case debugKeyClear:
		model.debugConsoleClearedUpto = interaction.ClearCursor
		return model.notify("Debug log view cleared.", toastInfo, "")
	case debugKeyCopyVisible, debugKeyCopySelected:
		if interaction.CopyPayload == "" {
			return model.notify("No visible debug log lines to copy.", toastInfo, "")
		}
		model.debugConsoleClickToCopy = model.debugConsole.snapshotView().ClickToCopy
		return tea.Batch(
			model.stageTerminalSequences(osc52ClipboardSequence(interaction.CopyPayload), ""),
			model.notify("Debug log copied.", toastInfo, ""),
		)
	case debugKeyFocusChanged:
		model.debugConsoleClickToCopy = model.debugConsole.snapshotView().ClickToCopy
	}
	return nil
}

func (model *tuiModel) handleDebugConsoleMouse(message tea.MouseMsg) tea.Cmd {
	if model.debugConsole == nil || message.Button != tea.MouseButtonLeft || message.Action != tea.MouseActionPress {
		return nil
	}
	snapshot := model.debugConsole.snapshotView()
	panel := renderDebugConsoleOverlay(snapshot, model.width, model.height, model.glyphs)
	panelWidth, panelHeight := lipgloss.Width(panel), lipgloss.Height(panel)
	originX := max((model.width-panelWidth)/2, 0)
	originY := max((model.height-panelHeight)/2, 0)
	hit := debugConsolePointerHitAt(snapshot, model.width, model.height, message.X-originX, message.Y-originY)
	switch hit.Target {
	case debugConsolePointerSnapshot:
		payload, copyable := model.debugConsole.copySnapshotField(hit.Index)
		if !copyable {
			return nil
		}
		return tea.Batch(
			model.stageTerminalSequences(osc52ClipboardSequence(payload), ""),
			model.notify("Debug snapshot value copied.", toastInfo, ""),
		)
	case debugConsolePointerFilter:
		model.debugConsole.setFilterExpanded(!snapshot.State.FilterExpanded)
	case debugConsolePointerCopyToggle:
		model.debugConsoleClickToCopy = !snapshot.ClickToCopy
		model.debugConsole.setClickToCopy(model.debugConsoleClickToCopy)
	case debugConsolePointerFilterOption:
		model.debugConsole.selectFilterOption(hit.Index)
	case debugConsolePointerRecord:
		interaction := model.debugConsole.selectRecord(hit.Index)
		if interaction.CopyPayload != "" {
			return tea.Batch(
				model.stageTerminalSequences(osc52ClipboardSequence(interaction.CopyPayload), ""),
				model.notify("Debug log copied.", toastInfo, ""),
			)
		}
	}
	return nil
}

func (model *tuiModel) debugConsoleSnapshotFields() []debugConsoleSnapshotField {
	fields := []debugConsoleSnapshotField{
		{Label: "Thread", Value: model.threadID, Copyable: model.threadID != ""},
		{Label: "Model", Value: displayModelName(model.modelName), Copyable: model.modelName != ""},
		{Label: "Agent", Value: model.agentName},
		{Label: "Status", Value: model.status},
		{Label: "Approval", Value: model.approvalMode.String()},
		{Label: "Messages", Value: strconv.Itoa(len(model.items))},
		{Label: "Tokens", Value: strconv.Itoa(max(model.totalTokens, 0))},
		{Label: "Working directory", Value: model.workingDir, Copyable: model.workingDir != ""},
	}
	if model.hookStatus != "" {
		fields = append(fields, debugConsoleSnapshotField{Label: "Hooks", Value: model.hookStatus})
	}
	return fields
}

func (model *tuiModel) appendDebugLog(level, message string, attributes ...debugConsoleAttribute) {
	if model.debugConsoleBuffer == nil {
		return
	}
	number, exists := debugConsoleLevelNumbers[level]
	if !exists {
		number = debugConsoleLevelNumbers["INFO"]
	}
	model.debugConsoleBuffer.append(debugConsoleRecord{
		Time: time.Now(), Level: level, LevelNumber: number, Logger: "dacode.tui", Message: message, Attributes: attributes,
	})
}

func versionSummary() string {
	return fmt.Sprintf("dacode version: %s\ndago (SDK) version: %s\nGo version: %s",
		buildVersion(), dagoapi.Version(), strings.TrimPrefix(runtime.Version(), "go"))
}

func (model *tuiModel) openExternalURLCommand(command, url string) tea.Cmd {
	model.appendItem(transcriptItem{kind: itemUser, text: command})
	model.appendItem(transcriptItem{kind: itemNotice, text: url})
	model.refreshTranscript()
	if model.browserLinks {
		return model.stageTerminalSequences("", browserOpenURLSequence(url))
	}
	opener := model.openURL
	return func() tea.Msg {
		if opener == nil {
			return externalURLOpenedMsg{url: url, err: errors.New("URL opener is unavailable")}
		}
		return externalURLOpenedMsg{url: url, err: opener(url)}
	}
}

func (model *tuiModel) addToolCall(call damessage.ToolCall) {
	if call.ID == "" {
		return
	}
	if _, exists := model.toolItems[call.ID]; exists {
		return
	}
	model.finishCurrentAssistant()
	model.appendItem(transcriptItem{
		kind: itemTool, callID: call.ID, name: call.Name, args: compactJSON(call.Arguments),
		lifecycle: toolRunning, startedAt: time.Now(), lineNums: model.showLineNumbers,
	})
	model.toolItems[call.ID] = len(model.items) - 1
	model.currentAssistant = -1
	model.status = "Using " + call.Name
}

func (model *tuiModel) completeTool(message damessage.Message) {
	index, exists := model.toolItems[message.ToolCallID]
	if !exists {
		model.appendItem(transcriptItem{
			kind: itemTool, callID: message.ToolCallID, name: message.Name,
			lifecycle: toolRunning, startedAt: time.Now(), lineNums: model.showLineNumbers,
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
	if item.lifecycle != toolRejected && item.lifecycle != toolSkipped {
		item.failed = message.ToolStatus == damessage.ToolStatusError
		if item.failed {
			item.lifecycle = toolError
		} else {
			item.lifecycle = toolSuccess
		}
	}
	if isAskUserTool(item.name) && item.failed && strings.TrimSpace(item.text) == "" {
		item.text = askUserFailedSummary
	}
	model.currentAssistant = -1
}

func (model *tuiModel) updateToolProgress(progress datool.Progress) {
	index, exists := model.toolItems[progress.CallID]
	if !exists {
		model.appendItem(transcriptItem{
			kind: itemTool, callID: progress.CallID, name: progress.Name,
			lifecycle: toolRunning, startedAt: time.Now(), lineNums: model.showLineNumbers,
		})
		index = len(model.items) - 1
		model.toolItems[progress.CallID] = index
	}
	item := &model.items[index]
	if progress.Name != "" {
		item.name = progress.Name
	}
	item.text = progress.Output
	if item.startedAt.IsZero() {
		item.startedAt = time.Now()
	}
	if progress.Status == "" {
		item.lifecycle = toolRunning
		return
	}
	item.done = true
	item.failed = progress.Status == damessage.ToolStatusError
	if item.failed {
		item.lifecycle = toolError
	} else {
		item.lifecycle = toolSuccess
	}
	model.currentAssistant = -1
}

func (model *tuiModel) finishStream(message streamDoneMsg) (tea.Model, tea.Cmd) {
	if model.turnCancel != nil {
		model.turnCancel()
	}
	model.turnCancel = nil
	model.stream = nil
	if model.subagentPanel != nil {
		if model.subagentPanel.finalizeRunning() {
			model.relayout()
		}
	}
	if model.cancelling || errors.Is(message.err, context.Canceled) {
		model.autoClassifierPendingResults = nil
		model.appendDebugLog("WARNING", "Agent run cancelled")
		model.abandonCurrentAssistant()
		model.markLatestUserCancelled()
		model.approval = nil
		model.status = "Finalizing cancellation"
		return model, cancelRunGeneration(model.runner, model.threadID, model.operationGeneration)
	}
	model.running = false
	model.flushDeferredTrace()
	model.applyCursorPreference(!model.terminalBlurred)
	if message.err != nil {
		model.startupFailed = len(model.items) <= 2
		model.autoClassifierPendingResults = nil
		model.appendDebugLog("ERROR", "Agent run failed", debugConsoleAttribute{Key: "error", Value: message.err.Error()})
		model.abandonCurrentAssistant()
		model.appendItem(transcriptItem{kind: itemError, text: message.err.Error()})
		model.status = "Error"
		model.refreshTranscript()
		return model, model.drainInputQueue()
	}
	model.startupFailed = false
	model.threadHasCheckpoint = true
	if autoClassifierHasSuccessfulResult(message.result.Messages, model.autoClassifierPendingResults) {
		model.autoClassifierReset = true
	}
	model.autoClassifierPendingResults = nil
	model.finishCurrentAssistant()
	model.appendDebugLog("INFO", "Agent run completed")
	model.restoreUsage(message.result.Messages)
	if goal, present := dagoal.FromState(message.result.State); present {
		model.goal = goal
	}
	if rubric := dagoapi.RubricSnapshotFromState(message.result.State); rubric.Criteria != "" {
		if len(rubric.Evaluations) == 0 {
			rubric.Evaluations = model.rubric.Evaluations
		}
		model.rubric = rubric
	}
	if model.approval == nil && model.askUser == nil && len(message.result.Interrupts) > 0 {
		interrupt := message.result.Interrupts[0]
		if isAskUserInterrupt(interrupt) {
			if err := model.presentAskUser(interrupt); err != nil {
				model.appendItem(transcriptItem{kind: itemError, text: "Cannot display question: " + err.Error()})
			}
		} else {
			requests, err := decodeApprovalRequests(interrupt.Value)
			if err != nil {
				model.appendItem(transcriptItem{kind: itemError, text: "Cannot display approval request: " + err.Error()})
			} else {
				model.presentApproval(requests)
			}
		}
	}
	if model.askUser != nil {
		model.askUser.ready = true
		model.askUser.focusCurrent()
		model.status = "Answer question"
	} else if model.approval != nil {
		model.approval.ready = true
		switch model.effectiveApprovalMode() {
		case approvalYOLO:
			model.approval.deferred = false
			model.approval.typingProtected = false
			model.approval.preparingReview = false
			model.approval.reviewing = false
			model.status = "Applying action"
			model.refreshTranscript()
			return model, model.resolveApproval(true)
		case approvalAuto:
			return model, model.beginAutomaticApprovalReview()
		}
		model.approval.preparingReview = false
		if !model.approval.deferred && model.userIsTyping(time.Now()) {
			model.approval.deferred = true
			model.approval.typingProtected = true
			model.approval.deferredAt = time.Now()
			model.approval.deferGeneration++
		}
		if model.approval.deferred {
			model.status = "Waiting for typing to finish"
		} else {
			model.status = "Review action"
		}
	} else {
		if model.oneShotRubric {
			action := "one-shot-clear"
			if model.oneShotPreviousRubric != "" {
				action = "one-shot-restore"
			}
			model.status = "Restoring rubric"
			model.refreshTranscript()
			return model, runRubricActionGeneration(model.ctx, model.runner, model.threadID, action, model.oneShotPreviousRubric, model.operationGeneration)
		}
		if len(model.pendingWorkflows) > 0 {
			model.refreshTranscript()
			return model, model.startWorkflowContinuation()
		}
		if len(model.inputQueue) > 0 || model.deferredActions.length() > 0 {
			model.status = "Ready"
			model.refreshTranscript()
			return model, model.drainInputQueue()
		}
		if model.goal != nil && model.goal.Actionable() {
			model.refreshTranscript()
			return model, model.startGoalContinuation()
		}
		model.status = "Ready"
	}
	model.refreshTranscript()
	return model, model.scheduleDeferredApproval()
}

func cancelRun(runner agentRunner, threadID string) tea.Cmd {
	return cancelRunGeneration(runner, threadID, 0)
}

func cancelRunGeneration(runner agentRunner, threadID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return cancelDoneMsg{err: runner.Cancel(ctx, threadID), generation: generation}
	}
}

func reviewApproval(ctx context.Context, runner agentRunner, request approvalReviewRequest) tea.Cmd {
	return reviewApprovalGeneration(ctx, runner, request, 0)
}

func reviewApprovalGeneration(ctx context.Context, runner agentRunner, request approvalReviewRequest, generation uint64) tea.Cmd {
	return func() tea.Msg {
		result, err := runner.Review(ctx, request)
		return reviewDoneMsg{result: result, err: err, generation: generation}
	}
}

func (model *tuiModel) beginAutomaticApprovalReview() tea.Cmd {
	state := model.approval
	if state == nil || !state.ready || model.autoModeNotice || model.effectiveApprovalMode() != approvalAuto {
		return nil
	}
	state.deferred = false
	state.typingProtected = false
	state.deferGeneration++
	state.preparingReview = false
	state.reasonMode = false
	state.reason.Blur()
	state.reviewing = true
	state.ready = false
	reviewContext, cancel := context.WithCancel(model.ctx)
	model.turnCancel = cancel
	model.running = true
	model.status = "Reviewing action"
	model.relayout()
	model.refreshTranscript()
	return reviewApprovalGeneration(reviewContext, model.runner, approvalReviewRequest{
		ThreadID: model.threadID, TurnID: model.autoClassifierTurnID, Mode: approvalAuto.String(), Reset: model.autoClassifierReset,
		WorkingDir: model.workingDir, Transcript: model.reviewTranscript(), Requests: state.requests,
		Classifier: model.autoClassifierContext(),
	}, model.operationGeneration)
}

func (model *tuiModel) effectiveApprovalMode() approvalMode {
	if model.approvalModePendingSet && model.approvalModePending == approvalManual {
		return approvalManual
	}
	return model.approvalMode
}

func (model *tuiModel) finishReview(message reviewDoneMsg) (tea.Model, tea.Cmd) {
	if model.turnCancel != nil {
		model.turnCancel()
	}
	model.turnCancel = nil
	model.running = false
	if model.approval == nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Automatic review completed without a pending action."})
		model.status = "Review error"
		model.refreshTranscript()
		return model, nil
	}
	model.approval.reviewing = false
	if model.approval.freezeReview {
		model.approval.ready = true
		model.status = "Switching to Manual"
		model.refreshTranscript()
		return model, nil
	}
	if model.cancelling {
		model.approval = nil
		model.status = "Finalizing cancellation"
		model.refreshTranscript()
		return model, cancelRunGeneration(model.runner, model.threadID, model.operationGeneration)
	}
	if model.approvalMode == approvalManual {
		model.approval.ready = true
		model.status = "Review action"
		model.refreshTranscript()
		return model, model.deferApprovalAfterReviewFailure()
	}
	if model.approvalMode == approvalYOLO {
		model.approval.ready = true
		model.status = "Applying action"
		model.refreshTranscript()
		return model, model.resolveApproval(true)
	}
	if errors.Is(message.err, context.Canceled) {
		model.approval = nil
		model.status = "Finalizing cancellation"
		model.refreshTranscript()
		return model, cancelRunGeneration(model.runner, model.threadID, model.operationGeneration)
	}
	model.autoClassifierReset = false
	if message.err != nil {
		if !strings.Contains(message.err.Error(), "human fallback threshold") {
			return model.rejectAutomaticApproval("Automatic review failed; action denied: " + message.err.Error())
		}
		model.approval.autoFallback = true
		model.approval.ready = true
		model.appendItem(transcriptItem{kind: itemNotice, text: "Automatic review unavailable; a user decision is required. " + message.err.Error()})
		model.status = "Review action"
		model.refreshTranscript()
		return model, model.deferApprovalAfterReviewFailure()
	}
	decisions := make(map[string]dagent.ApprovalChoice, len(model.approval.requests))
	allowed := make(map[string]struct{}, len(model.approval.requests))
	for _, request := range model.approval.requests {
		assessment, ok := message.result.Assessments[request.Call.ID]
		if !ok {
			return model.rejectAutomaticApproval("Automatic review omitted " + request.Call.Name + "; action denied.")
		}
		decision := dagent.ApprovalApprove
		if !assessment.approved() {
			decision = dagent.ApprovalReject
		} else {
			allowed[request.Call.ID] = struct{}{}
		}
		decisions[request.Call.ID] = dagent.ApprovalChoice{
			Decision: decision, Reason: assessment.Rationale, Message: assessment.Rationale,
		}
		if !assessment.approved() {
			model.markToolRejected(request.Call.ID, assessment.Rationale)
		}
		if !assessment.approved() {
			model.appendItem(transcriptItem{kind: itemNotice, text: fmt.Sprintf(
				"Automatic review denied %s (risk: %s, authorization: %s): %s",
				request.Call.Name, assessment.RiskLevel, assessment.UserAuthorization, assessment.Rationale,
			)})
		}
	}
	model.autoClassifierPendingResults = allowed
	return model, model.resumeApproval(decisions)
}

func (model *tuiModel) rejectAutomaticApproval(reason string) (tea.Model, tea.Cmd) {
	decisions := make(map[string]dagent.ApprovalChoice, len(model.approval.requests))
	for _, request := range model.approval.requests {
		model.markToolRejected(request.Call.ID, reason)
		decisions[request.Call.ID] = dagent.ApprovalChoice{
			Decision: dagent.ApprovalReject,
			Reason:   reason,
			Message:  reason,
		}
	}
	model.autoClassifierPendingResults = nil
	model.appendItem(transcriptItem{kind: itemError, text: reason})
	return model, model.resumeApproval(decisions)
}

func (model *tuiModel) manualApprovalVisible() bool {
	return model.approval != nil && (model.effectiveApprovalMode() == approvalManual || model.approval.autoFallback)
}

func autoClassifierHasSuccessfulResult(messages []damessage.Message, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, message := range messages {
		if message.Role != damessage.RoleTool || message.ToolStatus == damessage.ToolStatusError {
			continue
		}
		if _, ok := allowed[message.ToolCallID]; ok {
			return true
		}
	}
	return false
}

func (model *tuiModel) resolveApproval(approve bool) tea.Cmd {
	return model.resolveApprovalWithReason(approve, "")
}

func (model *tuiModel) resolveApprovalWithReason(approve bool, reason string) tea.Cmd {
	if approve && model.approval != nil && model.approval.autoFallback {
		model.autoClassifierReset = true
	}
	reason = sanitizeApprovalRejectReason(reason)
	decisions := make(map[string]dagent.ApprovalChoice, len(model.approval.requests))
	names := make([]string, 0, len(model.approval.requests))
	for _, request := range model.approval.requests {
		choice := dagent.ApprovalChoice{Decision: dagent.ApprovalApprove}
		if !approve {
			choice.Decision = dagent.ApprovalReject
			if reason == "" {
				choice.Reason = "Rejected by user."
			} else {
				choice.Reason = reason
				choice.Message = frameApprovalRejectReason(reason)
			}
		}
		decisions[request.Call.ID] = choice
		if !approve {
			model.markToolRejected(request.Call.ID, choice.Reason)
		}
		names = append(names, request.Call.Name)
	}
	sort.Strings(names)
	verb := "Approved"
	if !approve {
		verb = "Rejected"
	}
	notice := verb + ": " + strings.Join(names, ", ")
	if !approve && reason != "" {
		notice += "\nReason: " + reason
	}
	model.appendItem(transcriptItem{kind: itemNotice, text: notice})
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
		switch {
		case item.kind == itemUser:
			fmt.Fprintf(&transcript, "[user, trusted]\n%s\n\n", redactAutoClassifierText(item.text, nil))
		case item.kind == itemTool && isAskUserTool(item.name) && item.done:
			summary := askUserAnsweredSummary
			if item.failed {
				summary = askUserFailedSummary
			}
			fmt.Fprintf(&transcript, "[tool summary, untrusted]\nresult: %s\n\n", summary)
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
	if item.timestamp.IsZero() && !item.restored {
		item.timestamp = time.Now()
	}
	model.items = append(model.items, item)
	if !model.ready || model.chatScroll.FollowBottom {
		model.transcriptStart = max(len(model.items)-transcriptVirtualWindow, 0)
	} else {
		model.transcriptStart = transcriptVirtualStart(len(model.items), model.transcriptStart)
	}
}

func (model *tuiModel) markToolRejected(callID, reason string) {
	index, ok := model.toolItems[callID]
	if !ok || index < 0 || index >= len(model.items) {
		return
	}
	item := &model.items[index]
	item.lifecycle = toolRejected
	item.done = true
	item.failed = false
	if strings.TrimSpace(reason) != "" {
		item.text = reason
	}
}

func (model *tuiModel) markLatestUserCancelled() {
	for index := len(model.items) - 1; index >= 0; index-- {
		if model.items[index].kind == itemUser {
			model.items[index].cancelled = true
			return
		}
	}
}

func (model *tuiModel) configureDisplaySettings(path string) error {
	if model.displaySaveMu == nil {
		model.displaySaveMu = &sync.Mutex{}
	}
	model.displayGeneration++
	model.displaySaving = false
	model.displayActiveGeneration = 0
	model.displayDirty = false
	model.displaySettings = path
	settings, err := loadDisplaySettings(path)
	if err != nil {
		return err
	}
	model.showTimestamps = settings.ShowMessageTimestamps
	model.showScrollbar = settings.ShowScrollbar
	model.showLineNumbers = settings.ShowDiffLineNumbers
	model.threadSelectorPreferences = threadSelectorPreferences{
		RelativeTime: settings.ThreadRelativeTime,
		Agent:        settings.ThreadAgent,
		AllAgents:    settings.ThreadAllAgents,
	}
	return nil
}

func (model *tuiModel) configureApprovalNotices(path, reviewer string) error {
	return model.configureApprovalState(path, reviewer, false)
}

func (model *tuiModel) configureApprovalState(path, reviewer string, restore bool) error {
	model.autoModeNoticeConfigured = true
	model.autoModeNoticePath = path
	model.autoModeReviewer = reviewer
	model.approvalModeStore = newApprovalModeStore(path)
	yoloAcknowledged, yoloErr := hasYoloAcknowledgement(path)
	model.autoModeNoticeAcknowledged = true
	model.yoloModeAcknowledged = yoloAcknowledged
	var modeErr error
	if restore {
		model.approvalMode, modeErr = model.approvalModeStore.Load(model.threadID)
		if modeErr != nil {
			// A corrupt explicit preference is different from an absent
			// preference: fail closed without reintroducing the startup notice.
			model.approvalMode = approvalManual
		}
	}
	model.autoModeNotice = false
	if model.approvalMode == approvalYOLO && !yoloAcknowledged {
		model.approvalMode = approvalManual
		model.yoloModeNotice = true
	}
	var saveErr error
	if !restore || model.yoloModeNotice {
		saveErr = model.approvalModeStore.Save(model.threadID, model.approvalMode)
		if saveErr != nil && model.yoloModeNotice {
			model.approvalModeBlocked = true
		}
	}
	if saveErr != nil && model.approvalMode != approvalManual {
		model.approvalMode = approvalManual
		model.autoModeNotice = false
	}
	return errors.Join(yoloErr, modeErr, saveErr)
}

func (model *tuiModel) restoreApprovalMode(threadID string) error {
	model.approvalMode = approvalAuto
	model.approvalModeBlocked = false
	model.autoModeNotice = false
	model.yoloModeNotice = false
	if model.approvalModeStore == nil {
		return nil
	}
	mode, err := model.approvalModeStore.Load(threadID)
	if err != nil {
		// Corrupt explicit state fails closed, while a missing thread record is
		// returned by the store as the useful Auto default.
		model.approvalMode = approvalManual
		return err
	}
	model.approvalMode = mode
	model.autoModeNotice = false
	if mode == approvalYOLO && !model.yoloModeAcknowledged {
		model.approvalMode = approvalManual
		model.yoloModeNotice = true
		if err := model.approvalModeStore.Save(threadID, approvalManual); err != nil {
			model.approvalModeBlocked = true
			return err
		}
	}
	return nil
}

func (model *tuiModel) startNewApprovalThread(threadID string) error {
	model.approvalMode = approvalAuto
	model.approvalModeBlocked = false
	model.autoModeNotice = false
	model.yoloModeNotice = false
	if model.approvalModeStore == nil {
		return nil
	}
	return model.approvalModeStore.Save(threadID, approvalAuto)
}

func (model *tuiModel) startAfterApprovalNotice() tea.Cmd {
	if !model.approvalNoticeDeferred {
		return nil
	}
	model.approvalNoticeDeferred = false
	return model.initialSessionCommand()
}

func (model *tuiModel) initialSessionCommand() tea.Cmd {
	if model.sessionPicker != nil && model.sessionPicker.loading {
		if model.sessionPicker.requestID == 0 {
			model.sessionPicker.requestID = nextSessionPickerRequestID.Add(1)
		}
		return listSessionsForPicker(model.ctx, model.runner, model.sessionPicker.requestID)
	}
	if model.sessionPicker != nil && model.sessionPicker.resuming && len(model.sessionPicker.sessions) > 0 {
		options := model.resumeOptions
		options.AbortMode = cwdResumeAbortLaunch
		return prepareSessionGeneration(model.ctx, model.runner, model.sessionPicker.sessions[0].ThreadID, options, model.operationGeneration)
	}
	if model.sessionPicker != nil {
		return nil
	}
	if strings.TrimSpace(model.initial) != "" {
		return func() tea.Msg { return initialPromptMsg(model.initial) }
	}
	if strings.TrimSpace(model.initialGoal) != "" {
		return func() tea.Msg { return initialGoalMsg(model.initialGoal) }
	}
	return loadGoalGeneration(model.ctx, model.runner, model.threadID, model.operationGeneration)
}

func (model *tuiModel) persistDisplaySettings() tea.Cmd {
	model.displayDirty = true
	return model.startDisplaySettingsSave()
}

func (model *tuiModel) startDisplaySettingsSave() tea.Cmd {
	if model.displaySettings == "" || model.displaySaving || !model.displayDirty {
		return nil
	}
	model.displaySaving = true
	model.displayDirty = false
	model.displayGeneration++
	if model.displayGeneration == 0 {
		model.displayGeneration = 1
	}
	generation := model.displayGeneration
	model.displayActiveGeneration = generation
	path := model.displaySettings
	settings := displaySettings{
		ShowMessageTimestamps: model.showTimestamps,
		ShowScrollbar:         model.showScrollbar,
		ShowDiffLineNumbers:   model.showLineNumbers,
		ThreadRelativeTime:    model.threadSelectorPreferences.RelativeTime,
		ThreadAgent:           model.threadSelectorPreferences.Agent,
		ThreadAllAgents:       model.threadSelectorPreferences.AllAgents,
	}
	return func() tea.Msg {
		model.displaySaveMu.Lock()
		err := saveDisplaySettings(path, settings)
		model.displaySaveMu.Unlock()
		return displaySettingsSavedMsg{generation: generation, err: err}
	}
}

func (model *tuiModel) flushDisplaySettings() error {
	if model == nil || model.displaySettings == "" {
		return nil
	}
	settings := displaySettings{
		ShowMessageTimestamps: model.showTimestamps,
		ShowScrollbar:         model.showScrollbar,
		ShowDiffLineNumbers:   model.showLineNumbers,
		ThreadRelativeTime:    model.threadSelectorPreferences.RelativeTime,
		ThreadAgent:           model.threadSelectorPreferences.Agent,
		ThreadAllAgents:       model.threadSelectorPreferences.AllAgents,
	}
	if model.displaySaveMu == nil {
		model.displaySaveMu = &sync.Mutex{}
	}
	model.displaySaveMu.Lock()
	defer model.displaySaveMu.Unlock()
	return saveDisplaySettings(model.displaySettings, settings)
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
	if model.approval != nil {
		model.approval.reason.SetWidth(max(model.width-10, 12))
	}
	if model.goalReview != nil {
		model.goalReview.resize(max(model.width-4, 16))
		promptHeight := lipgloss.Height(renderGoalReviewWithGlyphs(model.goalReview, max(model.width-4, 16), model.externalEditorName, model.glyphs))
		viewportHeight := max(model.height-promptHeight-1, 3)
		viewportWidth := model.width
		if model.scrollbarVisible() {
			viewportWidth--
		}
		if !model.ready {
			model.viewport = viewport.New(viewportWidth, viewportHeight)
			model.ready = true
		} else {
			model.viewport.Width = viewportWidth
			model.viewport.Height = viewportHeight
		}
		return
	}
	if model.askUser != nil {
		model.askUser.resize(max(model.width-4, 16))
		promptHeight := lipgloss.Height(model.renderAskUser())
		viewportHeight := max(model.height-promptHeight-1, 3)
		viewportWidth := model.width
		if model.showScrollbar {
			viewportWidth--
		}
		if !model.ready {
			model.viewport = viewport.New(viewportWidth, viewportHeight)
			model.ready = true
		} else {
			model.viewport.Width = viewportWidth
			model.viewport.Height = viewportHeight
		}
		return
	}
	composerWidth := max(model.width-4, 10)
	model.composer.SetWidth(composerWidth)
	model.composer.SetHeight(composerContentHeight(model.composer.Value(), max(composerWidth-2, 1)))
	goalHeight := 0
	if panel := model.renderGoalPanel(); panel != "" {
		goalHeight = lipgloss.Height(panel) + 1
	}
	subagentHeight := 0
	if model.subagentPanel != nil {
		panel := renderSubagentPanel(model.subagentPanel.snapshot(), max(model.width-2, 1), model.glyphs)
		if panel != "" {
			subagentHeight = lipgloss.Height(panel) + 1
		}
	}
	tipHeight := 0
	if model.renderStartupTip() != "" {
		tipHeight = 1
	}
	fixedHeight := model.composer.Height() + 4 + model.inputAuxiliaryHeight() + goalHeight + subagentHeight + tipHeight
	maximumToastHeight := max(model.height-fixedHeight-3, 0)
	toast := renderToastsWithin(model.toasts, model.width, maximumToastHeight, model.glyphs, time.Now())
	model.toastHeight = 0
	if toast != "" {
		model.toastHeight = lipgloss.Height(toast)
	}
	viewportHeight := max(model.height-fixedHeight-model.toastHeight, 3)
	viewportWidth := model.width
	if model.scrollbarVisible() {
		viewportWidth--
	}
	if !model.ready {
		model.viewport = viewport.New(viewportWidth, viewportHeight)
		model.ready = true
	} else {
		model.viewport.Width = viewportWidth
		model.viewport.Height = viewportHeight
	}
	model.chatScroll.updateLayout(model.viewport.TotalLineCount(), model.viewport.Height)
	model.viewport.SetYOffset(model.chatScroll.Offset)
}

func (model *tuiModel) inputAuxiliaryHeight() int {
	height := 0
	if count := min(len(model.inputCompletion.items), 10); count > 0 {
		height += count
	}
	return height
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
	model.refreshTranscriptWithAnchor(false)
}

func (model *tuiModel) refreshSpinner() {
	if !model.ready || !statusSpinnerActive(model.projectStatusBarState()) {
		return
	}
	model.refreshTranscriptWithAnchor(false)
}

func (model *tuiModel) setLastUsage(usage damessage.Usage) {
	if usage.TotalTokens <= 0 {
		usage.TotalTokens = max(usage.InputTokens+usage.OutputTokens, 0)
	}
	model.lastUsage = usage
	model.totalTokens = usage.TotalTokens
}

func (model *tuiModel) restoreUsage(messages []damessage.Message) {
	costGeneration := model.costStats.generation
	model.costStats = sessionCostState{generation: costGeneration}
	model.threadUsage = damessage.AggregateUsage(messages)
	model.usageRequests = usageRequestCount(messages)
	model.lastUsage = damessage.Usage{}
	model.totalTokens = 0
	if usage, ok := damessage.LastUsage(messages); ok {
		model.setLastUsage(usage)
	}
}

func (model *tuiModel) resetUsage() {
	model.lastUsage = damessage.Usage{}
	model.threadUsage = damessage.Usage{}
	model.usageRequests = 0
	model.totalTokens = 0
	model.costStats = sessionCostState{}
}

func usageRequestCount(messages []damessage.Message) int {
	count := 0
	for _, message := range messages {
		if message.Usage != nil {
			count++
		}
		count += len(message.OtherUsage)
	}
	return count
}

func (model *tuiModel) tokenUsageSummary() string {
	modelLabel := displayModelName(model.modelName)
	if model.totalTokens <= 0 {
		parts := []string{"No token usage yet"}
		if model.contextWindow > 0 {
			parts = append(parts, formatTokenCount(model.contextWindow)+" token context window")
		}
		parts = append(parts, modelLabel)
		return strings.Join(parts, " · ")
	}
	usage := fmt.Sprintf("%s tokens used", formatTokenCount(model.totalTokens))
	if model.contextWindow > 0 {
		percentage := float64(model.totalTokens) / float64(model.contextWindow) * 100
		usage = fmt.Sprintf("%s / %s tokens (%.1f%%)", formatTokenCount(model.totalTokens), formatTokenCount(model.contextWindow), percentage)
	}
	lines := []string{usage + " · " + modelLabel}
	if model.lastUsage.InputTokens > 0 || model.lastUsage.OutputTokens > 0 {
		lines = append(lines,
			fmt.Sprintf("├ Input: %s", formatTokenCount(model.lastUsage.InputTokens)),
			fmt.Sprintf("└ Output: %s", formatTokenCount(model.lastUsage.OutputTokens)),
		)
	}
	if model.costStats.loaded && model.costStats.report.RequestCount > 0 {
		lines = append(lines, fmt.Sprintf("Session: %s input · %s output",
			formatTokenCount64(model.costStats.report.InputTokens), formatTokenCount64(model.costStats.report.OutputTokens)))
		if model.costStats.report.CacheReadTokens > 0 || model.costStats.report.CacheWriteTokens > 0 {
			lines = append(lines, fmt.Sprintf("Cache: %s read · %s written",
				formatTokenCount64(model.costStats.report.CacheReadTokens), formatTokenCount64(model.costStats.report.CacheWriteTokens)))
		}
	}
	return strings.Join(lines, "\n")
}

func (model *tuiModel) costSummary() string {
	if model.costStats.loaded {
		return formatSessionCostReport(model.costStats.report, model.costStats.pricingUnavailable)
	}
	if model.usageRequests == 0 {
		return "No model usage recorded for this thread yet."
	}
	if model.threadUsage.CostUSD <= 0 {
		requestLabel := "requests have"
		if model.usageRequests == 1 {
			requestLabel = "request has"
		}
		return fmt.Sprintf("Cost estimate unavailable\n\n%d recorded %s token usage, but no cost was reported.", model.usageRequests, requestLabel)
	}
	requestLabel := "requests"
	if model.usageRequests == 1 {
		requestLabel = "request"
	}
	return fmt.Sprintf("Estimated thread cost: %s\n\n%d recorded %s · %s input · %s output tokens",
		formatCost(model.threadUsage.CostUSD), model.usageRequests, requestLabel,
		formatTokenCount(model.threadUsage.InputTokens), formatTokenCount(model.threadUsage.OutputTokens))
}

func formatTokenCount(count int) string {
	if count < 1_000 {
		return strconv.Itoa(max(count, 0))
	}
	value := float64(count) / 1_000
	if count%1_000 == 0 {
		return fmt.Sprintf("%.0fk", value)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", value), "0"), ".") + "k"
}

func displayModelName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, ":") {
		return value
	}
	return "openai:" + value
}

func formatCost(cost float64) string {
	switch {
	case cost >= 1:
		return fmt.Sprintf("$%.2f", cost)
	case cost >= 0.0001:
		return fmt.Sprintf("$%.4f", cost)
	default:
		return fmt.Sprintf("$%.6f", cost)
	}
}

func (model *tuiModel) View() string {
	model.welcomeScreenHitTargets = model.welcomeScreenHitTargets[:0]
	if !model.ready {
		return "Starting dacode…" + model.themeSequence
	}
	if model.debugConsole != nil {
		panel := renderDebugConsoleOverlay(model.debugConsole.snapshotView(), model.width, model.height, model.glyphs)
		return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel) + model.terminalSequences() + model.themeSequence
	}
	if model.workflowPanel != nil {
		return model.renderModalWithToasts(model.renderWorkflowPanel())
	}
	if model.yoloModeNotice {
		return model.renderModalWithToasts(model.renderYoloModeNotice())
	}
	if model.pluginManager != nil {
		return model.renderModalWithToasts(renderPluginManagerWithGlyphs(model.pluginManager, model.width, model.height, model.glyphs))
	}
	if model.pluginReloadPrompt {
		return model.renderModalWithToasts(renderPluginReloadPromptWithGlyphs(model.width, model.height, model.glyphs))
	}
	if model.pluginReloading {
		return model.renderModalWithToasts(renderPluginReloadingWithGlyphs(model.width, model.height, model.glyphs))
	}
	if model.onboarding != nil {
		return model.renderModalWithToasts(renderOnboarding(model.onboarding, model.width, model.height, model.glyphs))
	}
	if model.updateModal != nil {
		return model.renderModalWithToasts(renderUpdateModal(model.updateModal, model.width, model.height, model.glyphs))
	}
	if model.authManager != nil && model.authManager.open {
		return model.renderModalWithToasts(model.renderAuthManager())
	}
	if model.mcpLogin != nil {
		return model.renderModalWithToasts(model.renderMCPLogin())
	}
	if model.mcpReconnectPrompt != nil {
		return model.renderModalWithToasts(model.mcpReconnectPrompt.render(model.width, model.height))
	}
	if model.mcpErrorDetail != "" {
		return model.renderModalWithToasts(renderMCPViewerError(model.mcpErrorServer, model.mcpErrorDetail, model.width, model.height, model.charset == charsetASCII))
	}
	if model.notificationCenter != nil {
		return model.renderModalWithToasts(model.notificationCenter.render(model.width, model.height, model.glyphs))
	}
	if model.notificationSettings != nil {
		return model.renderModalWithToasts(model.notificationSettings.render(model.width, model.height, model.glyphs))
	}
	if model.modelSelector != nil {
		return model.renderModalWithToasts(renderModelSelector(model.modelSelector, model.width, model.height, model.glyphs))
	}
	if model.installSelector != nil {
		return model.renderModalWithToasts(renderInstallSelector(model.installSelector, model.width, model.height, model.glyphs))
	}
	if model.restartPrompt != nil {
		return model.renderModalWithToasts(renderRestartPrompt(model.restartPrompt, model.width, model.height, model.glyphs))
	}
	if model.restarting {
		return renderLifecycleProgress("Restarting local agent server"+model.glyphs.Ellipsis, model.width, model.height, model.glyphs) + model.themeSequence
	}
	if model.mcpViewer != nil {
		return model.renderModalWithToasts(model.mcpViewer.render(model.width, model.height))
	}
	if model.themePicker != nil {
		return model.renderModalWithToasts(model.renderThemePicker())
	}
	if model.sessionPicker != nil {
		if model.resumeController != nil {
			return model.renderModalWithToasts(renderSessionResumePrompt(model.resumeController.Prompt(), model.width, model.height, model.glyphs))
		}
		return model.renderModalWithToasts(model.renderSessionPicker())
	}
	if model.effortPicker != nil {
		return model.renderModalWithToasts(model.renderEffortPicker())
	}
	if model.agentPicker != nil {
		return model.renderModalWithToasts(model.renderAgentPicker())
	}
	if model.skillTrust != nil {
		return model.renderModalWithToasts(model.renderSkillTrust())
	}
	if model.contextScreen {
		return model.renderModalWithToasts(model.renderContextUsage())
	}
	if model.goalReview != nil {
		terminalSequences := model.terminalSequences()
		return lipgloss.NewStyle().Foreground(colorBody).Render(
			model.renderViewport()+"\n"+renderGoalReviewWithGlyphs(model.goalReview, max(model.width-4, 16), model.externalEditorName, model.glyphs)+"\n"+model.renderStatus(),
		) + terminalSequences + model.themeSequence
	}
	if model.askUser != nil {
		terminalSequences := model.terminalSequences()
		return lipgloss.NewStyle().Foreground(colorBody).Render(
			model.renderViewport()+"\n"+model.renderAskUser()+"\n"+model.renderStatus(),
		) + terminalSequences + model.themeSequence
	}
	composer := model.composer
	if model.running {
		composer.Placeholder = "Agent is working…"
	}
	inputContent := composer.View()
	if len(model.inputCompletion.items) > 0 {
		for index, item := range model.inputCompletion.items[:min(len(model.inputCompletion.items), 10)] {
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(colorMuted)
			if index == model.inputCompletion.selected {
				prefix = model.glyphs.Cursor + " "
				style = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
			}
			label := item
			if model.inputCompletion.kind == completionSlash {
				label = slashCompletionLabelWithGlyphs(item, model.glyphs)
			}
			inputContent += "\n" + style.Render(prefix+unicodesecurity.RenderTerminalSafe(label))
		}
	}
	input := lipgloss.NewStyle().
		Border(model.uiBorder()).BorderForeground(colorPrimary).
		Padding(0, 1).Width(max(model.width-4, 1)).
		Render(inputContent)
	terminalSequences := model.terminalSequences()
	goalPanel := model.renderGoalPanel()
	if goalPanel != "" {
		goalPanel += "\n"
	}
	panel := ""
	if model.subagentPanel != nil {
		panel = renderSubagentPanel(model.subagentPanel.snapshot(), max(model.width-2, 1), model.glyphs)
		if panel != "" {
			panel += "\n"
		}
	}
	tip := model.renderStartupTip()
	if tip != "" {
		tip += "\n"
	}
	toast := renderToastsWithin(model.toasts, model.width, model.toastHeight, model.glyphs, time.Now())
	if toast != "" {
		toast += "\n"
	}
	body := lipgloss.NewStyle().Foreground(colorBody).Render(
		model.renderViewport() + "\n" + toast + panel + goalPanel + tip + input + "\n" + model.renderStatus(),
	)
	model.cacheWelcomeScreenTargets(body)
	return body + terminalSequences + model.themeSequence
}

func (model *tuiModel) uiBorder() lipgloss.Border {
	if model.glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		return lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	return lipgloss.RoundedBorder()
}

func (model *tuiModel) renderGoalPanel() string {
	if model.goal == nil {
		return ""
	}
	status := strings.ReplaceAll(string(model.goal.Status), "_", " ")
	header := lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).Render("Goal " + model.glyphs.Bullet + " " + status)
	objective := unicodesecurity.RenderTerminalSafe(model.goal.Objective)
	return lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorSuccess).
		Padding(0, 1).Width(max(model.width-2, 1)).Render(header + "\n" + objective)
}

func (model *tuiModel) renderYoloModeNotice() string {
	contentWidth := min(max(model.width-16, 36), 72)
	body := "You are about to enable YOLO mode. The agent may run shell commands, edit files, make network calls, and use other tools on this machine without asking you first.\n\n" +
		"Only continue if you are comfortable letting it act unsupervised. Leave YOLO at any time with Shift+Tab.\n\n" +
		"This policy-versioned notice appears once on this machine."
	hint := "Enter to enable YOLO  •  m for Manual  •  Esc to keep current mode"
	if model.yoloModeNoticeSaving {
		hint = "Saving acknowledgement…"
	}
	lines := []string{
		lipgloss.NewStyle().Foreground(colorError).Bold(true).Align(lipgloss.Center).Width(contentWidth).Render("YOLO mode"),
		"",
		lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth).Render(body),
		"",
		lipgloss.NewStyle().Foreground(colorMuted).Italic(true).Align(lipgloss.Center).Width(contentWidth).Render(hint),
	}
	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorError).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
}

func (model *tuiModel) renderViewport() string {
	content := lipgloss.NewStyle().Height(max(model.viewport.Height, 1)).Render(model.viewport.View())
	if !model.scrollbarVisible() || model.viewport.TotalLineCount() <= model.viewport.Height {
		return content
	}
	height := max(model.viewport.Height, 1)
	thumbStart, thumbHeight := model.chatScroll.thumb(height)
	track := make([]string, height)
	for row := range height {
		glyph := "░"
		style := lipgloss.NewStyle().Foreground(colorPanel)
		if row >= thumbStart && row < thumbStart+thumbHeight {
			glyph = "█"
			style = lipgloss.NewStyle().Foreground(colorPrimary)
		}
		track[row] = style.Render(glyph)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, content, strings.Join(track, "\n"))
}

func (model *tuiModel) renderContextUsage() string {
	contentWidth := min(max(model.width-12, 36), 88)
	modelLabel := displayModelName(model.modelName)
	usage := max(model.totalTokens, 0)
	maximum := "unavailable"
	if model.contextWindow > 0 {
		maximum = formatTokenCount(model.contextWindow)
	}
	right := formatTokenCount(usage) + " / " + maximum
	if model.contextWindow > 0 {
		right += fmt.Sprintf("  %.1f%%", float64(usage)/float64(model.contextWindow)*100)
	}
	left := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Context") +
		lipgloss.NewStyle().Foreground(colorMuted).Render(" • "+modelLabel)
	header := left + strings.Repeat(" ", max(contentWidth-lipgloss.Width(left)-lipgloss.Width(right), 1)) +
		lipgloss.NewStyle().Foreground(colorMuted).Render(right)

	lines := []string{header}
	if usage <= 0 {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render("No context usage reported yet."))
	} else {
		scale := max(model.contextWindow, usage)
		barWidth := min(contentWidth, 60)
		usedWidth := min(max(int(math.Round(float64(usage)/float64(scale)*float64(barWidth))), 1), barWidth)
		bar := lipgloss.NewStyle().Foreground(colorPrimary).Render(strings.Repeat("━", usedWidth)) +
			lipgloss.NewStyle().Foreground(colorPanel).Render(strings.Repeat("━", barWidth-usedWidth))
		usedPercentage := float64(usage) / float64(scale) * 100
		lines = append(lines, bar,
			fmt.Sprintf("━━ Used context%s%s  •  %.1f%%",
				strings.Repeat(" ", max(contentWidth-len("━━ Used context")-len(formatTokenCount(usage))-11, 1)),
				formatTokenCount(usage), usedPercentage),
		)
		if model.contextWindow > 0 {
			free := max(model.contextWindow-usage, 0)
			lines = append(lines, fmt.Sprintf("━━ Free space%s%s  •  %.1f%%",
				strings.Repeat(" ", max(contentWidth-len("━━ Free space")-len(formatTokenCount(free))-11, 1)),
				formatTokenCount(free), float64(free)/float64(scale)*100))
		}
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render("Esc to close"))
	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
}

func (model *tuiModel) renderTranscript() string {
	width := max(model.viewport.Width-4, 20)
	sections := make([]string, 0, len(model.items)+4)
	layout := make([]transcriptBlockLayout, 0, len(model.items)+4)
	line := 0
	appendSection := func(id transcriptBlockID, rendered string) {
		if len(sections) > 0 {
			line++ // strings.Join inserts one empty line between sections.
		}
		lines := strings.Count(rendered, "\n") + 1
		layout = append(layout, transcriptBlockLayout{id: id, start: line, lines: lines})
		sections = append(sections, rendered)
		line += lines
	}
	appendSection(transcriptBlockID{kind: transcriptBlockWelcome}, model.renderWelcome(width))
	start := transcriptVirtualStart(len(model.items), model.transcriptStart)
	model.transcriptStart = start
	if start > 0 {
		appendSection(transcriptBlockID{kind: transcriptBlockVirtualized}, lipgloss.NewStyle().Foreground(colorMuted).Italic(true).PaddingLeft(1).Render(
			fmt.Sprintf("%s %d earlier transcript items virtualized %s PgUp to load", model.glyphs.Ellipsis, start, model.glyphs.Bullet)))
	}
	groups := transcriptToolGroups(model.items, start)
	for index := start; index < len(model.items); {
		if group, ok := groups[index]; ok {
			summary := summarizeTranscriptTools(model.items[group.start:group.end])
			expanded := model.toolGroupExpanded[group.key]
			mark := model.glyphs.DisclosureClosed
			if expanded {
				mark = model.glyphs.DisclosureExpanded
			}
			if strings.Contains(summary, "Running") || strings.Contains(summary, "running") {
				summary += model.glyphs.Ellipsis
			}
			appendSection(transcriptBlockID{kind: transcriptBlockToolGroup, index: group.start, key: group.key}, lipgloss.NewStyle().Foreground(colorMuted).PaddingLeft(1).Render(mark+" "+summary+" "+model.glyphs.Bullet+" Ctrl+O toggle"))
			if expanded {
				for member := group.start; member < group.end; member++ {
					appendSection(transcriptBlockID{kind: transcriptBlockItem, index: member}, model.renderTranscriptItem(model.items[member], width))
				}
			}
			index = group.end
			continue
		}
		appendSection(transcriptBlockID{kind: transcriptBlockItem, index: index}, model.renderTranscriptItem(model.items[index], width))
		index++
	}
	if model.running {
		appendSection(transcriptBlockID{kind: transcriptBlockRunning}, lipgloss.NewStyle().Foreground(colorMuted).PaddingLeft(1).Render(
			model.spinner.View()+" "+unicodesecurity.RenderTerminalSafe(model.status)+model.glyphs.Ellipsis))
	}
	if model.manualApprovalVisible() {
		appendSection(transcriptBlockID{kind: transcriptBlockApproval}, renderApproval(model.approval, width))
	}
	model.transcriptLayout = layout
	return strings.Join(sections, "\n\n")
}

func (model *tuiModel) renderTranscriptItem(item transcriptItem, width int) string {
	rendered := renderItemWithGlyphs(item, width, model.glyphs)
	if model.showTimestamps && !item.timestamp.IsZero() && item.kind != itemNotice && item.kind != itemError {
		rendered += "\n" + renderMessageTimestamp(item.timestamp, width)
	}
	return rendered
}

func renderMessageTimestamp(timestamp time.Time, width int) string {
	local := timestamp.Local()
	label := local.Format("15:04:05")
	if year, month, day := local.Date(); year != time.Now().Year() || month != time.Now().Month() || day != time.Now().Day() {
		label = local.Format("Jan 2, 15:04:05")
	}
	return lipgloss.NewStyle().Foreground(colorMuted).Align(lipgloss.Right).Width(max(width-1, 1)).Render(label)
}

func (model *tuiModel) renderWelcome(width int) string {
	state := model.projectWelcomeBannerState()
	layout := renderWelcomeBannerLayout(state, width, model.glyphs)
	model.welcomeHitTargets = append(model.welcomeHitTargets[:0], layout.HitTargets...)
	return layout.View
}

func renderItem(item transcriptItem, width int) string {
	return renderItemWithGlyphs(item, width, unicodeUIGlyphs)
}

func renderItemWithGlyphs(item transcriptItem, width int, glyphs uiGlyphs) string {
	contentWidth := max(width-4, 10)
	displayText := item.text
	if item.kind == itemTool && item.done && isAskUserTool(item.name) {
		displayText = askUserAuditResult(item)
	}
	text := unicodesecurity.RenderTerminalSafe(displayText)
	switch item.kind {
	case itemUser:
		text, _ = collapseUserTranscriptWithGlyphs(text, item.expanded, glyphs)
		if item.cancelled {
			text = lipgloss.NewStyle().Faint(true).Render(text + "\n(cancelled)")
		}
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorPrimary).
			PaddingLeft(1).Width(contentWidth).Render(lipgloss.NewStyle().Foreground(colorBody).Render("> ") + text)
	case itemAssistant:
		return lipgloss.NewStyle().Foreground(colorBody).PaddingLeft(1).Width(contentWidth).Render(renderAssistantMarkdownWithGlyphs(displayText, contentWidth, glyphs))
	case itemTool:
		icon := "○"
		color := colorWarning
		status := item.lifecycle
		if status == "" {
			status = toolPending
			if item.done {
				status = toolSuccess
			}
			if item.failed {
				status = toolError
			}
		}
		if status == toolSuccess {
			icon = "✓"
			color = colorSuccess
		}
		if status == toolError {
			icon = "✗"
			color = colorError
		}
		if status == toolRejected {
			icon = "!"
			color = colorWarning
		}
		if status == toolSkipped {
			icon = "✗"
			color = colorWarning
		}
		name := unicodesecurity.RenderTerminalSafe(item.name)
		arguments := unicodesecurity.RenderTerminalSafe(toolArgumentDisplayWithGlyphs(item, contentWidth-lipgloss.Width(name)-18, glyphs))
		label := lifecycleLabel(item, status, time.Now())
		header := lipgloss.NewStyle().Foreground(color).Bold(true).Render(icon+" "+name) + " " +
			lipgloss.NewStyle().Foreground(colorMuted).Render(label)
		if arguments != "" {
			header += "  " + lipgloss.NewStyle().Foreground(colorMuted).Render(arguments)
		}
		body := header
		if item.text != "" {
			body += "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render(renderToolOutputWithGlyphs(text, item.expanded, glyphs))
		}
		if diff, ok := inlineToolDiff(item.name, item.args); ok && status == toolSuccess {
			body += "\n" + renderInlineDiffWithGlyphs(diff, item.lineNums, contentWidth, glyphs)
		}
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(color).
			PaddingLeft(1).Width(contentWidth).Render(body)
	case itemSkill:
		header := lipgloss.NewStyle().Foreground(colorSecondary).Bold(true).Render("◆ Skill: " + unicodesecurity.RenderTerminalSafe(item.name))
		if item.source != "" {
			header += lipgloss.NewStyle().Foreground(colorMuted).Render(" [" + unicodesecurity.RenderTerminalSafe(item.source) + "]")
		}
		body := header
		if item.detail != "" {
			body += "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render(unicodesecurity.RenderTerminalSafe(item.detail))
		}
		if item.request != "" {
			body += "\n" + lipgloss.NewStyle().Bold(true).Render("User request: ") + unicodesecurity.RenderTerminalSafe(item.request)
		}
		if strings.TrimSpace(item.text) != "" {
			body += "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render(renderToolOutputWithGlyphs(text, item.expanded, glyphs))
		}
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorSecondary).
			PaddingLeft(1).Width(contentWidth).Render(body)
	case itemError:
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(colorError).
			Foreground(colorError).PaddingLeft(1).Width(contentWidth).Render(text)
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).PaddingLeft(1).Width(contentWidth).Render(text)
	}
}

func renderApproval(state *approvalState, width int) string {
	if state.deferred {
		return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorWarning).
			Foreground(colorMuted).Padding(0, 1).Width(max(width, 1)).Render("Waiting for typing to finish...")
	}
	title := "Review requested"
	if len(state.requests) == 1 {
		title = ">>> " + unicodesecurity.RenderTerminalSafe(state.requests[0].Call.Name) + " Requires Approval <<<"
	} else if len(state.requests) > 1 {
		title = fmt.Sprintf(">>> %d Tool Calls Require Approval <<<", len(state.requests))
	}
	lines := []string{lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render(title)}
	lines = append(lines, renderApprovalDetails(state, width)...)
	warnings := approvalSecurityWarnings(state.requests)
	for _, warning := range warnings[:min(len(warnings), 3)] {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Bold(true).Render("Warning: ")+
			unicodesecurity.RenderTerminalSafe(warning))
	}
	if len(warnings) > 3 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Render(fmt.Sprintf("+%d more warnings", len(warnings)-3)))
	}
	hint := "Pausing…"
	if state.preparingReview || state.reviewing {
		hint = "Automatic review…"
	}
	if state.ready && state.reasonMode {
		lines = append(lines, "", state.reason.View())
		if state.reasonWarning != "" {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(state.reasonWarning))
		}
		hint = "Enter submit  •  Esc cancel  •  leave blank to reject without a reason"
	} else if state.ready {
		lines = append(lines, "")
		for index, option := range approvalOptions(state) {
			cursor := "  "
			style := lipgloss.NewStyle().Foreground(colorMuted)
			if index == state.selected {
				cursor = "› "
				style = lipgloss.NewStyle().Foreground(colorWarning).Bold(true)
			}
			lines = append(lines, style.Render(fmt.Sprintf("%s%d. %s", cursor, index+1, option.label)))
		}
		hint = "↑/↓ navigate  •  Enter select  •  y/a/n quick keys  •  Tab reject with feedback  •  Esc reject"
		for _, request := range state.requests {
			arguments := approvalArgumentMap(request.Call.Arguments)
			command, _ := arguments["command"].(string)
			if request.Call.Name == "execute" && approvalCommandExpandable(command) {
				hint += "  •  e expand"
				break
			}
		}
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(hint))
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorWarning).
		Padding(0, 1).Width(max(width, 1)).Render(strings.Join(lines, "\n"))
}

func approvalSecurityWarnings(requests []dagent.ApprovalRequest) []string {
	var warnings []string
	for _, request := range requests {
		var arguments any
		if json.Unmarshal(request.Call.Arguments, &arguments) != nil {
			continue
		}
		warnings = append(warnings, unicodesecurity.ScanArguments(request.Call.Name, arguments)...)
	}
	return warnings
}

func (model *tuiModel) renderStatus() string {
	return renderTwoLineStatusBar(model.projectStatusBarState(), max(model.width-1, 20), model.spinner.View(), model.glyphs)
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
