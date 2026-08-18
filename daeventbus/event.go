// Package daeventbus provides opt-in, transport-neutral external event ingress.
// Its Unix source only parses and forwards data; it grants no shell, model, or
// network authority by itself.
package daeventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind is the closed external event vocabulary.
type Kind string

const (
	KindCommand Kind = "command"
	KindPrompt  Kind = "prompt"
	KindSignal  Kind = "signal"
)

// Signal is the closed control-signal vocabulary.
type Signal string

const (
	SignalInterrupt  Signal = "interrupt"
	SignalForceClear Signal = "force-clear"
)

// Bypass describes the pinned queue-handling hint. A sink decides whether the
// hint has meaning; this package never executes an event.
type Bypass string

const (
	BypassQueued         Bypass = "queued"
	BypassAlways         Bypass = "always"
	BypassConnecting     Bypass = "connecting"
	BypassImmediateUI    Bypass = "immediate_ui"
	BypassSideEffectFree Bypass = "side_effect_free"
)

var (
	// ErrInvalidEvent identifies malformed event input.
	ErrInvalidEvent = errors.New("invalid external event")
	// ErrLimitExceeded identifies a finite ingress limit.
	ErrLimitExceeded = errors.New("external event limit exceeded")
	// ErrSink identifies a caller-owned sink failure. Sink text and panic values
	// are deliberately not exposed to clients.
	ErrSink = errors.New("external event sink failed")
	// ErrAlreadyRunning identifies concurrent Run calls on one source.
	ErrAlreadyRunning = errors.New("external event source already running")
	// ErrUnsupported identifies platforms without Unix-domain socket support.
	ErrUnsupported = errors.New("Unix external event source unsupported")
	// ErrUnsafePath identifies a socket path that violates local confinement.
	ErrUnsafePath = errors.New("unsafe external event socket path")
	// ErrPathOccupied identifies a non-stale entry at the requested socket path.
	ErrPathOccupied = errors.New("external event socket path occupied")
	// ErrTransport identifies a local socket lifecycle failure.
	ErrTransport = errors.New("external event socket transport failed")
)

// Event is the validated transport-independent event delivered to a Sink.
type Event struct {
	Kind          Kind   `json:"kind"`
	Payload       string `json:"payload"`
	Source        string `json:"source"`
	Bypass        Bypass `json:"bypass"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Reply is the version-1 newline-delimited acknowledgement shape.
type Reply struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Sink consumes validated events. Implementations must honor ctx; error and
// panic values are contained and converted into a static NACK.
type Sink interface {
	HandleExternalEvent(context.Context, Event) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(context.Context, Event) error

// HandleExternalEvent invokes the adapted function.
func (sink SinkFunc) HandleExternalEvent(ctx context.Context, event Event) error {
	return sink(ctx, event)
}

// Options bounds the local listener. Zero fields select useful finite defaults.
type Options struct {
	MaxLineBytes             int
	MaxPayloadBytes          int
	MaxCorrelationBytes      int
	MaxSocketPathBytes       int
	MaxEventsPerConnection   int
	MaxConcurrentConnections int
	ClientIdleTimeout        time.Duration
	SinkTimeout              time.Duration
	WriteTimeout             time.Duration
}

// DefaultOptions returns the production limits.
func DefaultOptions() Options {
	return Options{
		MaxLineBytes: 64 << 10, MaxPayloadBytes: 32 << 10,
		MaxCorrelationBytes: 256, MaxSocketPathBytes: 100,
		MaxEventsPerConnection: 256, MaxConcurrentConnections: 32,
		ClientIdleTimeout: 60 * time.Second, SinkTimeout: 30 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
}

type envelope struct {
	Kind          Kind            `json:"kind"`
	Payload       string          `json:"payload"`
	Bypass        json.RawMessage `json:"bypass"`
	CorrelationID json.RawMessage `json:"correlation_id"`
}

func compileOptions(options Options) Options {
	defaults := DefaultOptions()
	if options.MaxLineBytes == 0 {
		options.MaxLineBytes = defaults.MaxLineBytes
	}
	if options.MaxPayloadBytes == 0 {
		options.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	if options.MaxCorrelationBytes == 0 {
		options.MaxCorrelationBytes = defaults.MaxCorrelationBytes
	}
	if options.MaxSocketPathBytes == 0 {
		options.MaxSocketPathBytes = defaults.MaxSocketPathBytes
	}
	if options.MaxEventsPerConnection == 0 {
		options.MaxEventsPerConnection = defaults.MaxEventsPerConnection
	}
	if options.MaxConcurrentConnections == 0 {
		options.MaxConcurrentConnections = defaults.MaxConcurrentConnections
	}
	if options.ClientIdleTimeout == 0 {
		options.ClientIdleTimeout = defaults.ClientIdleTimeout
	}
	if options.SinkTimeout == 0 {
		options.SinkTimeout = defaults.SinkTimeout
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = defaults.WriteTimeout
	}
	if options.MaxLineBytes < 128 || options.MaxLineBytes > 1<<20 ||
		options.MaxPayloadBytes < 1 || options.MaxPayloadBytes > options.MaxLineBytes ||
		options.MaxCorrelationBytes < 1 || options.MaxCorrelationBytes > 4096 ||
		options.MaxSocketPathBytes < 16 || options.MaxSocketPathBytes > 100 ||
		options.MaxEventsPerConnection < 1 || options.MaxEventsPerConnection > 4096 ||
		options.MaxConcurrentConnections < 1 || options.MaxConcurrentConnections > 256 ||
		options.ClientIdleTimeout < time.Millisecond || options.ClientIdleTimeout > 10*time.Minute ||
		options.SinkTimeout < time.Millisecond || options.SinkTimeout > 10*time.Minute ||
		options.WriteTimeout < time.Millisecond || options.WriteTimeout > time.Minute {
		panic("daeventbus: options are outside their finite bounds")
	}
	return options
}

func decodeEvent(line []byte, source string, options Options) (Event, error) {
	if len(line) == 0 || len(line) > options.MaxLineBytes {
		return Event{}, ErrLimitExceeded
	}
	if !utf8.Valid(line) {
		return Event{}, fmt.Errorf("%w: input must be UTF-8 JSON", ErrInvalidEvent)
	}
	var raw envelope
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	if err := decoder.Decode(&raw); err != nil {
		return Event{}, fmt.Errorf("%w: malformed JSON", ErrInvalidEvent)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Event{}, fmt.Errorf("%w: multiple JSON values", ErrInvalidEvent)
	}
	if raw.Kind != KindCommand && raw.Kind != KindPrompt && raw.Kind != KindSignal {
		return Event{}, fmt.Errorf("%w: unknown kind", ErrInvalidEvent)
	}
	if raw.Payload == "" || strings.TrimSpace(raw.Payload) == "" || len(raw.Payload) > options.MaxPayloadBytes {
		return Event{}, fmt.Errorf("%w: payload is empty or too large", ErrInvalidEvent)
	}
	bypass := BypassQueued
	if len(raw.Bypass) != 0 && string(raw.Bypass) != "null" {
		if err := json.Unmarshal(raw.Bypass, &bypass); err != nil || !validBypass(bypass) {
			return Event{}, fmt.Errorf("%w: unknown bypass", ErrInvalidEvent)
		}
	}
	correlationID := ""
	if len(raw.CorrelationID) != 0 && string(raw.CorrelationID) != "null" {
		if err := json.Unmarshal(raw.CorrelationID, &correlationID); err != nil || !validCorrelationID(correlationID, options.MaxCorrelationBytes) {
			return Event{}, fmt.Errorf("%w: invalid correlation_id", ErrInvalidEvent)
		}
	}
	if raw.Kind == KindSignal {
		signal := Signal(strings.ToLower(strings.TrimSpace(raw.Payload)))
		if signal != SignalInterrupt && signal != SignalForceClear {
			return Event{}, fmt.Errorf("%w: unknown signal", ErrInvalidEvent)
		}
	}
	return Event{Kind: raw.Kind, Payload: raw.Payload, Source: source, Bypass: bypass, CorrelationID: correlationID}, nil
}

func recoverCorrelationID(line []byte, max int) string {
	if len(line) == 0 || len(line) > 1<<20 || !utf8.Valid(line) {
		return ""
	}
	var raw struct {
		CorrelationID string `json:"correlation_id"`
	}
	if json.Unmarshal(line, &raw) != nil || !validCorrelationID(raw.CorrelationID, max) {
		return ""
	}
	return raw.CorrelationID
}

func validBypass(value Bypass) bool {
	switch value {
	case BypassQueued, BypassAlways, BypassConnecting, BypassImmediateUI, BypassSideEffectFree:
		return true
	default:
		return false
	}
}

func validCorrelationID(value string, max int) bool {
	if value == "" {
		return false
	}
	if len(value) > max || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func callSink(ctx context.Context, sink Sink, event Event) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrSink
		}
	}()
	if err := sink.HandleExternalEvent(ctx, event); err != nil {
		return ErrSink
	}
	return nil
}

func nilInterface(value any) bool {
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
