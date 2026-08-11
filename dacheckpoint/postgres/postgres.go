// Package postgres implements the standard Python-schema-compatible PostgreSQL saver.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/serde"
)

// Migrations preserves the pinned upstream numbering and history exactly.
var Migrations = []string{
	`CREATE TABLE IF NOT EXISTS checkpoint_migrations (
    v INTEGER PRIMARY KEY
);`,
	`CREATE TABLE IF NOT EXISTS checkpoints (
    thread_id TEXT NOT NULL,
    checkpoint_ns TEXT NOT NULL DEFAULT '',
    checkpoint_id TEXT NOT NULL,
    parent_checkpoint_id TEXT,
    type TEXT,
    checkpoint JSONB NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (thread_id, checkpoint_ns, checkpoint_id)
);`,
	`CREATE TABLE IF NOT EXISTS checkpoint_blobs (
    thread_id TEXT NOT NULL,
    checkpoint_ns TEXT NOT NULL DEFAULT '',
    channel TEXT NOT NULL,
    version TEXT NOT NULL,
    type TEXT NOT NULL,
    blob BYTEA,
    PRIMARY KEY (thread_id, checkpoint_ns, channel, version)
);`,
	`CREATE TABLE IF NOT EXISTS checkpoint_writes (
    thread_id TEXT NOT NULL,
    checkpoint_ns TEXT NOT NULL DEFAULT '',
    checkpoint_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    idx INTEGER NOT NULL,
    channel TEXT NOT NULL,
    type TEXT,
    blob BYTEA NOT NULL,
    PRIMARY KEY (thread_id, checkpoint_ns, checkpoint_id, task_id, idx)
);`,
	`ALTER TABLE checkpoint_blobs ALTER COLUMN blob DROP not null;`,
	`SELECT 1;`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS checkpoints_thread_id_idx ON checkpoints(thread_id);`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS checkpoint_blobs_thread_id_idx ON checkpoint_blobs(thread_id);`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS checkpoint_writes_thread_id_idx ON checkpoint_writes(thread_id);`,
	`ALTER TABLE checkpoint_writes ADD COLUMN IF NOT EXISTS task_path TEXT NOT NULL DEFAULT '';`,
}

type codec interface {
	Encode(any) (serde.Typed, error)
	Decode(serde.Typed) (any, error)
}

// Saver owns or wraps a database/sql PostgreSQL pool.
type Saver struct {
	db    *sql.DB
	codec codec
	owned bool
	mu    sync.Mutex
	ready bool
}

func Open(dsn string) (*Saver, error) {
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL checkpoint database: %w", err)
	}
	saver := New(database, nil)
	saver.owned = true
	if err := saver.Setup(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return saver, nil
}

func New(database *sql.DB, payloadCodec codec) *Saver {
	if payloadCodec == nil {
		payloadCodec = serde.New(serde.Limits{})
	}
	return &Saver{db: database, codec: payloadCodec}
}

func (saver *Saver) Setup(ctx context.Context) error {
	saver.mu.Lock()
	defer saver.mu.Unlock()
	if saver.ready {
		return nil
	}
	if saver.db == nil {
		return fmt.Errorf("setup PostgreSQL saver: database is nil")
	}
	if _, err := saver.db.ExecContext(ctx, Migrations[0]); err != nil {
		return fmt.Errorf("apply PostgreSQL checkpoint migration 0: %w", err)
	}
	if _, err := saver.db.ExecContext(ctx,
		`INSERT INTO checkpoint_migrations (v) VALUES (0) ON CONFLICT (v) DO NOTHING`); err != nil {
		return err
	}
	for version := 1; version < len(Migrations); version++ {
		var applied bool
		if err := saver.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM checkpoint_migrations WHERE v = $1)`, version,
		).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		if _, err := saver.db.ExecContext(ctx, Migrations[version]); err != nil {
			return fmt.Errorf("apply PostgreSQL checkpoint migration %d: %w", version, err)
		}
		if _, err := saver.db.ExecContext(ctx,
			`INSERT INTO checkpoint_migrations (v) VALUES ($1) ON CONFLICT (v) DO NOTHING`, version,
		); err != nil {
			return err
		}
	}
	saver.ready = true
	return nil
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
	newVersions map[string]string,
) (dacheckpoint.Config, error) {
	if err := saver.Setup(ctx); err != nil {
		return dacheckpoint.Config{}, err
	}
	if err := config.Validate(); err != nil {
		return dacheckpoint.Config{}, err
	}
	inline, blobs, err := saver.splitCheckpoint(value)
	if err != nil {
		return dacheckpoint.Config{}, fmt.Errorf("prepare PostgreSQL checkpoint %q: %w", value.ID, err)
	}
	checkpointJSON, err := json.Marshal(inline)
	if err != nil {
		return dacheckpoint.Config{}, err
	}
	metadataJSON, err := marshalMetadata(metadata)
	if err != nil {
		return dacheckpoint.Config{}, err
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return dacheckpoint.Config{}, err
	}
	defer transaction.Rollback()
	for channel, item := range blobs {
		version, changed := newVersions[channel]
		if !changed {
			continue
		}
		if _, err := transaction.ExecContext(ctx, `
INSERT INTO checkpoint_blobs (thread_id, checkpoint_ns, channel, version, type, blob)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (thread_id, checkpoint_ns, channel, version) DO NOTHING`,
			config.ThreadID, config.Namespace, channel, version, item.Type, nullableBytes(item.Data),
		); err != nil {
			return dacheckpoint.Config{}, fmt.Errorf("put PostgreSQL checkpoint blob %q: %w", channel, err)
		}
	}
	if _, err := transaction.ExecContext(ctx, `
INSERT INTO checkpoints (thread_id, checkpoint_ns, checkpoint_id, parent_checkpoint_id, checkpoint, metadata)
VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb)
ON CONFLICT (thread_id, checkpoint_ns, checkpoint_id)
DO UPDATE SET checkpoint = EXCLUDED.checkpoint, metadata = EXCLUDED.metadata`,
		config.ThreadID, config.Namespace, value.ID, nullableString(config.CheckpointID),
		checkpointJSON, metadataJSON,
	); err != nil {
		return dacheckpoint.Config{}, fmt.Errorf("put PostgreSQL checkpoint %q: %w", value.ID, err)
	}
	if err := transaction.Commit(); err != nil {
		return dacheckpoint.Config{}, err
	}
	return dacheckpoint.Config{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: value.ID}, nil
}

func (saver *Saver) splitCheckpoint(value dacheckpoint.Checkpoint) (dacheckpoint.Checkpoint, map[string]serde.Typed, error) {
	inline := value
	inline.ChannelValues = make(map[string]any, len(value.ChannelValues))
	blobs := make(map[string]serde.Typed)
	for channel, item := range value.ChannelValues {
		if snapshot, ok := item.(dacheckpoint.DeltaSnapshot); ok {
			encoded, err := saver.codec.Encode(snapshot)
			if err != nil {
				return dacheckpoint.Checkpoint{}, nil, err
			}
			blobs[channel] = encoded
			inline.ChannelValues[channel] = true
			continue
		}
		if isInlinePrimitive(item) {
			inline.ChannelValues[channel] = item
			continue
		}
		encoded, err := saver.codec.Encode(item)
		if err != nil {
			return dacheckpoint.Checkpoint{}, nil, err
		}
		blobs[channel] = encoded
	}
	return inline, blobs, nil
}

func isInlinePrimitive(value any) bool {
	switch value.(type) {
	case nil, string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}

func (saver *Saver) PutWrites(
	ctx context.Context,
	config dacheckpoint.Config,
	taskID string,
	taskPath string,
	writes []dacheckpoint.ChannelWrite,
) error {
	if err := saver.Setup(ctx); err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.CheckpointID == "" || taskID == "" {
		return fmt.Errorf("put PostgreSQL writes: checkpoint id and task id are required")
	}
	replace := len(writes) > 0
	for _, write := range writes {
		if _, special := dacheckpoint.SpecialWriteIndexes[write.Channel]; !special {
			replace = false
			break
		}
	}
	query := `
INSERT INTO checkpoint_writes
    (thread_id, checkpoint_ns, checkpoint_id, task_id, task_path, idx, channel, type, blob)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (thread_id, checkpoint_ns, checkpoint_id, task_id, idx) DO NOTHING`
	if replace {
		query = `
INSERT INTO checkpoint_writes
    (thread_id, checkpoint_ns, checkpoint_id, task_id, task_path, idx, channel, type, blob)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (thread_id, checkpoint_ns, checkpoint_id, task_id, idx)
DO UPDATE SET channel = EXCLUDED.channel, type = EXCLUDED.type, blob = EXCLUDED.blob`
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for index, write := range writes {
		encoded, err := saver.codec.Encode(write.Value)
		if err != nil {
			return fmt.Errorf("encode PostgreSQL write checkpoint=%q channel=%q: %w", config.CheckpointID, write.Channel, err)
		}
		assigned := index
		if special, ok := dacheckpoint.SpecialWriteIndexes[write.Channel]; ok {
			assigned = special
		}
		if _, err := transaction.ExecContext(ctx, query,
			config.ThreadID, config.Namespace, config.CheckpointID, taskID, taskPath,
			assigned, write.Channel, encoded.Type, encoded.Data,
		); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (saver *Saver) GetTuple(ctx context.Context, config dacheckpoint.Config) (*dacheckpoint.Tuple, error) {
	if err := saver.Setup(ctx); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT checkpoint_id, parent_checkpoint_id, checkpoint, metadata
FROM checkpoints WHERE thread_id = $1 AND checkpoint_ns = $2`
	arguments := []any{config.ThreadID, config.Namespace}
	if config.CheckpointID != "" {
		query += " AND checkpoint_id = $3"
		arguments = append(arguments, config.CheckpointID)
	} else {
		query += " ORDER BY checkpoint_id DESC LIMIT 1"
	}
	var id string
	var parent sql.NullString
	var checkpointJSON, metadataJSON []byte
	if err := saver.db.QueryRowContext(ctx, query, arguments...).Scan(
		&id, &parent, &checkpointJSON, &metadataJSON,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	value, err := unmarshalCheckpoint(checkpointJSON)
	if err != nil {
		return nil, fmt.Errorf("decode PostgreSQL checkpoint %q: %w", id, err)
	}
	if err := saver.loadBlobs(ctx, config.ThreadID, config.Namespace, &value); err != nil {
		return nil, err
	}
	metadata, err := unmarshalMetadata(metadataJSON)
	if err != nil {
		return nil, err
	}
	actual := dacheckpoint.Config{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: id}
	writes, err := saver.pendingWrites(ctx, actual)
	if err != nil {
		return nil, err
	}
	tuple := &dacheckpoint.Tuple{Config: actual, Checkpoint: value, Metadata: metadata, PendingWrites: writes}
	if parent.Valid && parent.String != "" {
		parentConfig := dacheckpoint.Config{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: parent.String}
		tuple.Parent = &parentConfig
	}
	return tuple, nil
}

func (saver *Saver) loadBlobs(ctx context.Context, threadID, namespace string, value *dacheckpoint.Checkpoint) error {
	for channel, version := range value.ChannelVersions {
		var typeTag string
		var data []byte
		err := saver.db.QueryRowContext(ctx, `
SELECT type, blob FROM checkpoint_blobs
WHERE thread_id = $1 AND checkpoint_ns = $2 AND channel = $3 AND version = $4`,
			threadID, namespace, channel, version,
		).Scan(&typeTag, &data)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if typeTag == "empty" {
			continue
		}
		decoded, err := saver.codec.Decode(serde.Typed{Type: typeTag, Data: data})
		if err != nil {
			return serde.WithContext(err, value.ID, channel, typeTag)
		}
		value.ChannelValues[channel] = decoded
	}
	return nil
}

func (saver *Saver) pendingWrites(ctx context.Context, config dacheckpoint.Config) ([]dacheckpoint.PendingWrite, error) {
	rows, err := saver.db.QueryContext(ctx, `
SELECT task_id, task_path, idx, channel, type, blob FROM checkpoint_writes
WHERE thread_id = $1 AND checkpoint_ns = $2 AND checkpoint_id = $3
ORDER BY task_path, task_id, idx`, config.ThreadID, config.Namespace, config.CheckpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []dacheckpoint.PendingWrite
	for rows.Next() {
		var write dacheckpoint.PendingWrite
		var typeTag string
		var data []byte
		if err := rows.Scan(&write.TaskID, &write.TaskPath, &write.Index, &write.Channel, &typeTag, &data); err != nil {
			return nil, err
		}
		write.Value, err = saver.codec.Decode(serde.Typed{Type: typeTag, Data: data})
		if err != nil {
			return nil, serde.WithContext(err, config.CheckpointID, write.Channel, typeTag)
		}
		result = append(result, write)
	}
	return result, rows.Err()
}

func (saver *Saver) List(ctx context.Context, config *dacheckpoint.Config, options dacheckpoint.ListOptions) ([]dacheckpoint.Tuple, error) {
	if err := saver.Setup(ctx); err != nil {
		return nil, err
	}
	query := `SELECT thread_id, checkpoint_ns, checkpoint_id FROM checkpoints`
	var clauses []string
	var arguments []any
	parameter := 1
	if config != nil {
		if err := config.Validate(); err != nil {
			return nil, err
		}
		clauses = append(clauses, fmt.Sprintf("thread_id = $%d", parameter))
		arguments = append(arguments, config.ThreadID)
		parameter++
		clauses = append(clauses, fmt.Sprintf("checkpoint_ns = $%d", parameter))
		arguments = append(arguments, config.Namespace)
		parameter++
		if config.CheckpointID != "" {
			clauses = append(clauses, fmt.Sprintf("checkpoint_id = $%d", parameter))
			arguments = append(arguments, config.CheckpointID)
			parameter++
		}
	}
	if options.Before != nil && options.Before.CheckpointID != "" {
		clauses = append(clauses, fmt.Sprintf("checkpoint_id < $%d", parameter))
		arguments = append(arguments, options.Before.CheckpointID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY checkpoint_id DESC"
	rows, err := saver.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	var configs []dacheckpoint.Config
	for rows.Next() {
		var item dacheckpoint.Config
		if err := rows.Scan(&item.ThreadID, &item.Namespace, &item.CheckpointID); err != nil {
			rows.Close()
			return nil, err
		}
		configs = append(configs, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var result []dacheckpoint.Tuple
	for _, item := range configs {
		tuple, err := saver.GetTuple(ctx, item)
		if err != nil {
			return nil, err
		}
		if tuple == nil || !matchesMetadata(tuple.Metadata, options.Metadata) {
			continue
		}
		result = append(result, *tuple)
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
	for _, table := range []string{"checkpoints", "checkpoint_blobs", "checkpoint_writes"} {
		if _, err := transaction.ExecContext(ctx, "DELETE FROM "+table+" WHERE thread_id = $1", threadID); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (saver *Saver) CopyThread(ctx context.Context, sourceThreadID, targetThreadID string) error {
	if err := saver.Setup(ctx); err != nil {
		return err
	}
	if sourceThreadID == "" || targetThreadID == "" || sourceThreadID == targetThreadID {
		return fmt.Errorf("copy PostgreSQL thread: distinct source and target ids are required")
	}
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var exists bool
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM checkpoints WHERE thread_id = $1
UNION ALL SELECT 1 FROM checkpoint_blobs WHERE thread_id = $1
UNION ALL SELECT 1 FROM checkpoint_writes WHERE thread_id = $1
)`, targetThreadID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("copy PostgreSQL thread: target %q already exists", targetThreadID)
	}
	queries := []string{
		`INSERT INTO checkpoints (thread_id, checkpoint_ns, checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata)
         SELECT $1, checkpoint_ns, checkpoint_id, parent_checkpoint_id, type, checkpoint, metadata FROM checkpoints WHERE thread_id = $2`,
		`INSERT INTO checkpoint_blobs (thread_id, checkpoint_ns, channel, version, type, blob)
         SELECT $1, checkpoint_ns, channel, version, type, blob FROM checkpoint_blobs WHERE thread_id = $2`,
		`INSERT INTO checkpoint_writes (thread_id, checkpoint_ns, checkpoint_id, task_id, task_path, idx, channel, type, blob)
         SELECT $1, checkpoint_ns, checkpoint_id, task_id, task_path, idx, channel, type, blob FROM checkpoint_writes WHERE thread_id = $2`,
	}
	for _, query := range queries {
		if _, err := transaction.ExecContext(ctx, query, targetThreadID, sourceThreadID); err != nil {
			return err
		}
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
	// Conservatively retain the latest checkpoint's complete parent chain and delete
	// only abandoned forks. This cannot sever required delta history.
	for _, threadID := range threadIDs {
		if err := saver.pruneThread(ctx, threadID); err != nil {
			return err
		}
	}
	return nil
}

func (saver *Saver) pruneThread(ctx context.Context, threadID string) error {
	rows, err := saver.db.QueryContext(ctx, `SELECT checkpoint_ns, checkpoint_id, parent_checkpoint_id FROM checkpoints WHERE thread_id = $1 ORDER BY checkpoint_ns, checkpoint_id DESC`, threadID)
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
	rows.Close()
	transaction, err := saver.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for namespace, records := range byNamespace {
		parents := make(map[string]string, len(records))
		for _, record := range records {
			parents[record.id] = record.parent
		}
		keep := map[string]struct{}{}
		if len(records) > 0 {
			for id := records[0].id; id != ""; id = parents[id] {
				if _, exists := keep[id]; exists {
					break
				}
				keep[id] = struct{}{}
			}
		}
		for _, record := range records {
			if _, ok := keep[record.id]; ok {
				continue
			}
			if _, err := transaction.ExecContext(ctx, `DELETE FROM checkpoint_writes WHERE thread_id = $1 AND checkpoint_ns = $2 AND checkpoint_id = $3`, threadID, namespace, record.id); err != nil {
				return err
			}
			if _, err := transaction.ExecContext(ctx, `DELETE FROM checkpoints WHERE thread_id = $1 AND checkpoint_ns = $2 AND checkpoint_id = $3`, threadID, namespace, record.id); err != nil {
				return err
			}
		}
		if _, err := transaction.ExecContext(ctx, `
DELETE FROM checkpoint_blobs AS blob
WHERE blob.thread_id = $1 AND blob.checkpoint_ns = $2
AND NOT EXISTS (
    SELECT 1 FROM checkpoints AS item,
         jsonb_each_text(item.checkpoint -> 'channel_versions') AS version(channel, value)
    WHERE item.thread_id = blob.thread_id
      AND item.checkpoint_ns = blob.checkpoint_ns
      AND version.channel = blob.channel
      AND version.value = blob.version
)`, threadID, namespace); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (saver *Saver) GetDeltaChannelHistory(ctx context.Context, config dacheckpoint.Config, channels []string) (map[string]dacheckpoint.DeltaHistory, error) {
	result := make(map[string]dacheckpoint.DeltaHistory, len(channels))
	remaining := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		result[channel] = dacheckpoint.DeltaHistory{}
		remaining[channel] = struct{}{}
	}
	target, err := saver.GetTuple(ctx, config)
	if err != nil || target == nil {
		return result, err
	}
	current := target.Parent
	for current != nil && len(remaining) > 0 {
		tuple, err := saver.GetTuple(ctx, *current)
		if err != nil || tuple == nil {
			return nil, err
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

func unmarshalCheckpoint(data []byte) (dacheckpoint.Checkpoint, error) {
	var value dacheckpoint.Checkpoint
	if err := json.Unmarshal(data, &value); err != nil {
		return dacheckpoint.Checkpoint{}, err
	}
	if value.ChannelValues == nil {
		value.ChannelValues = map[string]any{}
	}
	return value, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if value == nil {
		return nil
	}
	return value
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

// MigrationStatements returns a defensive copy for schema conformance tests.
func MigrationStatements() []string { return append([]string(nil), Migrations...) }

// SortPendingWrites applies the persisted task_path, task_id, idx order.
func SortPendingWrites(writes []dacheckpoint.PendingWrite) {
	sort.SliceStable(writes, func(i, j int) bool {
		if writes[i].TaskPath != writes[j].TaskPath {
			return writes[i].TaskPath < writes[j].TaskPath
		}
		if writes[i].TaskID != writes[j].TaskID {
			return writes[i].TaskID < writes[j].TaskID
		}
		return writes[i].Index < writes[j].Index
	})
}
