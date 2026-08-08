package graph

import (
	"errors"
	"fmt"
	"sort"

	"github.com/semistrict/dago/checkpoint"
	"github.com/semistrict/dago/internal/graph/channel"
	"github.com/semistrict/dago/state"
)

var (
	ErrInvalidStateUpdate = errors.New("invalid concurrent state update")
	ErrUnknownStateField  = errors.New("unknown state field")
)

// Reducer combines the current field value with an ordered batch of superstep writes.
type Reducer func(current any, writes []any) (any, error)

// Cloner isolates mutable field values at task, reducer, and checkpoint boundaries.
type Cloner func(any) any

type fieldKind uint8

const (
	fieldLast fieldKind = iota
	fieldAggregate
	fieldDelta
	fieldEphemeral
)

// Field describes one state key.
type Field struct {
	kind              fieldKind
	initial           func() any
	reducer           Reducer
	clone             Cloner
	snapshotFrequency uint64
}

// LastValue creates a field that accepts at most one write per superstep.
func LastValue(clone Cloner) Field {
	return Field{kind: fieldLast, clone: clone}
}

// Aggregate creates a persistent reducer-backed field.
func Aggregate(initial func() any, reducer Reducer, clone Cloner) Field {
	return Field{kind: fieldAggregate, initial: initial, reducer: reducer, clone: clone}
}

// Delta creates a required delta field whose durable state is reconstructed from
// periodic snapshots and ancestor writes.
func Delta(
	initial func() any,
	reducer Reducer,
	clone Cloner,
	snapshotFrequency uint64,
) Field {
	return Field{
		kind: fieldDelta, initial: initial, reducer: reducer, clone: clone,
		snapshotFrequency: snapshotFrequency,
	}
}

// Ephemeral creates a last-value field omitted from checkpoints.
func Ephemeral(clone Cloner) Field {
	return Field{kind: fieldEphemeral, clone: clone}
}

func (field Field) validate(key string) error {
	if field.clone == nil {
		return fmt.Errorf("state field %q: cloner is required", key)
	}
	switch field.kind {
	case fieldLast, fieldEphemeral:
		return nil
	case fieldAggregate:
		if field.initial == nil || field.reducer == nil {
			return fmt.Errorf("state field %q: aggregate requires initial value and reducer", key)
		}
	case fieldDelta:
		if field.initial == nil || field.reducer == nil {
			return fmt.Errorf("state field %q: delta requires initial value and reducer", key)
		}
		if field.snapshotFrequency == 0 {
			return fmt.Errorf("state field %q: %w", key, channel.ErrInvalidSnapshotFrequency)
		}
	default:
		return fmt.Errorf("state field %q: unknown field kind", key)
	}
	return nil
}

// Schema controls reducer behavior and whether unregistered keys are accepted as
// last-value fields.
type Schema struct {
	Fields       map[string]Field
	AllowUnknown bool
}

func (schema Schema) Validate() error {
	for key, field := range schema.Fields {
		if key == "" {
			return fmt.Errorf("state schema contains an empty key")
		}
		if err := field.validate(key); err != nil {
			return err
		}
	}
	return nil
}

type fieldState struct {
	spec      Field
	value     any
	available bool
	delta     *channel.Delta[any]
}

type stateMachine struct {
	schema Schema
	fields map[string]*fieldState
}

func newStateMachine(
	schema Schema,
	stored map[string]any,
	histories map[string]checkpoint.DeltaHistory,
) (*stateMachine, error) {
	if err := schema.Validate(); err != nil {
		return nil, err
	}
	machine := &stateMachine{schema: schema, fields: make(map[string]*fieldState)}
	for key, spec := range schema.Fields {
		field := &fieldState{spec: spec}
		value, present := stored[key]
		switch spec.kind {
		case fieldAggregate:
			field.available = true
			if present {
				field.value = spec.clone(value)
			} else {
				field.value = spec.initial()
			}
		case fieldDelta:
			delta, err := channel.NewDelta(
				spec.initial,
				channel.DeltaReducer[any](spec.reducer),
				channel.Cloner[any](spec.clone),
				channel.WithSnapshotFrequency(spec.snapshotFrequency))
			if err != nil {
				return nil, fmt.Errorf("state field %q: %w", key, err)
			}
			var seed channel.CheckpointValue[any]
			if present {
				if snapshot, ok := value.(checkpoint.DeltaSnapshot); ok {
					seed = channel.SnapshotCheckpoint(snapshot.Value)
				} else {
					seed = channel.LegacyCheckpoint(value)
				}
			} else if history, ok := histories[key]; ok && history.HasSeed {
				if snapshot, ok := history.Seed.(checkpoint.DeltaSnapshot); ok {
					seed = channel.SnapshotCheckpoint(snapshot.Value)
				} else {
					seed = channel.LegacyCheckpoint(history.Seed)
				}
			} else {
				seed = channel.MissingCheckpoint[any]()
			}
			restored, err := delta.Restore(seed)
			if err != nil {
				return nil, fmt.Errorf("restore state field %q: %w", key, err)
			}
			if !present {
				if history, ok := histories[key]; ok {
					writes := make([]channel.DeltaWrite[any], 0, len(history.Writes))
					for _, write := range history.Writes {
						if overwrite, ok := write.Value.(state.Overwrite); ok {
							writes = append(writes, channel.OverwriteValue(overwrite.Value))
						} else {
							writes = append(writes, channel.UpdateValue(write.Value))
						}
					}
					if err := restored.Replay(writes); err != nil {
						return nil, fmt.Errorf("replay state field %q: %w", key, err)
					}
				}
			}
			field.delta = restored
		case fieldLast, fieldEphemeral:
			if present {
				field.value = spec.clone(value)
				field.available = true
			}
		}
		machine.fields[key] = field
	}
	for key, value := range stored {
		if _, known := machine.fields[key]; known {
			continue
		}
		if !schema.AllowUnknown {
			return nil, fmt.Errorf("%w %q", ErrUnknownStateField, key)
		}
		machine.fields[key] = &fieldState{
			spec:  Field{kind: fieldLast, clone: identityClone},
			value: value, available: true,
		}
	}
	return machine, nil
}

func (machine *stateMachine) values() (state.Values, error) {
	values := make(state.Values)
	for key, field := range machine.fields {
		if field.spec.kind == fieldDelta {
			value, err := field.delta.Get()
			if err != nil {
				if errors.Is(err, channel.ErrEmpty) {
					continue
				}
				return nil, err
			}
			values[key] = value
			continue
		}
		if field.available {
			values[key] = field.spec.clone(field.value)
		}
	}
	return values, nil
}

func (machine *stateMachine) apply(updates []state.Values) (map[string]bool, error) {
	grouped := make(map[string][]any)
	for _, update := range updates {
		keys := make([]string, 0, len(update))
		for key := range update {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if batch, ok := update[key].(state.Batch); ok {
				grouped[key] = append(grouped[key], batch.Values...)
			} else {
				grouped[key] = append(grouped[key], update[key])
			}
		}
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	overwrites := make(map[string]bool)
	for _, key := range keys {
		field, exists := machine.fields[key]
		if !exists {
			if !machine.schema.AllowUnknown {
				return nil, fmt.Errorf("%w %q", ErrUnknownStateField, key)
			}
			field = &fieldState{spec: Field{kind: fieldLast, clone: identityClone}}
			machine.fields[key] = field
		}
		wroteOverwrite, err := field.apply(grouped[key])
		if err != nil {
			return nil, fmt.Errorf("state field %q: %w", key, err)
		}
		overwrites[key] = wroteOverwrite
	}
	return overwrites, nil
}

// consumeEphemeral clears values read by the completed superstep. Writes produced
// by that superstep are applied afterward and remain available to the next one.
func (machine *stateMachine) consumeEphemeral() {
	for _, field := range machine.fields {
		if field.spec.kind == fieldEphemeral {
			field.value = nil
			field.available = false
		}
	}
}

func (field *fieldState) apply(writes []any) (bool, error) {
	overwriteIndex := -1
	for index, write := range writes {
		if _, ok := write.(state.Overwrite); ok {
			if overwriteIndex != -1 {
				return false, ErrInvalidStateUpdate
			}
			overwriteIndex = index
		}
	}
	if field.spec.kind == fieldDelta {
		deltaWrites := make([]channel.DeltaWrite[any], 0, len(writes))
		for _, write := range writes {
			if overwrite, ok := write.(state.Overwrite); ok {
				deltaWrites = append(deltaWrites, channel.OverwriteValue(overwrite.Value))
			} else {
				deltaWrites = append(deltaWrites, channel.UpdateValue(write))
			}
		}
		_, err := field.delta.Apply(deltaWrites)
		return overwriteIndex >= 0, err
	}
	if overwriteIndex >= 0 {
		field.value = field.spec.clone(writes[overwriteIndex].(state.Overwrite).Value)
		field.available = true
		return true, nil
	}
	switch field.spec.kind {
	case fieldLast, fieldEphemeral:
		if len(writes) != 1 {
			return false, ErrInvalidStateUpdate
		}
		field.value = field.spec.clone(writes[0])
		field.available = true
	case fieldAggregate:
		base := field.spec.initial()
		if field.available {
			base = field.spec.clone(field.value)
		}
		cloned := make([]any, len(writes))
		for index, write := range writes {
			cloned[index] = field.spec.clone(write)
		}
		next, err := field.spec.reducer(base, cloned)
		if err != nil {
			return false, err
		}
		field.value = next
		field.available = true
	}
	return false, nil
}

func (machine *stateMachine) checkpointValues(snapshots map[string]bool) (map[string]any, error) {
	values := make(map[string]any)
	for key, field := range machine.fields {
		switch field.spec.kind {
		case fieldDelta:
			if snapshots[key] {
				value, err := field.delta.Get()
				if err != nil {
					return nil, err
				}
				values[key] = checkpoint.DeltaSnapshot{Value: value}
			}
		case fieldEphemeral:
			continue
		default:
			if field.available {
				values[key] = field.spec.clone(field.value)
			}
		}
	}
	return values, nil
}

func (machine *stateMachine) deltaFields() map[string]uint64 {
	result := make(map[string]uint64)
	for key, field := range machine.fields {
		if field.spec.kind == fieldDelta && field.delta.IsAvailable() {
			result[key] = field.spec.snapshotFrequency
		}
	}
	return result
}

func identityClone(value any) any { return value }
