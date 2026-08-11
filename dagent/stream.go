package dagent

import (
	"context"
	"encoding/json"
	"io"

	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/datool"
	graph "github.com/semistrict/dago/internal/graph"
)

type EventMode string

const (
	EventTask      EventMode = "task"
	EventUpdate    EventMode = "update"
	EventValues    EventMode = "values"
	EventInterrupt EventMode = "interrupt"
	EventCustom    EventMode = "custom"
	EventToken     EventMode = "token"
	EventChild     EventMode = "child"
	EventToolProgress EventMode = "tool_progress"
)

type ChildEventPhase string

const (
	ChildStarted     ChildEventPhase = "started"
	ChildEventUpdate ChildEventPhase = "event"
	ChildCompleted   ChildEventPhase = "completed"
	ChildFailed      ChildEventPhase = "failed"
	ChildInterrupted ChildEventPhase = "interrupted"
)

// ChildEvent projects a nested agent run onto its parent's stream. Event is set
// for the event phase; terminal phases carry the child's visible output.
type ChildEvent struct {
	Phase      ChildEventPhase     `json:"phase"`
	Name       string              `json:"name"`
	ToolCallID string              `json:"tool_call_id"`
	Namespace  string              `json:"namespace,omitempty"`
	Event      *Event              `json:"event,omitempty"`
	Messages   []damessage.Message `json:"messages,omitempty"`
	Structured json.RawMessage     `json:"structured,omitempty"`
	State      dastate.Values      `json:"state,omitempty"`
	Interrupts []Interrupt         `json:"interrupts,omitempty"`
	Error      string              `json:"error,omitempty"`
}

// Event is a version-stable public execution event. Provider token chunks are
// represented separately from graph lifecycle events in model streams.
type Event struct {
	Mode      EventMode       `json:"mode"`
	Step      int             `json:"step"`
	Node      string          `json:"node,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
	Update    dastate.Values  `json:"update,omitempty"`
	Values    dastate.Values  `json:"values,omitempty"`
	Interrupt *Interrupt      `json:"interrupt,omitempty"`
	Custom    json.RawMessage `json:"custom,omitempty"`
	Chunk     *damodel.Chunk  `json:"chunk,omitempty"`
	Child     *ChildEvent     `json:"child,omitempty"`
	ToolProgress *datool.Progress `json:"tool_progress,omitempty"`
}

// Stream is an owned, bounded agent execution stream.
type Stream struct {
	graph              *graph.Stream
	private            map[string]bool
	discardResultState bool
}

// EncodeChildEvent creates the versioned custom-event envelope understood by
// Stream.Next. Tool implementations use it to forward nested runs without
// coupling the graph runtime to agent hierarchy.
func EncodeChildEvent(event ChildEvent) (json.RawMessage, error) {
	encoded, err := json.Marshal(streamEnvelope{Version: 1, Kind: "child", Child: &event})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// Stream starts an invocation. Consumers that stop before io.EOF must call Close.
func (agent *Agent) Stream(ctx context.Context, input Input, buffer int) *Stream {
	if input.Config.ThreadID == "" {
		input.Config.ThreadID = "default"
	}
	values := input.State.Clone()
	if values == nil {
		values = dastate.Values{}
	}
	if len(input.Messages) > 0 {
		values[MessagesKey] = damessage.EnsureIDs(input.Messages)
	}
	ensureMessageIDsInValues(values)
	return &Stream{
		graph: agent.graph.Stream(ctx, graph.Invocation{
			Config: input.Config, State: values, Resume: input.Resume, SkipValueEvents: input.SkipValueEvents,
		}, buffer),
		private: agent.private, discardResultState: input.DiscardResultState,
	}
}

func (stream *Stream) Next(ctx context.Context) (Event, error) {
	if stream == nil || stream.graph == nil {
		return Event{}, io.EOF
	}
	event, err := stream.graph.Next(ctx)
	if err != nil {
		return Event{}, err
	}
	result := Event{
		Mode: EventMode(event.Mode), Step: event.Step, Node: event.Node, TaskID: event.TaskID,
		Update: publicState(event.Update, stream.private), Values: publicState(event.Values, stream.private), Custom: append(json.RawMessage(nil), event.Custom...),
	}
	if event.Interrupt != nil {
		result.Interrupt = &Interrupt{ID: event.Interrupt.ID, Value: event.Interrupt.Value}
	}
	if event.Mode == graph.EventCustom && len(event.Custom) > 0 {
		var envelope streamEnvelope
		if json.Unmarshal(event.Custom, &envelope) == nil && envelope.Version == 1 {
			switch envelope.Kind {
			case "token":
				result.Mode = EventToken
				chunk := envelope.Chunk
				result.Chunk = &chunk
				result.Custom = nil
			case "child":
				result.Mode = EventChild
				result.Child = envelope.Child
				result.Custom = nil
			case "tool_progress":
				result.Mode = EventToolProgress
				result.ToolProgress = envelope.ToolProgress
				result.Custom = nil
			}
		}
	}
	return result, nil
}

func (stream *Stream) Result(ctx context.Context) (Result, error) {
	if stream == nil || stream.graph == nil {
		return Result{}, io.EOF
	}
	execution, err := stream.graph.Result(ctx)
	if err != nil {
		return Result{}, err
	}
	return resultFromExecution(execution, stream.private, stream.discardResultState)
}

func (stream *Stream) Close() error {
	if stream == nil || stream.graph == nil {
		return nil
	}
	return stream.graph.Close()
}
