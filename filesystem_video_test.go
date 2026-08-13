package dago

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
	"github.com/semistrict/dago/davideo"
)

type boundedVideoBackend struct {
	dabackend.Backend
	reads        int
	bounded      int
	requestedMax int64
	result       dabackend.ReadResult
}

func (backend *boundedVideoBackend) Read(context.Context, string, int, int) (dabackend.ReadResult, error) {
	backend.reads++
	return dabackend.ReadResult{}, errors.New("unbounded read must not be used")
}

func (backend *boundedVideoBackend) ReadBinary(_ context.Context, _ string, maxBytes int64) (dabackend.ReadResult, error) {
	backend.bounded++
	backend.requestedMax = maxBytes
	return backend.result, nil
}

func TestReadFileExtractsVideoWindow(t *testing.T) {
	raw := []byte{0xff, 0x00, 0x01}
	memory, err := dabackend.NewMemory(map[string]dabackend.FileData{
		"/movie.mp4": {Content: base64.StdEncoding.EncodeToString(raw), Encoding: dabackend.EncodingBase64},
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotContent []byte
	var gotWindow davideo.Window
	extractor := davideo.ExtractorFunc(func(_ context.Context, content []byte, window davideo.Window) ([]damessage.ContentBlock, error) {
		gotContent = append([]byte(nil), content...)
		gotWindow = window
		return []damessage.ContentBlock{
			{Type: damessage.BlockText, Text: "Frame at t=00:00:12.000"},
			{Type: damessage.BlockImage, Data: testJPEGBytes("frame"), MIMEType: "image/jpeg"},
		}, nil
	})
	read := filesystemTool(t, memory, Filesystem{VideoExtractor: extractor, VideoSamplingRate: 2}, "read_file")
	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.mp4","offset":12,"limit":7}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(gotContent) != string(raw) {
		t.Fatalf("content = %v", gotContent)
	}
	wantWindow := (davideo.Window{OffsetSeconds: 12, DurationSeconds: 7, SamplingRate: 2})
	if gotWindow != wantWindow {
		t.Fatalf("window = %#v, want %#v", gotWindow, wantWindow)
	}
	if len(result.Content) != 4 || result.Content[0].Text != "Read video /movie.mp4: sampled 1 frame." || !strings.Contains(result.Content[1].Text, "[12.000s, 19.000s)") {
		t.Fatalf("result = %#v", result.Content)
	}
}

func TestReadFileUsesDefaultVideoWindow(t *testing.T) {
	memory := videoMemory(t, "/movie.webm", []byte{0xff, 0x01})
	var got davideo.Window
	read := filesystemTool(t, memory, Filesystem{VideoExtractor: davideo.ExtractorFunc(func(_ context.Context, _ []byte, window davideo.Window) ([]damessage.ContentBlock, error) {
		got = window
		return nil, nil
	})}, "read_file")
	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.webm"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if got != (davideo.Window{DurationSeconds: 100, SamplingRate: 0.5}) {
		t.Fatalf("window = %#v", got)
	}
	if result.Content[0].Text != "Read video /movie.webm: sampled 0 frames." || !strings.Contains(result.Content[1].Text, "Reading first 100s") {
		t.Fatalf("result = %#v", result.Content)
	}
}

func TestReadFileRejectsInvalidVideoLimitBeforeExtraction(t *testing.T) {
	memory := videoMemory(t, "/movie.mov", []byte{0xff, 0x01})
	called := false
	read := filesystemTool(t, memory, Filesystem{VideoExtractor: davideo.ExtractorFunc(func(context.Context, []byte, davideo.Window) ([]damessage.ContentBlock, error) {
		called = true
		return nil, nil
	})}, "read_file")
	_, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.mov","limit":0}`), datool.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "limit must be > 0") || called {
		t.Fatalf("error = %v, called = %v", err, called)
	}
}

func TestReadFileEnforcesVideoInputCap(t *testing.T) {
	memory := videoMemory(t, "/movie.avi", []byte{0xff, 0x01, 0x02})
	read := filesystemTool(t, memory, Filesystem{MaxVideoBytes: 2, VideoExtractor: davideo.ExtractorFunc(func(context.Context, []byte, davideo.Window) ([]damessage.ContentBlock, error) {
		t.Fatal("extractor called after input cap")
		return nil, nil
	})}, "read_file")
	_, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.avi"}`), datool.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "maximum input size of 2 bytes") {
		t.Fatalf("error = %v", err)
	}
}

func TestReadFileUsesBackendVideoGuardBeforePayloadAllocation(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := &boundedVideoBackend{Backend: memory, result: dabackend.ReadResult{Data: &dabackend.FileData{
		Content: base64.StdEncoding.EncodeToString([]byte{0xff}), Encoding: dabackend.EncodingBase64,
	}}}
	read := filesystemTool(t, backend, Filesystem{MaxVideoBytes: 7, VideoExtractor: davideo.ExtractorFunc(func(context.Context, []byte, davideo.Window) ([]damessage.ContentBlock, error) {
		return nil, nil
	})}, "read_file")
	if _, err := read.Execute(t.Context(), json.RawMessage(`{"file_path":"/movie.mp4"}`), datool.Runtime{}); err != nil {
		t.Fatal(err)
	}
	if backend.reads != 0 || backend.bounded != 1 || backend.requestedMax != 7 {
		t.Fatalf("video reads: unbounded=%d bounded=%d max=%d", backend.reads, backend.bounded, backend.requestedMax)
	}
}

func TestReadFilePreservesVideoWindowOnExtractorError(t *testing.T) {
	memory := videoMemory(t, "/movie.mkv", []byte{0xff, 0x01})
	read := filesystemTool(t, memory, Filesystem{VideoExtractor: davideo.ExtractorFunc(func(context.Context, []byte, davideo.Window) ([]damessage.ContentBlock, error) {
		return nil, errors.New("decoder unavailable")
	})}, "read_file")
	_, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.mkv","offset":3,"limit":4}`), datool.Runtime{})
	if err == nil || !strings.Contains(err.Error(), "decoder unavailable") || !strings.Contains(err.Error(), "[3.000s, 7.000s)") {
		t.Fatalf("error = %v", err)
	}
}

func TestReadFileLeavesVideoOpaqueWithoutExtractor(t *testing.T) {
	raw := []byte{0xff, 0x01}
	memory := videoMemory(t, "/movie.mp4", raw)
	read := filesystemTool(t, memory, Filesystem{}, "read_file")
	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.mp4"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 2 || result.Content[1].Type != damessage.BlockVideo || string(result.Content[1].Data) != string(raw) {
		t.Fatalf("result = %#v", result.Content)
	}

	memory = videoMemory(t, "/movie.mkv", raw)
	read = filesystemTool(t, memory, Filesystem{}, "read_file")
	result, err = read.Execute(context.Background(), json.RawMessage(`{"file_path":"/movie.mkv"}`), datool.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[1].Type != damessage.BlockFile {
		t.Fatalf("mkv block type = %q", result.Content[1].Type)
	}
}

func TestReadFileAdvertisesVideoOnlyWhenConfigured(t *testing.T) {
	memory, err := dabackend.NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	plain := filesystemTool(t, memory, Filesystem{}, "read_file").Definition()
	video := filesystemTool(t, memory, Filesystem{VideoExtractor: davideo.ExtractorFunc(func(context.Context, []byte, davideo.Window) ([]damessage.ContentBlock, error) {
		return nil, nil
	})}, "read_file").Definition()
	if strings.Contains(plain.Description, "videos") || strings.Contains(string(plain.InputSchema), "For videos") {
		t.Fatalf("plain definition advertises video: %#v", plain)
	}
	if !strings.Contains(video.Description, "videos") || !strings.Contains(string(video.InputSchema), "For videos") {
		t.Fatalf("video definition omits video: %#v", video)
	}
}

func testJPEGBytes(payload string) []byte {
	result := []byte{0xff, 0xd8}
	result = append(result, payload...)
	return append(result, 0xff, 0xd9)
}

func videoMemory(t *testing.T, name string, raw []byte) *dabackend.Memory {
	t.Helper()
	memory, err := dabackend.NewMemory(map[string]dabackend.FileData{
		name: {Content: base64.StdEncoding.EncodeToString(raw), Encoding: dabackend.EncodingBase64},
	})
	if err != nil {
		t.Fatal(err)
	}
	return memory
}

func filesystemTool(t *testing.T, backend dabackend.Backend, options Filesystem, name string) datool.Tool {
	t.Helper()
	middleware := mustFilesystem(backend, options)
	for _, candidate := range middleware.Tools {
		if candidate.Definition().Name == name {
			return candidate
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func mustFilesystem(backend dabackend.Backend, options Filesystem, approvalOverrides ...[]dagent.ApprovalRule) dagent.Middleware {
	var rules []dagent.ApprovalRule
	if len(approvalOverrides) > 0 {
		rules = approvalOverrides[0]
	}
	middleware, err := newFilesystem(backend, options, rules)
	if err != nil {
		panic(err)
	}
	return middleware
}
