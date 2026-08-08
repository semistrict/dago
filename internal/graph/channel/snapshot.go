package channel

import "math"

// MaxDeltaSuperstepsSinceSnapshot bounds reconstruction when a channel stops
// receiving writes.
const MaxDeltaSuperstepsSinceSnapshot uint64 = 5000

// DeltaSnapshotCounter tracks updates and total supersteps since a channel's last
// full snapshot.
type DeltaSnapshotCounter struct {
	Updates    uint64
	Supersteps uint64
}

// Advance returns the counter for the next superstep. Counters saturate instead of
// wrapping so a due snapshot cannot become not-due after an extremely long run.
func (counter DeltaSnapshotCounter) Advance(updated bool) DeltaSnapshotCounter {
	counter.Supersteps = saturatingIncrement(counter.Supersteps)
	if updated {
		counter.Updates = saturatingIncrement(counter.Updates)
	}
	return counter
}

// ShouldSnapshot reports whether either the per-channel update cadence or the global
// superstep bound has been reached.
func (counter DeltaSnapshotCounter) ShouldSnapshot(snapshotFrequency uint64) bool {
	return snapshotFrequency > 0 &&
		(counter.Updates >= snapshotFrequency ||
			counter.Supersteps >= MaxDeltaSuperstepsSinceSnapshot)
}

// Reset returns the zero counter used immediately after a full snapshot.
func (counter DeltaSnapshotCounter) Reset() DeltaSnapshotCounter {
	return DeltaSnapshotCounter{}
}

func saturatingIncrement(value uint64) uint64 {
	if value == math.MaxUint64 {
		return value
	}
	return value + 1
}
