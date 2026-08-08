package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// Structured creates a tool that decodes its model-visible arguments into T before
// invoking the handler. JSON Schema generation is intentionally explicit: callers
// provide the reviewed schema sent to models.
func Structured[T any](
	definition Definition,
	handler func(context.Context, T, Runtime) (Result, error),
) (Tool, error) {
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, fmt.Errorf("structured tool %q: handler is required", definition.Name)
	}
	return Func{
		Spec: definition,
		Run: func(ctx context.Context, raw json.RawMessage, runtime Runtime) (Result, error) {
			if len(raw) > 0 && raw[0] == '"' {
				var encoded string
				if err := json.Unmarshal(raw, &encoded); err != nil {
					return Result{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
				}
				raw = json.RawMessage(encoded)
			}
			var input T
			if err := json.Unmarshal(raw, &input); err != nil {
				return Result{}, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
			}
			return handler(ctx, input, runtime)
		},
	}, nil
}
