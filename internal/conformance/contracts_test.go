package conformance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
)

type generatedContracts struct {
	Generated    bool             `json:"generated"`
	SourceSHA256 string           `json:"source_sha256"`
	Contract     contractFixtures `json:"contract"`
}

type contractFixtures struct {
	SchemaVersion int                     `json:"schema_version"`
	Provenance    []provenanceContract    `json:"provenance"`
	Message       damessage.Message       `json:"message"`
	Tool          datool.Definition       `json:"tool"`
	ModelResponse damodel.Response        `json:"model_response"`
	StateUpdate   dastate.Values          `json:"state_update"`
	Checkpoint    dacheckpoint.Checkpoint `json:"checkpoint"`
	StreamEvent   streamEventContract     `json:"stream_event"`
}

type provenanceContract struct {
	Source        string   `json:"source"`
	Path          string   `json:"path"`
	TestSelectors []string `json:"test_selectors"`
}

type streamEventContract struct {
	Version int `json:"version"`
	dagent.Event
}

func TestGeneratedContractsDecodeValidateAndRoundTrip(t *testing.T) {
	envelope := readGeneratedContracts(t)
	if err := validateGeneratedContracts(envelope); err != nil {
		t.Fatal(err)
	}
	contracts := []struct {
		name  string
		value any
	}{
		{name: "message", value: envelope.Contract.Message},
		{name: "tool", value: envelope.Contract.Tool},
		{name: "model_response", value: envelope.Contract.ModelResponse},
		{name: "state_update", value: envelope.Contract.StateUpdate},
		{name: "checkpoint", value: envelope.Contract.Checkpoint},
		{name: "stream_event", value: envelope.Contract.StreamEvent},
	}
	for _, contract := range contracts {
		t.Run(contract.name, func(t *testing.T) {
			assertJSONRoundTrip(t, contract.value)
		})
	}
}

func TestGeneratedContractsRejectUnknownFields(t *testing.T) {
	data := readGeneratedContractData(t)
	tests := []struct {
		name string
		path []string
	}{
		{name: "envelope", path: nil},
		{name: "contract", path: []string{"contract"}},
		{name: "message", path: []string{"contract", "message"}},
		{name: "tool", path: []string{"contract", "tool"}},
		{name: "model_response", path: []string{"contract", "model_response"}},
		{name: "checkpoint", path: []string{"contract", "checkpoint"}},
		{name: "stream_event", path: []string{"contract", "stream_event"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := addUnknownField(t, data, test.path)
			var envelope generatedContracts
			if err := decodeStrict(mutated, &envelope); err == nil {
				t.Fatal("decode unexpectedly accepted an unknown field")
			}
		})
	}
	t.Run("provenance", func(t *testing.T) {
		mutated := addUnknownProvenanceField(t, data)
		var envelope generatedContracts
		if err := decodeStrict(mutated, &envelope); err == nil {
			t.Fatal("decode unexpectedly accepted an unknown provenance field")
		}
	})
	t.Run("state_update_message", func(t *testing.T) {
		mutated := addUnknownStateUpdateMessageField(t, data)
		var envelope generatedContracts
		if err := decodeStrict(mutated, &envelope); err != nil {
			t.Fatal(err)
		}
		if err := validateGeneratedContracts(envelope); err == nil {
			t.Fatal("validation unexpectedly accepted an unknown state-update message field")
		}
	})
}

func TestGeneratedContractsRejectInvalidMutations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*generatedContracts)
	}{
		{name: "schema_version", mutate: func(value *generatedContracts) { value.Contract.SchemaVersion++ }},
		{name: "provenance", mutate: func(value *generatedContracts) { value.Contract.Provenance = nil }},
		{name: "message", mutate: func(value *generatedContracts) { value.Contract.Message.Role = "invalid" }},
		{name: "tool", mutate: func(value *generatedContracts) { value.Contract.Tool.Name = "" }},
		{name: "model_response", mutate: func(value *generatedContracts) { value.Contract.ModelResponse.Message.Role = "invalid" }},
		{name: "state_update", mutate: func(value *generatedContracts) {
			value.Contract.StateUpdate["messages"] = []any{map[string]any{"role": "invalid"}}
		}},
		{name: "checkpoint", mutate: func(value *generatedContracts) { value.Contract.Checkpoint.Version++ }},
		{name: "stream_event", mutate: func(value *generatedContracts) { value.Contract.StreamEvent.Mode = "invalid" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope := readGeneratedContracts(t)
			test.mutate(&envelope)
			if err := validateGeneratedContracts(envelope); err == nil {
				t.Fatal("validation unexpectedly accepted a mutated contract")
			}
		})
	}
}

func readGeneratedContracts(t *testing.T) generatedContracts {
	t.Helper()
	data := readGeneratedContractData(t)
	var envelope generatedContracts
	if err := decodeStrict(data, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func readGeneratedContractData(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/contracts.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateGeneratedContracts(envelope generatedContracts) error {
	if !envelope.Generated {
		return fmt.Errorf("fixture is not marked generated")
	}
	digest, err := hex.DecodeString(envelope.SourceSHA256)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("source_sha256 is not a SHA-256 digest")
	}
	contract := envelope.Contract
	if contract.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema version %d", contract.SchemaVersion)
	}
	if len(contract.Provenance) == 0 {
		return fmt.Errorf("provenance is required")
	}
	for index, provenance := range contract.Provenance {
		if provenance.Source == "" || provenance.Path == "" || len(provenance.TestSelectors) == 0 {
			return fmt.Errorf("provenance entry %d requires source, path, and test selectors", index)
		}
		for _, selector := range provenance.TestSelectors {
			if selector == "" {
				return fmt.Errorf("provenance entry %d has an empty test selector", index)
			}
		}
	}
	if err := contract.Message.Validate(); err != nil {
		return fmt.Errorf("message: %w", err)
	}
	if err := contract.Tool.Validate(); err != nil {
		return fmt.Errorf("tool: %w", err)
	}
	if err := contract.ModelResponse.Message.Validate(); err != nil {
		return fmt.Errorf("model response: %w", err)
	}
	encodedMessages, err := json.Marshal(contract.StateUpdate["messages"])
	if err != nil {
		return fmt.Errorf("state update messages: %w", err)
	}
	var messages []damessage.Message
	if err := decodeStrict(encodedMessages, &messages); err != nil {
		return fmt.Errorf("state update messages: %w", err)
	}
	if len(messages) == 0 {
		return fmt.Errorf("state update messages are required")
	}
	for index, message := range messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("state update message %d: %w", index, err)
		}
	}
	if contract.Checkpoint.Version != dacheckpoint.LatestVersion {
		return fmt.Errorf("checkpoint version = %d, want %d", contract.Checkpoint.Version, dacheckpoint.LatestVersion)
	}
	if contract.Checkpoint.ID == "" || contract.Checkpoint.ChannelValues == nil || contract.Checkpoint.ChannelVersions == nil || contract.Checkpoint.VersionsSeen == nil {
		return fmt.Errorf("checkpoint identity and channel maps are required")
	}
	if _, err := time.Parse(time.RFC3339Nano, contract.Checkpoint.Timestamp); err != nil {
		return fmt.Errorf("checkpoint timestamp: %w", err)
	}
	if contract.StreamEvent.Version != 1 {
		return fmt.Errorf("stream event version = %d, want 1", contract.StreamEvent.Version)
	}
	if contract.StreamEvent.Step < 0 || contract.StreamEvent.Node == "" || len(contract.StreamEvent.Update) == 0 {
		return fmt.Errorf("stream event step, node, and update are required")
	}
	switch contract.StreamEvent.Mode {
	case dagent.EventTask, dagent.EventUpdate, dagent.EventValues, dagent.EventInterrupt, dagent.EventCustom, dagent.EventToken, dagent.EventChild, dagent.EventToolProgress:
	default:
		return fmt.Errorf("unknown stream event mode %q", contract.StreamEvent.Mode)
	}
	return nil
}

func assertJSONRoundTrip(t *testing.T, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoded := reflect.New(reflect.TypeOf(value))
	if err := decodeStrict(encoded, decoded.Interface()); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(decoded.Elem().Interface())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, reencoded) {
		t.Fatalf("JSON changed across round trip:\nfirst:  %s\nsecond: %s", encoded, reencoded)
	}
}

func addUnknownField(t *testing.T, data []byte, path []string) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	target := document
	for _, element := range path {
		next, ok := target[element].(map[string]any)
		if !ok {
			t.Fatalf("path element %q is not an object", element)
		}
		target = next
	}
	target["unexpected_contract_field"] = true
	mutated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return mutated
}

func addUnknownProvenanceField(t *testing.T, data []byte) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	contract, ok := document["contract"].(map[string]any)
	if !ok {
		t.Fatal("contract is not an object")
	}
	provenance, ok := contract["provenance"].([]any)
	if !ok || len(provenance) == 0 {
		t.Fatal("provenance is not a non-empty array")
	}
	entry, ok := provenance[0].(map[string]any)
	if !ok {
		t.Fatal("provenance entry is not an object")
	}
	entry["unexpected_provenance_field"] = true
	mutated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return mutated
}

func addUnknownStateUpdateMessageField(t *testing.T, data []byte) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	contract, ok := document["contract"].(map[string]any)
	if !ok {
		t.Fatal("contract is not an object")
	}
	stateUpdate, ok := contract["state_update"].(map[string]any)
	if !ok {
		t.Fatal("state_update is not an object")
	}
	messages, ok := stateUpdate["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatal("state_update messages is not a non-empty array")
	}
	message, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatal("state_update message is not an object")
	}
	message["unexpected_contract_field"] = true
	mutated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return mutated
}
