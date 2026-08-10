package channel

import "fmt"

const DefaultDeltaSnapshotFrequency uint64 = 1000

// DeltaReducer applies a batch of writes to the accumulated value. Reducers must be
// deterministic and batching-invariant:
//
//	reduce(reduce(state, xs), ys) == reduce(state, append(xs, ys...))
//
// A reducer should treat its arguments as immutable and return the new value.
type DeltaReducer[T any] func(current T, writes []T) (T, error)

// Cloner isolates channel state from snapshots, overwrites, and callers.
type Cloner[T any] func(T) T

type deltaConfig struct {
	snapshotFrequency uint64
	ownedReducerInput bool
}

// DeltaOption configures a delta channel.
type DeltaOption func(*deltaConfig) error

// WithSnapshotFrequency sets the number of channel updates between full snapshots.
func WithSnapshotFrequency(frequency uint64) DeltaOption {
	return func(config *deltaConfig) error {
		if frequency == 0 {
			return ErrInvalidSnapshotFrequency
		}
		config.snapshotFrequency = frequency
		return nil
	}
}

// WithOwnedReducerInput allows Apply to transfer ownership of the accumulated
// value to the reducer. It is intended for serialized, retained state machines
// whose reducer can update its container without mutating values exposed to
// concurrent readers.
func WithOwnedReducerInput() DeltaOption {
	return func(config *deltaConfig) error {
		config.ownedReducerInput = true
		return nil
	}
}

type writeKind uint8

const (
	writeUpdate writeKind = iota
	writeOverwrite
)

// DeltaWrite is one persisted or in-flight channel write.
type DeltaWrite[T any] struct {
	kind  writeKind
	value T
}

// UpdateValue creates a normal reducer input.
func UpdateValue[T any](value T) DeltaWrite[T] {
	return DeltaWrite[T]{kind: writeUpdate, value: value}
}

// OverwriteValue creates a write that replaces the accumulated value.
func OverwriteValue[T any](value T) DeltaWrite[T] {
	return DeltaWrite[T]{kind: writeOverwrite, value: value}
}

// IsOverwrite reports whether the write replaces the accumulated value.
func (write DeltaWrite[T]) IsOverwrite() bool {
	return write.kind == writeOverwrite
}

// Value returns the write payload.
func (write DeltaWrite[T]) Value() T {
	return write.value
}

// CheckpointKind identifies how a channel value appeared in a checkpoint.
type CheckpointKind uint8

const (
	// CheckpointMissing means the checkpoint has no seed and history must be replayed.
	CheckpointMissing CheckpointKind = iota

	// CheckpointSnapshot is a full delta snapshot.
	CheckpointSnapshot

	// CheckpointLegacy is a full value written before the channel became a delta
	// channel. It is a migration seed, not a delta snapshot.
	CheckpointLegacy
)

// CheckpointValue is a missing value, a delta snapshot, or a pre-delta migration
// seed. Kind distinguishes an absent value from a present zero or nil value.
type CheckpointValue[T any] struct {
	Kind  CheckpointKind
	Value T
}

// MissingCheckpoint creates an absent checkpoint value.
func MissingCheckpoint[T any]() CheckpointValue[T] {
	return CheckpointValue[T]{Kind: CheckpointMissing}
}

// SnapshotCheckpoint creates a full delta snapshot seed.
func SnapshotCheckpoint[T any](value T) CheckpointValue[T] {
	return CheckpointValue[T]{Kind: CheckpointSnapshot, Value: value}
}

// LegacyCheckpoint creates a pre-delta full-value migration seed.
func LegacyCheckpoint[T any](value T) CheckpointValue[T] {
	return CheckpointValue[T]{Kind: CheckpointLegacy, Value: value}
}

// Delta stores an accumulated value in memory while durable checkpoints store only
// periodic full snapshots and the writes between them.
type Delta[T any] struct {
	initial           func() T
	reduce            DeltaReducer[T]
	clone             Cloner[T]
	snapshotFrequency uint64
	ownedReducerInput bool
	value             T
	available         bool
}

// NewDelta creates an uninitialized delta channel specification. Restore must be
// called before the channel is used by a graph. Restoring a missing checkpoint starts
// from the configured initial value and marks the channel available.
func NewDelta[T any](
	initial func() T,
	reducer DeltaReducer[T],
	cloner Cloner[T],
	options ...DeltaOption,
) (*Delta[T], error) {
	if initial == nil {
		return nil, fmt.Errorf("create delta channel: initial value factory is required")
	}
	if reducer == nil {
		return nil, fmt.Errorf("create delta channel: reducer is required")
	}
	if cloner == nil {
		return nil, fmt.Errorf("create delta channel: cloner is required")
	}

	config := deltaConfig{snapshotFrequency: DefaultDeltaSnapshotFrequency}
	for _, option := range options {
		if option == nil {
			return nil, fmt.Errorf("create delta channel: nil option")
		}
		if err := option(&config); err != nil {
			return nil, fmt.Errorf("create delta channel: %w", err)
		}
	}

	return &Delta[T]{
		initial:           initial,
		reduce:            reducer,
		clone:             cloner,
		snapshotFrequency: config.snapshotFrequency,
		ownedReducerInput: config.ownedReducerInput,
	}, nil
}

// Restore returns an independent channel initialized from a checkpoint value.
func (channel *Delta[T]) Restore(checkpoint CheckpointValue[T]) (*Delta[T], error) {
	restored := channel.specCopy()
	switch checkpoint.Kind {
	case CheckpointMissing:
		restored.value = restored.initial()
	case CheckpointSnapshot, CheckpointLegacy:
		restored.value = restored.clone(checkpoint.Value)
	default:
		return nil, fmt.Errorf(
			"restore delta channel: %w %d",
			ErrInvalidCheckpointKind,
			checkpoint.Kind,
		)
	}
	restored.available = true
	return restored, nil
}

// Copy returns an independent copy of the channel and its current value.
func (channel *Delta[T]) Copy() *Delta[T] {
	copied := channel.specCopy()
	if channel.available {
		copied.value = channel.clone(channel.value)
		copied.available = true
	}
	return copied
}

func (channel *Delta[T]) specCopy() *Delta[T] {
	return &Delta[T]{
		initial:           channel.initial,
		reduce:            channel.reduce,
		clone:             channel.clone,
		snapshotFrequency: channel.snapshotFrequency,
		ownedReducerInput: channel.ownedReducerInput,
	}
}

// SnapshotFrequency returns the configured per-channel update threshold.
func (channel *Delta[T]) SnapshotFrequency() uint64 {
	return channel.snapshotFrequency
}

// IsAvailable reports whether the channel has been restored or updated.
func (channel *Delta[T]) IsAvailable() bool {
	return channel.available
}

// Get returns an isolated copy of the accumulated value.
func (channel *Delta[T]) Get() (T, error) {
	if !channel.available {
		var zero T
		return zero, ErrEmpty
	}
	return channel.clone(channel.value), nil
}

// View returns the accumulated value without cloning. Graph fields use it only
// with an explicit immutable read-view contract.
func (channel *Delta[T]) View() (T, error) {
	if !channel.available {
		var zero T
		return zero, ErrEmpty
	}
	return channel.value, nil
}

// Checkpoint always returns a missing value. The graph checkpoint planner writes a
// full snapshot explicitly when the channel reaches its snapshot cadence.
func (channel *Delta[T]) Checkpoint() CheckpointValue[T] {
	return MissingCheckpoint[T]()
}

// Snapshot returns a full snapshot for persistence by the checkpoint planner.
func (channel *Delta[T]) Snapshot() (CheckpointValue[T], error) {
	value, err := channel.Get()
	if err != nil {
		return MissingCheckpoint[T](), err
	}
	return SnapshotCheckpoint(value), nil
}

// Apply applies one superstep's writes. A single overwrite replaces the value and
// suppresses all normal writes in the same superstep. More than one overwrite is an
// invalid concurrent update.
func (channel *Delta[T]) Apply(writes []DeltaWrite[T]) (bool, error) {
	if len(writes) == 0 {
		return false, nil
	}

	overwriteIndex := -1
	for index, write := range writes {
		if !write.IsOverwrite() {
			continue
		}
		if overwriteIndex != -1 {
			return false, ErrMultipleOverwrites
		}
		overwriteIndex = index
	}

	if overwriteIndex != -1 {
		channel.value = channel.clone(writes[overwriteIndex].Value())
		channel.available = true
		return true, nil
	}

	base := channel.initial()
	if channel.available && channel.ownedReducerInput {
		base = channel.value
	} else if channel.available {
		base = channel.clone(channel.value)
	}
	values := make([]T, 0, len(writes))
	for _, write := range writes {
		value := write.Value()
		if !channel.ownedReducerInput {
			value = channel.clone(value)
		}
		values = append(values, value)
	}
	next, err := channel.reduce(base, values)
	if err != nil {
		return false, fmt.Errorf("reduce delta channel update: %w", err)
	}
	channel.value = next
	channel.available = true
	return true, nil
}

// Replay applies persisted ancestor writes oldest-to-newest. If overwrites are
// present, the last one becomes the new base and only later normal writes are reduced.
// Replays may contain multiple overwrites because they span multiple supersteps.
func (channel *Delta[T]) Replay(writes []DeltaWrite[T]) error {
	if len(writes) == 0 {
		return nil
	}

	base := channel.initial()
	if channel.available {
		base = channel.clone(channel.value)
	}
	start := 0
	for index, write := range writes {
		if write.IsOverwrite() {
			base = channel.clone(write.Value())
			start = index + 1
		}
	}

	remaining := make([]T, 0, len(writes)-start)
	for _, write := range writes[start:] {
		if write.IsOverwrite() {
			continue
		}
		remaining = append(remaining, channel.clone(write.Value()))
	}
	if len(remaining) == 0 {
		channel.value = base
		channel.available = true
		return nil
	}

	next, err := channel.reduce(base, remaining)
	if err != nil {
		return fmt.Errorf("replay delta channel writes: %w", err)
	}
	channel.value = next
	channel.available = true
	return nil
}
