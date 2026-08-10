package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dmessage "github.com/semistrict/dago/message"
	dmodel "github.com/semistrict/dago/model"

	"shelley.exe.dev/claudetool"
	"shelley.exe.dev/db"
	"shelley.exe.dev/llm"
	"shelley.exe.dev/models"
)

// BenchmarkAgentE2E measures the complete agent request path while replacing
// only the remote model call with an immediate deterministic response. The
// HTTP handlers, conversation manager, SQLite persistence, checkpoints,
// streaming projection, and native tool execution remain enabled.
func BenchmarkAgentE2E(b *testing.B) {
	b.Run("ColdTextTurn", benchmarkAgentColdTextTurn)
	b.Run("GrowingTextConversation", benchmarkAgentGrowingTextConversation)
	b.Run("GrowingFilesystemToolConversation", benchmarkAgentGrowingFilesystemToolConversation)
}

// BenchmarkAgentHistoryScaling measures one steady-state text turn at fixed
// transcript sizes. History construction and loop hydration are deliberately
// outside the timer so results isolate the marginal cost of processing the
// next turn rather than the cost of building the fixture.
func BenchmarkAgentHistoryScaling(b *testing.B) {
	for _, priorTurns := range []int{1, 10, 50, 100, 250} {
		b.Run(fmt.Sprintf("PriorTurns_%03d", priorTurns), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				fixture := newSeededHistoryBenchmarkFixture(b, priorTurns)
				b.StartTimer()

				fixture.chat("benchmark text")
				fixture.waitDone()

				b.StopTimer()
				fixture.close()
				b.StartTimer()
			}
			b.StopTimer()
			b.ReportMetric(float64(priorTurns*2), "history_messages")
		})
	}
}

func benchmarkAgentColdTextTurn(b *testing.B) {
	workspace := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		fixture := newAgentBenchmarkFixture(b, workspace, true)
		b.StartTimer()

		conversationID := fixture.newConversation("benchmark text")

		// The model gate removes the artificial wait needed to install the
		// completion signal from the measurement while retaining all work on
		// both sides of the model boundary.
		b.StopTimer()
		fixture.installDoneSignal(conversationID)
		b.StartTimer()
		fixture.model.release()
		fixture.waitDone()

		b.StopTimer()
		fixture.close()
		b.StartTimer()
	}
}

func benchmarkAgentGrowingTextConversation(b *testing.B) {
	fixture := newWarmAgentBenchmarkFixture(b)
	defer fixture.close()

	var commits atomic.Int64
	fixture.database.Pool().OnCommit(func() { commits.Add(1) })
	fixture.model.calls.Store(0)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		fixture.chat("benchmark text")
		fixture.waitDone()
	}
	b.StopTimer()
	b.ReportMetric(float64(fixture.model.calls.Load())/float64(b.N), "model_calls/op")
	b.ReportMetric(float64(commits.Load())/float64(b.N), "sqlite_commits/op")
}

func benchmarkAgentGrowingFilesystemToolConversation(b *testing.B) {
	fixture := newWarmAgentBenchmarkFixture(b)
	defer fixture.close()

	var commits atomic.Int64
	fixture.database.Pool().OnCommit(func() { commits.Add(1) })
	fixture.model.calls.Store(0)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		fixture.chat("benchmark filesystem")
		fixture.waitDone()
	}
	b.StopTimer()
	b.ReportMetric(float64(fixture.model.calls.Load())/float64(b.N), "model_calls/op")
	b.ReportMetric(float64(commits.Load())/float64(b.N), "sqlite_commits/op")
}

type agentBenchmarkFixture struct {
	b              *testing.B
	database       *db.DB
	databaseClose  func()
	databaseDir    string
	hooksDir       string
	terminalDir    string
	server         *Server
	model          *agentBenchmarkModel
	conversationID string
	done           chan struct{}
}

func newWarmAgentBenchmarkFixture(b *testing.B) *agentBenchmarkFixture {
	b.Helper()
	b.StopTimer()
	fixture := newAgentBenchmarkFixture(b, b.TempDir(), true)
	conversationID := fixture.newConversation("benchmark warmup")
	fixture.installDoneSignal(conversationID)
	fixture.model.release()
	fixture.waitDone()
	fixture.model.disableGate()
	b.StartTimer()
	return fixture
}

func newSeededHistoryBenchmarkFixture(b *testing.B, priorTurns int) *agentBenchmarkFixture {
	b.Helper()
	if priorTurns < 1 {
		b.Fatalf("prior turns must be positive, got %d", priorTurns)
	}
	workspace := b.TempDir()
	fixture := newAgentBenchmarkFixture(b, workspace, false)
	ctx := context.Background()
	slug := fmt.Sprintf("benchmark-history-%d", priorTurns)
	modelID := "benchmark"
	conversation, err := fixture.database.CreateConversation(
		ctx, &slug, true, &workspace, &modelID, db.ConversationOptions{},
	)
	if err != nil {
		b.Fatal(err)
	}

	// Leave one turn for the untimed warmup below. That warmup establishes a
	// native checkpoint containing exactly priorTurns user/assistant pairs, so
	// the measured request exercises restoration instead of first-run import.
	seededTurns := priorTurns - 1
	messages := make([]db.CreateMessageParams, 0, seededTurns*2)
	for turn := range seededTurns {
		messages = append(messages,
			db.CreateMessageParams{
				ConversationID: conversation.ConversationID,
				Type:           db.MessageTypeUser,
				LLMData: llm.Message{
					Role:    llm.MessageRoleUser,
					Content: []llm.Content{{Type: llm.ContentTypeText, Text: fmt.Sprintf("prior request %d", turn)}},
				},
			},
			db.CreateMessageParams{
				ConversationID: conversation.ConversationID,
				Type:           db.MessageTypeAgent,
				LLMData: llm.Message{
					Role:      llm.MessageRoleAssistant,
					Content:   []llm.Content{{Type: llm.ContentTypeText, Text: fmt.Sprintf("prior response %d", turn)}},
					EndOfTurn: true,
				},
			},
		)
	}
	if _, err := fixture.database.CreateMessages(ctx, messages); err != nil {
		b.Fatal(err)
	}

	manager, err := fixture.server.getOrCreateConversationManager(ctx, conversation.ConversationID, "")
	if err != nil {
		b.Fatal(err)
	}
	if err := manager.ensureLoop(fixture.model, modelID); err != nil {
		b.Fatal(err)
	}
	fixture.conversationID = conversation.ConversationID
	fixture.installDoneSignal(conversation.ConversationID)
	fixture.chat("benchmark warmup")
	fixture.waitDone()
	fixture.model.calls.Store(0)
	return fixture
}

func newAgentBenchmarkFixture(b *testing.B, workspace string, gated bool) *agentBenchmarkFixture {
	b.Helper()
	dbTB := &agentBenchmarkTB{b: b}
	database, databaseClose := db.NewTestDB(dbTB)
	hooksDir, err := os.MkdirTemp("", "shelley-agent-benchmark-hooks-")
	if err != nil {
		b.Fatal(err)
	}
	model := newAgentBenchmarkModel(workspace, gated)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	server := NewServer(
		database,
		&agentBenchmarkLLMManager{model: model},
		claudetool.ToolSetConfig{WorkingDir: workspace, EnableBrowser: false},
		logger,
		true,
		"benchmark",
		"",
	)
	server.hooksDir = hooksDir
	if server.terminals != nil {
		server.terminals.SetSpawner(InProcessSpawner)
	}
	return &agentBenchmarkFixture{
		b: b, database: database, databaseClose: databaseClose,
		databaseDir: dbTB.dir, hooksDir: hooksDir,
		terminalDir: server.terminals.dir, server: server, model: model,
		done: make(chan struct{}, 1),
	}
}

func (fixture *agentBenchmarkFixture) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	fixture.server.stopAllConversations(ctx)
	cancel()
	fixture.databaseClose()
	_ = os.RemoveAll(fixture.databaseDir)
	_ = os.RemoveAll(fixture.hooksDir)
	_ = os.RemoveAll(fixture.terminalDir)
}

func (fixture *agentBenchmarkFixture) newConversation(message string) string {
	fixture.b.Helper()
	body, err := json.Marshal(ChatRequest{Message: message, Model: "benchmark"})
	if err != nil {
		fixture.b.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/new", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fixture.server.handleNewConversation(response, request)
	if response.Code != http.StatusCreated {
		fixture.b.Fatalf("new conversation status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		fixture.b.Fatal(err)
	}
	fixture.conversationID = result.ConversationID
	return result.ConversationID
}

func (fixture *agentBenchmarkFixture) installDoneSignal(conversationID string) {
	fixture.b.Helper()
	fixture.server.mu.Lock()
	manager := fixture.server.activeConversations[conversationID]
	fixture.server.mu.Unlock()
	if manager == nil {
		fixture.b.Fatalf("conversation manager %q not found", conversationID)
	}
	manager.mu.Lock()
	manager.onDone = func() {
		select {
		case fixture.done <- struct{}{}:
		default:
		}
	}
	manager.mu.Unlock()
}

func (fixture *agentBenchmarkFixture) chat(message string) {
	fixture.b.Helper()
	body, err := json.Marshal(ChatRequest{Message: message, Model: "benchmark"})
	if err != nil {
		fixture.b.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/conversation/"+fixture.conversationID+"/chat", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fixture.server.handleChatConversation(response, request, fixture.conversationID)
	if response.Code != http.StatusAccepted {
		fixture.b.Fatalf("chat status = %d: %s", response.Code, response.Body.String())
	}
}

func (fixture *agentBenchmarkFixture) waitDone() { <-fixture.done }

type agentBenchmarkTB struct {
	b   *testing.B
	dir string
}

func (tb *agentBenchmarkTB) Helper() { tb.b.Helper() }
func (tb *agentBenchmarkTB) Fatalf(format string, args ...any) {
	tb.b.Fatalf(format, args...)
}
func (tb *agentBenchmarkTB) TempDir() string {
	tb.b.Helper()
	dir, err := os.MkdirTemp("", "shelley-agent-benchmark-db-")
	if err != nil {
		tb.b.Fatal(err)
	}
	tb.dir = dir
	return dir
}

type agentBenchmarkModel struct {
	workspace string
	calls     atomic.Int64
	gateMu    sync.Mutex
	gate      chan struct{}
	releaseMu sync.Once
}

func newAgentBenchmarkModel(workspace string, gated bool) *agentBenchmarkModel {
	model := &agentBenchmarkModel{workspace: workspace}
	if gated {
		model.gate = make(chan struct{})
	}
	return model
}

func (model *agentBenchmarkModel) Profile() dmodel.Profile {
	return dmodel.Profile{
		Provider: "benchmark", Model: "benchmark", ContextWindow: 200000,
		MaxOutputTokens: 8192, ToolCalling: true, SupportsSeparateSystemMessage: true,
	}
}

func (model *agentBenchmarkModel) Invoke(ctx context.Context, request dmodel.Request) (dmodel.Response, error) {
	model.calls.Add(1)
	model.gateMu.Lock()
	gate := model.gate
	model.gateMu.Unlock()
	if gate != nil && agentBenchmarkRequestHasSystem(request) {
		select {
		case <-gate:
		case <-ctx.Done():
			return dmodel.Response{}, ctx.Err()
		}
	}

	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == dmessage.RoleTool {
		return dmodel.Response{Message: dmessage.Assistant("filesystem complete")}, nil
	}
	if agentBenchmarkLastHumanText(request) == "benchmark filesystem" {
		arguments, _ := json.Marshal(map[string]any{"path": model.workspace})
		return dmodel.Response{Message: dmessage.Message{
			Role: dmessage.RoleAssistant,
			ToolCalls: []dmessage.ToolCall{{
				ID: "benchmark-ls", Name: "ls", Arguments: arguments,
			}},
		}}, nil
	}
	return dmodel.Response{Message: dmessage.Assistant("benchmark complete")}, nil
}

func (model *agentBenchmarkModel) Stream(ctx context.Context, request dmodel.Request) (dmodel.Stream, error) {
	response, err := model.Invoke(ctx, request)
	if err != nil {
		return nil, err
	}
	return &agentBenchmarkStream{chunk: dmodel.Chunk{MessageDelta: response.Message, Done: true}}, nil
}

func (model *agentBenchmarkModel) release() {
	model.releaseMu.Do(func() {
		model.gateMu.Lock()
		if model.gate != nil {
			close(model.gate)
		}
		model.gateMu.Unlock()
	})
}

func (model *agentBenchmarkModel) disableGate() {
	model.gateMu.Lock()
	model.gate = nil
	model.gateMu.Unlock()
}

func agentBenchmarkRequestHasSystem(request dmodel.Request) bool {
	if request.SystemMessage != nil && request.SystemMessage.Role == dmessage.RoleSystem {
		return true
	}
	for _, item := range request.Messages {
		if item.Role == dmessage.RoleSystem {
			return true
		}
	}
	return false
}

func agentBenchmarkLastHumanText(request dmodel.Request) string {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == dmessage.RoleHuman {
			return request.Messages[index].TextContent()
		}
	}
	return ""
}

type agentBenchmarkStream struct {
	chunk dmodel.Chunk
	done  bool
}

func (stream *agentBenchmarkStream) Next(context.Context) (dmodel.Chunk, error) {
	if stream.done {
		return dmodel.Chunk{}, io.EOF
	}
	stream.done = true
	return stream.chunk, nil
}

func (*agentBenchmarkStream) Close() error { return nil }

type agentBenchmarkLLMManager struct{ model dmodel.Chat }

func (manager *agentBenchmarkLLMManager) GetChat(string) (dmodel.Chat, error) {
	return manager.model, nil
}
func (*agentBenchmarkLLMManager) GetAvailableModels() []string { return []string{"benchmark"} }
func (*agentBenchmarkLLMManager) HasModel(modelID string) bool { return modelID == "benchmark" }
func (*agentBenchmarkLLMManager) GetModelInfo(string) *models.ModelInfo {
	return nil
}
func (*agentBenchmarkLLMManager) RefreshCustomModels() error { return nil }
