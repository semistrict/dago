package serde

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

func TestSafeTypedTags(t *testing.T) {
	codec := New(Limits{})
	tests := []struct {
		name string
		in   any
		tag  string
	}{
		{name: "null", in: nil, tag: "null"},
		{name: "bytes", in: []byte{1, 2}, tag: "bytes"},
		{name: "map", in: map[string]any{"ok": true}, tag: "msgpack"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := codec.Encode(test.in)
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			if encoded.Type != test.tag {
				t.Fatalf("Encode().Type = %q, want %q", encoded.Type, test.tag)
			}
			if _, err := codec.Decode(encoded); err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
		})
	}
}

func TestSafeCheckpointRoundTrip(t *testing.T) {
	codec := New(Limits{})
	call := damessage.Message{
		ID: "assistant", Role: damessage.RoleAssistant,
		ToolCalls: []damessage.ToolCall{{
			ID: "call", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
		}},
	}
	input := dacheckpoint.Checkpoint{
		Version: dacheckpoint.LatestVersion, ID: "cp", Timestamp: "time",
		ChannelValues: map[string]any{
			"messages":  dacheckpoint.DeltaSnapshot{Value: []damessage.Message{call}},
			"overwrite": dastate.Overwrite{Value: map[string]any{"a": int64(1)}},
		},
		ChannelVersions: map[string]string{"messages": "v1"},
		VersionsSeen:    map[string]map[string]string{"node": {"messages": "v1"}},
		UpdatedChannels: []string{"messages"},
	}
	encoded, err := codec.Encode(input)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decodedAny, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	decoded, ok := decodedAny.(dacheckpoint.Checkpoint)
	if !ok {
		t.Fatalf("Decode() type = %T", decodedAny)
	}
	if decoded.ID != input.ID || !reflect.DeepEqual(decoded.ChannelVersions, input.ChannelVersions) {
		t.Fatalf("decoded checkpoint = %+v", decoded)
	}
	snapshot, ok := decoded.ChannelValues["messages"].(dacheckpoint.DeltaSnapshot)
	if !ok {
		t.Fatalf("messages value type = %T", decoded.ChannelValues["messages"])
	}
	messages, ok := snapshot.Value.([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("snapshot value = %#v", snapshot.Value)
	}
	decodedMessage, ok := messages[0].(damessage.Message)
	if !ok || decodedMessage.ToolCalls[0].Name != "lookup" {
		t.Fatalf("decoded message = %#v", messages[0])
	}
	if _, ok := decoded.ChannelValues["overwrite"].(dastate.Overwrite); !ok {
		t.Fatalf("overwrite type = %T", decoded.ChannelValues["overwrite"])
	}
}

func TestSafeMetadataRoundTrip(t *testing.T) {
	codec := New(Limits{})
	input := dacheckpoint.Metadata{
		Source: "loop", Step: 2, Parents: map[string]string{"": "cp"},
		CountersSinceDeltaSnapshot: map[string]dacheckpoint.DeltaCounter{
			"messages": {Updates: 4, Supersteps: 5},
		},
		Extra: map[string]any{"tenant": "one"},
	}
	encoded, err := codec.Encode(input)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, input) {
		t.Fatalf("Decode() = %#v, want %#v", decoded, input)
	}
}

func TestSafeRejectsPythonSpecificAndUnknownEncodings(t *testing.T) {
	codec := New(Limits{})
	if _, err := codec.Decode(Typed{Type: "pickle", Data: []byte("unsafe")}); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("pickle Decode() error = %v", err)
	}
	// ext8, one-byte payload, Python constructor extension 0, nil payload.
	if _, err := codec.Decode(Typed{Type: "msgpack", Data: []byte{0xc7, 0x01, 0x00, 0xc0}}); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("constructor extension Decode() error = %v", err)
	}
	value := "pointer"
	if _, err := codec.Encode(&value); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("pointer Encode() error = %v", err)
	}
}

func TestCompatibilityErrorCarriesRecordLocation(t *testing.T) {
	err := WithContext(fmt.Errorf("%w: pickle", ErrUnsupportedEncoding), "checkpoint-1", "messages", "pickle")
	var compatibility *CompatibilityError
	if !errors.As(err, &compatibility) {
		t.Fatalf("error type = %T", err)
	}
	if compatibility.CheckpointID != "checkpoint-1" || compatibility.Channel != "messages" || compatibility.Encoding != "pickle" {
		t.Fatalf("compatibility error = %#v", compatibility)
	}
	if !errors.Is(err, ErrUnsupportedEncoding) {
		t.Fatalf("unwrap = %v", err)
	}
}

func TestSafeRejectsMalformedAndOversizedPayloads(t *testing.T) {
	codec := New(Limits{MaxBytes: 3})
	if _, err := codec.Decode(Typed{Type: "msgpack", Data: []byte{0xdb, 0, 0, 0}}); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("truncated Decode() error = %v", err)
	}
	if _, err := codec.Decode(Typed{Type: "bytes", Data: []byte{1, 2, 3, 4}}); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("oversized Decode() error = %v", err)
	}
	if _, err := codec.Encode("four"); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("oversized Encode() error = %v", err)
	}
}

func TestSafeCollectionLimitDoesNotRestrictStringBytes(t *testing.T) {
	codec := New(Limits{MaxBytes: 1 << 20, MaxCollection: 2})
	want := strings.Repeat("image-data", 100_000)

	encoded, err := codec.Encode(want)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := codec.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded != want {
		t.Fatal("decoded large string differs from input")
	}
	if _, err := codec.Encode([]any{1, 2, 3}); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("oversized collection Encode() error = %v", err)
	}
}

func FuzzSafeDecodeNeverPanics(fuzz *testing.F) {
	fuzz.Add([]byte{0xc0})
	fuzz.Add([]byte{0xc7, 0x01, 0x07, 0xc0})
	codec := New(Limits{MaxBytes: 1 << 20, MaxCollection: 10_000})
	fuzz.Fuzz(func(t *testing.T, data []byte) {
		_, _ = codec.Decode(Typed{Type: "msgpack", Data: data})
	})
}
