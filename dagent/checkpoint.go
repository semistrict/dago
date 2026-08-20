package dagent

import (
	"context"
	"fmt"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/internal/graph"
)

// Snapshot is one reconstructed durable agent state. Next names nodes waiting
// at the addressed checkpoint; reading a snapshot never executes them.
type Snapshot struct {
	Config   dacheckpoint.Config   `json:"config"`
	State    dastate.Values        `json:"state"`
	Metadata dacheckpoint.Metadata `json:"metadata"`
	Next     []string              `json:"next"`
}

// State reconstructs the latest or addressed checkpoint, including delta
// channels, without executing pending work.
func (agent *Agent) State(ctx context.Context, config dacheckpoint.Config) (Snapshot, error) {
	if agent == nil || agent.saver == nil {
		return Snapshot{}, fmt.Errorf("agent has no checkpoint saver")
	}
	state, err := agent.graph.Snapshot(ctx, config)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Config: state.Config, State: publicState(state.State, agent.private),
		Metadata: state.Metadata, Next: append([]string(nil), state.Next...),
	}, nil
}

// UpdateState persists a child checkpoint containing values without executing
// any scheduled work.
func (agent *Agent) UpdateState(ctx context.Context, config dacheckpoint.Config, values dastate.Values) (Snapshot, error) {
	if agent == nil || agent.saver == nil {
		return Snapshot{}, fmt.Errorf("agent has no checkpoint saver")
	}
	state, err := agent.graph.UpdateState(ctx, graph.Invocation{Config: config, State: values})
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Config: state.Config, State: publicState(state.State, agent.private),
		Metadata: state.Metadata, Next: append([]string(nil), state.Next...),
	}, nil
}

// History lists newest-first checkpoint records for a thread or namespace.
func (agent *Agent) History(ctx context.Context, config dacheckpoint.Config, options dacheckpoint.ListOptions) ([]dacheckpoint.Tuple, error) {
	if agent == nil || agent.saver == nil {
		return nil, fmt.Errorf("agent has no checkpoint saver")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return agent.saver.List(ctx, &config, options)
}

// Fork copies a complete thread, including namespaces, checkpoints, pending
// writes, and delta ancestry, to a new thread ID.
func (agent *Agent) Fork(ctx context.Context, sourceThreadID, targetThreadID string) error {
	if agent == nil || agent.saver == nil {
		return fmt.Errorf("agent has no checkpoint saver")
	}
	if sourceThreadID == "" || targetThreadID == "" || sourceThreadID == targetThreadID {
		return fmt.Errorf("fork requires distinct non-empty source and target thread IDs")
	}
	return agent.saver.CopyThread(ctx, sourceThreadID, targetThreadID)
}

// DeleteThread removes every namespace and checkpoint for a thread.
func (agent *Agent) DeleteThread(ctx context.Context, threadID string) error {
	if agent == nil || agent.saver == nil {
		return fmt.Errorf("agent has no checkpoint saver")
	}
	if threadID == "" {
		return fmt.Errorf("thread ID is required")
	}
	return agent.saver.DeleteThread(ctx, threadID)
}

// Replay restores the addressed checkpoint and continues its saved pending tasks.
// A terminal checkpoint returns its values without starting a new run.
func (agent *Agent) Replay(ctx context.Context, config dacheckpoint.Config) (Result, error) {
	if config.CheckpointID == "" {
		return Result{}, fmt.Errorf("replay requires a checkpoint ID")
	}
	return agent.Invoke(ctx, FromCheckpoint(config))
}
