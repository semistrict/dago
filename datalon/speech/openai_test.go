package speech

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/datalon"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOpenAITranscriberUsesAuthenticatedBoundedMultipartRequest(t *testing.T) {
	t.Parallel()
	media := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(media, []byte("audio-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called++
		if request.URL.String() != "https://speech.example/v1/audio/transcriptions" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request = %s, auth = %q", request.URL, request.Header.Get("Authorization"))
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("model") != "gpt-4o-transcribe" {
			t.Fatalf("model = %q", request.FormValue("model"))
		}
		file, _, err := request.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		if string(data) != "audio-data" {
			t.Fatalf("media = %q", data)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"text":" transcribed "}`)), Header: make(http.Header)}, nil
	})}
	transcriber := NewOpenAI(client, "secret", "gpt-4o-transcribe", OpenAIOptions{BaseURL: "https://speech.example"})
	text, err := transcriber.Transcribe(t.Context(), datalon.Message{Metadata: map[string]any{"media_type": "voice", "media_path": media}})
	if err != nil || text != "transcribed" || called != 1 {
		t.Fatalf("Transcribe = %q, %v, calls %d", text, err, called)
	}
}

func TestOpenAITranscriberBoundsInputResponseAndContainsKey(t *testing.T) {
	t.Parallel()
	media := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(media, []byte("large"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 100))), Header: make(http.Header)}, nil
	})}
	transcriber := NewOpenAI(client, "do-not-leak", "model", OpenAIOptions{BaseURL: "https://speech.example", MaxInputBytes: 4, MaxResponseBytes: 8})
	_, err := transcriber.Transcribe(t.Context(), datalon.Message{Metadata: map[string]any{"voice_path": media}})
	if !errors.Is(err, ErrInvalidMedia) || calls != 0 {
		t.Fatalf("input bound = %v, calls %d", err, calls)
	}
	transcriber = NewOpenAI(client, "do-not-leak", "model", OpenAIOptions{BaseURL: "https://speech.example", MaxResponseBytes: 8})
	_, err = transcriber.Transcribe(t.Context(), datalon.Message{Metadata: map[string]any{"voice_path": media}})
	if !errors.Is(err, ErrTranscriptionBound) || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("response error = %v", err)
	}
}

func TestOpenAITranscriberCancellationAndConstructorInvariants(t *testing.T) {
	t.Parallel()
	media := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(media, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	transcriber := NewOpenAI(client, "key", "model", OpenAIOptions{BaseURL: "https://speech.example"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transcriber.Transcribe(ctx, datalon.Message{Metadata: map[string]any{"voice_path": media}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	for name, call := range map[string]func(){
		"client": func() { NewOpenAI(nil, "key", "model", OpenAIOptions{}) },
		"key":    func() { NewOpenAI(&http.Client{}, "", "model", OpenAIOptions{}) },
		"model":  func() { NewOpenAI(&http.Client{}, "key", "", OpenAIOptions{}) },
		"base":   func() { NewOpenAI(&http.Client{}, "key", "model", OpenAIOptions{BaseURL: "http://example.com"}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewOpenAI did not panic")
				}
			}()
			call()
		})
	}
}
