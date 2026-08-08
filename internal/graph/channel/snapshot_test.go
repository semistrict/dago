package channel

import (
	"math"
	"testing"
)

func TestDeltaSnapshotCounterUsesEitherThreshold(t *testing.T) {
	tests := []struct {
		name      string
		counter   DeltaSnapshotCounter
		frequency uint64
		want      bool
	}{
		{
			name:      "below both thresholds",
			counter:   DeltaSnapshotCounter{Updates: 2, Supersteps: 10},
			frequency: 3,
			want:      false,
		},
		{
			name:      "update threshold",
			counter:   DeltaSnapshotCounter{Updates: 3, Supersteps: 10},
			frequency: 3,
			want:      true,
		},
		{
			name:      "superstep threshold for inactive channel",
			counter:   DeltaSnapshotCounter{Updates: 0, Supersteps: MaxDeltaSuperstepsSinceSnapshot},
			frequency: 1000,
			want:      true,
		},
		{
			name:      "invalid zero frequency",
			counter:   DeltaSnapshotCounter{Updates: 1, Supersteps: MaxDeltaSuperstepsSinceSnapshot},
			frequency: 0,
			want:      false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.counter.ShouldSnapshot(test.frequency); got != test.want {
				t.Fatalf("ShouldSnapshot() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDeltaSnapshotCounterAdvanceResetAndSaturation(t *testing.T) {
	counter := DeltaSnapshotCounter{}.Advance(false)
	if counter != (DeltaSnapshotCounter{Supersteps: 1}) {
		t.Fatalf("Advance(false) = %+v, want one superstep", counter)
	}
	counter = counter.Advance(true)
	if counter != (DeltaSnapshotCounter{Updates: 1, Supersteps: 2}) {
		t.Fatalf("Advance(true) = %+v, want one update and two supersteps", counter)
	}
	if reset := counter.Reset(); reset != (DeltaSnapshotCounter{}) {
		t.Fatalf("Reset() = %+v, want zero", reset)
	}

	saturated := (DeltaSnapshotCounter{
		Updates:    math.MaxUint64,
		Supersteps: math.MaxUint64,
	}).Advance(true)
	if saturated.Updates != math.MaxUint64 || saturated.Supersteps != math.MaxUint64 {
		t.Fatalf("saturated Advance() = %+v, want max values", saturated)
	}
}
