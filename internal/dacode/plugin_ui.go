package dacode

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type pluginRuntimeController interface {
	PluginSnapshot(context.Context) (pluginManagerSnapshot, error)
	InstallPlugin(context.Context, string) error
	SetPluginEnabled(context.Context, string, bool) error
	UninstallPlugin(context.Context, string) error
	AddPluginMarketplace(context.Context, string) error
	RemovePluginMarketplace(context.Context, string) error
	ReloadPlugins(context.Context) (pluginReloadResult, error)
}

type pluginManagerTab uint8

const (
	pluginTabDiscover pluginManagerTab = iota
	pluginTabInstalled
	pluginTabMarketplaces
)

func (tab pluginManagerTab) label() string {
	switch tab {
	case pluginTabInstalled:
		return "Installed"
	case pluginTabMarketplaces:
		return "Marketplaces"
	default:
		return "Discover"
	}
}

type pluginManagerState struct {
	tab               pluginManagerTab
	selected          int
	loading           bool
	mutating          bool
	dirty             bool
	addingMarketplace bool
	confirmRemoval    bool
	marketplaceInput  textinput.Model
	snapshot          pluginManagerSnapshot
	error             string
	status            string
}

type pluginManagerSnapshotMsg struct {
	snapshot pluginManagerSnapshot
	err      error
}

type pluginManagerMutation string

const (
	pluginMutationInstall           pluginManagerMutation = "install"
	pluginMutationEnable            pluginManagerMutation = "enable"
	pluginMutationDisable           pluginManagerMutation = "disable"
	pluginMutationUninstall         pluginManagerMutation = "uninstall"
	pluginMutationAddMarketplace    pluginManagerMutation = "add_marketplace"
	pluginMutationRemoveMarketplace pluginManagerMutation = "remove_marketplace"
)

type pluginManagerMutationMsg struct {
	action pluginManagerMutation
	target string
	err    error
}

type pluginReloadMsg struct {
	result pluginReloadResult
	err    error
}

func newPluginManagerState() *pluginManagerState {
	input := textinput.New()
	input.Prompt = "Marketplace source: "
	input.Placeholder = "directory, file, GitHub, Git, or HTTPS URL"
	input.CharLimit = 4096
	return &pluginManagerState{loading: true, marketplaceInput: input}
}

func loadPluginManager(ctx context.Context, controller pluginRuntimeController) tea.Cmd {
	if controller == nil {
		panic("dacode: plugin runtime controller is required")
	}
	return func() tea.Msg {
		snapshot, err := controller.PluginSnapshot(ctx)
		return pluginManagerSnapshotMsg{snapshot: snapshot, err: err}
	}
}

func mutatePluginManager(ctx context.Context, controller pluginRuntimeController, action pluginManagerMutation, target string) tea.Cmd {
	if controller == nil {
		panic("dacode: plugin runtime controller is required")
	}
	return func() tea.Msg {
		var err error
		switch action {
		case pluginMutationInstall:
			err = controller.InstallPlugin(ctx, target)
		case pluginMutationEnable:
			err = controller.SetPluginEnabled(ctx, target, true)
		case pluginMutationDisable:
			err = controller.SetPluginEnabled(ctx, target, false)
		case pluginMutationUninstall:
			err = controller.UninstallPlugin(ctx, target)
		case pluginMutationAddMarketplace:
			err = controller.AddPluginMarketplace(ctx, target)
		case pluginMutationRemoveMarketplace:
			err = controller.RemovePluginMarketplace(ctx, target)
		default:
			panic("dacode: invalid plugin manager mutation")
		}
		return pluginManagerMutationMsg{action: action, target: target, err: err}
	}
}

func reloadPluginRuntime(ctx context.Context, controller pluginRuntimeController) tea.Cmd {
	if controller == nil {
		panic("dacode: plugin runtime controller is required")
	}
	return func() tea.Msg {
		result, err := controller.ReloadPlugins(ctx)
		return pluginReloadMsg{result: result, err: err}
	}
}

func (state *pluginManagerState) applySnapshot(message pluginManagerSnapshotMsg) {
	state.loading = false
	if message.err != nil {
		state.error = pluginManagerDisplayError(message.err)
		return
	}
	state.snapshot = message.snapshot
	state.error = ""
	state.clampSelection()
}

func (state *pluginManagerState) applyMutation(message pluginManagerMutationMsg) {
	state.mutating = false
	if message.err != nil {
		state.error = pluginManagerDisplayError(message.err)
		return
	}
	state.dirty = true
	state.error = ""
	state.status = pluginMutationLabel(message.action) + ". Reload pending."
	state.addingMarketplace = false
	state.marketplaceInput.SetValue("")
	state.loading = true
}

func pluginMutationLabel(action pluginManagerMutation) string {
	switch action {
	case pluginMutationInstall:
		return "Plugin installed"
	case pluginMutationEnable:
		return "Plugin enabled"
	case pluginMutationDisable:
		return "Plugin disabled"
	case pluginMutationUninstall:
		return "Plugin uninstalled"
	case pluginMutationAddMarketplace:
		return "Marketplace added"
	case pluginMutationRemoveMarketplace:
		return "Marketplace removed"
	default:
		return "Plugin state changed"
	}
}

func (state *pluginManagerState) switchTab(direction int) {
	count := int(pluginTabMarketplaces) + 1
	state.tab = pluginManagerTab((int(state.tab) + direction + count) % count)
	state.selected = 0
	state.error = ""
}

func (state *pluginManagerState) move(direction int) {
	count := state.rowCount()
	if count == 0 {
		state.selected = 0
		return
	}
	state.selected = (state.selected + direction + count) % count
}

func (state *pluginManagerState) clampSelection() {
	count := state.rowCount()
	if count == 0 {
		state.selected = 0
	} else if state.selected >= count {
		state.selected = count - 1
	}
}

func (state *pluginManagerState) rowCount() int {
	switch state.tab {
	case pluginTabInstalled:
		return len(state.snapshot.Installed)
	case pluginTabMarketplaces:
		return len(state.snapshot.Marketplaces)
	default:
		return len(state.snapshot.Available)
	}
}

func (state *pluginManagerState) selectedPlugin() (pluginManagerPlugin, bool) {
	var rows []pluginManagerPlugin
	if state.tab == pluginTabInstalled {
		rows = state.snapshot.Installed
	} else if state.tab == pluginTabDiscover {
		rows = state.snapshot.Available
	}
	if state.selected < 0 || state.selected >= len(rows) {
		return pluginManagerPlugin{}, false
	}
	return rows[state.selected], true
}

func (state *pluginManagerState) selectedMarketplace() (pluginManagerMarketplace, bool) {
	if state.tab != pluginTabMarketplaces || state.selected < 0 || state.selected >= len(state.snapshot.Marketplaces) {
		return pluginManagerMarketplace{}, false
	}
	return state.snapshot.Marketplaces[state.selected], true
}

// handleKey owns all plugin-manager navigation and mutations. It returns close
// separately so the host can decide whether a dirty manager needs the reload
// confirmation before dismissing it.
func (state *pluginManagerState) handleKey(ctx context.Context, controller pluginRuntimeController, message tea.KeyPressMsg) (tea.Cmd, bool) {
	if state == nil || controller == nil {
		panic("dacode: plugin manager state and controller are required")
	}
	if state.addingMarketplace {
		switch message.String() {
		case "esc":
			state.addingMarketplace = false
			state.marketplaceInput.Blur()
			state.marketplaceInput.SetValue("")
			state.error = ""
			return nil, false
		case "enter":
			source := strings.TrimSpace(state.marketplaceInput.Value())
			if source == "" {
				state.error = "Marketplace source is required."
				return nil, false
			}
			state.mutating = true
			return mutatePluginManager(ctx, controller, pluginMutationAddMarketplace, source), false
		default:
			var command tea.Cmd
			state.marketplaceInput, command = state.marketplaceInput.Update(message)
			return command, false
		}
	}
	if state.confirmRemoval {
		switch message.String() {
		case "esc":
			state.confirmRemoval = false
			return nil, false
		case "enter":
			marketplace, exists := state.selectedMarketplace()
			if !exists {
				state.confirmRemoval = false
				return nil, false
			}
			state.confirmRemoval = false
			state.mutating = true
			return mutatePluginManager(ctx, controller, pluginMutationRemoveMarketplace, marketplace.Name), false
		default:
			return nil, false
		}
	}
	if state.loading || state.mutating {
		return nil, message.String() == "esc"
	}
	switch message.String() {
	case "esc":
		return nil, true
	case "left", "shift+tab":
		state.switchTab(-1)
	case "right", "tab":
		state.switchTab(1)
	case "up":
		state.move(-1)
	case "down":
		state.move(1)
	case "enter":
		plugin, exists := state.selectedPlugin()
		if !exists {
			return nil, false
		}
		state.mutating = true
		if state.tab == pluginTabDiscover {
			return mutatePluginManager(ctx, controller, pluginMutationInstall, plugin.ID), false
		}
		action := pluginMutationEnable
		if plugin.Enabled {
			action = pluginMutationDisable
		}
		return mutatePluginManager(ctx, controller, action, plugin.ID), false
	default:
		runes := []rune(message.Text)
		if len(runes) != 1 {
			return nil, false
		}
		switch runes[0] {
		case 'u', 'U':
			plugin, exists := state.selectedPlugin()
			if state.tab != pluginTabInstalled || !exists {
				return nil, false
			}
			state.mutating = true
			return mutatePluginManager(ctx, controller, pluginMutationUninstall, plugin.ID), false
		case 'a', 'A':
			if state.tab != pluginTabMarketplaces {
				return nil, false
			}
			state.addingMarketplace = true
			state.error = ""
			state.marketplaceInput.Focus()
		case 'd', 'D':
			if _, exists := state.selectedMarketplace(); !exists {
				return nil, false
			}
			state.confirmRemoval = true
		}
	}
	return nil, false
}

func renderPluginManager(state *pluginManagerState, width, height int) string {
	return renderPluginManagerWithGlyphs(state, width, height, unicodeUIGlyphs)
}

func renderPluginManagerWithGlyphs(state *pluginManagerState, width, height int, glyphs uiGlyphs) string {
	contentWidth := min(max(width-12, 44), 86)
	separator := "  " + glyphs.Bullet + "  "
	var tabs []string
	for tab := pluginTabDiscover; tab <= pluginTabMarketplaces; tab++ {
		style := lipgloss.NewStyle().Foreground(colorMuted)
		if tab == state.tab {
			style = lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Underline(true)
		}
		tabs = append(tabs, style.Render(tab.label()))
	}
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Plugins"),
		strings.Join(tabs, "   "), "",
	}
	if state.loading {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("Loading plugins"+glyphs.Ellipsis))
	} else if state.addingMarketplace {
		lines = append(lines, state.marketplaceInput.View(), "", lipgloss.NewStyle().Foreground(colorMuted).Render("Enter add"+separator+"Esc cancel"))
	} else if state.confirmRemoval {
		marketplace, _ := state.selectedMarketplace()
		lines = append(lines,
			lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("Remove marketplace "+unicodesecurity.RenderTerminalSafe(marketplace.Name)+"?"),
			"", "Installed plugins must be uninstalled first. This removes the local marketplace record.", "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Enter remove"+separator+"Esc cancel"),
		)
	} else {
		rows := renderPluginManagerRows(state, contentWidth, glyphs)
		lines = append(lines, rows...)
	}
	if state.status != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorSuccess).Render(unicodesecurity.RenderTerminalSafe(state.status)))
	}
	if state.error != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorError).Render(unicodesecurity.RenderTerminalSafe(state.error)))
	}
	if !state.loading && !state.addingMarketplace && !state.confirmRemoval {
		help := glyphs.ArrowLeft + "/" + glyphs.ArrowRight + " tabs" + separator + glyphs.ArrowUp + "/" + glyphs.ArrowDown + " select" + separator + "Enter install" + separator + "Esc close"
		switch state.tab {
		case pluginTabInstalled:
			help = glyphs.ArrowLeft + "/" + glyphs.ArrowRight + " tabs" + separator + glyphs.ArrowUp + "/" + glyphs.ArrowDown + " select" + separator + "Enter enable/disable" + separator + "U uninstall" + separator + "Esc close"
		case pluginTabMarketplaces:
			help = glyphs.ArrowLeft + "/" + glyphs.ArrowRight + " tabs" + separator + glyphs.ArrowUp + "/" + glyphs.ArrowDown + " select" + separator + "A add" + separator + "D remove" + separator + "Esc close"
		}
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(help))
	}
	border := uiBorder(glyphs)
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).
		Padding(1, 2).Width(contentWidth).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func renderPluginManagerRows(state *pluginManagerState, width int, glyphs uiGlyphs) []string {
	var lines []string
	switch state.tab {
	case pluginTabInstalled:
		for index, row := range state.snapshot.Installed {
			status := "disabled"
			if row.Enabled {
				status = "enabled"
			}
			if row.Pending {
				status += " " + glyphs.Separator + " pending /reload"
			}
			components := fmt.Sprintf("%d skills %s %d MCP %s %d hooks", row.Skills, glyphs.Separator, row.MCP, glyphs.Separator, row.Hooks)
			lines = append(lines, renderPluginManagerRow(index == state.selected, row.Name+" @ "+row.Marketplace, status+" "+glyphs.Separator+" "+components, width, glyphs))
		}
	case pluginTabMarketplaces:
		for index, row := range state.snapshot.Marketplaces {
			status := fmt.Sprintf("%s %s %d plugins %s %d installed", row.Source, glyphs.Separator, row.PluginCount, glyphs.Separator, row.InstalledCount)
			if row.Error {
				status = "error " + glyphs.Separator + " " + status
			}
			lines = append(lines, renderPluginManagerRow(index == state.selected, row.Name, status, width, glyphs))
		}
	default:
		for index, row := range state.snapshot.Available {
			lines = append(lines, renderPluginManagerRow(index == state.selected, row.Name+" @ "+row.Marketplace, row.Description, width, glyphs))
		}
	}
	if len(lines) == 0 {
		label := "No plugins available. Add a marketplace first."
		if state.tab == pluginTabInstalled {
			label = "No plugins installed."
		} else if state.tab == pluginTabMarketplaces {
			label = "No marketplaces configured. Press A to add one."
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(label))
	}
	return lines
}

func renderPluginManagerRow(selected bool, title, detail string, width int, glyphs uiGlyphs) string {
	prefix := "  "
	style := lipgloss.NewStyle().Foreground(colorBody)
	if selected {
		prefix = glyphs.Cursor + " "
		style = style.Foreground(colorPrimary).Bold(true)
	}
	title = truncate(unicodesecurity.RenderTerminalSafe(title), max(width-4, 8))
	detail = truncate(unicodesecurity.RenderTerminalSafe(detail), max(width-6, 8))
	return style.Render(prefix+title) + "\n" + lipgloss.NewStyle().Foreground(colorMuted).Render("    "+detail)
}

func renderPluginReloadPrompt(width, height int) string {
	return renderPluginReloadPromptWithGlyphs(width, height, unicodeUIGlyphs)
}

func renderPluginReloadPromptWithGlyphs(width, height int, glyphs uiGlyphs) string {
	body := "Plugin changes are pending. Reloading rebuilds skills, hooks, and MCP connections for this session."
	lines := []string{
		lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("Reload plugins?"), "", body, "",
		lipgloss.NewStyle().Foreground(colorMuted).Render("Enter reload  " + glyphs.Bullet + "  Esc later"),
	}
	panel := lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorWarning).
		Padding(1, 2).Width(min(max(width-16, 40), 72)).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func renderPluginReloading(width, height int) string {
	return renderPluginReloadingWithGlyphs(width, height, unicodeUIGlyphs)
}

func renderPluginReloadingWithGlyphs(width, height int, glyphs uiGlyphs) string {
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Reloading configuration" + glyphs.Ellipsis), "",
		"Rebuilding plugin skills, hooks, MCP connections, and reloadable environment settings.",
	}
	panel := lipgloss.NewStyle().Border(uiBorder(glyphs)).BorderForeground(colorPrimary).
		Padding(1, 2).Width(min(max(width-16, 40), 72)).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, panel)
}

func pluginReloadSummary(result pluginReloadResult) string {
	lines := []string{"Configuration reloaded."}
	if len(result.Changes) == 0 {
		lines = append(lines, "Environment unchanged.")
	} else {
		lines = append(lines, result.Changes...)
	}
	if len(result.Added) > 0 {
		lines = append(lines, "Loaded plugins: "+strings.Join(result.Added, ", "))
	}
	if len(result.Removed) > 0 {
		lines = append(lines, "Unloaded plugins: "+strings.Join(result.Removed, ", "))
	}
	if len(result.Added)+len(result.Removed) == 0 {
		lines = append(lines, "Plugin state unchanged.")
	}
	for _, warning := range result.Warnings {
		lines = append(lines, "Warning: "+warning)
	}
	return strings.Join(lines, "\n")
}
