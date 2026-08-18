package dahousekeeping

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStateMigratorMovesFilesDirectoriesAndSidecars(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.MkdirAll(filepath.Join(config, "mcp-tokens"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions.db", "sessions.db-wal", "sessions.db-shm"} {
		if err := os.WriteFile(filepath.Join(config, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	migrator := NewStateMigrator(config, state, []string{"mcp-tokens", "sessions.db", "sessions.db-wal", "sessions.db-shm"}, StateMigrationOptions{})
	report := migrator.Migrate(t.Context())
	if report.Version != 1 || report.Moved != 4 || report.Failed != 0 || report.Skipped != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, entry := range report.Entries {
		if entry.Status != MigrationMoved {
			t.Fatalf("entry %+v was not moved", entry)
		}
		if _, err := os.Lstat(filepath.Join(config, entry.Name)); !os.IsNotExist(err) {
			t.Fatalf("legacy entry %q remains: %v", entry.Name, err)
		}
		if _, err := os.Lstat(filepath.Join(state, entry.Name)); err != nil {
			t.Fatalf("destination %q missing: %v", entry.Name, err)
		}
	}

	second := migrator.Migrate(t.Context())
	if second.Moved != 0 || second.Skipped != 4 || second.Failed != 0 {
		t.Fatalf("migration was not idempotent: %+v", second)
	}
}

func TestStateMigratorDoesNotCreateStateDirectoryWithoutPendingEntries(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	report := NewStateMigrator(config, state, []string{"sessions.db"}, StateMigrationOptions{}).Migrate(t.Context())
	if report.Skipped != 1 || report.Entries[0].Status != MigrationMissing {
		t.Fatalf("unexpected report: %+v", report)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("empty migration created state directory: %v", err)
	}
}

func TestLegacyStateNamesAreExactAndCannotBeMutated(t *testing.T) {
	first := LegacyStateNames()
	if first[len(first)-1] != "onboarding_complete" {
		t.Fatalf("unexpected onboarding marker: %q", first[len(first)-1])
	}
	first[0] = "changed"
	if second := LegacyStateNames(); second[0] != "mcp-tokens" {
		t.Fatalf("default names were mutated: %v", second)
	}
}

func TestStateMigratorNeverOverwritesOrFollowsSymlink(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "sessions.db"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "sessions.db"), []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(config, "history.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	report := NewStateMigrator(config, state, []string{"sessions.db", "history.jsonl"}, StateMigrationOptions{}).Migrate(t.Context())
	if report.Entries[0].Status != MigrationDestinationExists || report.Entries[1].Status != MigrationRejected {
		t.Fatalf("unsafe entries were not rejected: %+v", report)
	}
	content, err := os.ReadFile(filepath.Join(state, "sessions.db"))
	if err != nil || string(content) != "current" {
		t.Fatalf("destination was overwritten: %q, %v", content, err)
	}
	content, err = os.ReadFile(outside)
	if err != nil || string(content) != "secret" {
		t.Fatalf("symlink target changed: %q, %v", content, err)
	}
}

func TestStateMigratorKeepsSQLiteGroupTogetherOnPreflightConflict(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sessions.db", "sessions.db-wal", "sessions.db-shm"} {
		if err := os.WriteFile(filepath.Join(config, name), []byte("legacy "+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "sessions.db"), []byte("current database"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := NewStateMigrator(config, state, []string{"sessions.db", "sessions.db-wal", "sessions.db-shm"}, StateMigrationOptions{}).Migrate(t.Context())
	if report.Moved != 0 || report.Failed != 2 || report.Skipped != 1 {
		t.Fatalf("SQLite group was partially accepted: %+v", report)
	}
	for _, name := range []string{"sessions.db", "sessions.db-wal", "sessions.db-shm"} {
		content, err := os.ReadFile(filepath.Join(config, name))
		if err != nil || string(content) != "legacy "+name {
			t.Fatalf("legacy group member %q moved or changed: %q, %v", name, content, err)
		}
	}
}

func TestStateMigratorRejectsOrphanSQLiteSidecars(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "sessions.db-wal"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := NewStateMigrator(config, state, []string{"sessions.db", "sessions.db-wal"}, StateMigrationOptions{}).Migrate(t.Context())
	if report.Moved != 0 || report.Entries[1].Status != MigrationRejected {
		t.Fatalf("orphan sidecar moved: %+v", report)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("orphan sidecar created state directory: %v", err)
	}
}

func TestStateMigratorRejectsSharedExistingStateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Go FileMode does not expose Windows ACL entries")
	}
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "history.jsonl"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := NewStateMigrator(config, state, []string{"history.jsonl"}, StateMigrationOptions{}).Migrate(t.Context())
	if report.Entries[0].Status != MigrationRejected {
		t.Fatalf("shared state directory accepted: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(config, "history.jsonl")); err != nil {
		t.Fatalf("legacy state moved into shared directory: %v", err)
	}
}

func TestStateMigratorConcurrentCallsRemainIdempotent(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	state := filepath.Join(config, ".state")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "history.jsonl"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrator := NewStateMigrator(config, state, []string{"history.jsonl"}, StateMigrationOptions{})
	reports := make(chan MigrationReport, 2)
	go func() { reports <- migrator.Migrate(t.Context()) }()
	go func() { reports <- migrator.Migrate(t.Context()) }()
	first, second := <-reports, <-reports
	if first.Moved+second.Moved != 1 {
		t.Fatalf("concurrent migration moved %d copies: first=%+v second=%+v", first.Moved+second.Moved, first, second)
	}
	content, err := os.ReadFile(filepath.Join(state, "history.jsonl"))
	if err != nil || string(content) != "history" {
		t.Fatalf("concurrent migration lost state: %q, %v", content, err)
	}
}

func TestStateMigratorCancellationAndStaticValidation(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report := NewStateMigrator(config, filepath.Join(config, ".state"), []string{"sessions.db"}, StateMigrationOptions{}).Migrate(ctx)
	if report.Failed != 1 || report.Entries[0].Status != MigrationFailed || !report.Canceled {
		t.Fatalf("cancellation did not fail closed: %+v", report)
	}
	assertPanics(t, func() {
		NewStateMigrator(config, filepath.Join(config, "nested", ".state"), nil, StateMigrationOptions{})
	})
	assertPanics(t, func() {
		NewStateMigrator(config, filepath.Join(config, ".state"), []string{"../escape"}, StateMigrationOptions{})
	})
}

func assertPanics(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	call()
}
