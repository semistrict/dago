package graph

import (
	"context"
	"fmt"
	"sort"

	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/internal/graph/channel"
	"github.com/semistrict/dago/state"
)

func (graph *Compiled) Invoke(ctx context.Context, invocation Invocation) (Execution, error) {
	if err := invocation.Config.Validate(); err != nil {
		return Execution{}, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	machine, current, tasks, metadata, err := graph.restore(ctx, invocation)
	if err != nil {
		return Execution{}, err
	}
	if current.CheckpointID == "" {
		tasks = graph.startTasks()
		if _, err := machine.apply([]state.Values{invocation.State}); err != nil {
			return Execution{}, fmt.Errorf("apply graph input: %w", err)
		}
		current, metadata, err = graph.persist(ctx, checkpoint.Config{
			ThreadID: invocation.Config.ThreadID, Namespace: invocation.Config.Namespace,
		}, checkpoint.Checkpoint{}, machine, tasks, MetadataInput{
			Source: "input", Step: -1, Updated: keysOf(invocation.State),
			ForceDeltaSnapshots: true,
		})
		if err != nil {
			return Execution{}, err
		}
	} else if len(invocation.State) > 0 {
		if len(tasks) == 0 {
			tasks = graph.startTasks()
		}
		if graph.options.Saver != nil {
			if err := graph.options.Saver.PutWrites(ctx, current, "__input__", "", stateWrites(invocation.State)); err != nil {
				return Execution{}, err
			}
		}
		overwrites, err := machine.apply([]state.Values{invocation.State})
		if err != nil {
			return Execution{}, fmt.Errorf("apply graph input: %w", err)
		}
		current, metadata, err = graph.persist(ctx, current, checkpoint.Checkpoint{}, machine, tasks, MetadataInput{
			Source: "input", Step: metadata.Step + 1, Updated: keysOf(invocation.State),
			Overwrites: overwrites, PreviousMetadata: metadata,
		})
		if err != nil {
			return Execution{}, err
		}
	}
	if invocation.Resume != nil && graph.options.Saver != nil {
		if err := graph.options.Saver.PutWrites(ctx, current, "__resume__", "", []checkpoint.ChannelWrite{{
			Channel: checkpoint.ChannelResume, Value: invocation.Resume,
		}}); err != nil {
			return Execution{}, err
		}
	}

	resume := invocation.Resume
	for step := 0; step < graph.options.RecursionLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Execution{}, err
		}
		if len(tasks) == 0 {
			values, err := machine.values()
			return Execution{Config: current, State: values, Steps: step}, err
		}
		results := graph.executeTasks(ctx, machine, tasks, current, resume, step)
		resume = nil
		for _, result := range results {
			if result.err != nil {
				cancel(result.err)
				return Execution{}, fmt.Errorf("execute node %q: %w", result.task.node, result.err)
			}
		}

		interrupts := collectInterrupts(results)
		if len(interrupts) > 0 {
			current, metadata, err = graph.persist(ctx, current, checkpoint.Checkpoint{}, machine, tasks, MetadataInput{
				Source: "loop", Step: metadata.Step + 1, Interrupts: interrupts,
				PreviousMetadata: metadata,
			})
			if err != nil {
				return Execution{}, err
			}
			values, err := machine.values()
			if err != nil {
				return Execution{}, err
			}
			if graph.options.Writer != nil {
				for index := range interrupts {
					interrupt := interrupts[index]
					if err := graph.options.Writer.Write(ctx, Event{
						Mode: EventInterrupt, Step: step, Interrupt: &interrupt,
					}); err != nil {
						return Execution{}, err
					}
				}
			}
			return Execution{
				Config: current, State: values, Interrupts: interrupts, Steps: step + 1,
			}, nil
		}

		updates := make([]state.Values, len(results))
		if graph.options.Saver != nil {
			for index, result := range results {
				updates[index] = result.command.Update
				if len(result.command.Update) == 0 {
					continue
				}
				if err := graph.options.Saver.PutWrites(
					ctx, current, result.task.id, result.task.path, stateWrites(result.command.Update),
				); err != nil {
					return Execution{}, err
				}
			}
		} else {
			for index, result := range results {
				updates[index] = result.command.Update
			}
		}
		machine.consumeEphemeral()
		overwrites, err := machine.apply(updates)
		if err != nil {
			return Execution{}, err
		}
		next, err := graph.route(ctx, machine, results, step)
		if err != nil {
			return Execution{}, err
		}
		updated := updatedKeys(updates)
		current, metadata, err = graph.persist(ctx, current, checkpoint.Checkpoint{}, machine, next, MetadataInput{
			Source: "loop", Step: metadata.Step + 1, Updated: updated,
			Overwrites: overwrites, PreviousMetadata: metadata,
		})
		if err != nil {
			return Execution{}, err
		}
		if graph.options.Writer != nil {
			for _, result := range results {
				if len(result.command.Update) == 0 {
					continue
				}
				if err := graph.options.Writer.Write(ctx, Event{
					Mode: EventUpdate, Step: step, Node: result.task.node,
					TaskID: result.task.id, Update: result.command.Update.Clone(),
				}); err != nil {
					return Execution{}, err
				}
			}
			values, err := machine.values()
			if err != nil {
				return Execution{}, err
			}
			if err := graph.options.Writer.Write(ctx, Event{
				Mode: EventValues, Step: step, Values: values,
			}); err != nil {
				return Execution{}, err
			}
		}
		tasks = next
	}
	return Execution{}, ErrRecursionLimit
}

func (graph *Compiled) restore(
	ctx context.Context,
	invocation Invocation,
) (*stateMachine, checkpoint.Config, []task, checkpoint.Metadata, error) {
	if graph.options.Saver == nil {
		machine, err := newStateMachine(graph.schema, nil, nil)
		return machine, checkpoint.Config{}, nil, checkpoint.Metadata{Step: -1}, err
	}
	tuple, err := graph.options.Saver.GetTuple(ctx, invocation.Config)
	if err != nil {
		return nil, checkpoint.Config{}, nil, checkpoint.Metadata{}, err
	}
	if tuple == nil {
		machine, err := newStateMachine(graph.schema, nil, nil)
		return machine, checkpoint.Config{}, nil, checkpoint.Metadata{Step: -1}, err
	}
	deltaNames := make([]string, 0)
	for key, field := range graph.schema.Fields {
		if field.kind == fieldDelta {
			deltaNames = append(deltaNames, key)
		}
	}
	sort.Strings(deltaNames)
	histories, err := graph.options.Saver.GetDeltaChannelHistory(ctx, tuple.Config, deltaNames)
	if err != nil {
		return nil, checkpoint.Config{}, nil, checkpoint.Metadata{}, err
	}
	storedState := make(map[string]any, len(tuple.Checkpoint.ChannelValues))
	for key, value := range tuple.Checkpoint.ChannelValues {
		switch key {
		case checkpoint.ChannelTasks, checkpoint.ChannelInterrupt, checkpoint.ChannelResume,
			checkpoint.ChannelError, checkpoint.ChannelScheduled:
			continue
		default:
			storedState[key] = value
		}
	}
	machine, err := newStateMachine(graph.schema, storedState, histories)
	if err != nil {
		return nil, checkpoint.Config{}, nil, checkpoint.Metadata{}, err
	}
	tasks, err := decodeTasks(tuple.Checkpoint.ChannelValues[checkpoint.ChannelTasks])
	if err != nil {
		return nil, checkpoint.Config{}, nil, checkpoint.Metadata{}, err
	}
	return machine, tuple.Config, tasks, tuple.Metadata, nil
}

type MetadataInput struct {
	Source              string
	Step                int
	Updated             []string
	Overwrites          map[string]bool
	Interrupts          []Interrupt
	ForceDeltaSnapshots bool
	PreviousMetadata    checkpoint.Metadata
}

func (graph *Compiled) persist(
	ctx context.Context,
	parent checkpoint.Config,
	previous checkpoint.Checkpoint,
	machine *stateMachine,
	tasks []task,
	input MetadataInput,
) (checkpoint.Config, checkpoint.Metadata, error) {
	metadata := checkpoint.Metadata{
		Source: input.Source, Step: input.Step,
		CountersSinceDeltaSnapshot: make(map[string]checkpoint.DeltaCounter),
	}
	for key, counter := range input.PreviousMetadata.CountersSinceDeltaSnapshot {
		metadata.CountersSinceDeltaSnapshot[key] = counter
	}
	updatedSet := make(map[string]bool, len(input.Updated))
	for _, key := range input.Updated {
		updatedSet[key] = true
	}
	snapshots := make(map[string]bool)
	for key, frequency := range machine.deltaFields() {
		counter := metadata.CountersSinceDeltaSnapshot[key]
		counter.Supersteps++
		if updatedSet[key] {
			counter.Updates++
		}
		if input.ForceDeltaSnapshots || input.Overwrites[key] ||
			(channel.DeltaSnapshotCounter{
				Updates: counter.Updates, Supersteps: counter.Supersteps,
			}).ShouldSnapshot(frequency) {
			snapshots[key] = true
			counter = checkpoint.DeltaCounter{}
		}
		if counter != (checkpoint.DeltaCounter{}) {
			metadata.CountersSinceDeltaSnapshot[key] = counter
		} else {
			delete(metadata.CountersSinceDeltaSnapshot, key)
		}
	}
	values, err := machine.checkpointValues(snapshots)
	if err != nil {
		return checkpoint.Config{}, checkpoint.Metadata{}, err
	}
	values[checkpoint.ChannelTasks] = encodeTasks(tasks)
	if len(input.Interrupts) > 0 {
		values[checkpoint.ChannelInterrupt] = encodeInterrupts(input.Interrupts)
	}
	if graph.options.Saver == nil {
		return parent, metadata, nil
	}
	baseVersions := previous.ChannelVersions
	if len(baseVersions) == 0 && parent.CheckpointID != "" {
		tuple, err := graph.options.Saver.GetTuple(ctx, parent)
		if err != nil {
			return checkpoint.Config{}, checkpoint.Metadata{}, err
		}
		if tuple != nil {
			baseVersions = tuple.Checkpoint.ChannelVersions
		}
	}
	versions := make(map[string]string, len(baseVersions)+len(values))
	for key, version := range baseVersions {
		versions[key] = version
	}
	versionKeys := append([]string(nil), input.Updated...)
	for key := range snapshots {
		if !updatedSet[key] {
			versionKeys = append(versionKeys, key)
		}
	}
	versionKeys = append(versionKeys, checkpoint.ChannelTasks, checkpoint.ChannelInterrupt)
	versionKeys = uniqueSorted(versionKeys)
	newVersions := make(map[string]string, len(versionKeys))
	for _, key := range versionKeys {
		version, err := graph.options.Saver.NextVersion(versions[key])
		if err != nil {
			return checkpoint.Config{}, checkpoint.Metadata{}, err
		}
		versions[key] = version
		newVersions[key] = version
	}
	id, err := checkpoint.NewID(input.Step)
	if err != nil {
		return checkpoint.Config{}, checkpoint.Metadata{}, err
	}
	value := checkpoint.Checkpoint{
		Version: checkpoint.LatestVersion, ID: id,
		Timestamp: checkpointTimestamp(), ChannelValues: values,
		ChannelVersions: versions, VersionsSeen: map[string]map[string]string{},
		UpdatedChannels: uniqueSorted(input.Updated),
	}
	config, err := graph.options.Saver.Put(ctx, parent, value, metadata, newVersions)
	return config, metadata, err
}

func (graph *Compiled) route(
	ctx context.Context,
	machine *stateMachine,
	results []taskResult,
	step int,
) ([]task, error) {
	values, err := machine.values()
	if err != nil {
		return nil, err
	}
	var next []task
	for resultIndex, result := range results {
		pathPrefix := fmt.Sprintf("%08d/%08d", step, resultIndex)
		for sendIndex, send := range result.command.Sends {
			if send.Node == End {
				continue
			}
			if _, exists := graph.nodes[send.Node]; !exists {
				return nil, fmt.Errorf("route send to %q: %w", send.Node, ErrUnknownNode)
			}
			next = append(next, task{node: send.Node, input: send.Input.Clone(), path: fmt.Sprintf("%s/%08d", pathPrefix, sendIndex)})
		}
		destinations := result.command.Goto
		if len(destinations) == 0 {
			if router := graph.conditional[result.task.node]; router != nil {
				destinations, err = router(ctx, values.Clone())
				if err != nil {
					return nil, fmt.Errorf("route from %q: %w", result.task.node, err)
				}
			} else {
				destinations = graph.edges[result.task.node]
			}
		}
		for destinationIndex, destination := range destinations {
			if destination == End {
				continue
			}
			if _, exists := graph.nodes[destination]; !exists {
				return nil, fmt.Errorf("route to %q: %w", destination, ErrUnknownNode)
			}
			next = append(next, task{node: destination, path: fmt.Sprintf("%s/e%08d", pathPrefix, destinationIndex)})
		}
	}
	return normalizeTasks(next), nil
}

func (graph *Compiled) startTasks() []task {
	tasks := make([]task, 0, len(graph.edges[Start]))
	for index, node := range graph.edges[Start] {
		if node != End {
			tasks = append(tasks, task{node: node, path: fmt.Sprintf("s/%08d", index)})
		}
	}
	return normalizeTasks(tasks)
}

func collectInterrupts(results []taskResult) []Interrupt {
	result := make([]Interrupt, 0)
	for _, task := range results {
		if task.command.Interrupt != nil {
			interrupt := *task.command.Interrupt
			if interrupt.ID == "" {
				interrupt.ID = task.task.id
			}
			result = append(result, interrupt)
		}
	}
	return result
}

func stateWrites(values state.Values) []checkpoint.ChannelWrite {
	keys := keysOf(values)
	result := make([]checkpoint.ChannelWrite, 0, len(keys))
	for _, key := range keys {
		if batch, ok := values[key].(state.Batch); ok {
			for _, value := range batch.Values {
				result = append(result, checkpoint.ChannelWrite{Channel: key, Value: value})
			}
			continue
		}
		result = append(result, checkpoint.ChannelWrite{Channel: key, Value: values[key]})
	}
	return result
}

func keysOf(values state.Values) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func updatedKeys(updates []state.Values) []string {
	set := map[string]struct{}{}
	for _, update := range updates {
		for key := range update {
			set[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
