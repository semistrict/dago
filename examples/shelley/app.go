package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ls "github.com/langchain-ai/langsmith-go"
	"github.com/semistrict/dago"
	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	langsmithbackend "github.com/semistrict/dago/backend/langsmith"
	"github.com/semistrict/dago/checkpoint"
	checkpointsqlite "github.com/semistrict/dago/checkpoint/sqlite"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/providers/openai"
	"github.com/semistrict/dago/state"
)

type settings struct {
	Model       string `json:"model"`
	Backend     string `json:"backend"`
	SandboxName string `json:"sandbox_name,omitempty"`
}

const defaultModel = "gpt-5.6-luna"

type application struct {
	mu sync.RWMutex

	dataDirectory string
	settings      settings
	apiKey        string
	oauth         *openai.OAuthSession
	oauthState    string
	oauthError    string
	modelOverride model.Chat

	local         *backend.LocalShell
	remote        *langsmithbackend.Backend
	remoteName    string
	langsmith     *ls.Client
	checkpoints   *checkpointsqlite.Saver
	conversations *conversationStore
	running       map[string]context.CancelFunc
}

func newApplication(workspace, dataDirectory string) (*application, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return nil, err
	}
	local, err := backend.NewLocalShell(backend.LocalShellOptions{
		Filesystem: backend.FilesystemOptions{Root: workspace},
		ID:         "local:" + workspace,
	})
	if err != nil {
		return nil, err
	}
	checkpoints, err := checkpointsqlite.Open(filepath.Join(dataDirectory, "checkpoints.sqlite"))
	if err != nil {
		return nil, err
	}
	conversations, err := openConversationStore(filepath.Join(dataDirectory, "shelley.sqlite"))
	if err != nil {
		checkpoints.Close()
		return nil, err
	}
	application := &application{
		dataDirectory: dataDirectory,
		settings: settings{
			Model:   environmentDefault("OPENAI_MODEL", defaultModel),
			Backend: "local",
		},
		apiKey:        os.Getenv("OPENAI_API_KEY"),
		local:         local,
		langsmith:     ls.NewClient(),
		checkpoints:   checkpoints,
		conversations: conversations,
		running:       map[string]context.CancelFunc{},
	}
	tokenPath := filepath.Join(dataDirectory, "openai-oauth.json")
	if session, err := openai.LoadOAuthSession(tokenPath, openai.OAuthOptions{}); err == nil {
		application.oauth = session
		application.oauthState = "complete"
	}
	return application, nil
}

func (application *application) close() {
	application.mu.Lock()
	for _, cancel := range application.running {
		cancel()
	}
	application.running = map[string]context.CancelFunc{}
	application.mu.Unlock()
	if application.langsmith != nil {
		application.langsmith.Close()
	}
	if application.checkpoints != nil {
		_ = application.checkpoints.Close()
	}
	if application.conversations != nil && application.conversations.db != nil {
		_ = application.conversations.db.Close()
	}
}

func (application *application) currentModel() (model.Chat, string, error) {
	application.mu.RLock()
	configured := application.settings
	apiKey := application.apiKey
	oauth := application.oauth
	override := application.modelOverride
	application.mu.RUnlock()
	if override != nil {
		return override, "test", nil
	}
	options := openai.Options{Model: configured.Model, ContextWindow: 128_000}
	if oauth != nil {
		value, err := openai.NewSubscription(oauth, options)
		return value, "subscription", err
	}
	if apiKey != "" {
		value, err := openai.NewAPIKey(apiKey, options)
		return value, "api_key", err
	}
	return nil, "none", fmt.Errorf("configure an OpenAI API key or complete subscription sign-in")
}

func (application *application) currentBackend(ctx context.Context) (backend.Backend, settings, error) {
	application.mu.RLock()
	configured := application.settings
	if configured.Backend == "local" {
		local := application.local
		application.mu.RUnlock()
		return local, configured, nil
	}
	if application.remote != nil && application.remoteName == configured.SandboxName {
		remote := application.remote
		application.mu.RUnlock()
		return remote, configured, nil
	}
	application.mu.RUnlock()
	if configured.Backend != "langsmith" {
		return nil, configured, fmt.Errorf("unknown backend %q", configured.Backend)
	}
	if strings.TrimSpace(configured.SandboxName) == "" {
		return nil, configured, fmt.Errorf("an existing LangSmith sandbox name is required")
	}
	sandbox, err := application.langsmith.Sandboxes.Boxes.WaitSandbox(ctx, configured.SandboxName, ls.SandboxWaitParams{Timeout: 15 * time.Second})
	if err != nil {
		return nil, configured, fmt.Errorf("connect existing LangSmith sandbox: %w", err)
	}
	remote, err := langsmithbackend.New(sandbox, langsmithbackend.Options{})
	if err != nil {
		return nil, configured, err
	}
	application.mu.Lock()
	application.remote, application.remoteName = remote, configured.SandboxName
	application.mu.Unlock()
	return remote, configured, nil
}

func (application *application) buildAgent(ctx context.Context) (*dago.DeepAgent, settings, error) {
	chat, _, err := application.currentModel()
	if err != nil {
		return nil, settings{}, err
	}
	files, configured, err := application.currentBackend(ctx)
	if err != nil {
		return nil, configured, err
	}
	approval := agent.HumanApproval([]agent.ApprovalRule{
		{Pattern: "execute", Description: "Run a shell command in the selected backend"},
		{Pattern: "delete", Description: "Recursively delete a file or directory"},
	})
	compiled, err := dago.New(dago.Options{
		Name:         "shelley",
		Model:        chat,
		Backend:      files,
		Saver:        application.checkpoints,
		SystemPrompt: "You are Shelley, a careful coding agent working in the selected workspace. Inspect before editing, keep changes scoped, run focused verification, and explain irreversible risks before acting. Use todos for multi-step work and delegate independent research when useful.",
		Middleware:   []agent.Middleware{approval},
	})
	return compiled, configured, err
}

func (application *application) messages(ctx context.Context, threadID string) ([]message.Message, error) {
	tuple, err := application.checkpoints.GetTuple(ctx, checkpoint.Config{ThreadID: threadID})
	if err != nil || tuple == nil {
		return nil, err
	}
	if value, exists := tuple.Checkpoint.ChannelValues[agent.MessagesKey]; exists {
		return decodeMessageValue(value)
	}
	histories, err := application.checkpoints.GetDeltaChannelHistory(ctx, tuple.Config, []string{agent.MessagesKey})
	if err != nil {
		return nil, err
	}
	history := histories[agent.MessagesKey]
	var result []message.Message
	if history.HasSeed {
		result, err = decodeMessageValue(history.Seed)
		if err != nil {
			return nil, err
		}
	}
	for _, write := range history.Writes {
		value := write.Value
		if overwrite, ok := value.(state.Overwrite); ok {
			result, err = decodeMessageValue(overwrite.Value)
		} else {
			var updates []message.Message
			updates, err = decodeMessageValue(value)
			if err == nil {
				result, err = message.DeltaReduce(result, [][]message.Message{updates})
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func decodeMessageValue(value any) ([]message.Message, error) {
	if snapshot, ok := value.(checkpoint.DeltaSnapshot); ok {
		value = snapshot.Value
	}
	switch typed := value.(type) {
	case []message.Message:
		return typed, nil
	case []any:
		result := make([]message.Message, 0, len(typed))
		for _, item := range typed {
			if parsed, ok := item.(message.Message); ok {
				result = append(result, parsed)
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("messages have checkpoint type %T", value)
	}
}

func (application *application) beginRun(threadID string, parent context.Context) (context.Context, func(), error) {
	application.mu.Lock()
	defer application.mu.Unlock()
	if _, exists := application.running[threadID]; exists {
		return nil, nil, fmt.Errorf("conversation is already running")
	}
	ctx, cancel := context.WithCancel(parent)
	application.running[threadID] = cancel
	finish := func() {
		cancel()
		application.mu.Lock()
		delete(application.running, threadID)
		application.mu.Unlock()
	}
	return ctx, finish, nil
}

func (application *application) cancelRun(threadID string) bool {
	application.mu.RLock()
	cancel := application.running[threadID]
	application.mu.RUnlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func environmentDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, err error) {
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

func decodeJSONBody(request *http.Request, target any) error {
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}

func normalizedUploadPath(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(value rune) rune {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || strings.ContainsRune("._-", value) {
			return value
		}
		return '-'
	}, name)
	return "/.shelley/uploads/" + fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), name)
}

func contentTypeFor(filePath string) string {
	if value := mime.TypeByExtension(path.Ext(filePath)); value != "" {
		return value
	}
	return "application/octet-stream"
}
