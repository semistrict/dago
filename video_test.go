package dago

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/damessage"
)

func TestValidateVideoWindow(t *testing.T) {
	valid := VideoWindow{OffsetSeconds: 1.25, DurationSeconds: 10, SamplingRate: 0.5}
	if err := validateVideoWindow(valid); err != nil {
		t.Fatalf("valid window: %v", err)
	}
	tests := []VideoWindow{
		{OffsetSeconds: -1, DurationSeconds: 1, SamplingRate: 1},
		{OffsetSeconds: math.NaN(), DurationSeconds: 1, SamplingRate: 1},
		{OffsetSeconds: math.Inf(1), DurationSeconds: 1, SamplingRate: 1},
		{DurationSeconds: 0, SamplingRate: 1},
		{DurationSeconds: math.NaN(), SamplingRate: 1},
		{DurationSeconds: 1, SamplingRate: 0},
		{DurationSeconds: 1, SamplingRate: math.Inf(-1)},
	}
	for index, window := range tests {
		if err := validateVideoWindow(window); err == nil {
			t.Errorf("invalid window %d accepted: %#v", index, window)
		}
	}
}

func TestFormatVideoTimestamp(t *testing.T) {
	tests := map[float64]string{
		-1:          "00:00:00.000",
		0:           "00:00:00.000",
		1.234:       "00:00:01.234",
		61.001:      "00:01:01.001",
		3661.9996:   "01:01:02.000",
		360000.0004: "100:00:00.000",
	}
	for input, want := range tests {
		if got := formatVideoTimestamp(input); got != want {
			t.Errorf("formatVideoTimestamp(%v) = %q, want %q", input, got, want)
		}
	}
}

func TestSplitJPEGStream(t *testing.T) {
	input := append([]byte("noise"), jpegBytes("one")...)
	input = append(input, jpegBytes("two")...)
	input = append(input, 0xff, 0xd8, 'x')
	frames := splitJPEGStream(input)
	if len(frames) != 2 || string(frames[0]) != string(jpegBytes("one")) || string(frames[1]) != string(jpegBytes("two")) {
		t.Fatalf("frames = %#v", frames)
	}
}

func TestFFmpegVideoExtractorInterleavesAndCapsFrames(t *testing.T) {
	executable := writeVideoFixture(t, "#!/bin/sh\nprintf '\\377\\330one\\377\\331\\377\\330two\\377\\331\\377\\330three\\377\\331'\n")
	extractor := NewFFmpegVideoExtractor(FFmpegVideoOptions{Executable: executable, MaxFrames: 2, MaxEmittedBytes: 1024})
	blocks, err := extractor.Extract(context.Background(), []byte("source"), VideoWindow{OffsetSeconds: 3, DurationSeconds: 8, SamplingRate: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 5 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if blocks[0].Text != "Frame at t=00:00:03.000" || blocks[2].Text != "Frame at t=00:00:05.000" {
		t.Fatalf("timestamps = %q, %q", blocks[0].Text, blocks[2].Text)
	}
	if blocks[1].Type != damessage.BlockImage || string(blocks[1].Data) != string(jpegBytes("one")) || blocks[1].MIMEType != "image/jpeg" {
		t.Fatalf("first image = %#v", blocks[1])
	}
	if blocks[3].Type != damessage.BlockImage || string(blocks[3].Data) != string(jpegBytes("two")) {
		t.Fatalf("second image = %#v", blocks[3])
	}
	if !strings.Contains(blocks[4].Text, "Coverage truncated") || !strings.Contains(blocks[4].Text, "offset=5.000") {
		t.Fatalf("truncation hint = %q", blocks[4].Text)
	}
}

func TestFFmpegVideoExtractorReportsDecoderFailure(t *testing.T) {
	executable := writeVideoFixture(t, "#!/bin/sh\necho 'invalid media' >&2\nexit 7\n")
	extractor := NewFFmpegVideoExtractor(FFmpegVideoOptions{Executable: executable})
	_, err := extractor.Extract(context.Background(), []byte("source"), VideoWindow{DurationSeconds: 1, SamplingRate: 1})
	if err == nil || !strings.Contains(err.Error(), "invalid media") {
		t.Fatalf("error = %v", err)
	}
}

func TestFFmpegVideoExtractorHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	extractor := NewFFmpegVideoExtractor(FFmpegVideoOptions{Executable: "does-not-matter"})
	_, err := extractor.Extract(ctx, []byte("source"), VideoWindow{DurationSeconds: 1, SamplingRate: 1})
	if !errors.Is(err, context.Canceled) && (err == nil || !strings.Contains(err.Error(), "context canceled")) {
		t.Fatalf("error = %v", err)
	}
}

func TestFFmpegVideoExtractorEnforcesOutputBudget(t *testing.T) {
	executable := writeVideoFixture(t, "#!/bin/sh\nprintf '\\377\\330a-very-large-frame\\377\\331'\n")
	extractor := NewFFmpegVideoExtractor(FFmpegVideoOptions{Executable: executable, MaxEmittedBytes: 8, DecodeTimeout: time.Second})
	_, err := extractor.Extract(context.Background(), nil, VideoWindow{DurationSeconds: 1, SamplingRate: 1})
	if err == nil || !strings.Contains(err.Error(), "safety budget") {
		t.Fatalf("error = %v", err)
	}
}

func jpegBytes(payload string) []byte {
	result := []byte{0xff, 0xd8}
	result = append(result, payload...)
	return append(result, 0xff, 0xd9)
}

func writeVideoFixture(t *testing.T, content string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "video-fixture")
	if err := os.WriteFile(name, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return name
}
