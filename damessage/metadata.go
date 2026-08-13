package damessage

import (
	"encoding/json"
	"errors"
)

var (
	ErrNilMetadata = errors.New("metadata map is nil")
	ErrMetadataKey = errors.New("metadata key is required")
)

// MetadataAs decodes one typed value from a JSON metadata map. The boolean is
// false when the key is absent or its value cannot be decoded as T.
func MetadataAs[T any](metadata map[string]json.RawMessage, key string) (T, bool) {
	var zero T
	raw, ok := metadata[key]
	if !ok || len(raw) == 0 {
		return zero, false
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return zero, false
	}
	return value, true
}

// SetMetadata JSON-encodes value under key. A nil map or empty key is rejected
// so callers never silently lose metadata.
func SetMetadata[T any](metadata map[string]json.RawMessage, key string, value T) error {
	if metadata == nil {
		return ErrNilMetadata
	}
	if key == "" {
		return ErrMetadataKey
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	metadata[key] = raw
	return nil
}
