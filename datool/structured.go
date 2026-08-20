package datool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// Handler is the type-safe implementation of a generated tool.
type Handler[T, R any] func(context.Context, T) (R, error)

// TransformSchema rewrites a generated tool input schema.
type TransformSchema func(json.RawMessage) (json.RawMessage, error)

type newOptions struct {
	transforms []TransformSchema
	strict     bool
	direct     bool
	extra      map[string]json.RawMessage
}

// Option customizes a generated typed tool.
type Option interface {
	apply(*newOptions) error
}

type optionFunc func(*newOptions) error

func (option optionFunc) apply(options *newOptions) error {
	return option(options)
}

// WithTransformSchema customizes the model-visible schema after generation.
// Runtime argument validation remains derived from T and its struct tags, so
// dynamic provider hints do not bypass a handler's domain-specific validation.
// Multiple transforms run in option order.
func WithTransformSchema(transform TransformSchema) Option {
	return optionFunc(func(options *newOptions) error {
		if transform == nil {
			return fmt.Errorf("schema transform is required")
		}
		options.transforms = append(options.transforms, transform)
		return nil
	})
}

// WithStrict marks the generated tool definition as provider-strict.
func WithStrict() Option {
	return optionFunc(func(options *newOptions) error {
		options.strict = true
		return nil
	})
}

// WithDirect makes the typed tool result terminate the agent invocation.
func WithDirect() Option {
	return optionFunc(func(options *newOptions) error {
		options.direct = true
		return nil
	})
}

// WithExtra attaches one provider-specific definition value.
func WithExtra(key string, value any) Option {
	return optionFunc(func(options *newOptions) error {
		if key == "" {
			return fmt.Errorf("extra key is required")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode extra %q: %w", key, err)
		}
		if options.extra == nil {
			options.extra = map[string]json.RawMessage{}
		}
		options.extra[key] = encoded
		return nil
	})
}

type runtimeContextKey struct{}

// RuntimeFromContext returns the runtime attached while a typed tool handler is
// executing. The boolean is false when ctx did not originate from a tool call.
func RuntimeFromContext(ctx context.Context) (Runtime, bool) {
	runtime, ok := ctx.Value(runtimeContextKey{}).(Runtime)
	return runtime, ok
}

// New creates a Tool whose input schema and decoder are derived from T. It
// panics when the static declaration cannot produce a valid tool.
func New[T, R any](name, description string, handler Handler[T, R], options ...Option) Tool {
	created, err := newTypedTool(name, description, handler, options...)
	if err != nil {
		panic(err)
	}
	return created
}

func newTypedTool[T, R any](name, description string, handler Handler[T, R], options ...Option) (Tool, error) {
	definition := Definition{Name: name, Description: description}
	schema, err := Schema[T]()
	if err != nil {
		return nil, fmt.Errorf("typed tool %q: %w", definition.Name, err)
	}
	validationDefinition := definition
	validationDefinition.InputSchema = schema
	compiled, err := compileInputSchema(validationDefinition)
	if err != nil {
		return nil, err
	}
	configured := newOptions{}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("typed tool %q: option %d is nil", definition.Name, index)
		}
		if err := option.apply(&configured); err != nil {
			return nil, fmt.Errorf("typed tool %q: option %d: %w", definition.Name, index, err)
		}
	}
	for index, transform := range configured.transforms {
		schema, err = transform(cloneRaw(schema))
		if err != nil {
			return nil, fmt.Errorf("typed tool %q: transform schema %d: %w", definition.Name, index, err)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("typed tool %q: transform schema %d returned invalid JSON", definition.Name, index)
		}
	}
	definition.Strict = configured.strict
	definition.Direct = configured.direct
	definition.Extra = configured.extra
	definition.InputSchema = schema
	if _, err := compileInputSchema(definition); err != nil {
		return nil, err
	}
	return typed(definition, handler, compiled, true)
}

// MustNew is retained as an alias for New.
func MustNew[T, R any](name, description string, handler Handler[T, R], options ...Option) Tool {
	return New(name, description, handler, options...)
}

func typed[T, R any](definition Definition, handler Handler[T, R], compiled *jsonschema.Schema, strict bool) (Tool, error) {
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, fmt.Errorf("typed tool %q: handler is required", definition.Name)
	}
	return Func{
		Spec: definition,
		Run: func(ctx context.Context, raw json.RawMessage, runtime Runtime) (Result, error) {
			normalized, err := normalizeTypedArguments(raw)
			if err != nil {
				return Result{}, err
			}
			if compiled != nil {
				var document any
				decoder := json.NewDecoder(bytes.NewReader(normalized))
				decoder.UseNumber()
				if err := decoder.Decode(&document); err != nil {
					return Result{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
				}
				if err := compiled.Validate(document); err != nil {
					return Result{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
				}
			}
			var input T
			decoder := json.NewDecoder(bytes.NewReader(normalized))
			if strict {
				decoder.DisallowUnknownFields()
			}
			if err := decoder.Decode(&input); err != nil {
				return Result{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
			}
			if err := requireJSONEOF(decoder); err != nil {
				return Result{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
			}
			value, err := handler(context.WithValue(ctx, runtimeContextKey{}, runtime), input)
			if err != nil {
				return Result{}, err
			}
			return ResultFrom(value)
		},
	}, nil
}

func normalizeTypedArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
		}
		raw = json.RawMessage(encoded)
	}
	return raw, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func compileInputSchema(definition Definition) (*jsonschema.Schema, error) {
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	var document any
	if err := json.Unmarshal(definition.InputSchema, &document); err != nil {
		return nil, fmt.Errorf("typed tool %q: compile input schema: %w", definition.Name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const location = "urn:dago:tool-input"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, fmt.Errorf("typed tool %q: compile input schema: %w", definition.Name, err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("typed tool %q: compile input schema: %w", definition.Name, err)
	}
	return compiled, nil
}
