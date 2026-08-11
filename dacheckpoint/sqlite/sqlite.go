// Package sqlite implements the standard Python-schema-compatible SQLite saver.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/serde"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS checkpoints (
    thread_id TEXT NOT NULL,
    checkpoint_ns TEXT NOT NULL DEFAULT '',
    checkpoint_id TEXT NOT NULL,
    parent_checkpoint_id TEXT,
    type TEXT,
    checkpoint BLOB,
    metadata BLOB,
    PRIMARY KEY (thread_id, checkpoint_ns, checkpoint_id)
);
CREATE TABLE IF NOT EXISTS writes (
    thread_id TEXT NOT NULL,
    checkpoint_ns TEXT NOT NULL DEFAULT '',
    checkpoint_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    idx INTEGER NOT NULL,
    channel TEXT NOT NULL,
    type TEXT,
    value BLOB,
    PRIMARY KEY (thread_id, checkpoint_ns, checkpoint_id, task_id, idx)
);`

type codec interface {
	Encode(any) (serde.Typed, error)
	Decode(serde.Typed) (any, error)
}

// Saver stores checkpoints in the exact standard SQLite table layout.
type Saver struct {
	db    *sql.DB
	codec codec
	owned bool
	once  sync.Once
	err   error
}

// Open opens a SQLite file and initializes its saver schema.
func Open(path string) (*Saver, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite checkpoint database: %w", err)
	}
	// The standard Python saver serializes access through one connection. Keeping one
	// connection also makes :memory: behave as one database under database/sql.
	database.SetMaxOpenConns(1)
	saver := New(database, nil)
	saver.owned = true
	if err := saver.Setup(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return saver, nil
}

// New wraps an existing database/sql SQLite handle.
func New(database *sql.DB, payloadCodec codec) *Saver {
	if payloadCodec == nil {
		payloadCodec = serde.New(serde.Limits{})
	}
	return &Saver{db: database, codec: payloadCodec}
}

func (saver *Saver) Setup(ctx context.Context) error {
	saver.once.Do(func() {
		if saver.db == nil {
			saver.err = fmt.Errorf("setup SQLite saver: database is nil")
			return
		}
		_, saver.err = saver.db.ExecContext(ctx, schema)
		if saver.err != nil {
			saver.err = fmt.Errorf("setup SQLite saver: %w", saver.err)
		}
	})
	return saver.err
}

func (saver *Saver) Close() error {
	if saver.owned && saver.db != nil {
		return saver.db.Close()
	}
	return nil
}

func (saver *Saver) Put(
	ctx context.Context,
	config dacheckpoint.Config,
	value dacheckpoint.Checkpoint,
	metadata dacheckpoint.Metadata,
	_ map[string]string,
) (dacheckpoint.Config, error) {
	if err := saver.Setup(ctx); err != nil {
		return dacheckpoint.Config{}, err
	}
	if err := config.Validate(); err != nil {
		return dacheckpoint.Config{}, err
	}
	encoded, err := saver.codec.Encode(value)
	if err != nil {
		return dacheckpoint.Config{}, fmt.Errorf("encode SQLite checkpoint %q: %w", value.ID, err)
	}
	metadataData, err := marshalMetadata(metadata)
	if err != nil {
		return dacheckpoint.Config{}, fmt.Errorf("encode SQLite checkpoint metadata: %w", err)
	}
	_, err = saver.db.ExecContext(ctx, `
INSERT OR REPLACE INTO checkpoints
    (thread_id, checkpoint_ns, checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		config.ThreadID, config.Namespace, value.ID, nullable(config.CheckpointID),
		encoded.Type, encoded.Data, metadataData,
	)
	if err != nil {
		return dacheckpoint.Config{}, fmt.Errorf("put SQLite checkpoint %q: %w", value.ID, err)
	}
	return dacheckpoint.Config{
		ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: value.ID,
	}, nil
}

func (saver *Saver) PutWrites(
	ctx context.Context,
	config dacheckpoint.Config,
	taskID string,
	_ string,
	writes []dacheckpoint.ChannelWrite,
) error {
	if err := saver.Setup(ctx); err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.CheckpointID == "" || taskID == "" {
		return fmt.Errorf("put SQLite writes: checkpoint id and task id are required")
	}
	replace := len(writes) > 0
	for _, write := range writes {
		if _, special := dacheckpoint.SpecialWriteIndexes[write.Channel]; !special {
			replace = false
			break
		}
	}
	verb := "INSERT OR IGNORE"
	if replace {
		verb = "INSERT OR REPLACE"
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite writes: %w", err)
	}
	defer transaction.Rollback()
	query := verb + ` INTO writes
    (thread_id, checkpoint_ns, checkpoint_id, task_id, idx, channel, type, value)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	for index, write := range writes {
		encoded, err := saver.codec.Encode(write.Value)
		if err != nil {
			return fmt.Errorf("encode SQLite write %q: %w", write.Channel, err)
		}
		assigned := index
		if special, ok := dacheckpoint.SpecialWriteIndexes[write.Channel]; ok {
			assigned = special
		}
		if _, err := transaction.ExecContext(ctx, query,
			config.ThreadID, config.Namespace, config.CheckpointID, taskID, assigned,
			write.Channel, encoded.Type, encoded.Data,
		); err != nil {
			return fmt.Errorf("put SQLite write %q: %w", write.Channel, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit SQLite writes: %w", err)
	}
	return nil
}

func (saver *Saver) GetTuple(ctx context.Context, config dacheckpoint.Config) (*dacheckpoint.Tuple, error) {
	if err := saver.Setup(ctx); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata
FROM checkpoints WHERE thread_id = ? AND checkpoint_ns = ?`
	arguments := []any{config.ThreadID, config.Namespace}
	if config.CheckpointID != "" {
		query += " AND checkpoint_id = ?"
		arguments = append(arguments, config.CheckpointID)
	} else {
		query += " ORDER BY checkpoint_id DESC LIMIT 1"
	}
	row := saver.db.QueryRowContext(ctx, query, arguments...)
	return saver.scanTuple(ctx, config.ThreadID, config.Namespace, row)
}

type rowScanner interface {
	Scan(...any) error
}

func (saver *Saver) scanTuple(
	ctx context.Context,
	threadID string,
	namespace string,
	row rowScanner,
) (*dacheckpoint.Tuple, error) {
	var id string
	var parent sql.NullString
	var typeTag sql.NullString
	var payload, metadataData []byte
	if err := row.Scan(&id, &parent, &typeTag, &payload, &metadataData); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get SQLite checkpoint: %w", err)
	}
	decoded, err := saver.codec.Decode(serde.Typed{Type: typeTag.String, Data: payload})
	if err != nil {
		return nil, serde.WithContext(err, id, "$checkpoint", typeTag.String)
	}
	value, ok := decoded.(dacheckpoint.Checkpoint)
	if !ok {
		return nil, fmt.Errorf("decode SQLite checkpoint %q: got %T", id, decoded)
	}
	metadata, err := unmarshalMetadata(metadataData)
	if err != nil {
		return nil, fmt.Errorf("decode SQLite checkpoint metadata %q: %w", id, err)
	}
	config := dacheckpoint.Config{ThreadID: threadID, Namespace: namespace, CheckpointID: id}
	writes, err := saver.pendingWrites(ctx, config)
	if err != nil {
		return nil, err
	}
	tuple := &dacheckpoint.Tuple{
		Config: config, Checkpoint: value, Metadata: metadata, PendingWrites: writes,
	}
	if parent.Valid && parent.String != "" {
		parentConfig := dacheckpoint.Config{ThreadID: threadID, Namespace: namespace, CheckpointID: parent.String}
		tuple.Parent = &parentConfig
	}
	return tuple, nil
}

func (saver *Saver) pendingWrites(ctx context.Context, config dacheckpoint.Config) ([]dacheckpoint.PendingWrite, error) {
	rows, err := saver.db.QueryContext(ctx, `
SELECT task_id, idx, channel, type, value FROM writes
WHERE thread_id = ? AND checkpoint_ns = ? AND checkpoint_id = ?
ORDER BY task_id, idx`, config.ThreadID, config.Namespace, config.CheckpointID)
	if err != nil {
		return nil, fmt.Errorf("list SQLite pending writes: %w", err)
	}
	defer rows.Close()
	var result []dacheckpoint.PendingWrite
	for rows.Next() {
		var write dacheckpoint.PendingWrite
		var typeTag sql.NullString
		var payload []byte
		if err := rows.Scan(&write.TaskID, &write.Index, &write.Channel, &typeTag, &payload); err != nil {
			return nil, err
		}
		write.Value, err = saver.codec.Decode(serde.Typed{Type: typeTag.String, Data: payload})
		if err != nil {
			return nil, serde.WithContext(err, config.CheckpointID, write.Channel, typeTag.String)
		}
		result = append(result, write)
	}
	return result, rows.Err()
}

func (saver *Saver) List(
	ctx context.Context,
	config *dacheckpoint.Config,
	options dacheckpoint.ListOptions,
) ([]dacheckpoint.Tuple, error) {
	if err := saver.Setup(ctx); err != nil {
		return nil, err
	}
	query := `SELECT thread_id, checkpoint_ns, checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata FROM checkpoints`
	var clauses []string
	var arguments []any
	if config != nil {
		if err := config.Validate(); err != nil {
			return nil, err
		}
		clauses = append(clauses, "thread_id = ?", "checkpoint_ns = ?")
		arguments = append(arguments, config.ThreadID, config.Namespace)
		if config.CheckpointID != "" {
			clauses = append(clauses, "checkpoint_id = ?")
			arguments = append(arguments, config.CheckpointID)
		}
	}
	if options.Before != nil && options.Before.CheckpointID != "" {
		clauses = append(clauses, "checkpoint_id < ?")
		arguments = append(arguments, options.Before.CheckpointID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + joinAND(clauses)
	}
	query += " ORDER BY checkpoint_id DESC"
	rows, err := saver.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("list SQLite checkpoints: %w", err)
	}
	type listedRow struct {
		threadID, namespace, id string
		parent, typeTag         sql.NullString
		payload, metadata       []byte
	}
	var rawRows []listedRow
	for rows.Next() {
		var row listedRow
		if err := rows.Scan(
			&row.threadID, &row.namespace, &row.id, &row.parent, &row.typeTag,
			&row.payload, &row.metadata,
		); err != nil {
			rows.Close()
			return nil, err
		}
		rawRows = append(rawRows, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var result []dacheckpoint.Tuple
	for _, row := range rawRows {
		metadata, err := unmarshalMetadata(row.metadata)
		if err != nil {
			return nil, err
		}
		if !matchesMetadata(metadata, options.Metadata) {
			continue
		}
		decoded, err := saver.codec.Decode(serde.Typed{Type: row.typeTag.String, Data: row.payload})
		if err != nil {
			return nil, serde.WithContext(err, row.id, "$checkpoint", row.typeTag.String)
		}
		value, ok := decoded.(dacheckpoint.Checkpoint)
		if !ok {
			return nil, fmt.Errorf("decode SQLite checkpoint %q: got %T", row.id, decoded)
		}
		rowConfig := dacheckpoint.Config{ThreadID: row.threadID, Namespace: row.namespace, CheckpointID: row.id}
		writes, err := saver.pendingWrites(ctx, rowConfig)
		if err != nil {
			return nil, err
		}
		tuple := dacheckpoint.Tuple{Config: rowConfig, Checkpoint: value, Metadata: metadata, PendingWrites: writes}
		if row.parent.Valid && row.parent.String != "" {
			parentConfig := dacheckpoint.Config{ThreadID: row.threadID, Namespace: row.namespace, CheckpointID: row.parent.String}
			tuple.Parent = &parentConfig
		}
		result = append(result, tuple)
		if options.Limit > 0 && len(result) >= options.Limit {
			break
		}
	}
	return result, nil
}

func (saver *Saver) DeleteThread(ctx context.Context, threadID string) error {
	if err := saver.Setup(ctx); err != nil {
		return err
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, "DELETE FROM checkpoints WHERE thread_id = ?", threadID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM writes WHERE thread_id = ?", threadID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (saver *Saver) CopyThread(ctx context.Context, sourceThreadID, targetThreadID string) error {
	if err := saver.Setup(ctx); err != nil {
		return err
	}
	if sourceThreadID == "" || targetThreadID == "" || sourceThreadID == targetThreadID {
		return fmt.Errorf("copy SQLite thread: distinct source and target ids are required")
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var exists int
	if err := transaction.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkpoints WHERE thread_id = ?", targetThreadID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return fmt.Errorf("copy SQLite thread: target %q already exists", targetThreadID)
	}
	if _, err := transaction.ExecContext(ctx, `
INSERT INTO checkpoints (thread_id, checkpoint_ns, checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata)
SELECT ?, checkpoint_ns, checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata
FROM checkpoints WHERE thread_id = ?`, targetThreadID, sourceThreadID); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `
INSERT INTO writes (thread_id, checkpoint_ns, checkpoint_id, task_id, idx, channel, type, value)
SELECT ?, checkpoint_ns, checkpoint_id, task_id, idx, channel, type, value
FROM writes WHERE thread_id = ?`, targetThreadID, sourceThreadID); err != nil {
		return err
	}
	return transaction.Commit()
}

func (saver *Saver) Prune(ctx context.Context, threadIDs []string, strategy dacheckpoint.PruneStrategy) error {
	if strategy == dacheckpoint.PruneDelete {
		for _, threadID := range threadIDs {
			if err := saver.DeleteThread(ctx, threadID); err != nil {
				return err
			}
		}
		return nil
	}
	if strategy != dacheckpoint.PruneKeepLatest {
		return fmt.Errorf("%w %q", dacheckpoint.ErrUnsupportedPrune, strategy)
	}
	if err := saver.Setup(ctx); err != nil {
		return err
	}
	for _, threadID := range threadIDs {
		if err := saver.pruneThread(ctx, threadID); err != nil {
			return err
		}
	}
	return nil
}

func (saver *Saver) pruneThread(ctx context.Context, threadID string) error {
	rows, err := saver.db.QueryContext(ctx, `
SELECT checkpoint_ns, checkpoint_id, parent_checkpoint_id
FROM checkpoints WHERE thread_id = ? ORDER BY checkpoint_ns, checkpoint_id DESC`, threadID)
	if err != nil {
		return err
	}
	type record struct{ id, parent string }
	byNamespace := map[string][]record{}
	for rows.Next() {
		var namespace, id string
		var parent sql.NullString
		if err := rows.Scan(&namespace, &id, &parent); err != nil {
			rows.Close()
			return err
		}
		byNamespace[namespace] = append(byNamespace[namespace], record{id: id, parent: parent.String})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for namespace, records := range byNamespace {
		if len(records) == 0 {
			continue
		}
		parents := make(map[string]string, len(records))
		for _, record := range records {
			parents[record.id] = record.parent
		}
		keep := map[string]struct{}{}
		for id := records[0].id; id != ""; id = parents[id] {
			if _, exists := keep[id]; exists {
				break
			}
			keep[id] = struct{}{}
		}
		for _, record := range records {
			if _, ok := keep[record.id]; ok {
				continue
			}
			if _, err := transaction.ExecContext(ctx, `DELETE FROM writes WHERE thread_id = ? AND checkpoint_ns = ? AND checkpoint_id = ?`, threadID, namespace, record.id); err != nil {
				return err
			}
			if _, err := transaction.ExecContext(ctx, `DELETE FROM checkpoints WHERE thread_id = ? AND checkpoint_ns = ? AND checkpoint_id = ?`, threadID, namespace, record.id); err != nil {
				return err
			}
		}
	}
	return transaction.Commit()
}

func (saver *Saver) GetDeltaChannelHistory(
	ctx context.Context,
	config dacheckpoint.Config,
	channels []string,
) (map[string]dacheckpoint.DeltaHistory, error) {
	if err := saver.Setup(ctx); err != nil {
		return nil, err
	}
	result := make(map[string]dacheckpoint.DeltaHistory, len(channels))
	remaining := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		remaining[channel] = struct{}{}
		result[channel] = dacheckpoint.DeltaHistory{}
	}
	if len(remaining) == 0 {
		return result, nil
	}
	target, err := saver.GetTuple(ctx, config)
	if err != nil || target == nil {
		return result, err
	}
	current := target.Parent
	for current != nil && len(remaining) > 0 {
		tuple, err := saver.GetTuple(ctx, *current)
		if err != nil {
			return nil, err
		}
		if tuple == nil {
			break
		}
		terminated := make([]string, 0)
		for channel := range remaining {
			if seed, ok := tuple.Checkpoint.ChannelValues[channel]; ok {
				history := result[channel]
				history.Seed, history.HasSeed = seed, true
				result[channel] = history
				terminated = append(terminated, channel)
			}
		}
		for index := len(tuple.PendingWrites) - 1; index >= 0; index-- {
			write := tuple.PendingWrites[index]
			if _, ok := remaining[write.Channel]; ok {
				history := result[write.Channel]
				history.Writes = append(history.Writes, write)
				result[write.Channel] = history
			}
		}
		for _, channel := range terminated {
			delete(remaining, channel)
		}
		current = tuple.Parent
	}
	for channel, history := range result {
		for left, right := 0, len(history.Writes)-1; left < right; left, right = left+1, right-1 {
			history.Writes[left], history.Writes[right] = history.Writes[right], history.Writes[left]
		}
		result[channel] = history
	}
	return result, nil
}

func (saver *Saver) NextVersion(current string) (string, error) {
	return dacheckpoint.NextVersion(current)
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func joinAND(clauses []string) string {
	result := ""
	for index, clause := range clauses {
		if index > 0 {
			result += " AND "
		}
		result += clause
	}
	return result
}

func matchesMetadata(metadata dacheckpoint.Metadata, filter map[string]any) bool {
	if len(filter) == 0 {
		return true
	}
	values := map[string]any{"source": metadata.Source, "step": metadata.Step, "run_id": metadata.RunID}
	for key, value := range metadata.Extra {
		values[key] = value
	}
	for key, want := range filter {
		if !reflect.DeepEqual(values[key], want) {
			return false
		}
	}
	return true
}

func marshalMetadata(metadata dacheckpoint.Metadata) ([]byte, error) {
	value := make(map[string]any, len(metadata.Extra)+5)
	for key, item := range metadata.Extra {
		value[key] = item
	}
	value["source"], value["step"] = metadata.Source, metadata.Step
	if len(metadata.Parents) > 0 {
		value["parents"] = metadata.Parents
	}
	if metadata.RunID != "" {
		value["run_id"] = metadata.RunID
	}
	if len(metadata.CountersSinceDeltaSnapshot) > 0 {
		counters := make(map[string][2]uint64, len(metadata.CountersSinceDeltaSnapshot))
		for key, counter := range metadata.CountersSinceDeltaSnapshot {
			counters[key] = [2]uint64{counter.Updates, counter.Supersteps}
		}
		value["counters_since_delta_snapshot"] = counters
	}
	return json.Marshal(value)
}

func unmarshalMetadata(data []byte) (dacheckpoint.Metadata, error) {
	if len(data) == 0 {
		return dacheckpoint.Metadata{}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return dacheckpoint.Metadata{}, err
	}
	metadata := dacheckpoint.Metadata{Extra: map[string]any{}}
	for key, value := range raw {
		switch key {
		case "source":
			_ = json.Unmarshal(value, &metadata.Source)
		case "step":
			_ = json.Unmarshal(value, &metadata.Step)
		case "parents":
			_ = json.Unmarshal(value, &metadata.Parents)
		case "run_id":
			_ = json.Unmarshal(value, &metadata.RunID)
		case "counters_since_delta_snapshot":
			var counters map[string][2]uint64
			if err := json.Unmarshal(value, &counters); err != nil {
				return dacheckpoint.Metadata{}, err
			}
			metadata.CountersSinceDeltaSnapshot = make(map[string]dacheckpoint.DeltaCounter, len(counters))
			for channel, counter := range counters {
				metadata.CountersSinceDeltaSnapshot[channel] = dacheckpoint.DeltaCounter{Updates: counter[0], Supersteps: counter[1]}
			}
		default:
			var item any
			if err := json.Unmarshal(value, &item); err != nil {
				return dacheckpoint.Metadata{}, err
			}
			metadata.Extra[key] = item
		}
	}
	if len(metadata.Extra) == 0 {
		metadata.Extra = nil
	}
	return metadata, nil
}

// SchemaSQL returns the exact idempotent schema setup script for introspection tests.
func SchemaSQL() string { return schema }

// SortPendingWrites exposes the Python ordering rule for package conformance tests.
func SortPendingWrites(writes []dacheckpoint.PendingWrite) {
	sort.SliceStable(writes, func(i, j int) bool {
		if writes[i].TaskID != writes[j].TaskID {
			return writes[i].TaskID < writes[j].TaskID
		}
		return writes[i].Index < writes[j].Index
	})
}
