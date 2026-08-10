package dago

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxAgentProtocolResponse = 8 << 20

// AgentProtocolOptions configures an Agent Protocol background-run client.
type AgentProtocolOptions struct {
	URL        string
	APIKey     string
	Headers    map[string]string
	HTTPClient *http.Client
}

// AgentProtocolRunner launches and manages remote background agents over the
// language-neutral Agent Protocol HTTP API.
type AgentProtocolRunner struct {
	baseURL *url.URL
	headers http.Header
	client  *http.Client
}

func NewAgentProtocolRunner(options AgentProtocolOptions) (*AgentProtocolRunner, error) {
	baseURL, err := url.Parse(strings.TrimSpace(options.URL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") {
		return nil, fmt.Errorf("agent protocol: valid http(s) URL is required")
	}
	baseURL.RawQuery, baseURL.Fragment = "", ""
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	headers := make(http.Header, len(options.Headers)+3)
	for name, value := range options.Headers {
		if strings.EqualFold(name, "x-api-key") {
			return nil, fmt.Errorf("agent protocol: x-api-key is reserved; use APIKey")
		}
		headers.Set(name, value)
	}
	if headers.Get("x-auth-scheme") == "" {
		headers.Set("x-auth-scheme", "langsmith")
	}
	apiKey := strings.TrimSpace(options.APIKey)
	if apiKey == "" {
		for _, name := range []string{"LANGGRAPH_API_KEY", "LANGSMITH_API_KEY", "LANGCHAIN_API_KEY"} {
			if value := strings.Trim(strings.TrimSpace(os.Getenv(name)), `"'`); value != "" {
				apiKey = value
				break
			}
		}
	}
	if apiKey != "" {
		headers.Set("x-api-key", apiKey)
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &AgentProtocolRunner{baseURL: baseURL, headers: headers, client: &clientCopy}, nil
}

func (runner *AgentProtocolRunner) Start(ctx context.Context, graphID, description string) (AsyncRun, error) {
	if strings.TrimSpace(graphID) == "" {
		return AsyncRun{}, fmt.Errorf("agent protocol: graph id is required")
	}
	var thread struct {
		ThreadID string `json:"thread_id"`
	}
	if err := runner.request(ctx, http.MethodPost, "/threads", map[string]any{}, &thread); err != nil {
		return AsyncRun{}, fmt.Errorf("agent protocol: create thread: %w", err)
	}
	if thread.ThreadID == "" {
		return AsyncRun{}, fmt.Errorf("agent protocol: create thread returned an empty thread id")
	}
	run, err := runner.createRun(ctx, thread.ThreadID, graphID, description, false)
	if err != nil {
		return AsyncRun{}, err
	}
	run.ThreadID = thread.ThreadID
	run.Status = "running"
	return run, nil
}

func (runner *AgentProtocolRunner) Update(ctx context.Context, graphID, threadID, message string) (AsyncRun, error) {
	if strings.TrimSpace(graphID) == "" || strings.TrimSpace(threadID) == "" {
		return AsyncRun{}, fmt.Errorf("agent protocol: graph id and thread id are required")
	}
	run, err := runner.createRun(ctx, threadID, graphID, message, true)
	if err != nil {
		return AsyncRun{}, err
	}
	run.ThreadID = threadID
	run.Status = "running"
	return run, nil
}

func (runner *AgentProtocolRunner) createRun(ctx context.Context, threadID, graphID, content string, interrupt bool) (AsyncRun, error) {
	payload := map[string]any{
		"assistant_id": graphID,
		"input":        map[string]any{"messages": []map[string]any{{"role": "user", "content": content}}},
		"stream_mode":  "values", "stream_subgraphs": false, "stream_resumable": false,
	}
	if interrupt {
		payload["multitask_strategy"] = "interrupt"
	}
	var response struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	endpoint := "/threads/" + url.PathEscape(threadID) + "/runs"
	if err := runner.request(ctx, http.MethodPost, endpoint, payload, &response); err != nil {
		return AsyncRun{}, fmt.Errorf("agent protocol: create run: %w", err)
	}
	if response.RunID == "" {
		return AsyncRun{}, fmt.Errorf("agent protocol: create run returned an empty run id")
	}
	return AsyncRun{RunID: response.RunID, Status: response.Status}, nil
}

func (runner *AgentProtocolRunner) Check(ctx context.Context, threadID, runID string) (AsyncRun, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(runID) == "" {
		return AsyncRun{}, fmt.Errorf("agent protocol: thread id and run id are required")
	}
	var response struct {
		Status string `json:"status"`
		Error  any    `json:"error"`
	}
	base := "/threads/" + url.PathEscape(threadID)
	if err := runner.request(ctx, http.MethodGet, base+"/runs/"+url.PathEscape(runID), nil, &response); err != nil {
		return AsyncRun{}, fmt.Errorf("agent protocol: get run: %w", err)
	}
	result := AsyncRun{ThreadID: threadID, RunID: runID, Status: response.Status}
	if response.Status == "error" {
		result.Error = stringifyProtocolValue(response.Error)
		if result.Error == "" {
			result.Error = "The async subagent encountered an error."
		}
	}
	if response.Status != "success" {
		return result, nil
	}
	var thread struct {
		Values map[string]any `json:"values"`
	}
	if err := runner.request(ctx, http.MethodGet, base, nil, &thread); err != nil {
		result.Result = "(completed with no output messages)"
		return result, nil
	}
	messages, _ := thread.Values["messages"].([]any)
	if len(messages) == 0 {
		result.Result = "(completed with no output messages)"
		return result, nil
	}
	last := messages[len(messages)-1]
	if object, ok := last.(map[string]any); ok {
		content, exists := object["content"]
		if !exists {
			content = ""
		}
		result.ResultValue = content
		result.Result = stringifyProtocolValue(result.ResultValue)
		result.HasResult = true
	} else {
		result.Result = stringifyProtocolValue(last)
		result.HasResult = true
	}
	return result, nil
}

func stringifyProtocolValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func (runner *AgentProtocolRunner) Cancel(ctx context.Context, threadID, runID string) error {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(runID) == "" {
		return fmt.Errorf("agent protocol: thread id and run id are required")
	}
	endpoint := "/threads/" + url.PathEscape(threadID) + "/runs/" + url.PathEscape(runID) + "/cancel?wait=0&action=interrupt"
	if err := runner.request(ctx, http.MethodPost, endpoint, nil, nil); err != nil {
		return fmt.Errorf("agent protocol: cancel run: %w", err)
	}
	return nil
}

func (runner *AgentProtocolRunner) request(ctx context.Context, method, endpoint string, payload any, destination any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	target := strings.TrimRight(runner.baseURL.String(), "/") + endpoint
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	request.Header = runner.headers.Clone()
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := runner.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxAgentProtocolResponse+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(data) > maxAgentProtocolResponse {
		return fmt.Errorf("response exceeds %d bytes", maxAgentProtocolResponse)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if destination == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

var _ AsyncSubagentRunner = (*AgentProtocolRunner)(nil)
