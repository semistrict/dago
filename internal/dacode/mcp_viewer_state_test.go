package dacode

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

func TestMCPViewerZeroAndConnectingStatesAreUseful(t *testing.T) {
	state := newMCPViewerState(nil, mcpViewerOptions{})
	if got := state.render(80, 20); !strings.Contains(got, "No MCP servers configured") {
		t.Fatalf("empty viewer = %q", got)
	}
	connecting := newMCPViewerState(nil, mcpViewerOptions{Connecting: true})
	if got := connecting.render(80, 20); !strings.Contains(got, "Loading MCP tools") {
		t.Fatalf("connecting viewer = %q", got)
	}
	var zero mcpViewerState
	if action := zero.handleKey("enter"); action.Kind != mcpViewerNoAction {
		t.Fatalf("zero viewer action = %#v", action)
	}
	if got := zero.render(0, 0); !strings.Contains(got, "No MCP servers") {
		t.Fatalf("zero viewer render = %q", got)
	}
}

func TestMCPViewerZeroStateCanRefreshAndExpand(t *testing.T) {
	var state mcpViewerState
	state.refresh([]mcpViewerServer{{Name: "server", Tools: []mcpViewerTool{{Name: "tool"}}}}, false)
	state.move(1)
	if action := state.activate(); action.Kind != mcpViewerNoAction {
		t.Fatalf("tool activation = %#v", action)
	}
	row, ok := state.selectedRow()
	if !ok || row.kind != mcpViewerToolRow || !state.expanded[mcpViewerToolKey{server: row.server, tool: row.tool}] {
		t.Fatalf("zero-state expansion = rows %#v, selected %d, expanded %#v", state.rows, state.selected, state.expanded)
	}
}

func TestMCPViewerNormalizesBoundsAndPromotesAttentionRows(t *testing.T) {
	tools := make([]mcpViewerTool, maxMCPViewerTools+1)
	for index := range tools {
		tools[index] = mcpViewerTool{Name: fmt.Sprintf("tool_%04d", index)}
	}
	tools[0].Parameters = make([]mcpViewerParameter, maxMCPViewerParameters+1)
	servers := []mcpViewerServer{
		{Name: "healthy", Transport: "stdio", Status: mcpViewerOK, Tools: tools},
		{Name: "broken", Transport: "http", Status: mcpViewerError},
		{Name: "login", Transport: "http", Status: mcpViewerUnauthenticated, Tools: []mcpViewerTool{{Name: "must-drop"}}},
		{Name: "waiting", Transport: "sse", Status: mcpViewerAwaitingReconnect},
		{Name: "disabled", Status: mcpViewerDisabled, PendingReconnect: true},
		{Name: "invalid", Status: mcpViewerServerStatus(99)},
	}
	state := newMCPViewerState(servers, mcpViewerOptions{})
	if !state.truncated || len(state.servers[2].Tools) > maxMCPViewerTools {
		t.Fatalf("normalization limits were not applied: truncated=%t", state.truncated)
	}
	if state.servers[0].Name != "login" || state.servers[1].Name != "waiting" {
		t.Fatalf("attention order = %q, %q", state.servers[0].Name, state.servers[1].Name)
	}
	login := findMCPViewerServer(t, state.servers, "login")
	if len(login.Tools) != 0 || login.Detail == "" {
		t.Fatalf("unauthenticated normalization = %#v", login)
	}
	disabled := findMCPViewerServer(t, state.servers, "disabled")
	if !disabled.PendingReconnect || disabled.Transport != "unknown" {
		t.Fatalf("disabled normalization = %#v", disabled)
	}
	invalid := findMCPViewerServer(t, state.servers, "invalid")
	if invalid.Status != mcpViewerError || invalid.Detail == "" {
		t.Fatalf("invalid status did not fail closed: %#v", invalid)
	}
	healthy := findMCPViewerServer(t, state.servers, "healthy")
	if len(healthy.Tools) != maxMCPViewerTools || len(healthy.Tools[0].Parameters) != maxMCPViewerParameters {
		t.Fatalf("tool bounds = tools:%d params:%d", len(healthy.Tools), len(healthy.Tools[0].Parameters))
	}
}

func TestMCPViewerServerLimitIsFiniteAndStable(t *testing.T) {
	servers := make([]mcpViewerServer, maxMCPViewerServers+20)
	for index := range servers {
		servers[index] = mcpViewerServer{Name: fmt.Sprintf("server_%03d", index)}
	}
	state := newMCPViewerState(servers, mcpViewerOptions{})
	if len(state.servers) != maxMCPViewerServers || !state.truncated {
		t.Fatalf("server bounds = %d, truncated=%t", len(state.servers), state.truncated)
	}
	if state.servers[0].Name != "server_000" || state.servers[len(state.servers)-1].Name != "server_127" {
		t.Fatalf("stable bounded order = %q ... %q", state.servers[0].Name, state.servers[len(state.servers)-1].Name)
	}
}

func TestMCPViewerServerLimitKeepsLateAttentionRows(t *testing.T) {
	servers := make([]mcpViewerServer, maxMCPViewerServers+1)
	for index := range servers {
		servers[index] = mcpViewerServer{Name: fmt.Sprintf("healthy_%03d", index)}
	}
	servers[len(servers)-1] = mcpViewerServer{Name: "login-needed", Status: mcpViewerUnauthenticated}
	state := newMCPViewerState(servers, mcpViewerOptions{})
	if len(state.servers) != maxMCPViewerServers || state.servers[0].Name != "login-needed" {
		t.Fatalf("bounded attention order starts with %q across %d servers", state.servers[0].Name, len(state.servers))
	}
}

func TestMCPViewerFilterMatchesOnlyServerAndToolNames(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{
		{Name: "alpha server", Tools: []mcpViewerTool{{Name: "read_file", Description: "secret needle"}, {Name: "write_file"}}},
		{Name: "beta", Tools: []mcpViewerTool{{Name: "alpha_search"}}},
		{Name: "empty alpha"},
	}, mcpViewerOptions{})
	state.setFilter("alpha")
	if len(state.rows) != 5 {
		t.Fatalf("alpha rows = %#v", state.rows)
	}
	if row := state.rows[len(state.rows)-1]; state.servers[row.server].Name != "beta" || row.kind != mcpViewerToolRow {
		t.Fatalf("tool-name match row = %#v", row)
	}
	state.setFilter("needle")
	if len(state.rows) != 0 || !strings.Contains(state.render(80, 20), "No matching tools") {
		t.Fatalf("description unexpectedly matched filter: %#v", state.rows)
	}
	state.setFilter("alpha server")
	if len(state.rows) != 3 {
		t.Fatalf("multi-token server match rows = %#v", state.rows)
	}
	state.setFilter(strings.Repeat("x", maxMCPViewerQueryRunes+20) + "\x1b")
	if utf8.RuneCountInString(state.query) > maxMCPViewerQueryRunes || strings.ContainsRune(state.query, '\x1b') {
		t.Fatalf("unsafe unbounded query = %q", state.query)
	}
}

func TestMCPViewerNavigationWrapsAndJumpsBetweenHeaders(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{
		{Name: "one", Tools: []mcpViewerTool{{Name: "a"}, {Name: "b"}}},
		{Name: "two", Tools: []mcpViewerTool{{Name: "c"}}},
	}, mcpViewerOptions{})
	if len(state.rows) != 5 {
		t.Fatalf("row count = %d", len(state.rows))
	}
	state.handleKey("up")
	if state.selected != 4 {
		t.Fatalf("wrapped up selection = %d", state.selected)
	}
	state.handleKey("down")
	if state.selected != 0 {
		t.Fatalf("wrapped down selection = %d", state.selected)
	}
	state.selected = 2
	state.handleKey("shift+tab")
	if state.selected != 0 {
		t.Fatalf("jumped up selection = %d", state.selected)
	}
	state.handleKey("tab")
	if state.selected != 3 {
		t.Fatalf("jumped down selection = %d", state.selected)
	}
	state.handleKey("tab")
	if state.selected != 0 {
		t.Fatalf("wrapped server jump = %d", state.selected)
	}
}

func TestMCPViewerRefreshPreservesSelectedIdentity(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{
		{Name: "one", Tools: []mcpViewerTool{{Name: "a"}}},
		{Name: "two", Tools: []mcpViewerTool{{Name: "b"}}},
	}, mcpViewerOptions{})
	state.selected = 3
	state.query = "old filter"
	state.expanded[mcpViewerToolKey{server: 1, tool: 0}] = true
	state.refresh([]mcpViewerServer{
		{Name: "attention", Status: mcpViewerUnauthenticated},
		{Name: "one", Tools: []mcpViewerTool{{Name: "a"}}},
		{Name: "two", Tools: []mcpViewerTool{{Name: "b"}}},
	}, true)
	server, tool := state.selectedIdentity()
	if server != "two" || tool != "b" || !state.pendingReconnect {
		t.Fatalf("refreshed identity = %q/%q, pending=%t", server, tool, state.pendingReconnect)
	}
	if state.query != "" || len(state.expanded) != 0 {
		t.Fatalf("refresh retained stale filter/expansion: query=%q expanded=%v", state.query, state.expanded)
	}
}

func TestMCPViewerEnterEmitsLoginErrorOrExpansionBehavior(t *testing.T) {
	secretDetail := "private error detail"
	state := newMCPViewerState([]mcpViewerServer{
		{Name: "login", Status: mcpViewerUnauthenticated},
		{Name: "broken", Status: mcpViewerError, Detail: secretDetail},
		{Name: "healthy", Tools: []mcpViewerTool{{Name: "tool"}}},
	}, mcpViewerOptions{})
	if action := state.handleKey("enter"); action.Kind != mcpViewerLogin || action.Server != "login" {
		t.Fatalf("login action = %#v", action)
	}
	state.jumpServer(1)
	action := state.handleKey("enter")
	if action.Kind != mcpViewerShowError || action.Server != "broken" || action.Detail != secretDetail {
		t.Fatalf("error action = %#v", action)
	}
	for _, rendered := range []string{fmt.Sprint(action), fmt.Sprintf("%#v", action)} {
		if strings.Contains(rendered, secretDetail) {
			t.Fatalf("formatted action leaked details: %q", rendered)
		}
	}
	state.jumpServer(1)
	if action := state.handleKey("enter"); action.Kind != mcpViewerNoAction {
		t.Fatalf("healthy header action = %#v", action)
	}
	state.handleKey("down")
	if action := state.handleKey("enter"); action.Kind != mcpViewerNoAction {
		t.Fatalf("tool activation action = %#v", action)
	}
	row, _ := state.selectedRow()
	if !state.expanded[mcpViewerToolKey{server: row.server, tool: row.tool}] {
		t.Fatal("tool was not expanded")
	}
}

func TestMCPViewerToggleAllAffectsOnlyVisibleTools(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{{Name: "server", Tools: []mcpViewerTool{
		{Name: "visible_one"}, {Name: "visible_two"}, {Name: "hidden"},
	}}}, mcpViewerOptions{})
	state.setFilter("visible")
	state.handleKey("ctrl+e")
	if !state.expanded[mcpViewerToolKey{server: 0, tool: 0}] || !state.expanded[mcpViewerToolKey{server: 0, tool: 1}] {
		t.Fatal("visible tools were not expanded")
	}
	if state.expanded[mcpViewerToolKey{server: 0, tool: 2}] {
		t.Fatal("hidden tool was expanded")
	}
	state.handleKey("ctrl+e")
	if state.expanded[mcpViewerToolKey{server: 0, tool: 0}] || state.expanded[mcpViewerToolKey{server: 0, tool: 1}] {
		t.Fatal("visible tools were not collapsed")
	}
}

func TestMCPViewerReconnectToggleAndCloseActionsAreGuarded(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{{Name: "server", Tools: []mcpViewerTool{{Name: "tool"}}}}, mcpViewerOptions{})
	if action := state.handleKey("ctrl+r"); action.Kind != mcpViewerNoAction {
		t.Fatalf("unguarded reconnect action = %#v", action)
	}
	state.pendingReconnect = true
	if action := state.handleKey("ctrl+r"); action.Kind != mcpViewerReconnect {
		t.Fatalf("reconnect action = %#v", action)
	}
	if action := state.handleKey("f2"); action.Kind != mcpViewerToggleDisabled || action.Server != "server" {
		t.Fatalf("header toggle action = %#v", action)
	}
	state.handleKey("down")
	if action := state.handleKey("f2"); action.Kind != mcpViewerNoAction {
		t.Fatalf("tool toggle action = %#v", action)
	}
	if action := state.handleKey("esc"); action.Kind != mcpViewerClose {
		t.Fatalf("close action = %#v", action)
	}
}

func TestMCPViewerRenderingIsBoundedTerminalSafeAndASCIIAware(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{
		{Name: "unsafe\x1b[31m\nserver", Status: mcpViewerDisabled, Detail: "disabled\r\nreason"},
		{Name: "healthy", Transport: "stdio", Tools: []mcpViewerTool{{
			Name: "tool", Description: strings.Repeat("description", 100),
			Parameters: []mcpViewerParameter{{Name: "path\nname", Type: "string", Required: true}},
		}}},
	}, mcpViewerOptions{PendingReconnect: true})
	unicodeView := state.render(48, 10)
	if strings.ContainsRune(unicodeView, '\x1b') || !strings.Contains(unicodeView, "<U+001B CONTROL>") || !strings.Contains(unicodeView, "⏸") {
		t.Fatalf("unicode terminal-safe view = %q", unicodeView)
	}
	state.ascii = true
	asciiView := state.render(48, 10)
	for _, forbidden := range []string{"⏸", "•", "—", "↑", "↓", "…"} {
		if strings.Contains(asciiView, forbidden) {
			t.Fatalf("ASCII view contains %q: %q", forbidden, asciiView)
		}
	}
	if !strings.Contains(asciiView, "[||]") || !strings.Contains(state.helpText(), "Ctrl+R") {
		t.Fatalf("ASCII status/help = %q", asciiView)
	}
	assertMCPRenderBounds(t, asciiView, 48, 10)
}

func TestMCPViewerExpandedToolAndErrorDetailsAreSafeAndBounded(t *testing.T) {
	state := newMCPViewerState([]mcpViewerServer{{Name: "server", Tools: []mcpViewerTool{{
		Name: "tool", Description: "full description",
		Parameters: []mcpViewerParameter{{Name: "path", Type: "string", Required: true}},
	}}}}, mcpViewerOptions{})
	state.handleKey("down")
	state.handleKey("enter")
	view := state.render(80, 12)
	for _, expected := range []string{"full description", "Parameters:", "path: string *"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("expanded view missing %q: %q", expected, view)
		}
	}
	detail := renderMCPViewerError("bad\x1bserver", "first line\nsecond\x00line", 40, 8, true)
	if strings.ContainsRune(detail, '\x1b') || strings.ContainsRune(detail, '\x00') || !strings.Contains(detail, "<U+001B CONTROL>") || !strings.Contains(detail, "<U+0000 CONTROL>") {
		t.Fatalf("unsafe error detail = %q", detail)
	}
	if !strings.Contains(detail, "c copy error - Esc close") {
		t.Fatalf("ASCII error help = %q", detail)
	}
	assertMCPRenderBounds(t, detail, 40, 8)
}

func TestMCPReconnectPromptsReturnExplicitChoices(t *testing.T) {
	login := newMCPReconnectPrompt(mcpReconnectAfterLogin, []string{"alpha"}, false)
	if got := login.render(80, 10); !strings.Contains(got, "✓ Connected to alpha") || !strings.Contains(got, "Esc to defer") {
		t.Fatalf("login prompt = %q", got)
	}
	if choice := login.handleKey("enter"); choice != mcpReconnectNow {
		t.Fatalf("login Enter choice = %d", choice)
	}
	if choice := login.handleKey("esc"); choice != mcpReconnectLater {
		t.Fatalf("login Esc choice = %d", choice)
	}
	changes := newMCPReconnectPrompt(mcpReconnectApplyChanges, []string{"alpha", "beta"}, true)
	if got := changes.render(80, 10); !strings.Contains(got, "alpha, beta") || !strings.Contains(got, "Esc to defer") {
		t.Fatalf("changes prompt = %q", got)
	}
	force := newMCPReconnectPrompt(mcpReconnectForce, nil, true)
	if got := force.render(80, 10); !strings.Contains(got, "Force reconnect?") || !strings.Contains(got, "Esc to cancel") {
		t.Fatalf("force prompt = %q", got)
	}
	if choice := force.handleKey("esc"); choice != mcpReconnectCancel {
		t.Fatalf("force Esc choice = %d", choice)
	}
	if choice := force.handleKey("x"); choice != mcpReconnectNoChoice {
		t.Fatalf("unknown key choice = %d", choice)
	}
}

func TestMCPReconnectPromptRenderingIsBoundedAndTerminalSafe(t *testing.T) {
	names := make([]string, maxMCPViewerServers+10)
	for index := range names {
		names[index] = strings.Repeat("n", maxMCPViewerInlineRunes+10)
	}
	names[0] = "unsafe\x1b\nname"
	prompt := newMCPReconnectPrompt(mcpReconnectApplyChanges, names, true)
	if len(prompt.servers) != maxMCPViewerServers {
		t.Fatalf("bounded prompt servers = %d", len(prompt.servers))
	}
	view := prompt.render(36, 6)
	if strings.ContainsRune(view, '\x1b') || !strings.Contains(prompt.servers[0], "<U+001B CONTROL>") {
		t.Fatalf("unsafe reconnect prompt = %q", view)
	}
	assertMCPRenderBounds(t, view, 36, 6)
	assertPanicsMCPViewer(t, func() { newMCPReconnectPrompt(mcpReconnectPromptKind(99), nil, false) })
}

func findMCPViewerServer(t *testing.T, servers []mcpViewerServer, name string) mcpViewerServer {
	t.Helper()
	for _, server := range servers {
		if server.Name == name {
			return server
		}
	}
	t.Fatalf("missing server %q", name)
	return mcpViewerServer{}
}

func assertMCPRenderBounds(t *testing.T, rendered string, width, height int) {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	if len(lines) > height {
		t.Fatalf("rendered lines = %d, want <= %d", len(lines), height)
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) > width || ansi.StringWidth(line) > width {
			t.Fatalf("line exceeds width %d (runes=%d, cells=%d): %q", width, utf8.RuneCountInString(line), ansi.StringWidth(line), line)
		}
	}
}

func assertPanicsMCPViewer(t *testing.T, operation func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("operation did not panic")
		}
	}()
	operation()
}
