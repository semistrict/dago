package dacode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUpdateStateStoreUsesUsefulDefaultsAndPersistsEveryLifecycleField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "update.json")
	store := newUpdateStateStore(path)
	state, err := store.Load(t.Context())
	if err != nil || state != newUpdatePersistentState() {
		t.Fatalf("default state = %#v, %v", state, err)
	}
	now := time.Date(2026, 8, 17, 12, 34, 56, 789, time.FixedZone("offset", 3600))
	state, err = store.Update(t.Context(), func(state *updatePersistentState) error {
		state.AutoUpdateConsent = true
		state.ImplicitDefaultAcknowledged = true
		state.NotifiedVersion, state.NotifiedAt = "v1.2.3", now
		state.SkipOnceVersion, state.SkipVersion = "v1.2.4", "v1.2.5"
		state.CooldownVersion, state.CooldownUntil = "v1.2.6", now.Add(time.Hour)
		state.RestartVersion, state.RestartAttempts, state.RestartedAt = "v1.2.7", 2, now
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(t.Context())
	if err != nil || loaded != state || loaded.NotifiedAt.Location() != time.UTC || loaded.NotifiedAt.Nanosecond() != 0 {
		t.Fatalf("loaded state = %#v, %v", loaded, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, %v", info, err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v, %v", directory, err)
	}
}

func TestUpdateStateStoreRejectsMalformedOversizedAndUnsafeRecords(t *testing.T) {
	for name, payload := range map[string][]byte{
		"unknown":   []byte(`{"version":1,"unknown":true}`),
		"version":   []byte(`{"version":2}`),
		"relation":  []byte(`{"version":1,"notified_at":"2026-08-17T00:00:00Z"}`),
		"value":     []byte(`{"version":1,"skip_version":" bad"}`),
		"oversized": make([]byte, maxUpdateStateBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "update.json")
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := newUpdateStateStore(path).Load(t.Context()); err == nil {
				t.Fatal("unsafe state was accepted")
			}
		})
	}
	t.Run("permissions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "update.json")
		if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := newUpdateStateStore(path).Load(t.Context()); err == nil {
			t.Fatal("non-private state was accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		directory := t.TempDir()
		target, path := filepath.Join(directory, "target.json"), filepath.Join(directory, "update.json")
		if err := os.WriteFile(target, []byte(`{"version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		store := newUpdateStateStore(path)
		if _, err := store.Load(t.Context()); err == nil {
			t.Fatal("symlink state was read")
		}
		if _, err := store.Update(t.Context(), func(*updatePersistentState) error { return nil }); err == nil {
			t.Fatal("symlink state was replaced")
		}
	})
}

func TestUpdateStateStoreCancellationAndMutatorFailureLeavePriorState(t *testing.T) {
	store := newUpdateStateStore(filepath.Join(t.TempDir(), "state", "update.json"))
	if _, err := store.Update(t.Context(), func(state *updatePersistentState) error {
		state.SkipVersion = "v1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(t.Context(), func(state *updatePersistentState) error {
		state.SkipVersion = "v2"
		return errors.New("stop")
	}); err == nil {
		t.Fatal("mutator failure was ignored")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Update(ctx, func(state *updatePersistentState) error {
		state.SkipVersion = "v3"
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled update = %v", err)
	}
	state, err := store.Load(t.Context())
	if err != nil || state.SkipVersion != "v1" {
		t.Fatalf("state after failed updates = %#v, %v", state, err)
	}
}

func TestUpdateStateStoreSerializesConcurrentTransactions(t *testing.T) {
	store := newUpdateStateStore(filepath.Join(t.TempDir(), "state", "update.json"))
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := store.Update(t.Context(), func(state *updatePersistentState) error {
				state.RestartVersion = "v1"
				state.RestartAttempts++
				state.RestartedAt = time.Now()
				return nil
			}); err != nil {
				t.Errorf("update: %v", err)
			}
		})
	}
	group.Wait()
	state, err := store.Load(t.Context())
	if err != nil || state.RestartAttempts != 8 {
		t.Fatalf("concurrent state = %#v, %v", state, err)
	}
}
