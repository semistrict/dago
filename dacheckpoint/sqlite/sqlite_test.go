package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/serde"
	"github.com/semistrict/dago/damessage"
)

func TestConstructorsRejectMissingDatabaseOrCodec(t *testing.T) {
	t.Run("database", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("nil database was accepted")
			}
		}()
		New(nil)
	})
	t.Run("codec", func(t *testing.T) {
		var payloadCodec *serde.Safe
		defer func() {
			if recover() == nil {
				t.Fatal("typed-nil codec was accepted")
			}
		}()
		NewWithCodec(&sql.DB{}, payloadCodec)
	})
}

func TestSaverListAppliesFiniteDefaultAndRejectsNegativeLimit(t *testing.T) {
	saver := openTestSaver(t)
	ctx := context.Background()
	root := dacheckpoint.Config{ThreadID: "bounded"}
	for index := 0; index <= dacheckpoint.DefaultListLimit; index++ {
		checkpoint := testCheckpoint(fmt.Sprintf("cp-%03d", index), nil)
		if _, err := saver.Put(ctx, root, checkpoint, dacheckpoint.Metadata{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	tuples, err := saver.List(ctx, &root, dacheckpoint.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tuples) != dacheckpoint.DefaultListLimit {
		t.Fatalf("default list length = %d, want %d", len(tuples), dacheckpoint.DefaultListLimit)
	}
	if _, err := saver.List(ctx, &root, dacheckpoint.ListOptions{Limit: -1}); err == nil {
		t.Fatal("negative list limit was accepted")
	}
}

func TestSchemaMatchesPinnedPythonSaver(t *testing.T) {
	saver := openTestSaver(t)
	expected := map[string][]column{
		"checkpoints": {
			{name: "thread_id", typ: "TEXT", notNull: 1, primaryKey: 1},
			{name: "checkpoint_ns", typ: "TEXT", notNull: 1, defaultValue: "''", primaryKey: 2},
			{name: "checkpoint_id", typ: "TEXT", notNull: 1, primaryKey: 3},
			{name: "parent_checkpoint_id", typ: "TEXT"},
			{name: "type", typ: "TEXT"},
			{name: "checkpoint", typ: "BLOB"},
			{name: "metadata", typ: "BLOB"},
		},
		"writes": {
			{name: "thread_id", typ: "TEXT", notNull: 1, primaryKey: 1},
			{name: "checkpoint_ns", typ: "TEXT", notNull: 1, defaultValue: "''", primaryKey: 2},
			{name: "checkpoint_id", typ: "TEXT", notNull: 1, primaryKey: 3},
			{name: "task_id", typ: "TEXT", notNull: 1, primaryKey: 4},
			{name: "idx", typ: "INTEGER", notNull: 1, primaryKey: 5},
			{name: "channel", typ: "TEXT", notNull: 1},
			{name: "type", typ: "TEXT"},
			{name: "value", typ: "BLOB"},
		},
	}
	for table, want := range expected {
		got := tableColumns(t, saver.db, table)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s columns =\n%+v\nwant\n%+v", table, got, want)
		}
	}
	var journalMode string
	if err := saver.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode error = %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
}

func TestSaverPutGetListAndMetadataFilter(t *testing.T) {
	ctx := context.Background()
	saver := openTestSaver(t)
	root := dacheckpoint.Config{ThreadID: "thread"}
	cp1 := testCheckpoint("cp1", map[string]any{"value": "one"})
	c1, err := saver.Put(ctx, root, cp1, dacheckpoint.Metadata{
		Source: "input", Step: 0, Extra: map[string]any{"tenant": "a"},
	}, nil)
	if err != nil {
		t.Fatalf("Put(cp1) error = %v", err)
	}
	if err := saver.PutWrites(ctx, c1, "task", "ignored", []dacheckpoint.ChannelWrite{
		{Channel: "value", Value: map[string]any{"write": "one"}},
	}); err != nil {
		t.Fatalf("PutWrites() error = %v", err)
	}
	cp2 := testCheckpoint("cp2", map[string]any{"value": "two"})
	c2, err := saver.Put(ctx, c1, cp2, dacheckpoint.Metadata{Source: "loop", Step: 1}, nil)
	if err != nil {
		t.Fatalf("Put(cp2) error = %v", err)
	}

	latest, err := saver.GetTuple(ctx, root)
	if err != nil || latest == nil {
		t.Fatalf("GetTuple(latest) = %+v, %v", latest, err)
	}
	if latest.Config != c2 || latest.Checkpoint.ChannelValues["value"] != "two" {
		t.Fatalf("latest tuple = %+v", latest)
	}
	first, err := saver.GetTuple(ctx, c1)
	if err != nil || first == nil || len(first.PendingWrites) != 1 {
		t.Fatalf("GetTuple(cp1) = %+v, %v", first, err)
	}
	rows, err := saver.List(ctx, &root, dacheckpoint.ListOptions{
		Metadata: map[string]any{"tenant": "a"}, Before: &c2,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Config.CheckpointID != "cp1" {
		t.Fatalf("List() = %+v", rows)
	}
}

func TestSaverWriteConflictBehavior(t *testing.T) {
	ctx := context.Background()
	saver := openTestSaver(t)
	config := dacheckpoint.Config{ThreadID: "thread", CheckpointID: "cp"}
	if _, err := saver.Put(ctx, dacheckpoint.Config{ThreadID: "thread"}, testCheckpoint("cp", nil), dacheckpoint.Metadata{}, nil); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := saver.PutWrites(ctx, config, "task", "", []dacheckpoint.ChannelWrite{
		{Channel: "value", Value: "first"}, {Channel: dacheckpoint.ChannelError, Value: "old"},
	}); err != nil {
		t.Fatalf("PutWrites(first) error = %v", err)
	}
	// Because the batch contains an ordinary write, Python uses INSERT OR IGNORE for
	// the entire batch, including its special row.
	if err := saver.PutWrites(ctx, config, "task", "", []dacheckpoint.ChannelWrite{
		{Channel: "value", Value: "ignored"}, {Channel: dacheckpoint.ChannelError, Value: "also ignored"},
	}); err != nil {
		t.Fatalf("PutWrites(second) error = %v", err)
	}
	// An all-special batch uses replacement.
	if err := saver.PutWrites(ctx, config, "task", "", []dacheckpoint.ChannelWrite{
		{Channel: dacheckpoint.ChannelError, Value: "new"},
	}); err != nil {
		t.Fatalf("PutWrites(special) error = %v", err)
	}
	tuple, err := saver.GetTuple(ctx, config)
	if err != nil {
		t.Fatalf("GetTuple() error = %v", err)
	}
	values := map[string]any{}
	for _, write := range tuple.PendingWrites {
		values[write.Channel] = write.Value
	}
	if values["value"] != "first" || values[dacheckpoint.ChannelError] != "new" {
		t.Fatalf("pending values = %+v", values)
	}
}

func TestSaverDeltaHistoryUsesSeedAncestorAndExcludesTarget(t *testing.T) {
	ctx := context.Background()
	saver := openTestSaver(t)
	root := dacheckpoint.Config{ThreadID: "thread"}
	cp0 := testCheckpoint("cp0", map[string]any{"items": []any{"old"}})
	c0, _ := saver.Put(ctx, root, cp0, dacheckpoint.Metadata{}, nil)
	_ = saver.PutWrites(ctx, c0, "task-0", "", []dacheckpoint.ChannelWrite{{Channel: "items", Value: []any{"subsumed"}}})
	cp1 := testCheckpoint("cp1", map[string]any{"items": dacheckpoint.DeltaSnapshot{Value: []any{"seed"}}})
	c1, _ := saver.Put(ctx, c0, cp1, dacheckpoint.Metadata{}, nil)
	_ = saver.PutWrites(ctx, c1, "task-a", "", []dacheckpoint.ChannelWrite{{Channel: "items", Value: []any{"first"}}})
	_ = saver.PutWrites(ctx, c1, "task-b", "", []dacheckpoint.ChannelWrite{{Channel: "items", Value: []any{"second"}}})
	cp2 := testCheckpoint("cp2", map[string]any{})
	c2, _ := saver.Put(ctx, c1, cp2, dacheckpoint.Metadata{}, nil)
	_ = saver.PutWrites(ctx, c2, "target", "", []dacheckpoint.ChannelWrite{{Channel: "items", Value: []any{"pending"}}})

	histories, err := saver.GetDeltaChannelHistory(ctx, c2, []string{"items"})
	if err != nil {
		t.Fatalf("GetDeltaChannelHistory() error = %v", err)
	}
	history := histories["items"]
	if !history.HasSeed {
		t.Fatal("history has no seed")
	}
	if _, ok := history.Seed.(dacheckpoint.DeltaSnapshot); !ok {
		t.Fatalf("seed type = %T", history.Seed)
	}
	var writes []any
	for _, write := range history.Writes {
		writes = append(writes, write.Value)
	}
	want := []any{[]any{"first"}, []any{"second"}}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %#v, want %#v", writes, want)
	}
}

func TestSaverCopyDeleteAndPrune(t *testing.T) {
	ctx := context.Background()
	saver := openTestSaver(t)
	root := dacheckpoint.Config{ThreadID: "source"}
	c1, _ := saver.Put(ctx, root, testCheckpoint("cp1", nil), dacheckpoint.Metadata{}, nil)
	_, _ = saver.Put(ctx, c1, testCheckpoint("cp2", nil), dacheckpoint.Metadata{}, nil)
	// Add an abandoned fork that keep_latest may safely remove.
	_, _ = saver.Put(ctx, c1, testCheckpoint("branch", nil), dacheckpoint.Metadata{}, nil)
	if err := saver.Prune(ctx, []string{"source"}, dacheckpoint.PruneKeepLatest); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if err := saver.CopyThread(ctx, "source", "target"); err != nil {
		t.Fatalf("CopyThread() error = %v", err)
	}
	if err := saver.DeleteThread(ctx, "source"); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}
	if tuple, err := saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: "target"}); err != nil || tuple == nil {
		t.Fatalf("target GetTuple() = %+v, %v", tuple, err)
	}
	if tuple, err := saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: "source"}); err != nil || tuple != nil {
		t.Fatalf("source GetTuple() = %+v, %v", tuple, err)
	}
}

func TestSaverRejectsUnsafePayloadWithCheckpointContext(t *testing.T) {
	saver := openTestSaver(t)
	value := "unsafe pointer"
	checkpointValue := testCheckpoint("cp", map[string]any{"unsafe": &value})
	_, err := saver.Put(context.Background(), dacheckpoint.Config{ThreadID: "thread"}, checkpointValue, dacheckpoint.Metadata{}, nil)
	if !errors.Is(err, serde.ErrUnsupportedType) {
		t.Fatalf("Put() error = %v, want %v", err, serde.ErrUnsupportedType)
	}
}

func TestReadsAndContinuesPinnedPythonSafeFixture(t *testing.T) {
	fixture := filepath.Join("..", "..", "conformance", "python", "testdata", "python-safe.sqlite")
	if override := os.Getenv("DAGO_PYTHON_SQLITE_FIXTURE"); override != "" {
		fixture = override
	}
	copyPath := filepath.Join(t.TempDir(), "python-safe.sqlite")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	saver, err := Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer saver.Close()
	ctx := context.Background()
	loaded, err := saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: "python-safe"})
	if err != nil || loaded == nil {
		t.Fatalf("GetTuple() = %#v, %v", loaded, err)
	}
	if loaded.Checkpoint.ChannelValues["scalar"] != "python" {
		t.Fatalf("scalar = %#v", loaded.Checkpoint.ChannelValues["scalar"])
	}
	messages, ok := loaded.Checkpoint.ChannelValues["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", loaded.Checkpoint.ChannelValues["messages"])
	}
	parsed, ok := messages[0].(damessage.Message)
	if !ok || parsed.TextContent() != "hello from Python" {
		t.Fatalf("message = %#v", messages[0])
	}
	if _, ok := loaded.Checkpoint.ChannelValues["delta"].(dacheckpoint.DeltaSnapshot); !ok {
		t.Fatalf("delta = %T", loaded.Checkpoint.ChannelValues["delta"])
	}
	if len(loaded.PendingWrites) != 3 {
		t.Fatalf("pending writes = %#v", loaded.PendingWrites)
	}

	continued := loaded.Checkpoint
	continued.ID = "1f000001-0000-6000-8000-000000000002"
	continued.Timestamp = "2026-08-08T12:01:00+00:00"
	continued.ChannelValues = map[string]any{"continued_by_go": true}
	child, err := saver.Put(ctx, loaded.Config, continued, dacheckpoint.Metadata{Source: "loop", Step: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	history, err := saver.GetDeltaChannelHistory(ctx, child, []string{"delta"})
	if err != nil {
		t.Fatal(err)
	}
	if !history["delta"].HasSeed || len(history["delta"].Writes) != 1 {
		t.Fatalf("delta history = %#v", history["delta"])
	}
}

type column struct {
	name         string
	typ          string
	notNull      int
	defaultValue string
	primaryKey   int
}

func tableColumns(t *testing.T, database *sql.DB, table string) []column {
	t.Helper()
	rows, err := database.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s) error = %v", table, err)
	}
	defer rows.Close()
	var result []column
	for rows.Next() {
		var cid int
		var value column
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &value.name, &value.typ, &value.notNull, &defaultValue, &value.primaryKey); err != nil {
			t.Fatalf("table_info(%s) Scan() error = %v", table, err)
		}
		value.defaultValue = defaultValue.String
		result = append(result, value)
	}
	return result
}

func openTestSaver(t *testing.T) *Saver {
	t.Helper()
	saver, err := Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := saver.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return saver
}

func testCheckpoint(id string, values map[string]any) dacheckpoint.Checkpoint {
	if values == nil {
		values = map[string]any{}
	}
	return dacheckpoint.Checkpoint{
		Version: dacheckpoint.LatestVersion, ID: id, Timestamp: id,
		ChannelValues: values, ChannelVersions: map[string]string{},
		VersionsSeen: map[string]map[string]string{},
	}
}
