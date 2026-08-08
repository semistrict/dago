package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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
}

func TestStructuredDecodesAndInjectsRuntime(t *testing.T) {
	type input struct {
		Key string `json:"key"`
	}
	definition := Definition{
		Name:        "lookup",
		Description: "Look up a value.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	created, err := Structured(definition, func(
		ctx context.Context,
		input input,
		runtime Runtime,
	) (Result, error) {
		return TextResult(input.Key + ":" + runtime.CallID), nil
	})
	if err != nil {
		t.Fatalf("Structured() error = %v", err)
	}

	result, err := created.Execute(
		context.Background(),
		json.RawMessage(`{"key":"answer"}`),
		Runtime{CallID: "call-1"},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := result.Content[0].Text; got != "answer:call-1" {
		t.Fatalf("result text = %q", got)
	}
	legacy, err := created.Execute(
		context.Background(),
		json.RawMessage(`"{\"key\":\"legacy\"}"`),
		Runtime{CallID: "call-2"},
	)
	if err != nil || legacy.Content[0].Text != "legacy:call-2" {
		t.Fatalf("legacy result = %#v, %v", legacy, err)
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
