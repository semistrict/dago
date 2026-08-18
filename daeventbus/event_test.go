package daeventbus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultOptionsAreFiniteAndUseful(t *testing.T) {
	got := DefaultOptions()
	want := Options{
		MaxLineBytes: 64 << 10, MaxPayloadBytes: 32 << 10,
		MaxCorrelationBytes: 256, MaxSocketPathBytes: 100,
		MaxEventsPerConnection: 256, MaxConcurrentConnections: 32,
		ClientIdleTimeout: 60 * time.Second, SinkTimeout: 30 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v", got)
	}
}

func TestDecodeEventPreservesPinnedVocabulary(t *testing.T) {
	options := compileOptions(Options{})
	event, err := decodeEvent([]byte(`{"kind":"command","payload":"/clear","bypass":"always","correlation_id":"req-1"}`), "test", options)
	if err != nil {
		t.Fatal(err)
	}
	want := Event{Kind: KindCommand, Payload: "/clear", Source: "test", Bypass: BypassAlways, CorrelationID: "req-1"}
	if !reflect.DeepEqual(event, want) {
		t.Fatalf("event = %#v", event)
	}
	for _, signal := range []Signal{SignalInterrupt, SignalForceClear} {
		line := []byte(`{"kind":"signal","payload":"` + signal + `"}`)
		if _, err := decodeEvent(line, "test", options); err != nil {
			t.Fatalf("signal %q: %v", signal, err)
		}
	}
}

func TestDecodeEventRejectsMalformedAndBoundedInput(t *testing.T) {
	options := compileOptions(Options{MaxLineBytes: 256, MaxPayloadBytes: 8, MaxCorrelationBytes: 4})
	for _, line := range [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`not-json`), []byte("\xff"),
		[]byte(`{"kind":"unknown","payload":"x"}`),
		[]byte(`{"kind":"prompt","payload":" "}`),
		[]byte(`{"kind":"prompt","payload":"123456789"}`),
		[]byte(`{"kind":"prompt","payload":"x","bypass":"root"}`),
		[]byte(`{"kind":"prompt","payload":"x","correlation_id":"12345"}`),
		[]byte(`{"kind":"signal","payload":"reboot"}`),
		[]byte(`{"kind":"prompt","payload":"x"} {"kind":"prompt","payload":"y"}`),
	} {
		if _, err := decodeEvent(line, "test", options); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("line %q error = %v", line, err)
		}
	}
	if _, err := decodeEvent([]byte(strings.Repeat("x", 257)), "test", options); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized line error = %v", err)
	}
}

func TestConstructorRejectsStaticInvalidInputs(t *testing.T) {
	valid := SinkFunc(func(context.Context, Event) error { return nil })
	var typedNil *testSink
	for name, operation := range map[string]func(){
		"nil sink":       func() { NewUnixSource(nil, "/tmp/private/events.sock", Options{}) },
		"typed nil sink": func() { NewUnixSource(typedNil, "/tmp/private/events.sock", Options{}) },
		"relative path":  func() { NewUnixSource(valid, "events.sock", Options{}) },
		"unclean path":   func() { NewUnixSource(valid, "/tmp/../tmp/events.sock", Options{}) },
		"root path":      func() { NewUnixSource(valid, "/", Options{}) },
		"control path":   func() { NewUnixSource(valid, "/tmp/private/event\n.sock", Options{}) },
		"long path":      func() { NewUnixSource(valid, "/"+strings.Repeat("x", 101), Options{}) },
		"negative limit": func() { NewUnixSource(valid, "/tmp/private/events.sock", Options{MaxPayloadBytes: -1}) },
		"nil context": func() {
			NewUnixSource(valid, "/tmp/private/events.sock", Options{}).Run(nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("operation did not panic")
				}
			}()
			operation()
		})
	}
}

type testSink struct{}

func (*testSink) HandleExternalEvent(context.Context, Event) error { return nil }
