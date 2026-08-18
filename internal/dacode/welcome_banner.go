package dacode

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type welcomeBannerState struct {
	Version              string
	Model                string
	WorkingDirectory     string
	ProjectLabel         string
	ProjectURL           string
	Agent                string
	ApprovalMode         string
	ThreadID             string
	MCPTools             int
	MCPLoginRequired     int
	MCPErrors            int
	MCPAwaitingReconnect int
	ShowVersion          bool
	ShowModel            bool
	ShowWorkingDirectory bool
	ShowProject          bool
	ShowThreadID         bool
	Ready                bool
	StartupError         string
}

const maxWelcomeBannerWidth = 4096

type welcomeHitTargetKind string

const (
	welcomeHitVersion welcomeHitTargetKind = "version"
	welcomeHitThread  welcomeHitTargetKind = "thread"
	welcomeHitMCP     welcomeHitTargetKind = "mcp"
	welcomeHitProject welcomeHitTargetKind = "project"
)

type welcomeHitTarget struct {
	Kind  welcomeHitTargetKind
	X     int
	Y     int
	Width int
	Label string
	Value string
}

type welcomeBannerLayout struct {
	View       string
	HitTargets []welcomeHitTarget
}

func renderWelcomeBanner(state welcomeBannerState, width int, glyphs uiGlyphs) string {
	return renderWelcomeBannerLayout(state, width, glyphs).View
}

func renderWelcomeBannerLayout(state welcomeBannerState, width int, glyphs uiGlyphs) welcomeBannerLayout {
	state = normalizeWelcomeBannerState(state)
	width = min(max(width, 16), maxWelcomeBannerWidth)
	contentWidth := max(width-4, 16)
	title := "dacode"
	if state.ShowVersion && strings.TrimSpace(state.Version) != "" {
		title += " " + strings.TrimSpace(state.Version)
	}
	lines := []string{lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render(unicodesecurity.RenderTerminalSafe(title))}
	targetRows := map[int]welcomeHitTarget{}
	if state.ShowVersion && state.Version != "" {
		targetRows[0] = welcomeHitTarget{Kind: welcomeHitVersion, Value: state.Version}
	}
	identity := make([]string, 0, 4)
	if state.Agent != "" && state.Agent != defaultAgentName {
		identity = append(identity, "agent:"+unicodesecurity.RenderTerminalSafe(state.Agent))
	}
	if state.ShowModel && state.Model != "" {
		identity = append(identity, unicodesecurity.RenderTerminalSafe(displayModelName(state.Model)))
	}
	if state.ApprovalMode != "" {
		identity = append(identity, unicodesecurity.RenderTerminalSafe(state.ApprovalMode))
	}
	if len(identity) > 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(strings.Join(identity, "  "+glyphs.Bullet+"  ")))
	}
	if state.ShowWorkingDirectory && state.WorkingDirectory != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Working directory: "+
			unicodesecurity.RenderTerminalSafe(shortPath(state.WorkingDirectory))))
	}
	if state.ShowProject && state.ProjectLabel != "" && state.ProjectURL != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Project: "+unicodesecurity.RenderTerminalSafe(state.ProjectLabel)))
		targetRows[len(lines)-1] = welcomeHitTarget{Kind: welcomeHitProject, Value: state.ProjectURL}
	}
	if state.ShowThreadID && state.ThreadID != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Thread ID: "+
			unicodesecurity.RenderTerminalSafe(state.ThreadID)))
		targetRows[len(lines)-1] = welcomeHitTarget{Kind: welcomeHitThread, Value: state.ThreadID}
	}
	if mcp := welcomeMCPStatus(state, glyphs); mcp != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(mcp))
		targetRows[len(lines)-1] = welcomeHitTarget{Kind: welcomeHitMCP, Value: "mcp"}
	}
	if state.StartupError != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Bold(true).Render(glyphs.Error+" Startup failed"),
			lipgloss.NewStyle().Foreground(colorError).Render(unicodesecurity.RenderTerminalSafe(state.StartupError)))
	} else if state.Ready {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorBody).Render("Ready to code. What would you like to build?"))
	} else {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render(glyphs.Hourglass+" Starting agent"+glyphs.Ellipsis))
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Enter send  "+glyphs.Bullet+"  "+glyphs.Newline+" newline  "+glyphs.Bullet+"  / commands"))
	for index, line := range lines {
		lines[index] = truncateDisplayLine(line, contentWidth, glyphs.Ellipsis)
	}
	targets := make([]welcomeHitTarget, 0, len(targetRows))
	for row := range lines {
		target, exists := targetRows[row]
		if !exists {
			continue
		}
		target.X, target.Y = 3, row+1
		target.Label = safeStatusText(ansi.Strip(lines[row]), contentWidth)
		target.Width = min(ansi.StringWidth(target.Label), contentWidth)
		if target.Width > 0 {
			targets = append(targets, target)
		}
	}
	view := lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorPrimary).
		Padding(0, 2).Width(max(width-6, 1)).Render(strings.Join(lines, "\n"))
	return welcomeBannerLayout{View: view, HitTargets: targets}
}

func normalizeWelcomeBannerState(state welcomeBannerState) welcomeBannerState {
	state.Version = boundedWelcomeText(state.Version, 128)
	state.Model = boundedWelcomeText(state.Model, 1024)
	state.WorkingDirectory = boundedWelcomeText(state.WorkingDirectory, 1024)
	state.ProjectLabel = boundedWelcomeText(state.ProjectLabel, 256)
	state.ProjectURL = boundedWelcomeText(state.ProjectURL, 16<<10)
	state.Agent = boundedWelcomeText(state.Agent, 128)
	state.ApprovalMode = boundedWelcomeText(state.ApprovalMode, 64)
	state.ThreadID = boundedWelcomeText(state.ThreadID, 256)
	state.StartupError = boundedWelcomeText(state.StartupError, 1024)
	state.MCPTools = clampWelcomeCount(state.MCPTools)
	state.MCPLoginRequired = clampWelcomeCount(state.MCPLoginRequired)
	state.MCPErrors = clampWelcomeCount(state.MCPErrors)
	state.MCPAwaitingReconnect = clampWelcomeCount(state.MCPAwaitingReconnect)
	return state
}

func boundedWelcomeText(value string, limit int) string {
	value = strings.TrimSpace(value)
	characters := []rune(value)
	if len(characters) > limit {
		characters = characters[:limit]
	}
	return string(characters)
}

func clampWelcomeCount(value int) int { return min(max(value, 0), 1_000_000) }

func welcomeMCPStatus(state welcomeBannerState, glyphs uiGlyphs) string {
	if state.MCPTools <= 0 && state.MCPLoginRequired <= 0 && state.MCPErrors <= 0 && state.MCPAwaitingReconnect <= 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("MCP: %d tools", max(state.MCPTools, 0))}
	if state.MCPLoginRequired > 0 {
		parts = append(parts, fmt.Sprintf("%d need login", state.MCPLoginRequired))
	}
	if state.MCPErrors > 0 {
		parts = append(parts, fmt.Sprintf("%d errors", state.MCPErrors))
	}
	if state.MCPAwaitingReconnect > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting reconnect", state.MCPAwaitingReconnect))
	}
	return strings.Join(parts, " "+glyphs.Bullet+" ")
}

func truncateDisplayLine(line string, width int, ellipsis string) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(line) <= width {
		return line
	}
	return ansi.Truncate(line, width, ellipsis)
}
