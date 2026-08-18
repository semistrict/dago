package dacode

import (
	"context"
	"errors"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/daupdate"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type updateModalPhase uint8

const (
	updateModalChecking updateModalPhase = iota
	updateModalCurrent
	updateModalAvailable
	updateModalConfirm
	updateModalApplying
	updateModalComplete
	updateModalFailed
)

type updateModalAction uint8

const (
	updateModalNoAction updateModalAction = iota
	updateModalCancel
	updateModalApply
	updateModalRetry
)

type updateModalState struct {
	phase  updateModalPhase
	result daupdate.Result
	err    string
}

func newUpdateModal() *updateModalState { return &updateModalState{phase: updateModalChecking} }

func (state *updateModalState) finishCheck(result daupdate.Result, err error) {
	if state == nil {
		return
	}
	state.result = result
	state.err = safeUpdateModalError(err)
	if err != nil {
		state.phase = updateModalFailed
		return
	}
	switch result.Status {
	case daupdate.UpdateAvailable:
		state.phase = updateModalAvailable
	case daupdate.UpToDate, daupdate.CurrentNewer:
		state.phase = updateModalCurrent
	default:
		state.err = "The update service returned an invalid status."
		state.phase = updateModalFailed
	}
}

func (state *updateModalState) finishApply(result daupdate.Result, err error) {
	if state == nil {
		return
	}
	state.result = result
	state.err = safeUpdateModalError(err)
	if err != nil {
		state.phase = updateModalFailed
		return
	}
	if !result.Applied {
		state.err = "The update was not installed."
		state.phase = updateModalFailed
		return
	}
	state.phase = updateModalComplete
}

func (state *updateModalState) handleKey(key string) updateModalAction {
	if state == nil {
		return updateModalNoAction
	}
	switch state.phase {
	case updateModalChecking, updateModalApplying:
		if key == "esc" || key == "ctrl+c" {
			return updateModalCancel
		}
	case updateModalCurrent, updateModalComplete:
		if key == "enter" || key == "esc" || key == "ctrl+c" {
			return updateModalCancel
		}
	case updateModalAvailable:
		switch strings.ToLower(key) {
		case "enter", "y":
			state.phase = updateModalConfirm
		case "esc", "ctrl+c", "n":
			return updateModalCancel
		}
	case updateModalConfirm:
		switch strings.ToLower(key) {
		case "enter", "y":
			state.phase = updateModalApplying
			return updateModalApply
		case "esc", "ctrl+c", "n":
			state.phase = updateModalAvailable
		}
	case updateModalFailed:
		switch strings.ToLower(key) {
		case "enter", "r":
			state.phase = updateModalChecking
			state.err = ""
			return updateModalRetry
		case "esc", "ctrl+c", "q":
			return updateModalCancel
		}
	}
	return updateModalNoAction
}

func renderUpdateModal(state *updateModalState, width, height int, glyphs uiGlyphs) string {
	if state == nil {
		return ""
	}
	contentWidth := min(max(width-12, 40), 68)
	lines := []string{lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Software Update"), ""}
	switch state.phase {
	case updateModalChecking:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(glyphs.Hourglass+" Checking the signed release channel"+glyphs.Ellipsis), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Esc cancel"))
	case updateModalCurrent:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorSuccess).Render(glyphs.Checkmark+" You are up to date."),
			updateModalVersionLine(state.result, glyphs), "", lipgloss.NewStyle().Foreground(colorMuted).Render("Enter or Esc close"))
	case updateModalAvailable:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("Update available"),
			updateModalVersionLine(state.result, glyphs),
			lipgloss.NewStyle().Foreground(colorMuted).Render("The artifact signature and digest are verified before replacement."), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Enter update  "+glyphs.Bullet+"  Esc later"))
	case updateModalConfirm:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("Replace the current executable?"),
			updateModalVersionLine(state.result, glyphs),
			lipgloss.NewStyle().Foreground(colorMuted).Render("A restart is required after the update."), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Y/Enter confirm  "+glyphs.Bullet+"  N/Esc back"))
	case updateModalApplying:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(glyphs.Hourglass+" Downloading and verifying update"+glyphs.Ellipsis), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Esc cancel"))
	case updateModalComplete:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).Render(glyphs.Checkmark+" Update installed."),
			updateModalVersionLine(state.result, glyphs),
			lipgloss.NewStyle().Foreground(colorMuted).Render("Quit and restart to use the new release."), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Enter or Esc close"))
	case updateModalFailed:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Bold(true).Render(glyphs.Error+" Update failed"),
			lipgloss.NewStyle().Foreground(colorError).Render(unicodesecurity.RenderTerminalSafe(state.err)), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("R/Enter retry  "+glyphs.Bullet+"  Esc close"))
	}
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, contentWidth+8), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}

func updateModalVersionLine(result daupdate.Result, glyphs uiGlyphs) string {
	parts := make([]string, 0, 3)
	if result.CurrentVersion != "" {
		parts = append(parts, "current "+unicodesecurity.RenderTerminalSafe(result.CurrentVersion))
	}
	if result.LatestVersion != "" {
		parts = append(parts, "latest "+unicodesecurity.RenderTerminalSafe(result.LatestVersion))
	}
	if result.Channel != "" {
		parts = append(parts, "channel "+unicodesecurity.RenderTerminalSafe(result.Channel))
	}
	separator := "  " + glyphs.Bullet + "  "
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		separator = " " + glyphs.Bullet + " "
	}
	return lipgloss.NewStyle().Foreground(colorBody).Render(strings.Join(parts, separator))
}

func safeUpdateModalError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "The update was cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		return "The update timed out. Check your network connection and try again."
	case errors.Is(err, daupdate.ErrUntrustedManifest):
		return "The release manifest signature is not trusted."
	case errors.Is(err, daupdate.ErrInvalidManifest):
		return "The release manifest is invalid."
	case errors.Is(err, daupdate.ErrArtifactMismatch):
		return "The downloaded artifact failed verification."
	case errors.Is(err, daupdate.ErrAuthorization):
		return "Update installation was not authorized."
	case errors.Is(err, daupdate.ErrInvalidVersion):
		return "The current release version is invalid."
	case errors.Is(err, daupdate.ErrApplyFailed):
		return "The verified update could not replace the executable."
	case errors.Is(err, daupdate.ErrUpdateCheckFailed):
		return "The release channel could not be checked."
	default:
		return "The update operation failed."
	}
}
