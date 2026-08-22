// Package claudeagent adapts the Claude CLI print protocol to damodel.Chat.
//
// The adapter runs each model turn in an empty working directory, with local
// customization sources, all built-in tools except Skill, and browser integration
// disabled. Request skills are materialized into an ephemeral session plugin.
// The authenticated user home remains available solely so the CLI can use its
// existing local login. Prior messages are materialized as Claude's native JSONL
// transcript and replayed with --resume. Request tools are exposed through an
// ephemeral loopback MCP server. Claude is stopped at the first MCP tool request
// so the caller-owned runtime remains the only tool executor.
package claudeagent

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/internal/optionvalue"
)

const (
	providerName                 = "claude_agent"
	toolServerName               = "model_tools"
	sessionPluginName            = "dago-session"
	maximumInlineToolDescription = 2048
	maximumSessionSkills         = 1000
	maximumSessionSkillBytes     = 1 << 20
	maximumOutputLine            = 4 << 20
	maximumDiagnostics           = 32 << 10
)

var mcpToolName = regexp.MustCompile(`\A[A-Za-z0-9_-]{1,128}\z`)

// Options configures a Claude CLI model. A zero value finds "claude" on PATH
// and uses an isolated temporary workspace with the CLI's ambient local login.
type Options struct {
	CLIPath         string
	ContextWindow   int
	MaxOutputTokens int
}

// Client is a Claude CLI-backed chat model.
type Client struct {
	model         string
	options       Options
	command       func(context.Context, string, ...string) *exec.Cmd
	homeDirectory func() (string, error)
	turn          chan struct{}
	process       *cliProcess
	history       []damessage.Message
	key           string
}

// New constructs a Claude CLI-backed model. The executable is resolved when a
// request is made so construction remains network- and filesystem-independent.
func New(model string, optionValues ...Options) *Client {
	options := optionvalue.Resolve("Claude agent client", optionValues)
	model = strings.TrimSpace(model)
	if model == "" {
		panic("claude agent: model is required")
	}
	if options.ContextWindow < 0 || options.MaxOutputTokens < 0 {
		panic("claude agent: token limits cannot be negative")
	}
	return &Client{model: model, options: options, command: exec.CommandContext, homeDirectory: os.UserHomeDir, turn: make(chan struct{}, 1)}
}

// Profile reports only the capabilities preserved by the isolated adapter.
func (client *Client) Profile() damodel.Profile {
	return damodel.Profile{
		Provider: providerName, Model: client.model,
		ContextWindow: client.options.ContextWindow, MaxOutputTokens: client.options.MaxOutputTokens,
		ToolCalling: true, ParallelToolCalls: true, StructuredOutput: true, NativeStreaming: true, NativeSkills: true,
		SupportsSeparateSystemMessage: true, SupportsReasoning: true,
		ReasoningLevels: []string{"low", "medium", "high", "xhigh", "max"},
	}
}

// Invoke executes one isolated bidirectional stream-JSON turn.
func (client *Client) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	if ctx == nil {
		panic("claude agent: nil context")
	}
	select {
	case client.turn <- struct{}{}:
		defer func() { <-client.turn }()
	case <-ctx.Done():
		return damodel.Response{}, ctx.Err()
	}
	return client.run(ctx, request, nil)
}

// Stream forwards the CLI's partial stream-JSON events as model chunks while
// retaining the process turn lock until completion or close.
func (client *Client) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	if ctx == nil {
		panic("claude agent: nil context")
	}
	select {
	case client.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	producerContext, cancel := context.WithCancel(ctx)
	stream := &responseStream{
		ctx: producerContext, cancel: cancel, items: make(chan streamItem, 1), done: make(chan struct{}),
	}
	go func() {
		defer func() { <-client.turn }()
		defer close(stream.done)
		defer close(stream.items)
		emitted := newStreamEmission()
		response, err := client.run(producerContext, request, func(chunk damodel.Chunk) error {
			emitted.record(chunk)
			if stream.send(streamItem{chunk: chunk}) {
				return nil
			}
			return producerContext.Err()
		})
		if err != nil {
			stream.send(streamItem{err: err})
			return
		}
		stream.send(streamItem{chunk: emitted.terminal(response)})
	}()
	return stream, nil
}

type responseStream struct {
	ctx       context.Context
	cancel    context.CancelFunc
	items     chan streamItem
	done      chan struct{}
	closeOnce sync.Once
}

type streamItem struct {
	chunk damodel.Chunk
	err   error
}

func (stream *responseStream) Next(ctx context.Context) (damodel.Chunk, error) {
	select {
	case item, ok := <-stream.items:
		if !ok {
			return damodel.Chunk{}, io.EOF
		}
		return item.chunk, item.err
	case <-ctx.Done():
		stream.cancel()
		return damodel.Chunk{}, ctx.Err()
	}
}
func (stream *responseStream) Close() error {
	stream.closeOnce.Do(func() {
		stream.cancel()
		<-stream.done
	})
	return nil
}
func (stream *responseStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *responseStream) send(item streamItem) bool {
	select {
	case stream.items <- item:
		return true
	case <-stream.ctx.Done():
		return false
	}
}

type streamEmission struct {
	text      map[int]string
	reasoning map[int]string
}

func newStreamEmission() *streamEmission {
	return &streamEmission{text: map[int]string{}, reasoning: map[int]string{}}
}

func (emission *streamEmission) record(chunk damodel.Chunk) {
	for fallbackIndex, block := range chunk.MessageDelta.Content {
		index := fallbackIndex
		if block.Index != nil {
			index = *block.Index
		}
		switch block.Type {
		case damessage.BlockText:
			emission.text[index] += block.Text
		case damessage.BlockReasoning:
			emission.reasoning[index] += block.Reasoning
		}
	}
}

func (emission *streamEmission) terminal(response damodel.Response) damodel.Chunk {
	message := response.Message.Clone()
	content := make([]damessage.ContentBlock, 0, len(message.Content))
	for index, block := range message.Content {
		switch block.Type {
		case damessage.BlockText:
			block.Text = remainingStreamValue(block.Text, emission.text[index])
			if block.Text == "" && len(block.Citations) == 0 && len(block.Extra) == 0 {
				continue
			}
		case damessage.BlockReasoning:
			block.Reasoning = remainingStreamValue(block.Reasoning, emission.reasoning[index])
			if block.Reasoning == "" && len(block.Extra) == 0 {
				continue
			}
		}
		content = append(content, block)
	}
	message.Content = content
	return damodel.Chunk{
		MessageDelta: message,
		Structured:   append(json.RawMessage(nil), response.Structured...),
		Done:         true,
	}
}

func remainingStreamValue(complete, emitted string) string {
	if strings.HasPrefix(complete, emitted) {
		return strings.TrimPrefix(complete, emitted)
	}
	return complete
}

type cliEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Event   struct {
		Type    string `json:"type"`
		Index   int    `json:"index,omitempty"`
		Message struct {
			ID    string   `json:"id,omitempty"`
			Model string   `json:"model,omitempty"`
			Usage cliUsage `json:"usage,omitempty"`
		} `json:"message,omitempty"`
		Delta struct {
			Type     string `json:"type,omitempty"`
			Text     string `json:"text,omitempty"`
			Thinking string `json:"thinking,omitempty"`
		} `json:"delta,omitempty"`
	} `json:"event,omitempty"`
	Message struct {
		ID      string          `json:"id,omitempty"`
		Model   string          `json:"model,omitempty"`
		Content json.RawMessage `json:"content"`
		Usage   cliUsage        `json:"usage,omitempty"`
	} `json:"message,omitempty"`
	Usage cliUsage `json:"usage,omitempty"`
	Error string   `json:"error,omitempty"`
}

type cliUsage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type cliContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

func (client *Client) run(ctx context.Context, request damodel.Request, emit func(damodel.Chunk) error) (damodel.Response, error) {
	if err := ctx.Err(); err != nil {
		return damodel.Response{}, err
	}
	for index, definition := range request.Tools {
		if err := definition.Validate(); err != nil {
			return damodel.Response{}, fmt.Errorf("claude agent: tool %d: %w", index, err)
		}
	}
	definitions := request.Tools
	if request.ToolChoice != nil && strings.EqualFold(request.ToolChoice.Mode, "none") {
		definitions = nil
	}
	definitions, sessionSkills := prepareSessionSkills(definitions, request.Skills)
	key, err := processKey(request, definitions, sessionSkills)
	if err != nil {
		return damodel.Response{}, err
	}
	incoming := visibleMessages(request.Messages)
	continuing := client.process != nil && client.key == key && client.process.alive() && messagePrefix(incoming, client.history)
	if !continuing {
		client.stopProcess()
		process, currentContent, startErr := client.startProcess(request, definitions, sessionSkills, incoming)
		if startErr != nil {
			return damodel.Response{}, startErr
		}
		client.process = process
		client.key = key
		if err := process.writeUser(currentContent); err != nil {
			client.stopProcess()
			return damodel.Response{}, err
		}
	} else {
		suffix := incoming[len(client.history):]
		toolResults, other := splitContinuation(suffix)
		for _, result := range toolResults {
			if err := client.process.bridge.fulfill(result); err != nil {
				client.stopProcess()
				return damodel.Response{}, err
			}
		}
		if len(other) > 0 {
			content, err := userContent(other)
			if err != nil {
				return damodel.Response{}, err
			}
			if err := client.process.writeUser(content); err != nil {
				client.stopProcess()
				return damodel.Response{}, err
			}
		}
	}

	response, readErr := client.process.read(ctx, emit)
	if readErr != nil && continuing && ctx.Err() == nil {
		client.stopProcess()
		process, currentContent, startErr := client.startProcess(request, definitions, sessionSkills, incoming)
		if startErr != nil {
			return damodel.Response{}, errors.Join(readErr, startErr)
		}
		client.process = process
		client.key = key
		if writeErr := process.writeUser(currentContent); writeErr != nil {
			client.stopProcess()
			return damodel.Response{}, errors.Join(readErr, writeErr)
		}
		response, readErr = process.read(ctx, emit)
	}
	if readErr != nil {
		client.stopProcess()
		return damodel.Response{}, readErr
	}
	if len(response.Message.Content) == 0 && len(response.Message.ToolCalls) == 0 {
		client.stopProcess()
		return damodel.Response{}, errors.New("claude agent: CLI returned no assistant content")
	}
	if request.ResponseFormat != nil {
		text := response.Message.TextContent()
		if !json.Valid([]byte(text)) {
			return damodel.Response{}, errors.New("claude agent: structured response is not valid JSON")
		}
		response.Structured = json.RawMessage(text)
	}
	client.history = append(cloneMessages(incoming), response.Message.Clone())
	return response, nil
}

type cliProcess struct {
	root             string
	projectDirectory string
	sessionID        string
	command          *exec.Cmd
	stdin            io.WriteCloser
	scanner          *bufio.Scanner
	diagnostics      *boundedBuffer
	bridge           *toolBridge
	cancel           context.CancelFunc
	done             chan error
	waited           bool
	waitErr          error
	replayed         map[string]bool
}

func (client *Client) startProcess(request damodel.Request, definitions []datool.Definition, skills []damodel.Skill, messages []damessage.Message) (*cliProcess, any, error) {
	root, err := os.MkdirTemp("", "dago-claude-agent-")
	if err != nil {
		return nil, nil, fmt.Errorf("claude agent: create isolation root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	workingDirectory := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workingDirectory, 0o700); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: create isolated working directory: %w", err)
	}
	workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: resolve isolated working directory: %w", err)
	}
	pluginDirectory, err := materializeSessionPlugin(root, skills)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	homeDirectory, err := client.homeDirectory()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: resolve authenticated home: %w", err)
	}
	homeDirectory = strings.TrimSpace(homeDirectory)
	if homeDirectory == "" || !filepath.IsAbs(homeDirectory) {
		cleanup()
		return nil, nil, errors.New("claude agent: authenticated home must be an absolute path")
	}
	bridge, err := newToolBridge(definitions)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	sessionID, err := randomUUID()
	if err != nil {
		bridge.Close()
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: create session ID: %w", err)
	}
	currentContent, replaying, err := writeTranscript(homeDirectory, workingDirectory, sessionID, client.model, messages, bridge)
	if err != nil {
		bridge.Close()
		cleanup()
		return nil, nil, err
	}
	arguments, err := client.arguments(request, systemText(request), sessionID, replaying, bridge, pluginDirectory)
	if err != nil {
		bridge.Close()
		cleanup()
		return nil, nil, err
	}
	cliPath := strings.TrimSpace(client.options.CLIPath)
	if cliPath == "" {
		cliPath, err = exec.LookPath("claude")
		if err != nil {
			bridge.Close()
			cleanup()
			return nil, nil, fmt.Errorf("claude agent: find Claude CLI: %w", err)
		}
	}
	processContext, cancel := context.WithCancel(context.Background())
	command := client.command(processContext, cliPath, arguments...)
	command.Dir = workingDirectory
	command.Env = filteredEnvironment(os.Environ(), homeDirectory)
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		bridge.Close()
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: open CLI stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		bridge.Close()
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: open CLI stdout: %w", err)
	}
	diagnostics := &boundedBuffer{limit: maximumDiagnostics}
	command.Stderr = diagnostics
	if err := command.Start(); err != nil {
		cancel()
		bridge.Close()
		cleanup()
		return nil, nil, fmt.Errorf("claude agent: start CLI: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maximumOutputLine)
	replayedMessages := make(map[string]bool)
	if replaying {
		for _, message := range messages {
			if message.Role == damessage.RoleAssistant && message.ID != "" {
				replayedMessages[message.ID] = true
			}
		}
	}
	process := &cliProcess{
		root: root, projectDirectory: transcriptDirectory(homeDirectory, workingDirectory), sessionID: sessionID,
		command: command, stdin: stdin, scanner: scanner, diagnostics: diagnostics, bridge: bridge,
		cancel: cancel, done: make(chan error, 1), replayed: replayedMessages,
	}
	go func() { process.done <- command.Wait() }()
	return process, currentContent, nil
}

func (process *cliProcess) writeUser(content any) error {
	frame := map[string]any{
		"type": "user", "session_id": process.sessionID, "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": content},
	}
	if err := json.NewEncoder(process.stdin).Encode(frame); err != nil {
		return fmt.Errorf("claude agent: write streaming input: %w", err)
	}
	return nil
}

func (process *cliProcess) read(ctx context.Context, emit func(damodel.Chunk) error) (damodel.Response, error) {
	type result struct {
		response damodel.Response
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, _, err := readCLIOutput(context.Background(), process.scanner, process.bridge, process.replayed, emit)
		results <- result{response: response, err: err}
	}()
	select {
	case value := <-results:
		if value.err != nil {
			if exited, waitErr := process.exitStatus(); exited && waitErr != nil {
				return damodel.Response{}, cliExitError(waitErr, process.diagnostics.String())
			}
		}
		return value.response, value.err
	case <-ctx.Done():
		process.stop()
		<-results
		return damodel.Response{}, ctx.Err()
	}
}

func (process *cliProcess) alive() bool {
	exited, _ := process.exitStatus()
	return !exited
}

func (process *cliProcess) exitStatus() (bool, error) {
	if process.waited {
		return true, process.waitErr
	}
	select {
	case process.waitErr = <-process.done:
		process.waited = true
		return true, process.waitErr
	default:
		return false, nil
	}
}

func (process *cliProcess) stop() {
	if process == nil {
		return
	}
	process.cancel()
	_ = process.stdin.Close()
	if !process.waited {
		process.waitErr = <-process.done
		process.waited = true
	}
	_ = process.bridge.Close()
	_ = os.RemoveAll(process.projectDirectory)
	_ = os.RemoveAll(process.root)
}

func (client *Client) stopProcess() {
	if client.process != nil {
		client.process.stop()
	}
	client.process = nil
	client.history = nil
	client.key = ""
}

// Close stops the persistent CLI session and removes its ephemeral transcript.
func (client *Client) Close() error {
	client.turn <- struct{}{}
	defer func() { <-client.turn }()
	client.stopProcess()
	return nil
}

func (client *Client) arguments(request damodel.Request, systemPrompt, sessionID string, replaying bool, bridge *toolBridge, pluginDirectory string) ([]string, error) {
	arguments := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--no-session-persistence",
		"--no-chrome", "--strict-mcp-config", "--tools", "Skill", "--setting-sources=", "--settings", `{"disableSkillShellExecution":true}`,
		"--prompt-suggestions", "false", "--system-prompt", systemPrompt, "--model", client.model,
		"--permission-mode", "dontAsk",
	}
	if pluginDirectory != "" {
		arguments = append(arguments, "--plugin-dir", pluginDirectory)
	}
	if replaying {
		arguments = append(arguments, "--resume", sessionID)
	} else {
		arguments = append(arguments, "--session-id", sessionID)
	}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		arguments = append(arguments, "--effort", request.Reasoning.Effort)
	}
	if request.ResponseFormat != nil {
		if !json.Valid(request.ResponseFormat.Schema) {
			return nil, errors.New("claude agent: response schema is not valid JSON")
		}
		arguments = append(arguments, "--json-schema", string(request.ResponseFormat.Schema))
	}
	if bridge != nil && len(bridge.names) > 0 {
		config, err := bridge.Config()
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, "--mcp-config", config, "--allowed-tools", strings.Join(bridge.allowedNames(), ","))
	}
	return arguments, nil
}

func systemText(request damodel.Request) string {
	var parts []string
	if request.SystemMessage != nil {
		parts = append(parts, request.SystemMessage.TextContent())
	}
	for _, message := range request.Messages {
		if message.Role == damessage.RoleSystem {
			parts = append(parts, message.TextContent())
		}
	}
	return strings.Join(parts, "\n\n")
}

func processKey(request damodel.Request, definitions []datool.Definition, skills []damodel.Skill) (string, error) {
	value := struct {
		System         string
		Tools          []datool.Definition
		Skills         []damodel.Skill
		ToolChoice     *damodel.ToolChoice
		ResponseFormat *damodel.ResponseFormat
		Reasoning      *damodel.Reasoning
	}{systemText(request), definitions, skills, request.ToolChoice, request.ResponseFormat, request.Reasoning}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("claude agent: encode process configuration: %w", err)
	}
	return string(encoded), nil
}

func prepareSessionSkills(definitions []datool.Definition, provided []damodel.Skill) ([]datool.Definition, []damodel.Skill) {
	prepared := append([]datool.Definition(nil), definitions...)
	byName := make(map[string]damodel.Skill, len(provided)+len(definitions))
	for _, skill := range provided {
		byName[skill.Name] = skill
	}
	for index := range prepared {
		description := prepared[index].Description
		if len(description) <= maximumInlineToolDescription {
			continue
		}
		name := toolManualSkillName(prepared[index].Name)
		byName[name] = damodel.Skill{
			Name: name, Description: "Detailed instructions for the " + prepared[index].Name + " MCP tool. Load before calling this tool.",
			Instructions: description,
		}
		prepared[index].Description = "Before calling this tool, use the Skill tool to load `" + sessionPluginName + ":" + name + "`; it contains the complete instructions."
	}
	skills := make([]damodel.Skill, 0, len(byName))
	for _, skill := range byName {
		skills = append(skills, skill)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return prepared, skills
}

func toolManualSkillName(toolName string) string {
	var normalized strings.Builder
	separator := false
	for _, value := range strings.ToLower(toolName) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' {
			normalized.WriteRune(value)
			separator = false
		} else if normalized.Len() > 0 && !separator {
			normalized.WriteByte('-')
			separator = true
		}
	}
	base := strings.Trim(normalized.String(), "-")
	if base == "" {
		base = "tool"
	}
	if len(base) > 45 {
		base = strings.TrimRight(base[:45], "-")
	}
	digest := sha256.Sum256([]byte(toolName))
	return "mcp-" + base + "-" + hex.EncodeToString(digest[:4])
}

func materializeSessionPlugin(root string, skills []damodel.Skill) (string, error) {
	if len(skills) == 0 {
		return "", nil
	}
	if len(skills) > maximumSessionSkills {
		return "", fmt.Errorf("claude agent: session has %d skills, maximum is %d", len(skills), maximumSessionSkills)
	}
	pluginDirectory := filepath.Join(root, "skill-plugin")
	manifestDirectory := filepath.Join(pluginDirectory, ".claude-plugin")
	if err := os.MkdirAll(manifestDirectory, 0o700); err != nil {
		return "", fmt.Errorf("claude agent: create session plugin: %w", err)
	}
	manifest, _ := json.Marshal(map[string]string{
		"name": sessionPluginName, "version": "1.0.0", "description": "Session-scoped model skills.",
	})
	if err := os.WriteFile(filepath.Join(manifestDirectory, "plugin.json"), manifest, 0o600); err != nil {
		return "", fmt.Errorf("claude agent: write session plugin manifest: %w", err)
	}
	seen := make(map[string]bool, len(skills))
	for _, skill := range skills {
		if !validSkillName(skill.Name) {
			return "", fmt.Errorf("claude agent: invalid session skill name %q", skill.Name)
		}
		if seen[skill.Name] {
			return "", fmt.Errorf("claude agent: duplicate session skill %q", skill.Name)
		}
		seen[skill.Name] = true
		if strings.TrimSpace(skill.Description) == "" {
			return "", fmt.Errorf("claude agent: session skill %q has no description", skill.Name)
		}
		if len(skill.Instructions) > maximumSessionSkillBytes {
			return "", fmt.Errorf("claude agent: session skill %q exceeds %d bytes", skill.Name, maximumSessionSkillBytes)
		}
		description, _ := json.Marshal(skill.Description)
		content := "---\nname: " + skill.Name + "\ndescription: " + string(description) + "\n---\n\n" + strings.TrimSpace(skill.Instructions) + "\n"
		directory := filepath.Join(pluginDirectory, "skills", skill.Name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", fmt.Errorf("claude agent: create session skill %q: %w", skill.Name, err)
		}
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
			return "", fmt.Errorf("claude agent: write session skill %q: %w", skill.Name, err)
		}
	}
	return pluginDirectory, nil
}

func validSkillName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
		return false
	}
	for _, value := range name {
		if value != '-' && (value < 'a' || value > 'z') && (value < '0' || value > '9') {
			return false
		}
	}
	return true
}

func visibleMessages(messages []damessage.Message) []damessage.Message {
	result := make([]damessage.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role != damessage.RoleSystem && message.Role != damessage.RoleRemove {
			result = append(result, message.Clone())
		}
	}
	return result
}

func cloneMessages(messages []damessage.Message) []damessage.Message {
	result := make([]damessage.Message, len(messages))
	for index := range messages {
		result[index] = messages[index].Clone()
	}
	return result
}

func messagePrefix(messages, prefix []damessage.Message) bool {
	return len(messages) >= len(prefix) && reflect.DeepEqual(messages[:len(prefix)], prefix)
}

func splitContinuation(messages []damessage.Message) (tools, other []damessage.Message) {
	for _, message := range messages {
		if message.Role == damessage.RoleTool {
			tools = append(tools, message)
		} else {
			other = append(other, message)
		}
	}
	return tools, other
}

type transcriptTurn struct {
	role       string
	content    []any
	stopReason string
	messageID  string
}

func buildTranscriptTurns(messages []damessage.Message, bridge *toolBridge) ([]transcriptTurn, error) {
	var turns []transcriptTurn
	for _, message := range messages {
		role := "user"
		var content []any
		switch message.Role {
		case damessage.RoleHuman:
			for _, block := range message.Content {
				switch block.Type {
				case damessage.BlockText:
					content = append(content, map[string]any{"type": "text", "text": block.Text})
				case damessage.BlockImage:
					content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": block.MIMEType, "data": block.Data}})
				case damessage.BlockFile:
					content = append(content, map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": block.MIMEType, "data": block.Data}})
				}
			}
		case damessage.RoleTool:
			result := map[string]any{"type": "tool_result", "tool_use_id": message.ToolCallID, "content": message.TextContent()}
			if message.ToolStatus == damessage.ToolStatusError {
				result["is_error"] = true
			}
			content = append(content, result)
		case damessage.RoleAssistant:
			role = "assistant"
			for _, block := range message.Content {
				if block.Type == damessage.BlockText && block.Text != "" {
					content = append(content, map[string]any{"type": "text", "text": block.Text})
				}
			}
			for _, call := range message.ToolCalls {
				var input any
				if err := json.Unmarshal(call.Arguments, &input); err != nil {
					return nil, fmt.Errorf("claude agent: transcript tool call %q: %w", call.ID, err)
				}
				name := call.Name
				if bridge != nil {
					name = bridge.cliName(name)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": name, "input": input})
			}
		default:
			continue
		}
		if len(content) == 0 {
			continue
		}
		stopReason := ""
		if role == "assistant" {
			stopReason = "end_turn"
			if len(message.ToolCalls) > 0 {
				stopReason = "tool_use"
			}
		}
		if len(turns) > 0 && turns[len(turns)-1].role == role {
			turns[len(turns)-1].content = append(turns[len(turns)-1].content, content...)
			if stopReason == "tool_use" {
				turns[len(turns)-1].stopReason = stopReason
			}
			if turns[len(turns)-1].messageID == "" {
				turns[len(turns)-1].messageID = message.ID
			}
		} else {
			turns = append(turns, transcriptTurn{role: role, content: content, stopReason: stopReason, messageID: message.ID})
		}
	}
	return turns, nil
}

func userContent(messages []damessage.Message) (any, error) {
	turns, err := buildTranscriptTurns(messages, nil)
	if err != nil {
		return nil, err
	}
	if len(turns) != 1 || turns[0].role != "user" {
		return nil, errors.New("claude agent: continuation must contain one user turn")
	}
	return compactTextContent(turns[0].content), nil
}

func compactTextContent(content []any) any {
	if len(content) == 1 {
		if block, ok := content[0].(map[string]any); ok && block["type"] == "text" {
			return block["text"]
		}
	}
	return content
}

func initializeHome(homeDirectory, workingDirectory string) error {
	if err := os.MkdirAll(filepath.Join(homeDirectory, ".claude"), 0o700); err != nil {
		return fmt.Errorf("claude agent: create isolated home: %w", err)
	}
	config := map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{workingDirectory: map[string]any{"hasTrustDialogAccepted": true}},
	}
	encoded, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(homeDirectory, ".claude.json"), encoded, 0o600); err != nil {
		return fmt.Errorf("claude agent: initialize isolated home: %w", err)
	}
	return nil
}

func writeTranscript(homeDirectory, workingDirectory, sessionID, model string, messages []damessage.Message, bridge *toolBridge) (any, bool, error) {
	turns, err := buildTranscriptTurns(messages, bridge)
	if err != nil {
		return nil, false, err
	}
	if len(turns) == 0 || turns[len(turns)-1].role != "user" {
		return nil, false, errors.New("claude agent: request must end with a user or tool-result turn")
	}
	last := turns[len(turns)-1]
	current := compactTextContent(last.content)
	historyEnd := len(turns) - 1
	if onlyToolResults(last.content) {
		historyEnd = len(turns)
		current = "Continue from the completed tool results without repeating any tool call whose result is already present."
	}
	if historyEnd == 0 {
		return current, false, nil
	}
	projectDirectory := transcriptDirectory(homeDirectory, workingDirectory)
	if err := os.MkdirAll(projectDirectory, 0o700); err != nil {
		return nil, false, fmt.Errorf("claude agent: create transcript directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(projectDirectory, sessionID+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("claude agent: create transcript: %w", err)
	}
	encoder := json.NewEncoder(file)
	var parent any
	for _, turn := range turns[:historyEnd] {
		uuid, uuidErr := randomUUID()
		if uuidErr != nil {
			file.Close()
			return nil, false, fmt.Errorf("claude agent: create transcript message ID: %w", uuidErr)
		}
		message := map[string]any{"role": turn.role, "content": turn.content}
		entryType := turn.role
		if turn.role == "assistant" {
			message["type"] = "message"
			messageID := turn.messageID
			if messageID == "" {
				messageID = "msg_" + strings.ReplaceAll(uuid, "-", "")
			}
			message["id"] = messageID
			message["model"] = model
			message["stop_reason"] = turn.stopReason
			message["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0}
		}
		entry := map[string]any{
			"parentUuid": parent, "isSidechain": false, "type": entryType, "message": message,
			"uuid": uuid, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "cwd": workingDirectory,
			"sessionId": sessionID, "userType": "external", "entrypoint": "dago-go",
		}
		if err := encoder.Encode(entry); err != nil {
			file.Close()
			return nil, false, fmt.Errorf("claude agent: write transcript: %w", err)
		}
		parent = uuid
	}
	if err := file.Close(); err != nil {
		return nil, false, fmt.Errorf("claude agent: close transcript: %w", err)
	}
	return current, true, nil
}

func transcriptDirectory(homeDirectory, workingDirectory string) string {
	return filepath.Join(homeDirectory, ".claude", "projects", sanitizeProjectPath(workingDirectory))
}

func onlyToolResults(content []any) bool {
	if len(content) == 0 {
		return false
	}
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			return false
		}
	}
	return true
}

func sanitizeProjectPath(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' {
			result.WriteRune(char)
		} else {
			result.WriteByte('-')
		}
	}
	return result.String()
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := cryptorand.Read(value); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func readCLIOutput(ctx context.Context, scanner *bufio.Scanner, bridge *toolBridge, replayed map[string]bool, emit func(damodel.Chunk) error) (damodel.Response, bool, error) {
	response := damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant}}
	terminal := false
	suppressPartial := false
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return damodel.Response{}, false, err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var envelope cliEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			return damodel.Response{}, false, fmt.Errorf("claude agent: decode CLI stream: %w", err)
		}
		switch envelope.Type {
		case "assistant":
			if replayed[envelope.Message.ID] {
				continue
			}
			response.Message.ID = envelope.Message.ID
			var content []cliContent
			if err := json.Unmarshal(envelope.Message.Content, &content); err != nil {
				return damodel.Response{}, false, fmt.Errorf("claude agent: decode assistant content: %w", err)
			}
			for _, block := range content {
				switch block.Type {
				case "text":
					response.Message.Content = append(response.Message.Content, damessage.ContentBlock{Type: damessage.BlockText, Text: block.Text})
				case "thinking":
					response.Message.Content = append(response.Message.Content, damessage.ContentBlock{Type: damessage.BlockReasoning, Reasoning: block.Thinking})
				case "tool_use":
					if block.Name == "Skill" {
						continue
					}
					if bridge != nil {
						bridge.registerToolCall(block.ID, block.Name, block.Input)
					}
					name := block.Name
					if bridge != nil {
						name = bridge.originalName(name)
					}
					arguments := block.Input
					if len(arguments) == 0 {
						arguments = json.RawMessage(`{}`)
					}
					response.Message.ToolCalls = append(response.Message.ToolCalls, damessage.ToolCall{ID: block.ID, Name: name, Arguments: append(json.RawMessage(nil), arguments...)})
				}
			}
			mergeUsage(&response.Message, envelope.Message.Usage, envelope.Message.Model)
		case "stream_event":
			switch envelope.Event.Type {
			case "message_start":
				suppressPartial = replayed[envelope.Event.Message.ID]
				if !suppressPartial {
					response.Message.ID = envelope.Event.Message.ID
					mergeUsage(&response.Message, envelope.Event.Message.Usage, envelope.Event.Message.Model)
				}
			case "content_block_delta":
				if suppressPartial || emit == nil {
					break
				}
				blockIndex := envelope.Event.Index
				var block damessage.ContentBlock
				switch envelope.Event.Delta.Type {
				case "text_delta":
					block = damessage.ContentBlock{Type: damessage.BlockText, Text: envelope.Event.Delta.Text, Index: &blockIndex}
				case "thinking_delta":
					block = damessage.ContentBlock{Type: damessage.BlockReasoning, Reasoning: envelope.Event.Delta.Thinking, Index: &blockIndex}
				default:
					break
				}
				if block.Type != "" && (block.Text != "" || block.Reasoning != "") {
					chunk := damodel.Chunk{MessageDelta: damessage.Message{ID: response.Message.ID, Role: damessage.RoleAssistant, Content: []damessage.ContentBlock{block}}}
					if err := emit(chunk); err != nil {
						return damodel.Response{}, false, err
					}
				}
			}
			if envelope.Event.Type == "message_stop" && len(response.Message.ToolCalls) > 0 {
				damodel.SetOutcome(&response.Message, damodel.FinishReasonToolCalls, nil)
				return response, false, nil
			}
		case "result":
			mergeUsage(&response.Message, envelope.Usage, "")
			if envelope.Subtype != "success" {
				message := strings.TrimSpace(envelope.Error)
				if message == "" {
					message = "Claude CLI result subtype " + envelope.Subtype
				}
				return damodel.Response{}, true, errors.New("claude agent: " + message)
			}
			damodel.SetOutcome(&response.Message, damodel.FinishReasonStop, nil)
			terminal = true
			return response, terminal, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return damodel.Response{}, false, fmt.Errorf("claude agent: read CLI stream: %w", err)
	}
	return response, terminal, errors.New("claude agent: CLI stream ended before a result")
}

func mergeUsage(message *damessage.Message, usage cliUsage, model string) {
	input := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	if input == 0 && usage.OutputTokens == 0 && message.Usage != nil {
		return
	}
	if model == "" && message.Usage != nil {
		model = message.Usage.Model
	}
	message.Usage = &damessage.Usage{
		InputTokens: input, OutputTokens: usage.OutputTokens, TotalTokens: input + usage.OutputTokens,
		InputDetails: map[string]int{"cache_creation": usage.CacheCreationInputTokens, "cache_read": usage.CacheReadInputTokens},
		Provider:     providerName, Model: model,
	}
}

func filteredEnvironment(environment []string, homeDirectory string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_MANAGED_SETTINGS_PATH",
			"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_AGENT_SDK_VERSION":
			continue
		}
		result = append(result, entry)
	}
	return append(result, "HOME="+homeDirectory, "CLAUDE_CODE_ENTRYPOINT=dago-go")
}

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := max(0, buffer.limit-len(buffer.data))
	buffer.data = append(buffer.data, value[:min(len(value), remaining)]...)
	return len(value), nil
}
func (buffer *boundedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.data)
}

func cliExitError(err error, diagnostics string) error {
	diagnostics = strings.TrimSpace(diagnostics)
	if diagnostics == "" {
		return fmt.Errorf("claude agent: CLI failed: %w", err)
	}
	return fmt.Errorf("claude agent: CLI failed: %w: %s", err, diagnostics)
}

type toolBridge struct {
	listener net.Listener
	server   *http.Server
	url      string
	token    string
	names    map[string]string
	mu       sync.Mutex
	expected map[string]*expectedTool
	waiting  []*expectedTool
	calls    []*pendingTool
}

type expectedTool struct {
	id        string
	name      string
	arguments string
	result    chan *mcp.CallToolResult
	bound     bool
	fulfilled bool
}

type pendingTool struct {
	name      string
	arguments string
	bound     chan *expectedTool
}

func newToolBridge(definitions []datool.Definition) (*toolBridge, error) {
	bridge := &toolBridge{names: make(map[string]string, len(definitions)), expected: make(map[string]*expectedTool)}
	if len(definitions) == 0 {
		return bridge, nil
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("claude agent: listen for tool bridge: %w", err)
	}
	bridge.listener = listener
	tokenBytes := make([]byte, 24)
	if _, err := cryptorand.Read(tokenBytes); err != nil {
		listener.Close()
		return nil, fmt.Errorf("claude agent: create tool bridge authority: %w", err)
	}
	bridge.token = hex.EncodeToString(tokenBytes)
	server := mcp.NewServer(&mcp.Implementation{Name: toolServerName, Version: "1"}, nil)
	for index, definition := range definitions {
		name := definition.Name
		if !mcpToolName.MatchString(name) || bridge.names[name] != "" {
			name = fmt.Sprintf("tool_%d", index+1)
		}
		bridge.names[name] = definition.Name
		server.AddTool(&mcp.Tool{Name: name, Description: definition.Description, InputSchema: append(json.RawMessage(nil), definition.InputSchema...)}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			call := &pendingTool{name: request.Params.Name, arguments: canonicalJSON(request.Params.Arguments), bound: make(chan *expectedTool, 1)}
			bridge.mu.Lock()
			bridge.calls = append(bridge.calls, call)
			bridge.matchLocked()
			bridge.mu.Unlock()
			var expected *expectedTool
			select {
			case expected = <-call.bound:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case result := <-expected.result:
				return result, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	}
	pathBytes := make([]byte, 16)
	if _, err := cryptorand.Read(pathBytes); err != nil {
		listener.Close()
		return nil, fmt.Errorf("claude agent: create tool bridge path: %w", err)
	}
	path := "/" + hex.EncodeToString(pathBytes)
	bridge.url = "http://" + listener.Addr().String() + path
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	bridge.server = &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != path || request.Header.Get("Authorization") != "Bearer "+bridge.token {
			http.NotFound(writer, request)
			return
		}
		handler.ServeHTTP(writer, request)
	})}
	go func() { _ = bridge.server.Serve(listener) }()
	return bridge, nil
}

func (bridge *toolBridge) Config() (string, error) {
	if bridge == nil || len(bridge.names) == 0 {
		return "", nil
	}
	payload := map[string]any{"mcpServers": map[string]any{toolServerName: map[string]any{
		"type": "http", "url": bridge.url, "headers": map[string]string{"Authorization": "Bearer " + bridge.token},
	}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("claude agent: encode tool bridge configuration: %w", err)
	}
	return string(encoded), nil
}

func (bridge *toolBridge) allowedNames() []string {
	result := make([]string, 0, len(bridge.names))
	for name := range bridge.names {
		result = append(result, "mcp__"+toolServerName+"__"+name)
	}
	sort.Strings(result)
	return result
}

func (bridge *toolBridge) originalName(value string) string {
	prefix := "mcp__" + toolServerName + "__"
	name := strings.TrimPrefix(value, prefix)
	if original := bridge.names[name]; original != "" {
		return original
	}
	return value
}

func (bridge *toolBridge) cliName(value string) string {
	if bridge == nil {
		return value
	}
	for exposed, original := range bridge.names {
		if original == value {
			return "mcp__" + toolServerName + "__" + exposed
		}
	}
	return value
}

func (bridge *toolBridge) registerToolCall(id, cliName string, arguments json.RawMessage) {
	if bridge == nil || id == "" {
		return
	}
	name := strings.TrimPrefix(cliName, "mcp__"+toolServerName+"__")
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.expected[id] != nil {
		return
	}
	expected := &expectedTool{id: id, name: name, arguments: canonicalJSON(arguments), result: make(chan *mcp.CallToolResult, 1)}
	bridge.expected[id] = expected
	bridge.waiting = append(bridge.waiting, expected)
	bridge.matchLocked()
}

func (bridge *toolBridge) fulfill(message damessage.Message) error {
	bridge.mu.Lock()
	expected := bridge.expected[message.ToolCallID]
	if expected == nil {
		bridge.mu.Unlock()
		return fmt.Errorf("claude agent: no pending MCP call %q", message.ToolCallID)
	}
	if expected.fulfilled {
		bridge.mu.Unlock()
		return fmt.Errorf("claude agent: MCP call %q was already fulfilled", message.ToolCallID)
	}
	expected.fulfilled = true
	bridge.mu.Unlock()
	result := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: message.TextContent()}}, IsError: message.ToolStatus == damessage.ToolStatusError}
	expected.result <- result
	return nil
}

func (bridge *toolBridge) matchLocked() {
	for expectedIndex := 0; expectedIndex < len(bridge.waiting); {
		expected := bridge.waiting[expectedIndex]
		matched := -1
		for callIndex, call := range bridge.calls {
			if call.name == expected.name && call.arguments == expected.arguments {
				matched = callIndex
				break
			}
		}
		if matched < 0 {
			expectedIndex++
			continue
		}
		call := bridge.calls[matched]
		bridge.calls = append(bridge.calls[:matched], bridge.calls[matched+1:]...)
		bridge.waiting = append(bridge.waiting[:expectedIndex], bridge.waiting[expectedIndex+1:]...)
		expected.bound = true
		call.bound <- expected
	}
}

func canonicalJSON(value json.RawMessage) string {
	if len(value) == 0 {
		return "{}"
	}
	var buffer bytes.Buffer
	if json.Compact(&buffer, value) == nil {
		return buffer.String()
	}
	return string(value)
}

func (bridge *toolBridge) Close() error {
	if bridge == nil || bridge.server == nil {
		return nil
	}
	return bridge.server.Close()
}
