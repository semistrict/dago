package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	client, err := NewAPIKey("secret", Options{Model: "test-model", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Invoke(context.Background(), model.Request{
		Messages:    []message.Message{message.System("be useful"), message.Human("hello")},
		Tools:       []tool.Definition{{Name: "lookup", Description: "look up a value", InputSchema: json.RawMessage(`{"type":"object"}`), Strict: true}},
		PromptCache: &model.PromptCache{Key: "thread-key", Retention: "24h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.TextContent() != "done" || len(response.Message.ToolCalls) != 1 || response.Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("response = %#v", response)
	}
	if response.Message.Usage == nil || response.Message.Usage.TotalTokens != 12 {
		t.Fatalf("usage = %#v", response.Message.Usage)
	}
	if got["model"] != "test-model" || got["parallel_tool_calls"] != true {
		t.Fatalf("request = %#v", got)
	}
	if got["prompt_cache_key"] != "thread-key" || got["prompt_cache_retention"] != "24h" {
		t.Fatalf("prompt cache = %#v", got)
	}
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["strict"] != true {
		t.Fatalf("tools = %#v", got["tools"])
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

func TestStreamYieldsTextToolCallUsageAndDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"i\",\"call_id\":\"c\",\"name\":\"lookup\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i\",\"delta\":\"{\\\"q\\\":\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"i\",\"delta\":\"\\\"go\\\"}\"}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"i\"}\n\n")
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
			if err == nil {
				response.Body.Close()
			}
			return err
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
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		account = request.Header.Get("ChatGPT-Account-ID")
		_, _ = io.WriteString(writer, `{"id":"r","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer server.Close()
	client, err := NewSubscription(staticCredentials{Credentials{AccessToken: "token", AccountID: "workspace"}}, Options{Model: "m", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Invoke(context.Background(), model.Request{Messages: []message.Message{message.Human("hello")}})
	if err != nil || account != "workspace" {
		t.Fatalf("account = %q, error = %v", account, err)
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
