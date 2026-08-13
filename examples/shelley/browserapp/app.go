// Package browserapp provides Shelley's browser-native application boundary.
// It deliberately depends only on portable dago packages: host processes,
// sockets, Git worktrees, and operating-system files never enter this package.
package browserapp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/semistrict/dago"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/examples/shelley/llm"
)

const (
	snapshotVersion = 1
	modelID         = "browser-predictable"
	webGPUModelID   = "local-webgpu"
	webGPUModelName = "Qwen3.5 0.8B (WebGPU)"
	webGPUContext   = 8192
	workspaceRoot   = "/workspace"
)

// Request is the language-neutral request passed across the worker boundary.
type Request struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// Response is returned across the worker boundary. Changed tells the worker
// to persist a fresh snapshot after sending the response.
type Response struct {
	Status               int               `json:"status"`
	Headers              map[string]string `json:"headers,omitempty"`
	Body                 json.RawMessage   `json:"body,omitempty"`
	Changed              bool              `json:"changed,omitempty"`
	ContinueConversation string            `json:"continue_conversation,omitempty"`
}

// Conversation is the stable subset of Shelley's conversation contract used
// by both the browser runtime and the existing UI.
type Conversation struct {
	ConversationID       string  `json:"conversation_id"`
	Slug                 *string `json:"slug"`
	UserInitiated        bool    `json:"user_initiated"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`
	Cwd                  *string `json:"cwd"`
	Archived             bool    `json:"archived"`
	ParentConversationID *string `json:"parent_conversation_id"`
	Model                *string `json:"model"`
	ConversationOptions  string  `json:"conversation_options"`
	CurrentGeneration    int64   `json:"current_generation"`
	AgentWorking         bool    `json:"agent_working"`
	Tags                 string  `json:"tags"`
	IsDraft              bool    `json:"is_draft"`
	Draft                string  `json:"draft"`
	QueuedMessages       string  `json:"queued_messages"`
}

type conversationWithState struct {
	Conversation
	Working       bool     `json:"working"`
	SubagentCount int      `json:"subagent_count"`
	Preview       string   `json:"preview,omitempty"`
	MaxSequenceID int64    `json:"max_sequence_id"`
	Participants  []string `json:"participants,omitempty"`
}

type apiMessage struct {
	MessageID      string  `json:"message_id"`
	ConversationID string  `json:"conversation_id"`
	SequenceID     int64   `json:"sequence_id"`
	Type           string  `json:"type"`
	LLMData        *string `json:"llm_data,omitempty"`
	UsageData      *string `json:"usage_data,omitempty"`
	CreatedAt      string  `json:"created_at"`
	Generation     int64   `json:"generation"`
	EndOfTurn      *bool   `json:"end_of_turn,omitempty"`
	ModelName      *string `json:"model_name,omitempty"`
}

type record struct {
	Conversation Conversation        `json:"conversation"`
	Messages     []damessage.Message `json:"messages"`
	API          []apiMessage        `json:"api_messages"`
	Cancel       context.CancelFunc  `json:"-"`
	Pending      []damessage.Message `json:"-"`
	PendingCtx   context.Context     `json:"-"`
}

type browserQueuedMessage struct {
	ID        string      `json:"id"`
	LLM       llm.Message `json:"llm"`
	CreatedAt time.Time   `json:"created_at"`
	Model     string      `json:"model"`
}

type snapshot struct {
	Version       int                           `json:"version"`
	Conversations map[string]persistedRecord    `json:"conversations"`
	Files         map[string]dabackend.FileData `json:"files,omitempty"`
	Directories   []string                      `json:"directories,omitempty"`
}

type persistedRecord struct {
	Conversation Conversation        `json:"conversation"`
	Messages     []damessage.Message `json:"messages"`
	API          []apiMessage        `json:"api_messages"`
}

// CustomModel is the browser-session model configuration accepted by
// Shelley's existing Models dialog. APIKey is intentionally excluded from
// Snapshot: the page owns its shorter-lived secret storage policy.
type CustomModel struct {
	ModelID           string `json:"model_id"`
	DisplayName       string `json:"display_name"`
	ProviderType      string `json:"provider_type"`
	Endpoint          string `json:"endpoint"`
	APIKey            string `json:"api_key"`
	ModelName         string `json:"model_name"`
	MaxTokens         int64  `json:"max_tokens"`
	Tags              string `json:"tags"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	ReasoningSupport  string `json:"reasoning_support"`
	ReasoningMap      string `json:"reasoning_map"`
	ImageSupport      string `json:"image_support"`
	SupportsReasoning bool   `json:"supports_reasoning"`
	SupportsImages    bool   `json:"supports_images"`
}

// App owns browser-local conversations, the virtual workspace, and one
// portable dago agent. Events use the same JSON shapes as Shelley's SSE API.
type App struct {
	mu             sync.RWMutex
	agents         map[string]*dagent.Agent
	customModels   map[string]CustomModel
	providerModels map[string]CustomModel
	webGPUModel    damodel.Chat
	saver          dacheckpoint.Saver
	workspace      Workspace
	backend        dabackend.Backend
	shellExecutor  ShellExecutor
	conversations  map[string]*record
	emit           func(json.RawMessage)
	now            func() time.Time
}

// New constructs a browser-safe Shelley application with the deterministic
// local model. A remote model bridge can replace this model without changing
// the UI transport or persistence protocol.
func New() (*App, error) {
	return newApp(nil, nil)
}

// NewWithShell constructs the browser application with an isolated shell
// executor. The executor never receives host filesystem or process access.
func NewWithShell(executor ShellExecutor) (*App, error) {
	if executor == nil {
		return nil, fmt.Errorf("browser shell executor is required")
	}
	return newApp(executor, nil)
}

// NewWithShellAndSaver constructs the browser application with durable graph
// checkpoints and an isolated browser shell executor.
func NewWithShellAndSaver(executor ShellExecutor, saver dacheckpoint.Saver) (*App, error) {
	if executor == nil {
		return nil, fmt.Errorf("browser shell executor is required")
	}
	if saver == nil {
		return nil, fmt.Errorf("browser checkpoint saver is required")
	}
	return newApp(executor, saver)
}

// NewWithWorkspaceAndSaver constructs the WASM application around a shared
// browser workspace. The workspace owns file persistence independently, so
// application snapshots contain conversations rather than file bodies.
func NewWithWorkspaceAndSaver(workspace Workspace, executor ShellExecutor, saver dacheckpoint.Saver) (*App, error) {
	if workspace == nil {
		return nil, fmt.Errorf("browser workspace is required")
	}
	if executor == nil {
		return nil, fmt.Errorf("browser shell executor is required")
	}
	if saver == nil {
		return nil, fmt.Errorf("browser checkpoint saver is required")
	}
	return newAppWithWorkspace(workspace, executor, saver)
}

// ConfigureWebGPUModel installs the browser worker's local inference bridge.
// Model weights and execution remain owned by the worker; the Go application
// only sees the provider-neutral model contract.
func (app *App) ConfigureWebGPUModel(model damodel.Chat) error {
	if model == nil {
		return fmt.Errorf("browser WebGPU model is required")
	}
	app.mu.RLock()
	backend := app.backend
	app.mu.RUnlock()
	agent, err := newAgent(backend, model, webGPUModelID, app.saver)
	if err != nil {
		return err
	}
	app.mu.Lock()
	app.webGPUModel = model
	app.agents[webGPUModelID] = agent
	app.mu.Unlock()
	return nil
}

func newApp(executor ShellExecutor, saver dacheckpoint.Saver) (*App, error) {
	workspace, err := newBrowserWorkspace(map[string]dabackend.FileData{
		workspaceRoot + "/README.md": {
			Content:  "# Browser workspace\n\nFiles created by the agent are stored in this browser.\n",
			Encoding: dabackend.EncodingUTF8,
		},
	})
	if err != nil {
		return nil, err
	}
	return newAppWithWorkspace(workspace, executor, saver)
}

func newAppWithWorkspace(workspace Workspace, executor ShellExecutor, saver dacheckpoint.Saver) (*App, error) {
	backend := dabackend.Backend(workspace)
	if executor != nil {
		backend = &browserSandbox{Workspace: workspace, execute: executor}
	}
	agent, err := newAgent(backend, newPredictableModel(), modelID, saver)
	if err != nil {
		return nil, err
	}
	return &App{
		agents: map[string]*dagent.Agent{modelID: agent}, customModels: map[string]CustomModel{},
		providerModels: map[string]CustomModel{},
		saver:          saver,
		workspace:      workspace, backend: backend, shellExecutor: executor,
		conversations: map[string]*record{}, now: time.Now,
	}, nil
}

// SetEventSink installs the unified-stream publisher. The sink must return
// quickly; the worker owns buffering and persistence scheduling.
func (app *App) SetEventSink(sink func(json.RawMessage)) {
	app.mu.Lock()
	defer app.mu.Unlock()
	app.emit = sink
}

// Handle dispatches one browser request without opening a network socket.
func (app *App) Handle(request Request) Response {
	method := strings.ToUpper(request.Method)
	if method == "" {
		method = "GET"
	}
	parsed, err := url.Parse(request.URL)
	if err != nil {
		return errorResponse(400, err)
	}
	path := parsed.Path

	switch {
	case method == "GET" && path == "/api/capabilities":
		shellReady := dabackend.CapabilitiesOf(app.backend).Execute
		local := []string{
			"agent", "conversations", "conversation_fork", "conversation_retry", "conversation_queue",
			"conversation_generations", "conversation_compaction", "drafts", "full_text_search",
			"virtual_filesystem", "directory_access", "file_upload", "file_export", "checkpoint_snapshot",
		}
		unavailable := []string{
			"shell", "pty", "host_processes", "unrestricted_host_filesystem", "git", "git_worktrees",
			"server_oauth", "server_notification_channels", "local_browser_control",
		}
		if shellReady {
			local = append(local, "shell")
			unavailable = unavailable[1:]
		}
		return jsonResponse(200, map[string]any{
			"runtime":     "wasm",
			"local":       local,
			"remote":      []string{"model_provider"},
			"unavailable": unavailable,
		})
	case method == "GET" && path == "/api/models", method == "POST" && path == "/api/models/refresh":
		return jsonResponse(200, app.availableModels())
	case method == "GET" && path == "/api/tools":
		return jsonResponse(200, map[string]any{"tools": availableTools(dabackend.CapabilitiesOf(app.backend).Execute)})
	case method == "POST" && path == "/api/browser-shell":
		return app.executeBrowserShell(request.Body)
	case method == "GET" && path == "/api/auth/openai/status":
		app.mu.RLock()
		_, openAIReady := app.providerModels["gpt-5.6-luna"]
		app.mu.RUnlock()
		if openAIReady {
			return jsonResponse(200, map[string]any{"state": "complete", "ready": true, "model_id": "gpt-5.6-luna"})
		}
		return jsonResponse(200, map[string]any{"state": "disabled", "ready": false, "model_id": ""})
	case method == "POST" && path == "/api/browser-openai-key":
		return app.configureBrowserOpenAI(request.Body)
	case method == "GET" && path == "/api/conversations":
		if parsed.Query().Get("search_content") == "true" {
			return jsonResponse(200, app.searchConversationsByArchive(parsed.Query().Get("q"), false))
		}
		return jsonResponse(200, app.listConversations(false, parsed.Query().Get("q")))
	case method == "GET" && path == "/api/conversations/search":
		return jsonResponse(200, app.searchConversations(parsed.Query().Get("q")))
	case method == "GET" && path == "/api/conversations/snapshot":
		items := app.listConversations(false, "")
		return jsonResponse(200, map[string]any{"conversations": items, "hash": listHash(items)})
	case method == "GET" && path == "/api/conversations/archived":
		return jsonResponse(200, app.listConversations(true, ""))
	case method == "POST" && path == "/api/conversations/new":
		return app.newConversation(request.Body)
	case method == "POST" && path == "/api/conversations/draft":
		return app.newDraft(request.Body)
	case method == "POST" && path == "/api/conversations/distill-new-generation":
		return app.distillNewGeneration(request.Body)
	case strings.HasPrefix(path, "/api/conversation-by-slug/") && method == "GET":
		return app.bySlug(strings.TrimPrefix(path, "/api/conversation-by-slug/"))
	case strings.HasPrefix(path, "/api/conversation/"):
		return app.handleConversation(method, path, parsed.Query(), request.Body)
	case method == "GET" && path == "/api/terminals":
		return jsonResponse(200, []any{})
	case method == "GET" && path == "/api/validate-cwd":
		candidate := parsed.Query().Get("path")
		valid := candidate == workspaceRoot || strings.HasPrefix(candidate, workspaceRoot+"/")
		if !valid {
			return jsonResponse(200, map[string]any{"valid": false, "error": "browser paths must be inside " + workspaceRoot})
		}
		return jsonResponse(200, map[string]any{"valid": true})
	case method == "GET" && path == "/api/list-directory":
		return app.listDirectory(parsed.Query().Get("path"))
	case method == "GET" && path == "/api/find-files":
		return app.findFiles(parsed.Query().Get("dir"), parsed.Query().Get("q"))
	case method == "POST" && path == "/api/create-directory":
		return app.createDirectory(request.Body)
	case method == "GET" && path == "/api/read":
		return app.readFile(parsed.Query().Get("path"), false)
	case method == "GET" && path == "/api/read-file":
		return app.readFile(parsed.Query().Get("path"), true)
	case method == "POST" && path == "/api/write-file":
		return app.writeFile(request.Body)
	case path == "/api/user-agents-md":
		if method == "GET" {
			return app.readFile(workspaceRoot+"/AGENTS.md", true)
		}
		if method == "POST" {
			return app.writeUserAgents(bodyOrEmpty(request.Body))
		}
		return errorResponse(405, fmt.Errorf("method not allowed"))
	case method == "GET" && path == "/version-check":
		return jsonResponse(200, map[string]any{"current_version": "browser", "latest_version": "browser", "update_available": false, "should_notify": false})
	case path == "/feature-flags":
		if method != "GET" {
			return changedResponse(204, nil)
		}
		return jsonResponse(200, []any{})
	case method == "GET" && path == "/settings":
		return jsonResponse(200, map[string]string{})
	case method == "GET" && path == "/api/notification-channels":
		return jsonResponse(200, []any{})
	case path == "/api/custom-models":
		return app.handleCustomModels(method, request.Body)
	case method == "POST" && path == "/api/custom-models-test":
		return app.testCustomModel(request.Body)
	case strings.HasPrefix(path, "/api/custom-models/"):
		return app.handleCustomModel(method, strings.TrimPrefix(path, "/api/custom-models/"), request.Body)
	case (method == "GET" || method == "POST") && path == "/api/model-costs":
		return jsonResponse(200, map[string]any{"costs": map[string]any{}})
	default:
		return capabilityResponse(path)
	}
}

func (app *App) newConversation(body string) Response {
	var input struct {
		Message             string          `json:"message"`
		Model               string          `json:"model"`
		Cwd                 string          `json:"cwd"`
		ConversationOptions json.RawMessage `json:"conversation_options"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, fmt.Errorf("decode conversation: %w", err))
	}
	if strings.TrimSpace(input.Message) == "" {
		return errorResponse(400, fmt.Errorf("message is required"))
	}
	conversation, err := app.createRecord(input.Model, input.Cwd, input.ConversationOptions, false, "")
	if err != nil {
		return errorResponse(500, err)
	}
	if !app.prepareTurn(conversation.ConversationID, input.Message) {
		return errorResponse(500, fmt.Errorf("prepare conversation turn"))
	}
	response := changedResponse(202, map[string]string{"conversation_id": conversation.ConversationID})
	response.ContinueConversation = conversation.ConversationID
	return response
}

func (app *App) newDraft(body string) Response {
	var input struct {
		Draft               string          `json:"draft"`
		Model               string          `json:"model"`
		Cwd                 string          `json:"cwd"`
		ConversationOptions json.RawMessage `json:"conversation_options"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, fmt.Errorf("decode draft: %w", err))
	}
	conversation, err := app.createRecord(input.Model, input.Cwd, input.ConversationOptions, true, input.Draft)
	if err != nil {
		return errorResponse(500, err)
	}
	return changedResponse(201, conversation)
}

func (app *App) createRecord(model, cwd string, options json.RawMessage, draft bool, draftText string) (Conversation, error) {
	idText := randomText()
	if len(idText) < 8 {
		return Conversation{}, fmt.Errorf("generate conversation id")
	}
	id := "c" + strings.ToLower(idText[:8])
	if model == "" {
		model = app.defaultModelID()
	}
	if cwd == "" {
		cwd = workspaceRoot
	}
	app.mu.RLock()
	configured := app.agents[model] != nil
	app.mu.RUnlock()
	if !configured {
		return Conversation{}, fmt.Errorf("model %q is not configured in this browser session", model)
	}
	if cwd != workspaceRoot && !strings.HasPrefix(cwd, workspaceRoot+"/") {
		return Conversation{}, fmt.Errorf("browser paths must be inside %s", workspaceRoot)
	}
	optionText := "{}"
	if len(options) > 0 && string(options) != "null" {
		optionText = string(options)
	}
	now := app.now().UTC().Format(time.RFC3339Nano)
	conversation := Conversation{
		ConversationID: id, UserInitiated: true, CreatedAt: now, UpdatedAt: now,
		Cwd: &cwd, Model: &model, ConversationOptions: optionText, CurrentGeneration: 1,
		Tags: "[]", IsDraft: draft, Draft: draftText, QueuedMessages: "[]",
	}
	app.mu.Lock()
	app.conversations[id] = &record{Conversation: conversation}
	app.mu.Unlock()
	return conversation, nil
}

func (app *App) handleConversation(method, path string, query url.Values, body string) Response {
	tail := strings.TrimPrefix(path, "/api/conversation/")
	id, action, _ := strings.Cut(tail, "/")
	if id == "" {
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	switch {
	case method == "GET" && action == "":
		return app.getConversation(id, query)
	case method == "POST" && action == "chat":
		var input struct {
			Message             string          `json:"message"`
			Model               string          `json:"model"`
			Cwd                 string          `json:"cwd"`
			ConversationOptions json.RawMessage `json:"conversation_options"`
			Queue               bool            `json:"queue"`
		}
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			return errorResponse(400, err)
		}
		if strings.HasPrefix(strings.TrimSpace(input.Message), "/model") {
			return app.changeModel(id, input.Message)
		}
		if input.Queue {
			return app.queueTurn(id, input.Message, input.Model)
		}
		if err := app.applyDraftSelections(id, input.Model, input.Cwd, input.ConversationOptions); err != nil {
			return errorResponse(400, err)
		}
		if !app.prepareTurn(id, input.Message) {
			return errorResponse(409, fmt.Errorf("conversation is already working"))
		}
		response := changedResponse(202, map[string]string{"status": "accepted"})
		response.ContinueConversation = id
		return response
	case method == "POST" && action == "cancel":
		app.mu.Lock()
		record := app.conversations[id]
		if record != nil && record.Cancel != nil {
			record.Cancel()
		}
		app.mu.Unlock()
		return changedResponse(200, map[string]string{"status": "cancelled"})
	case method == "POST" && action == "cancel-queued":
		return app.cancelQueued(id, query.Get("queued_id"))
	case method == "POST" && action == "fork":
		return app.forkConversation(id, body)
	case method == "POST" && action == "retry":
		return app.retryConversation(id, "")
	case method == "POST" && action == "continue":
		var input struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal([]byte(body), &input)
		return app.retryConversation(id, input.Model)
	case method == "POST" && action == "new-generation":
		return app.startNewGeneration(id)
	case method == "PUT" && action == "draft":
		return app.updateDraft(id, body)
	case method == "POST" && action == "archive":
		return app.setArchived(id, true)
	case method == "POST" && action == "unarchive":
		return app.setArchived(id, false)
	case method == "POST" && action == "delete":
		app.mu.Lock()
		if record := app.conversations[id]; record != nil && record.Cancel != nil {
			record.Cancel()
		}
		saver := app.saver
		app.mu.Unlock()
		if saver != nil {
			if err := saver.DeleteThread(context.Background(), id); err != nil {
				return errorResponse(500, err)
			}
		}
		app.mu.Lock()
		delete(app.conversations, id)
		app.mu.Unlock()
		return changedResponse(200, map[string]string{"status": "deleted"})
	case method == "POST" && action == "rename":
		var input struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			return errorResponse(400, err)
		}
		return app.rename(id, input.Slug)
	case method == "POST" && action == "tags":
		var input struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			return errorResponse(400, err)
		}
		return app.setTags(id, input.Tags)
	case method == "GET" && action == "subagents":
		return jsonResponse(200, []any{})
	default:
		return capabilityResponse(action)
	}
}

func (app *App) applyDraftSelections(id, model, cwd string, options json.RawMessage) error {
	app.mu.Lock()
	defer app.mu.Unlock()
	record := app.conversations[id]
	if record == nil {
		return fmt.Errorf("conversation not found")
	}
	if !record.Conversation.IsDraft {
		return nil
	}
	if model != "" {
		if app.agents[model] == nil {
			return fmt.Errorf("model %q is not configured in this browser session", model)
		}
		record.Conversation.Model = pointer(model)
	}
	if cwd != "" {
		if cwd != workspaceRoot && !strings.HasPrefix(cwd, workspaceRoot+"/") {
			return fmt.Errorf("browser paths must be inside %s", workspaceRoot)
		}
		record.Conversation.Cwd = pointer(cwd)
	}
	if len(options) > 0 && string(options) != "null" {
		record.Conversation.ConversationOptions = string(options)
	}
	return nil
}

func (app *App) updateDraft(id, body string) Response {
	var input struct {
		Draft *string `json:"draft"`
		Model *string `json:"model"`
		Cwd   *string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	record := app.conversations[id]
	if record == nil || !record.Conversation.IsDraft {
		return errorResponse(404, fmt.Errorf("draft conversation not found"))
	}
	if input.Model != nil {
		if app.agents[*input.Model] == nil {
			return errorResponse(400, fmt.Errorf("model %q is not configured in this browser session", *input.Model))
		}
		record.Conversation.Model = pointer(*input.Model)
	}
	if input.Cwd != nil {
		if *input.Cwd != workspaceRoot && !strings.HasPrefix(*input.Cwd, workspaceRoot+"/") {
			return errorResponse(400, fmt.Errorf("browser paths must be inside %s", workspaceRoot))
		}
		record.Conversation.Cwd = pointer(*input.Cwd)
	}
	if input.Draft != nil {
		record.Conversation.Draft = *input.Draft
	}
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	return changedResponse(200, record.Conversation)
}

func (app *App) changeModel(id, command string) Response {
	model := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "/model"))
	if model == "" {
		return errorResponse(400, fmt.Errorf("usage: /model <model>"))
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.agents[model] == nil {
		return errorResponse(400, fmt.Errorf("model %q is not configured in this browser session", model))
	}
	record := app.conversations[id]
	if record == nil {
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	if record.Conversation.AgentWorking {
		return errorResponse(409, fmt.Errorf("conversation is already working"))
	}
	record.Conversation.Model = pointer(model)
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	return changedResponse(200, map[string]string{"status": "changed", "model": model})
}

func (app *App) forkConversation(id, body string) Response {
	var input struct {
		MessageID  string `json:"message_id"`
		SequenceID int64  `json:"sequence_id"`
	}
	if strings.TrimSpace(body) != "" {
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			return errorResponse(400, err)
		}
	}
	app.mu.RLock()
	source := app.conversations[id]
	if source == nil {
		app.mu.RUnlock()
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	conversation := source.Conversation
	apiMessages := append([]apiMessage(nil), source.API...)
	nativeMessages := cloneMessages(source.Messages)
	app.mu.RUnlock()
	if len(apiMessages) == 0 {
		return errorResponse(400, fmt.Errorf("conversation has no messages to fork"))
	}
	cutoff := input.SequenceID
	if input.MessageID != "" {
		cutoff = 0
		for _, message := range apiMessages {
			if message.MessageID == input.MessageID {
				cutoff = message.SequenceID
				break
			}
		}
		if cutoff == 0 {
			return errorResponse(404, fmt.Errorf("message not found"))
		}
	}
	if cutoff <= 0 || cutoff > apiMessages[len(apiMessages)-1].SequenceID {
		cutoff = apiMessages[len(apiMessages)-1].SequenceID
	}
	generation := int64(0)
	selectedIDs := map[string]bool{}
	for _, message := range apiMessages {
		if message.SequenceID == cutoff {
			generation = message.Generation
			break
		}
	}
	if generation == 0 {
		return errorResponse(400, fmt.Errorf("invalid fork point"))
	}
	selectedAPI := make([]apiMessage, 0)
	for _, message := range apiMessages {
		if message.Generation == generation && message.SequenceID <= cutoff {
			selectedAPI = append(selectedAPI, message)
			selectedIDs[message.MessageID] = true
		}
	}
	if len(selectedAPI) == 0 {
		return errorResponse(400, fmt.Errorf("invalid fork point"))
	}
	selectedNative := make([]damessage.Message, 0, len(selectedAPI))
	for _, message := range nativeMessages {
		if selectedIDs[message.ID] {
			selectedNative = append(selectedNative, message.Clone())
		}
	}
	created, err := app.createRecord(pointerText(conversation.Model), pointerText(conversation.Cwd), json.RawMessage(conversation.ConversationOptions), false, "")
	if err != nil {
		return errorResponse(500, err)
	}
	app.mu.Lock()
	forked := app.conversations[created.ConversationID]
	forked.Messages = selectedNative
	forked.Conversation.CurrentGeneration = 1
	forked.Conversation.Slug = app.uniqueForkSlugLocked(conversation.Slug)
	for index, message := range selectedAPI {
		message.ConversationID = created.ConversationID
		message.SequenceID = int64(index + 1)
		message.Generation = 1
		forked.API = append(forked.API, message)
	}
	created = forked.Conversation
	app.mu.Unlock()
	return changedResponse(201, created)
}

func (app *App) uniqueForkSlugLocked(source *string) *string {
	base := "fork"
	if source != nil && *source != "" {
		base = slugify(*source + "-fork")
	}
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		available := true
		for _, record := range app.conversations {
			if record.Conversation.Slug != nil && *record.Conversation.Slug == candidate {
				available = false
				break
			}
		}
		if available {
			return pointer(candidate)
		}
	}
}

func (app *App) retryConversation(id, model string) Response {
	app.mu.RLock()
	record := app.conversations[id]
	if record == nil {
		app.mu.RUnlock()
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	if record.Conversation.AgentWorking {
		app.mu.RUnlock()
		return errorResponse(409, fmt.Errorf("conversation is already working"))
	}
	hasError := len(record.API) > 0 && record.API[len(record.API)-1].Type == "error"
	app.mu.RUnlock()
	if !hasError {
		return jsonResponse(202, map[string]string{"status": "not_applicable"})
	}
	if model != "" {
		app.mu.RLock()
		configured := app.agents[model] != nil
		app.mu.RUnlock()
		if !configured {
			return errorResponse(400, fmt.Errorf("model %q is not configured in this browser session", model))
		}
	}
	if app.saver != nil {
		if err := app.saver.DeleteThread(context.Background(), id); err != nil {
			return errorResponse(500, err)
		}
	}
	app.mu.Lock()
	record = app.conversations[id]
	if model != "" {
		record.Conversation.Model = pointer(model)
	}
	for len(record.Messages) > 0 && strings.HasPrefix(record.Messages[len(record.Messages)-1].TextContent(), "Agent error: ") {
		record.Messages = record.Messages[:len(record.Messages)-1]
	}
	if len(record.Messages) == 0 {
		app.mu.Unlock()
		return errorResponse(409, fmt.Errorf("conversation has no request to retry"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	record.Cancel = cancel
	record.Pending = cloneMessages(record.Messages)
	record.PendingCtx = ctx
	record.Conversation.AgentWorking = true
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	app.mu.Unlock()
	app.publish(app.streamFrame(id, nil, true))
	response := changedResponse(202, map[string]string{"status": "retrying"})
	response.ContinueConversation = id
	return response
}

func (app *App) startNewGeneration(id string) Response {
	app.mu.RLock()
	record := app.conversations[id]
	if record == nil {
		app.mu.RUnlock()
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	if record.Conversation.AgentWorking {
		app.mu.RUnlock()
		return errorResponse(409, fmt.Errorf("conversation is already working"))
	}
	app.mu.RUnlock()
	if app.saver != nil {
		if err := app.saver.DeleteThread(context.Background(), id); err != nil {
			return errorResponse(500, err)
		}
	}
	app.mu.Lock()
	record = app.conversations[id]
	record.Messages = nil
	record.Pending = nil
	record.PendingCtx = nil
	record.Conversation.CurrentGeneration++
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	conversation := record.Conversation
	app.mu.Unlock()
	app.publish(app.streamFrame(id, nil, false))
	return changedResponse(200, conversation)
}

func (app *App) distillNewGeneration(body string) Response {
	var input struct {
		SourceConversationID string `json:"source_conversation_id"`
		Model                string `json:"model"`
		Cwd                  string `json:"cwd"`
		Method               string `json:"method"`
		Instructions         string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, err)
	}
	if input.SourceConversationID == "" {
		return errorResponse(400, fmt.Errorf("source_conversation_id is required"))
	}
	if input.Method != "" && input.Method != "default" && input.Method != "compact" {
		return errorResponse(400, fmt.Errorf("unknown distill method %q", input.Method))
	}
	if input.Cwd != "" && input.Cwd != workspaceRoot && !strings.HasPrefix(input.Cwd, workspaceRoot+"/") {
		return errorResponse(400, fmt.Errorf("browser paths must be inside %s", workspaceRoot))
	}
	app.mu.RLock()
	record := app.conversations[input.SourceConversationID]
	if record == nil {
		app.mu.RUnlock()
		return errorResponse(404, fmt.Errorf("source conversation not found"))
	}
	if record.Conversation.AgentWorking {
		app.mu.RUnlock()
		return errorResponse(409, fmt.Errorf("conversation is already working"))
	}
	selectedModel := input.Model
	if selectedModel == "" {
		selectedModel = pointerText(record.Conversation.Model)
	}
	var model damodel.Chat
	if agentModel, ok := app.providerModels[selectedModel]; ok {
		model, _ = customModelChat(agentModel)
	} else if customModel, ok := app.customModels[selectedModel]; ok {
		model, _ = customModelChat(customModel)
	} else if selectedModel == modelID {
		model = newPredictableModel()
	} else if selectedModel == webGPUModelID {
		model = app.webGPUModel
	}
	history := cloneMessages(record.Messages)
	app.mu.RUnlock()
	if model == nil {
		return errorResponse(400, fmt.Errorf("model %q is not configured in this browser session", selectedModel))
	}
	var transcript strings.Builder
	for _, message := range history {
		text := strings.TrimSpace(message.TextContent())
		if text == "" {
			continue
		}
		fmt.Fprintf(&transcript, "%s: %s\n", message.Role, text)
	}
	prompt := "Summarize this conversation for another coding agent. Preserve decisions, file paths, changes, unresolved work, and constraints."
	if input.Instructions != "" {
		prompt += " Additional instructions: " + input.Instructions
	}
	prompt += "\n\n" + transcript.String()
	result, err := model.Invoke(context.Background(), damodel.Request{Messages: []damessage.Message{damessage.Human(prompt)}})
	if err != nil {
		return errorResponse(500, fmt.Errorf("compact conversation: %w", err))
	}
	summary := strings.TrimSpace(result.Message.TextContent())
	if summary == "" {
		return errorResponse(500, fmt.Errorf("compact conversation returned an empty summary"))
	}
	if app.saver != nil {
		if err := app.saver.DeleteThread(context.Background(), input.SourceConversationID); err != nil {
			return errorResponse(500, err)
		}
	}
	app.mu.Lock()
	record = app.conversations[input.SourceConversationID]
	record.Conversation.CurrentGeneration++
	record.Conversation.Model = pointer(selectedModel)
	if input.Cwd != "" {
		record.Conversation.Cwd = pointer(input.Cwd)
	}
	message := damessage.Assistant("Conversation summary:\n\n" + summary)
	message.ID = newMessageID()
	record.Messages = []damessage.Message{message}
	projected := projectMessage(input.SourceConversationID, int64(len(record.API)+1), record.Conversation.CurrentGeneration, selectedModel, message, app.now())
	record.API = append(record.API, projected)
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	generation := record.Conversation.CurrentGeneration
	app.mu.Unlock()
	app.publish(app.streamFrame(input.SourceConversationID, []apiMessage{projected}, false))
	return changedResponse(201, map[string]any{
		"status": "created", "conversation_id": input.SourceConversationID, "current_generation": generation,
	})
}

func (app *App) prepareTurn(id, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	app.mu.Lock()
	record := app.conversations[id]
	if record == nil || record.Conversation.AgentWorking {
		app.mu.Unlock()
		return false
	}
	if record.Conversation.IsDraft {
		record.Conversation.IsDraft = false
		record.Conversation.Draft = ""
	}
	human := damessage.Human(text)
	human.ID = newMessageID()
	record.Messages = append(record.Messages, human)
	model := pointerText(record.Conversation.Model)
	apiHuman := projectMessage(id, int64(len(record.API)+1), record.Conversation.CurrentGeneration, model, human, app.now())
	record.API = append(record.API, apiHuman)
	record.Conversation.AgentWorking = true
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	if record.Conversation.Slug == nil {
		slug := slugify(text)
		record.Conversation.Slug = &slug
	}
	ctx, cancel := context.WithCancel(context.Background())
	record.Cancel = cancel
	record.Pending = cloneMessages(record.Messages)
	record.PendingCtx = ctx
	app.mu.Unlock()

	app.publish(app.streamFrame(id, []apiMessage{apiHuman}, true))
	return true
}

func (app *App) queueTurn(id, text, model string) Response {
	text = strings.TrimSpace(text)
	if text == "" {
		return errorResponse(400, fmt.Errorf("message is required"))
	}
	app.mu.Lock()
	record := app.conversations[id]
	if record == nil {
		app.mu.Unlock()
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	if !record.Conversation.AgentWorking {
		app.mu.Unlock()
		if model != "" {
			if err := app.applyDraftSelections(id, model, "", nil); err != nil {
				return errorResponse(400, err)
			}
		}
		if !app.prepareTurn(id, text) {
			return errorResponse(409, fmt.Errorf("conversation is already working"))
		}
		response := changedResponse(202, map[string]string{"status": "accepted"})
		response.ContinueConversation = id
		return response
	}
	if model == "" {
		model = pointerText(record.Conversation.Model)
	}
	queued, err := parseBrowserQueue(record.Conversation.QueuedMessages)
	if err != nil {
		app.mu.Unlock()
		return errorResponse(500, err)
	}
	queued = append(queued, browserQueuedMessage{
		ID:        newMessageID(),
		LLM:       llm.Message{Role: llm.MessageRoleUser, Content: llm.TextContent(text)},
		CreatedAt: app.now().UTC(), Model: model,
	})
	record.Conversation.QueuedMessages = marshalBrowserQueue(queued)
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	app.mu.Unlock()
	app.publish(app.streamFrame(id, nil, true))
	return changedResponse(202, map[string]string{"status": "queued"})
}

func parseBrowserQueue(raw string) ([]browserQueuedMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var queued []browserQueuedMessage
	if err := json.Unmarshal([]byte(raw), &queued); err != nil {
		return nil, fmt.Errorf("decode queued messages: %w", err)
	}
	return queued, nil
}

func marshalBrowserQueue(queued []browserQueuedMessage) string {
	if len(queued) == 0 {
		return "[]"
	}
	raw, _ := json.Marshal(queued)
	return string(raw)
}

func browserQueuedText(message browserQueuedMessage) string {
	var result strings.Builder
	for _, content := range message.LLM.Content {
		if content.Type == llm.ContentTypeText {
			result.WriteString(content.Text)
		}
	}
	return result.String()
}

func (app *App) cancelQueued(id, queuedID string) Response {
	app.mu.Lock()
	record := app.conversations[id]
	if record == nil {
		app.mu.Unlock()
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	queued, err := parseBrowserQueue(record.Conversation.QueuedMessages)
	if err != nil {
		app.mu.Unlock()
		return errorResponse(500, err)
	}
	if queuedID == "" {
		queued = nil
	} else {
		kept := queued[:0]
		for _, message := range queued {
			if message.ID != queuedID {
				kept = append(kept, message)
			}
		}
		queued = kept
	}
	record.Conversation.QueuedMessages = marshalBrowserQueue(queued)
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	working := record.Conversation.AgentWorking
	app.mu.Unlock()
	app.publish(app.streamFrame(id, nil, working))
	return changedResponse(200, map[string]string{"status": "ok"})
}

func (app *App) drainQueued(id string) {
	app.mu.Lock()
	record := app.conversations[id]
	if record == nil || record.Conversation.AgentWorking {
		app.mu.Unlock()
		return
	}
	queued, err := parseBrowserQueue(record.Conversation.QueuedMessages)
	if err != nil || len(queued) == 0 {
		app.mu.Unlock()
		return
	}
	next := queued[0]
	record.Conversation.QueuedMessages = marshalBrowserQueue(queued[1:])
	if next.Model != "" && app.agents[next.Model] != nil {
		record.Conversation.Model = pointer(next.Model)
	}
	app.mu.Unlock()
	if app.prepareTurn(id, browserQueuedText(next)) {
		app.Continue(id)
	}
}

// Continue starts a turn that Handle prepared. Keeping this explicit lets
// transports acknowledge the request before a fast local model can finish.
func (app *App) Continue(id string) bool {
	app.mu.Lock()
	record := app.conversations[id]
	if record == nil || len(record.Pending) == 0 || record.Cancel == nil {
		app.mu.Unlock()
		return false
	}
	history := cloneMessages(record.Pending)
	record.Pending = nil
	ctx := record.PendingCtx
	record.PendingCtx = nil
	app.mu.Unlock()
	go app.runTurn(ctx, id, history)
	return true
}

func (app *App) runTurn(ctx context.Context, id string, history []damessage.Message) {
	app.mu.RLock()
	record := app.conversations[id]
	selectedModel := ""
	if record != nil {
		selectedModel = pointerText(record.Conversation.Model)
	}
	agent := app.agents[selectedModel]
	saver := app.saver
	app.mu.RUnlock()
	var result dagent.Result
	var err error
	if agent == nil {
		err = fmt.Errorf("model %q is not configured in this browser session", selectedModel)
	} else {
		input := history
		if saver != nil {
			tuple, checkpointErr := saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: id})
			if checkpointErr != nil {
				err = checkpointErr
			} else if tuple != nil && len(history) > 0 {
				input = history[len(history)-1:]
			}
		}
		if err == nil {
			stream := agent.Stream(ctx, dagent.Input{
				Config: dacheckpoint.Config{ThreadID: id}, Messages: input,
			})
			defer stream.Close()
			projected := make(map[string]bool)
			for {
				event, nextErr := stream.Next(ctx)
				if nextErr == io.EOF {
					break
				}
				if nextErr != nil {
					err = nextErr
					break
				}
				switch event.Mode {
				case dagent.EventUpdate:
					if event.Node != "model" && event.Node != "tools" {
						continue
					}
					messages, ok := event.Update[dagent.MessagesKey].([]damessage.Message)
					if !ok {
						continue
					}
					additions := app.projectRuntimeMessages(id, selectedModel, messages, projected)
					if len(additions) > 0 {
						app.publish(app.streamFrame(id, additions, true))
					}
				case dagent.EventToolProgress:
					if event.ToolProgress != nil {
						app.publish(map[string]any{
							"conversation_id": id,
							"tool_progress": map[string]any{
								"tool_use_id": event.ToolProgress.CallID,
								"tool_name":   event.ToolProgress.Name,
								"output":      event.ToolProgress.Output,
							},
						})
					}
				case dagent.EventToken:
					if event.Chunk != nil {
						app.publishToken(id, *event.Chunk)
					}
				}
			}
			if err == nil {
				result, err = stream.Result(ctx)
			}
		}
	}
	app.mu.Lock()
	record = app.conversations[id]
	if record == nil {
		app.mu.Unlock()
		return
	}
	record.Cancel = nil
	record.Conversation.AgentWorking = false
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	var additions []apiMessage
	if err == nil {
		record.Messages = cloneMessages(result.Messages)
	} else if !errors.Is(err, context.Canceled) {
		message := damessage.Assistant("Agent error: " + err.Error())
		message.ID = newMessageID()
		record.Messages = append(record.Messages, message)
		projected := projectMessage(id, int64(len(record.API)+1), record.Conversation.CurrentGeneration, selectedModel, message, app.now())
		projected.Type = "error"
		record.API = append(record.API, projected)
		additions = append(additions, projected)
	}
	app.mu.Unlock()
	app.publish(app.streamFrame(id, additions, false))
	app.drainQueued(id)
}

func (app *App) projectRuntimeMessages(id, model string, messages []damessage.Message, seen map[string]bool) []apiMessage {
	app.mu.Lock()
	defer app.mu.Unlock()
	record := app.conversations[id]
	if record == nil {
		return nil
	}
	additions := make([]apiMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role != damessage.RoleAssistant && message.Role != damessage.RoleTool {
			continue
		}
		key := message.ID
		if key == "" {
			key = string(message.Role) + ":" + message.ToolCallID + ":" + message.TextContent()
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		projected := projectMessage(id, int64(len(record.API)+1), record.Conversation.CurrentGeneration, model, message, app.now())
		record.API = append(record.API, projected)
		additions = append(additions, projected)
	}
	return additions
}

func (app *App) publishToken(id string, chunk damodel.Chunk) {
	for index, block := range chunk.MessageDelta.Content {
		blockIndex := index
		if block.Index != nil {
			blockIndex = *block.Index
		}
		delta := map[string]any{"index": blockIndex}
		switch block.Type {
		case damessage.BlockText:
			if block.Text == "" {
				continue
			}
			delta["type"] = "text"
			delta["text"] = block.Text
		case damessage.BlockReasoning:
			if block.Reasoning == "" {
				continue
			}
			delta["type"] = "thinking"
			delta["text"] = block.Reasoning
		default:
			continue
		}
		app.publish(map[string]any{"conversation_id": id, "stream_delta": delta})
	}
}

func (app *App) streamFrame(id string, messages []apiMessage, working bool) map[string]any {
	app.mu.RLock()
	selectedModel := modelID
	if record := app.conversations[id]; record != nil && record.Conversation.Model != nil {
		selectedModel = *record.Conversation.Model
	}
	app.mu.RUnlock()
	items := app.listConversations(false, "")
	return map[string]any{
		"conversation_id":    id,
		"messages":           messages,
		"conversation_state": map[string]any{"conversation_id": id, "working": working, "model": selectedModel},
		"conversation_list_patch": map[string]any{
			"old_hash": nil, "new_hash": listHash(items), "reset": true,
			"patch": []map[string]any{{"op": "replace", "path": "", "value": items}},
			"at":    app.now().UTC().Format(time.RFC3339Nano),
		},
	}
}

func (app *App) publish(value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	app.mu.RLock()
	sink := app.emit
	app.mu.RUnlock()
	if sink != nil {
		sink(raw)
	}
}

func (app *App) getConversation(id string, query url.Values) Response {
	app.mu.RLock()
	record := app.conversations[id]
	if record == nil {
		app.mu.RUnlock()
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	messages := append([]apiMessage(nil), record.API...)
	conversation := record.Conversation
	maxSequenceID := len(record.API)
	app.mu.RUnlock()
	if since := query.Get("last_sequence_id"); since != "" {
		var cursor int64
		_, _ = fmt.Sscan(since, &cursor)
		filtered := messages[:0]
		for _, message := range messages {
			if message.SequenceID > cursor {
				filtered = append(filtered, message)
			}
		}
		messages = filtered
	}
	return jsonResponse(200, map[string]any{
		"conversation_id": id, "messages": messages, "conversation": conversation,
		"context_window_size": approximateContext(messages), "max_sequence_id": maxSequenceID,
	})
}

func (app *App) listConversations(archived bool, query string) []conversationWithState {
	query = strings.ToLower(strings.TrimSpace(query))
	app.mu.RLock()
	defer app.mu.RUnlock()
	items := make([]conversationWithState, 0, len(app.conversations))
	for _, record := range app.conversations {
		if record.Conversation.Archived != archived {
			continue
		}
		preview := record.Conversation.Draft
		for _, message := range record.Messages {
			if message.Role == damessage.RoleHuman && message.TextContent() != "" {
				preview = message.TextContent()
				break
			}
		}
		if query != "" && !strings.Contains(strings.ToLower(preview+" "+pointerText(record.Conversation.Slug)), query) {
			continue
		}
		items = append(items, conversationWithState{
			Conversation: record.Conversation, Working: record.Conversation.AgentWorking,
			Preview: preview, MaxSequenceID: int64(len(record.API)), Participants: []string{},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	return items
}

func (app *App) searchConversations(query string) []conversationWithState {
	return app.searchConversationsByArchive(query)
}

func (app *App) searchConversationsByArchive(query string, archived ...bool) []conversationWithState {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		if len(archived) > 0 {
			return app.listConversations(archived[0], "")
		}
		active := app.listConversations(false, "")
		return append(active, app.listConversations(true, "")...)
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	items := make([]conversationWithState, 0)
	for _, record := range app.conversations {
		if len(archived) > 0 && record.Conversation.Archived != archived[0] {
			continue
		}
		haystack := pointerText(record.Conversation.Slug) + " " + record.Conversation.Draft
		for _, message := range record.Messages {
			haystack += " " + message.TextContent()
		}
		if !strings.Contains(strings.ToLower(haystack), query) {
			continue
		}
		preview := record.Conversation.Draft
		for _, message := range record.Messages {
			if message.Role == damessage.RoleHuman && message.TextContent() != "" {
				preview = message.TextContent()
				break
			}
		}
		items = append(items, conversationWithState{
			Conversation: record.Conversation, Working: record.Conversation.AgentWorking,
			Preview: preview, MaxSequenceID: int64(len(record.API)), Participants: []string{},
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	return items
}

func (app *App) bySlug(slug string) Response {
	app.mu.RLock()
	defer app.mu.RUnlock()
	for _, record := range app.conversations {
		if record.Conversation.Slug != nil && *record.Conversation.Slug == slug {
			return jsonResponse(200, record.Conversation)
		}
	}
	return errorResponse(404, fmt.Errorf("conversation not found"))
}

func (app *App) setArchived(id string, archived bool) Response {
	app.mu.Lock()
	defer app.mu.Unlock()
	record := app.conversations[id]
	if record == nil {
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	record.Conversation.Archived = archived
	record.Conversation.UpdatedAt = app.now().UTC().Format(time.RFC3339Nano)
	return changedResponse(200, record.Conversation)
}

func (app *App) rename(id, slug string) Response {
	slug = slugify(slug)
	if slug == "" {
		return errorResponse(400, fmt.Errorf("slug is required"))
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	for candidate, existing := range app.conversations {
		if candidate != id && existing.Conversation.Slug != nil && *existing.Conversation.Slug == slug {
			return errorResponse(409, fmt.Errorf("slug already exists"))
		}
	}
	record := app.conversations[id]
	if record == nil {
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	record.Conversation.Slug = &slug
	return changedResponse(200, record.Conversation)
}

func (app *App) setTags(id string, tags []string) Response {
	raw, err := json.Marshal(tags)
	if err != nil {
		return errorResponse(400, err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	record := app.conversations[id]
	if record == nil {
		return errorResponse(404, fmt.Errorf("conversation not found"))
	}
	record.Conversation.Tags = string(raw)
	return changedResponse(200, record.Conversation)
}

func (app *App) listDirectory(directory string) Response {
	if directory == "" {
		directory = workspaceRoot
	}
	listed, err := app.workspace.List(context.Background(), directory)
	if err != nil {
		return errorResponse(400, err)
	}
	entries := make([]map[string]any, 0, len(listed.Entries))
	seen := make(map[string]bool)
	for _, entry := range listed.Entries {
		name := strings.TrimSuffix(entry.Path, "/")
		if index := strings.LastIndex(name, "/"); index >= 0 {
			name = name[index+1:]
		}
		entries = append(entries, map[string]any{"name": name, "is_dir": entry.IsDir})
		seen[name] = true
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i]["name"].(string) < entries[j]["name"].(string) })
	parent := directory
	if directory != workspaceRoot {
		parent = directory[:strings.LastIndex(directory, "/")]
		if parent == "" {
			parent = workspaceRoot
		}
	}
	return jsonResponse(200, map[string]any{"path": directory, "parent": parent, "entries": entries})
}

func (app *App) createDirectory(body string) Response {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, err)
	}
	if err := app.workspace.CreateDirectory(context.Background(), input.Path); err != nil {
		return errorResponse(400, err)
	}
	return changedResponse(201, map[string]string{"path": strings.TrimSuffix(input.Path, "/")})
}

func (app *App) readFile(path string, wrapped bool) Response {
	result, err := app.workspace.Read(context.Background(), path, 0, 8*1024*1024)
	if err != nil || result.Data == nil {
		if err == nil {
			err = fmt.Errorf("file not found")
		}
		return errorResponse(404, err)
	}
	if wrapped {
		return jsonResponse(200, map[string]string{"content": result.Data.Content})
	}
	raw, _ := json.Marshal(result.Data.Content)
	return Response{Status: 200, Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}, Body: raw}
}

func (app *App) writeFile(body string) Response {
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, err)
	}
	if input.Path == "" || !strings.HasPrefix(input.Path, workspaceRoot+"/") {
		return errorResponse(400, fmt.Errorf("browser file must be inside %s", workspaceRoot))
	}
	if _, err := app.workspace.Write(context.Background(), input.Path, input.Content); err != nil {
		return errorResponse(400, err)
	}
	return changedResponse(200, map[string]string{"path": input.Path})
}

func (app *App) writeUserAgents(body string) Response {
	var input struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, err)
	}
	if _, err := app.workspace.Write(context.Background(), workspaceRoot+"/AGENTS.md", input.Content); err != nil {
		return errorResponse(500, err)
	}
	return changedResponse(200, map[string]string{"status": "saved"})
}

func bodyOrEmpty(body string) string {
	if strings.TrimSpace(body) == "" {
		return `{"content":""}`
	}
	return body
}

func (app *App) findFiles(directory, query string) Response {
	if directory == "" {
		directory = workspaceRoot
	}
	result, err := app.workspace.Glob(context.Background(), "**", directory)
	if err != nil {
		return errorResponse(400, err)
	}
	query = strings.ToLower(query)
	matches := make([]map[string]any, 0, len(result.Matches))
	for _, item := range result.Matches {
		relative := strings.TrimPrefix(item.Path, strings.TrimSuffix(directory, "/")+"/")
		if query != "" && !strings.Contains(strings.ToLower(relative), query) {
			continue
		}
		matches = append(matches, map[string]any{"path": relative})
	}
	return jsonResponse(200, map[string]any{"dir": directory, "query": query, "matches": matches, "total": len(matches), "truncated": false})
}

func (app *App) executeBrowserShell(body string) Response {
	sandbox, ok := dabackend.SandboxOf(app.backend)
	if !ok {
		return errorResponse(501, fmt.Errorf("browser shell is unavailable"))
	}
	var input struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd,omitempty"`
		Timeout *int   `json:"timeout,omitempty"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, fmt.Errorf("decode browser shell request: %w", err))
	}
	if input.Cwd != "" && input.Cwd != workspaceRoot {
		return errorResponse(400, fmt.Errorf("browser shell working directory must be %s", workspaceRoot))
	}
	var timeout *time.Duration
	if input.Timeout != nil {
		if *input.Timeout < 0 || *input.Timeout > 3600 {
			return errorResponse(400, fmt.Errorf("browser shell timeout must be between 0 and 3600 seconds"))
		}
		value := time.Duration(*input.Timeout) * time.Second
		timeout = &value
	}
	result, err := dabackend.ExecuteSandbox(context.Background(), sandbox, input.Command, dabackend.ExecuteOptions{Timeout: timeout})
	if err != nil {
		return errorResponse(500, err)
	}
	return changedResponse(200, result)
}

func (app *App) configureBrowserOpenAI(body string) Response {
	var input struct {
		APIKey   string `json:"api_key"`
		Endpoint string `json:"endpoint,omitempty"`
	}
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		return errorResponse(400, fmt.Errorf("decode OpenAI key: %w", err))
	}
	input.APIKey = strings.TrimSpace(input.APIKey)
	if input.APIKey == "" {
		return errorResponse(400, fmt.Errorf("OpenAI API key is required"))
	}
	endpoint := strings.TrimSpace(input.Endpoint)
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1"
	}
	specs := []struct {
		id, displayName string
	}{
		{"gpt-5.6-luna", "GPT-5.6 Luna"},
		{"gpt-5.6-terra", "GPT-5.6 Terra"},
		{"gpt-5.6-sol", "GPT-5.6 Sol"},
	}
	models := make(map[string]CustomModel, len(specs))
	agents := make(map[string]*dagent.Agent, len(specs))
	for _, spec := range specs {
		model := CustomModel{
			ModelID: spec.id, DisplayName: spec.displayName, ProviderType: "openai-responses",
			Endpoint: endpoint, APIKey: input.APIKey, ModelName: spec.id,
			MaxTokens: 1050000, ReasoningSupport: "yes", ImageSupport: "yes",
			SupportsReasoning: true, SupportsImages: true,
		}
		chat, err := customModelChat(model)
		if err != nil {
			return errorResponse(400, err)
		}
		agent, err := newAgent(app.backend, chat, model.ModelID, app.saver)
		if err != nil {
			return errorResponse(400, err)
		}
		models[model.ModelID] = model
		agents[model.ModelID] = agent
	}
	app.mu.Lock()
	for id := range app.providerModels {
		delete(app.agents, id)
	}
	app.providerModels = models
	for id, agent := range agents {
		app.agents[id] = agent
	}
	app.mu.Unlock()
	return jsonResponse(200, map[string]any{"status": "configured", "models": app.availableModels()})
}

func (app *App) handleCustomModels(method, body string) Response {
	switch method {
	case "GET":
		return jsonResponse(200, publicCustomModels(app.listCustomModels()))
	case "POST":
		var model CustomModel
		if err := json.Unmarshal([]byte(body), &model); err != nil {
			return errorResponse(400, fmt.Errorf("decode model: %w", err))
		}
		if model.ModelID == "" {
			model.ModelID = app.uniqueModelID(model.ModelName)
		}
		saved, err := app.setCustomModel(model, false)
		if err != nil {
			return errorResponse(400, err)
		}
		return jsonResponse(201, publicCustomModel(saved))
	default:
		return errorResponse(405, fmt.Errorf("method not allowed"))
	}
}

func (app *App) handleCustomModel(method, tail, body string) Response {
	id, action, _ := strings.Cut(tail, "/")
	if id == "" {
		return errorResponse(404, fmt.Errorf("model not found"))
	}
	app.mu.RLock()
	existing, exists := app.customModels[id]
	app.mu.RUnlock()
	if !exists {
		return errorResponse(404, fmt.Errorf("model %q not found", id))
	}
	if action == "duplicate" && method == "POST" {
		var input struct {
			DisplayName string `json:"display_name"`
		}
		_ = json.Unmarshal([]byte(body), &input)
		existing.ModelID = app.uniqueModelID(existing.ModelName)
		if input.DisplayName != "" {
			existing.DisplayName = input.DisplayName
		} else {
			existing.DisplayName += " (copy)"
		}
		saved, err := app.setCustomModel(existing, false)
		if err != nil {
			return errorResponse(400, err)
		}
		return jsonResponse(201, publicCustomModel(saved))
	}
	if action != "" {
		return errorResponse(404, fmt.Errorf("model action not found"))
	}
	switch method {
	case "GET":
		return jsonResponse(200, publicCustomModel(existing))
	case "PUT":
		var updated CustomModel
		if err := json.Unmarshal([]byte(body), &updated); err != nil {
			return errorResponse(400, fmt.Errorf("decode model: %w", err))
		}
		updated.ModelID = id
		if updated.APIKey == "" {
			updated.APIKey = existing.APIKey
		}
		saved, err := app.setCustomModel(updated, true)
		if err != nil {
			return errorResponse(400, err)
		}
		return jsonResponse(200, publicCustomModel(saved))
	case "DELETE":
		app.mu.Lock()
		delete(app.customModels, id)
		delete(app.agents, id)
		app.mu.Unlock()
		return Response{Status: 204}
	default:
		return errorResponse(405, fmt.Errorf("method not allowed"))
	}
}

func (app *App) testCustomModel(body string) Response {
	var model CustomModel
	if err := json.Unmarshal([]byte(body), &model); err != nil {
		return errorResponse(400, fmt.Errorf("decode model: %w", err))
	}
	if model.APIKey == "" && model.ModelID != "" {
		app.mu.RLock()
		stored := app.customModels[model.ModelID]
		app.mu.RUnlock()
		model.APIKey = stored.APIKey
	}
	chat, err := customModelChat(model)
	if err != nil {
		return errorResponse(400, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := chat.Invoke(ctx, damodel.Request{Messages: []damessage.Message{
		damessage.Human("Say 'test successful' in exactly two words."),
	}})
	if err != nil {
		return jsonResponse(200, map[string]any{"success": false, "message": "Test failed: " + err.Error()})
	}
	if strings.TrimSpace(result.Message.TextContent()) == "" {
		return jsonResponse(200, map[string]any{"success": false, "message": "Test failed: empty response from model"})
	}
	return jsonResponse(200, map[string]any{"success": true, "message": "Test successful! Response: " + result.Message.TextContent()})
}

func (app *App) setCustomModel(model CustomModel, replacing bool) (CustomModel, error) {
	model.DisplayName = strings.TrimSpace(model.DisplayName)
	model.Endpoint = strings.TrimRight(strings.TrimSpace(model.Endpoint), "/")
	model.ModelName = strings.TrimSpace(model.ModelName)
	model.APIKey = strings.TrimSpace(model.APIKey)
	if model.ModelID == "" || model.DisplayName == "" || model.Endpoint == "" || model.ModelName == "" || model.APIKey == "" {
		return CustomModel{}, fmt.Errorf("display_name, endpoint, api_key, and model_name are required")
	}
	if model.ProviderType != "openai-responses" && model.ProviderType != "openrouter-responses" {
		return CustomModel{}, fmt.Errorf("provider_type must be 'openai-responses' or 'openrouter-responses'")
	}
	if model.MaxTokens <= 0 {
		model.MaxTokens = 200000
	}
	if model.ImageSupport == "" {
		model.ImageSupport = "auto"
	}
	if model.ReasoningSupport == "" {
		model.ReasoningSupport = "auto"
	}
	model.SupportsImages = model.ImageSupport != "no"
	model.SupportsReasoning = model.ReasoningSupport != "no"
	chat, err := customModelChat(model)
	if err != nil {
		return CustomModel{}, err
	}
	agent, err := newAgent(app.backend, chat, model.ModelID, app.saver)
	if err != nil {
		return CustomModel{}, err
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if _, exists := app.customModels[model.ModelID]; exists && !replacing {
		return CustomModel{}, fmt.Errorf("model %q already exists", model.ModelID)
	}
	app.customModels[model.ModelID] = model
	app.agents[model.ModelID] = agent
	return model, nil
}

func (app *App) listCustomModels() []CustomModel {
	app.mu.RLock()
	defer app.mu.RUnlock()
	result := make([]CustomModel, 0, len(app.customModels))
	for _, model := range app.customModels {
		result = append(result, model)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DisplayName < result[j].DisplayName })
	return result
}

func publicCustomModel(model CustomModel) CustomModel {
	model.APIKey = ""
	return model
}

func publicCustomModels(models []CustomModel) []CustomModel {
	for index := range models {
		models[index] = publicCustomModel(models[index])
	}
	return models
}

func (app *App) uniqueModelID(modelName string) string {
	base := "browser-" + slugify(modelName)
	if base == "browser-" {
		base = "browser-model"
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	if _, exists := app.customModels[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, exists := app.customModels[candidate]; !exists {
			return candidate
		}
	}
}

// Snapshot returns portable conversation JSON for IndexedDB. Browser-owned
// files are persisted independently by the filesystem bridge and never form
// one application-sized object.
func (app *App) Snapshot() ([]byte, error) {
	app.mu.RLock()
	conversations := make(map[string]persistedRecord, len(app.conversations))
	for id, record := range app.conversations {
		conversation := record.Conversation
		conversation.AgentWorking = false
		conversations[id] = persistedRecord{
			Conversation: conversation, Messages: cloneMessages(record.Messages), API: append([]apiMessage(nil), record.API...),
		}
	}
	app.mu.RUnlock()
	saved := snapshot{Version: snapshotVersion, Conversations: conversations}
	if workspace, ok := app.workspace.(*browserWorkspace); ok {
		saved.Files = workspace.Snapshot()
		saved.Directories = workspace.Directories()
	}
	return json.Marshal(saved)
}

// Restore atomically replaces browser-local durable state.
func (app *App) Restore(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var saved snapshot
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode browser snapshot: %w", err)
	}
	if saved.Version != snapshotVersion {
		return fmt.Errorf("unsupported browser snapshot version %d", saved.Version)
	}
	if workspace, ok := app.workspace.(*browserWorkspace); ok && saved.Files != nil {
		if err := workspace.Replace(saved.Files); err != nil {
			return fmt.Errorf("restore browser workspace: %w", err)
		}
		for _, directory := range saved.Directories {
			if err := workspace.CreateDirectory(context.Background(), directory); err != nil {
				return fmt.Errorf("restore browser directory: %w", err)
			}
		}
	}
	app.mu.RLock()
	backend := app.backend
	app.mu.RUnlock()
	defaultAgent, err := newAgent(backend, newPredictableModel(), modelID, app.saver)
	if err != nil {
		return err
	}
	agents := map[string]*dagent.Agent{modelID: defaultAgent}
	app.mu.RLock()
	models := make([]CustomModel, 0, len(app.customModels)+len(app.providerModels))
	for _, model := range app.customModels {
		models = append(models, model)
	}
	for _, model := range app.providerModels {
		models = append(models, model)
	}
	webGPUModel := app.webGPUModel
	app.mu.RUnlock()
	for _, model := range models {
		chat, chatErr := customModelChat(model)
		if chatErr != nil {
			return chatErr
		}
		agent, agentErr := newAgent(backend, chat, model.ModelID, app.saver)
		if agentErr != nil {
			return agentErr
		}
		agents[model.ModelID] = agent
	}
	if webGPUModel != nil {
		agent, agentErr := newAgent(backend, webGPUModel, webGPUModelID, app.saver)
		if agentErr != nil {
			return agentErr
		}
		agents[webGPUModelID] = agent
	}
	restored := make(map[string]*record, len(saved.Conversations))
	for id, value := range saved.Conversations {
		value.Conversation.AgentWorking = false
		restored[id] = &record{Conversation: value.Conversation, Messages: cloneMessages(value.Messages), API: append([]apiMessage(nil), value.API...)}
	}
	app.mu.Lock()
	for _, record := range app.conversations {
		if record.Cancel != nil {
			record.Cancel()
		}
	}
	app.agents = agents
	app.conversations = restored
	app.mu.Unlock()
	return nil
}

func newPredictableModel() damodel.Chat {
	model := modeltest.NewPredictable(modeltest.PredictableOptions{
		DefaultResponse: "The browser-local dago agent is ready. Try `hello`, `echo: text`, or a structured tool command.",
	})
	prefix := strings.ToLower(randomText())
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	return &uniqueIDModel{Chat: model, prefix: prefix + "-"}
}

func newAgent(backend dabackend.Backend, model damodel.Chat, name string, saver dacheckpoint.Saver) (*dagent.Agent, error) {
	tools := []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep"}
	if dabackend.CapabilitiesOf(backend).Execute {
		tools = append(tools, "execute")
	}
	options := []dago.Option{
		dago.WithName("shelley-browser-" + name),
		dago.WithBackend(backend),
		dago.WithSaver(saver),
		dago.WithFilesystem(dago.Filesystem{Tools: tools}),
		dago.WithTodo(),
		dago.WithoutSubagents(),
		dago.WithoutSummary(),
	}
	if browserInterpreterEnabled {
		options = append(options, dago.WithInterpreter(dago.Interpreter{}))
	}
	return dago.NewAgent(model, options...), nil
}

// uniqueIDModel prevents a fresh WASM instance from reusing deterministic
// local-model IDs that were already persisted by an earlier instance.
type uniqueIDModel struct {
	damodel.Chat
	prefix string
}

func (model *uniqueIDModel) Invoke(ctx context.Context, request damodel.Request) (damodel.Response, error) {
	response, err := model.Chat.Invoke(ctx, request)
	if err == nil {
		model.rewrite(&response.Message)
	}
	return response, err
}

func (model *uniqueIDModel) Stream(ctx context.Context, request damodel.Request) (damodel.Stream, error) {
	stream, err := model.Chat.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	return &uniqueIDStream{Stream: stream, model: model, ctx: ctx}, nil
}

func (model *uniqueIDModel) rewrite(message *damessage.Message) {
	if message.ID != "" {
		message.ID = model.prefix + message.ID
	}
	for index := range message.ToolCalls {
		if message.ToolCalls[index].ID != "" {
			message.ToolCalls[index].ID = model.prefix + message.ToolCalls[index].ID
		}
	}
}

type uniqueIDStream struct {
	damodel.Stream
	model *uniqueIDModel
	ctx   context.Context
}

func (stream *uniqueIDStream) Chunks() iter.Seq2[damodel.Chunk, error] {
	return damodel.Chunks(stream.ctx, stream)
}

func (stream *uniqueIDStream) Next(ctx context.Context) (damodel.Chunk, error) {
	chunk, err := stream.Stream.Next(ctx)
	if err == nil {
		stream.model.rewrite(&chunk.MessageDelta)
	}
	return chunk, err
}

func projectMessage(conversationID string, sequence, generation int64, modelName string, message damessage.Message, now time.Time) apiMessage {
	projected := nativeToShelley(message)
	raw, _ := json.Marshal(projected)
	text := string(raw)
	typeName := "user"
	switch message.Role {
	case damessage.RoleAssistant:
		typeName = "agent"
	case damessage.RoleTool:
		typeName = "tool"
	case damessage.RoleSystem:
		typeName = "system"
	}
	id := message.ID
	if id == "" {
		id = newMessageID()
	}
	end := projected.EndOfTurn
	return apiMessage{
		MessageID: id, ConversationID: conversationID, SequenceID: sequence, Type: typeName,
		LLMData: &text, CreatedAt: now.UTC().Format(time.RFC3339Nano), Generation: generation,
		EndOfTurn: &end, ModelName: pointer(modelName),
	}
}

func nativeToShelley(message damessage.Message) llm.Message {
	role := llm.MessageRoleUser
	if message.Role == damessage.RoleAssistant {
		role = llm.MessageRoleAssistant
	}
	result := llm.Message{Role: role, EndOfTurn: message.Role == damessage.RoleAssistant && len(message.ToolCalls) == 0}
	for _, block := range message.Content {
		switch block.Type {
		case damessage.BlockText:
			result.Content = append(result.Content, llm.Content{ID: block.ID, Type: llm.ContentTypeText, Text: block.Text})
		case damessage.BlockReasoning:
			result.Content = append(result.Content, llm.Content{ID: block.ID, Type: llm.ContentTypeThinking, Thinking: block.Reasoning})
		}
	}
	for _, call := range message.ToolCalls {
		result.Content = append(result.Content, llm.Content{ID: call.ID, Type: llm.ContentTypeToolUse, ToolName: call.Name, ToolInput: append(json.RawMessage(nil), call.Arguments...)})
	}
	if message.Role == damessage.RoleTool {
		result.Content = []llm.Content{{
			Type: llm.ContentTypeToolResult, ToolUseID: message.ToolCallID,
			ToolError:  message.ToolStatus == damessage.ToolStatusError,
			ToolResult: []llm.Content{{Type: llm.ContentTypeText, Text: message.TextContent()}},
		}}
	}
	return result
}

func (app *App) availableModels() []map[string]any {
	app.mu.RLock()
	providerModels := make([]CustomModel, 0, len(app.providerModels))
	for _, id := range []string{"gpt-5.6-luna", "gpt-5.6-terra", "gpt-5.6-sol"} {
		if model, exists := app.providerModels[id]; exists {
			providerModels = append(providerModels, model)
		}
	}
	webGPUReady := app.webGPUModel != nil
	app.mu.RUnlock()
	customModels := app.listCustomModels()
	result := make([]map[string]any, 0, len(providerModels)+len(customModels)+2)
	for index, model := range providerModels {
		result = append(result, map[string]any{
			"id": model.ModelID, "display_name": model.DisplayName, "source": "OpenAI",
			"api_type": model.ProviderType, "ready": true, "is_default": index == 0,
			"max_context_tokens": model.MaxTokens, "supports_images": model.SupportsImages,
			"supports_reasoning": model.SupportsReasoning,
		})
	}
	for index, model := range customModels {
		result = append(result, map[string]any{
			"id": model.ModelID, "display_name": model.DisplayName, "source": "custom",
			"api_type": model.ProviderType, "ready": true, "is_default": len(providerModels) == 0 && index == 0,
			"max_context_tokens": model.MaxTokens, "supports_images": model.SupportsImages,
			"supports_reasoning": model.SupportsReasoning,
		})
	}
	if webGPUReady {
		result = append(result, map[string]any{
			"id": webGPUModelID, "display_name": webGPUModelName, "source": "local WebGPU",
			"ready": true, "is_default": len(providerModels) == 0 && len(customModels) == 0,
			"max_context_tokens": webGPUContext,
		})
	}
	result = append(result, map[string]any{
		"id": modelID, "display_name": "Browser predictable", "source": "local WASM",
		"ready": true, "is_default": len(providerModels) == 0 && len(customModels) == 0 && !webGPUReady, "max_context_tokens": 200000,
	})
	return result
}

func (app *App) defaultModelID() string {
	app.mu.RLock()
	_, openAIReady := app.providerModels["gpt-5.6-luna"]
	webGPUReady := app.webGPUModel != nil
	app.mu.RUnlock()
	if openAIReady {
		return "gpt-5.6-luna"
	}
	models := app.listCustomModels()
	if len(models) > 0 {
		return models[0].ModelID
	}
	if webGPUReady {
		return webGPUModelID
	}
	return modelID
}

func availableTools(includeExecute bool) []map[string]any {
	items := []struct{ name, summary string }{
		{"ls", "List browser-workspace files."}, {"read_file", "Read a browser-workspace file."},
		{"write_file", "Write a browser-workspace file."}, {"edit_file", "Edit a browser-workspace file."},
		{"delete", "Delete a browser-workspace path."}, {"glob", "Match browser-workspace paths."},
		{"grep", "Search browser-workspace files."}, {"write_todos", "Track the agent plan."},
	}
	if browserInterpreterEnabled {
		items = append(items, struct{ name, summary string }{"js_eval", "Evaluate persistent JavaScript with programmatic read-only filesystem tools."})
	}
	if includeExecute {
		items = append(items, struct{ name, summary string }{"execute", "Run a sandboxed just-bash command in the browser workspace."})
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{"name": item.name, "summary": item.summary, "default_on": true})
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

func newMessageID() string {
	text := randomText()
	if len(text) > 12 {
		text = text[:12]
	}
	return "m" + strings.ToLower(text)
}

func randomText() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	text := make([]byte, 26)
	if _, err := rand.Read(text); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	for index := range text {
		text[index] = alphabet[text[index]%byte(len(alphabet))]
	}
	return string(text)
}

func slugify(value string) string {
	var result strings.Builder
	lastDash := false
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			result.WriteRune(character)
			lastDash = false
		} else if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
		if result.Len() >= 48 {
			break
		}
	}
	return strings.Trim(result.String(), "-")
}

func pointer[T any](value T) *T { return &value }

func pointerText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func listHash(items []conversationWithState) string {
	raw, _ := json.Marshal(items)
	var hash uint64 = 1469598103934665603
	for _, value := range raw {
		hash ^= uint64(value)
		hash *= 1099511628211
	}
	return fmt.Sprintf("%x", hash)
}

func approximateContext(messages []apiMessage) int {
	characters := 0
	for _, message := range messages {
		if message.LLMData != nil {
			characters += len(*message.LLMData)
		}
	}
	if characters == 0 {
		return 0
	}
	return (characters + 3) / 4
}

func jsonResponse(status int, value any) Response {
	body, err := json.Marshal(value)
	if err != nil {
		return errorResponse(500, err)
	}
	return Response{Status: status, Headers: map[string]string{"Content-Type": "application/json"}, Body: body}
}

func changedResponse(status int, value any) Response {
	response := jsonResponse(status, value)
	response.Changed = true
	return response
}

func errorResponse(status int, err error) Response {
	body, _ := json.Marshal(map[string]string{"error": err.Error()})
	return Response{Status: status, Headers: map[string]string{"Content-Type": "application/json"}, Body: body}
}

func capabilityResponse(name string) Response {
	body, _ := json.Marshal(map[string]any{
		"error": "capability is unavailable in the browser runtime", "capability": strings.Trim(name, "/"),
	})
	return Response{
		Status:  501,
		Headers: map[string]string{"Content-Type": "application/json", "X-Shelley-Capability": "unavailable"},
		Body:    body,
	}
}
