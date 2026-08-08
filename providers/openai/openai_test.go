package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/tool"
)

func TestInvokeMapsResponsesAPI(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if value := request.Header.Get("Authorization"); value != "Bearer secret" {
			t.Fatalf("authorization = %q", value)
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
          "id":"resp_1","status":"completed",
          "output":[
            {"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"done"}]},
            {"type":"function_call","id":"item_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"go\"}"}
          ],
          "usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}
        }`)
	}))
	defer server.Close()

	client, err := NewAPIKey("secret", Options{Model: "test-model", BaseURL: server.URL, HTTPClient: server.Client(), MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Invoke(context.Background(), model.Request{
		Messages:    []message.Message{message.System("be useful"), message.Human("hello")},
		Tools:       []tool.Definition{{Name: "lookup", Description: "look up a value", InputSchema: json.RawMessage(`{"type":"object"}`), Strict: true}},
		PromptCache: &model.PromptCache{Key: "thread-key", Retention: "24h"},
		Reasoning:   &model.Reasoning{Effort: "high", Summary: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "done" || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].ID != "call_1" || string(response.Message.ToolCalls[0].Arguments) != `{"q":"go"}` {
		t.Fatalf("response = %#v", response)
	}
	if response.Message.Usage == nil || response.Message.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
	if got["model"] != "test-model" || got["parallel_tool_calls"] != true {
		t.Fatalf("request = %#v", got)
	}
	if got["max_output_tokens"] != float64(4096) {
		t.Fatalf("max_output_tokens = %#v", got["max_output_tokens"])
	}
	if _, exists := got["store"]; exists {
		t.Fatalf("API-key request unexpectedly set store: %#v", got["store"])
	}
	if got["prompt_cache_key"] != "thread-key" || got["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt cache = %#v", got)
	}
	reasoning, ok := got["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", got["reasoning"])
	}
	include, ok := got["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", got["include"])
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["strict"] != true {
		t.Fatalf("tools = %#v", got["tools"])
	}
}

func TestInvokeMapsProviderWebSearch(t *testing.T) {
	var got map[string]any
	start, end := 0, 6
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(writer, `{
          "id":"resp_search","status":"completed",
          "output":[
            {"type":"web_search_call","id":"search_1","action":{"type":"search","queries":["dago docs"]}},
            {"type":"message","content":[{"type":"output_text","text":"result","annotations":[{"type":"url_citation","url":"https://example.test","title":"Example","start_index":0,"end_index":6}]}]}
          ]
        }`)
	}))
	defer server.Close()
	client, err := NewAPIKey("secret", Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client(), WebSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Invoke(context.Background(), model.Request{Messages: []message.Message{message.Human("search")}})
	if err != nil {
		t.Fatal(err)
	}
	tools := got["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tools = %#v", tools)
	}
	if len(response.Message.Content) != 2 || response.Message.Content[0].Type != message.BlockServerTool {
		t.Fatalf("content = %#v", response.Message.Content)
	}
	if got := response.Message.Content[0].Extra["arguments"]; string(got) != `{"query":"dago docs"}` {
		t.Fatalf("arguments = %s", got)
	}
	citations := response.Message.Content[1].Citations
	if len(citations) != 1 || citations[0].URL != "https://example.test" || citations[0].StartIndex == nil || *citations[0].StartIndex != start || citations[0].EndIndex == nil || *citations[0].EndIndex != end {
		t.Fatalf("citations = %#v", citations)
	}
}

func TestInvokeMapsToolHistoryAndStructuredOutput(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&got)
		_, _ = io.WriteString(writer, `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"{\"answer\":42}"}]}]}`)
	}))
	defer server.Close()
	client, _ := NewAPIKey("secret", Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	assistant := message.Message{Role: message.RoleAssistant, ToolCalls: []message.ToolCall{{ID: "c", Name: "calculate", Arguments: json.RawMessage(`{"x":40}`)}}}
	response, err := client.Invoke(context.Background(), model.Request{
		Messages:       []message.Message{assistant, message.Tool("c", "42")},
		ResponseFormat: &model.ResponseFormat{Name: "answer", Schema: json.RawMessage(`{"type":"object"}`), Strict: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(response.Structured) != `{"answer":42}` {
		t.Fatalf("structured = %s", response.Structured)
	}
	input := got["input"].([]any)
	if input[0].(map[string]any)["type"] != "function_call" || input[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
}

func TestInvokePreservesAndReplaysEncryptedReasoning(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			_, _ = io.WriteString(writer, `{
                  "id":"r1","output":[
                    {"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Need a lookup."}],"encrypted_content":"opaque-state"},
                    {"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"go\"}"}
                  ]}`)
			return
		}
		_, _ = io.WriteString(writer, `{"id":"r2","output":[{"type":"message","id":"m2","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer server.Close()

	client, _ := NewAPIKey("secret", Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	first, err := client.Invoke(context.Background(), model.Request{Messages: []message.Message{message.Human("look it up")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Message.Content) != 1 || first.Message.Content[0].Type != message.BlockReasoning || first.Message.Content[0].Reasoning != "Need a lookup." {
		t.Fatalf("first reasoning = %#v", first.Message.Content)
	}
	if len(first.Message.ToolCalls) != 1 {
		t.Fatalf("first tool calls = %#v", first.Message.ToolCalls)
	}
	if _, err := client.Invoke(context.Background(), model.Request{Messages: []message.Message{first.Message, message.Tool("call_1", "result")}}); err != nil {
		t.Fatal(err)
	}
	input := requests[1]["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("replayed input = %#v", input)
	}
	reasoning := input[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "opaque-state" {
		t.Fatalf("replayed reasoning = %#v", reasoning)
	}
	if input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("replayed tool turn = %#v", input)
	}
}

func TestStreamPreservesReasoningSummaryAndOpaqueState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"output_index\":0,\"delta\":\"thinking\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"thinking\"}],\"encrypted_content\":\"opaque\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()
	client, _ := NewAPIKey("secret", Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	stream, err := client.Stream(context.Background(), model.Request{Messages: []message.Message{message.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	delta, err := stream.Next(context.Background())
	if err != nil || delta.MessageDelta.Content[0].Reasoning != "thinking" {
		t.Fatalf("reasoning delta = %#v, %v", delta, err)
	}
	state, err := stream.Next(context.Background())
	if err != nil || len(state.MessageDelta.Content[0].Extra[reasoningStateKey]) == 0 {
		t.Fatalf("reasoning state = %#v, %v", state, err)
	}
	done, err := stream.Next(context.Background())
	if err != nil || !done.Done {
		t.Fatalf("done = %#v, %v", done, err)
	}
}

func TestStreamYieldsTextToolCallUsageAndDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"i\",\"call_id\":\"c\",\"name\":\"lookup\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i\",\"delta\":\"{\\\"q\\\":\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i\",\"delta\":\"\\\"go\\\"}\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"i\",\"arguments\":\"{\\\"q\\\":\\\"go\\\"}\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	defer server.Close()
	client, _ := NewAPIKey("secret", Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	stream, err := client.Stream(context.Background(), model.Request{Messages: []message.Message{message.Human("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.MessageDelta.TextContent() != "hi" {
		t.Fatalf("first = %#v, %v", first, err)
	}
	second, err := stream.Next(context.Background())
	if err != nil || len(second.MessageDelta.ToolCalls) != 1 || string(second.MessageDelta.ToolCalls[0].Arguments) != `{"q":"go"}` {
		t.Fatalf("second = %#v, %v", second, err)
	}
	third, err := stream.Next(context.Background())
	if err != nil || !third.Done || third.MessageDelta.Usage.TotalTokens != 3 {
		t.Fatalf("third = %#v, %v", third, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal error = %v", err)
	}
}

func TestContextOverflowIsTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, `{"error":{"message":"too long","code":"context_length_exceeded"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	client, _ := NewAPIKey("secret", Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	_, err := client.Invoke(context.Background(), model.Request{Messages: []message.Message{message.Human("hello")}})
	if !errors.Is(err, model.ErrContextOverflow) {
		t.Fatalf("error = %v", err)
	}
}

func TestOAuthLoginPKCEPersistenceAndRefresh(t *testing.T) {
	var authorizationForm url.Values
	refreshes := 0
	idToken := testJWT(map[string]any{"chatgpt_account_id": "account-1"})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth/token" {
			http.NotFound(writer, request)
			return
		}
		if strings.HasPrefix(request.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			_ = request.ParseForm()
			authorizationForm = request.Form
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "id_token": idToken, "expires_in": 1})
			return
		}
		refreshes++
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "access-2", "refresh_token": "refresh-2", "id_token": idToken, "expires_in": 3600})
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "auth", "tokens.json")
	session, err := Login(context.Background(), OAuthOptions{
		Issuer: server.URL, HTTPClient: server.Client(), Listener: listener, StorePath: storePath,
		OpenURL: func(authorizeURL string) error {
			parsed, err := url.Parse(authorizeURL)
			if err != nil {
				return err
			}
			if parsed.Query().Get("code_challenge_method") != "S256" || parsed.Query().Get("state") == "" {
				t.Fatalf("authorize URL = %s", authorizeURL)
			}
			callback, _ := url.Parse(parsed.Query().Get("redirect_uri"))
			query := callback.Query()
			query.Set("code", "authorization-code")
			query.Set("state", parsed.Query().Get("state"))
			callback.RawQuery = query.Encode()
			response, err := http.Get(callback.String())
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("callback status = %s", response.Status)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorizationForm.Get("code_verifier") == "" || authorizationForm.Get("code") != "authorization-code" {
		t.Fatalf("authorization form = %#v", authorizationForm)
	}
	info, err := os.Stat(storePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, %v", info, err)
	}
	credentials, err := session.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessToken != "access-2" || credentials.AccountID != "account-1" || refreshes != 1 {
		t.Fatalf("credentials = %#v, refreshes = %d", credentials, refreshes)
	}
	loaded, err := LoadOAuthSession(storePath, OAuthOptions{Issuer: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Tokens().RefreshToken != "refresh-2" {
		t.Fatalf("loaded tokens = %#v", loaded.Tokens())
	}
}

func TestOAuthRejectsWrongState(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	_, err := Login(context.Background(), OAuthOptions{
		Issuer: server.URL, HTTPClient: server.Client(), Listener: listener,
		OpenURL: func(authorizeURL string) error {
			parsed, _ := url.Parse(authorizeURL)
			callback := parsed.Query().Get("redirect_uri") + "?code=x&state=wrong"
			response, requestErr := http.Get(callback)
			if response != nil {
				response.Body.Close()
			}
			return requestErr
		},
	})
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func testJWT(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestOAuthCancellation(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Login(ctx, OAuthOptions{Listener: listener, OpenURL: func(string) error { return nil }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestSubscriptionAddsAccountHeader(t *testing.T) {
	var account string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		account = request.Header.Get("ChatGPT-Account-ID")
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()
	client, err := NewSubscription(staticCredentials{Credentials{AccessToken: "token", AccountID: "workspace"}}, Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client(), MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Invoke(context.Background(), model.Request{Messages: []message.Message{message.Human("hello")}})
	if err != nil || account != "workspace" || payload["store"] != false {
		t.Fatalf("account = %q, store = %#v, error = %v", account, payload["store"], err)
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %#v", payload["stream"])
	}
	if _, exists := payload["max_output_tokens"]; exists {
		t.Fatalf("subscription request unexpectedly set max_output_tokens: %#v", payload["max_output_tokens"])
	}
	if response.Message.TextContent() != "ok" || response.Message.Usage == nil || response.Message.Usage.TotalTokens != 2 {
		t.Fatalf("response = %#v", response)
	}
}

func TestOAuthTokenFileRejectsIncompleteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOAuthSession(path, OAuthOptions{})
	if err == nil {
		t.Fatal("expected incomplete token error")
	}
}

func TestExpiredOAuthRefreshHonorsContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	session := &OAuthSession{options: oauthDefaults(OAuthOptions{Issuer: server.URL, HTTPClient: server.Client()}), tokens: OAuthTokens{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := session.Credentials(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}
