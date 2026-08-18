package dacode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const lifecycleOperationTimeout = 30 * time.Second

type sessionCompactionResult struct {
	Output       string
	CheckpointID string
	Failed       bool
}

type sessionCompactor interface {
	CompactSession(context.Context, string, string) (sessionCompactionResult, error)
}

type compactionFinishedMsg struct {
	result     sessionCompactionResult
	err        error
	generation uint64
}

func (model *tuiModel) configureLifecycle(stateDirectory string, restart restartController, resume sessionResumeOptions) {
	if model == nil {
		panic("dacode: TUI model is required")
	}
	if stateDirectory == "" || !filepath.IsAbs(stateDirectory) || strings.ContainsRune(stateDirectory, 0) {
		panic("dacode: absolute lifecycle state directory is required")
	}
	if _, err := normalizeResumeDirectory(resume.CurrentDirectory, false); err != nil {
		panic("dacode: valid lifecycle working directory is required")
	}
	if resume.CompactThreshold < 0 {
		panic("dacode: compact-on-resume threshold cannot be negative")
	}
	model.onboardingStateDirectory = filepath.Clean(stateDirectory)
	model.restartController = restart
	model.resumeOptions = resume
}

func (model *tuiModel) persistOnboarding(result onboardingResult) tea.Cmd {
	stateDirectory := model.onboardingStateDirectory
	agentName := model.agentName
	if agentName == "" {
		agentName = defaultAgentName
	}
	return func() tea.Msg {
		if stateDirectory == "" {
			return onboardingSavedMsg{result: result, err: errors.New("onboarding storage is unavailable")}
		}
		return onboardingSavedMsg{result: result, err: persistOnboardingResult(stateDirectory, agentName, result)}
	}
}

func restartLocalAgentServer(parent context.Context, controller restartController) tea.Cmd {
	return func() tea.Msg {
		if controller == nil {
			return restartFinishedMsg{err: errors.New("restart is unavailable")}
		}
		ctx, cancel := context.WithTimeout(parent, lifecycleOperationTimeout)
		defer cancel()
		return restartFinishedMsg{err: controller.Restart(ctx)}
	}
}

func (model *tuiModel) startCompaction() tea.Cmd {
	if model.running || model.shellRunning {
		return model.notify("Cannot compact while another operation is running.", toastWarning, "")
	}
	compactor, ok := model.runner.(sessionCompactor)
	if !ok {
		model.appendItem(transcriptItem{kind: itemError, text: "Conversation compaction is unavailable in this runtime."})
		model.refreshTranscript()
		return nil
	}
	model.appendItem(transcriptItem{kind: itemUser, text: "/offload"})
	model.running = true
	model.status = "Compacting conversation"
	model.applyCursorPreference(false)
	threadID := model.threadID
	checkpointID := model.compactionCheckpointID
	generation := model.operationGeneration
	model.compactionCheckpointID = ""
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(model.ctx, lifecycleOperationTimeout)
		defer cancel()
		result, err := compactor.CompactSession(ctx, threadID, checkpointID)
		return compactionFinishedMsg{result: result, err: err, generation: generation}
	}
}

func renderLifecycleProgress(message string, width, height int, glyphs uiGlyphs) string {
	contentWidth := min(max(width-12, 36), 64)
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).Padding(1, 2).Width(contentWidth + 4).
		Render(lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Align(lipgloss.Center).Width(contentWidth).
			Render(unicodesecurity.RenderTerminalSafe(message)))
	return lipgloss.Place(max(width, contentWidth+8), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

func renderSessionResumePrompt(prompt sessionResumePrompt, width, height int, glyphs uiGlyphs) string {
	switch {
	case prompt.Agent != nil:
		return renderAgentResumePrompt(prompt.Agent, width, height, glyphs)
	case prompt.CWD != nil:
		return renderCWDResumePrompt(prompt.CWD, width, height, glyphs)
	case prompt.Compact != nil:
		return renderCompactResumePrompt(prompt.Compact, width, height, glyphs)
	default:
		return renderLifecycleProgress("Preparing session"+glyphs.Ellipsis, width, height, glyphs)
	}
}

func boundedLifecycleError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(unicodesecurity.RenderTerminalSafe(err.Error()))
	characters := []rune(value)
	if len(characters) > 512 {
		value = string(characters[:509]) + "..."
	}
	return value
}
