package dahousekeeping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTraceHandlerWritesRedactedBoundedJSONAndSnapshots(t *testing.T) {
	var output bytes.Buffer
	handler := NewTraceHandler(&output, TraceOptions{Capacity: 2})
	logger := slog.New(handler.WithAttrs([]slog.Attr{slog.String("component", "stream")}))
	logger.Debug("first", "authorization", "Bearer secret")
	logger.Info("second\nline", "request_id", "abc")
	logger.Error("third", "api-key", "top-secret", "unsupported", struct{ Secret string }{"value"})

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d trace lines, want 3: %q", len(lines), output.String())
	}
	var last TraceRecord
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Attributes["api-key"] != "[REDACTED]" || last.Attributes["unsupported"] != "[UNAVAILABLE]" {
		t.Fatalf("attributes were not safely projected: %+v", last.Attributes)
	}
	if strings.Contains(output.String(), "Bearer secret") || strings.Contains(output.String(), "top-secret") {
		t.Fatal("secret reached trace output")
	}
	if strings.Contains(output.String(), "second\\nline") {
		t.Fatal("record message retained a newline")
	}
	snapshot := handler.Snapshot(0)
	if snapshot.TotalEmitted != 3 || snapshot.NextIndex != 3 || snapshot.Dropped != 1 || len(snapshot.Records) != 2 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Records[0].Index != 1 || snapshot.Records[1].Index != 2 {
		t.Fatalf("snapshot is not chronological: %+v", snapshot.Records)
	}
	// A caller cannot mutate retained records through a snapshot.
	snapshot.Records[0].Attributes["request_id"] = "mutated"
	if got := handler.Snapshot(1).Records[0].Attributes["request_id"]; got != "abc" {
		t.Fatalf("snapshot aliased internal state: %q", got)
	}
}

func TestTraceHandlerHonorsLevelCancellationAndWriterErrors(t *testing.T) {
	var output bytes.Buffer
	handler := NewTraceHandler(&output, TraceOptions{MinimumLevel: new(slog.LevelWarn)})
	logger := slog.New(handler)
	logger.Info("hidden")
	logger.Warn("visible")
	if strings.Contains(output.String(), "hidden") || !strings.Contains(output.String(), "visible") {
		t.Fatalf("level filtering failed: %q", output.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := handler.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelError, "cancelled", 0))
	if err != context.Canceled {
		t.Fatalf("got %v, want cancellation", err)
	}
	failing := NewTraceHandler(errorWriter{}, TraceOptions{})
	err = failing.Handle(t.Context(), slog.NewRecord(time.Now(), slog.LevelInfo, "failure", 0))
	if err == nil || !strings.Contains(err.Error(), "write debug trace") {
		t.Fatalf("writer failure was not returned safely: %v", err)
	}
}

func TestOpenTraceFileIsPrivateAppendOnlyAndBounded(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(parent, "debug.jsonl")
	file, err := OpenTraceFile(path, TraceFileOptions{MaxBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTraceFile(path, TraceFileOptions{MaxBytes: 8}); !errors.Is(err, ErrTraceLocked) {
		t.Fatalf("concurrent writer: got %v, want trace lock", err)
	}
	if _, err := file.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("56789")); !errors.Is(err, ErrTraceLimit) {
		t.Fatalf("got %v, want trace limit", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) || info.Size() != 4 {
		t.Fatalf("unsafe trace file: mode=%o size=%d", info.Mode().Perm(), info.Size())
	}
	file, err = OpenTraceFile(path, TraceFileOptions{MaxBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("56")); err != nil {
		t.Fatal(err)
	}
	file.Close()
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "123456" {
		t.Fatalf("trace file did not append: %q, %v", content, err)
	}
}

func TestOpenTraceFileRejectsRelativeSharedAndSymlinkPaths(t *testing.T) {
	if _, err := OpenTraceFile("relative.log", TraceFileOptions{}); !errors.Is(err, ErrUnsafeTracePath) {
		t.Fatalf("relative path: got %v", err)
	}
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if _, err := OpenTraceFile(filepath.Join(shared, "debug.log"), TraceFileOptions{}); !errors.Is(err, ErrUnsafeTracePath) {
			t.Fatalf("shared parent: got %v", err)
		}
	}
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(private, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "debug.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := OpenTraceFile(link, TraceFileOptions{}); !errors.Is(err, ErrUnsafeTracePath) {
		t.Fatalf("symlink path: got %v", err)
	}
}

func TestResolveDebugConfigurationAndStart(t *testing.T) {
	values := map[string]string{
		DebugEnvironmentVariable:     "yes",
		DebugFileEnvironmentVariable: filepath.Join(t.TempDir(), "private", "debug.jsonl"),
		LogLevelEnvironmentVariable:  "warning",
	}
	configuration := ResolveDebugConfiguration(func(name string) (string, bool) {
		value, exists := values[name]
		return value, exists
	}, filepath.Join(t.TempDir(), "default.log"))
	if !configuration.Enabled || configuration.Level != slog.LevelWarn || configuration.Path != values[DebugFileEnvironmentVariable] {
		t.Fatalf("unexpected debug configuration: %+v", configuration)
	}
	logger, closer, err := configuration.Start(TraceFileOptions{}, TraceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("visible", "access_token", "secret")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(configuration.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "hidden") || strings.Contains(string(content), "secret") || !strings.Contains(string(content), "visible") {
		t.Fatalf("configured trace was unsafe: %q", content)
	}

	invalid := ResolveDebugConfiguration(func(name string) (string, bool) {
		if name == LogLevelEnvironmentVariable {
			return "verbose", true
		}
		return "", false
	}, "/private/default.log")
	if invalid.Warning == "" || invalid.Level != slog.LevelInfo || invalid.Enabled {
		t.Fatalf("invalid level did not safely fall back: %+v", invalid)
	}
	disabledLogger, disabledCloser, err := invalid.Start(TraceFileOptions{}, TraceOptions{})
	if err != nil || disabledLogger != nil || disabledCloser != nil {
		t.Fatalf("disabled tracing performed work: logger=%v closer=%v err=%v", disabledLogger, disabledCloser, err)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("sensitive backend detail") }

type pointerWriter struct{}

func (*pointerWriter) Write(value []byte) (int, error) { return len(value), nil }

func TestNewTraceHandlerRejectsTypedNilDestination(t *testing.T) {
	var destination *pointerWriter
	defer func() {
		if recover() == nil {
			t.Fatal("typed nil destination did not panic")
		}
	}()
	NewTraceHandler(destination, TraceOptions{})
}
