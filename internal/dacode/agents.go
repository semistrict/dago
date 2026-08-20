package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	defaultAgentName          = "dacode"
	agentInstructionsFilename = "AGENTS.md"
	agentSkillsDirectory      = "skills"
	agentSessionsDirectory    = "sessions"
	agentSessionGeneration    = "generation"
	defaultAgentFilename      = "default-agent"
	recentAgentFilename       = "recent-agent"
	maxAgentInstructionsBytes = 1 << 20
	maxDefaultAgentBytes      = 256
	maxDiscoveredAgents       = 256
	maxAgentStateEntries      = 1024
	maxAgentNameBytes         = 128
	maxAgentGenerationBytes   = 64
)

var reservedAgentNames = map[string]struct{}{
	"bin": {}, "plugins": {}, "conversation_history": {},
}

type agentInfo struct {
	Name    string
	Current bool
	Default bool
}

type agentPickerState struct {
	agents        []agentInfo
	selected      int
	loading       bool
	savingDefault bool
	err           error
	notice        string
}

type agentsLoadedMsg struct {
	agents []agentInfo
	err    error
}

type agentSwitchedMsg struct {
	name       string
	threadID   string
	err        error
	generation uint64
}

type defaultAgentSavedMsg struct {
	name       string
	newDefault string
	err        error
}

func listAgents(ctx context.Context, runner agentRunner) tea.Cmd {
	return func() tea.Msg {
		agents, err := runner.ListAgents(ctx)
		return agentsLoadedMsg{agents: agents, err: err}
	}
}

func switchAgent(ctx context.Context, runner agentRunner, name, threadID string) tea.Cmd {
	return switchAgentGeneration(ctx, runner, name, threadID, 0)
}

func switchAgentGeneration(ctx context.Context, runner agentRunner, name, threadID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		return agentSwitchedMsg{name: name, threadID: threadID, generation: generation, err: runner.SwitchAgent(ctx, name)}
	}
}

func setDefaultAgent(ctx context.Context, runner agentRunner, name string) tea.Cmd {
	return func() tea.Msg {
		newDefault, err := runner.SetDefaultAgent(ctx, name)
		return defaultAgentSavedMsg{name: name, newDefault: newDefault, err: err}
	}
}

func (model *tuiModel) finishAgentList(message agentsLoadedMsg) {
	if model.agentPicker == nil {
		return
	}
	model.agentPicker.loading = false
	model.agentPicker.agents = message.agents
	model.agentPicker.err = message.err
	for index, agent := range message.agents {
		if agent.Current {
			model.agentPicker.selected = index
			break
		}
	}
}

func (model *tuiModel) finishAgentSwitch(message agentSwitchedMsg) {
	if message.err != nil {
		model.status = "Agent switch failed"
		model.appendItem(transcriptItem{kind: itemError, text: "Could not switch agent: " + message.err.Error()})
		model.refreshTranscript()
		return
	}
	model.operationGeneration++
	model.agentName = message.name
	model.threadID = message.threadID
	model.threadHasCheckpoint = false
	approvalModeErr := model.startNewApprovalThread(message.threadID)
	model.goal = nil
	model.items = nil
	model.toolItems = map[string]int{}
	model.currentAssistant = -1
	model.resetUsage()
	model.status = "Ready"
	model.appendItem(transcriptItem{kind: itemNotice, text: "Switched to agent " + message.name + ". Started a new thread."})
	if approvalModeErr != nil {
		model.appendItem(transcriptItem{kind: itemError, text: "Could not initialize thread approval mode; using Manual: " + approvalModeErr.Error()})
	}
	model.refreshTranscript()
}

func (model *tuiModel) finishDefaultAgentSave(message defaultAgentSavedMsg) {
	if model.agentPicker == nil {
		return
	}
	picker := model.agentPicker
	picker.savingDefault = false
	if message.err != nil {
		picker.notice = "Failed to save default: " + message.err.Error()
		return
	}
	for index := range picker.agents {
		picker.agents[index].Default = picker.agents[index].Name == message.newDefault
	}
	if message.newDefault == "" {
		picker.notice = "Default cleared"
	} else {
		picker.notice = "Default set to " + message.newDefault
	}
}

func (model *tuiModel) handleAgentKey(message tea.KeyPressMsg) (tea.Cmd, bool) {
	picker := model.agentPicker
	if picker == nil {
		return nil, false
	}
	switch message.String() {
	case "esc", "q", "ctrl+c":
		model.agentPicker = nil
		return nil, true
	case "up", "k", "shift+tab":
		if !picker.loading && !picker.savingDefault && len(picker.agents) > 0 {
			picker.selected = (picker.selected - 1 + len(picker.agents)) % len(picker.agents)
			picker.notice = ""
		}
		return nil, true
	case "down", "j", "tab":
		if !picker.loading && !picker.savingDefault && len(picker.agents) > 0 {
			picker.selected = (picker.selected + 1) % len(picker.agents)
			picker.notice = ""
		}
		return nil, true
	case "ctrl+s":
		if !picker.loading && !picker.savingDefault && picker.err == nil && len(picker.agents) > 0 {
			picker.savingDefault = true
			picker.notice = "Saving default" + model.glyphs.Ellipsis
			return setDefaultAgent(model.ctx, model.runner, picker.agents[picker.selected].Name), true
		}
		return nil, true
	case "enter":
		if picker.loading || picker.savingDefault || picker.err != nil || len(picker.agents) == 0 {
			return nil, true
		}
		selected := picker.agents[picker.selected]
		model.agentPicker = nil
		if selected.Current {
			return nil, true
		}
		threadID, err := newThreadID()
		if err != nil {
			model.appendItem(transcriptItem{kind: itemError, text: "Could not start a thread for the selected agent: " + err.Error()})
			model.refreshTranscript()
			return nil, true
		}
		if model.interactionBusy() {
			return model.deferAgentSwitch(selected.Name, threadID), true
		}
		model.status = "Switching agent"
		model.refreshTranscript()
		return switchAgentGeneration(model.ctx, model.runner, selected.Name, threadID, model.operationGeneration), true
	}
	return nil, true
}

func (model *tuiModel) renderAgentPicker() string {
	picker := model.agentPicker
	if picker == nil {
		return ""
	}
	contentWidth := min(max(model.width-16, 36), 64)
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Select Agent"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("Switching starts a new thread with that agent's instructions."),
		"",
	}
	switch {
	case picker.loading:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(model.spinner.View()+" Loading agents"+model.glyphs.Ellipsis))
	case picker.err != nil:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorError).Render(unicodesecurity.RenderTerminalSafe(picker.err.Error())))
	case len(picker.agents) == 0:
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No agents found."))
	default:
		for index, agent := range picker.agents {
			marker := "  "
			if index == picker.selected {
				marker = model.glyphs.Cursor + " "
			}
			labels := make([]string, 0, 2)
			if agent.Current {
				labels = append(labels, "current")
			}
			if agent.Default {
				labels = append(labels, "default")
			}
			label := ""
			if len(labels) > 0 {
				label = " (" + strings.Join(labels, ", ") + ")"
			}
			line := marker + unicodesecurity.RenderTerminalSafe(agent.Name) + label
			style := lipgloss.NewStyle().Foreground(colorBody).Padding(0, 1).Width(contentWidth)
			if index == picker.selected {
				style = style.Background(colorPrimary).Foreground(colorBackground).Bold(true)
			}
			lines = append(lines, style.Render(line))
		}
	}
	lines = append(lines, "")
	separator := "  " + model.glyphs.Bullet + "  "
	navigation := model.glyphs.ArrowUp + model.glyphs.ArrowDown + " or Tab switch" + separator + "Enter select"
	hint := navigation + "\nCtrl+S set default" + separator + "Esc cancel"
	if picker.savingDefault {
		hint = model.spinner.View() + " Saving default" + model.glyphs.Ellipsis
	} else if picker.notice != "" {
		hint = unicodesecurity.RenderTerminalSafe(picker.notice) + "\n" + navigation + separator + "Esc cancel"
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(hint))
	panel := lipgloss.NewStyle().Border(model.uiBorder()).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(model.width, model.height, lipgloss.Center, lipgloss.Center, panel)
}

func discoverAgents(ctx context.Context, stateDir, current string) ([]agentInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names := []string{defaultAgentName}
	directory, err := os.Open(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	var entries []os.DirEntry
	if err == nil {
		defer directory.Close()
		entries, err = directory.ReadDir(maxAgentStateEntries + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		if len(entries) > maxAgentStateEntries {
			return nil, fmt.Errorf("list agents: state entry count exceeds %d", maxAgentStateEntries)
		}
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if validateAgentName(name) != nil || name == defaultAgentName || strings.HasPrefix(name, ".") || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		directory, openErr := os.OpenRoot(stateDir)
		if openErr != nil {
			return nil, fmt.Errorf("open agent directory: %w", openErr)
		}
		info, statErr := directory.Lstat(filepath.Join(name, agentInstructionsFilename))
		_ = directory.Close()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		names = append(names, name)
		if len(names) > maxDiscoveredAgents {
			return nil, fmt.Errorf("list agents: profile count exceeds %d", maxDiscoveredAgents)
		}
	}
	sort.Strings(names)
	defaultName, _ := configuredDefaultAgent(stateDir)
	agents := make([]agentInfo, len(names))
	for index, name := range names {
		agents[index] = agentInfo{Name: name, Current: name == current, Default: name == defaultName}
	}
	return agents, nil
}

func loadAgentInstructions(ctx context.Context, stateDir, name string) (string, error) {
	if err := validateAgentName(name); err != nil {
		return "", err
	}
	if name == defaultAgentName {
		return "", ctx.Err()
	}
	agents, err := discoverAgents(ctx, stateDir, "")
	if err != nil {
		return "", err
	}
	available := false
	for _, agent := range agents {
		if agent.Name == name {
			available = true
			break
		}
	}
	if !available {
		return "", fmt.Errorf("agent %q is no longer available", name)
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return "", fmt.Errorf("open agent storage: %w", err)
	}
	defer root.Close()
	markerPath := filepath.Join(name, agentInstructionsFilename)
	markerInfo, err := root.Lstat(markerPath)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("instruction marker is not a regular file")
		}
		return "", fmt.Errorf("inspect agent %q instructions: %w", name, err)
	}
	file, err := root.Open(markerPath)
	if err != nil {
		return "", fmt.Errorf("open agent %q instructions: %w", name, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect opened agent %q instructions: %w", name, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(markerInfo, openedInfo) {
		return "", fmt.Errorf("agent %q instructions changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAgentInstructionsBytes+1))
	if err != nil {
		return "", fmt.Errorf("read agent %q instructions: %w", name, err)
	}
	if len(data) > maxAgentInstructionsBytes {
		return "", fmt.Errorf("agent %q instructions exceed %d bytes", name, maxAgentInstructionsBytes)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("agent %q instructions are not valid UTF-8", name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func configuredDefaultAgent(stateDir string) (string, error) {
	root, err := os.OpenRoot(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open agent settings: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat(defaultAgentFilename)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect default agent: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("default agent setting is not a regular file")
	}
	file, err := root.Open(defaultAgentFilename)
	if err != nil {
		return "", fmt.Errorf("open default agent: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect opened default agent: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", fmt.Errorf("default agent setting changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDefaultAgentBytes+1))
	if err != nil {
		return "", fmt.Errorf("read default agent: %w", err)
	}
	if len(data) > maxDefaultAgentBytes {
		return "", fmt.Errorf("default agent setting exceeds %d bytes", maxDefaultAgentBytes)
	}
	rawName := strings.TrimSuffix(string(data), "\n")
	rawName = strings.TrimSuffix(rawName, "\r")
	name := rawName
	if err := validateAgentName(name); err != nil {
		return "", fmt.Errorf("default agent setting: %w", err)
	}
	return name, nil
}

func toggleDefaultAgent(ctx context.Context, stateDir, name string) (string, error) {
	if err := validateAgentName(name); err != nil {
		return "", err
	}
	agents, err := discoverAgents(ctx, stateDir, "")
	if err != nil {
		return "", err
	}
	available := false
	for _, agent := range agents {
		if agent.Name == name {
			available = true
			break
		}
	}
	if !available {
		return "", fmt.Errorf("agent %q is no longer available", name)
	}
	current, err := configuredDefaultAgent(stateDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(stateDir, defaultAgentFilename)
	if current == name {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("clear default agent: %w", err)
		}
		return "", nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create agent settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(stateDir, ".default-agent-*")
	if err != nil {
		return "", fmt.Errorf("create default agent setting: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return "", fmt.Errorf("secure default agent setting: %w", err)
	}
	if _, err := io.WriteString(temporary, name+"\n"); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write default agent setting: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync default agent setting: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close default agent setting: %w", err)
	}
	if err := replaceFileDurably(temporaryPath, path); err != nil {
		return "", fmt.Errorf("replace default agent setting: %w", err)
	}
	return name, nil
}

func validateAgentName(name string) error {
	if name == "" {
		return fmt.Errorf("agent name is empty")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("agent name is not valid UTF-8")
	}
	if len(name) > maxAgentNameBytes {
		return fmt.Errorf("agent name exceeds %d bytes", maxAgentNameBytes)
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("agent name has leading or trailing whitespace")
	}
	if name == "." || name == ".." || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("agent name %q is not a single path component", name)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("agent name %q is reserved", name)
	}
	for reserved := range reservedAgentNames {
		if strings.EqualFold(name, reserved) {
			return fmt.Errorf("agent name %q is reserved", name)
		}
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("agent name contains control characters")
		}
	}
	return nil
}
