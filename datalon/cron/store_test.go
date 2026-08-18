package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore("assistant", filepath.Join(t.TempDir(), "cron"), Options{})
}

func TestParseSchedule(t *testing.T) {
	t.Parallel()
	tests := map[string]Schedule{
		"in 30m":     {Kind: OneShot, Minutes: 30, Display: "in 30m"},
		" every 2h ": {Kind: Recurring, Minutes: 120, Display: " every 2h "},
	}
	for input, want := range tests {
		got, err := ParseSchedule(input)
		if err != nil {
			t.Fatalf("ParseSchedule(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseSchedule(%q) = %+v, want %+v", input, got, want)
		}
	}
	for _, input := range []string{"", "at noon", "in 0m", "in -1m", "every 1s", "in 999999h", "in 1 m"} {
		if _, err := ParseSchedule(input); !errors.Is(err, ErrInvalidJob) {
			t.Fatalf("ParseSchedule(%q) error = %v", input, err)
		}
	}
}

func TestStorePersistsVersionedPrivateJobs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	schedule, _ := ParseSchedule("in 30m")
	job, err := store.Create(t.Context(), "check status", schedule, Origin{ConversationID: "chat", ChannelID: "telegram"}, CreateOptions{Name: "status", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.ID) != 12 || job.NextRunAt != now.Add(30*time.Minute) {
		t.Fatalf("job = %+v", job)
	}
	for _, path := range []string{filepath.Dir(store.Path()), store.Path()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o700)
		if path == store.Path() {
			want = 0o600
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var persisted diskFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != 1 || len(persisted.Jobs) != 1 {
		t.Fatalf("persisted = %+v", persisted)
	}
}

func TestStoreClaimsOneShotAndRecurringBeforeExecution(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	oneShot, _ := ParseSchedule("in 1m")
	recurring, _ := ParseSchedule("every 15m")
	one, err := store.Create(t.Context(), "once", oneShot, Origin{ConversationID: "chat"}, CreateOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := store.Create(t.Context(), "repeat", recurring, Origin{ConversationID: "chat"}, CreateOptions{RepeatTimes: 2, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.claim(t.Context(), one.ID, now.Add(time.Minute))
	if err != nil || !ok || claimed.Enabled || !claimed.NextRunAt.IsZero() {
		t.Fatalf("one-shot claim = %+v, %v, %v", claimed, ok, err)
	}
	first, ok, err := store.claim(t.Context(), repeat.ID, now.Add(15*time.Minute))
	if err != nil || !ok || first.Repeat.Completed != 1 || first.NextRunAt != now.Add(30*time.Minute) {
		t.Fatalf("first recurring claim = %+v, %v, %v", first, ok, err)
	}
	second, ok, err := store.claim(t.Context(), repeat.ID, now.Add(45*time.Minute))
	if err != nil || !ok || second.Enabled || second.Repeat.Completed != 2 || !second.NextRunAt.IsZero() {
		t.Fatalf("second recurring claim = %+v, %v, %v", second, ok, err)
	}
}

func TestStoreScopesEditsAndRemovals(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	schedule, _ := ParseSchedule("every 5m")
	current := Origin{ConversationID: "current", ChannelID: "telegram"}
	other := Origin{ConversationID: "other", ChannelID: "telegram"}
	job, err := store.Create(t.Context(), "private", schedule, current, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Edit(t.Context(), job.ID, other, EditOptions{Enabled: new(false)}); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-scope edit = %v", err)
	}
	if _, err := store.Remove(t.Context(), job.ID, other); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-scope remove = %v", err)
	}
	updated, err := store.Edit(t.Context(), job.ID, current, EditOptions{Enabled: new(false)})
	if err != nil || updated.Enabled {
		t.Fatalf("scoped edit = %+v, %v", updated, err)
	}
	jobs, err := store.List(t.Context(), &other)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("other list = %+v, %v", jobs, err)
	}
	if _, err := store.Remove(t.Context(), job.ID, current); err != nil {
		t.Fatal(err)
	}
}

func TestStorePrunesOnlyExpiredCompletedJobs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	oneShot, _ := ParseSchedule("in 1m")
	expired, err := store.Create(t.Context(), "expired secret", oneShot, Origin{ConversationID: "old"}, CreateOptions{Now: now.Add(-40 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Create(t.Context(), "fresh", oneShot, Origin{ConversationID: "new"}, CreateOptions{Now: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Create(t.Context(), "active", oneShot, Origin{ConversationID: "active"}, CreateOptions{Now: now.Add(-60 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []Job{expired, fresh} {
		if _, ok, claimErr := store.claim(t.Context(), job.ID, job.NextRunAt); claimErr != nil || !ok {
			t.Fatalf("claim %s = %v, %v", job.ID, ok, claimErr)
		}
	}
	removed, err := store.PruneCompleted(t.Context(), 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].ID != expired.ID {
		t.Fatalf("removed = %#v", removed)
	}
	jobs, err := store.List(t.Context(), nil)
	if err != nil || len(jobs) != 2 || jobs[0].ID != fresh.ID || jobs[1].ID != active.ID {
		t.Fatalf("kept = %#v, %v", jobs, err)
	}
}

func TestStorePruneRejectsNegativeRetentionAndCancellation(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	if _, err := store.PruneCompleted(t.Context(), -time.Second, time.Now()); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("negative retention error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.PruneCompleted(ctx, 0, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestStoreFailsClosedOnCorruptionSpecialFilesAndBounds(t *testing.T) {
	t.Parallel()
	t.Run("corruption", func(t *testing.T) {
		store := newTestStore(t)
		if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.Path(), []byte(`{"version":2,"jobs":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.List(t.Context(), nil); err == nil {
			t.Fatal("unsupported version accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		store := newTestStore(t)
		if err := os.MkdirAll(filepath.Dir(store.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte(`{"version":1,"jobs":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, store.Path()); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := store.List(t.Context(), nil); !errors.Is(err, ErrStoreBound) {
			t.Fatalf("symlink error = %v", err)
		}
	})
	t.Run("limits", func(t *testing.T) {
		store := NewStore("assistant", filepath.Join(t.TempDir(), "cron"), Options{MaxJobs: 1, MaxPromptBytes: 4})
		schedule, _ := ParseSchedule("in 1m")
		if _, err := store.Create(t.Context(), "large", schedule, Origin{ConversationID: "chat"}, CreateOptions{}); !errors.Is(err, ErrInvalidJob) {
			t.Fatalf("large prompt error = %v", err)
		}
		if _, err := store.Create(t.Context(), "one", schedule, Origin{ConversationID: "chat"}, CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(t.Context(), "two", schedule, Origin{ConversationID: "chat"}, CreateOptions{}); !errors.Is(err, ErrStoreBound) {
			t.Fatalf("job bound error = %v", err)
		}
	})
}

func TestStoreCancellationWinsBeforeIO(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v", err)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled List created state: %v", err)
	}
}

func TestStoreSerializesConcurrentCreates(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	schedule, _ := ParseSchedule("every 1m")
	errs := make(chan error, 32)
	for range 32 {
		go func() {
			_, err := store.Create(t.Context(), "tick", schedule, Origin{ConversationID: "chat"}, CreateOptions{})
			errs <- err
		}()
	}
	for range 32 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := store.List(t.Context(), nil)
	if err != nil || len(jobs) != 32 {
		t.Fatalf("jobs = %d, error = %v", len(jobs), err)
	}
}

func TestNewStoreRejectsRequiredStaticInputs(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]func(){
		"assistant": func() { NewStore("../escape", t.TempDir(), Options{}) },
		"directory": func() { NewStore("assistant", "", Options{}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewStore did not panic")
				}
			}()
			call()
		})
	}
}

func TestNewStoreForConfigUsesAssistantCronDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewStoreForConfig(datalon.Config{AssistantID: "helper", StateRoot: root}, Options{})
	want := filepath.Join(root, "helper", "cron", "jobs.json")
	if store.Path() != want {
		t.Fatalf("Path = %q, want %q", store.Path(), want)
	}
}
