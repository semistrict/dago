package agent

import (
	"context"
	"encoding/json"
	"io"

	graph "github.com/semistrict/dago/internal/graph"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/model"
	"github.com/semistrict/dago/state"
)

type EventMode string

const (
	EventTask      EventMode = "task"
	EventUpdate    EventMode = "update"
	EventValues    EventMode = "values"
	EventInterrupt EventMode = "interrupt"
	EventCustom    EventMode = "custom"
	EventToken     EventMode = "token"
)

// Event is a version-stable public execution event. Provider token chunks are
// represented separately from graph lifecycle events in model streams.
type Event struct {
	Mode      EventMode       `json:"mode"`
	Step      int             `json:"step"`
	Node      string          `json:"node,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
	Update    state.Values    `json:"update,omitempty"`
	Values    state.Values    `json:"values,omitempty"`
	Interrupt *Interrupt      `json:"interrupt,omitempty"`
	Custom    json.RawMessage `json:"custom,omitempty"`
	Chunk     *model.Chunk    `json:"chunk,omitempty"`
}

// Stream is an owned, bounded agent execution stream.
type Stream struct {
	graph   *graph.Stream
	private map[string]bool
}

// Stream starts an invocation. Consumers that stop before io.EOF must call Close.
func (agent *Agent) Stream(ctx context.Context, input Input, buffer int) *Stream {
	if input.Config.ThreadID == "" {
		input.Config.ThreadID = "default"
	}
	values := input.State.Clone()
	if values == nil {
		values = state.Values{}
	}
	if len(input.Messages) > 0 {
		values[MessagesKey] = message.EnsureIDs(input.Messages)
	}
	ensureMessageIDsInValues(values)
	return &Stream{graph: agent.graph.Stream(ctx, graph.Invocation{Config: input.Config, State: values, Resume: input.Resume}, buffer), private: agent.private}
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
		if json.Unmarshal(event.Custom, &envelope) == nil && envelope.Version == 1 && envelope.Kind == "token" {
			result.Mode = EventToken
			chunk := envelope.Chunk
			result.Chunk = &chunk
			result.Custom = nil
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
	return resultFromExecution(execution, stream.private)
}

func (stream *Stream) Close() error {
	if stream == nil || stream.graph == nil {
		return nil
	}
	return stream.graph.Close()
}
