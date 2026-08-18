// Package tracing adds provider-neutral per-run tracing to a datalon runtime.
package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/datalon"
)

const (
	defaultProject          = "deepagents-talon"
	defaultRunName          = "talon.agent"
	defaultMaxPayloadBytes  = 1 << 20
	defaultMaxMetadataBytes = 256 << 10
	defaultMaxErrorBytes    = 16 << 10
	defaultFinishTimeout    = 5 * time.Second
)

// Run is the bounded start record supplied to a Sink.
type Run struct {
	Name           string
	Project        string
	AssistantID    string
	ConversationID string
	Input          string
	Metadata       map[string]any
	Tags           []string
	StartTime      time.Time
}

// Completion records one runtime outcome.
type Completion struct {
	Output  string
	Error   string
	EndTime time.Time
}

// Span finishes one started trace.
type Span interface {
	End(context.Context, Completion) error
}

// Sink starts traces in an application-owned tracing provider.
type Sink interface {
	Begin(context.Context, Run) (Span, error)
}

// Options controls optional names, bounds, and error reporting. Its zero value
// is useful: a stable project/run name, 1 MiB payloads, 256 KiB metadata,
// 16 KiB errors, and a five-second finish bound.
type Options struct {
	Project          string
	RunName          string
	MaxPayloadBytes  int
	MaxMetadataBytes int
	MaxErrorBytes    int
	FinishTimeout    time.Duration
	OnError          func(error)
}

// Runtime wraps one required runtime with one required trace sink.
type Runtime struct {
	runtime     datalon.Runtime
	sink        Sink
	assistantID string
	options     Options
	enabled     bool
	replicas    []string
	redact      func(string) string
	safeErrors  bool
}

// New constructs an always-enabled tracing runtime without performing I/O.
func New(runtime datalon.Runtime, sink Sink, assistantID string, options Options) *Runtime {
	return newRuntime(runtime, sink, assistantID, options, true)
}

// NewFromEnv constructs a runtime enabled only when LANGSMITH_TRACING is
// truthy and LANGSMITH_API_KEY is non-empty. A nil environment reads the
// process environment.
func NewFromEnv(runtime datalon.Runtime, sink Sink, assistantID string, environment map[string]string, options Options) *Runtime {
	return newRuntime(runtime, sink, assistantID, options, Enabled(environment))
}

// Enabled reports the pinned opt-in environment contract.
func Enabled(environment map[string]string) bool {
	if environment == nil {
		environment = processEnvironment()
	}
	tracing := strings.ToLower(strings.TrimSpace(environment["LANGSMITH_TRACING"]))
	switch tracing {
	case "1", "true", "yes", "on":
	default:
		return false
	}
	return strings.TrimSpace(environment["LANGSMITH_API_KEY"]) != ""
}

func newRuntime(runtime datalon.Runtime, sink Sink, assistantID string, options Options, enabled bool) *Runtime {
	if nilValue(runtime) {
		panic("tracing runtime is required")
	}
	if nilValue(sink) {
		panic("tracing sink is required")
	}
	assistantID = strings.TrimSpace(assistantID)
	if assistantID == "" || len(assistantID) > 128 || strings.ContainsAny(assistantID, "\x00\r\n") {
		panic("tracing assistant ID is invalid")
	}
	options = options.withDefaults()
	return &Runtime{runtime: runtime, sink: sink, assistantID: assistantID, options: options, enabled: enabled, redact: func(value string) string { return value }}
}

func (options Options) withDefaults() Options {
	if options.MaxPayloadBytes < 0 || options.MaxMetadataBytes < 0 || options.MaxErrorBytes < 0 || options.FinishTimeout < 0 {
		panic("tracing limits cannot be negative")
	}
	if strings.TrimSpace(options.Project) == "" {
		options.Project = defaultProject
	}
	if strings.TrimSpace(options.RunName) == "" {
		options.RunName = defaultRunName
	}
	if options.MaxPayloadBytes <= 0 {
		options.MaxPayloadBytes = defaultMaxPayloadBytes
	}
	if options.MaxMetadataBytes <= 0 {
		options.MaxMetadataBytes = defaultMaxMetadataBytes
	}
	if options.MaxErrorBytes <= 0 {
		options.MaxErrorBytes = defaultMaxErrorBytes
	}
	if options.FinishTimeout <= 0 {
		options.FinishTimeout = defaultFinishTimeout
	}
	if len(options.Project) > 256 || len(options.RunName) > 256 || strings.ContainsAny(options.Project+options.RunName, "\x00\r\n") {
		panic("tracing project or run name is invalid")
	}
	if options.MaxPayloadBytes > 16<<20 || options.MaxMetadataBytes > 4<<20 || options.MaxErrorBytes > 1<<20 || options.FinishTimeout > time.Minute {
		panic("tracing limits exceed hard maxima")
	}
	return options
}

func (runtime *Runtime) Start(ctx context.Context) error { return runtime.runtime.Start(ctx) }
func (runtime *Runtime) Stop(ctx context.Context) error  { return runtime.runtime.Stop(ctx) }

// Invoke traces one channel or scheduler run while preserving the wrapped
// runtime's result, error, cancellation, and panic behavior.
func (runtime *Runtime) Invoke(ctx context.Context, request datalon.Request) (result datalon.Result, invokeErr error) {
	if runtime == nil || nilValue(runtime.runtime) || nilValue(runtime.sink) {
		panic("initialized tracing runtime is required")
	}
	if !runtime.enabled {
		return runtime.runtime.Invoke(ctx, request)
	}
	metadata := boundedMetadata(request.Metadata, runtime.options.MaxMetadataBytes)
	metadata = redactMetadata(metadata, runtime.redact)
	metadata["assistant_id"] = runtime.assistantID
	metadata["conversation_id"] = boundedString(request.ConversationID, 4096)
	tags := []string{"deepagents-talon", "assistant:" + runtime.assistantID}
	if trigger, ok := metadata["trigger"].(string); ok && trigger != "" {
		tags = append(tags, "trigger:"+boundedString(trigger, 128))
	}
	span, err := runtime.begin(ctx, Run{
		Name: runtime.options.RunName, Project: runtime.options.Project,
		AssistantID: runtime.assistantID, ConversationID: boundedString(request.ConversationID, 4096),
		Input: boundedString(runtime.redact(request.Text), runtime.options.MaxPayloadBytes), Metadata: metadata,
		Tags: tags, StartTime: time.Now().UTC(),
	})
	if err != nil || nilValue(span) {
		if err == nil {
			err = fmt.Errorf("trace sink returned no span")
		}
		runtime.reportTraceError("start trace", err)
		return runtime.runtime.Invoke(ctx, request)
	}
	defer func() {
		completion := Completion{Output: boundedString(runtime.redact(result.Text), runtime.options.MaxPayloadBytes), EndTime: time.Now().UTC()}
		if invokeErr != nil {
			completion.Error = boundedString(runtime.redact(invokeErr.Error()), runtime.options.MaxErrorBytes)
		}
		if panicValue := recover(); panicValue != nil {
			completion.Error = "runtime panicked"
			runtime.finish(ctx, span, completion)
			panic(panicValue)
		}
		runtime.finish(ctx, span, completion)
	}()
	return runtime.runtime.Invoke(ctx, request)
}

func (runtime *Runtime) finish(ctx context.Context, span Span, completion Completion) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtime.options.FinishTimeout)
	defer cancel()
	if err := span.End(finishCtx, completion); err != nil {
		runtime.reportTraceError("finish trace", err)
	}
}

func (runtime *Runtime) begin(ctx context.Context, run Run) (Span, error) {
	primary, err := runtime.sink.Begin(ctx, run)
	if err != nil || nilValue(primary) {
		return primary, err
	}
	spans := []Span{primary}
	for _, project := range runtime.replicas {
		replica := run
		replica.Project = project
		span, replicaErr := runtime.sink.Begin(ctx, replica)
		if replicaErr != nil || nilValue(span) {
			if replicaErr == nil {
				replicaErr = fmt.Errorf("trace sink returned no span")
			}
			runtime.reportTraceError("start replica trace", replicaErr)
			continue
		}
		spans = append(spans, span)
	}
	return spanGroup(spans), nil
}

type spanGroup []Span

func (spans spanGroup) End(ctx context.Context, completion Completion) error {
	var first error
	for _, span := range spans {
		if err := span.End(ctx, completion); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (runtime *Runtime) reportTraceError(operation string, err error) {
	if runtime.safeErrors {
		runtime.report(fmt.Errorf("%s failed", operation))
		return
	}
	runtime.report(fmt.Errorf("%s: %w", operation, err))
}

func (runtime *Runtime) report(err error) {
	if err != nil && runtime.options.OnError != nil {
		runtime.options.OnError(err)
	}
}

func boundedMetadata(metadata map[string]any, limit int) map[string]any {
	result := map[string]any{}
	if len(metadata) == 0 {
		return result
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	used := 2
	for _, key := range keys {
		if key == "" || len(key) > 1024 || strings.ContainsAny(key, "\x00\r\n") {
			continue
		}
		encoded, err := json.Marshal(metadata[key])
		if err != nil || len(encoded)+len(key)+4 > limit-used {
			continue
		}
		var cloned any
		if err := json.Unmarshal(encoded, &cloned); err != nil {
			continue
		}
		result[key] = cloned
		used += len(encoded) + len(key) + 4
	}
	return result
}

func redactMetadata(metadata map[string]any, redact func(string) string) map[string]any {
	result := make(map[string]any, len(metadata))
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[redact(key)] = redactValue(metadata[key], redact)
	}
	return result
}

func redactValue(value any, redact func(string) string) any {
	switch typed := value.(type) {
	case string:
		return redact(typed)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = redactValue(typed[index], redact)
		}
		return result
	case map[string]any:
		return redactMetadata(typed, redact)
	default:
		return value
	}
}

func boundedString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}

func nilValue(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

func processEnvironment() map[string]string {
	result := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			result[key] = value
		}
	}
	return result
}

var _ datalon.Runtime = (*Runtime)(nil)
