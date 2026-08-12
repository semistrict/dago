package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/semistrict/dago/dacache"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
)

const (
	Start = "__start__"
	End   = "__end__"
)

var (
	ErrRecursionLimit = errors.New("graph recursion limit reached")
	ErrInterrupted    = errors.New("graph interrupted")
	ErrUnknownNode    = errors.New("unknown graph node")
)

// Node performs one graph task.
type Node func(context.Context, dastate.Values, Runtime) (Command, error)

// Router chooses destinations after a node's state update has been applied.
type Router func(context.Context, dastate.Values) ([]string, error)

// Send schedules a node with an isolated state overlay.
type Send struct {
	Node  string
	Input dastate.Values
}

// Interrupt pauses execution until the same thread is invoked with a resume value.
type Interrupt struct {
	ID    string
	Value any
}

// Command is a node's state update and routing intent.
type Command struct {
	Update    dastate.Values
	Goto      []string
	Sends     []Send
	Interrupt *Interrupt
}

// Runtime contains task-scoped values.
type Runtime struct {
	Context  any
	Config   dacheckpoint.Config
	Store    dastore.Store
	Cache    dacache.Cache
	Previous dastate.Values
	TaskID   string
	Resume   any
	Writer   EventWriter
}

// RetryPolicy controls whole-node retries. Attempts includes the initial call.
// Retryable defaults to retrying every non-context error.
type RetryPolicy struct {
	Attempts  int
	Backoff   time.Duration
	Retryable func(error) bool
}

// EventMode identifies an execution stream event.
type EventMode string

const (
	EventTask      EventMode = "task"
	EventUpdate    EventMode = "update"
	EventValues    EventMode = "values"
	EventInterrupt EventMode = "interrupt"
	EventCustom    EventMode = "custom"
)

// Event is emitted in deterministic executor order.
type Event struct {
	Mode      EventMode
	Step      int
	Node      string
	TaskID    string
	Update    dastate.Values
	Values    dastate.Values
	Interrupt *Interrupt
	Custom    json.RawMessage
}

// EventWriter receives graph events. Implementations must honor context cancellation.
type EventWriter interface {
	Write(context.Context, Event) error
}

// Invocation starts or resumes one thread.
type Invocation struct {
	Config          dacheckpoint.Config
	State           dastate.Values
	Resume          any
	SkipValueEvents bool
}

// Execution is the terminal or paused graph result.
type Execution struct {
	Config     dacheckpoint.Config
	State      dastate.Values
	Interrupts []Interrupt
	Steps      int
}

// Snapshot is one reconstructed durable graph state without executing pending
// tasks. Next contains the nodes scheduled by the addressed checkpoint.
type Snapshot struct {
	Config   dacheckpoint.Config
	State    dastate.Values
	Metadata dacheckpoint.Metadata
	Next     []string
}

// CompileOptions configure one compiled graph.
type CompileOptions struct {
	Saver dacheckpoint.Saver
	// RetainThreadState keeps the reconstructed state machine for active threads.
	// It avoids replaying durable delta history on every invocation. Callers that
	// mutate checkpoints out of band must leave this disabled.
	RetainThreadState bool
	RecursionLimit    int
	MaxConcurrency    int
	Context           any
	Store             dastore.Store
	Cache             dacache.Cache
	Writer            EventWriter
	Retry             RetryPolicy
}

// Builder validates and compiles a state graph.
type Builder struct {
	schema      Schema
	nodes       map[string]Node
	edges       map[string][]string
	conditional map[string]Router
}

func NewBuilder(schema Schema) *Builder {
	return &Builder{
		schema: schema, nodes: make(map[string]Node), edges: make(map[string][]string),
		conditional: make(map[string]Router),
	}
}

func (builder *Builder) AddNode(name string, node Node) error {
	if name == "" || name == Start || name == End {
		return fmt.Errorf("add graph node: invalid name %q", name)
	}
	if node == nil {
		return fmt.Errorf("add graph node %q: implementation is nil", name)
	}
	if _, exists := builder.nodes[name]; exists {
		return fmt.Errorf("add graph node %q: duplicate", name)
	}
	builder.nodes[name] = node
	return nil
}

func (builder *Builder) AddEdge(from, to string) error {
	if from == End || to == Start || from == "" || to == "" {
		return fmt.Errorf("add graph edge %q -> %q: invalid endpoint", from, to)
	}
	builder.edges[from] = append(builder.edges[from], to)
	return nil
}

func (builder *Builder) AddConditional(name string, router Router) error {
	if name == "" || name == Start || name == End || router == nil {
		return fmt.Errorf("add conditional route for %q: invalid route", name)
	}
	if _, exists := builder.conditional[name]; exists {
		return fmt.Errorf("add conditional route for %q: duplicate", name)
	}
	builder.conditional[name] = router
	return nil
}

func (builder *Builder) Compile(options CompileOptions) (*Compiled, error) {
	if err := builder.schema.Validate(); err != nil {
		return nil, err
	}
	if len(builder.edges[Start]) == 0 {
		return nil, fmt.Errorf("compile graph: start has no outgoing edge")
	}
	for from, destinations := range builder.edges {
		if from != Start {
			if _, exists := builder.nodes[from]; !exists {
				return nil, fmt.Errorf("compile graph: edge source %q: %w", from, ErrUnknownNode)
			}
		}
		for _, destination := range destinations {
			if destination == End {
				continue
			}
			if _, exists := builder.nodes[destination]; !exists {
				return nil, fmt.Errorf("compile graph: edge destination %q: %w", destination, ErrUnknownNode)
			}
		}
	}
	for name := range builder.conditional {
		if _, exists := builder.nodes[name]; !exists {
			return nil, fmt.Errorf("compile graph: conditional source %q: %w", name, ErrUnknownNode)
		}
	}
	if options.RecursionLimit <= 0 {
		options.RecursionLimit = 100
	}
	if options.MaxConcurrency <= 0 {
		options.MaxConcurrency = 16
	}
	if options.Retry.Attempts <= 0 {
		options.Retry.Attempts = 1
	}
	compiled := &Compiled{
		schema: builder.schema, nodes: cloneNodes(builder.nodes), edges: cloneEdges(builder.edges),
		conditional: cloneRouters(builder.conditional), options: options,
	}
	if options.RetainThreadState {
		compiled.sessions = &threadSessions{values: make(map[threadKey]*threadSession)}
	}
	return compiled, nil
}

type Compiled struct {
	schema      Schema
	nodes       map[string]Node
	edges       map[string][]string
	conditional map[string]Router
	options     CompileOptions
	sessions    *threadSessions
}

type threadKey struct {
	threadID  string
	namespace string
}

type threadSessions struct {
	mu     sync.Mutex
	values map[threadKey]*threadSession
}

type threadSession struct {
	mu       sync.Mutex
	valid    bool
	machine  *stateMachine
	config   dacheckpoint.Config
	tasks    []task
	metadata dacheckpoint.Metadata
}

func (graph *Compiled) threadSession(config dacheckpoint.Config) *threadSession {
	if graph.sessions == nil {
		return nil
	}
	key := threadKey{threadID: config.ThreadID, namespace: config.Namespace}
	graph.sessions.mu.Lock()
	defer graph.sessions.mu.Unlock()
	if session := graph.sessions.values[key]; session != nil {
		return session
	}
	session := &threadSession{}
	graph.sessions.values[key] = session
	return session
}

type task struct {
	node  string
	input dastate.Values
	id    string
	path  string
}

type taskResult struct {
	task    task
	command Command
	err     error
}

func cloneNodes(values map[string]Node) map[string]Node {
	result := make(map[string]Node, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneEdges(values map[string][]string) map[string][]string {
	result := make(map[string][]string, len(values))
	for key, value := range values {
		result[key] = append([]string(nil), value...)
	}
	return result
}

func cloneRouters(values map[string]Router) map[string]Router {
	result := make(map[string]Router, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func normalizeTasks(tasks []task) []task {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].path != tasks[j].path {
			return tasks[i].path < tasks[j].path
		}
		return tasks[i].node < tasks[j].node
	})
	for index := range tasks {
		tasks[index].id = fmt.Sprintf("%08d:%s", index, tasks[index].node)
		if tasks[index].path == "" {
			tasks[index].path = fmt.Sprintf("%08d", index)
		}
	}
	return tasks
}

func (graph *Compiled) executeTasks(
	ctx context.Context,
	machine *stateMachine,
	tasks []task,
	config dacheckpoint.Config,
	resume any,
	step int,
) []taskResult {
	results := make([]taskResult, len(tasks))
	semaphore := make(chan struct{}, graph.options.MaxConcurrency)
	var wait sync.WaitGroup
	for index := range tasks {
		index := index
		if graph.options.Writer != nil {
			if err := graph.options.Writer.Write(ctx, Event{
				Mode: EventTask, Step: step, Node: tasks[index].node, TaskID: tasks[index].id,
			}); err != nil {
				results[index] = taskResult{task: tasks[index], err: err}
				continue
			}
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index] = taskResult{task: tasks[index], err: context.Cause(ctx)}
				return
			}
			values, err := machine.values()
			if err != nil {
				results[index] = taskResult{task: tasks[index], err: err}
				return
			}
			previous := values.Clone()
			for key, value := range tasks[index].input {
				values[key] = value
			}
			runtime := Runtime{
				Context: graph.options.Context, Config: config,
				Store: graph.options.Store, Cache: graph.options.Cache,
				Previous: previous, TaskID: tasks[index].id, Resume: resume,
				Writer: graph.options.Writer,
			}
			command, err := graph.callNodeWithRetry(ctx, graph.nodes[tasks[index].node], values, runtime)
			results[index] = taskResult{task: tasks[index], command: command, err: err}
		}()
	}
	wait.Wait()
	return results
}

func (graph *Compiled) callNodeWithRetry(
	ctx context.Context,
	node Node,
	values dastate.Values,
	runtime Runtime,
) (Command, error) {
	var last error
	for attempt := 0; attempt < graph.options.Retry.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Command{}, err
		}
		command, err := callNode(ctx, node, values.Clone(), runtime)
		if err == nil {
			return command, nil
		}
		last = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			(graph.options.Retry.Retryable != nil && !graph.options.Retry.Retryable(err)) ||
			attempt+1 >= graph.options.Retry.Attempts {
			break
		}
		if graph.options.Retry.Backoff > 0 {
			timer := time.NewTimer(graph.options.Retry.Backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return Command{}, context.Cause(ctx)
			}
		}
	}
	return Command{}, last
}

func callNode(ctx context.Context, node Node, values dastate.Values, runtime Runtime) (
	command Command,
	err error,
) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("graph node panic: %v", recovered)
		}
	}()
	return node(ctx, values, runtime)
}
