package speech

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/datalon"
)

const pythonTranscriptionScript = `
import sys
from transformers import pipeline
result = pipeline("automatic-speech-recognition", model=sys.argv[1], device=sys.argv[2])(sys.argv[3])
if isinstance(result, dict):
    text = result.get("text", "")
else:
    text = getattr(result, "text", result)
sys.stdout.write(str(text).strip())
`

// Runner executes the fixed ffmpeg and Python argv used by LocalTranscriber.
// Implementations must honor context cancellation and output limits.
type Runner interface {
	Run(context.Context, string, []string, int) ([]byte, []byte, error)
}

// LocalOptions configures local Parakeet inference. Its zero value uses the
// pinned model, CPU, ffmpeg, python3, a two-minute timeout, a 64 MiB input cap,
// a 512 MiB WAV cap, and a 1 MiB process-output cap.
type LocalOptions struct {
	Model          string
	Device         string
	FFmpeg         string
	Python         string
	Timeout        time.Duration
	MaxInputBytes  int64
	MaxWAVBytes    int64
	MaxOutputBytes int
	Runner         Runner
}

// LocalTranscriber converts media to 16 kHz mono WAV and invokes a local
// Transformers automatic-speech-recognition pipeline.
type LocalTranscriber struct {
	options LocalOptions
	mu      sync.Mutex
}

// NewLocal constructs the local default transcriber without doing I/O.
func NewLocal(options LocalOptions) *LocalTranscriber {
	if options.Timeout < 0 || options.MaxInputBytes < 0 || options.MaxWAVBytes < 0 || options.MaxOutputBytes < 0 {
		panic("datalon/speech: local limits cannot be negative")
	}
	if options.Model == "" {
		options.Model = DefaultLocalModel
	}
	if options.Device == "" {
		options.Device = "cpu"
	}
	if options.FFmpeg == "" {
		options.FFmpeg = "ffmpeg"
	}
	if options.Python == "" {
		options.Python = "python3"
	}
	if options.Timeout == 0 {
		options.Timeout = 2 * time.Minute
	}
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = 64 << 20
	}
	if options.MaxWAVBytes == 0 {
		options.MaxWAVBytes = 512 << 20
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = 1 << 20
	}
	if nilValue(options.Runner) {
		options.Runner = execRunner{}
	}
	if len(options.Model) > 512 || len(options.Device) > 64 || len(options.FFmpeg) > 4096 || len(options.Python) > 4096 ||
		strings.ContainsAny(options.Model+options.Device+options.FFmpeg+options.Python, "\x00\r\n") ||
		options.Timeout > 30*time.Minute || options.MaxInputBytes > 1<<30 || options.MaxWAVBytes > 2<<30 || options.MaxOutputBytes > 8<<20 {
		panic("datalon/speech: local option exceeds its finite bound")
	}
	return &LocalTranscriber{options: options}
}

func (transcriber *LocalTranscriber) Transcribe(ctx context.Context, message datalon.Message) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := mediaPath(message)
	if err != nil {
		return "", err
	}
	inputPath, err := copyMedia(ctx, path, transcriber.options.MaxInputBytes)
	if err != nil {
		return "", err
	}
	defer os.Remove(inputPath)
	callCtx, cancel := context.WithTimeout(ctx, transcriber.options.Timeout)
	defer cancel()
	temporary, err := os.CreateTemp("", "datalon-voice-*.wav")
	if err != nil {
		return "", fmt.Errorf("create voice WAV: %w", err)
	}
	wavPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		os.Remove(wavPath)
		return "", fmt.Errorf("secure voice WAV: %w", err)
	}
	if err := temporary.Close(); err != nil {
		os.Remove(wavPath)
		return "", fmt.Errorf("close voice WAV: %w", err)
	}
	defer os.Remove(wavPath)
	_, stderr, err := transcriber.options.Runner.Run(callCtx, transcriber.options.FFmpeg, []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", inputPath,
		"-ar", "16000", "-ac", "1", "-f", "wav", wavPath,
	}, transcriber.options.MaxOutputBytes)
	if err != nil {
		return "", fmt.Errorf("convert voice media: %w: %s", err, bounded(strings.TrimSpace(string(stderr)), 4096))
	}
	if err := validateMedia(wavPath, transcriber.options.MaxWAVBytes); err != nil {
		return "", err
	}
	transcriber.mu.Lock()
	defer transcriber.mu.Unlock()
	stdout, stderr, err := transcriber.options.Runner.Run(callCtx, transcriber.options.Python, []string{
		"-c", pythonTranscriptionScript, transcriber.options.Model, transcriber.options.Device, wavPath,
	}, transcriber.options.MaxOutputBytes)
	if err != nil {
		return "", fmt.Errorf("run local voice model: %w: %s", err, bounded(strings.TrimSpace(string(stderr)), 4096))
	}
	return strings.TrimSpace(string(stdout)), nil
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, arguments []string, limit int) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		return stdout.Bytes(), stderr.Bytes(), ErrTranscriptionBound
	}
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	if buffer.exceeded {
		return len(value), ErrTranscriptionBound
	}
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return len(value), ErrTranscriptionBound
	}
	if len(value) > remaining {
		_, _ = buffer.Buffer.Write(value[:remaining])
		buffer.exceeded = true
		return len(value), ErrTranscriptionBound
	}
	return buffer.Buffer.Write(value)
}
