// Package davideo defines video extraction contracts and an optional FFmpeg adapter.
package davideo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/dago/damessage"
)

const (
	DefaultMaxVideoInputBytes   = 1024 * 1024 * 1024
	DefaultMaxVideoFrames       = 64
	DefaultMaxVideoEmittedBytes = 4 * 1024 * 1024
	DefaultVideoDecodeTimeout   = 10 * time.Second
	DefaultVideoSamplingRate    = 0.5
)

// Window selects a contiguous source window and output sampling rate.
type Window struct {
	OffsetSeconds   float64
	DurationSeconds float64
	SamplingRate    float64
}

// Extractor converts video bytes to interleaved timestamp and JPEG image
// blocks. Implementations must never return the original video payload.
type Extractor interface {
	Extract(context.Context, []byte, Window) ([]damessage.ContentBlock, error)
}

// ExtractorFunc adapts a function to Extractor.
type ExtractorFunc func(context.Context, []byte, Window) ([]damessage.ContentBlock, error)

func (function ExtractorFunc) Extract(ctx context.Context, content []byte, window Window) ([]damessage.ContentBlock, error) {
	return function(ctx, content, window)
}

// FFmpegOptions configures the optional FFmpeg-backed extractor.
type FFmpegOptions struct {
	Executable      string
	MaxFrames       int
	MaxEmittedBytes int
	DecodeTimeout   time.Duration
}

// FFmpegExtractor samples a bounded JPEG stream from an ffmpeg executable.
// Constructing it does not require ffmpeg to be installed; a missing executable
// is reported only when Extract is called.
type FFmpegExtractor struct {
	executable      string
	maxFrames       int
	maxEmittedBytes int
	decodeTimeout   time.Duration
}

func NewFFmpeg(options FFmpegOptions) *FFmpegExtractor {
	if options.Executable == "" {
		options.Executable = "ffmpeg"
	}
	if options.MaxFrames <= 0 {
		options.MaxFrames = DefaultMaxVideoFrames
	}
	if options.MaxEmittedBytes <= 0 {
		options.MaxEmittedBytes = DefaultMaxVideoEmittedBytes
	}
	if options.DecodeTimeout <= 0 {
		options.DecodeTimeout = DefaultVideoDecodeTimeout
	}
	return &FFmpegExtractor{
		executable: options.Executable, maxFrames: options.MaxFrames,
		maxEmittedBytes: options.MaxEmittedBytes, decodeTimeout: options.DecodeTimeout,
	}
}

func (extractor *FFmpegExtractor) Extract(ctx context.Context, content []byte, window Window) ([]damessage.ContentBlock, error) {
	if err := validateVideoWindow(window); err != nil {
		return nil, err
	}
	if extractor == nil {
		return nil, fmt.Errorf("video extractor is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, extractor.decodeTimeout)
	defer cancel()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-ss", formatVideoNumber(window.OffsetSeconds), "-i", "pipe:0",
		"-t", formatVideoNumber(window.DurationSeconds),
		"-vf", "fps=" + formatVideoNumber(window.SamplingRate) + ",scale=min(1920\\,iw):min(1080\\,ih):force_original_aspect_ratio=decrease",
		"-frames:v", strconv.Itoa(extractor.maxFrames), "-q:v", "3",
		"-f", "image2pipe", "-vcodec", "mjpeg", "pipe:1",
	}
	command := exec.CommandContext(deadlineCtx, extractor.executable, args...)
	command.Stdin = bytes.NewReader(content)
	stdout := &boundedBuffer{limit: extractor.maxEmittedBytes + 1}
	stderr := &boundedBuffer{limit: 64 << 10}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("video decoding exceeded the %.1fs safety budget", extractor.decodeTimeout.Seconds())
	}
	frames := splitJPEGStream(stdout.Bytes())
	truncated := stdout.exceeded || len(frames) >= extractor.maxFrames
	if len(frames) > extractor.maxFrames {
		frames = frames[:extractor.maxFrames]
	}
	if err != nil && !stdout.exceeded {
		detail := strings.TrimSpace(string(stderr.Bytes()))
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("failed to decode video frames: %s", detail)
	}
	if len(frames) == 0 {
		if stdout.exceeded {
			return nil, fmt.Errorf("video frame output exceeded the %d byte safety budget before emitting a frame", extractor.maxEmittedBytes)
		}
		return nil, fmt.Errorf("no frames decoded for window [%.3fs, %.3fs)", window.OffsetSeconds, window.OffsetSeconds+window.DurationSeconds)
	}
	blocks := make([]damessage.ContentBlock, 0, len(frames)*2+1)
	emittedBytes := 0
	lastSeconds := window.OffsetSeconds
	for index, jpeg := range frames {
		seconds := window.OffsetSeconds + float64(index)/window.SamplingRate
		text := "Frame at t=" + formatVideoTimestamp(seconds)
		encodedSize := base64.StdEncoding.EncodedLen(len(jpeg))
		if emittedBytes+len(text)+encodedSize > extractor.maxEmittedBytes {
			if index == 0 {
				return nil, fmt.Errorf("video frame output exceeded the %d byte safety budget before emitting a frame", extractor.maxEmittedBytes)
			}
			truncated = true
			break
		}
		blocks = append(blocks,
			damessage.ContentBlock{Type: damessage.BlockText, Text: text},
			damessage.ContentBlock{Type: damessage.BlockImage, Data: append([]byte(nil), jpeg...), MIMEType: "image/jpeg"},
		)
		emittedBytes += len(text) + encodedSize
		lastSeconds = seconds
	}
	if truncated {
		blocks = append(blocks, damessage.ContentBlock{Type: damessage.BlockText, Text: fmt.Sprintf(
			"Coverage truncated at t=%s: the output or frame cap was reached before the full window was decoded. Continue from offset=%.3f to see the remaining frames.",
			formatVideoTimestamp(lastSeconds), lastSeconds,
		)})
	}
	return blocks, nil
}

func validateVideoWindow(window Window) error {
	if math.IsNaN(window.OffsetSeconds) || math.IsInf(window.OffsetSeconds, 0) || window.OffsetSeconds < 0 {
		return fmt.Errorf("offset_seconds must be >= 0, got %v", window.OffsetSeconds)
	}
	if math.IsNaN(window.DurationSeconds) || math.IsInf(window.DurationSeconds, 0) || window.DurationSeconds <= 0 {
		return fmt.Errorf("duration_seconds must be > 0, got %v", window.DurationSeconds)
	}
	if math.IsNaN(window.SamplingRate) || math.IsInf(window.SamplingRate, 0) || window.SamplingRate <= 0 {
		return fmt.Errorf("sampling_rate must be > 0, got %v", window.SamplingRate)
	}
	return nil
}

func formatVideoNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func formatVideoTimestamp(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	totalMilliseconds := int64(math.Round(seconds * 1000))
	hours := totalMilliseconds / 3_600_000
	totalMilliseconds %= 3_600_000
	minutes := totalMilliseconds / 60_000
	totalMilliseconds %= 60_000
	wholeSeconds := totalMilliseconds / 1000
	milliseconds := totalMilliseconds % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, wholeSeconds, milliseconds)
}

func splitJPEGStream(value []byte) [][]byte {
	var result [][]byte
	for cursor := 0; cursor+1 < len(value); {
		start := bytes.Index(value[cursor:], []byte{0xff, 0xd8})
		if start < 0 {
			break
		}
		start += cursor
		end := bytes.Index(value[start+2:], []byte{0xff, 0xd9})
		if end < 0 {
			break
		}
		end += start + 4
		result = append(result, append([]byte(nil), value[start:end]...))
		cursor = end
	}
	return result
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.buffer.Write(value)
	return original, nil
}

func (buffer *boundedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }

var _ Extractor = (*FFmpegExtractor)(nil)
var _ Extractor = ExtractorFunc(nil)
