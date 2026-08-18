// Package speech provides opt-in inbound voice transcription for datalon channels.
package speech

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/dago/datalon"
)

const DefaultLocalModel = "nvidia/parakeet-tdt-0.6b-v3"

var (
	ErrInvalidMedia       = errors.New("voice transcription media is invalid")
	ErrTranscriptionBound = errors.New("voice transcription limit exceeded")
)

// Transcriber turns a local media path from a channel message into text.
type Transcriber interface {
	Transcribe(context.Context, datalon.Message) (string, error)
}

// Config is the environment-selected transcription mode.
type Config struct {
	Enabled bool
	Local   bool
	Model   string
	Device  string
}

// ConfigFromEnv parses the current Talon and legacy speech environment keys. A
// nil map reads the process environment. Invalid boolean values return an error.
func ConfigFromEnv(env map[string]string) (Config, error) {
	if env == nil {
		env = environment()
	}
	raw := first(env, "DEEPAGENTS_TALON_VOICE_TRANSCRIPTION_ENABLED", "SPEECH_ENABLED")
	if raw == "" {
		return Config{}, nil
	}
	enabled, ok := parseBool(raw)
	if !ok {
		return Config{}, fmt.Errorf("voice transcription enabled must be true or false")
	}
	if !enabled {
		return Config{}, nil
	}
	model := first(env, "DEEPAGENTS_TALON_VOICE_TRANSCRIPTION_MODEL")
	local := model == "" || model == DefaultLocalModel || strings.HasPrefix(model, "nvidia/parakeet")
	if model == "" {
		model = DefaultLocalModel
	}
	device := first(env, "DEEPAGENTS_TALON_VOICE_TRANSCRIPTION_DEVICE", "SPEECH_DEVICE")
	if device == "" {
		device = "cpu"
	}
	if len(model) > 512 || len(device) > 64 || strings.ContainsAny(model+device, "\x00\r\n") {
		return Config{}, fmt.Errorf("voice transcription model or device is invalid")
	}
	return Config{Enabled: true, Local: local, Model: model, Device: device}, nil
}

// TranscribeMessage appends a successful transcript without mutating the input.
// Non-voice messages and empty transcripts pass through unchanged.
func TranscribeMessage(ctx context.Context, transcriber Transcriber, message datalon.Message, maxTranscriptBytes int) (datalon.Message, error) {
	if nilValue(transcriber) || !eligible(message) {
		return message, nil
	}
	if maxTranscriptBytes <= 0 {
		maxTranscriptBytes = 64 << 10
	}
	if maxTranscriptBytes > 1<<20 {
		return message, ErrTranscriptionBound
	}
	text, err := transcriber.Transcribe(ctx, message)
	if err != nil {
		return message, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return message, nil
	}
	if len(text) > maxTranscriptBytes || !utf8.ValidString(text) {
		return message, ErrTranscriptionBound
	}
	updated := message
	updated.Metadata = cloneMetadata(message.Metadata)
	updated.Metadata["voice_transcribed"] = true
	if strings.TrimSpace(message.Text) == "" {
		updated.Text = text
	} else {
		updated.Text = message.Text + "\n\n" + text
	}
	return updated, nil
}

func eligible(message datalon.Message) bool {
	if _, ok := pathValue(message.Metadata["voice_path"]); ok {
		return true
	}
	mediaType, _ := message.Metadata["media_type"].(string)
	return mediaType == "voice" || mediaType == "video"
}

func mediaPath(message datalon.Message) (string, error) {
	if path, ok := pathValue(message.Metadata["voice_path"]); ok {
		return path, nil
	}
	if path, ok := pathValue(message.Metadata["media_path"]); ok {
		return path, nil
	}
	return "", ErrInvalidMedia
}

func pathValue(value any) (string, bool) {
	path, ok := value.(string)
	return path, ok && path != "" && len(path) <= 4096 && !strings.ContainsRune(path, 0)
}

func validateMedia(path string, maxBytes int64) error {
	file, err := openMedia(path, maxBytes)
	if err != nil {
		return err
	}
	return file.Close()
}

func readMedia(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	file, err := openMedia(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data := make([]byte, 0, min(maxBytes, 64<<10))
	buffer := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := file.Read(buffer)
		if int64(len(data)+count) > maxBytes {
			return nil, ErrTranscriptionBound
		}
		data = append(data, buffer[:count]...)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("%w: read media: %v", ErrInvalidMedia, readErr)
		}
	}
	return data, nil
}

func copyMedia(ctx context.Context, path string, maxBytes int64) (string, error) {
	data, err := readMedia(ctx, path, maxBytes)
	if err != nil {
		return "", err
	}
	extension := filepath.Ext(path)
	if len(extension) > 16 {
		extension = ""
	}
	temporary, err := os.CreateTemp("", "datalon-voice-input-*"+extension)
	if err != nil {
		return "", fmt.Errorf("copy voice media: %w", err)
	}
	name := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		os.Remove(name)
		return "", fmt.Errorf("secure voice media copy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		temporary.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		os.Remove(name)
		return "", fmt.Errorf("write voice media copy: %w", err)
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("close voice media copy: %w", err)
	}
	return name, nil
}

func openMedia(path string, maxBytes int64) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		if err == nil {
			err = ErrInvalidMedia
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidMedia, err)
	}
	directory, base := filepath.Split(path)
	if directory == "" {
		directory = "."
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: open media root: %v", ErrInvalidMedia, err)
	}
	file, err := root.Open(base)
	root.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: open media: %v", ErrInvalidMedia, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > maxBytes {
		file.Close()
		return nil, ErrInvalidMedia
	}
	return file, nil
}

func cloneMetadata(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+1)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func environment() map[string]string {
	result := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}

func first(env map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(env[key]); value != "" {
			return value
		}
	}
	return ""
}

func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true, true
	case "0", "false", "no":
		return false, true
	default:
		return false, false
	}
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
