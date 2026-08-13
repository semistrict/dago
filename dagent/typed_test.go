package dagent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/semistrict/dago/damessage"
)

type typedRecord struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestFieldErasesTypedReducerAndCheckpointForm(t *testing.T) {
	field := Field(FieldSpec[typedRecord]{
		Kind: FieldAggregate, Contract: "test.typed.v1",
		Initial: func() typedRecord { return typedRecord{Name: "initial"} },
		Reduce: func(current typedRecord, updates []typedRecord) (typedRecord, error) {
			for _, update := range updates {
				current.Count += update.Count
			}
			return current, nil
		},
		Clone: func(value typedRecord) typedRecord { return value },
	})
	if err := field.validate("typed"); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	value, err := field.Reduce(
		map[string]any{"name": "restored", "count": float64(2)},
		[]any{typedRecord{Count: 3}, map[string]any{"name": "", "count": float64(4)}},
	)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if got := value.(typedRecord); got.Name != "restored" || got.Count != 9 {
		t.Fatalf("Reduce() = %#v", got)
	}
	if _, err := field.Reduce(typedRecord{}, []any{"wrong"}); err == nil {
		t.Fatal("Reduce() accepted an incompatible update")
	}
}

func TestFieldAllowsNilForNilableTypes(t *testing.T) {
	field := Field(FieldSpec[map[string]int]{
		Kind: FieldLast, Contract: "test.nilable.v1",
		Clone: func(value map[string]int) map[string]int { return value },
	})
	if got, ok := field.Clone(nil).(map[string]int); !ok || got != nil {
		t.Fatalf("Clone(nil) = %#v, %v", got, ok)
	}
}

func TestRuntimeTypedAccessHandlesLiveAndCheckpointValues(t *testing.T) {
	live := typedRecord{Name: "live", Count: 1}
	if got, ok := ResumeAs[typedRecord](Runtime{Resume: live}); !ok || got != live {
		t.Fatalf("ResumeAs(live) = %#v, %v", got, ok)
	}
	restored := map[string]any{"name": "restored", "count": float64(2)}
	if got, ok := ResumeAs[typedRecord](Runtime{Resume: restored}); !ok || got.Name != "restored" || got.Count != 2 {
		t.Fatalf("ResumeAs(restored) = %#v, %v", got, ok)
	}
	if got, ok := InterruptAs[typedRecord](Interrupt{Value: restored}); !ok || got.Count != 2 {
		t.Fatalf("InterruptAs() = %#v, %v", got, ok)
	}
	if got, ok := DepsAs[*typedRecord](Runtime{Deps: &live}); !ok || got != &live {
		t.Fatalf("DepsAs() = %#v, %v", got, ok)
	}
}

func TestHumanApprovalDecodesCheckpointResume(t *testing.T) {
	middleware := HumanApproval([]ApprovalRule{{Pattern: "write"}})
	call := damessage.ToolCall{ID: "call-1", Name: "write", Arguments: json.RawMessage(`{}`)}
	response, err := middleware.BeforeTools(context.Background(), ToolBatchRequest{
		Calls: []damessage.ToolCall{call},
		Runtime: Runtime{Resume: map[string]any{"decisions": map[string]any{
			"call-1": map[string]any{"decision": "approve"},
		}}},
	})
	if err != nil {
		t.Fatalf("BeforeTools() error = %v", err)
	}
	if len(response.Calls) != 1 || response.Calls[0].ID != call.ID || !response.ResumeConsumed {
		t.Fatalf("BeforeTools() = %#v", response)
	}
}

func TestStructuredOutputTypedRoundTripAndValidation(t *testing.T) {
	declaration := StructuredOutputFor[typedRecord]("record", "A record")
	if declaration.Name != "record" || declaration.Description != "A record" || !declaration.Strict || !json.Valid(declaration.Schema) {
		t.Fatalf("StructuredOutputFor() = %#v", declaration)
	}
	value, err := StructuredAs[typedRecord](Result{Structured: json.RawMessage(`{"name":"ok","count":3}`)})
	if err != nil || value != (typedRecord{Name: "ok", Count: 3}) {
		t.Fatalf("StructuredAs() = %#v, %v", value, err)
	}
	_, err = StructuredAs[typedRecord](Result{Structured: json.RawMessage(`{"name":"bad","count":"three"}`)})
	if !errors.Is(err, ErrStructuredValidation) {
		t.Fatalf("StructuredAs(invalid) error = %v", err)
	}
}

func TestStructuredOutputForSchemaMatchesDatoolContract(t *testing.T) {
	declaration := StructuredOutputFor[typedRecord]("record", "")
	var schema map[string]any
	if err := json.Unmarshal(declaration.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schema["required"], []any{"name", "count"}) {
		t.Fatalf("required = %#v", schema["required"])
	}
}
