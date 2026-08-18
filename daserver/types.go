// Package daserver exposes dago agents through the LangGraph Agent Server HTTP
// protocol used by LangSmith Studio and the LangGraph SDKs.
package daserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
)

// Graph is the execution and durable-state surface required by Agent Server.
// *dagent.Agent satisfies this interface.
type Graph interface {
	Stream(context.Context, dagent.Input) *dagent.Stream
	State(context.Context, dacheckpoint.Config) (dagent.Snapshot, error)
	UpdateState(context.Context, dacheckpoint.Config, dastate.Values) (dagent.Snapshot, error)
	History(context.Context, dacheckpoint.Config, dacheckpoint.ListOptions) ([]dacheckpoint.Tuple, error)
	Fork(context.Context, string, string) error
	DeleteThread(context.Context, string) error
}

// Runtime contains server-owned dependencies and assistant configuration passed
// to a graph factory. Factories must use Saver for thread state so Studio state,
// history, interrupts, and replay address the same checkpoints as runs.
type Runtime struct {
	Saver  dacheckpoint.Saver
	Store  dastore.Store
	Config map[string]any
	Deps   any
}

// Factory constructs a graph for one assistant configuration.
type Factory func(context.Context, Runtime) (Graph, error)

// AdaptFactory preserves a factory's concrete graph return type while adapting
// it to the server contract.
func AdaptFactory[T Graph](factory func(context.Context, Runtime) (T, error)) Factory {
	if factory == nil {
		panic("Agent Server graph factory is required")
	}
	return func(ctx context.Context, runtime Runtime) (Graph, error) {
		graph, err := factory(ctx, runtime)
		if err != nil {
			return nil, err
		}
		if nilDependency(graph) {
			return nil, fmt.Errorf("graph factory returned nil")
		}
		return graph, nil
	}
}

// GraphNode and GraphEdge are Studio's drawable graph records.
type GraphNode struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

type GraphEdge struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Conditional bool   `json:"conditional,omitempty"`
}

type DrawableGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphRegistration declares one graph available to assistants.
type GraphRegistration struct {
	ID            string
	Name          string
	Description   string
	Factory       Factory
	InputSchema   json.RawMessage
	OutputSchema  json.RawMessage
	StateSchema   json.RawMessage
	ConfigSchema  json.RawMessage
	ContextSchema json.RawMessage
	Graph         DrawableGraph
}

// Options configures an Agent Server instance.
type Options struct {
	Saver          dacheckpoint.Saver
	Store          dastore.Store
	Deps           any
	StatePath      string
	QueueWorkers   int
	AllowedOrigins []string
	Now            func() time.Time
}

type Assistant struct {
	AssistantID string         `json:"assistant_id"`
	GraphID     string         `json:"graph_id"`
	Name        string         `json:"name"`
	Description *string        `json:"description"`
	Config      map[string]any `json:"config"`
	Context     any            `json:"context"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	Version     int            `json:"version"`
}

type Thread struct {
	ThreadID   string         `json:"thread_id"`
	CreatedAt  string         `json:"created_at"`
	UpdatedAt  string         `json:"updated_at"`
	Metadata   map[string]any `json:"metadata"`
	Status     string         `json:"status"`
	Values     map[string]any `json:"values,omitempty"`
	Interrupts map[string]any `json:"interrupts,omitempty"`
}

type Run struct {
	RunID             string         `json:"run_id"`
	ThreadID          string         `json:"thread_id"`
	AssistantID       string         `json:"assistant_id"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
	Status            string         `json:"status"`
	Metadata          map[string]any `json:"metadata"`
	Kwargs            map[string]any `json:"kwargs"`
	MultitaskStrategy string         `json:"multitask_strategy"`
}

type Checkpoint struct {
	ThreadID      string         `json:"thread_id"`
	Namespace     string         `json:"checkpoint_ns"`
	CheckpointID  string         `json:"checkpoint_id"`
	CheckpointMap map[string]any `json:"checkpoint_map,omitempty"`
}

type ThreadTask struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Error      *string      `json:"error"`
	Interrupts []any        `json:"interrupts"`
	Checkpoint *Checkpoint  `json:"checkpoint"`
	State      *ThreadState `json:"state"`
	Result     any          `json:"result"`
}

type ThreadState struct {
	Values           map[string]any `json:"values"`
	Next             []string       `json:"next"`
	Checkpoint       *Checkpoint    `json:"checkpoint"`
	Metadata         map[string]any `json:"metadata"`
	CreatedAt        *string        `json:"created_at"`
	ParentCheckpoint *Checkpoint    `json:"parent_checkpoint"`
	Tasks            []ThreadTask   `json:"tasks"`
}

type createRunRequest struct {
	AssistantID       string         `json:"assistant_id"`
	CheckpointID      string         `json:"checkpoint_id"`
	Checkpoint        *Checkpoint    `json:"checkpoint"`
	Input             any            `json:"input"`
	Command           map[string]any `json:"command"`
	Metadata          map[string]any `json:"metadata"`
	Context           any            `json:"context"`
	Config            map[string]any `json:"config"`
	MultitaskStrategy string         `json:"multitask_strategy"`
	StreamMode        any            `json:"stream_mode"`
	IfNotExists       string         `json:"if_not_exists"`
}
