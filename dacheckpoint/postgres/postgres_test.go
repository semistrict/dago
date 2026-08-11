package postgres

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
)

func TestMigrationsMatchPinnedSequence(t *testing.T) {
	statements := MigrationStatements()
	if len(statements) != 10 {
		t.Fatalf("migration count = %d, want 10", len(statements))
	}
	wants := []string{
		"checkpoint_migrations", "CREATE TABLE IF NOT EXISTS checkpoints",
		"checkpoint_blobs", "checkpoint_writes", "DROP not null", "SELECT 1",
		"checkpoints_thread_id_idx", "checkpoint_blobs_thread_id_idx",
		"checkpoint_writes_thread_id_idx", "task_path",
	}
	for index, want := range wants {
		if !strings.Contains(statements[index], want) {
			t.Fatalf("migration %d does not contain %q: %s", index, want, statements[index])
		}
	}
	statements[0] = "mutated"
	if MigrationStatements()[0] == "mutated" {
		t.Fatal("MigrationStatements returned shared storage")
	}
}

func TestSortPendingWritesUsesTaskPathTaskIDIndex(t *testing.T) {
	writes := []dacheckpoint.PendingWrite{
		{TaskPath: "b", TaskID: "a", Index: 0},
		{TaskPath: "a", TaskID: "b", Index: 0},
		{TaskPath: "a", TaskID: "a", Index: 2},
		{TaskPath: "a", TaskID: "a", Index: 1},
	}
	SortPendingWrites(writes)
	got := [][3]any{}
	for _, write := range writes {
		got = append(got, [3]any{write.TaskPath, write.TaskID, write.Index})
	}
	want := [][3]any{{"a", "a", 1}, {"a", "a", 2}, {"a", "b", 0}, {"b", "a", 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
}

func TestSaverIntegration(t *testing.T) {
	dsn := os.Getenv("DAGO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("DAGO_POSTGRES_TEST_DSN is not set")
	}
	saver, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	ctx := context.Background()
	assertLiveSchema(t, saver.db)
	thread := "dago-postgres-integration"
	if err := saver.DeleteThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = saver.DeleteThread(context.Background(), thread) })
	root := dacheckpoint.Config{ThreadID: thread}
	value := dacheckpoint.Checkpoint{
		Version: dacheckpoint.LatestVersion, ID: "00000000-0000-6000-8000-000000000001", Timestamp: "2026-01-01T00:00:00Z",
		ChannelValues:   map[string]any{"inline": "value", "delta": dacheckpoint.DeltaSnapshot{Value: []any{"seed"}}},
		ChannelVersions: map[string]string{"inline": "00000000000000000000000000000001.0.1", "delta": "00000000000000000000000000000001.0.2"},
		VersionsSeen:    map[string]map[string]string{},
	}
	config, err := saver.Put(ctx, root, value, dacheckpoint.Metadata{Source: "input", Step: 0}, value.ChannelVersions)
	if err != nil {
		t.Fatal(err)
	}
	if err := saver.PutWrites(ctx, config, "task", "path", []dacheckpoint.ChannelWrite{{Channel: "delta", Value: []any{"next"}}}); err != nil {
		t.Fatal(err)
	}
	tuple, err := saver.GetTuple(ctx, config)
	if err != nil || tuple == nil {
		t.Fatalf("GetTuple = %#v, %v", tuple, err)
	}
	if tuple.Checkpoint.ChannelValues["inline"] != "value" {
		t.Fatalf("inline value = %#v", tuple.Checkpoint.ChannelValues["inline"])
	}
	if _, ok := tuple.Checkpoint.ChannelValues["delta"].(dacheckpoint.DeltaSnapshot); !ok {
		t.Fatalf("delta value type = %T", tuple.Checkpoint.ChannelValues["delta"])
	}
}

func TestSaverMigratesPinnedOlderSchema(t *testing.T) {
	dsn := os.Getenv("DAGO_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("DAGO_POSTGRES_TEST_DSN is not set")
	}
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	ctx := context.Background()
	const schema = "dago_checkpoint_migration_test"
	if _, err := database.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = database.ExecContext(context.Background(), `SET search_path TO public`)
		_, _ = database.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	if _, err := database.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	for version := 0; version <= 3; version++ {
		if _, err := database.ExecContext(ctx, Migrations[version]); err != nil {
			t.Fatalf("apply old migration %d: %v", version, err)
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO checkpoint_migrations (v) VALUES ($1)`, version); err != nil {
			t.Fatal(err)
		}
	}
	saver := New(database, nil)
	if err := saver.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	assertLiveSchema(t, database)
}

func assertLiveSchema(t *testing.T, database *sql.DB) {
	t.Helper()
	rows, err := database.Query(`SELECT v FROM checkpoint_migrations ORDER BY v`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var migrations []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		migrations = append(migrations, version)
	}
	if want := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}; !reflect.DeepEqual(migrations, want) {
		t.Fatalf("migration rows = %v, want %v", migrations, want)
	}

	expectedColumns := map[string][]string{
		"checkpoints":           {"thread_id", "checkpoint_ns", "checkpoint_id", "parent_checkpoint_id", "type", "checkpoint", "metadata"},
		"checkpoint_blobs":      {"thread_id", "checkpoint_ns", "channel", "version", "type", "blob"},
		"checkpoint_writes":     {"thread_id", "checkpoint_ns", "checkpoint_id", "task_id", "idx", "channel", "type", "blob", "task_path"},
		"checkpoint_migrations": {"v"},
	}
	for table, want := range expectedColumns {
		columnRows, err := database.Query(`SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 ORDER BY ordinal_position`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for columnRows.Next() {
			var column string
			if err := columnRows.Scan(&column); err != nil {
				columnRows.Close()
				t.Fatal(err)
			}
			got = append(got, column)
		}
		columnRows.Close()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s columns = %v, want %v", table, got, want)
		}
	}

	indexRows, err := database.Query(`SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND indexname IN ('checkpoints_thread_id_idx','checkpoint_blobs_thread_id_idx','checkpoint_writes_thread_id_idx') ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defer indexRows.Close()
	var indexes []string
	for indexRows.Next() {
		var name string
		if err := indexRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	wantIndexes := []string{"checkpoint_blobs_thread_id_idx", "checkpoint_writes_thread_id_idx", "checkpoints_thread_id_idx"}
	if !reflect.DeepEqual(indexes, wantIndexes) {
		t.Fatalf("indexes = %v, want %v", indexes, wantIndexes)
	}
}

func TestReadsAndContinuesPinnedPythonSafeFixture(t *testing.T) {
	dsn := os.Getenv("DAGO_PYTHON_POSTGRES_FIXTURE_DSN")
	if dsn == "" {
		t.Skip("DAGO_PYTHON_POSTGRES_FIXTURE_DSN is not set")
	}
	saver, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	ctx := context.Background()
	tuple, err := saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: "python-safe-postgres"})
	if err != nil || tuple == nil {
		t.Fatalf("GetTuple = %#v, %v", tuple, err)
	}
	if tuple.Checkpoint.ChannelValues["scalar"] != "python" {
		t.Fatalf("scalar = %#v", tuple.Checkpoint.ChannelValues["scalar"])
	}
	if !reflect.DeepEqual(tuple.Checkpoint.ChannelValues["bytes"], []byte{0, 1, 's', 'a', 'f', 'e'}) {
		t.Fatalf("bytes = %#v", tuple.Checkpoint.ChannelValues["bytes"])
	}
	messages, ok := tuple.Checkpoint.ChannelValues["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", tuple.Checkpoint.ChannelValues["messages"])
	}
	if _, ok := messages[0].(damessage.Message); !ok {
		t.Fatalf("message type = %T", messages[0])
	}
	if _, ok := tuple.Checkpoint.ChannelValues["delta"].(dacheckpoint.DeltaSnapshot); !ok {
		t.Fatalf("delta type = %T", tuple.Checkpoint.ChannelValues["delta"])
	}
	if len(tuple.PendingWrites) == 0 {
		t.Fatal("pending writes are missing")
	}

	continued := tuple.Checkpoint
	continued.ID = "1f000011-0000-6000-8000-000000000001"
	continued.Timestamp = "2026-08-08T12:11:00+00:00"
	continued.ChannelValues["continued_by_go"] = true
	if _, err := saver.Put(ctx, tuple.Config, continued, dacheckpoint.Metadata{Source: "loop", Step: 1}, nil); err != nil {
		t.Fatal(err)
	}
}
