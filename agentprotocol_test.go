package dago

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type protocolRequest struct {
	method  string
	path    string
	query   string
	headers http.Header
	body    map[string]any
}

func TestAgentProtocolRunnerLifecycle(t *testing.T) {
	var mu sync.Mutex
	var requests []protocolRequest
	runIndex := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if request.Body != nil {
			data, _ := io.ReadAll(request.Body)
			if len(data) > 0 {
				_ = json.Unmarshal(data, &body)
			}
		}
		mu.Lock()
		requests = append(requests, protocolRequest{method: request.Method, path: request.URL.EscapedPath(), query: request.URL.RawQuery, headers: request.Header.Clone(), body: body})
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.EscapedPath() == "/api/threads":
			_, _ = writer.Write([]byte(`{"thread_id":"thread/1"}`))
		case request.Method == http.MethodPost && request.URL.EscapedPath() == "/api/threads/thread%2F1/runs":
			runIndex++
			_, _ = writer.Write([]byte(`{"run_id":"run/` + string(rune('0'+runIndex)) + `","status":"pending"}`))
		case request.Method == http.MethodGet && request.URL.EscapedPath() == "/api/threads/thread%2F1/runs/run%2F1":
			_, _ = writer.Write([]byte(`{"status":"success"}`))
		case request.Method == http.MethodGet && request.URL.EscapedPath() == "/api/threads/thread%2F1":
			_, _ = writer.Write([]byte(`{"values":{"messages":[{"role":"assistant","content":"report"}]}}`))
		case request.Method == http.MethodPost && request.URL.EscapedPath() == "/api/threads/thread%2F1/runs/run%2F2/cancel":
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.Error(writer, "unexpected "+request.Method+" "+request.URL.String(), http.StatusBadRequest)
		}
	}))
	defer server.Close()
	runner, err := NewAgentProtocolRunner(AgentProtocolOptions{
		URL: server.URL + "/api/", APIKey: "secret", Headers: map[string]string{"X-Custom": "yes"}, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started, err := runner.Start(ctx, "research", "find it")
	if err != nil || started.ThreadID != "thread/1" || started.RunID != "run/1" || started.Status != "running" {
		t.Fatalf("start = %#v, err = %v", started, err)
	}
	checked, err := runner.Check(ctx, started.ThreadID, started.RunID)
	if err != nil || checked.Status != "success" || checked.Result != "report" {
		t.Fatalf("check = %#v, err = %v", checked, err)
	}
	updated, err := runner.Update(ctx, "research", started.ThreadID, "more")
	if err != nil || updated.RunID != "run/2" || updated.Status != "running" {
		t.Fatalf("update = %#v, err = %v", updated, err)
	}
	if err := runner.Cancel(ctx, updated.ThreadID, updated.RunID); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 6 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].headers.Get("x-api-key") != "secret" || requests[0].headers.Get("x-auth-scheme") != "langsmith" || requests[0].headers.Get("X-Custom") != "yes" {
		t.Fatalf("headers = %#v", requests[0].headers)
	}
	startInput := requests[1].body["input"].(map[string]any)["messages"].([]any)[0].(map[string]any)
	if requests[1].body["assistant_id"] != "research" || startInput["content"] != "find it" {
		t.Fatalf("start body = %#v", requests[1].body)
	}
	if requests[4].body["multitask_strategy"] != "interrupt" {
		t.Fatalf("update body = %#v", requests[4].body)
	}
	if requests[5].query != "wait=0&action=interrupt" {
		t.Fatalf("cancel query = %q", requests[5].query)
	}
}

func TestAgentProtocolCheckErrorAndMissingOutput(t *testing.T) {
	status := "error"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/runs/run") {
			if status == "error" {
				_, _ = writer.Write([]byte(`{"status":"error","error":{"message":"broken"}}`))
			} else {
				_, _ = writer.Write([]byte(`{"status":"success"}`))
			}
			return
		}
		_, _ = writer.Write([]byte(`{"values":{}}`))
	}))
	defer server.Close()
	runner, _ := NewAgentProtocolRunner(AgentProtocolOptions{URL: server.URL, APIKey: "none", HTTPClient: server.Client()})
	failed, err := runner.Check(context.Background(), "thread", "run")
	if err != nil || failed.Error != `{"message":"broken"}` {
		t.Fatalf("failed = %#v, err = %v", failed, err)
	}
	status = "success"
	finished, err := runner.Check(context.Background(), "thread", "run")
	if err != nil || finished.Result != "(completed with no output messages)" {
		t.Fatalf("finished = %#v, err = %v", finished, err)
	}
}

func TestAgentProtocolCheckIgnoresThreadFetchFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/runs/") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"status":"success"}`))
			return
		}
		http.Error(writer, "down", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	runner, _ := NewAgentProtocolRunner(AgentProtocolOptions{URL: server.URL, APIKey: "none", HTTPClient: server.Client()})
	result, err := runner.Check(context.Background(), "thread", "run")
	if err != nil || result.Result != "(completed with no output messages)" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestAgentProtocolPreservesStructuredMessageContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.Contains(request.URL.Path, "/runs/") {
			_, _ = writer.Write([]byte(`{"status":"success"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"values":{"messages":[{"content":[{"type":"text","text":"report"}]}]}}`))
	}))
	defer server.Close()
	runner, _ := NewAgentProtocolRunner(AgentProtocolOptions{URL: server.URL, APIKey: "none", HTTPClient: server.Client()})
	result, err := runner.Check(context.Background(), "thread", "run")
	parts, ok := result.ResultValue.([]any)
	if err != nil || !ok || len(parts) != 1 || !strings.Contains(result.Result, "report") {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestAgentProtocolDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	runner, _ := NewAgentProtocolRunner(AgentProtocolOptions{URL: server.URL, APIKey: "secret", HTTPClient: server.Client()})
	if _, err := runner.Start(context.Background(), "graph", "task"); err == nil {
		t.Fatal("redirect succeeded")
	}
	if redirected != 0 {
		t.Fatalf("redirect target received %d requests", redirected)
	}
}

func TestAgentProtocolConfigurationValidationAndEnvironmentKey(t *testing.T) {
	for _, value := range []string{"", "localhost:8123", "ftp://example.com"} {
		if _, err := NewAgentProtocolRunner(AgentProtocolOptions{URL: value}); err == nil {
			t.Fatalf("URL %q accepted", value)
		}
	}
	if _, err := NewAgentProtocolRunner(AgentProtocolOptions{URL: "https://example.com", Headers: map[string]string{"X-API-Key": "bad"}}); err == nil {
		t.Fatal("reserved header accepted")
	}
	t.Setenv("LANGGRAPH_API_KEY", " env-key ")
	runner, err := NewAgentProtocolRunner(AgentProtocolOptions{URL: "https://example.com"})
	if err != nil || runner.headers.Get("x-api-key") != "env-key" {
		t.Fatalf("key = %q, err = %v", runner.headers.Get("x-api-key"), err)
	}
}

func TestAsyncSubagentURLBuildsAgentProtocolRunner(t *testing.T) {
	middleware, err := AsyncSubagentMiddleware(AsyncSubagentOptions{Subagents: []AsyncSubagent{{
		Name: "remote", Description: "Remote worker", GraphID: "worker", URL: "https://example.com",
	}}})
	if err != nil || len(middleware.Tools) != 5 {
		t.Fatalf("middleware = %#v, err = %v", middleware, err)
	}
}
