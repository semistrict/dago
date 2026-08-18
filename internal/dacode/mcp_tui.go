package dacode

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	talonmcp "github.com/semistrict/dago/datalon/mcp"
	"github.com/semistrict/dago/datalon/mcp/oauthpolicy"
)

const mcpTUIOperationTimeout = 2 * time.Minute

type mcpLoginPhase uint8

const (
	mcpLoginPreparing mcpLoginPhase = iota
	mcpLoginAuthorize
	mcpLoginWorkspace
	mcpLoginDevice
	mcpLoginWaiting
	mcpLoginSucceeded
	mcpLoginCancelled
	mcpLoginFailed
)

type mcpLoginEvent struct {
	generation uint64
	phase      mcpLoginPhase
	url        string
	userCode   string
}

type mcpLoginState struct {
	generation uint64
	server     string
	phase      mcpLoginPhase
	url        string
	displayURL string
	userCode   string
	input      []byte
	flow       *mcpLoginFlow
}

type mcpLoginFlow struct {
	Events    <-chan mcpLoginEvent
	responses chan<- string
	cancel    context.CancelFunc
	once      sync.Once
}

func (flow *mcpLoginFlow) Cancel() {
	if flow != nil && flow.cancel != nil {
		flow.once.Do(flow.cancel)
	}
}

func (flow *mcpLoginFlow) Respond(value string) bool {
	if flow == nil || flow.responses == nil || len(value) > mcpLoginInputLimit || !utf8.ValidString(value) {
		return false
	}
	select {
	case flow.responses <- value:
		return true
	default:
		return false
	}
}

type mcpTUIInteraction struct {
	ctx        context.Context
	generation uint64
	events     chan<- mcpLoginEvent
	responses  <-chan string
}

func (interaction *mcpTUIInteraction) Authorize(ctx context.Context, authorizationURL string) (string, error) {
	full, display, err := validateMCPAuthorizeURL(authorizationURL)
	if err != nil {
		return "", err
	}
	if !interaction.emit(ctx, mcpLoginEvent{generation: interaction.generation, phase: mcpLoginAuthorize, url: full, userCode: display}) {
		return "", context.Canceled
	}
	return interaction.waitResponse(ctx)
}

func (interaction *mcpTUIInteraction) SelectSlackWorkspace(ctx context.Context) (string, error) {
	if !interaction.emit(ctx, mcpLoginEvent{generation: interaction.generation, phase: mcpLoginWorkspace}) {
		return "", context.Canceled
	}
	return interaction.waitResponse(ctx)
}

func (interaction *mcpTUIInteraction) PresentDeviceCode(ctx context.Context, device oauthpolicy.DeviceCode) error {
	full, display, err := validateMCPAuthorizeURL(device.VerificationURI)
	if err != nil {
		return err
	}
	code := boundedMCPSingleLine(device.UserCode, 128, "")
	if code == "" {
		return errors.New("MCP device code is unavailable")
	}
	if !interaction.emit(ctx, mcpLoginEvent{generation: interaction.generation, phase: mcpLoginDevice, url: full, userCode: display + " | code " + code}) {
		return context.Canceled
	}
	return nil
}

func (interaction *mcpTUIInteraction) emit(ctx context.Context, event mcpLoginEvent) bool {
	select {
	case interaction.events <- event:
		return true
	case <-ctx.Done():
		return false
	case <-interaction.ctx.Done():
		return false
	}
}

func (interaction *mcpTUIInteraction) waitResponse(ctx context.Context) (string, error) {
	select {
	case response := <-interaction.responses:
		return strings.TrimSpace(response), nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-interaction.ctx.Done():
		return "", interaction.ctx.Err()
	}
}

func startMCPLoginFlow(ctx context.Context, generation uint64, server string, controller mcpRuntimeController) *mcpLoginFlow {
	if ctx == nil || generation == 0 || strings.TrimSpace(server) == "" || controller == nil {
		panic("dacode: MCP login flow dependencies are required")
	}
	flowContext, cancel := context.WithTimeout(ctx, mcpTUIOperationTimeout)
	events := make(chan mcpLoginEvent, 4)
	responses := make(chan string, 1)
	flow := &mcpLoginFlow{Events: events, responses: responses, cancel: cancel}
	interaction := &mcpTUIInteraction{ctx: flowContext, generation: generation, events: events, responses: responses}
	go func() {
		defer close(events)
		defer flow.Cancel()
		err := controller.LoginMCP(flowContext, server, interaction)
		phase := mcpLoginSucceeded
		if err != nil {
			phase = mcpLoginFailed
			if flowContext.Err() != nil {
				phase = mcpLoginCancelled
			}
		}
		select {
		case events <- mcpLoginEvent{generation: generation, phase: phase}:
		case <-flowContext.Done():
		}
	}()
	return flow
}

type mcpLoginEventMsg struct {
	flow   *mcpLoginFlow
	event  mcpLoginEvent
	closed bool
}

type mcpRuntimeMsg struct {
	action     string
	server     string
	servers    []mcpViewerServer
	pending    bool
	err        error
	generation uint64
}

func (model *tuiModel) configureMCP(controller mcpRuntimeController) {
	if controller == nil {
		panic("dacode: MCP runtime controller is required")
	}
	model.mcpController = controller
}

func (model *tuiModel) openMCPViewer() tea.Cmd {
	if model.mcpController == nil {
		model.appendItem(transcriptItem{kind: itemError, text: "MCP management is unavailable."})
		model.refreshTranscript()
		return nil
	}
	model.mcpViewer = newMCPViewerState(nil, mcpViewerOptions{Connecting: true, ASCII: model.charset == charsetASCII})
	return model.refreshMCPViewer()
}

func (model *tuiModel) refreshMCPViewer() tea.Cmd {
	controller := model.mcpController
	if controller == nil {
		return nil
	}
	generation := model.operationGeneration
	return func() tea.Msg {
		servers, pending, err := controller.SnapshotMCP()
		return mcpRuntimeMsg{action: "snapshot", servers: servers, pending: pending, err: err, generation: generation}
	}
}

func (model *tuiModel) handleMCPViewerKey(message tea.KeyMsg) tea.Cmd {
	state := model.mcpViewer
	if state == nil {
		return nil
	}
	key := message.String()
	if message.Type == tea.KeyRunes && len(message.Runes) > 0 {
		state.setFilter(state.query + string(message.Runes))
		return nil
	}
	if key == "backspace" || key == "delete" {
		runes := []rune(state.query)
		if len(runes) > 0 {
			state.setFilter(string(runes[:len(runes)-1]))
		}
		return nil
	}
	action := state.handleKey(key)
	switch action.Kind {
	case mcpViewerClose:
		model.mcpViewer = nil
		return model.drainInputQueue()
	case mcpViewerLogin:
		return model.startMCPLogin(action.Server)
	case mcpViewerShowError:
		model.mcpErrorServer, model.mcpErrorDetail = action.Server, action.Detail
	case mcpViewerReconnect:
		model.mcpReconnectPrompt = newMCPReconnectPrompt(mcpReconnectApplyChanges, nil, model.charset == charsetASCII)
	case mcpViewerToggleDisabled:
		return model.toggleMCPServer(action.Server)
	}
	return nil
}

func (model *tuiModel) startMCPLogin(server string) tea.Cmd {
	if model.mcpController == nil {
		return nil
	}
	if model.mcpLogin != nil && model.mcpLogin.flow != nil {
		model.mcpLogin.flow.Cancel()
	}
	model.mcpLoginGeneration++
	if model.mcpLoginGeneration == 0 {
		model.mcpLoginGeneration = 1
	}
	state := &mcpLoginState{generation: model.mcpLoginGeneration, server: boundedMCPSingleLine(server, maxMCPViewerInlineRunes, "MCP server"), phase: mcpLoginPreparing}
	state.flow = startMCPLoginFlow(model.ctx, state.generation, server, model.mcpController)
	model.mcpLogin = state
	return waitMCPLoginEvent(state.flow)
}

func waitMCPLoginEvent(flow *mcpLoginFlow) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-flow.Events
		return mcpLoginEventMsg{flow: flow, event: event, closed: !ok}
	}
}

func (model *tuiModel) handleMCPLoginEvent(message mcpLoginEventMsg) tea.Cmd {
	state := model.mcpLogin
	if state == nil || state.flow != message.flow {
		return nil
	}
	if message.closed {
		state.flow = nil
		if state.phase != mcpLoginSucceeded && state.phase != mcpLoginFailed && state.phase != mcpLoginCancelled {
			state.phase = mcpLoginCancelled
		}
		clear(state.input)
		state.input = nil
		return nil
	}
	if message.event.generation != state.generation {
		return nil
	}
	state.phase = message.event.phase
	state.input = nil
	if message.event.url != "" {
		state.url = message.event.url
		state.displayURL = message.event.userCode
	}
	if message.event.phase == mcpLoginSucceeded {
		server := state.server
		state.flow = nil
		model.mcpLogin = nil
		model.mcpReconnectPrompt = newMCPReconnectPrompt(mcpReconnectAfterLogin, []string{server}, model.charset == charsetASCII)
		return nil
	}
	return waitMCPLoginEvent(message.flow)
}

func (model *tuiModel) handleMCPLoginKey(message tea.KeyMsg) tea.Cmd {
	state := model.mcpLogin
	if state == nil {
		return nil
	}
	// Once callback entry has started, every printable rune is callback data.
	// In particular, a literal "c" in a callback must not be mistaken for the
	// copy-URL shortcut. The shortcut remains available before entry begins.
	if message.Type == tea.KeyRunes && len(message.Runes) > 0 && (state.input != nil || (message.String() != "c" && message.String() != "C")) {
		text := string(message.Runes)
		if validMCPLoginInput(text) && len(state.input)+len(text) <= mcpLoginInputLimit {
			state.input = append(state.input, text...)
		}
		return nil
	}
	switch message.String() {
	case "esc":
		if state.flow != nil {
			state.flow.Cancel()
		}
		clear(state.input)
		model.mcpLogin = nil
		return nil
	case "c", "C":
		if state.url != "" && (state.phase == mcpLoginAuthorize || state.phase == mcpLoginDevice) {
			return model.stageTerminalSequences(osc52ClipboardSequence(state.url), "")
		}
	case "backspace", "delete":
		if len(state.input) > 0 {
			_, size := utf8.DecodeLastRune(state.input)
			clear(state.input[len(state.input)-size:])
			state.input = state.input[:len(state.input)-size]
		}
	case "enter":
		if state.phase == mcpLoginFailed || state.phase == mcpLoginCancelled {
			model.mcpLogin = nil
			return nil
		}
		if state.phase == mcpLoginAuthorize || state.phase == mcpLoginWorkspace {
			value := strings.TrimSpace(string(state.input))
			if state.phase == mcpLoginAuthorize && value == "" {
				return nil
			}
			if !state.flow.Respond(value) {
				return nil
			}
			clear(state.input)
			state.input = nil
			state.phase = mcpLoginWaiting
		}
	}
	return nil
}

func validMCPLoginInput(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (model *tuiModel) toggleMCPServer(server string) tea.Cmd {
	controller := model.mcpController
	generation := model.operationGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(model.ctx, 30*time.Second)
		defer cancel()
		err := controller.ToggleMCPDisabled(ctx, server)
		return mcpRuntimeMsg{action: "toggle", server: server, err: err, generation: generation}
	}
}

func (model *tuiModel) reconnectMCP() tea.Cmd {
	controller := model.mcpController
	generation := model.operationGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(model.ctx, mcpTUIOperationTimeout)
		defer cancel()
		err := controller.ReconnectMCP(ctx)
		return mcpRuntimeMsg{action: "reconnect", err: err, generation: generation}
	}
}

func (model *tuiModel) handleMCPRuntime(message mcpRuntimeMsg) tea.Cmd {
	if message.err != nil {
		if message.action == "snapshot" {
			model.mcpViewer = nil
		}
		model.appendItem(transcriptItem{kind: itemError, text: "MCP " + boundedMCPSingleLine(message.action, 32, "operation") + " failed."})
		model.refreshTranscript()
		return nil
	}
	switch message.action {
	case "snapshot":
		if model.mcpViewer != nil {
			model.mcpViewer.connecting = false
			model.mcpViewer.refresh(message.servers, message.pending)
		}
	case "toggle":
		model.mcpReconnectPrompt = newMCPReconnectPrompt(mcpReconnectApplyChanges, []string{message.server}, model.charset == charsetASCII)
		return model.refreshMCPViewer()
	case "reconnect":
		model.mcpReconnectPrompt = nil
		model.appendItem(transcriptItem{kind: itemNotice, text: "MCP servers reconnected."})
		model.refreshTranscript()
		return model.refreshMCPViewer()
	}
	return nil
}

func (model *tuiModel) handleMCPReconnectKey(message tea.KeyMsg) tea.Cmd {
	choice := model.mcpReconnectPrompt.handleKey(message.String())
	switch choice {
	case mcpReconnectNow:
		if model.interactionBusy() {
			return model.deferMCPReconnect()
		}
		return model.reconnectMCP()
	case mcpReconnectLater, mcpReconnectCancel:
		model.mcpReconnectPrompt = nil
	}
	return nil
}

func (model *tuiModel) handleMCPErrorKey(message tea.KeyMsg) tea.Cmd {
	switch message.String() {
	case "c", "C":
		return model.stageTerminalSequences(osc52ClipboardSequence(model.mcpErrorDetail), "")
	case "esc":
		model.mcpErrorServer, model.mcpErrorDetail = "", ""
	}
	return nil
}

func (model *tuiModel) renderMCPLogin() string {
	state := model.mcpLogin
	if state == nil {
		return ""
	}
	message := "Preparing secure MCP login..."
	help := "Esc cancel"
	switch state.phase {
	case mcpLoginAuthorize:
		message = "Open the authorization URL, then paste the final callback URL."
		help = "Enter submit hidden callback | c copy URL | Esc cancel"
	case mcpLoginWorkspace:
		message = "Enter the Slack workspace ID, or submit an empty value to choose in Slack."
		help = "Enter continue | Esc cancel"
	case mcpLoginDevice:
		message = "Complete device authorization: " + state.displayURL
		help = "c copy URL | Esc cancel"
	case mcpLoginWaiting:
		message = "Waiting for MCP authorization..."
	case mcpLoginFailed:
		message = "MCP login failed. Check provider access and token-store permissions."
		help = "Enter close | Esc close"
	case mcpLoginCancelled:
		message = "MCP login cancelled."
		help = "Enter close | Esc close"
	}
	lines := []string{"MCP OAuth Login: " + state.server, "", message}
	if state.displayURL != "" && state.phase == mcpLoginAuthorize {
		lines = append(lines, state.displayURL)
	}
	if len(state.input) > 0 {
		lines = append(lines, "Input: <hidden>")
	}
	lines = append(lines, "", help)
	for index := range lines {
		lines[index] = truncateMCPRunes(singleLineMCPSafe(lines[index]), min(max(model.width, 24), 160))
	}
	return strings.Join(lines[:min(len(lines), min(max(model.height, 6), 20))], "\n")
}

func validateMCPAuthorizeURL(raw string) (string, string, error) {
	if len(raw) == 0 || len(raw) > maxAuthManagerURLSize || strings.ContainsAny(raw, "\x00\r\n") {
		return "", "", errors.New("MCP authorization URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", "", errors.New("MCP authorization URL is invalid")
	}
	for key := range parsed.Query() {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "credential") || strings.Contains(lower, "api_key") {
			return "", "", errors.New("MCP authorization URL contains secret material")
		}
	}
	display := *parsed
	display.RawQuery = ""
	return parsed.String(), display.String(), nil
}

func (state *mcpLoginState) String() string {
	if state == nil {
		return "mcpLoginState(<nil>)"
	}
	return fmt.Sprintf("mcpLoginState(server=%s,phase=%d,<redacted>)", state.server, state.phase)
}

var _ talonmcp.Interaction = (*mcpTUIInteraction)(nil)
var _ oauthpolicy.WorkspaceSelector = (*mcpTUIInteraction)(nil)
var _ oauthpolicy.DeviceCodePresenter = (*mcpTUIInteraction)(nil)
