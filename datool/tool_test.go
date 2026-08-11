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
	alias, err := Alias(target, "bash")
	if err != nil {
		t.Fatal(err)
	}
	definition := alias.Definition()
	if definition.Name != "bash" || !strings.Contains(string(definition.InputSchema), "command") {
		t.Fatalf("alias definition = %#v", definition)
	}
	result, err := alias.Execute(context.Background(), json.RawMessage(`{"command":"pwd"}`), Runtime{CallID: "call-1"})
	if err != nil || result.Content[0].Text != `call-1:{"command":"pwd"}` {
		t.Fatalf("alias result = %#v, %v", result, err)
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
