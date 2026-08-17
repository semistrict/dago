// Package datool defines provider-neutral tool schemas and execution contracts.
package datool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/dastore"
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

// StateAs reads one typed state value. It accepts both its live Go form and the
// plain-data form produced by a checkpoint round trip.
func StateAs[T any](state StateReader, key string) (T, bool) {
	var zero T
	if state == nil {
		return zero, false
	}
	value, ok := state.Get(key)
	if !ok {
		return zero, false
	}
	return decodeRuntimeValue[T](value)
}

// StreamWriter emits custom tool progress events.
type StreamWriter interface {
	Write(ctx context.Context, value json.RawMessage) error
}

// Progress is a provider-neutral tool lifecycle update. Status is empty for
// intermediate output and set for the terminal success or error update.
type Progress struct {
	CallID string               `json:"call_id"`
	Name   string               `json:"name"`
	Output string               `json:"output"`
	Status damessage.ToolStatus `json:"status,omitempty"`
}

type progressEnvelope struct {
	Version  int      `json:"version"`
	Kind     string   `json:"kind"`
	Progress Progress `json:"tool_progress"`
}

// EmitProgress writes a typed tool-progress event through the current runtime.
func EmitProgress(ctx context.Context, progress Progress) error {
	runtime, ok := RuntimeFromContext(ctx)
	if !ok || runtime.Stream == nil {
		return nil
	}
	if progress.CallID == "" {
		progress.CallID = runtime.CallID
	}
	encoded, err := json.Marshal(progressEnvelope{Version: 1, Kind: "tool_progress", Progress: progress})
	if err != nil {
		return fmt.Errorf("encode tool progress: %w", err)
	}
	return runtime.Stream.Write(ctx, encoded)
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
	Store        dastore.Store
	Stream       StreamWriter
	Deps         any
	// Configurable contains immutable, runtime-only invocation settings supplied
	// by the application.
	Configurable Configurable
}

// Configurable is a defensive, read-only view of invocation settings.
type Configurable struct{ values map[string]any }

// NewConfigurable builds an isolated settings view. Container values are
// recursively copied while typed descriptors remain typed.
func NewConfigurable(values map[string]any) Configurable {
	return Configurable{values: cloneConfigurableMap(values)}
}

// Get returns an isolated copy of one setting.
func (config Configurable) Get(key string) (any, bool) {
	value, ok := config.values[key]
	return cloneConfigurableValue(value), ok
}

// Snapshot returns an isolated map suitable for forwarding into a nested
// invocation. Mutating the returned values cannot affect the source runtime.
func (config Configurable) Snapshot() map[string]any {
	return cloneConfigurableMap(config.values)
}

func cloneConfigurableMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneConfigurableValue(value)
	}
	return result
}

func cloneConfigurableValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneConfigurableMap(typed)
	case []any:
		result := make([]any, len(typed))
		for index := range typed {
			result[index] = cloneConfigurableValue(typed[index])
		}
		return result
	case map[string]string:
		result := make(map[string]string, len(typed))
		for key, item := range typed {
			result[key] = item
		}
		return result
	case []string:
		return append([]string(nil), typed...)
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	default:
		return value
	}
}

// ResumeAs decodes a live or checkpoint-restored resume value as T.
func ResumeAs[T any](runtime Runtime) (T, bool) { return decodeRuntimeValue[T](runtime.Resume) }

// DepsAs returns typed application dependencies.
func DepsAs[T any](runtime Runtime) (T, bool) {
	typed, ok := runtime.Deps.(T)
	return typed, ok
}

// Interrupt pauses the containing agent before a tool result is committed.
// Resuming the agent re-enters the tool with Runtime.Resume populated.
type Interrupt struct {
	ID    string
	Value any
}

// InterruptAs decodes a live or checkpoint-restored interrupt value as T.
func InterruptAs[T any](interrupt Interrupt) (T, bool) {
	return decodeRuntimeValue[T](interrupt.Value)
}

func decodeRuntimeValue[T any](value any) (T, bool) {
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

// Handoff asks the enclosing orchestrator to continue at Destination after the
// current agent commits the tool result and state update. This is the Go-native
// equivalent of a parent-graph command; the agent terminates and returns the
// handoff instead of trying to resolve a node that belongs to its caller.
type Handoff struct {
	Destination string `json:"destination"`
}

// Result is normalized tool output. Update is merged into graph state by reducers;
// it is not exposed to the model directly.
type Result struct {
	Content []damessage.ContentBlock `json:"content,omitempty"`
	// Structured preserves the JSON representation of a typed handler return for
	// trusted in-process consumers such as the JavaScript PTC bridge. It is not
	// serialized into model-visible tool results.
	Structured json.RawMessage `json:"-"`
	// Status marks model-visible tool failures that still carry useful content,
	// such as partial search results. Empty defaults to success.
	Status     damessage.ToolStatus      `json:"status,omitempty"`
	Artifact   json.RawMessage           `json:"artifact,omitempty"`
	OtherUsage []damessage.PurposedUsage `json:"other_usage,omitempty"`
	Update     map[string]any            `json:"-"`
	Interrupt  *Interrupt                `json:"-"`
	Handoff    *Handoff                  `json:"-"`
}

// TextResult creates a text-only tool result.
func TextResult(text string) Result {
	return Result{Content: []damessage.ContentBlock{{Type: damessage.BlockText, Text: text}}}
}

// ResultFrom normalizes a typed tool handler's return value. Result values pass
// through unchanged, strings become text content, and all other values are
// JSON-encoded as text content.
func ResultFrom(value any) (Result, error) {
	if result, ok := value.(Result); ok {
		return result, nil
	}
	if result, ok := value.(*Result); ok {
		if result == nil {
			return TextResult("null"), nil
		}
		return *result, nil
	}
	if text, ok := value.(string); ok {
		return TextResult(text), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return Result{}, fmt.Errorf("convert tool result to JSON: %w", err)
	}
	result := TextResult(string(encoded))
	result.Structured = append(json.RawMessage(nil), encoded...)
	return result, nil
}

// Tool executes validated JSON arguments.
type Tool interface {
	Definition() Definition
	Execute(ctx context.Context, arguments json.RawMessage, runtime Runtime) (Result, error)
}

// Alias exposes an existing tool under another model-visible name while
// preserving its schema, execution authority, runtime injection, and result
// semantics. It is useful at application boundaries that are migrating a
// stable tool name onto a canonical implementation.
func Alias(target Tool, name string) (Tool, error) {
	if target == nil {
		return nil, fmt.Errorf("alias tool: target is required")
	}
	definition := target.Definition()
	definition.Name = name
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("alias tool: %w", err)
	}
	return Func{Spec: definition, Run: func(ctx context.Context, arguments json.RawMessage, runtime Runtime) (Result, error) {
		return target.Execute(ctx, arguments, runtime)
	}}, nil
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
		copy.Content = append([]damessage.ContentBlock{}, result.Content...)
		for index := range copy.Content {
			blockMessage := damessage.Message{Content: []damessage.ContentBlock{copy.Content[index]}}
			copy.Content[index] = blockMessage.Clone().Content[0]
		}
	}
	copy.Artifact = cloneRaw(result.Artifact)
	copy.Structured = cloneRaw(result.Structured)
	if result.Interrupt != nil {
		interrupt := *result.Interrupt
		copy.Interrupt = &interrupt
	}
	copy.OtherUsage = append([]damessage.PurposedUsage{}, result.OtherUsage...)
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
