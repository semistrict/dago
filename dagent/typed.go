package dagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"github.com/semistrict/dago/datool"
)

// ResumeAs decodes a live or checkpoint-restored resume value as T.
func ResumeAs[T any](runtime Runtime) (T, bool) { return decodeValue[T](runtime.Resume) }

// DepsAs returns typed application dependencies.
func DepsAs[T any](runtime Runtime) (T, bool) {
	typed, ok := runtime.Deps.(T)
	return typed, ok
}

// InterruptAs decodes a live or checkpoint-restored interrupt value as T.
func InterruptAs[T any](interrupt Interrupt) (T, bool) {
	return decodeValue[T](interrupt.Value)
}

// StructuredOutputFor derives a structured-output declaration from T. It
// panics when T cannot produce a schema, matching datool.MustNew for static
// declarations.
func StructuredOutputFor[T any](name, description string) *StructuredOutput {
	output, err := structuredOutputFor[T](name, description)
	if err != nil {
		panic(err)
	}
	return output
}

func structuredOutputFor[T any](name, description string) (*StructuredOutput, error) {
	schema, err := datool.Schema[T]()
	if err != nil {
		return nil, fmt.Errorf("structured output %q: %w", name, err)
	}
	return &StructuredOutput{
		Strategy: StructuredAuto, Name: name, Description: description,
		Schema: schema, Strict: true,
	}, nil
}

// StructuredAs decodes and validates a result's structured payload against the
// schema derived from T.
func StructuredAs[T any](result Result) (T, error) {
	var zero T
	declaration, err := structuredOutputFor[T]("result", "")
	if err != nil {
		return zero, err
	}
	prepared, err := prepareStructuredOutput(declaration)
	if err != nil {
		return zero, err
	}
	if err := validateStructured(prepared, result.Structured); err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(result.Structured))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("decode structured output: %w", err)
	}
	if err := requireStructuredEOF(decoder); err != nil {
		return zero, fmt.Errorf("decode structured output: %w", err)
	}
	return value, nil
}

func decodeValue[T any](value any) (T, bool) {
	var zero T
	if value == nil {
		return zero, false
	}
	if typed, ok := value.(T); ok {
		return typed, true
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return zero, false
	}
	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return zero, false
	}
	return decoded, true
}

func decodeFieldValue[T any](value any) (T, bool) {
	if value != nil {
		return decodeValue[T](value)
	}
	var zero T
	typeOfT := reflect.TypeFor[T]()
	if typeOfT == nil {
		return zero, true
	}
	switch typeOfT.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return zero, true
	default:
		return zero, false
	}
}

func requireStructuredEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
