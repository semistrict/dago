package speech

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/dago/datalon"
)

// OpenAIOptions contains optional endpoint and finite resource controls. Its
// zero value uses the official API, a two-minute timeout, a 25 MiB media cap,
// and a 1 MiB response cap.
type OpenAIOptions struct {
	BaseURL          string
	Timeout          time.Duration
	MaxInputBytes    int64
	MaxResponseBytes int64
}

// OpenAITranscriber calls an OpenAI-compatible audio transcription endpoint.
type OpenAITranscriber struct {
	client  *http.Client
	apiKey  string
	model   string
	options OpenAIOptions
}

// NewOpenAI constructs a remote transcriber. Client, API key, and model are
// mandatory positional dependencies; invalid static values panic.
func NewOpenAI(client *http.Client, apiKey, model string, options OpenAIOptions) *OpenAITranscriber {
	if client == nil {
		panic("datalon/speech: nil HTTP client")
	}
	if apiKey == "" || len(apiKey) > 4096 || strings.ContainsAny(apiKey, "\r\n") {
		panic("datalon/speech: invalid API key")
	}
	if model == "" || len(model) > 512 || strings.ContainsAny(model, "\x00\r\n") {
		panic("datalon/speech: invalid transcription model")
	}
	if options.BaseURL == "" {
		options.BaseURL = "https://api.openai.com"
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(options.BaseURL) > 2048 {
		panic("datalon/speech: API base must be an HTTPS origin or trusted base path")
	}
	if options.Timeout < 0 || options.MaxInputBytes < 0 || options.MaxResponseBytes < 0 {
		panic("datalon/speech: remote limits cannot be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 2 * time.Minute
	}
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = 25 << 20
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = 1 << 20
	}
	if options.Timeout > 30*time.Minute || options.MaxInputBytes > 1<<30 || options.MaxResponseBytes > 8<<20 {
		panic("datalon/speech: remote option exceeds its finite bound")
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &OpenAITranscriber{client: &clone, apiKey: apiKey, model: model, options: options}
}

func (transcriber *OpenAITranscriber) Transcribe(ctx context.Context, message datalon.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := mediaPath(message)
	if err != nil {
		return "", err
	}
	media, err := readMedia(ctx, path, transcriber.options.MaxInputBytes)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", fmt.Errorf("create transcription form: %w", err)
	}
	if _, err := part.Write(media); err != nil {
		return "", fmt.Errorf("write transcription form: %w", err)
	}
	if err := writer.WriteField("model", transcriber.model); err != nil {
		return "", fmt.Errorf("write transcription model: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close transcription form: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, transcriber.options.Timeout)
	defer cancel()
	endpoint := strings.TrimRight(transcriber.options.BaseURL, "/") + "/v1/audio/transcriptions"
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, &body)
	if err != nil {
		return "", fmt.Errorf("create transcription request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+transcriber.apiKey)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := transcriber.client.Do(request)
	if err != nil {
		if callCtx.Err() != nil {
			return "", callCtx.Err()
		}
		return "", fmt.Errorf("send transcription request: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, transcriber.options.MaxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read transcription response: %w", err)
	}
	if int64(len(data)) > transcriber.options.MaxResponseBytes {
		return "", ErrTranscriptionBound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("transcription endpoint returned HTTP %d: %s", response.StatusCode, bounded(string(data), 4096))
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return "", fmt.Errorf("decode transcription response: %w", err)
	}
	return strings.TrimSpace(decoded.Text), nil
}
