package dahousekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultTraceCapacity   = 1_000
	defaultTraceFileBytes  = 8 << 20
	maxTraceCapacity       = 10_000
	maxTraceMessageBytes   = 4 << 10
	maxTraceValueBytes     = 2 << 10
	maxTraceAttributes     = 32
	maxTraceAttributeBytes = 128
)

var (
	ErrUnsafeTracePath = errors.New("unsafe debug trace path")
	ErrTraceLimit      = errors.New("debug trace byte limit reached")
	ErrTraceLocked     = errors.New("debug trace file is already in use")
)

// TraceOptions configures a structured trace handler. The zero value captures
// debug-and-higher records and retains the latest 1,000 records in memory.
type TraceOptions struct {
	MinimumLevel *slog.Level
	Capacity     int
}

// TraceRecord is a redacted, bounded debug record.
type TraceRecord struct {
	Version    int               `json:"version"`
	Index      uint64            `json:"index"`
	Time       time.Time         `json:"time"`
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// TraceSnapshot contains chronological retained records. NextIndex can be used
// as the since argument of a later Snapshot call.
type TraceSnapshot struct {
	Version      int           `json:"version"`
	Records      []TraceRecord `json:"records"`
	NextIndex    uint64        `json:"next_index"`
	TotalEmitted uint64        `json:"total_emitted"`
	Dropped      uint64        `json:"dropped"`
}

type traceState struct {
	mu          sync.Mutex
	destination io.Writer
	capacity    int
	records     []TraceRecord
	next        uint64
	dropped     uint64
}

// TraceHandler is a centralized slog handler with an in-memory resume buffer.
// Child handlers returned by WithAttrs and WithGroup share the same destination
// and buffer.
type TraceHandler struct {
	state  *traceState
	level  slog.Level
	attrs  []slog.Attr
	groups []string
}

// NewTraceHandler constructs a handler around a caller-owned destination.
// Construction performs no I/O. A nil destination or invalid static option
// panics; zero options are useful defaults.
func NewTraceHandler(destination io.Writer, options TraceOptions) *TraceHandler {
	if nilWriter(destination) {
		panic("dahousekeeping: trace destination is required")
	}
	capacity := options.Capacity
	if capacity == 0 {
		capacity = defaultTraceCapacity
	}
	if capacity < 0 || capacity > maxTraceCapacity {
		panic("dahousekeeping: trace capacity is out of range")
	}
	level := slog.LevelDebug
	if options.MinimumLevel != nil {
		level = *options.MinimumLevel
	}
	return &TraceHandler{
		state: &traceState{destination: destination, capacity: capacity, records: make([]TraceRecord, 0, capacity)},
		level: level,
	}
}

func nilWriter(destination io.Writer) bool {
	if destination == nil {
		return true
	}
	value := reflect.ValueOf(destination)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (handler *TraceHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= handler.level
}

func (handler *TraceHandler) Handle(ctx context.Context, record slog.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	attributes := make(map[string]string)
	for _, attribute := range handler.attrs {
		addTraceAttribute(attributes, handler.groups, attribute)
	}
	record.Attrs(func(attribute slog.Attr) bool {
		if len(attributes) >= maxTraceAttributes {
			return false
		}
		addTraceAttribute(attributes, handler.groups, attribute)
		return len(attributes) < maxTraceAttributes
	})
	entry := TraceRecord{
		Version: 1, Time: record.Time, Level: record.Level.String(),
		Message: boundTraceText(record.Message, maxTraceMessageBytes), Attributes: attributes,
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	handler.state.mu.Lock()
	defer handler.state.mu.Unlock()
	entry.Index = handler.state.next
	handler.state.next++
	if handler.state.capacity > 0 {
		if len(handler.state.records) == handler.state.capacity {
			copy(handler.state.records, handler.state.records[1:])
			handler.state.records[len(handler.state.records)-1] = cloneTraceRecord(entry)
			handler.state.dropped++
		} else {
			handler.state.records = append(handler.state.records, cloneTraceRecord(entry))
		}
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode debug trace: %w", err)
	}
	encoded = append(encoded, '\n')
	if _, err := handler.state.destination.Write(encoded); err != nil {
		return fmt.Errorf("write debug trace: %w", err)
	}
	return nil
}

func (handler *TraceHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	copyHandler := *handler
	copyHandler.attrs = append(append([]slog.Attr(nil), handler.attrs...), attributes...)
	return &copyHandler
}

func (handler *TraceHandler) WithGroup(name string) slog.Handler {
	copyHandler := *handler
	name = boundTraceText(name, maxTraceAttributeBytes)
	if name != "" {
		copyHandler.groups = append(append([]string(nil), handler.groups...), name)
	}
	return &copyHandler
}

// Snapshot returns records with Index >= since. If since predates the retained
// window, Dropped reports the number of unavailable records in this response.
func (handler *TraceHandler) Snapshot(since uint64) TraceSnapshot {
	handler.state.mu.Lock()
	defer handler.state.mu.Unlock()
	snapshot := TraceSnapshot{Version: 1, NextIndex: handler.state.next, TotalEmitted: handler.state.next}
	oldest := handler.state.next
	if len(handler.state.records) > 0 {
		oldest = handler.state.records[0].Index
	}
	if since < oldest {
		snapshot.Dropped = oldest - since
		since = oldest
	}
	for _, record := range handler.state.records {
		if record.Index >= since {
			snapshot.Records = append(snapshot.Records, cloneTraceRecord(record))
		}
	}
	return snapshot
}

func addTraceAttribute(destination map[string]string, groups []string, attribute slog.Attr) {
	if len(destination) >= maxTraceAttributes {
		return
	}
	key := boundTraceText(attribute.Key, maxTraceAttributeBytes)
	if key == "" {
		return
	}
	if len(groups) > 0 {
		key = boundTraceText(strings.Join(append(append([]string(nil), groups...), key), "."), maxTraceAttributeBytes)
	}
	if sensitiveTraceKey(key) {
		destination[key] = "[REDACTED]"
		return
	}
	destination[key] = traceValue(attribute.Value)
}

func traceValue(value slog.Value) (result string) {
	defer func() {
		if recover() != nil {
			result = "[UNAVAILABLE]"
		}
	}()
	value = value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return boundTraceText(value.String(), maxTraceValueBytes)
	case slog.KindBool:
		return strconvFormatBool(value.Bool())
	case slog.KindInt64:
		return fmt.Sprintf("%d", value.Int64())
	case slog.KindUint64:
		return fmt.Sprintf("%d", value.Uint64())
	case slog.KindFloat64:
		if math.IsNaN(value.Float64()) || math.IsInf(value.Float64(), 0) {
			return "[UNAVAILABLE]"
		}
		return fmt.Sprintf("%g", value.Float64())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().UTC().Format(time.RFC3339Nano)
	default:
		return "[UNAVAILABLE]"
	}
}

func strconvFormatBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func sensitiveTraceKey(key string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
	for _, marker := range []string{"authorization", "password", "passwd", "secret", "token", "apikey", "cookie"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func boundTraceText(value string, limit int) string {
	value = strings.Map(func(character rune) rune {
		if character == '\r' || character == '\n' || character == 0 || (character < 0x20 && character != '\t') {
			return ' '
		}
		return character
	}, value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func cloneTraceRecord(record TraceRecord) TraceRecord {
	copyRecord := record
	if record.Attributes != nil {
		copyRecord.Attributes = make(map[string]string, len(record.Attributes))
		for key, value := range record.Attributes {
			copyRecord.Attributes[key] = value
		}
	}
	return copyRecord
}

// TraceFileOptions bounds a debug trace file. The zero value caps it at 8 MiB.
type TraceFileOptions struct {
	MaxBytes int64
}

const (
	DebugEnvironmentVariable     = "DEEPAGENTS_CODE_DEBUG"
	DebugFileEnvironmentVariable = "DEEPAGENTS_CODE_DEBUG_FILE"
	LogLevelEnvironmentVariable  = "DEEPAGENTS_CODE_LOG_LEVEL"
)

// DebugConfiguration is the resolved, side-effect-free environment contract for
// file tracing. Warning is nonempty only when an invalid log level fell back.
type DebugConfiguration struct {
	Enabled bool
	Path    string
	Level   slog.Level
	Warning string
}

// ResolveDebugConfiguration resolves the pinned debug environment variables.
// lookup and defaultPath are mandatory positional dependencies. No file is
// opened and no warning is emitted by this function.
func ResolveDebugConfiguration(lookup func(string) (string, bool), defaultPath string) DebugConfiguration {
	if lookup == nil {
		panic("dahousekeeping: environment lookup is required")
	}
	if defaultPath == "" {
		panic("dahousekeeping: default debug path is required")
	}
	rawEnabled, _ := lookup(DebugEnvironmentVariable)
	enabled := envTruthy(rawEnabled)
	configuration := DebugConfiguration{Enabled: enabled, Path: defaultPath, Level: slog.LevelInfo}
	if enabled {
		configuration.Level = slog.LevelDebug
	}
	if path, exists := lookup(DebugFileEnvironmentVariable); exists && strings.TrimSpace(path) != "" {
		configuration.Path = strings.TrimSpace(path)
	}
	if rawLevel, exists := lookup(LogLevelEnvironmentVariable); exists && strings.TrimSpace(rawLevel) != "" {
		switch strings.ToUpper(strings.TrimSpace(rawLevel)) {
		case "DEBUG":
			configuration.Level = slog.LevelDebug
		case "INFO":
			configuration.Level = slog.LevelInfo
		case "WARNING", "WARN":
			configuration.Level = slog.LevelWarn
		case "ERROR", "CRITICAL":
			configuration.Level = slog.LevelError
		default:
			configuration.Warning = "invalid debug log level; using the safe default"
		}
	}
	return configuration
}

func envTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Start opens the configured private file and constructs a logger when enabled.
// A disabled configuration performs no I/O and returns nil values.
func (configuration DebugConfiguration) Start(fileOptions TraceFileOptions, traceOptions TraceOptions) (*slog.Logger, io.Closer, error) {
	if !configuration.Enabled {
		return nil, nil, nil
	}
	file, err := OpenTraceFile(configuration.Path, fileOptions)
	if err != nil {
		return nil, nil, err
	}
	traceOptions.MinimumLevel = new(configuration.Level)
	handler := NewTraceHandler(file, traceOptions)
	return slog.New(handler), file, nil
}

type boundedTraceFile struct {
	mu       sync.Mutex
	file     *os.File
	lock     *os.File
	root     *os.Root
	lockName string
	written  int64
	maxBytes int64
}

// OpenTraceFile opens an append-only, owner-private, bounded trace file. The
// required path must be absolute and its final parent must be owner-private; a
// missing parent is created with mode 0700. Symbolic-link targets are rejected.
func OpenTraceFile(path string, options TraceFileOptions) (io.WriteCloser, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." {
		return nil, ErrUnsafeTracePath
	}
	maximum := options.MaxBytes
	if maximum == 0 {
		maximum = defaultTraceFileBytes
	}
	if maximum < 1 || maximum > 1<<30 {
		panic("dahousekeeping: trace file byte limit is out of range")
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	if !validName(name) {
		return nil, ErrUnsafeTracePath
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("open debug trace: %w", ErrUnsafeTracePath)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !ownerPrivateDirectory(parentInfo) || parentInfo.Mode()&os.ModeSymlink != 0 {
		return nil, ErrUnsafeTracePath
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("open debug trace: %w", ErrUnsafeTracePath)
	}
	lockName := name + ".lock"
	lock, err := root.OpenFile(lockName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		root.Close()
		if errors.Is(err, os.ErrExist) {
			return nil, ErrTraceLocked
		}
		return nil, fmt.Errorf("lock debug trace: %w", ErrUnsafeTracePath)
	}
	cleanupLock := func() {
		_ = lock.Close()
		_ = root.Remove(lockName)
		_ = root.Close()
	}
	if info, statErr := root.Lstat(name); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximum {
			cleanupLock()
			return nil, ErrUnsafeTracePath
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		cleanupLock()
		return nil, fmt.Errorf("open debug trace: %w", ErrUnsafeTracePath)
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		cleanupLock()
		return nil, fmt.Errorf("open debug trace: %w", ErrUnsafeTracePath)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		cleanupLock()
		return nil, fmt.Errorf("secure debug trace: %w", ErrUnsafeTracePath)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		file.Close()
		cleanupLock()
		return nil, ErrUnsafeTracePath
	}
	return &boundedTraceFile{file: file, lock: lock, root: root, lockName: lockName, written: info.Size(), maxBytes: maximum}, nil
}

func (file *boundedTraceFile) Write(value []byte) (int, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if int64(len(value)) > file.maxBytes-file.written {
		return 0, ErrTraceLimit
	}
	written, err := file.file.Write(value)
	file.written += int64(written)
	return written, err
}

func (file *boundedTraceFile) Close() error {
	file.mu.Lock()
	defer file.mu.Unlock()
	fileError := file.file.Close()
	lockError := file.lock.Close()
	removeError := file.root.Remove(file.lockName)
	rootError := file.root.Close()
	return errors.Join(fileError, lockError, removeError, rootError)
}
