// Package clbench adapts a caller-supplied structured agent to the
// continual-learning-bench system lifecycle.
//
// The package performs no model, network, process, credential, or host
// filesystem access. Factory and Agent are the authority boundary.
package clbench

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MemoryPath is the sole file loaded as durable strategy memory by the
	// pinned system adapter.
	MemoryPath = "/memory/AGENTS.md"
	// SeedMemory is the empty strategy scaffold restored by Reset.
	SeedMemory = "# Strategy notes\n\n(empty - update this as you learn)\n"
	// DefaultModel is the pinned adapter's default model label. The package
	// does not initialize that provider or read its credentials.
	DefaultModel = "anthropic:claude-sonnet-4-6"
	// DefaultName is the system identifier used in responses and artifacts.
	DefaultName = "deepagents"

	defaultTurnTimeout   = 10 * time.Minute
	defaultMaxTurns      = 10_000
	defaultMaxSchemas    = 128
	defaultMaxPrompt     = 1 << 20
	defaultMaxSchema     = 256 << 10
	defaultMaxAction     = 1 << 20
	defaultMaxFiles      = 256
	defaultMaxFile       = 1 << 20
	defaultMaxFilesTotal = 8 << 20
	defaultMaxUsage      = 1_000
	defaultMaxTokens     = 1_000_000_000
	defaultMaxError      = 4_096
	defaultMaxLabel      = 256
)

// SystemPrompt is supplied to every schema-specific Agent built by Factory.
// Memory is untrusted reference data and the agent, rather than a second
// extraction call, owns distillation into MemoryPath.
const SystemPrompt = `You are being evaluated on a continual-learning benchmark: a sequence of related instances in a shared environment. You are scored on how much you improve as you learn from earlier instances.

Your durable strategy lives in /memory/AGENTS.md, which is loaded into your context every turn. As you discover what works in this environment, keep that file up to date with the edit_file/write_file tools: record concise, generalizable lessons (tendencies to exploit, what worked, what to avoid) and prune anything you find to be wrong. It is the only thing that carries into the next instance, so invest in it. Never store secrets or credentials.

When you are given feedback on a previous action, use it to update your notes before you act again.`

var (
	// ErrTurnLimit reports that the configured interaction bound was reached.
	ErrTurnLimit = errors.New("clbench interaction limit reached")
	// ErrSchemaLimit reports that too many distinct response schemas were used.
	ErrSchemaLimit = errors.New("clbench response schema limit reached")
	// ErrPayloadTooLarge classifies bounded prompt, schema, action, file, usage,
	// and error payload rejection.
	ErrPayloadTooLarge = errors.New("clbench payload too large")
)

// Schema is one task-supplied structured response contract.
type Schema struct {
	Name     string
	Document json.RawMessage
}

// NewSchema constructs a required named JSON schema. Invalid static schema
// declarations panic rather than failing during a benchmark turn.
func NewSchema(name string, document json.RawMessage) Schema {
	name = strings.TrimSpace(name)
	if name == "" {
		panic("clbench schema name is required")
	}
	if !utf8.ValidString(name) {
		panic("clbench schema name must be UTF-8")
	}
	if !json.Valid(document) {
		panic("clbench schema must be valid JSON")
	}
	return Schema{Name: name, Document: append(json.RawMessage(nil), document...)}
}

// File is one in-state filesystem record threaded between turns.
type File struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// AgentConfig is immutable schema-specific construction input. A factory
// should configure native structured output and load MemorySources as
// untrusted memory on every invocation.
type AgentConfig struct {
	ResponseSchema Schema
	Model          string
	SystemPrompt   string
	MemorySources  []string
}

// TurnInput contains exactly the state visible to one agent interaction.
type TurnInput struct {
	Prompt string
	Files  map[string]File
}

// Usage is one model message's provider-reported token usage.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// TurnOutput is one native structured action and the resulting in-state
// filesystem. A nil Files map preserves the input filesystem; a non-nil empty
// map deliberately clears it.
type TurnOutput struct {
	Action json.RawMessage
	Files  map[string]File
	Usage  []Usage
}

// Agent performs one model interaction for a fixed response schema.
// Implementations must honor ctx and must not mutate TurnInput.
type Agent interface {
	Invoke(context.Context, TurnInput) (TurnOutput, error)
}

// AgentFunc adapts a function to Agent.
type AgentFunc func(context.Context, TurnInput) (TurnOutput, error)

// Invoke implements Agent.
func (invoke AgentFunc) Invoke(ctx context.Context, input TurnInput) (TurnOutput, error) {
	return invoke(ctx, input)
}

// Factory builds an Agent once per canonical response schema. Implementations
// own all provider selection and authentication.
type Factory interface {
	Build(context.Context, AgentConfig) (Agent, error)
}

// FactoryFunc adapts a function to Factory.
type FactoryFunc func(context.Context, AgentConfig) (Agent, error)

// Build implements Factory.
func (build FactoryFunc) Build(ctx context.Context, config AgentConfig) (Agent, error) {
	return build(ctx, config)
}

// Options controls local lifecycle and payload limits. Zero values select
// useful finite defaults.
type Options struct {
	Name               string
	Model              string
	TurnTimeout        time.Duration
	MaxTurns           int
	MaxSchemas         int
	MaxPromptBytes     int
	MaxSchemaBytes     int
	MaxActionBytes     int
	MaxFiles           int
	MaxFileBytes       int
	MaxFilesTotalBytes int
	MaxUsageRecords    int
	MaxTokens          int
	MaxErrorBytes      int
	MaxLabelBytes      int
}

// Query is one benchmark instance. Feedback, when nonblank, takes precedence
// over feedback retained from Observe.
type Query struct {
	Prompt         string
	Feedback       string
	ResponseSchema Schema
}

// NewQuery constructs a query from its optional prompt and required schema.
func NewQuery(prompt string, schema Schema) Query {
	validateStaticSchema(schema)
	return Query{Prompt: prompt, ResponseSchema: cloneSchema(schema)}
}

// Observation is the outcome of the preceding action.
type Observation struct {
	Content string
}

// Metadata is the stable per-step data expected by benchmark viewers.
type Metadata struct {
	System      string            `json:"system"`
	Model       string            `json:"model"`
	Interaction int               `json:"interaction"`
	MemoryFiles map[string]string `json:"memory_files"`
}

// Response is one structured benchmark action and its viewer metadata.
type Response struct {
	Action   json.RawMessage `json:"action"`
	Metadata Metadata        `json:"metadata"`
}

// UsageEvent is the aggregate token event recorded for one turn.
type UsageEvent struct {
	CallType     string `json:"call_type"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	TotalTokens  int    `json:"total_tokens"`
}

// Artifacts are the final system details consumed by a run viewer.
type Artifacts struct {
	ArtifactType     string            `json:"artifact_type"`
	Model            string            `json:"model"`
	InteractionCount int               `json:"interaction_count"`
	MemoryFiles      map[string]string `json:"memory_files"`
}

// System is one continual-learning rollout. Operations are serialized so an
// instance is race-safe; separate instances remain fully parallel-safe.
type System struct {
	factory Factory
	options Options

	mu              sync.Mutex
	agents          map[string]Agent
	files           map[string]File
	pendingFeedback string
	interactions    int
	usage           []UsageEvent
}

// New constructs a system around the required caller-supplied factory. It
// performs no model initialization or other I/O.
func New(factory Factory, options Options) *System {
	if isNil(factory) {
		panic("clbench agent factory is required")
	}
	options = withDefaults(options)
	return &System{
		factory: factory,
		options: options,
		agents:  make(map[string]Agent),
		files:   seedFiles(),
	}
}

// Name returns the configured benchmark system identifier.
func (system *System) Name() string { return system.options.Name }

// ParallelSafe reports the pinned in-state adapter property. It describes
// separate System instances, not concurrent turns within one rollout.
func (system *System) ParallelSafe() bool { return true }

// SupportsBaseline reports that Reset produces a genuinely stateless baseline.
func (system *System) SupportsBaseline() bool { return true }

// Respond performs exactly one agent invocation, threading validated in-state
// files into the next turn and aggregating provider-reported usage.
func (system *System) Respond(ctx context.Context, query Query) (Response, error) {
	system.mu.Lock()
	defer system.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if system.interactions >= system.options.MaxTurns {
		return Response{}, ErrTurnLimit
	}
	schema, cacheKey, err := system.validateSchema(query.ResponseSchema)
	if err != nil {
		return Response{}, err
	}
	if !utf8.ValidString(query.Prompt) || !utf8.ValidString(query.Feedback) {
		return Response{}, errors.New("clbench prompt and feedback must be UTF-8")
	}
	if len(query.Prompt) > system.options.MaxPromptBytes || len(query.Feedback) > system.options.MaxPromptBytes {
		return Response{}, ErrPayloadTooLarge
	}

	system.interactions++
	feedback := strings.TrimSpace(query.Feedback)
	if feedback == "" {
		feedback = system.pendingFeedback
	}
	system.pendingFeedback = ""
	prompt := query.Prompt
	if prompt == "" {
		prompt = "(no content)"
	}
	if feedback != "" {
		prompt = "Feedback on your previous action:\n" + feedback + "\n\n" + prompt
	}
	if len(prompt) > system.options.MaxPromptBytes {
		return Response{}, ErrPayloadTooLarge
	}

	turnCtx, cancel := context.WithTimeout(ctx, system.options.TurnTimeout)
	defer cancel()
	agent := system.agents[cacheKey]
	if agent == nil {
		if len(system.agents) >= system.options.MaxSchemas {
			return Response{}, ErrSchemaLimit
		}
		config := AgentConfig{
			ResponseSchema: schema,
			Model:          system.options.Model,
			SystemPrompt:   SystemPrompt,
			MemorySources:  []string{MemoryPath},
		}
		agent, err = invokeFactory(system.factory, turnCtx, config, system.options.MaxErrorBytes)
		if contextErr := turnCtx.Err(); contextErr != nil {
			return Response{}, contextErr
		}
		if err != nil {
			return Response{}, err
		}
		if isNil(agent) {
			return Response{}, errors.New("clbench agent factory returned nil")
		}
		system.agents[cacheKey] = agent
	}

	output, err := invokeAgent(agent, turnCtx, TurnInput{Prompt: prompt, Files: cloneFiles(system.files)}, system.options.MaxErrorBytes)
	if contextErr := turnCtx.Err(); contextErr != nil {
		return Response{}, contextErr
	}
	if err != nil {
		return Response{}, err
	}
	if err := system.validateAction(output.Action); err != nil {
		return Response{}, err
	}
	files := system.files
	if output.Files != nil {
		files, err = system.validateFiles(output.Files)
		if err != nil {
			return Response{}, err
		}
	}
	usage, err := system.aggregateUsage(output.Usage)
	if err != nil {
		return Response{}, err
	}

	system.files = files
	if usage != nil {
		system.usage = append(system.usage, *usage)
	}
	return Response{
		Action: append(json.RawMessage(nil), output.Action...),
		Metadata: Metadata{
			System:      DefaultName,
			Model:       system.options.Model,
			Interaction: system.interactions,
			MemoryFiles: memorySnapshot(system.files),
		},
	}, nil
}

// Observe retains nonblank outcome text for the next Respond call. It performs
// no model invocation and does not alter memory directly.
func (system *System) Observe(observation Observation) error {
	system.mu.Lock()
	defer system.mu.Unlock()
	if !utf8.ValidString(observation.Content) {
		return errors.New("clbench observation must be UTF-8")
	}
	if len(observation.Content) > system.options.MaxPromptBytes {
		return ErrPayloadTooLarge
	}
	if content := strings.TrimSpace(observation.Content); content != "" {
		system.pendingFeedback = content
	}
	return nil
}

// Reset wipes learned files, pending feedback, and interaction count. Schema-
// specific agents remain cached because they hold no learned state.
func (system *System) Reset() {
	system.mu.Lock()
	defer system.mu.Unlock()
	system.files = seedFiles()
	system.pendingFeedback = ""
	system.interactions = 0
}

// UsageEvents returns an owned snapshot of recorded completion usage. Like the
// pinned base system accounting, Reset does not discard run-level usage.
func (system *System) UsageEvents() []UsageEvent {
	system.mu.Lock()
	defer system.mu.Unlock()
	return append([]UsageEvent(nil), system.usage...)
}

// GetRunArtifacts returns final durable memory and interaction metadata.
func (system *System) GetRunArtifacts() Artifacts {
	system.mu.Lock()
	defer system.mu.Unlock()
	return Artifacts{
		ArtifactType:     DefaultName,
		Model:            system.options.Model,
		InteractionCount: system.interactions,
		MemoryFiles:      memorySnapshot(system.files),
	}
}

func (system *System) validateSchema(schema Schema) (Schema, string, error) {
	name := strings.TrimSpace(schema.Name)
	if name == "" || !utf8.ValidString(name) || !json.Valid(schema.Document) {
		return Schema{}, "", errors.New("clbench response schema requires a name and valid JSON")
	}
	if len(name) > system.options.MaxLabelBytes || len(schema.Document) > system.options.MaxSchemaBytes {
		return Schema{}, "", ErrPayloadTooLarge
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, schema.Document); err != nil {
		return Schema{}, "", errors.New("clbench response schema is invalid")
	}
	canonical := append(json.RawMessage(nil), compact.Bytes()...)
	return Schema{Name: name, Document: canonical}, name + "\x00" + string(canonical), nil
}

func (system *System) validateAction(action json.RawMessage) error {
	if len(action) == 0 {
		return errors.New("clbench agent did not return a structured object matching the response schema")
	}
	if len(action) > system.options.MaxActionBytes {
		return ErrPayloadTooLarge
	}
	trimmed := bytes.TrimSpace(action)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return errors.New("clbench agent did not return a structured object matching the response schema")
	}
	return nil
}

func (system *System) validateFiles(files map[string]File) (map[string]File, error) {
	if len(files) > system.options.MaxFiles {
		return nil, ErrPayloadTooLarge
	}
	result := make(map[string]File, len(files))
	total := 0
	for filePath, file := range files {
		if !validPath(filePath) {
			return nil, fmt.Errorf("clbench invalid in-state file path %q", truncate(filePath, system.options.MaxErrorBytes))
		}
		if len(file.Content) > system.options.MaxFileBytes {
			return nil, ErrPayloadTooLarge
		}
		switch file.Encoding {
		case "", "utf-8":
			if !utf8.ValidString(file.Content) {
				return nil, errors.New("clbench UTF-8 file contains invalid text")
			}
			file.Encoding = "utf-8"
		case "base64":
			if _, err := base64.StdEncoding.DecodeString(file.Content); err != nil {
				return nil, errors.New("clbench file contains invalid base64")
			}
		default:
			return nil, fmt.Errorf("clbench unsupported file encoding %q", truncate(file.Encoding, system.options.MaxErrorBytes))
		}
		remaining := system.options.MaxFilesTotalBytes - total
		if len(filePath) > remaining {
			return nil, ErrPayloadTooLarge
		}
		remaining -= len(filePath)
		if len(file.Content) > remaining {
			return nil, ErrPayloadTooLarge
		}
		total += len(filePath) + len(file.Content)
		result[filePath] = file
	}
	return result, nil
}

func (system *System) aggregateUsage(records []Usage) (*UsageEvent, error) {
	if len(records) == 0 {
		return nil, nil
	}
	if len(records) > system.options.MaxUsageRecords {
		return nil, ErrPayloadTooLarge
	}
	event := &UsageEvent{CallType: "completion", Model: system.options.Model}
	for _, usage := range records {
		if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.InputTokens > system.options.MaxTokens-event.InputTokens || usage.OutputTokens > system.options.MaxTokens-event.OutputTokens {
			return nil, ErrPayloadTooLarge
		}
		event.InputTokens += usage.InputTokens
		event.OutputTokens += usage.OutputTokens
	}
	if event.InputTokens > system.options.MaxTokens-event.OutputTokens {
		return nil, ErrPayloadTooLarge
	}
	event.TotalTokens = event.InputTokens + event.OutputTokens
	return event, nil
}

func withDefaults(options Options) Options {
	if options.Name == "" {
		options.Name = DefaultName
	}
	if options.Model == "" {
		options.Model = DefaultModel
	}
	values := []struct {
		value *int
		name  string
		def   int
	}{
		{&options.MaxTurns, "maximum turns", defaultMaxTurns},
		{&options.MaxSchemas, "maximum schemas", defaultMaxSchemas},
		{&options.MaxPromptBytes, "maximum prompt bytes", defaultMaxPrompt},
		{&options.MaxSchemaBytes, "maximum schema bytes", defaultMaxSchema},
		{&options.MaxActionBytes, "maximum action bytes", defaultMaxAction},
		{&options.MaxFiles, "maximum files", defaultMaxFiles},
		{&options.MaxFileBytes, "maximum file bytes", defaultMaxFile},
		{&options.MaxFilesTotalBytes, "maximum total file bytes", defaultMaxFilesTotal},
		{&options.MaxUsageRecords, "maximum usage records", defaultMaxUsage},
		{&options.MaxTokens, "maximum tokens", defaultMaxTokens},
		{&options.MaxErrorBytes, "maximum error bytes", defaultMaxError},
		{&options.MaxLabelBytes, "maximum label bytes", defaultMaxLabel},
	}
	for _, item := range values {
		if *item.value < 0 {
			panic("clbench " + item.name + " cannot be negative")
		}
		if *item.value == 0 {
			*item.value = item.def
		}
	}
	if options.TurnTimeout < 0 {
		panic("clbench turn timeout cannot be negative")
	}
	if options.TurnTimeout == 0 {
		options.TurnTimeout = defaultTurnTimeout
	}
	options.Name = strings.TrimSpace(options.Name)
	options.Model = strings.TrimSpace(options.Model)
	if options.Name == "" || options.Model == "" {
		panic("clbench name and model labels are required")
	}
	if !utf8.ValidString(options.Name) || !utf8.ValidString(options.Model) {
		panic("clbench name and model labels must be UTF-8")
	}
	if len(options.Name) > options.MaxLabelBytes || len(options.Model) > options.MaxLabelBytes {
		panic("clbench name or model label is too large")
	}
	return options
}

func validateStaticSchema(schema Schema) {
	if strings.TrimSpace(schema.Name) == "" || !utf8.ValidString(schema.Name) || !json.Valid(schema.Document) {
		panic("clbench response schema requires a name and valid JSON")
	}
}

func cloneSchema(schema Schema) Schema {
	return Schema{Name: schema.Name, Document: append(json.RawMessage(nil), schema.Document...)}
}

func seedFiles() map[string]File {
	return map[string]File{MemoryPath: {Content: SeedMemory, Encoding: "utf-8"}}
}

func cloneFiles(files map[string]File) map[string]File {
	if files == nil {
		return nil
	}
	result := make(map[string]File, len(files))
	for filePath, file := range files {
		result[filePath] = file
	}
	return result
}

func memorySnapshot(files map[string]File) map[string]string {
	return map[string]string{MemoryPath: files[MemoryPath].Content}
}

func validPath(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.HasPrefix(value, "/") && !strings.ContainsRune(value, 0) && path.Clean(value) == value
}

func invokeFactory(factory Factory, ctx context.Context, config AgentConfig, maxError int) (agent Agent, err error) {
	defer func() {
		if recover() != nil {
			agent = nil
			err = errors.New("clbench agent factory panicked")
		}
	}()
	agent, err = factory.Build(ctx, AgentConfig{
		ResponseSchema: cloneSchema(config.ResponseSchema),
		Model:          config.Model,
		SystemPrompt:   config.SystemPrompt,
		MemorySources:  append([]string(nil), config.MemorySources...),
	})
	if err != nil {
		return nil, wrappedError{prefix: "clbench build agent", cause: err, max: maxError}
	}
	return agent, nil
}

func invokeAgent(agent Agent, ctx context.Context, input TurnInput, maxError int) (output TurnOutput, err error) {
	defer func() {
		if recover() != nil {
			output = TurnOutput{}
			err = errors.New("clbench agent panicked")
		}
	}()
	output, err = agent.Invoke(ctx, input)
	if err != nil {
		return TurnOutput{}, wrappedError{prefix: "clbench agent turn", cause: err, max: maxError}
	}
	return output, nil
}

type wrappedError struct {
	prefix string
	cause  error
	max    int
}

func (err wrappedError) Error() string {
	return err.prefix + ": " + truncate(err.cause.Error(), err.max)
}
func (err wrappedError) Unwrap() error { return err.cause }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
