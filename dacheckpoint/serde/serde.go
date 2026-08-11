// Package serde implements the safe language-neutral checkpoint payload subset.
package serde

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastate"
)

const DeltaSnapshotExtension int8 = 7

var (
	ErrUnsupportedType     = errors.New("unsupported checkpoint payload type")
	ErrUnsupportedEncoding = errors.New("unsupported checkpoint payload encoding")
	ErrMalformedPayload    = errors.New("malformed checkpoint payload")
)

// CompatibilityError identifies an unsupported or malformed persisted payload
// without losing its checkpoint and channel location.
type CompatibilityError struct {
	CheckpointID string
	Channel      string
	Encoding     string
	Cause        error
}

func (err *CompatibilityError) Error() string {
	return fmt.Sprintf("checkpoint payload compatibility error checkpoint=%q channel=%q encoding=%q: %v", err.CheckpointID, err.Channel, err.Encoding, err.Cause)
}

func (err *CompatibilityError) Unwrap() error { return err.Cause }

// WithContext attaches the durable record location to a codec failure.
func WithContext(err error, checkpointID, channel, encoding string) error {
	if err == nil {
		return nil
	}
	return &CompatibilityError{CheckpointID: checkpointID, Channel: channel, Encoding: encoding, Cause: err}
}

// Typed is the type tag and bytes stored by SQLite and PostgreSQL blob columns.
type Typed struct {
	Type string
	Data []byte
}

// Limits bound untrusted payload allocation and recursion.
type Limits struct {
	MaxDepth      int
	MaxCollection int
	MaxBytes      int
}

// Safe is an allowlisted serializer. It never imports modules, invokes constructors,
// decodes pickle, or executes serialized callables.
type Safe struct {
	limits Limits
}

func New(limits Limits) *Safe {
	if limits.MaxDepth <= 0 {
		limits.MaxDepth = 64
	}
	if limits.MaxCollection <= 0 {
		limits.MaxCollection = 1_000_000
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 64 << 20
	}
	return &Safe{limits: limits}
}

func (codec *Safe) Encode(value any) (Typed, error) {
	if value == nil {
		return Typed{Type: "null"}, nil
	}
	switch value := value.(type) {
	case []byte:
		return Typed{Type: "bytes", Data: append([]byte(nil), value...)}, nil
	case json.RawMessage:
		return Typed{Type: "bytes", Data: append([]byte(nil), value...)}, nil
	}
	wire, err := toWire(value, 0, codec.limits.MaxDepth)
	if err != nil {
		return Typed{}, err
	}
	data, err := marshalMessagePack(wire, codec.limits)
	if err != nil {
		return Typed{}, err
	}
	return Typed{Type: "msgpack", Data: data}, nil
}

func (codec *Safe) Decode(value Typed) (any, error) {
	if len(value.Data) > codec.limits.MaxBytes {
		return nil, fmt.Errorf("%w: payload exceeds %d bytes", ErrMalformedPayload, codec.limits.MaxBytes)
	}
	switch value.Type {
	case "null":
		return nil, nil
	case "bytes", "bytearray":
		return append([]byte(nil), value.Data...), nil
	case "json":
		decoded, err := decodeJSON(value.Data)
		if err != nil {
			return nil, fmt.Errorf("decode legacy JSON checkpoint payload: %w", err)
		}
		return fromWire(decoded)
	case "msgpack":
		decoded, err := unmarshalMessagePack(value.Data, codec.limits)
		if err != nil {
			return nil, err
		}
		return fromWire(decoded)
	case "pickle":
		return nil, fmt.Errorf("%w: pickle", ErrUnsupportedEncoding)
	default:
		return nil, fmt.Errorf("%w: type tag %q", ErrUnsupportedEncoding, value.Type)
	}
}

type extension struct {
	code  int8
	value any
}

func toWire(value any, depth, maxDepth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: maximum nesting depth exceeded", ErrUnsupportedType)
	}
	if value == nil {
		return nil, nil
	}
	switch value := value.(type) {
	case bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return value, nil
	case []byte:
		return append([]byte(nil), value...), nil
	case json.RawMessage:
		if !json.Valid(value) {
			return nil, fmt.Errorf("%w: invalid raw JSON", ErrUnsupportedType)
		}
		return decodeJSON(value)
	case dacheckpoint.DeltaSnapshot:
		wire, err := toWire(value.Value, depth+1, maxDepth)
		if err != nil {
			return nil, err
		}
		return extension{code: DeltaSnapshotExtension, value: wire}, nil
	case dacheckpoint.Checkpoint:
		return checkpointToWire(value, depth+1, maxDepth)
	case dacheckpoint.Metadata:
		return metadataToWire(value, depth+1, maxDepth)
	case damessage.Message:
		return taggedJSONRecord("dago.message.v1", value)
	case dastate.Overwrite:
		wire, err := toWire(value.Value, depth+1, maxDepth)
		if err != nil {
			return nil, err
		}
		return map[string]any{"$type": "dago.overwrite.v1", "value": wire}, nil
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			wire, err := toWire(item, depth+1, maxDepth)
			if err != nil {
				return nil, fmt.Errorf("checkpoint map key %q: %w", key, err)
			}
			result[key] = wire
		}
		return result, nil
	case dastate.Values:
		return toWire(map[string]any(value), depth, maxDepth)
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Slice, reflect.Array:
		result := make([]any, reflected.Len())
		for index := 0; index < reflected.Len(); index++ {
			wire, err := toWire(reflected.Index(index).Interface(), depth+1, maxDepth)
			if err != nil {
				return nil, fmt.Errorf("checkpoint list index %d: %w", index, err)
			}
			result[index] = wire
		}
		return result, nil
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%w: map key type %s", ErrUnsupportedType, reflected.Type().Key())
		}
		result := make(map[string]any, reflected.Len())
		iterator := reflected.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			wire, err := toWire(iterator.Value().Interface(), depth+1, maxDepth)
			if err != nil {
				return nil, fmt.Errorf("checkpoint map key %q: %w", key, err)
			}
			result[key] = wire
		}
		return result, nil
	case reflect.Pointer:
		return nil, fmt.Errorf("%w: pointer %T", ErrUnsupportedType, value)
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedType, value)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedType, value)
	}
}

func checkpointToWire(value dacheckpoint.Checkpoint, depth, maxDepth int) (map[string]any, error) {
	channelValues, err := toWire(value.ChannelValues, depth, maxDepth)
	if err != nil {
		return nil, err
	}
	versionsSeen, err := toWire(value.VersionsSeen, depth, maxDepth)
	if err != nil {
		return nil, err
	}
	updated, err := toWire(value.UpdatedChannels, depth, maxDepth)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"v": value.Version, "id": value.ID,
		"ts": value.Timestamp, "channel_values": channelValues,
		"channel_versions": stringMapToAny(value.ChannelVersions),
		"versions_seen":    versionsSeen, "updated_channels": updated,
	}, nil
}

func metadataToWire(value dacheckpoint.Metadata, depth, maxDepth int) (map[string]any, error) {
	counters := make(map[string]any, len(value.CountersSinceDeltaSnapshot))
	for key, counter := range value.CountersSinceDeltaSnapshot {
		counters[key] = []any{counter.Updates, counter.Supersteps}
	}
	extra, err := toWire(value.Extra, depth, maxDepth)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"$type": "dago.metadata.v1", "source": value.Source, "step": value.Step,
		"parents": stringMapToAny(value.Parents), "run_id": value.RunID,
		"counters_since_delta_snapshot": counters, "extra": extra,
	}, nil
}

func taggedJSONRecord(tag string, value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", tag, err)
	}
	decoded, err := decodeJSON(data)
	if err != nil {
		return nil, err
	}
	record, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("encode %s: expected object", tag)
	}
	record["$type"] = tag
	return record, nil
}

func fromWire(value any) (any, error) {
	switch value := value.(type) {
	case extension:
		if value.code != DeltaSnapshotExtension {
			return nil, fmt.Errorf("%w: MessagePack extension %d", ErrUnsupportedEncoding, value.code)
		}
		decoded, err := fromWire(value.value)
		if err != nil {
			return nil, err
		}
		return dacheckpoint.DeltaSnapshot{Value: decoded}, nil
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			decoded, err := fromWire(item)
			if err != nil {
				return nil, err
			}
			result[index] = decoded
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			decoded, err := fromWire(item)
			if err != nil {
				return nil, err
			}
			result[key] = decoded
		}
		tag, _ := result["$type"].(string)
		switch tag {
		case "":
			if isCheckpointRecord(result) {
				return wireToCheckpoint(result)
			}
			return result, nil
		case "dago.message.v1":
			delete(result, "$type")
			var decoded damessage.Message
			if err := recordInto(result, &decoded); err != nil {
				return nil, err
			}
			return decoded, nil
		case "dago.overwrite.v1":
			return dastate.Overwrite{Value: result["value"]}, nil
		case "dago.checkpoint.v1":
			return wireToCheckpoint(result)
		case "dago.metadata.v1":
			return wireToMetadata(result)
		default:
			return nil, fmt.Errorf("%w: record type %q", ErrUnsupportedEncoding, tag)
		}
	default:
		return value, nil
	}
}

func wireToCheckpoint(record map[string]any) (dacheckpoint.Checkpoint, error) {
	data := dacheckpoint.Checkpoint{
		Version: intNumber(record["v"]), ID: stringNumber(record["id"]),
		Timestamp: stringNumber(record["ts"]), ChannelValues: map[string]any{},
		ChannelVersions: map[string]string{}, VersionsSeen: map[string]map[string]string{},
	}
	if values, ok := record["channel_values"].(map[string]any); ok {
		data.ChannelValues = values
	}
	if versions, ok := record["channel_versions"].(map[string]any); ok {
		for key, value := range versions {
			data.ChannelVersions[key] = stringNumber(value)
		}
	}
	if seen, ok := record["versions_seen"].(map[string]any); ok {
		for node, raw := range seen {
			channels, ok := raw.(map[string]any)
			if !ok {
				return dacheckpoint.Checkpoint{}, fmt.Errorf("%w: versions_seen.%s", ErrMalformedPayload, node)
			}
			data.VersionsSeen[node] = make(map[string]string, len(channels))
			for channel, version := range channels {
				data.VersionsSeen[node][channel] = stringNumber(version)
			}
		}
	}
	if updated, ok := record["updated_channels"].([]any); ok {
		for _, item := range updated {
			data.UpdatedChannels = append(data.UpdatedChannels, stringNumber(item))
		}
	}
	return data, nil
}

func wireToMetadata(record map[string]any) (dacheckpoint.Metadata, error) {
	data := dacheckpoint.Metadata{
		Source: stringNumber(record["source"]), Step: intNumber(record["step"]),
		RunID: stringNumber(record["run_id"]), Parents: map[string]string{},
		CountersSinceDeltaSnapshot: map[string]dacheckpoint.DeltaCounter{}, Extra: map[string]any{},
	}
	if parents, ok := record["parents"].(map[string]any); ok {
		for key, value := range parents {
			data.Parents[key] = stringNumber(value)
		}
	}
	if counters, ok := record["counters_since_delta_snapshot"].(map[string]any); ok {
		for key, raw := range counters {
			items, ok := raw.([]any)
			if !ok || len(items) != 2 {
				return dacheckpoint.Metadata{}, fmt.Errorf("%w: delta counter %q", ErrMalformedPayload, key)
			}
			data.CountersSinceDeltaSnapshot[key] = dacheckpoint.DeltaCounter{
				Updates: uintNumber(items[0]), Supersteps: uintNumber(items[1]),
			}
		}
	}
	if extra, ok := record["extra"].(map[string]any); ok {
		data.Extra = extra
	}
	return data, nil
}

func recordInto(record map[string]any, target any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	return nil
}

func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	return normalizeJSONNumbers(value)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}

func normalizeJSONNumbers(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return integer, nil
		}
		return value.Float64()
	case []any:
		for index, item := range value {
			normalized, err := normalizeJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			value[index] = normalized
		}
	case map[string]any:
		for key, item := range value {
			normalized, err := normalizeJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			value[key] = normalized
		}
	}
	return value, nil
}

func stringMapToAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func intNumber(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case uint64:
		return int(value)
	default:
		return 0
	}
}

func uintNumber(value any) uint64 {
	switch value := value.(type) {
	case int:
		return uint64(value)
	case int64:
		return uint64(value)
	case uint64:
		return value
	default:
		return 0
	}
}

func stringNumber(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case int:
		return fmt.Sprintf("%d", value)
	case int64:
		return fmt.Sprintf("%d", value)
	case uint64:
		return fmt.Sprintf("%d", value)
	case float64:
		return fmt.Sprintf("%g", value)
	default:
		return ""
	}
}

func isCheckpointRecord(value map[string]any) bool {
	_, hasVersion := value["v"]
	_, hasID := value["id"]
	_, hasValues := value["channel_values"]
	_, hasVersions := value["channel_versions"]
	_, hasSeen := value["versions_seen"]
	return hasVersion && hasID && hasValues && hasVersions && hasSeen
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
