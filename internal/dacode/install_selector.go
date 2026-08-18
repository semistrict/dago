package dacode

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dainstall"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

type installSelectorEntry struct {
	Name        string
	Kind        dainstall.Kind
	Description string
	BuiltIn     bool
}

type installSelectorAction uint8

const (
	installSelectorNoAction installSelectorAction = iota
	installSelectorCancel
	installSelectorAlreadyAvailable
	installSelectorConfirm
	installSelectorInstall
)

type installSelectorResult struct {
	Action  installSelectorAction
	Entry   installSelectorEntry
	Request installRequest
}

type installRequest struct {
	SelectorID uint64
	Generation uint64
	Kind       dainstall.Kind
	Name       string
	Force      bool
}

type installSelectorArguments struct {
	Query string
	Force bool
}

type installSelectorState struct {
	entries       []installSelectorEntry
	visible       []int
	selected      int
	query         string
	force         bool
	argumentError string
	selectorID    uint64
	generation    uint64
	confirmation  *installRequest
}

var nextInstallSelectorID atomic.Uint64

func newInstallSelector() *installSelectorState {
	specs := dacodeInstallCatalog()
	entries := make([]installSelectorEntry, 0, len(specs))
	for _, spec := range specs {
		entries = append(entries, installSelectorEntry{Name: spec.Name, Kind: spec.Kind, Description: spec.Description, BuiltIn: spec.BuiltIn})
	}
	return newInstallSelectorWithEntries(entries)
}

func newInstallSelectorWithEntries(entries []installSelectorEntry) *installSelectorState {
	state := &installSelectorState{selectorID: nextInstallSelectorID.Add(1), generation: 1}
	seen := map[string]bool{}
	for _, entry := range entries {
		if len(state.entries) == 256 {
			break
		}
		entry.Name = strings.ToLower(strings.TrimSpace(entry.Name))
		entry.Description = boundedTerminalText(entry.Description, 512)
		if !validInstallSelectorName(entry.Name) || seen[string(entry.Kind)+"\x00"+entry.Name] ||
			(entry.Kind != dainstall.Extra && entry.Kind != dainstall.Package) {
			continue
		}
		seen[string(entry.Kind)+"\x00"+entry.Name] = true
		state.entries = append(state.entries, entry)
	}
	sort.Slice(state.entries, func(left, right int) bool {
		if state.entries[left].BuiltIn != state.entries[right].BuiltIn {
			return state.entries[left].BuiltIn
		}
		if state.entries[left].Name != state.entries[right].Name {
			return state.entries[left].Name < state.entries[right].Name
		}
		return state.entries[left].Kind < state.entries[right].Kind
	})
	state.refilter()
	return state
}

func (state *installSelectorState) handleKey(key string, pageHeight int) installSelectorResult {
	if state.argumentError != "" {
		if key == "esc" || key == "ctrl+c" {
			return installSelectorResult{Action: installSelectorCancel}
		}
		return installSelectorResult{}
	}
	if state.confirmation != nil {
		entry, ok := state.entryForRequest(*state.confirmation)
		if !ok {
			state.confirmation = nil
			return installSelectorResult{}
		}
		switch strings.ToLower(key) {
		case "y", "enter":
			request := *state.confirmation
			state.confirmation = nil
			return installSelectorResult{Action: installSelectorInstall, Entry: entry, Request: request}
		case "n", "esc", "ctrl+c":
			state.confirmation = nil
		}
		return installSelectorResult{}
	}
	switch key {
	case "esc", "ctrl+c":
		return installSelectorResult{Action: installSelectorCancel}
	case "up":
		state.move(-1)
	case "down":
		state.move(1)
	case "pgup":
		state.move(-max(pageHeight, 1))
	case "pgdown":
		state.move(max(pageHeight, 1))
	case "backspace":
		characters := []rune(state.query)
		if len(characters) > 0 {
			state.setQuery(string(characters[:len(characters)-1]))
		}
	case "enter":
		entry, ok := state.selectedEntry()
		if !ok {
			return installSelectorResult{}
		}
		if entry.BuiltIn {
			return installSelectorResult{Action: installSelectorAlreadyAvailable, Entry: entry}
		}
		request := state.request(entry)
		if state.force {
			return installSelectorResult{Action: installSelectorInstall, Entry: entry, Request: request}
		}
		state.confirmation = &request
		return installSelectorResult{Action: installSelectorConfirm, Entry: entry, Request: request}
	default:
		characters := []rune(key)
		if len(characters) == 1 && unicode.IsPrint(characters[0]) && len([]rune(state.query)) < 128 {
			state.setQuery(state.query + key)
		}
	}
	return installSelectorResult{}
}

func parseInstallSelectorArguments(arguments string) (installSelectorArguments, error) {
	if len(arguments) > 512 || hasModelSelectorControl(arguments) {
		return installSelectorArguments{}, fmt.Errorf("install arguments are invalid")
	}
	result := installSelectorArguments{}
	for _, argument := range strings.Fields(arguments) {
		switch {
		case argument == "--force":
			result.Force = true
		case strings.HasPrefix(argument, "-"):
			return installSelectorArguments{}, fmt.Errorf("unknown install option %q", boundedTerminalText(argument, 80))
		case result.Query != "":
			return installSelectorArguments{}, fmt.Errorf("install accepts at most one integration name")
		default:
			result.Query = strings.ToLower(argument)
		}
	}
	return result, nil
}

// installStartupRecoveryBypassCapable recognizes only the bounded TUI syntax.
// It grants no execution authority; exact allowlisting remains the selector and
// controller's responsibility.
func installStartupRecoveryBypassCapable(arguments string) bool {
	_, err := parseInstallSelectorArguments(arguments)
	return err == nil
}

func (state *installSelectorState) applyArguments(arguments installSelectorArguments) {
	state.force = arguments.Force
	state.setQuery(arguments.Query)
}

func (state *installSelectorState) setArgumentError(err error) {
	state.argumentError = boundedTerminalText(err.Error(), 320)
	state.confirmation = nil
	state.visible = nil
}

func (state *installSelectorState) request(entry installSelectorEntry) installRequest {
	return installRequest{
		SelectorID: state.selectorID, Generation: state.generation,
		Kind: entry.Kind, Name: entry.Name, Force: state.force,
	}
}

func (state *installSelectorState) entryForRequest(request installRequest) (installSelectorEntry, bool) {
	if state == nil || request.SelectorID != state.selectorID || request.Generation != state.generation ||
		!validInstallSelectorName(request.Name) {
		return installSelectorEntry{}, false
	}
	for _, entry := range state.entries {
		if entry.Kind == request.Kind && entry.Name == request.Name && !entry.BuiltIn {
			return entry, true
		}
	}
	return installSelectorEntry{}, false
}

func validInstallSelectorName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for index, character := range name {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			index > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	last := name[len(name)-1]
	return last >= 'a' && last <= 'z' || last >= '0' && last <= '9'
}

func (state *installSelectorState) setQuery(query string) {
	state.query = strings.TrimSpace(query)
	state.confirmation = nil
	state.refilter()
}

func (state *installSelectorState) refilter() {
	selectedName := ""
	selectedKind := dainstall.Kind("")
	if entry, ok := state.selectedEntry(); ok {
		selectedName = entry.Name
		selectedKind = entry.Kind
	}
	query := strings.ToLower(state.query)
	state.visible = state.visible[:0]
	for index, entry := range state.entries {
		if query == "" || strings.Contains(entry.Name, query) || strings.Contains(strings.ToLower(entry.Description), query) || strings.Contains(strings.ToLower(string(entry.Kind)), query) {
			state.visible = append(state.visible, index)
		}
	}
	state.selected = 0
	for visible, entryIndex := range state.visible {
		if state.entries[entryIndex].Name == selectedName && state.entries[entryIndex].Kind == selectedKind {
			state.selected = visible
			break
		}
	}
}

func (state *installSelectorState) move(delta int) {
	if len(state.visible) == 0 {
		state.selected = 0
		return
	}
	state.selected = wrapIndex(state.selected+delta, len(state.visible))
}

func (state *installSelectorState) selectedEntry() (installSelectorEntry, bool) {
	if state == nil || state.selected < 0 || state.selected >= len(state.visible) {
		return installSelectorEntry{}, false
	}
	return state.entries[state.visible[state.selected]], true
}

func renderInstallSelector(state *installSelectorState, width, height int, glyphs uiGlyphs) string {
	if state == nil {
		return ""
	}
	contentWidth := min(max(width-12, 40), 76)
	lines := []string{
		lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Render("Install optional integration"),
		lipgloss.NewStyle().Foreground(colorMuted).Render("Only integrations allowlisted by this build can be selected."),
		lipgloss.NewStyle().Foreground(colorBody).Render("Filter: ") + lipgloss.NewStyle().Foreground(colorPrimary).Render(unicodesecurity.RenderTerminalSafe(state.query)+glyphs.Cursor),
		"",
	}
	if state.argumentError != "" {
		lines = append(lines,
			lipgloss.NewStyle().Foreground(colorError).Render("Invalid /install arguments: "+unicodesecurity.RenderTerminalSafe(state.argumentError)), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Esc cancel"))
	} else if state.confirmation != nil {
		entry, _ := state.entryForRequest(*state.confirmation)
		lines = append(lines,
			lipgloss.NewStyle().Foreground(colorWarning).Bold(true).Render("Run the allowlisted installer for "+unicodesecurity.RenderTerminalSafe(entry.Name)+"?"),
			lipgloss.NewStyle().Foreground(colorMuted).Render("Third-party installer and build code may execute."), "",
			lipgloss.NewStyle().Foreground(colorMuted).Render("Y/Enter install  "+glyphs.Bullet+"  N/Esc cancel"))
	} else if len(state.visible) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render("No matching integration."))
	} else {
		if state.force {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorWarning).Render("--force: confirmation will be skipped; allowlisting still applies."), "")
		}
		listHeight := min(max(height-11, 3), 16)
		start := max(state.selected-listHeight/2, 0)
		start = min(start, max(len(state.visible)-listHeight, 0))
		for visible := start; visible < min(start+listHeight, len(state.visible)); visible++ {
			entry := state.entries[state.visible[visible]]
			status := "installable"
			if entry.BuiltIn {
				status = "included"
			}
			label := entry.Name + "  " + glyphs.Bullet + "  " + status
			if entry.Description != "" {
				label += "  " + glyphs.Bullet + "  " + entry.Description
			}
			prefix := "  "
			style := lipgloss.NewStyle().Foreground(colorBody).Width(contentWidth)
			if visible == state.selected {
				prefix = glyphs.Cursor + " "
				style = style.Background(colorPanel).Foreground(colorPrimary).Bold(true)
			}
			lines = append(lines, style.Render(prefix+unicodesecurity.RenderTerminalSafe(label)))
		}
		lines = append(lines, "", lipgloss.NewStyle().Foreground(colorMuted).Render(glyphs.ArrowUp+"/"+glyphs.ArrowDown+" navigate  "+glyphs.Bullet+"  Enter select  "+glyphs.Bullet+"  Esc cancel"))
	}
	border := lipgloss.RoundedBorder()
	if glyphs.BoxHorizontal == asciiUIGlyphs.BoxHorizontal {
		border = lipgloss.Border{Top: "-", Bottom: "-", Left: "|", Right: "|", TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+"}
	}
	panel := lipgloss.NewStyle().Border(border).BorderForeground(colorPrimary).Padding(1, 2).Width(contentWidth + 4).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(max(width, contentWidth+8), max(height, lipgloss.Height(panel)), lipgloss.Center, lipgloss.Center, panel)
}
