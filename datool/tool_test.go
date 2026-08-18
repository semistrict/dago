package datool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/dago/damessage"
)

func TestDefinitionValidate(t *testing.T) {
	valid := Definition{
		Name:        "lookup",
		Description: "Look up a value.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	invalid := valid
	invalid.Name = "bad name"
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("Validate() error = %v, want %v", err, ErrInvalidDefinition)
	}
	invalid = valid
	invalid.Extra = map[string]json.RawMessage{"cache_control": json.RawMessage(`{`)}
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("Validate() invalid extra error = %v, want %v", err, ErrInvalidDefinition)
	}
}

func TestFuncDefinitionDeepClonesExtras(t *testing.T) {
	function := Func{Spec: Definition{
		Name:        "lookup",
		Description: "Look up a value.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Extra:       map[string]json.RawMessage{"cache_control": json.RawMessage(`{"type":"ephemeral"}`)},
	}}
	definition := function.Definition()
	definition.InputSchema[0] = '['
	definition.Extra["cache_control"][0] = '['
	if !json.Valid(function.Spec.InputSchema) || !json.Valid(function.Spec.Extra["cache_control"]) {
		t.Fatalf("Definition() mutated source: %#v", function.Spec)
	}
}

func TestAliasPreservesSchemaAndDelegatesRuntime(t *testing.T) {
	target := Func{Spec: Definition{
		Name: "execute", Description: "Run a command.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}, Run: func(_ context.Context, raw json.RawMessage, runtime Runtime) (Result, error) {
		return TextResult(runtime.CallID + ":" + string(raw)), nil
	}}
	alias := Alias(target, "bash")
	definition := alias.Definition()
	if definition.Name != "bash" || !strings.Contains(string(definition.InputSchema), "command") {
		t.Fatalf("alias definition = %#v", definition)
	}
	result, err := alias.Execute(context.Background(), json.RawMessage(`{"command":"pwd"}`), Runtime{CallID: "call-1"})
	if err != nil || result.Content[0].Text != `call-1:{"command":"pwd"}` {
		t.Fatalf("alias result = %#v, %v", result, err)
	}
}

func TestAliasPanicsForInvalidStaticInputs(t *testing.T) {
	type typedNilTool struct{ Tool }
	var target *typedNilTool
	for name, work := range map[string]func(){
		"nil target":       func() { Alias(nil, "bash") },
		"typed nil target": func() { Alias(target, "bash") },
		"invalid name": func() {
			Alias(Func{Spec: Definition{Name: "execute", Description: "Run.", InputSchema: json.RawMessage(`{"type":"object"}`)}}, "bad name")
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Alias did not panic")
				}
			}()
			work()
		})
	}
}

func TestConfigurableSnapshotAndGetAreDefensive(t *testing.T) {
	source := map[string]any{"nested": map[string]any{"items": []any{"original"}}}
	config := NewConfigurable(source)
	source["nested"].(map[string]any)["items"].([]any)[0] = "source mutation"
	value, ok := config.Get("nested")
	if !ok {
		t.Fatal("nested setting missing")
	}
	value.(map[string]any)["items"].([]any)[0] = "get mutation"
	snapshot := config.Snapshot()
	if got := snapshot["nested"].(map[string]any)["items"].([]any)[0]; got != "original" {
		t.Fatalf("snapshot value = %#v", got)
	}
	snapshot["nested"].(map[string]any)["items"].([]any)[0] = "snapshot mutation"
	value, _ = config.Get("nested")
	if got := value.(map[string]any)["items"].([]any)[0]; got != "original" {
		t.Fatalf("configurable value = %#v", got)
	}
}

func TestFuncRejectsInvalidArgumentsAndWrapsErrors(t *testing.T) {
	wantErr := errors.New("failed")
	function := Func{
		Spec: Definition{Name: "f"},
		Run: func(context.Context, json.RawMessage, Runtime) (Result, error) {
			return Result{}, wantErr
		},
	}
	if _, err := function.Execute(context.Background(), []byte("{"), Runtime{}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("invalid Execute() error = %v", err)
	}
	if _, err := function.Execute(context.Background(), []byte(`{}`), Runtime{}); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), `tool "f"`) {
		t.Fatalf("failed Execute() error = %v", err)
	}
}

func TestFuncClonesPurposedUsage(t *testing.T) {
	usage := []damessage.PurposedUsage{{Purpose: "lookup", Usage: damessage.Usage{
		InputTokens: 3, InputDetails: map[string]int{"cached": 2},
	}}}
	function := Func{
		Spec: Definition{Name: "f"},
		Run: func(context.Context, json.RawMessage, Runtime) (Result, error) {
			return Result{OtherUsage: usage}, nil
		},
	}
	result, err := function.Execute(context.Background(), json.RawMessage(`{}`), Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	usage[0].InputDetails["cached"] = 9
	if result.OtherUsage[0].InputDetails["cached"] != 2 {
		t.Fatalf("purposed usage was not cloned: %#v", result.OtherUsage)
	}
}

func TestResultFromPreservesTypedJSONForInProcessConsumers(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "number", value: 7, want: `7`},
		{name: "null", value: nil, want: `null`},
		{name: "array", value: []int{1, 2}, want: `[1,2]`},
		{name: "object", value: map[string]any{"ok": true}, want: `{"ok":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ResultFrom(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Structured) != test.want || result.Content[0].Text != test.want {
				t.Fatalf("structured = %q, content = %q", result.Structured, result.Content[0].Text)
			}
			clone := result.Clone()
			clone.Structured[0] ^= 1
			if string(result.Structured) != test.want {
				t.Fatal("Clone() aliased structured JSON")
			}
		})
	}
	text, err := ResultFrom("plain")
	if err != nil || len(text.Structured) != 0 || text.Content[0].Text != "plain" {
		t.Fatalf("string result = %#v, %v", text, err)
	}
}
