package dacode

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	maxMCPViewerServers     = 128
	maxMCPViewerTools       = 1024
	maxMCPViewerParameters  = 64
	maxMCPViewerQueryRunes  = 256
	maxMCPViewerInlineRunes = 200
	maxMCPViewerDetailRunes = 16 << 10
)

type mcpViewerServerStatus uint8

const (
	mcpViewerOK mcpViewerServerStatus = iota
	mcpViewerUnauthenticated
	mcpViewerAwaitingReconnect
	mcpViewerError
	mcpViewerDisabled
)

type mcpViewerParameter struct {
	Name     string
	Type     string
	Required bool
}

type mcpViewerTool struct {
	Name        string
	Description string
	Parameters  []mcpViewerParameter
}

type mcpViewerServer struct {
	Name             string
	Transport        string
	Tools            []mcpViewerTool
	Status           mcpViewerServerStatus
	Detail           string
	PendingReconnect bool
}

type mcpViewerOptions struct {
	Connecting       bool
	PendingReconnect bool
	ASCII            bool
}

type mcpViewerRowKind uint8

const (
	mcpViewerServerRow mcpViewerRowKind = iota
	mcpViewerToolRow
)

type mcpViewerRow struct {
	kind   mcpViewerRowKind
	server int
	tool   int
}

type mcpViewerToolKey struct {
	server int
	tool   int
}

type mcpViewerActionKind uint8

const (
	mcpViewerNoAction mcpViewerActionKind = iota
	mcpViewerClose
	mcpViewerLogin
	mcpViewerShowError
	mcpViewerReconnect
	mcpViewerToggleDisabled
)

type mcpViewerAction struct {
	Kind   mcpViewerActionKind
	Server string
	Detail string
}

func (action mcpViewerAction) String() string {
	return fmt.Sprintf("mcpViewerAction(kind=%d,server=%s,<detail redacted>)", action.Kind, singleLineMCPSafe(action.Server))
}

func (action mcpViewerAction) GoString() string { return action.String() }

type mcpViewerState struct {
	servers          []mcpViewerServer
	rows             []mcpViewerRow
	selected         int
	query            string
	expanded         map[mcpViewerToolKey]bool
	connecting       bool
	pendingReconnect bool
	ascii            bool
	truncated        bool
}

// newMCPViewerState builds an app-neutral viewer. Server metadata is required
// positionally; nil selects the useful empty state. Options are all optional,
// and construction performs no I/O.
func newMCPViewerState(servers []mcpViewerServer, options mcpViewerOptions) *mcpViewerState {
	state := &mcpViewerState{expanded: make(map[mcpViewerToolKey]bool), connecting: options.Connecting, ascii: options.ASCII}
	state.refresh(servers, options.PendingReconnect)
	return state
}

func (state *mcpViewerState) refresh(servers []mcpViewerServer, pendingReconnect bool) {
	if state == nil {
		panic("dacode: MCP viewer state is required")
	}
	if state.expanded == nil {
		state.expanded = make(map[mcpViewerToolKey]bool)
	}
	selectedServer, selectedTool := state.selectedIdentity()
	state.query = ""
	clear(state.expanded)
	normalized, truncated := normalizeMCPViewerServers(servers)
	state.servers = normalized
	state.pendingReconnect = pendingReconnect
	state.truncated = truncated
	state.rebuildRows()
	state.selectIdentity(selectedServer, selectedTool)
}

func normalizeMCPViewerServers(servers []mcpViewerServer) ([]mcpViewerServer, bool) {
	servers = append([]mcpViewerServer(nil), servers...)
	sort.SliceStable(servers, func(left, right int) bool {
		return mcpViewerAttentionPriority(normalizeMCPViewerStatus(servers[left].Status)) <
			mcpViewerAttentionPriority(normalizeMCPViewerStatus(servers[right].Status))
	})
	truncated := len(servers) > maxMCPViewerServers
	if len(servers) > maxMCPViewerServers {
		servers = servers[:maxMCPViewerServers]
	}
	normalized := make([]mcpViewerServer, 0, len(servers))
	toolBudget := maxMCPViewerTools
	for _, source := range servers {
		if toolBudget == 0 && len(source.Tools) > 0 {
			truncated = true
		}
		server := mcpViewerServer{
			Name:             boundedMCPSingleLine(source.Name, maxMCPViewerInlineRunes, "unnamed"),
			Transport:        boundedMCPSingleLine(source.Transport, maxMCPViewerInlineRunes, "unknown"),
			Status:           normalizeMCPViewerStatus(source.Status),
			Detail:           boundedMCPText(source.Detail, maxMCPViewerDetailRunes, false),
			PendingReconnect: source.PendingReconnect,
		}
		if server.Status == mcpViewerOK {
			server.Detail = ""
			limit := min(len(source.Tools), toolBudget)
			if limit < len(source.Tools) {
				truncated = true
			}
			server.Tools = make([]mcpViewerTool, 0, limit)
			for _, tool := range source.Tools[:limit] {
				normalizedTool, toolTruncated := normalizeMCPViewerTool(tool)
				server.Tools = append(server.Tools, normalizedTool)
				truncated = truncated || toolTruncated
			}
			toolBudget -= limit
		} else {
			server.Tools = nil
			if server.Detail == "" {
				server.Detail = defaultMCPViewerDetail(server.Status)
			}
		}
		if server.Status != mcpViewerDisabled {
			server.PendingReconnect = false
		}
		normalized = append(normalized, server)
	}
	return normalized, truncated
}

func normalizeMCPViewerTool(source mcpViewerTool) (mcpViewerTool, bool) {
	tool := mcpViewerTool{
		Name:        boundedMCPSingleLine(source.Name, maxMCPViewerInlineRunes, "unnamed tool"),
		Description: boundedMCPText(source.Description, maxMCPViewerDetailRunes, false),
	}
	limit := min(len(source.Parameters), maxMCPViewerParameters)
	tool.Parameters = make([]mcpViewerParameter, 0, limit)
	for _, parameter := range source.Parameters[:limit] {
		tool.Parameters = append(tool.Parameters, mcpViewerParameter{
			Name:     boundedMCPSingleLine(parameter.Name, 80, "unnamed"),
			Type:     boundedMCPSingleLine(parameter.Type, 80, "any"),
			Required: parameter.Required,
		})
	}
	return tool, len(source.Parameters) > maxMCPViewerParameters
}

func normalizeMCPViewerStatus(status mcpViewerServerStatus) mcpViewerServerStatus {
	switch status {
	case mcpViewerOK, mcpViewerUnauthenticated, mcpViewerAwaitingReconnect, mcpViewerError, mcpViewerDisabled:
		return status
	default:
		return mcpViewerError
	}
}

func defaultMCPViewerDetail(status mcpViewerServerStatus) string {
	switch status {
	case mcpViewerUnauthenticated:
		return "Authentication is required."
	case mcpViewerAwaitingReconnect:
		return "Ready to load after reconnect."
	case mcpViewerDisabled:
		return "Disabled by user."
	default:
		return "No error details were reported."
	}
}

func mcpViewerAttentionPriority(status mcpViewerServerStatus) int {
	switch status {
	case mcpViewerUnauthenticated:
		return 0
	case mcpViewerAwaitingReconnect:
		return 1
	default:
		return 2
	}
}

func (state *mcpViewerState) setFilter(query string) {
	if state == nil {
		return
	}
	state.query = boundedMCPSingleLine(query, maxMCPViewerQueryRunes, "")
	state.rebuildRows()
	state.selected = 0
	if len(state.rows) == 0 {
		state.selected = -1
	}
}

func (state *mcpViewerState) rebuildRows() {
	state.rows = state.rows[:0]
	tokens := strings.Fields(strings.ToLower(state.query))
	for serverIndex, server := range state.servers {
		tools, visible := visibleMCPViewerTools(server, tokens)
		if !visible {
			continue
		}
		state.rows = append(state.rows, mcpViewerRow{kind: mcpViewerServerRow, server: serverIndex, tool: -1})
		for _, toolIndex := range tools {
			state.rows = append(state.rows, mcpViewerRow{kind: mcpViewerToolRow, server: serverIndex, tool: toolIndex})
		}
	}
	if len(state.rows) == 0 {
		state.selected = -1
	} else if state.selected < 0 || state.selected >= len(state.rows) {
		state.selected = 0
	}
}

func visibleMCPViewerTools(server mcpViewerServer, tokens []string) ([]int, bool) {
	if len(tokens) == 0 {
		tools := make([]int, len(server.Tools))
		for index := range tools {
			tools[index] = index
		}
		return tools, true
	}
	serverName := strings.ToLower(server.Name)
	if containsAllMCPTokens(serverName, tokens) {
		if len(server.Tools) == 0 {
			return nil, false
		}
		tools := make([]int, len(server.Tools))
		for index := range tools {
			tools[index] = index
		}
		return tools, true
	}
	tools := make([]int, 0, len(server.Tools))
	for index, tool := range server.Tools {
		if containsAllMCPTokens(strings.ToLower(tool.Name), tokens) {
			tools = append(tools, index)
		}
	}
	return tools, len(tools) > 0
}

func containsAllMCPTokens(value string, tokens []string) bool {
	for _, token := range tokens {
		if !strings.Contains(value, token) {
			return false
		}
	}
	return true
}

func (state *mcpViewerState) selectedIdentity() (string, string) {
	row, ok := state.selectedRow()
	if !ok {
		return "", ""
	}
	server := state.servers[row.server]
	if row.kind == mcpViewerToolRow {
		return server.Name, server.Tools[row.tool].Name
	}
	return server.Name, ""
}

func (state *mcpViewerState) selectIdentity(serverName, toolName string) {
	state.selected = 0
	if len(state.rows) == 0 {
		state.selected = -1
		return
	}
	for index, row := range state.rows {
		server := state.servers[row.server]
		if server.Name != serverName {
			continue
		}
		if row.kind == mcpViewerServerRow && toolName == "" {
			state.selected = index
			return
		}
		if row.kind == mcpViewerToolRow && server.Tools[row.tool].Name == toolName {
			state.selected = index
			return
		}
	}
}

func (state *mcpViewerState) selectedRow() (mcpViewerRow, bool) {
	if state == nil || state.selected < 0 || state.selected >= len(state.rows) {
		return mcpViewerRow{}, false
	}
	return state.rows[state.selected], true
}

func (state *mcpViewerState) move(delta int) {
	if state == nil || len(state.rows) == 0 || delta == 0 {
		return
	}
	state.selected = (state.selected + delta) % len(state.rows)
	if state.selected < 0 {
		state.selected += len(state.rows)
	}
}

func (state *mcpViewerState) jumpServer(delta int) {
	if state == nil || len(state.rows) == 0 || delta == 0 {
		return
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	for offset := 1; offset <= len(state.rows); offset++ {
		index := (state.selected + step*offset) % len(state.rows)
		if index < 0 {
			index += len(state.rows)
		}
		if state.rows[index].kind == mcpViewerServerRow {
			state.selected = index
			return
		}
	}
}

func (state *mcpViewerState) handleKey(key string) mcpViewerAction {
	if state == nil {
		return mcpViewerAction{}
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "up":
		state.move(-1)
	case "down":
		state.move(1)
	case "shift+tab":
		state.jumpServer(-1)
	case "tab":
		state.jumpServer(1)
	case "enter":
		return state.activate()
	case "ctrl+e":
		state.toggleAll()
	case "ctrl+r":
		if state.pendingReconnect {
			return mcpViewerAction{Kind: mcpViewerReconnect}
		}
	case "f2":
		row, ok := state.selectedRow()
		if ok && row.kind == mcpViewerServerRow {
			return mcpViewerAction{Kind: mcpViewerToggleDisabled, Server: state.servers[row.server].Name}
		}
	case "esc", "escape":
		return mcpViewerAction{Kind: mcpViewerClose}
	}
	return mcpViewerAction{}
}

func (state *mcpViewerState) activate() mcpViewerAction {
	row, ok := state.selectedRow()
	if !ok {
		return mcpViewerAction{}
	}
	server := state.servers[row.server]
	if row.kind == mcpViewerToolRow {
		if state.expanded == nil {
			state.expanded = make(map[mcpViewerToolKey]bool)
		}
		key := mcpViewerToolKey{server: row.server, tool: row.tool}
		state.expanded[key] = !state.expanded[key]
		return mcpViewerAction{}
	}
	switch server.Status {
	case mcpViewerUnauthenticated:
		return mcpViewerAction{Kind: mcpViewerLogin, Server: server.Name}
	case mcpViewerError:
		return mcpViewerAction{Kind: mcpViewerShowError, Server: server.Name, Detail: server.Detail}
	default:
		return mcpViewerAction{}
	}
}

func (state *mcpViewerState) toggleAll() {
	if state == nil {
		return
	}
	keys := make([]mcpViewerToolKey, 0)
	anyCollapsed := false
	for _, row := range state.rows {
		if row.kind != mcpViewerToolRow {
			continue
		}
		key := mcpViewerToolKey{server: row.server, tool: row.tool}
		keys = append(keys, key)
		if !state.expanded[key] {
			anyCollapsed = true
		}
	}
	for _, key := range keys {
		state.expanded[key] = anyCollapsed
	}
}

func (state *mcpViewerState) render(width, height int) string {
	if state == nil {
		return ""
	}
	width = min(max(width, 24), 160)
	height = min(max(height, 6), 80)
	lines := []string{"MCP Servers", ""}
	rowBudget := max(height-4, 1)
	if len(state.servers) == 0 {
		if state.connecting {
			lines = append(lines, "Loading MCP tools...")
		} else {
			lines = append(lines, "No MCP servers configured.")
		}
	} else if len(state.rows) == 0 {
		lines = append(lines, "No matching tools.")
	} else {
		start := state.selected
		spaceBefore := max((rowBudget-min(len(state.renderRow(state.rows[state.selected], true)), rowBudget))/2, 0)
		usedBefore := 0
		for index := state.selected - 1; index >= 0; index-- {
			height := len(state.renderRow(state.rows[index], false))
			if usedBefore+height > spaceBefore {
				break
			}
			usedBefore += height
			start = index
		}
		for index := start; index < len(state.rows) && len(lines)-2 < rowBudget; index++ {
			rowLines := state.renderRow(state.rows[index], index == state.selected)
			for _, line := range rowLines {
				if len(lines)-2 >= rowBudget {
					break
				}
				lines = append(lines, line)
			}
		}
	}
	if state.truncated && len(lines)-2 < rowBudget {
		lines = append(lines, "Some servers or tools are hidden by display limits.")
	}
	lines = append(lines, "", state.helpText())
	for index := range lines {
		lines[index] = truncateMCPRunes(singleLineMCPSafe(lines[index]), width)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func (state *mcpViewerState) renderRow(row mcpViewerRow, selected bool) []string {
	server := state.servers[row.server]
	prefix := "  "
	if selected {
		prefix = "> "
	}
	if row.kind == mcpViewerServerRow {
		return []string{prefix + state.renderServerHeader(server)}
	}
	tool := server.Tools[row.tool]
	key := mcpViewerToolKey{server: row.server, tool: row.tool}
	if !state.expanded[key] {
		line := prefix + "  " + tool.Name
		if tool.Description != "" {
			line += " " + tool.Description
		}
		return []string{line}
	}
	lines := []string{prefix + "  " + tool.Name}
	if tool.Description != "" {
		lines = append(lines, "      "+tool.Description)
	}
	if len(tool.Parameters) > 0 {
		lines = append(lines, "      Parameters:")
		for _, parameter := range tool.Parameters {
			required := ""
			if parameter.Required {
				required = " *"
			}
			lines = append(lines, "        "+parameter.Name+": "+parameter.Type+required)
		}
	}
	return lines
}

func (state *mcpViewerState) renderServerHeader(server mcpViewerServer) string {
	glyphs := uiGlyphsForASCII(state.ascii)
	glyph, bullet, dash := mcpViewerGlyph(server.Status, state.ascii), glyphs.Bullet, glyphs.Dash
	base := fmt.Sprintf("%s %s %s", glyph, server.Name, server.Transport)
	switch server.Status {
	case mcpViewerOK:
		label := "tools"
		if len(server.Tools) == 1 {
			label = "tool"
		}
		return fmt.Sprintf("%s %s %d %s", base, bullet, len(server.Tools), label)
	case mcpViewerUnauthenticated:
		return fmt.Sprintf("%s %s unauthenticated %s Enter to log in", base, bullet, dash)
	case mcpViewerAwaitingReconnect:
		return fmt.Sprintf("%s %s ready to load %s Ctrl+R to load tools", base, bullet, dash)
	case mcpViewerError:
		return fmt.Sprintf("%s %s error %s Enter for details", base, bullet, dash)
	case mcpViewerDisabled:
		detail := truncateMCPRunes(singleLineMCPSafe(server.Detail), maxMCPViewerInlineRunes)
		if detail == "" {
			return fmt.Sprintf("%s %s disabled", base, bullet)
		}
		return fmt.Sprintf("%s %s disabled %s %s", base, bullet, dash, detail)
	default:
		return fmt.Sprintf("%s %s error", base, bullet)
	}
}

func mcpViewerGlyph(status mcpViewerServerStatus, ascii bool) string {
	glyphs := uiGlyphsForASCII(ascii)
	switch status {
	case mcpViewerOK:
		return glyphs.Checkmark
	case mcpViewerUnauthenticated:
		return glyphs.Warning
	case mcpViewerAwaitingReconnect:
		return glyphs.CircleEmpty
	case mcpViewerDisabled:
		return glyphs.Pause
	default:
		return glyphs.Error
	}
}

func (state *mcpViewerState) helpText() string {
	glyphs := uiGlyphsForASCII(state.ascii)
	separator := " " + glyphs.Bullet + " "
	parts := []string{glyphs.ArrowUp + "/" + glyphs.ArrowDown + " navigate", "Enter expand/login/details", "F2 disable/enable", "Ctrl+E expand all"}
	if state.ascii {
		parts[0] = "Up/Down navigate"
	}
	if state.pendingReconnect {
		parts = append(parts, "Ctrl+R reconnect")
	}
	parts = append(parts, "type to filter", "Esc close")
	return strings.Join(parts, separator)
}

func renderMCPViewerError(serverName, detail string, width, height int, ascii bool) string {
	width = min(max(width, 24), 160)
	height = min(max(height, 6), 80)
	name := boundedMCPSingleLine(serverName, maxMCPViewerInlineRunes, "unnamed")
	detail = boundedMCPText(detail, maxMCPViewerDetailRunes, true)
	if detail == "" {
		detail = "No error details were reported."
	}
	separator := " " + uiGlyphsForASCII(ascii).Bullet + " "
	lines := []string{"MCP Server Error: " + name, ""}
	for _, line := range strings.Split(detail, "\n") {
		lines = append(lines, line)
		if len(lines) >= height-2 {
			break
		}
	}
	lines = append(lines, "", "c copy error"+separator+"Esc close")
	for index := range lines {
		lines[index] = truncateMCPRunes(singleLineMCPSafe(lines[index]), width)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

type mcpReconnectPromptKind uint8

const (
	mcpReconnectAfterLogin mcpReconnectPromptKind = iota
	mcpReconnectApplyChanges
	mcpReconnectForce
)

type mcpReconnectChoice uint8

const (
	mcpReconnectNoChoice mcpReconnectChoice = iota
	mcpReconnectNow
	mcpReconnectLater
	mcpReconnectCancel
)

type mcpReconnectPromptState struct {
	kind    mcpReconnectPromptKind
	servers []string
	ascii   bool
}

func newMCPReconnectPrompt(kind mcpReconnectPromptKind, serverNames []string, ascii bool) *mcpReconnectPromptState {
	if kind > mcpReconnectForce {
		panic("dacode: invalid MCP reconnect prompt kind")
	}
	limit := min(len(serverNames), maxMCPViewerServers)
	servers := make([]string, 0, limit)
	for _, name := range serverNames[:limit] {
		servers = append(servers, boundedMCPSingleLine(name, maxMCPViewerInlineRunes, "unnamed"))
	}
	return &mcpReconnectPromptState{kind: kind, servers: servers, ascii: ascii}
}

func (state *mcpReconnectPromptState) handleKey(key string) mcpReconnectChoice {
	if state == nil {
		return mcpReconnectNoChoice
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "enter":
		return mcpReconnectNow
	case "esc", "escape":
		if state.kind == mcpReconnectForce {
			return mcpReconnectCancel
		}
		return mcpReconnectLater
	default:
		return mcpReconnectNoChoice
	}
}

func (state *mcpReconnectPromptState) render(width, height int) string {
	if state == nil {
		return ""
	}
	width = min(max(width, 24), 160)
	height = min(max(height, 6), 20)
	title, body, help := state.copy()
	lines := []string{title, "", body, "", help}
	for index := range lines {
		lines[index] = truncateMCPRunes(singleLineMCPSafe(lines[index]), width)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func (state *mcpReconnectPromptState) copy() (string, string, string) {
	separator := ", "
	names := strings.Join(state.servers, separator)
	switch state.kind {
	case mcpReconnectApplyChanges:
		if names == "" {
			names = "the selected servers"
		}
		return "Apply MCP server changes?", "Reconnect to apply the changes to " + names + ".", "Enter to reconnect, Esc to defer"
	case mcpReconnectForce:
		return "Force reconnect?", "No MCP login is queued. Restart will drop the current session and reload all servers.", "Enter to restart, Esc to cancel"
	default:
		name := names
		if name == "" {
			name = "MCP server"
		}
		check := uiGlyphsForASCII(state.ascii).Checkmark
		return check + " Connected to " + name, "Reconnect to load new tools.", "Enter to reconnect, Esc to defer"
	}
}

func boundedMCPSingleLine(value string, limit int, fallback string) string {
	value = singleLineMCPSafe(strings.TrimSpace(value))
	value = truncateMCPRunes(value, limit)
	if value == "" {
		return fallback
	}
	return value
}

func boundedMCPText(value string, limit int, keepNewlines bool) string {
	value = unicodesecurity.RenderTerminalSafe(strings.TrimSpace(value))
	if !keepNewlines {
		value = strings.ReplaceAll(value, "\n", " ")
	}
	return truncateMCPRunes(value, limit)
}

func singleLineMCPSafe(value string) string {
	return strings.ReplaceAll(unicodesecurity.RenderTerminalSafe(value), "\n", " ")
}

func truncateMCPRunes(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	runes := []rune(value)
	if ansi.StringWidth(value) <= limit && len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	if len(runes) > limit {
		value = string(runes[:limit-3]) + "..."
	}
	return ansi.Truncate(value, limit, "...")
}
