package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/datalon/cron"
)

type fakeCronStore struct {
	mu      sync.Mutex
	jobs    []cron.Job
	listErr error
	list    func(context.Context)
	prune   func(context.Context)
}

func (store *fakeCronStore) List(ctx context.Context, _ *cron.Origin) ([]cron.Job, error) {
	if store.list != nil {
		store.list(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]cron.Job(nil), store.jobs...), store.listErr
}

func (store *fakeCronStore) PruneCompleted(ctx context.Context, retainFor time.Duration, now time.Time) ([]cron.Job, error) {
	if store.prune != nil {
		store.prune(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	removed := completedJobs(store.jobs, now.Add(-retainFor))
	removedIDs := make(map[string]struct{}, len(removed))
	for _, job := range removed {
		removedIDs[job.ID] = struct{}{}
	}
	kept := make([]cron.Job, 0, len(store.jobs)-len(removed))
	for _, job := range store.jobs {
		if _, ok := removedIDs[job.ID]; !ok {
			kept = append(kept, job)
		}
	}
	store.jobs = kept
	return removed, nil
}

func TestDryRunAndCleanApplyPinnedRetentionWithoutLeakingData(t *testing.T) {
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	state := filepath.Join(t.TempDir(), "assistant")
	media := filepath.Join(state, "media", "inbound", "nested")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(media, "private-message-id.ogg")
	fresh := filepath.Join(media, "fresh.ogg")
	writeAt(t, old, "old secret media", now.Add(-25*time.Hour), 0o644)
	writeAt(t, fresh, "fresh", now.Add(-time.Hour), 0o644)
	store := &fakeCronStore{jobs: []cron.Job{
		{ID: "expired-secret-id", Prompt: "private prompt", Enabled: false, CreatedAt: now.Add(-40 * 24 * time.Hour)},
		{ID: "fresh", Enabled: false, CreatedAt: now.Add(-time.Hour)},
		{ID: "active", Enabled: true, CreatedAt: now.Add(-60 * 24 * time.Hour), NextRunAt: now.Add(time.Hour)},
	}}
	manager := New(state, store, Options{})
	dry, err := manager.DryRunAt(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.CompletedCronJobs != 1 || dry.Files != 1 || dry.Bytes != int64(len("old secret media")) || len(dry.Entries) != 2 {
		t.Fatalf("dry report = %#v", dry)
	}
	encoded, err := json.Marshal(dry)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-message-id", "expired-secret-id", "private prompt", state} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report leaked %q: %s", secret, encoded)
		}
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("dry run changed old media: %v", err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("dry run changed permissions: %v, %v", info, err)
	}

	report, err := manager.CleanAt(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.DryRun || report.CompletedCronJobs != 1 || report.Files != 1 || report.SecuredFiles != 2 || report.SecuredDirectories == 0 {
		t.Fatalf("cleanup report = %#v", report)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired media remains: %v", err)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fresh media = %v, %v", info, err)
	}
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state root = %v, %v", info, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.jobs) != 2 || store.jobs[0].ID != "fresh" || store.jobs[1].ID != "active" {
		t.Fatalf("remaining jobs = %#v", store.jobs)
	}
}

func TestWalkFailsClosedBeforeDeletionOnLinksAndLimits(t *testing.T) {
	now := time.Now().UTC()
	for name, prepare := range map[string]func(*testing.T, string){
		"symlink": func(t *testing.T, media string) {
			target := filepath.Join(t.TempDir(), "outside")
			writeAt(t, target, "secret", now.Add(-48*time.Hour), 0o600)
			if err := os.Symlink(target, filepath.Join(media, "escape")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
		},
		"entry bound": func(t *testing.T, media string) {
			writeAt(t, filepath.Join(media, "second"), "two", now.Add(-48*time.Hour), 0o600)
		},
		"file bound": func(t *testing.T, media string) {
			writeAt(t, filepath.Join(media, "large"), "large", now.Add(-48*time.Hour), 0o600)
		},
		"depth bound": func(t *testing.T, media string) {
			if err := os.MkdirAll(filepath.Join(media, "one", "two"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"selected bytes": func(t *testing.T, media string) {
			writeAt(t, filepath.Join(media, "second"), "two", now.Add(-48*time.Hour), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "assistant")
			media := filepath.Join(state, "media", "inbound")
			if err := os.MkdirAll(media, 0o700); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(media, "first")
			writeAt(t, old, "one", now.Add(-48*time.Hour), 0o600)
			prepare(t, media)
			options := Options{}
			if name == "entry bound" {
				options.MaxWalkEntries = 2
			}
			if name == "file bound" {
				options.MaxArtifactBytes = 4
			}
			if name == "depth bound" {
				options.MaxDepth = 1
			}
			if name == "selected bytes" {
				options.MaxSelectedBytes = 5
			}
			_, err := New(state, &fakeCronStore{}, options).CleanAt(t.Context(), now)
			if err == nil || name == "symlink" && !errors.Is(err, ErrUnsafeState) || name != "symlink" && !errors.Is(err, ErrLifecycleLimit) {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Stat(old); err != nil {
				t.Fatalf("valid file was deleted before failed walk: %v", err)
			}
		})
	}
}

func TestCancellationStopsBeforeAndDuringMutation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "assistant")
	media := filepath.Join(state, "media", "inbound")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(media, "old")
	writeAt(t, old, "old", time.Now().Add(-48*time.Hour), 0o600)
	manager := New(state, &fakeCronStore{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Clean(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel error = %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	store := &fakeCronStore{prune: func(context.Context) { cancel() }}
	manager = New(state, store, Options{})
	if _, err := manager.Clean(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-clean cancellation = %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("media deleted after cancellation: %v", err)
	}
}

func TestPathReplacementIsRejectedBeforeMutation(t *testing.T) {
	now := time.Now().UTC()
	state := filepath.Join(t.TempDir(), "assistant")
	media := filepath.Join(state, "media", "inbound")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(media, "old")
	modified := now.Add(-48 * time.Hour)
	writeAt(t, old, "old", modified, 0o600)
	store := &fakeCronStore{list: func(context.Context) {
		if err := os.Remove(old); err != nil {
			t.Fatal(err)
		}
		writeAt(t, old, "new", modified, 0o600)
	}}
	if _, err := New(state, store, Options{}).CleanAt(t.Context(), now); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("replacement error = %v", err)
	}
	data, err := os.ReadFile(old)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement was deleted: %q, %v", data, err)
	}
}

func TestExplicitArtifactPoliciesAreConfinedAndAcknowledged(t *testing.T) {
	state := filepath.Join(t.TempDir(), "assistant")
	traces := filepath.Join(state, "traces")
	if err := os.MkdirAll(traces, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(traces, "old.json")
	writeAt(t, old, "trace", time.Now().Add(-time.Hour), 0o600)
	manager := New(state, &fakeCronStore{}, Options{FilePolicies: []FilePolicy{{Kind: ArtifactTracing, RelativeRoot: "traces", RetainFor: time.Minute}}})
	report, err := manager.Clean(t.Context())
	if err != nil || report.Files != 1 {
		t.Fatalf("trace cleanup = %#v, %v", report, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trace remains: %v", err)
	}

	for index, call := range []func(){
		func() { New("relative", &fakeCronStore{}, Options{}) },
		func() { New(state, nil, Options{}) },
		func() { New(state, &fakeCronStore{}, Options{CronRetention: -1}) },
		func() {
			New(state, &fakeCronStore{}, Options{FilePolicies: []FilePolicy{{Kind: ArtifactTracing, RelativeRoot: "../outside"}}})
		},
		func() {
			New(state, &fakeCronStore{}, Options{FilePolicies: []FilePolicy{{Kind: ArtifactSession, RelativeRoot: "channels/whatsapp", RetainFor: time.Hour}}})
		},
		func() {
			New(state, &fakeCronStore{}, Options{FilePolicies: []FilePolicy{{Kind: ArtifactChannel, RelativeRoot: "channels/whatsapp", RetainFor: time.Hour}}})
		},
	} {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("static constructor input did not panic")
				}
			}()
			call()
		})
	}
	_ = New(state, &fakeCronStore{}, Options{FilePolicies: []FilePolicy{{
		Kind: ArtifactSession, RelativeRoot: "channels/whatsapp", RetainFor: time.Hour,
		Acknowledgement: SessionDeletionAcknowledgement,
	}}})
	_ = New(state, &fakeCronStore{}, Options{FilePolicies: []FilePolicy{{
		Kind: ArtifactChannel, RelativeRoot: "channels/whatsapp/artifacts", RetainFor: time.Hour,
	}}})
}

func TestReportEntriesAreBounded(t *testing.T) {
	state := filepath.Join(t.TempDir(), "assistant")
	media := filepath.Join(state, "media", "inbound")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two", "three"} {
		writeAt(t, filepath.Join(media, name), name, time.Now().Add(-48*time.Hour), 0o600)
	}
	report, err := New(state, &fakeCronStore{}, Options{MaxReportEntries: 1}).DryRun(t.Context())
	if err != nil || len(report.Entries) != 1 || report.TruncatedEntries != 2 || report.Files != 3 {
		t.Fatalf("report = %#v, %v", report, err)
	}
}

func TestImmediateRetentionAndCronResultBounds(t *testing.T) {
	now := time.Now().UTC()
	state := filepath.Join(t.TempDir(), "assistant")
	media := filepath.Join(state, "media", "inbound")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(media, "current")
	writeAt(t, current, "value", now, 0o600)
	store := &fakeCronStore{jobs: []cron.Job{{ID: "one", Enabled: true, CreatedAt: now}, {ID: "two", Enabled: true, CreatedAt: now}}}
	if _, err := New(state, store, Options{MaxCronJobs: 1}).DryRunAt(t.Context(), now); !errors.Is(err, ErrLifecycleLimit) {
		t.Fatalf("cron bound error = %v", err)
	}
	report, err := New(state, &fakeCronStore{}, Options{ImmediateMediaCleanup: true}).CleanAt(t.Context(), now)
	if err != nil || report.Files != 1 {
		t.Fatalf("immediate cleanup = %#v, %v", report, err)
	}
	if _, err := os.Stat(current); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current artifact remains: %v", err)
	}
}

func TestOptionsFromEnvMatchesPinnedVariables(t *testing.T) {
	options, err := OptionsFromEnv(map[string]string{
		CronRetentionEnv: "0", MediaRetentionEnv: "12", MaxMediaBytesEnv: "12345",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !options.ImmediateCronCleanup || options.MediaRetention != 12*time.Hour || options.MaxArtifactBytes != 12345 {
		t.Fatalf("options = %#v", options)
	}
	for _, env := range []map[string]string{
		{CronRetentionEnv: "-1"}, {MediaRetentionEnv: "invalid"}, {MaxMediaBytesEnv: "0"},
	} {
		if _, err := OptionsFromEnv(env); err == nil {
			t.Fatalf("invalid env passed: %#v", env)
		}
	}
}

func TestManagerUsesConcreteCronStore(t *testing.T) {
	state := filepath.Join(t.TempDir(), "assistant")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	store := cron.NewStore("assistant", filepath.Join(state, "cron"), cron.Options{})
	report, err := New(state, store, Options{}).DryRun(t.Context())
	if err != nil || report.CompletedCronJobs != 0 || report.Files != 0 {
		t.Fatalf("concrete store dry run = %#v, %v", report, err)
	}
}

func writeAt(t *testing.T, path, content string, modified time.Time, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}
