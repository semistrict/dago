package damessage

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMetadataTypedAccess(t *testing.T) {
	type details struct {
		Count int `json:"count"`
	}
	metadata := map[string]json.RawMessage{}
	if err := SetMetadata(metadata, "details", details{Count: 3}); err != nil {
		t.Fatalf("SetMetadata() error = %v", err)
	}
	value, ok := MetadataAs[details](metadata, "details")
	if !ok || value.Count != 3 {
		t.Fatalf("MetadataAs() = %#v, %v", value, ok)
	}
	if _, ok := MetadataAs[string](metadata, "details"); ok {
		t.Fatal("MetadataAs[string]() accepted object metadata")
	}
	if _, ok := MetadataAs[details](metadata, "missing"); ok {
		t.Fatal("MetadataAs() reported a missing key")
	}
}

func TestSetMetadataRejectsInvalidDestinationAndValue(t *testing.T) {
	if err := SetMetadata[string](nil, "key", "value"); !errors.Is(err, ErrNilMetadata) {
		t.Fatalf("SetMetadata(nil) error = %v", err)
	}
	if err := SetMetadata(map[string]json.RawMessage{}, "", "value"); !errors.Is(err, ErrMetadataKey) {
		t.Fatalf("SetMetadata(empty key) error = %v", err)
	}
	if err := SetMetadata(map[string]json.RawMessage{}, "invalid", make(chan int)); err == nil {
		t.Fatal("SetMetadata() accepted an unsupported JSON value")
	}
}
