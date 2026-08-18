package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon"
)

type fakeHTTPClient struct {
	do func(*http.Request) (*http.Response, error)
}

func (client *fakeHTTPClient) Do(request *http.Request) (*http.Response, error) {
	return client.do(request)
}

func TestConstructorsRequireDependenciesAndBoundedDefaults(t *testing.T) {
	client := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return apiResponse(http.StatusOK, telegramUser{ID: 1}), nil
	}}
	channel := New("123:test-token", client, Options{})
	options := channel.Options()
	if options.APIBase != defaultAPIBase || options.PollTimeout != 30*time.Second ||
		options.PollInterval != time.Second || options.RequestTimeout != 35*time.Second ||
		options.MaxUpdates != 100 || options.MaxResponseBytes != 1<<20 ||
		options.Exposure.Mode() != ExposureSelf {
		t.Fatalf("defaults = %+v", options)
	}

	requirePanic(t, func() { New("", client, Options{}) })
	requirePanic(t, func() { New("token/escape", client, Options{}) })
	requirePanic(t, func() { New("token%2Fescape", client, Options{}) })
	var typedNil *fakeHTTPClient
	requirePanic(t, func() { New("token", typedNil, Options{}) })
	requirePanic(t, func() { NewWebhook("token", client, "", Options{}) })
	requirePanic(t, func() { OpenExposure("yes") })
	requirePanic(t, func() { SelfExposure() })
	requirePanic(t, func() {
		New("token", client, Options{MaxUpdates: 101})
	})
}

func TestWebhookMapsAllowedMessagesAndAuthenticates(t *testing.T) {
	client := getMeClient(999)
	channel := NewWebhook("123:test-token", client, "hook_secret", Options{
		Exposure:      AllowlistExposure([]string{"111"}, []string{"-10022"}),
		MaxMediaBytes: 10,
	})
	var mu sync.Mutex
	var messages []datalon.Message
	if err := channel.Start(t.Context(), func(_ context.Context, message datalon.Message) error {
		mu.Lock()
		messages = append(messages, message)
		mu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Stop(context.Background()) })

	private := map[string]any{
		"update_id": 7,
		"message": map[string]any{
			"message_id": 10,
			"chat":       map[string]any{"id": 111, "type": "private"},
			"from":       map[string]any{"id": 111},
			"caption":    "bounded document",
			"document": map[string]any{
				"file_id": "file-1", "file_size": 11,
				"file_name": "report.pdf", "mime_type": "application/pdf",
			},
		},
	}
	response := webhookRequest(channel, "hook_secret", private)
	if response.Code != http.StatusNoContent {
		t.Fatalf("private webhook status = %d, body = %q", response.Code, response.Body.String())
	}

	channelPost := map[string]any{
		"update_id": 8,
		"channel_post": map[string]any{
			"message_id": 12,
			"chat":       map[string]any{"id": -10022, "type": "channel"},
			"text":       "channel input",
		},
	}
	if response := webhookRequest(channel, "hook_secret", channelPost); response.Code != http.StatusNoContent {
		t.Fatalf("channel webhook status = %d", response.Code)
	}

	mu.Lock()
	got := append([]datalon.Message(nil), messages...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("messages = %#v", got)
	}
	if got[0].ConversationID != "111" || got[0].SenderID != "111" ||
		got[0].MessageID != "10" || got[0].Text != "bounded document" ||
		got[0].Metadata["provider"] != "telegram" ||
		got[0].Metadata["chat_type"] != "private" ||
		got[0].Metadata["has_media"] != false ||
		got[0].Metadata["media_error"] == "" {
		t.Fatalf("private message = %#v", got[0])
	}
	if _, exists := got[0].Metadata["file_id"]; exists {
		t.Fatal("oversized media retained its downloadable file ID")
	}
	if got[1].ConversationID != "-10022" || got[1].SenderID != "" ||
		got[1].Text != "channel input" || got[1].Metadata["chat_type"] != "channel" {
		t.Fatalf("channel post = %#v", got[1])
	}

	before := len(got)
	if response := webhookRequest(channel, "wrong", private); response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d", response.Code)
	}
	disallowed := map[string]any{
		"message": map[string]any{
			"message_id": 11,
			"chat":       map[string]any{"id": 222, "type": "private"},
			"from":       map[string]any{"id": 222},
			"text":       "blocked",
		},
	}
	if response := webhookRequest(channel, "hook_secret", disallowed); response.Code != http.StatusNoContent {
		t.Fatalf("disallowed status = %d", response.Code)
	}
	group := map[string]any{
		"message": map[string]any{
			"message_id": 13,
			"chat":       map[string]any{"id": -44, "type": "group"},
			"from":       map[string]any{"id": 111},
			"text":       "ignored group",
		},
	}
	if response := webhookRequest(channel, "hook_secret", group); response.Code != http.StatusNoContent {
		t.Fatalf("group status = %d", response.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != before {
		t.Fatalf("unauthorized or unsupported messages dispatched: %#v", messages[before:])
	}
}

func TestWebhookRejectsOversizedInvalidAndFailedUpdates(t *testing.T) {
	channel := NewWebhook("token", getMeClient(1), "secret", Options{
		Exposure:        OpenExposure(OpenExposureAcknowledgement),
		MaxWebhookBytes: 128,
	})
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error {
		return errors.New("agent failed")
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Stop(context.Background()) })

	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 129)))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	response := httptest.NewRecorder()
	channel.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", response.Code)
	}

	if response := webhookRaw(channel, "secret", "{"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d", response.Code)
	}
	valid := `{"message":{"message_id":1,"chat":{"id":2,"type":"private"},"from":{"id":2},"text":"run"}}`
	if response := webhookRaw(channel, "secret", valid); response.Code != http.StatusInternalServerError {
		t.Fatalf("handler failure status = %d", response.Code)
	}
}

func TestWebhookIntegratesWithDatalonHost(t *testing.T) {
	var sent map[string]any
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		switch methodFromRequest(request) {
		case "getMe":
			return apiResponse(http.StatusOK, telegramUser{ID: 99}), nil
		case "sendMessage":
			if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
				return nil, err
			}
			return apiResponse(http.StatusOK, map[string]any{"message_id": 42}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}}
	channel := NewWebhook("token", client, "secret", Options{
		Exposure: SelfExposure("7"),
	})
	host := datalon.NewHost(nil, datalon.Config{
		StateRoot: t.TempDir(), Workspace: t.TempDir(),
	}, channel)
	if err := host.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background()) })
	payload := `{"message":{"message_id":3,"chat":{"id":7,"type":"private"},"from":{"id":7},"text":"echo me"}}`
	response := webhookRaw(channel, "secret", payload)
	if response.Code != http.StatusNoContent {
		t.Fatalf("webhook status = %d, body = %q", response.Code, response.Body.String())
	}
	if sent["chat_id"] != "7" || sent["text"] != "echo me" {
		t.Fatalf("outbound send = %#v", sent)
	}
}

func TestStopCancelsAndWaitsForWebhookHandler(t *testing.T) {
	channel := NewWebhook("token", getMeClient(1), "secret", Options{
		Exposure: OpenExposure(OpenExposureAcknowledgement),
	})
	started := make(chan struct{})
	if err := channel.Start(t.Context(), func(ctx context.Context, _ datalon.Message) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		payload := `{"message":{"message_id":1,"chat":{"id":2,"type":"private"},"from":{"id":2},"text":"wait"}}`
		response <- webhookRaw(channel, "secret", payload)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("webhook handler did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := channel.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-response:
		if result.Code != http.StatusInternalServerError {
			t.Fatalf("canceled webhook status = %d", result.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook request did not finish after Stop")
	}
}

func TestLongPollingPersistsOffsetAndCancellationStopsBlockedRequest(t *testing.T) {
	offsetFile := t.TempDir() + "/telegram/offset.json"
	updates := []map[string]any{
		{
			"update_id": 10,
			"message": map[string]any{
				"message_id": 1,
				"chat":       map[string]any{"id": 111, "type": "private"},
				"from":       map[string]any{"id": 111},
				"text":       "first",
			},
		},
		{
			"update_id": 11,
			"channel_post": map[string]any{
				"message_id": 2,
				"chat":       map[string]any{"id": -10022, "type": "channel"},
				"text":       "second",
			},
		},
	}
	var polls atomic.Int32
	blocked := make(chan struct{})
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		switch methodFromRequest(request) {
		case "getMe":
			return apiResponse(http.StatusOK, telegramUser{ID: 999}), nil
		case "getUpdates":
			if polls.Add(1) == 1 {
				return apiResponse(http.StatusOK, updates), nil
			}
			select {
			case <-blocked:
			default:
				close(blocked)
			}
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			t.Fatalf("unexpected method %q", methodFromRequest(request))
			return nil, nil
		}
	}}
	channel := New("123:secret", client, Options{
		Exposure:     AllowlistExposure([]string{"111"}, []string{"-10022"}),
		OffsetFile:   offsetFile,
		PollInterval: time.Millisecond,
		PollTimeout:  time.Second,
	})
	received := make(chan datalon.Message, 2)
	if err := channel.Start(t.Context(), func(_ context.Context, message datalon.Message) error {
		received <- message
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for polling message")
		}
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("second long poll did not begin")
	}
	if err := channel.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	offset, err := loadOffset(offsetFile)
	if err != nil || offset != 12 {
		t.Fatalf("offset = %d, error = %v", offset, err)
	}
	if info, err := os.Stat(offsetFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("offset mode error = %v, info = %#v", err, info)
	}
}

func TestPollingRetriesHandlerFailureWithoutAdvancingOffset(t *testing.T) {
	update := map[string]any{
		"update_id": 20,
		"message": map[string]any{
			"message_id": 1,
			"chat":       map[string]any{"id": 111, "type": "private"},
			"from":       map[string]any{"id": 111},
			"text":       "retry",
		},
	}
	var polls atomic.Int32
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		if methodFromRequest(request) == "getMe" {
			return apiResponse(http.StatusOK, telegramUser{ID: 999}), nil
		}
		polls.Add(1)
		return apiResponse(http.StatusOK, []any{update}), nil
	}}
	channel := New("token", client, Options{
		Exposure:      SelfExposure("111"),
		PollInterval:  time.Millisecond,
		PollTimeout:   time.Second,
		MaxErrorBytes: 16,
	})
	calls := make(chan struct{}, 2)
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error {
		select {
		case calls <- struct{}{}:
		default:
		}
		return errors.New(strings.Repeat("private", 20))
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Stop(context.Background()) })
	for range 2 {
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("handler failure was not retried")
		}
	}
	if polls.Load() < 2 {
		t.Fatalf("polls = %d", polls.Load())
	}
	if err := channel.LastError(); err == nil || len([]rune(err.Error())) > 16 {
		t.Fatalf("bounded last error = %v", err)
	}
}

func TestSendChunksUnicodeAndSanitizesRetryableErrors(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		if methodFromRequest(request) == "getMe" {
			return apiResponse(http.StatusOK, telegramUser{ID: 1}), nil
		}
		var params map[string]any
		if err := json.NewDecoder(request.Body).Decode(&params); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		texts = append(texts, params["text"].(string))
		count := len(texts)
		mu.Unlock()
		return apiResponse(http.StatusOK, map[string]any{"message_id": count}), nil
	}}
	channel := New("123:do-not-leak", client, Options{})
	text := strings.Repeat("🙂", maxTelegramTextRunes+1)
	result := channel.Send(t.Context(), "-123", text)
	if !result.Success || result.MessageID != "2" {
		t.Fatalf("send result = %#v", result)
	}
	mu.Lock()
	if len(texts) != 2 || len([]rune(texts[0])) != maxTelegramTextRunes ||
		len([]rune(texts[1])) != 1 || strings.Join(texts, "") != text {
		t.Fatalf("chunks = %#v", texts)
	}
	mu.Unlock()

	failing := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return apiErrorResponse(http.StatusTooManyRequests, "slow down", 3600), nil
	}}
	failed := New("123:credential", failing, Options{MaxRetryDelay: time.Millisecond}).
		Send(t.Context(), "12", "hello")
	if failed.Success || !failed.Retryable || !strings.Contains(failed.Error, "slow down") ||
		strings.Contains(failed.Error, "credential") {
		t.Fatalf("failed send = %#v", failed)
	}

	htmlFailure := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader("gateway unavailable")),
			Header:     make(http.Header),
		}, nil
	}}
	failed = New("token", htmlFailure, Options{}).Send(t.Context(), "12", "hello")
	if failed.Success || !failed.Retryable || strings.Contains(failed.Error, "gateway unavailable") {
		t.Fatalf("non-JSON gateway failure = %#v", failed)
	}
}

func TestResponseAndUpdateBoundsFailClosed(t *testing.T) {
	oversized := &fakeHTTPClient{do: func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", 65))),
			Header:     make(http.Header),
		}, nil
	}}
	channel := New("token", oversized, Options{MaxResponseBytes: 64})
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error { return nil }); !errors.Is(err, ErrPayloadTooBig) {
		t.Fatalf("oversized response error = %v", err)
	}

	var pollCalls atomic.Int32
	tooMany := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		if methodFromRequest(request) == "getMe" {
			return apiResponse(http.StatusOK, telegramUser{ID: 1}), nil
		}
		pollCalls.Add(1)
		return apiResponse(http.StatusOK, []any{
			map[string]any{"update_id": 1},
			map[string]any{"update_id": 2},
		}), nil
	}}
	channel = New("token", tooMany, Options{
		Exposure: SelfExposure("1"), MaxUpdates: 1, PollInterval: time.Millisecond,
	})
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error {
		t.Fatal("oversized update batch dispatched")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Stop(context.Background()) })
	deadline := time.Now().Add(time.Second)
	for channel.LastError() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(channel.LastError(), ErrPayloadTooBig) || pollCalls.Load() == 0 {
		t.Fatalf("last error = %v, polls = %d", channel.LastError(), pollCalls.Load())
	}
}

func TestAPIErrorsCapRetryDelayAndDoNotExposeCredentialedURL(t *testing.T) {
	if got := boundedRetryAfter(math.MaxFloat64, time.Second); got != time.Second {
		t.Fatalf("huge retry-after = %s", got)
	}
	var calls atomic.Int32
	client := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		if methodFromRequest(request) == "getMe" {
			return apiResponse(http.StatusOK, telegramUser{ID: 1}), nil
		}
		calls.Add(1)
		return apiErrorResponse(http.StatusTooManyRequests, "rate limited", 3600), nil
	}}
	channel := New("123:credential", client, Options{
		Exposure: SelfExposure("1"), PollInterval: time.Second,
		MaxRetryDelay: 2 * time.Millisecond,
	})
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Stop(context.Background()) })
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("retry-after was not capped; calls = %d", calls.Load())
	}
	err := channel.LastError()
	if err == nil || strings.Contains(err.Error(), "credential") {
		t.Fatalf("last error leaked credential or is missing: %v", err)
	}
}

func TestTransportAndHandlerPanicsDoNotLeakOrCrash(t *testing.T) {
	credential := "123:sensitive-token"
	leaking := &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		return nil, errors.New(request.URL.String())
	}}
	channel := New(credential, leaking, Options{})
	err := channel.Start(t.Context(), func(context.Context, datalon.Message) error { return nil })
	if err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("transport error leaked credential: %v", err)
	}

	webhookChannel := NewWebhook("token", getMeClient(99), "secret", Options{
		Exposure: OpenExposure(OpenExposureAcknowledgement),
	})
	if err := webhookChannel.Start(t.Context(), func(context.Context, datalon.Message) error {
		panic("private panic value")
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = webhookChannel.Stop(context.Background()) })
	payload := `{"message":{"message_id":1,"chat":{"id":2,"type":"private"},"from":{"id":2},"text":"run"}}`
	response := webhookRaw(webhookChannel, "secret", payload)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private panic value") {
		t.Fatalf("panic response = %d %q", response.Code, response.Body.String())
	}
}

func webhookRequest(channel *Channel, secret string, payload any) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(payload)
	return webhookRaw(channel, secret, string(encoded))
}

func webhookRaw(channel *Channel, secret, payload string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
	request.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	response := httptest.NewRecorder()
	channel.ServeHTTP(response, request)
	return response
}

func getMeClient(id int64) *fakeHTTPClient {
	return &fakeHTTPClient{do: func(request *http.Request) (*http.Response, error) {
		if methodFromRequest(request) != "getMe" {
			return nil, errors.New("unexpected request")
		}
		return apiResponse(http.StatusOK, telegramUser{ID: id}), nil
	}}
}

func apiResponse(status int, result any) *http.Response {
	payload, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(string(payload))),
		Header: make(http.Header),
	}
}

func apiErrorResponse(status int, description string, retryAfter float64) *http.Response {
	payload, _ := json.Marshal(map[string]any{
		"ok": false, "description": description,
		"parameters": map[string]any{"retry_after": retryAfter},
	})
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(string(payload))),
		Header: make(http.Header),
	}
}

func methodFromRequest(request *http.Request) string {
	parts := strings.Split(request.URL.Path, "/")
	return parts[len(parts)-1]
}

func requirePanic(t *testing.T, run func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	run()
}
