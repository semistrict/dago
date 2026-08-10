// Package tool defines provider-neutral tool schemas and execution contracts.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/store"
)

var (
	ErrInvalidDefinition = errors.New("invalid tool definition")
	ErrInvalidArguments  = errors.New("invalid tool arguments")
)

// Definition is the model-visible description of a tool.
type Definition struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	InputSchema json.RawMessage            `json:"input_schema"`
	Strict      bool                       `json:"strict,omitempty"`
	Direct      bool                       `json:"direct_return,omitempty"`
	Extra       map[string]json.RawMessage `json:"extras,omitempty"`
}

// Validate checks the common restrictions required by model providers and the agent
// factory.
func (definition Definition) Validate() error {
	if strings.TrimSpace(definition.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidDefinition)
	}
	if strings.ContainsAny(definition.Name, " \t\r\n") {
		return fmt.Errorf("%w: name %q contains whitespace", ErrInvalidDefinition, definition.Name)
	}
	if strings.TrimSpace(definition.Description) == "" {
		return fmt.Errorf("%w: description is required", ErrInvalidDefinition)
	}
	if !json.Valid(definition.InputSchema) {
		return fmt.Errorf("%w: input schema is not valid JSON", ErrInvalidDefinition)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return fmt.Errorf("%w: input schema must be an object", ErrInvalidDefinition)
	}
	if rawType, ok := schema["type"]; ok {
		var schemaType string
		if err := json.Unmarshal(rawType, &schemaType); err != nil || schemaType != "object" {
			return fmt.Errorf("%w: input schema type must be object", ErrInvalidDefinition)
		}
	}
	for key, value := range definition.Extra {
		if key == "" || !json.Valid(value) {
			return fmt.Errorf("%w: extra %q is not valid JSON", ErrInvalidDefinition, key)
		}
	}
	return nil
}

// StateReader exposes immutable runtime state to tools without coupling the public
// tool package to the graph implementation.
type StateReader interface {
	Get(key string) (any, bool)
}

// StreamWriter emits custom tool progress events.
type StreamWriter interface {
	Write(ctx context.Context, value json.RawMessage) error
}

// Runtime contains values injected by the agent runtime and omitted from the model's
// JSON arguments.
type Runtime struct {
	CallID       string
	TaskID       string
	ThreadID     string
	Namespace    string
	CheckpointID string
	Resume       any
	State        StateReader
	Store        store.Store
	Stream       StreamWriter
	Context      any
}

// Interrupt pauses the containing agent before a tool result is committed.
// Resuming the agent re-enters the tool with Runtime.Resume populated.
type Interrupt struct {
	ID    string
	Value any
}

// Result is normalized tool output. Update is merged into graph state by reducers;
// it is not exposed to the model directly.
type Result struct {
	Content    []message.ContentBlock  `json:"content,omitempty"`
	Artifact   json.RawMessage         `json:"artifact,omitempty"`
	OtherUsage []message.PurposedUsage `json:"other_usage,omitempty"`
	Update     map[string]any          `json:"-"`
	Interrupt  *Interrupt              `json:"-"`
}

// TextResult creates a text-only tool result.
func TextResult(text string) Result {
	return Result{Content: []message.ContentBlock{{Type: message.BlockText, Text: text}}}
}

// Tool executes validated JSON arguments.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, arguments json.RawMessage, runtime Runtime) (Result, error)
}

// Func adapts a function to Tool.
type Func struct {
	Spec Definition
	Run  func(context.Context, json.RawMessage, Runtime) (Result, error)
}

func (function Func) Definition() Definition {
	definition := function.Spec
	definition.InputSchema = cloneRaw(definition.InputSchema)
	definition.Extra = cloneRawMap(definition.Extra)
	return definition
}

func (function Func) Execute(
	ctx context.Context,
	arguments json.RawMessage,
	runtime Runtime,
) (Result, error) {
	if function.Run == nil {
		return Result{}, fmt.Errorf("execute tool %q: implementation is nil", function.Spec.Name)
	}
	if !json.Valid(arguments) {
		return Result{}, fmt.Errorf("execute tool %q: %w", function.Spec.Name, ErrInvalidArguments)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	result, err := function.Run(ctx, cloneRaw(arguments), runtime)
	if err != nil {
		return Result{}, fmt.Errorf("execute tool %q: %w", function.Spec.Name, err)
	}
	return result.Clone(), nil
}

// Clone returns an isolated result.
func (result Result) Clone() Result {
	copy := result
	if result.Content != nil {
		copy.Content = append([]message.ContentBlock{}, result.Content...)
		for index := range copy.Content {
			blockMessage := message.Message{Content: []message.ContentBlock{copy.Content[index]}}
			copy.Content[index] = blockMessage.Clone().Content[0]
		}
	}
	copy.Artifact = cloneRaw(result.Artifact)
	if result.Interrupt != nil {
		interrupt := *result.Interrupt
		copy.Interrupt = &interrupt
	}
	copy.OtherUsage = append([]message.PurposedUsage{}, result.OtherUsage...)
	for index := range copy.OtherUsage {
		copy.OtherUsage[index].InputDetails = cloneMap(result.OtherUsage[index].InputDetails)
		copy.OtherUsage[index].OutputDetails = cloneMap(result.OtherUsage[index].OutputDetails)
	}
	if result.Update != nil {
		copy.Update = make(map[string]any, len(result.Update))
		for key, value := range result.Update {
			copy.Update[key] = value
		}
	}
	return copy
}

func cloneMap[K comparable, V any](values map[K]V) map[K]V {
	if values == nil {
		return nil
	}
	result := make(map[K]V, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	if value == nil {
		return nil
	}
	return append(json.RawMessage{}, value...)
}

func cloneRawMap(value map[string]json.RawMessage) map[string]json.RawMessage {
	if value == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(value))
	for key, item := range value {
		result[key] = cloneRaw(item)
	}
	return result
}
