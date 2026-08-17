// Package daworkflow provides deterministic JavaScript orchestration as an
// optional dago middleware extension.
package daworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/dago/damessage"
)

var (
	// ErrUnavailable is returned on targets that cannot host the JavaScript runtime.
	ErrUnavailable = errors.New("workflow runtime is unavailable")
	// ErrBudgetExhausted is returned when a script attempts to start an agent after its token budget is spent.
	ErrBudgetExhausted = errors.New("workflow token budget exhausted")
)

// Meta is the runtime value of a workflow module's exported meta binding.
type Meta struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	WhenToUse   string  `json:"whenToUse,omitempty"`
	Phases      []Phase `json:"phases,omitempty"`
}

// Phase describes one model-visible progress group.
type Phase struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// AgentRequest is one deterministic agent call emitted by a script.
type AgentRequest struct {
	Prompt    string          `json:"prompt"`
	Label     string          `json:"label,omitempty"`
	Phase     string          `json:"phase,omitempty"`
	Model     string          `json:"model,omitempty"`
	Effort    string          `json:"effort,omitempty"`
	Isolation string          `json:"isolation,omitempty"`
	AgentType string          `json:"agent_type,omitempty"`
	Schema    json.RawMessage `json:"schema,omitempty"`
	// ReportTokens publishes the cumulative usage observed by a live agent call.
	// It is supplied by the runtime and is never persisted or exposed to scripts.
	ReportTokens func(int64) `json:"-"`
}

// AgentResponse is the JSON-safe value, token usage, and optional transcript
// returned by an agent. Transcript is persisted separately and never enters
// replay matching or script-visible values.
type AgentResponse struct {
	Value      any                 `json:"value"`
	Tokens     int64               `json:"tokens,omitempty"`
	Transcript []damessage.Message `json:"-"`
}

// AgentRunner executes one isolated workflow agent call.
type AgentRunner interface {
	RunWorkflowAgent(context.Context, AgentRequest) (AgentResponse, error)
}

// AgentFunc adapts a function to AgentRunner.
type AgentFunc func(context.Context, AgentRequest) (AgentResponse, error)

func (run AgentFunc) RunWorkflowAgent(ctx context.Context, request AgentRequest) (AgentResponse, error) {
	return run(ctx, request)
}

// ScriptResolver resolves saved and nested workflow names or paths.
type ScriptResolver interface {
	ResolveWorkflow(context.Context, string) (string, error)
}

// ResolverFunc adapts a function to ScriptResolver.
type ResolverFunc func(context.Context, string) (string, error)

func (resolve ResolverFunc) ResolveWorkflow(ctx context.Context, reference string) (string, error) {
	return resolve(ctx, reference)
}

// Event is a durable, JSON-safe progress record.
type Event struct {
	Version   int    `json:"version"`
	Sequence  int64  `json:"sequence"`
	Kind      string `json:"kind"`
	Phase     string `json:"phase,omitempty"`
	Label     string `json:"label,omitempty"`
	Message   string `json:"message,omitempty"`
	Call      int    `json:"call,omitempty"`
	Tokens    int64  `json:"tokens,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
}

// JournalEntry records the exact call identity and result used for same-session replay.
type JournalEntry struct {
	Version  int           `json:"version"`
	Call     int           `json:"call"`
	Key      string        `json:"key"`
	Request  AgentRequest  `json:"request"`
	Response AgentResponse `json:"response"`
}

// Options bounds one workflow execution.
type Options struct {
	MaxConcurrency int
	MaxAgents      int
	MaxItems       int
	SchemaRetries  int
	MemoryLimit    uint64
	StackLimit     uint64
	Timeout        time.Duration
	TokenBudget    int64
	Resolver       ScriptResolver
	Resume         []JournalEntry
	Progress       func(context.Context, Event) error
	// SessionDirectory optionally persists scripts, per-agent records, journals,
	// and final task output beneath a caller-owned session directory.
	SessionDirectory string
	// Completed receives an isolated terminal status so a host can publish its
	// native task-completion notification.
	Completed func(context.Context, Status)
}

// Result is the completed script value plus execution evidence.
type Result struct {
	Version    int            `json:"version"`
	Meta       Meta           `json:"meta"`
	Value      any            `json:"value"`
	Logs       []string       `json:"logs,omitempty"`
	Events     []Event        `json:"events,omitempty"`
	Journal    []JournalEntry `json:"journal,omitempty"`
	AgentCalls int            `json:"agent_calls"`
	Tokens     int64          `json:"tokens"`
}

// Runtime executes deterministic JavaScript orchestration scripts.
type Runtime struct {
	runner  AgentRunner
	options Options
}

// New constructs a bounded workflow runtime.
func New(runner AgentRunner, options Options) *Runtime {
	if runner == nil {
		panic("workflow agent runner is required")
	}
	if options.MaxConcurrency < 0 || options.MaxAgents < 0 || options.MaxItems < 0 || options.Timeout < 0 || options.TokenBudget < 0 {
		panic("workflow limits cannot be negative")
	}
	hardConcurrency := min(16, max(1, runtime.GOMAXPROCS(0)-2))
	if options.MaxConcurrency == 0 {
		options.MaxConcurrency = hardConcurrency
	} else if options.MaxConcurrency > hardConcurrency {
		options.MaxConcurrency = hardConcurrency
	}
	if options.MaxAgents == 0 {
		options.MaxAgents = 1000
	} else if options.MaxAgents > 1000 {
		options.MaxAgents = 1000
	}
	if options.MaxItems == 0 {
		options.MaxItems = 4096
	} else if options.MaxItems > 4096 {
		options.MaxItems = 4096
	}
	if options.SchemaRetries < 0 {
		panic("workflow schema retries cannot be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Minute
	}
	if options.MemoryLimit == 0 {
		options.MemoryLimit = 64 << 20
	}
	if options.StackLimit == 0 {
		options.StackLimit = 512 << 10
	}
	copy := options
	copy.Resume = append([]JournalEntry(nil), options.Resume...)
	return &Runtime{runner: runner, options: copy}
}

// Run validates and executes a workflow script with JSON-safe arguments.
func (workflow *Runtime) Run(ctx context.Context, script string, args any) (Result, error) {
	if workflow == nil || workflow.runner == nil {
		return Result{}, fmt.Errorf("%w: no agent runner", ErrUnavailable)
	}
	return runWorkflow(ctx, workflow.runner, script, normalizeWorkflowValue(args), workflow.options)
}

type workflowShared struct {
	runner AgentRunner
	opts   Options
	sem    chan struct{}

	mu         sync.Mutex
	agentCalls int
	tokens     int64
	eventSeq   int64
	events     []Event
	journal    map[int]JournalEntry
	resume     map[int]JournalEntry
	nesting    atomic.Int32
}

func newWorkflowShared(runner AgentRunner, options Options) *workflowShared {
	shared := &workflowShared{
		runner: runner, opts: options, sem: make(chan struct{}, options.MaxConcurrency),
		journal: map[int]JournalEntry{}, resume: map[int]JournalEntry{},
	}
	for _, entry := range options.Resume {
		shared.resume[entry.Call] = entry
	}
	return shared
}

func (shared *workflowShared) emit(ctx context.Context, event Event) error {
	shared.mu.Lock()
	event.Version = 1
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	shared.eventSeq++
	event.Sequence = shared.eventSeq
	shared.events = append(shared.events, event)
	shared.mu.Unlock()
	if shared.opts.Progress != nil {
		return shared.opts.Progress(ctx, event)
	}
	return nil
}

func (shared *workflowShared) snapshot() (events []Event, journal []JournalEntry, calls int, tokens int64) {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	events = append([]Event(nil), shared.events...)
	journal = make([]JournalEntry, 0, len(shared.journal))
	for _, entry := range shared.journal {
		journal = append(journal, entry)
	}
	sort.Slice(journal, func(i, j int) bool { return journal[i].Call < journal[j].Call })
	return events, journal, shared.agentCalls, shared.tokens
}

func normalizeWorkflowValue(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&normalized) != nil {
		return value
	}
	return normalizeWorkflowNumbers(normalized)
}

func normalizeWorkflowNumbers(value any) any {
	switch value := value.(type) {
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return integer
		}
		float, _ := value.Float64()
		return float
	case []any:
		for index := range value {
			value[index] = normalizeWorkflowNumbers(value[index])
		}
	case map[string]any:
		for key := range value {
			value[key] = normalizeWorkflowNumbers(value[key])
		}
	}
	return value
}
