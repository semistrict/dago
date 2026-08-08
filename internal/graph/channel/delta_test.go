package channel

import (
	"errors"
	"reflect"
	"testing"
)

func TestDeltaApplyAndCheckpoint(t *testing.T) {
	channel := restoreDelta(t, newStringSliceDelta(t), MissingCheckpoint[[]string]())

	updated, err := channel.Apply([]DeltaWrite[[]string]{
		UpdateValue([]string{"first"}),
		UpdateValue([]string{"second"}),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !updated {
		t.Fatal("Apply() updated = false, want true")
	}
	assertStrings(t, channel, []string{"first", "second"})

	if checkpoint := channel.Checkpoint(); checkpoint.Kind != CheckpointMissing {
		t.Fatalf("Checkpoint().Kind = %v, want CheckpointMissing", checkpoint.Kind)
	}
}

func TestDeltaApplyOverwriteSuppressesSuperstepWrites(t *testing.T) {
	channel := restoreDelta(t, newStringSliceDelta(t), LegacyCheckpoint([]string{"old"}))

	updated, err := channel.Apply([]DeltaWrite[[]string]{
		UpdateValue([]string{"before"}),
		OverwriteValue([]string{"reset"}),
		UpdateValue([]string{"after"}),
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !updated {
		t.Fatal("Apply() updated = false, want true")
	}
	assertStrings(t, channel, []string{"reset"})
}

func TestDeltaApplyRejectsMultipleOverwritesWithoutMutation(t *testing.T) {
	channel := restoreDelta(t, newStringSliceDelta(t), LegacyCheckpoint([]string{"old"}))

	updated, err := channel.Apply([]DeltaWrite[[]string]{
		OverwriteValue([]string{"first"}),
		OverwriteValue([]string{"second"}),
	})
	if !errors.Is(err, ErrMultipleOverwrites) {
		t.Fatalf("Apply() error = %v, want %v", err, ErrMultipleOverwrites)
	}
	if updated {
		t.Fatal("Apply() updated = true, want false")
	}
	assertStrings(t, channel, []string{"old"})
}

func TestDeltaReplayUsesLastOverwriteAsResetPoint(t *testing.T) {
	channel := restoreDelta(t, newStringSliceDelta(t), LegacyCheckpoint([]string{"seed"}))

	err := channel.Replay([]DeltaWrite[[]string]{
		UpdateValue([]string{"discarded"}),
		OverwriteValue([]string{"first-reset"}),
		UpdateValue([]string{"also-discarded"}),
		OverwriteValue([]string{"last-reset"}),
		UpdateValue([]string{"kept"}),
	})
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	assertStrings(t, channel, []string{"last-reset", "kept"})
}

func TestDeltaRestoreDistinguishesMissingFromPresentNil(t *testing.T) {
	reducer := func(current *string, writes []*string) (*string, error) {
		if len(writes) == 0 {
			return current, nil
		}
		return writes[len(writes)-1], nil
	}
	cloner := func(value *string) *string {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	spec, err := NewDelta(func() *string {
		value := "initial"
		return &value
	}, reducer, cloner)
	if err != nil {
		t.Fatalf("NewDelta() error = %v", err)
	}

	missing, err := restoreDelta(t, spec, MissingCheckpoint[*string]()).Get()
	if err != nil {
		t.Fatalf("missing seed Get() error = %v", err)
	}
	if missing == nil || *missing != "initial" {
		t.Fatalf("missing seed value = %v, want initial", missing)
	}

	presentNil, err := restoreDelta(t, spec, LegacyCheckpoint[*string](nil)).Get()
	if err != nil {
		t.Fatalf("nil seed Get() error = %v", err)
	}
	if presentNil != nil {
		t.Fatalf("present nil seed = %v, want nil", *presentNil)
	}
}

func TestDeltaReducerFailureDoesNotReplaceState(t *testing.T) {
	wantErr := errors.New("reducer failed")
	spec, err := NewDelta(
		func() []string { return []string{} },
		func(current []string, writes [][]string) ([]string, error) {
			current = append(current, "mutated clone")
			return nil, wantErr
		},
		cloneStrings,
	)
	if err != nil {
		t.Fatalf("NewDelta() error = %v", err)
	}
	channel := restoreDelta(t, spec, LegacyCheckpoint([]string{"stable"}))

	updated, err := channel.Apply([]DeltaWrite[[]string]{UpdateValue([]string{"write"})})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Apply() error = %v, want %v", err, wantErr)
	}
	if updated {
		t.Fatal("Apply() updated = true, want false")
	}
	assertStrings(t, channel, []string{"stable"})
}

func TestDeltaCopiesValuesAtBoundaries(t *testing.T) {
	seed := []string{"seed"}
	channel := restoreDelta(t, newStringSliceDelta(t), SnapshotCheckpoint(seed))
	seed[0] = "changed outside"
	assertStrings(t, channel, []string{"seed"})

	got, err := channel.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	got[0] = "changed result"
	assertStrings(t, channel, []string{"seed"})

	copy := channel.Copy()
	_, err = copy.Apply([]DeltaWrite[[]string]{UpdateValue([]string{"copy only"})})
	if err != nil {
		t.Fatalf("copy Apply() error = %v", err)
	}
	assertStrings(t, channel, []string{"seed"})
	assertStrings(t, copy, []string{"seed", "copy only"})
}

func TestDeltaStartsUnavailableUntilRestore(t *testing.T) {
	channel := newStringSliceDelta(t)
	if channel.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false")
	}
	if _, err := channel.Get(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Get() error = %v, want %v", err, ErrEmpty)
	}

	restored := restoreDelta(t, channel, MissingCheckpoint[[]string]())
	if !restored.IsAvailable() {
		t.Fatal("restored IsAvailable() = false, want true")
	}
	assertStrings(t, restored, []string{})
}

func TestDeltaRestoreRejectsUnknownCheckpointKind(t *testing.T) {
	channel := newStringSliceDelta(t)
	_, err := channel.Restore(CheckpointValue[[]string]{Kind: CheckpointKind(255)})
	if !errors.Is(err, ErrInvalidCheckpointKind) {
		t.Fatalf("Restore() error = %v, want %v", err, ErrInvalidCheckpointKind)
	}
}

func TestDeltaValidatesConfiguration(t *testing.T) {
	reducer := func(current []string, writes [][]string) ([]string, error) {
		return current, nil
	}

	tests := []struct {
		name    string
		initial func() []string
		reducer DeltaReducer[[]string]
		cloner  Cloner[[]string]
		options []DeltaOption
		wantErr error
	}{
		{name: "missing initial", reducer: reducer, cloner: cloneStrings},
		{name: "missing reducer", initial: func() []string { return nil }, cloner: cloneStrings},
		{name: "missing cloner", initial: func() []string { return nil }, reducer: reducer},
		{
			name:    "zero frequency",
			initial: func() []string { return nil },
			reducer: reducer,
			cloner:  cloneStrings,
			options: []DeltaOption{WithSnapshotFrequency(0)},
			wantErr: ErrInvalidSnapshotFrequency,
		},
		{
			name:    "nil option",
			initial: func() []string { return nil },
			reducer: reducer,
			cloner:  cloneStrings,
			options: []DeltaOption{nil},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewDelta(test.initial, test.reducer, test.cloner, test.options...)
			if err == nil {
				t.Fatal("NewDelta() error = nil, want non-nil")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("NewDelta() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func newStringSliceDelta(t *testing.T) *Delta[[]string] {
	t.Helper()
	reducer := func(current []string, writes [][]string) ([]string, error) {
		result := cloneStrings(current)
		for _, write := range writes {
			result = append(result, write...)
		}
		return result, nil
	}
	channel, err := NewDelta(
		func() []string { return []string{} },
		reducer,
		cloneStrings,
	)
	if err != nil {
		t.Fatalf("NewDelta() error = %v", err)
	}
	return channel
}

func restoreDelta[T any](
	t *testing.T,
	spec *Delta[T],
	checkpoint CheckpointValue[T],
) *Delta[T] {
	t.Helper()
	restored, err := spec.Restore(checkpoint)
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	return restored
}

func cloneStrings(value []string) []string {
	if value == nil {
		return nil
	}
	return append([]string{}, value...)
}

func assertStrings(t *testing.T, channel *Delta[[]string], want []string) {
	t.Helper()
	got, err := channel.Get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get() = %v, want %v", got, want)
	}
}
