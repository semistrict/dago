// Package dagent implements the provider-neutral model/tool loop and middleware
// contracts required by deep agents.
package dagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/datool"
)

const (
	MessagesKey             = "messages"
	StructuredResponseKey   = "structured_response"
	toolDirectKey           = "__agent_tool_direct"
	structuredRetryKey      = "__agent_structured_retry"
	structuredRetryCountKey = "__agent_structured_retry_count"
	jumpToKey               = "__agent_jump_to"
)

var (
	ErrDuplicateMiddleware  = errors.New("duplicate middleware name")
	ErrDuplicateTool        = errors.New("duplicate tool name")
	ErrUnknownTool          = errors.New("unknown tool")
	ErrInvalidModelOutput   = errors.New("invalid model output")
	ErrConflictingUpdate    = errors.New("conflicting tool state update")
	ErrStructuredValidation = errors.New("structured output validation failed")
)

// Runtime is the immutable execution context exposed to middleware.
type Runtime struct {
	Deps         any
	Config       dacheckpoint.Config
	Configurable datool.Configurable
	Store        dastore.Store
	Cache        dacache.Cache
	Previous     dastate.Values
	TaskID       string
	Resume       any
	Writer       EventWriter
}

// EventWriter emits JSON-safe custom progress records.
type EventWriter interface {
	Write(context.Context, json.RawMessage) error
}

// Hook runs at a middleware lifecycle boundary.
type Hook func(context.Context, dastate.Values, Runtime) (dastate.Values, error)

// ModelRequest is mutable-by-copy input to a model wrapper. Messages exclude the
// system message so wrappers can compose prompts without searching message history.
type ModelRequest struct {
	Model          damodel.Chat
	Messages       []damessage.Message
	SystemMessage  *damessage.Message
	Tools          []datool.Tool
	ToolChoice     *damodel.ToolChoice
	ResponseFormat *damodel.ResponseFormat
	PromptCache    *damodel.PromptCache
	Reasoning      *damodel.Reasoning
	State          dastate.Values
	Runtime        Runtime
	// MessagesReadOnly marks an active-thread view that wrappers may scan and
	// forward without cloning. Wrappers must clone before editing messages.
	MessagesReadOnly bool
	// InvocationMetadata and InvocationTags describe the agent run for
	// middleware, tracing, and evaluation. They are never forwarded as provider
	// request parameters.
	InvocationMetadata map[string]json.RawMessage
	InvocationTags     []string
	// Metadata and Tags are explicit provider request values. Model wrappers may
	// set them when the selected provider supports those fields.
	Metadata map[string]json.RawMessage
	Tags     []string
}

func (request ModelRequest) Clone() ModelRequest {
	copy := request
	copy.Messages = cloneMessages(request.Messages)
	if request.SystemMessage != nil {
		messageCopy := request.SystemMessage.Clone()
		copy.SystemMessage = &messageCopy
	}
	copy.Tools = append([]datool.Tool(nil), request.Tools...)
	if request.ToolChoice != nil {
		choice := *request.ToolChoice
		copy.ToolChoice = &choice
	}
	if request.ResponseFormat != nil {
		format := *request.ResponseFormat
		format.Schema = append(json.RawMessage(nil), request.ResponseFormat.Schema...)
		copy.ResponseFormat = &format
	}
	if request.PromptCache != nil {
		cache := *request.PromptCache
		copy.PromptCache = &cache
	}
	if request.Reasoning != nil {
		reasoning := *request.Reasoning
		copy.Reasoning = &reasoning
	}
	copy.State = request.State.Clone()
	copy.InvocationMetadata = cloneRawMap(request.InvocationMetadata)
	copy.InvocationTags = append([]string(nil), request.InvocationTags...)
	copy.Metadata = cloneRawMap(request.Metadata)
	copy.Tags = append([]string(nil), request.Tags...)
	return copy
}

// ModelResponse is the normalized response seen by model wrappers.
type ModelResponse struct {
	Messages   []damessage.Message
	Structured json.RawMessage
	Update     dastate.Values
}

type ModelHandler func(context.Context, ModelRequest) (ModelResponse, error)
type ModelWrapper func(context.Context, ModelRequest, ModelHandler) (ModelResponse, error)

// ToolCallRequest contains one executable model tool call.
type ToolCallRequest struct {
	Call    damessage.ToolCall
	Tool    datool.Tool
	State   dastate.Values
	Runtime Runtime
}

type ToolCallResponse struct {
	Result datool.Result
	Call   *damessage.ToolCall
}

type ToolHandler func(context.Context, ToolCallRequest) (ToolCallResponse, error)
type ToolWrapper func(context.Context, ToolCallRequest, ToolHandler) (ToolCallResponse, error)

// ToolBatchRequest is evaluated before any tool in a model response executes. It is
// the safe boundary for approval middleware because no sibling call has run yet.
type ToolBatchRequest struct {
	Calls   []damessage.ToolCall
	Tools   map[string]datool.Tool
	State   dastate.Values
	Runtime Runtime
}

// ToolBatchResponse may edit calls, synthesize rejected-call messages, or interrupt
// the graph. Calls omitted from the response keep their original value.
type ToolBatchResponse struct {
	Calls          []damessage.ToolCall
	Messages       []damessage.Message
	Interrupt      *Interrupt
	ResumeConsumed bool
}

type ToolBatchHook func(context.Context, ToolBatchRequest) (ToolBatchResponse, error)

// FieldKind describes a middleware-owned state field.
type FieldKind string

const (
	FieldLast      FieldKind = "last"
	FieldAggregate FieldKind = "aggregate"
	FieldDelta     FieldKind = "delta"
	FieldEphemeral FieldKind = "ephemeral"
)

// StateField is a public state reducer declaration. Contract must identify reducer
// semantics so independently declared fields can be checked for compatibility.
type StateField struct {
	Kind              FieldKind
	Contract          string
	Private           bool
	Initial           func() any
	Reduce            func(any, []any) (any, error)
	Clone             func(any) any
	SnapshotFrequency uint64
}

// FieldSpec is the typed construction form of StateField. Field erases T only
// after installing the reducer and cloner adapters required by the graph.
type FieldSpec[T any] struct {
	Kind              FieldKind
	Contract          string
	Private           bool
	Initial           func() T
	Reduce            func(T, []T) (T, error)
	Clone             func(T) T
	SnapshotFrequency uint64
}

// Field constructs an erased state-field declaration from typed functions.
func Field[T any](spec FieldSpec[T]) StateField {
	field := StateField{
		Kind: spec.Kind, Contract: spec.Contract, Private: spec.Private,
		SnapshotFrequency: spec.SnapshotFrequency,
	}
	if spec.Initial != nil {
		field.Initial = func() any { return spec.Initial() }
	}
	if spec.Reduce != nil {
		field.Reduce = func(current any, updates []any) (any, error) {
			typedCurrent, ok := decodeFieldValue[T](current)
			if !ok {
				return nil, fmt.Errorf("state field %q current value has type %T", spec.Contract, current)
			}
			typedUpdates := make([]T, len(updates))
			for index, update := range updates {
				var decoded bool
				typedUpdates[index], decoded = decodeFieldValue[T](update)
				if !decoded {
					return nil, fmt.Errorf("state field %q update %d has type %T", spec.Contract, index, update)
				}
			}
			return spec.Reduce(typedCurrent, typedUpdates)
		}
	}
	if spec.Clone != nil {
		field.Clone = func(value any) any {
			typed, ok := decodeFieldValue[T](value)
			if !ok {
				panic(fmt.Sprintf("state field %q clone value has type %T", spec.Contract, value))
			}
			return spec.Clone(typed)
		}
	}
	return field
}

func (field StateField) validate(name string) error {
	if field.Contract == "" || field.Clone == nil {
		return fmt.Errorf("agent state field %q requires contract and cloner", name)
	}
	switch field.Kind {
	case FieldLast, FieldEphemeral:
		return nil
	case FieldAggregate, FieldDelta:
		if field.Initial == nil || field.Reduce == nil {
			return fmt.Errorf("agent state field %q requires initial value and reducer", name)
		}
		if field.Kind == FieldDelta && field.SnapshotFrequency == 0 {
			return fmt.Errorf("agent delta state field %q requires snapshot frequency", name)
		}
		return nil
	default:
		return fmt.Errorf("agent state field %q has unsupported kind %q", name, field.Kind)
	}
}

// Middleware is a declarative collection of lifecycle hooks. The first registered
// wrapper is outermost; before hooks run in registration order and after hooks run
// in reverse order.
type Middleware struct {
	Name           string
	SerializedName string
	Fields         map[string]StateField
	Tools          []datool.Tool
	BeforeAgent    Hook
	BeforeModel    Hook
	WrapModelCall  ModelWrapper
	AfterModel     Hook
	BeforeTools    ToolBatchHook
	WrapToolCall   ToolWrapper
	AfterAgent     Hook
}

type StructuredStrategy string

const (
	StructuredAuto     StructuredStrategy = "auto"
	StructuredProvider StructuredStrategy = "provider"
	StructuredTool     StructuredStrategy = "tool"
)

// StructuredOutput configures provider-native or synthetic-tool structured output.
type StructuredOutput struct {
	Strategy     StructuredStrategy
	Name         string
	Description  string
	Schema       json.RawMessage
	Strict       bool
	HandleErrors bool
	// MaxRetries bounds correction attempts after an invalid structured response.
	// Zero preserves the caller's existing graph-level execution bound.
	MaxRetries         int
	ToolMessageContent string
	compiled           *jsonschema.Schema
}

func prepareStructuredOutput(output *StructuredOutput) (*StructuredOutput, error) {
	if output == nil {
		return nil, nil
	}
	if output.Name == "" || !json.Valid(output.Schema) {
		return nil, fmt.Errorf("structured output requires a name and valid JSON schema")
	}
	if output.MaxRetries < 0 {
		return nil, fmt.Errorf("structured output retries cannot be negative")
	}
	switch output.Strategy {
	case "", StructuredAuto, StructuredProvider, StructuredTool:
	default:
		return nil, fmt.Errorf("unsupported structured output strategy %q", output.Strategy)
	}
	var document any
	if err := json.Unmarshal(output.Schema, &document); err != nil {
		return nil, fmt.Errorf("compile structured output schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectSchemaLoader{})
	const location = "urn:dago:structured-output"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, fmt.Errorf("compile structured output schema: %w", err)
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("compile structured output schema: %w", err)
	}
	copy := *output
	copy.Schema = append(json.RawMessage(nil), output.Schema...)
	copy.compiled = compiled
	return &copy, nil
}

type rejectSchemaLoader struct{}

func (rejectSchemaLoader) Load(location string) (any, error) {
	return nil, fmt.Errorf("external JSON Schema resource %q is not allowed", location)
}

func cloneMessages(values []damessage.Message) []damessage.Message {
	result := make([]damessage.Message, len(values))
	for index := range values {
		result[index] = values[index].Clone()
	}
	return result
}

func cloneRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		result[key] = append(json.RawMessage(nil), value...)
	}
	return result
}
