package channel

import "errors"

var (
	// ErrEmpty is returned when a channel has not been initialized or updated.
	ErrEmpty = errors.New("channel is empty")

	// ErrMultipleOverwrites is returned when one superstep contains more than one
	// overwrite. Concurrent overwrites have no deterministic winner.
	ErrMultipleOverwrites = errors.New("delta channel received multiple overwrites in one superstep")

	// ErrInvalidSnapshotFrequency is returned when a configured snapshot frequency
	// is zero.
	ErrInvalidSnapshotFrequency = errors.New("delta snapshot frequency must be positive")

	// ErrInvalidCheckpointKind is returned when a decoded checkpoint does not carry
	// a supported missing, snapshot, or legacy-value discriminator.
	ErrInvalidCheckpointKind = errors.New("invalid delta checkpoint kind")
)
