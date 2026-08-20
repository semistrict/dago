package lazycue

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/daproviders/openai"
)

func TestIsRetryableAnthropicErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"429 too many requests", &openai.Error{Status: http.StatusTooManyRequests}, true},
		{"500 server error", &openai.Error{Status: http.StatusInternalServerError}, true},
		{"503 unavailable", &openai.Error{Status: http.StatusServiceUnavailable}, true},
		{"400 bad request", &openai.Error{Status: http.StatusBadRequest}, false},
		{"401 unauthorized", &openai.Error{Status: http.StatusUnauthorized}, false},
		{"wrapped 503", fmt.Errorf("model request: %w", &openai.Error{Status: 503}), true},
		{"transport error", errors.New("openai: request: connection reset by peer"), true},
		{"decode error", errors.New("openai: decode response: unexpected EOF"), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isRetryableModelError(context.Background(), test.err); got != test.want {
				t.Errorf("isRetryableModelError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestRunAgentRejectsUnknownModeBeforeNetwork(t *testing.T) {
	result, err := runAgent(context.Background(), &Browser{}, "https://app.example", "model", "key", "description", agentConfig{Mode: agentMode(99)})
	if err == nil || result != nil {
		t.Fatalf("RunAgent = (%v, %v), want nil and error", result, err)
	}
}

func TestRunAPIsRejectNilContextBeforeWork(t *testing.T) {
	if result, err := Run(nil, "https://app.example", Options{}, "description"); err == nil || result != nil {
		t.Fatalf("Run = (%v, %v), want nil and error", result, err)
	}
	if result, err := runAgent(nil, &Browser{}, "https://app.example", "model", "key", "description", agentConfig{}); err == nil || result != nil {
		t.Fatalf("RunAgent = (%v, %v), want nil and error", result, err)
	}
}

const validResponsesResponse = `{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`

func newTestChat(t *testing.T, baseURL string) *retryingChat {
	t.Helper()
	client := openai.NewAPIKey("test-key", "gpt-5.6-luna", openai.Options{BaseURL: baseURL, RetryBackoff: []time.Duration{}})
	return &retryingChat{inner: client, attempts: modelMaxAttempts, backoff: 0}
}

func invokeTestChat(ctx context.Context, chat damodel.Chat) (damodel.Response, error) {
	return chat.Invoke(ctx, damodel.Request{Messages: []damessage.Message{damessage.Human("hi")}})
}

func TestCallAnthropicRetriesTransient5xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, validResponsesResponse)
	}))
	defer server.Close()

	response, err := invokeTestChat(context.Background(), newTestChat(t, server.URL))
	if err != nil {
		t.Fatalf("native model returned error after retries: %v", err)
	}
	if got := response.Message.TextContent(); got != "ok" {
		t.Fatalf("response text = %q, want ok", got)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestCallAnthropicDoesNotRetry4xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := invokeTestChat(context.Background(), newTestChat(t, server.URL))
	if err == nil {
		t.Fatal("expected error from 400 response")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestCallAnthropicExhaustsAttempts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := invokeTestChat(context.Background(), newTestChat(t, server.URL))
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := calls.Load(); got != modelMaxAttempts {
		t.Errorf("attempts = %d, want %d", got, modelMaxAttempts)
	}
}

func TestCallAnthropicStopsOnContextCancel(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		http.Error(w, "released", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	chat := newTestChat(t, server.URL)
	done := make(chan error, 1)
	go func() {
		_, err := invokeTestChat(ctx, chat)
		done <- err
	}()
	<-started
	cancel()
	err := <-done
	close(release)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}
