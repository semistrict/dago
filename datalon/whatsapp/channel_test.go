package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon"
)

type postCall struct {
	path    string
	payload any
}

type fakeTransport struct {
	mu sync.Mutex

	messages json.RawMessage
	health   json.RawMessage
	post     json.RawMessage
	getErr   error
	postErr  error
	posts    []postCall
	onGet    func(string)
	onPost   func(string)
}

func (transport *fakeTransport) Get(_ context.Context, path string) (json.RawMessage, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if transport.onGet != nil {
		transport.onGet(path)
	}
	if transport.getErr != nil {
		return nil, transport.getErr
	}
	if path == "/health" {
		if transport.health == nil {
			return json.RawMessage(`{"status":"connected"}`), nil
		}
		return append(json.RawMessage(nil), transport.health...), nil
	}
	if transport.messages == nil {
		return json.RawMessage(`[]`), nil
	}
	result := append(json.RawMessage(nil), transport.messages...)
	transport.messages = json.RawMessage(`[]`)
	return result, nil
}

func (transport *fakeTransport) Post(_ context.Context, path string, payload any) (json.RawMessage, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.posts = append(transport.posts, postCall{path: path, payload: payload})
	if transport.onPost != nil {
		transport.onPost(path)
	}
	if transport.postErr != nil {
		return nil, transport.postErr
	}
	if transport.post == nil {
		return json.RawMessage(`{"success":true,"message_id":"sent"}`), nil
	}
	return append(json.RawMessage(nil), transport.post...), nil
}

func newTestChannel(t *testing.T, transport Transport, options Options) *Channel {
	t.Helper()
	root := t.TempDir()
	if options.InboundMediaDir == "" {
		options.InboundMediaDir = filepath.Join(root, "inbound")
	}
	if options.OutboundMediaRoot == "" {
		options.OutboundMediaRoot = filepath.Join(root, "outbound")
		if err := os.MkdirAll(options.OutboundMediaRoot, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return New(transport, filepath.Join(root, "session"), options)
}

func installHandler(channel *Channel, handler datalon.Handler) {
	channel.mu.Lock()
	channel.handler = handler
	channel.mu.Unlock()
}

func TestPollParsesAliasesAndAppliesSelfExposure(t *testing.T) {
	transport := &fakeTransport{messages: json.RawMessage(`[
        {"body":"self","chatId":"chat","senderId":"operator","messageId":"one","messageType":"chat","fromSelf":true},
        {"text":"operator","chat_id":"chat","user_id":"backup","message_id":"two"},
        {"text":"blocked","chat_id":"chat","user_id":"other","message_id":"three"}
    ]`)}
	channel := newTestChannel(t, transport, Options{Exposure: Exposure{OperatorIDs: []string{"backup"}}})
	var received []datalon.Message
	installHandler(channel, func(_ context.Context, message datalon.Message) error {
		received = append(received, message)
		return nil
	})
	if err := channel.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[0].Text != "self" || received[0].Metadata["provider"] != "whatsapp" || received[1].SenderID != "backup" {
		t.Fatalf("received = %#v", received)
	}
}

func TestAllowlistUsesConversationOrCaseSensitiveMentionGlob(t *testing.T) {
	transport := &fakeTransport{messages: json.RawMessage(`[
        {"text":"anything","chat_id":"allowed"},
        {"text":"@agent please","chat_id":"other"},
        {"text":"@Agent please","chat_id":"blocked"}
    ]`)}
	channel := newTestChannel(t, transport, Options{Exposure: Exposure{
		Mode: ExposureAllowlist, Conversations: []string{"allowed"}, MentionPatterns: []string{"@agent *"},
	}})
	var received []string
	installHandler(channel, func(_ context.Context, message datalon.Message) error {
		received = append(received, message.ConversationID)
		return nil
	})
	if err := channel.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(received, []string{"allowed", "other"}) {
		t.Fatalf("received = %#v", received)
	}
}

func TestInboundMediaIsClassifiedBoundedAndConfined(t *testing.T) {
	root := t.TempDir()
	voice := filepath.Join(root, "voice.ogg")
	large := filepath.Join(root, "large.ogg")
	if err := os.WriteFile(voice, []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(large, []byte("oversized"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "pending.ogg")
	outside := filepath.Join(t.TempDir(), "secret.ogg")
	transport := &fakeTransport{messages: json.RawMessage(fmt.Sprintf(`[
        {"body":"media","chatId":"chat","messageType":"document","mediaType":"document",
         "mediaPaths":[%q,%q,%q,%q],"mediaMimeTypes":["audio/ogg","audio/ogg","audio/ogg","audio/ogg"],"fromSelf":true}
    ]`, voice, large, missing, outside))}
	channel := New(transport, filepath.Join(t.TempDir(), "session"), Options{InboundMediaDir: root, OutboundMediaRoot: t.TempDir(), MaxMediaBytes: 5})
	var received datalon.Message
	installHandler(channel, func(_ context.Context, message datalon.Message) error { received = message; return nil })
	if err := channel.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	resolvedVoice, err := filepath.EvalSymlinks(voice)
	if err != nil {
		t.Fatal(err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(missing))
	if err != nil {
		t.Fatal(err)
	}
	resolvedMissing := filepath.Join(resolvedParent, filepath.Base(missing))
	if received.Metadata["media_type"] != "voice" || received.Metadata["voice_path"] != resolvedVoice || !reflect.DeepEqual(received.Metadata["media_paths"], []string{resolvedVoice, resolvedMissing}) {
		t.Fatalf("metadata = %#v", received.Metadata)
	}
}

func TestAllRejectedInboundMediaRetainsTextAndAddsError(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "voice.ogg")
	if err := os.WriteFile(media, []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{messages: json.RawMessage(fmt.Sprintf(`[{"body":"oversized","chatId":"chat","mediaType":"voice","mediaPaths":[%q],"fromSelf":true}]`, media))}
	channel := New(transport, filepath.Join(t.TempDir(), "session"), Options{InboundMediaDir: root, OutboundMediaRoot: t.TempDir(), MaxMediaBytes: 1})
	var received datalon.Message
	installHandler(channel, func(_ context.Context, message datalon.Message) error { received = message; return nil })
	if err := channel.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if received.Text != "oversized" || received.Metadata["has_media"] != false || received.Metadata["media_error"] == "" {
		t.Fatalf("message = %#v", received)
	}
}

func TestSendFormatsChunksAddsHeaderAndProjectsFailures(t *testing.T) {
	transport := &fakeTransport{}
	channel := newTestChannel(t, transport, Options{})
	result := channel.Send(t.Context(), "chat", "**bold** "+strings.Repeat("x", 4_096))
	if !result.Success || result.MessageID != "sent" {
		t.Fatalf("Send = %#v", result)
	}
	transport.mu.Lock()
	posts := append([]postCall(nil), transport.posts...)
	transport.mu.Unlock()
	if len(posts) < 2 || posts[0].path != "/send" {
		t.Fatalf("posts = %#v", posts)
	}
	first := posts[0].payload.(map[string]any)["text"].(string)
	second := posts[1].payload.(map[string]any)["text"].(string)
	if first != "*deepagents bot*\n*bold*" || !strings.HasPrefix(second, "*deepagents bot*\n") || len([]rune(second)) > 4_096 {
		t.Fatalf("chunks = %q, %q", first, second[:min(len(second), 40)])
	}

	transport.post = json.RawMessage(`{"success":false,"error":"network unavailable"}`)
	transport.posts = nil
	result = channel.Send(t.Context(), "chat", "hello")
	if result.Success || !result.Retryable || !strings.Contains(result.Error, "network unavailable") {
		t.Fatalf("failed Send = %#v", result)
	}
}

func TestSendCancellationWinsWhenTransportReturnsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	transport := &fakeTransport{onPost: func(string) { cancel() }}
	result := newTestChannel(t, transport, Options{}).Send(ctx, "chat", "hello")
	if result.Success || result.Error != context.Canceled.Error() || result.Retryable {
		t.Fatalf("Send = %#v", result)
	}
}

func TestSendMediaConfinesStagesAndCapsFiles(t *testing.T) {
	root := t.TempDir()
	inbound := t.TempDir()
	image := filepath.Join(root, "image.png")
	if err := os.WriteFile(image, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{}
	channel := New(transport, filepath.Join(t.TempDir(), "session"), Options{
		InboundMediaDir: inbound, OutboundMediaRoot: root, MaxMediaBytes: 5,
	})
	result := channel.SendMedia(t.Context(), "chat", Media{Path: "image.png", Type: "image", Caption: "caption"})
	if !result.Success {
		t.Fatalf("SendMedia = %#v", result)
	}
	transport.mu.Lock()
	payload := transport.posts[0].payload.(map[string]any)
	transport.mu.Unlock()
	staged := payload["filePath"].(string)
	if filepath.Dir(staged) != inbound || payload["caption"] != "*deepagents bot*\ncaption" {
		t.Fatalf("payload = %#v", payload)
	}
	data, err := os.ReadFile(staged)
	if err != nil || string(data) != "image" {
		t.Fatalf("staged = %q, %v", data, err)
	}
	if info, err := os.Stat(staged); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged permissions = %v, %v", info, err)
	}

	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed := channel.SendMedia(t.Context(), "chat", Media{Path: outside, Type: "image"}); failed.Success || !strings.Contains(failed.Error, "escapes outbound root") {
		t.Fatalf("outside media = %#v", failed)
	}
	large := filepath.Join(root, "large.png")
	if err := os.WriteFile(large, []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed := channel.SendMedia(t.Context(), "chat", Media{Path: large, Type: "image"}); failed.Success || !strings.Contains(failed.Error, "exceeds 5 bytes") {
		t.Fatalf("large media = %#v", failed)
	}
}

func TestLifecycleCreatesPrivateDirsAndReportsPairing(t *testing.T) {
	transport := &fakeTransport{health: json.RawMessage(`{"status":"qr_pending"}`)}
	channel := newTestChannel(t, transport, Options{PollInterval: time.Hour, HealthInterval: time.Hour})
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for channel.Status().Detail != "qr_pending" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := channel.Status(); status.Detail != "qr_pending" || status.Connected {
		t.Fatalf("status = %#v", status)
	}
	for _, directory := range []string{channel.session, channel.options.InboundMediaDir} {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory %s = %v, %v", directory, info, err)
		}
	}
	if err := channel.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if channel.Status().Detail != "disconnected" {
		t.Fatalf("stopped status = %#v", channel.Status())
	}
}

func TestPollRejectsAdversarialBridgePayloadsWithoutDispatch(t *testing.T) {
	tests := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`[null]`),
		json.RawMessage(`[{"text":"missing chat"}]`),
		json.RawMessage(`[{"chatId":"chat","text":"` + strings.Repeat("x", 20) + `"}]`),
		json.RawMessage(`[{"chatId":"chat","mediaPaths":["a","b"]}]`),
	}
	for index, payload := range tests {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			transport := &fakeTransport{messages: payload}
			channel := newTestChannel(t, transport, Options{MaxTextBytes: 10, MaxMediaPaths: 1})
			called := false
			installHandler(channel, func(context.Context, datalon.Message) error { called = true; return nil })
			if err := channel.pollOnce(t.Context()); err == nil {
				t.Fatal("adversarial payload passed")
			}
			if called {
				t.Fatal("invalid batch partially dispatched")
			}
		})
	}
}

func TestCallerTransportCannotBypassChannelPayloadBound(t *testing.T) {
	transport := &fakeTransport{messages: json.RawMessage(`[{"chatId":"chat","body":"allowed by field bounds","fromSelf":true}]`)}
	channel := newTestChannel(t, transport, Options{MaxBridgeBytes: 16})
	called := false
	installHandler(channel, func(context.Context, datalon.Message) error { called = true; return nil })
	if err := channel.pollOnce(t.Context()); !errors.Is(err, ErrBridgePayloadTooLarge) {
		t.Fatalf("poll error = %v", err)
	}
	if called {
		t.Fatal("oversized transport payload was dispatched")
	}

	transport.post = json.RawMessage(`{"success":true,"message_id":"too-large"}`)
	result := channel.Send(t.Context(), "chat", "hello")
	if result.Success || result.Error != ErrBridgePayloadTooLarge.Error() {
		t.Fatalf("Send = %#v", result)
	}
}

func TestSendSanitizesAndBoundsBridgeErrors(t *testing.T) {
	transport := &fakeTransport{post: json.RawMessage(`{"success":false,"error":"bad\u0000secret"}`)}
	channel := newTestChannel(t, transport, Options{MaxErrorBytes: 8})
	result := channel.Send(t.Context(), "chat", "hello")
	if result.Success || strings.ContainsRune(result.Error, '\x00') || len(result.Error) > 11 {
		t.Fatalf("Send = %#v", result)
	}
}

func TestOptionsClampMediaAndRequireOpenAcknowledgement(t *testing.T) {
	channel := newTestChannel(t, &fakeTransport{}, Options{MaxMediaBytes: MaxWhatsAppMediaBytes + 1})
	if channel.options.MaxMediaBytes != MaxWhatsAppMediaBytes || channel.options.BotHeader != DefaultBotHeader || channel.options.PollInterval != time.Second {
		t.Fatalf("defaults = %#v", channel.options)
	}
	open := newTestChannel(t, &fakeTransport{}, Options{Exposure: Exposure{Mode: ExposureOpen, OpenAcknowledgement: OpenAcknowledgement}})
	if open.exposure.mode != ExposureOpen {
		t.Fatalf("open policy = %#v", open.exposure)
	}
}

func TestConstructorsRejectUnsafeStaticInputs(t *testing.T) {
	tests := []func(){
		func() { New(nil, "/session", Options{}) },
		func() {
			var transport *fakeTransport
			New(transport, "/session", Options{})
		},
		func() { New(&fakeTransport{}, "", Options{}) },
		func() { New(&fakeTransport{}, "/session", Options{MaxMediaBytes: -1}) },
		func() { New(&fakeTransport{}, "/session", Options{Exposure: Exposure{Mode: ExposureOpen}}) },
		func() { NewHTTPTransport("https://127.0.0.1:3000", "token", HTTPOptions{}) },
		func() { NewHTTPTransport("http://example.com:3000", "token", HTTPOptions{}) },
		func() { NewHTTPTransport("http://127.0.0.1:3000/path", "token", HTTPOptions{}) },
		func() { NewHTTPTransport("http://127.0.0.1:3000", "", HTTPOptions{}) },
	}
	for index, call := range tests {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("constructor did not panic")
				}
			}()
			call()
		})
	}
}

func TestHTTPTransportIsAuthenticatedLoopbackOnlyAndBounded(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		if request.URL.Path == "/large" {
			_, _ = response.Write([]byte(`{"value":"` + strings.Repeat("x", 100) + `"}`))
			return
		}
		if request.URL.Path == "/failed" {
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = response.Write([]byte(`{"error":"denied"}`))
			return
		}
		_, _ = response.Write([]byte(`{"status":"connected"}`))
	}))
	defer server.Close()
	transport := NewHTTPTransport(server.URL, "test-token", HTTPOptions{MaxResponseBytes: 32})
	payload, err := transport.Get(t.Context(), "/health")
	if err != nil || string(payload) != `{"status":"connected"}` || authorization != "Bearer test-token" {
		t.Fatalf("Get = %s, %v; auth %q", payload, err, authorization)
	}
	if _, err := transport.Get(t.Context(), "/large"); !errors.Is(err, ErrBridgePayloadTooLarge) {
		t.Fatalf("large error = %v", err)
	}
	if _, err := transport.Get(t.Context(), "/failed"); err == nil || !strings.Contains(err.Error(), "401: denied") {
		t.Fatalf("HTTP error = %v", err)
	}
	if _, err := transport.Get(t.Context(), "//evil"); err == nil {
		t.Fatal("invalid endpoint passed")
	}
}

func TestConcurrentStatusSendAndStopAreRaceSafe(t *testing.T) {
	transport := &fakeTransport{}
	channel := newTestChannel(t, transport, Options{PollInterval: time.Hour, HealthInterval: time.Hour})
	if err := channel.Start(t.Context(), func(context.Context, datalon.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_ = channel.Status()
			_ = channel.Send(t.Context(), "chat", "hello")
		}()
	}
	wait.Wait()
	if err := channel.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}
