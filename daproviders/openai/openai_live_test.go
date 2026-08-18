package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/datool"
)

const (
	liveOpenAIFlag         = "DAGO_OPENAI_LIVE"
	liveOpenAIOAuthFile    = "DAGO_OPENAI_OAUTH_FILE"
	liveOpenAIModel        = "DAGO_OPENAI_LIVE_MODEL"
	liveOpenAIDefaultModel = "gpt-5.6-luna"
)

type liveOAuthFileSource struct {
	path string
}

func (source liveOAuthFileSource) Credentials(context.Context) (Credentials, error) {
	data, err := os.ReadFile(source.path)
	if err != nil {
		return Credentials{}, fmt.Errorf("read OAuth file: %w", err)
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return Credentials{}, fmt.Errorf("decode OAuth file: %w", err)
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.AccountID == "" {
		return Credentials{}, errors.New("OAuth file is missing an access token or account ID")
	}
	return Credentials{AccessToken: auth.Tokens.AccessToken, AccountID: auth.Tokens.AccountID}, nil
}

func TestLiveResponsesWebSocketOAuthEndToEnd(t *testing.T) {
	if os.Getenv(liveOpenAIFlag) != "1" {
		t.Skip("set DAGO_OPENAI_LIVE=1 to run live API tests")
	}
	authFile := os.Getenv(liveOpenAIOAuthFile)
	if authFile == "" {
		t.Fatal("DAGO_OPENAI_OAUTH_FILE must point to an existing OAuth JSON file")
	}
	model := os.Getenv(liveOpenAIModel)
	if model == "" {
		model = liveOpenAIDefaultModel
	}
	client := NewSubscription(liveOAuthFileSource{path: authFile}, model, Options{
		ContextWindow:    272000,
		DefaultReasoning: &damodel.Reasoning{Effort: "low", Summary: "auto"},
		RetryBackoff:     []time.Duration{time.Second, 2 * time.Second},
		Headers:          map[string][]string{"originator": {"dago-live-test"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	lookup := datool.Definition{
		Name: "lookup", Description: "Return the requested test value.", Strict: true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
	}
	system := damessage.System("Follow the requested output format exactly and keep answers minimal. After lookup returns JSON, reply with exactly its result field.")
	baseRequest := damodel.Request{SystemMessage: &system, Tools: []datool.Definition{lookup}}
	if err := client.Prewarm(ctx, baseRequest); err != nil {
		t.Fatalf("prewarm over websocket: %v", err)
	}
	connection, warmedID := liveWebSocketState(t, client)
	if warmedID == "" {
		t.Fatal("prewarm did not establish a continuation response")
	}
	underlying := connection.conn

	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	argument := "alpha-" + unique
	firstRequest := baseRequest
	firstRequest.Messages = []damessage.Message{damessage.Human("Call lookup exactly once with q " + argument + ". Do not answer directly.")}
	firstRequest.ToolChoice = &damodel.ToolChoice{Mode: "tool", Name: "lookup"}
	first, streamedChunks := collectLiveResponse(t, ctx, client, firstRequest)
	if streamedChunks == 0 {
		t.Fatal("tool response did not yield any incremental stream chunks")
	}
	if len(first.Message.ToolCalls) != 1 || first.Message.ToolCalls[0].Name != "lookup" {
		t.Fatalf("forced tool response = %#v", first.Message.ToolCalls)
	}
	var arguments struct {
		Q string `json:"q"`
	}
	if err := json.Unmarshal(first.Message.ToolCalls[0].Arguments, &arguments); err != nil || arguments.Q != argument {
		t.Fatalf("tool arguments = %s, decoded = %#v, error = %v", first.Message.ToolCalls[0].Arguments, arguments, err)
	}
	connection, firstID := liveWebSocketState(t, client)
	if connection.conn != underlying {
		t.Fatal("prewarm and first generated turn did not reuse one websocket")
	}
	if firstID == "" || firstID == warmedID {
		t.Fatalf("first continuation response ID = %q after warmup %q", firstID, warmedID)
	}

	toolResult := "LIVE_WS_TOOL_" + unique
	secondRequest := baseRequest
	secondRequest.Messages = []damessage.Message{
		firstRequest.Messages[0],
		first.Message,
		damessage.Tool(first.Message.ToolCalls[0].ID, `{"result":"`+toolResult+`"}`),
	}
	secondRequest.ToolChoice = &damodel.ToolChoice{Mode: "none"}
	second, streamedChunks := collectLiveResponse(t, ctx, client, secondRequest)
	if streamedChunks == 0 || !strings.Contains(second.Message.TextContent(), toolResult) {
		t.Fatalf("continued tool result = %q, streamed chunks = %d", second.Message.TextContent(), streamedChunks)
	}
	connection, secondID := liveWebSocketState(t, client)
	if connection.conn != underlying {
		t.Fatal("tool result continuation opened a different websocket")
	}
	if secondID == "" || secondID == firstID {
		t.Fatalf("second continuation response ID = %q after %q", secondID, firstID)
	}

	resetValue := "LIVE_WS_RESET_" + unique
	resetRequest := baseRequest
	resetRequest.Messages = []damessage.Message{damessage.Human("Reply with exactly " + resetValue)}
	resetRequest.ToolChoice = &damodel.ToolChoice{Mode: "none"}
	reset, _ := collectLiveResponse(t, ctx, client, resetRequest)
	if !strings.Contains(reset.Message.TextContent(), resetValue) {
		t.Fatalf("reset response = %q", reset.Message.TextContent())
	}
	connection, resetID := liveWebSocketState(t, client)
	if connection.conn != underlying {
		t.Fatal("unrelated-history reset did not reuse the existing websocket")
	}
	if resetID == "" || resetID == secondID {
		t.Fatalf("reset continuation response ID = %q after %q", resetID, secondID)
	}

	cancelRequest := baseRequest
	cancelRequest.Messages = []damessage.Message{damessage.Human("Write a detailed explanation of websocket multiplexing.")}
	stream, err := client.Stream(ctx, cancelRequest)
	if err != nil {
		t.Fatalf("start cancellation stream: %v", err)
	}
	readCtx, cancelRead := context.WithCancel(ctx)
	cancelRead()
	if _, err := stream.Next(readCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled websocket read error = %v", err)
	}
	_ = stream.Close()
	if connection.conn != nil {
		t.Fatal("canceled response left its websocket connected")
	}

	reconnectValue := "LIVE_WS_RECONNECT_" + unique
	reconnectRequest := baseRequest
	reconnectRequest.Messages = []damessage.Message{damessage.Human("Reply with exactly " + reconnectValue)}
	reconnectRequest.ToolChoice = &damodel.ToolChoice{Mode: "none"}
	reconnected, _ := collectLiveResponse(t, ctx, client, reconnectRequest)
	if !strings.Contains(reconnected.Message.TextContent(), reconnectValue) {
		t.Fatalf("reconnected response = %q", reconnected.Message.TextContent())
	}
	connection, reconnectID := liveWebSocketState(t, client)
	if connection.conn == nil || connection.conn == underlying {
		t.Fatal("request after cancellation did not establish a fresh websocket")
	}
	if reconnectID == "" {
		t.Fatal("request after cancellation did not establish a continuation")
	}
}

func TestLiveResponsesRemoteCompactionOAuthEndToEnd(t *testing.T) {
	if os.Getenv(liveOpenAIFlag) != "1" {
		t.Skip("set DAGO_OPENAI_LIVE=1 to run live API tests")
	}
	authFile := os.Getenv(liveOpenAIOAuthFile)
	if authFile == "" {
		t.Fatal("DAGO_OPENAI_OAUTH_FILE must point to an existing OAuth JSON file")
	}
	model := os.Getenv(liveOpenAIModel)
	if model == "" {
		model = liveOpenAIDefaultModel
	}
	client := NewSubscription(liveOAuthFileSource{path: authFile}, model, Options{
		ContextWindow: 272000, CompactionThreshold: 1,
		DefaultReasoning: &damodel.Reasoning{Effort: "low", Summary: "auto"},
		RetryBackoff:     []time.Duration{time.Second, 2 * time.Second},
		Headers:          map[string][]string{"originator": {"dago-live-test"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	unique := fmt.Sprintf("LIVE_COMPACT_%d", time.Now().UnixNano())
	firstPrompt := damessage.Human("Remember the exact token " + unique + ". Reply with exactly ACK.")
	first, err := client.Invoke(ctx, damodel.Request{Messages: []damessage.Message{firstPrompt}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Message.TextContent(), "ACK") {
		t.Fatalf("post-compaction response = %q", first.Message.TextContent())
	}
	foundCompaction := false
	for _, block := range first.Message.Content {
		if block.Type != damessage.BlockNonStandard {
			continue
		}
		var item struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if json.Unmarshal(block.NonStandard, &item) == nil && isCompactionType(item.Type) && item.EncryptedContent != "" {
			foundCompaction = true
		}
	}
	if !foundCompaction {
		t.Fatal("real compaction response did not preserve an encrypted compaction item")
	}
	connection, compactedID := liveWebSocketState(t, client)
	if compactedID == "" {
		t.Fatal("post-compaction inference did not establish a continuation")
	}
	underlying := connection.conn

	client.options.CompactionThreshold = defaultServerCompactionThreshold
	second, err := client.Invoke(ctx, damodel.Request{Messages: []damessage.Message{
		firstPrompt, first.Message, damessage.Human("What exact token did I ask you to remember? Reply with only the token."),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Message.TextContent(), unique) {
		t.Fatalf("continued compacted response = %q", second.Message.TextContent())
	}
	connection, continuedID := liveWebSocketState(t, client)
	if connection.conn != underlying || continuedID == "" || continuedID == compactedID {
		t.Fatalf("compacted continuation reused connection = %t, response IDs = %q then %q", connection.conn == underlying, compactedID, continuedID)
	}
}

func collectLiveResponse(t *testing.T, ctx context.Context, client *Client, request damodel.Request) (damodel.Response, int) {
	t.Helper()
	stream, err := client.Stream(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	response := damodel.Response{Message: damessage.Message{Role: damessage.RoleAssistant}}
	streamedChunks := 0
	done := false
	for chunk, nextErr := range stream.Chunks() {
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if chunk.Done {
			done = true
		} else {
			streamedChunks++
		}
		mergeChunk(&response, chunk)
	}
	if !done {
		t.Fatal("live response stream ended without response.completed")
	}
	return response, streamedChunks
}

func liveWebSocketState(t *testing.T, client *Client) (*responsesWebSocketConnection, string) {
	t.Helper()
	if client.websockets == nil {
		t.Fatal("client has no websocket pool")
	}
	client.websockets.mu.Lock()
	defer client.websockets.mu.Unlock()
	if client.websockets.disabled {
		t.Fatal("websocket transport was disabled; the request may have fallen back to HTTP")
	}
	if len(client.websockets.connections) != 1 {
		t.Fatalf("websocket connection count = %d, want 1", len(client.websockets.connections))
	}
	connection := client.websockets.connections[0]
	if connection.busy || connection.conn == nil {
		t.Fatalf("websocket connection state = busy %t, connected %t", connection.busy, connection.conn != nil)
	}
	responseID := ""
	if connection.continuation != nil {
		responseID = connection.continuation.responseID
	}
	return connection, responseID
}
