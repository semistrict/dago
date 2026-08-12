package graph

import (
	"context"
	"sort"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dastate"
)

// Snapshot reconstructs a checkpoint, including delta channels, without
// executing any scheduled work.
func (graph *Compiled) Snapshot(ctx context.Context, config dacheckpoint.Config) (Snapshot, error) {
	if err := config.Validate(); err != nil {
		return Snapshot{}, err
	}
	machine, current, tasks, metadata, err := graph.restore(ctx, Invocation{Config: config})
	if err != nil {
		return Snapshot{}, err
	}
	values, err := machine.values()
	if err != nil {
		return Snapshot{}, err
	}
	next := make([]string, 0, len(tasks))
	for _, task := range tasks {
		next = append(next, task.node)
	}
	sort.Strings(next)
	return Snapshot{Config: current, State: values, Metadata: metadata, Next: next}, nil
}

// UpdateState applies values to an addressed checkpoint and persists a child
// checkpoint without executing or changing its scheduled tasks.
func (graph *Compiled) UpdateState(ctx context.Context, invocation Invocation) (Snapshot, error) {
	if err := invocation.Config.Validate(); err != nil {
		return Snapshot{}, err
	}
	session := graph.threadSession(invocation.Config)
	if session != nil {
		session.mu.Lock()
		defer session.mu.Unlock()
		session.valid = false
	}
	machine, current, tasks, metadata, err := graph.restore(ctx, invocation)
	if err != nil {
		return Snapshot{}, err
	}
	overwrites, err := machine.apply([]dastate.Values{invocation.State})
	if err != nil {
		return Snapshot{}, err
	}
	previous := dacheckpoint.Checkpoint{}
	if current.CheckpointID != "" {
		tuple, err := graph.options.Saver.GetTuple(ctx, current)
		if err != nil {
			return Snapshot{}, err
		}
		if tuple != nil {
			previous = tuple.Checkpoint
		}
	}
	parent := current
	if parent.CheckpointID == "" {
		parent = dacheckpoint.Config{ThreadID: invocation.Config.ThreadID, Namespace: invocation.Config.Namespace}
	}
	step := metadata.Step + 1
	if current.CheckpointID == "" {
		step = -1
	}
	current, metadata, err = graph.persist(ctx, parent, previous, machine, tasks, MetadataInput{
		Source: "update", Step: step, Updated: keysOf(invocation.State), Overwrites: overwrites,
		// State edits can fork from any historical checkpoint. Snapshot delta
		// channels so one branch cannot observe writes belonging to a sibling.
		ForceDeltaSnapshots: true, PreviousMetadata: metadata,
	})
	if err != nil {
		return Snapshot{}, err
	}
	values, err := machine.values()
	if err != nil {
		return Snapshot{}, err
	}
	next := make([]string, 0, len(tasks))
	for _, task := range tasks {
		next = append(next, task.node)
	}
	sort.Strings(next)
	return Snapshot{Config: current, State: values, Metadata: metadata, Next: next}, nil
}
