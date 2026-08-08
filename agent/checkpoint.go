package agent

import (
	"context"
	"fmt"

	"github.com/semistrict/dago/checkpoint"
)

// History lists newest-first checkpoint records for a thread or namespace.
func (agent *Agent) History(ctx context.Context, config checkpoint.Config, options checkpoint.ListOptions) ([]checkpoint.Tuple, error) {
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
func (agent *Agent) Replay(ctx context.Context, config checkpoint.Config) (Result, error) {
	if config.CheckpointID == "" {
		return Result{}, fmt.Errorf("replay requires a checkpoint ID")
	}
	return agent.Invoke(ctx, Input{Config: config})
}
