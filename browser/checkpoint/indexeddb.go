// Package checkpoint implements browser persistence for graph checkpoints.
package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dacheckpoint/serde"
)

// CheckpointStore is the asynchronous storage boundary used by the browser
// checkpointer. The WASM entrypoint implements it with normalized IndexedDB
// object stores.
type CheckpointStore interface {
	Execute(context.Context, string, []byte) ([]byte, error)
}

// IndexedDBSaver implements the complete graph checkpoint contract over the
// browser checkpoint storage protocol.
type IndexedDBSaver struct {
	store CheckpointStore
	codec *serde.Safe
}

type browserCheckpointRecord struct {
	ThreadID           string                `json:"thread_id"`
	Namespace          string                `json:"namespace"`
	CheckpointID       string                `json:"checkpoint_id"`
	ParentCheckpointID string                `json:"parent_checkpoint_id,omitempty"`
	Type               string                `json:"type"`
	Checkpoint         []byte                `json:"checkpoint"`
	Metadata           dacheckpoint.Metadata `json:"metadata"`
}

type browserWriteRecord struct {
	ThreadID     string `json:"thread_id"`
	Namespace    string `json:"namespace"`
	CheckpointID string `json:"checkpoint_id"`
	TaskID       string `json:"task_id"`
	TaskPath     string `json:"task_path,omitempty"`
	Index        int    `json:"index"`
	Channel      string `json:"channel"`
	Type         string `json:"type"`
	Value        []byte `json:"value"`
	Replace      bool   `json:"replace,omitempty"`
}

func NewIndexedDBSaver(store CheckpointStore) *IndexedDBSaver {
	return &IndexedDBSaver{store: store, codec: serde.New(serde.Limits{})}
}

func (saver *IndexedDBSaver) execute(ctx context.Context, operation string, input, output any) error {
	if saver == nil || saver.store == nil {
		return fmt.Errorf("browser checkpoint store is required")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode browser checkpoint %s request: %w", operation, err)
	}
	result, err := saver.store.Execute(ctx, operation, payload)
	if err != nil {
		return fmt.Errorf("browser checkpoint %s: %w", operation, err)
	}
	if output != nil && len(result) > 0 {
		if err := json.Unmarshal(result, output); err != nil {
			return fmt.Errorf("decode browser checkpoint %s response: %w", operation, err)
		}
	}
	return nil
}

func (saver *IndexedDBSaver) Put(
	ctx context.Context,
	config dacheckpoint.Config,
	value dacheckpoint.Checkpoint,
	metadata dacheckpoint.Metadata,
	_ map[string]string,
) (dacheckpoint.Config, error) {
	if err := ctx.Err(); err != nil {
		return dacheckpoint.Config{}, err
	}
	if err := config.Validate(); err != nil {
		return dacheckpoint.Config{}, err
	}
	if value.ID == "" {
		return dacheckpoint.Config{}, fmt.Errorf("put browser checkpoint: id is required")
	}
	if value.Version == 0 {
		value.Version = dacheckpoint.LatestVersion
	}
	encoded, err := saver.codec.Encode(value)
	if err != nil {
		return dacheckpoint.Config{}, fmt.Errorf("encode browser checkpoint %q: %w", value.ID, err)
	}
	record := browserCheckpointRecord{
		ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: value.ID,
		ParentCheckpointID: config.CheckpointID, Type: encoded.Type, Checkpoint: encoded.Data,
		Metadata: metadata,
	}
	if err := saver.execute(ctx, "put_checkpoint", record, nil); err != nil {
		return dacheckpoint.Config{}, err
	}
	return dacheckpoint.Config{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: value.ID}, nil
}

func (saver *IndexedDBSaver) PutWrites(
	ctx context.Context,
	config dacheckpoint.Config,
	taskID string,
	taskPath string,
	writes []dacheckpoint.ChannelWrite,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.CheckpointID == "" || taskID == "" {
		return fmt.Errorf("put browser checkpoint writes: checkpoint id and task id are required")
	}
	records := make([]browserWriteRecord, 0, len(writes))
	for index, write := range writes {
		assigned := index
		_, replace := dacheckpoint.SpecialWriteIndexes[write.Channel]
		if replace {
			assigned = dacheckpoint.SpecialWriteIndexes[write.Channel]
		}
		encoded, err := saver.codec.Encode(write.Value)
		if err != nil {
			return fmt.Errorf("encode browser checkpoint write %q: %w", write.Channel, err)
		}
		records = append(records, browserWriteRecord{
			ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: config.CheckpointID,
			TaskID: taskID, TaskPath: taskPath, Index: assigned, Channel: write.Channel,
			Type: encoded.Type, Value: encoded.Data, Replace: replace,
		})
	}
	return saver.execute(ctx, "put_writes", map[string]any{"writes": records}, nil)
}

func (saver *IndexedDBSaver) GetTuple(ctx context.Context, config dacheckpoint.Config) (*dacheckpoint.Tuple, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	var record *browserCheckpointRecord
	if err := saver.execute(ctx, "get_checkpoint", config, &record); err != nil || record == nil {
		return nil, err
	}
	return saver.decodeTuple(ctx, *record)
}

func (saver *IndexedDBSaver) decodeTuple(ctx context.Context, record browserCheckpointRecord) (*dacheckpoint.Tuple, error) {
	decoded, err := saver.codec.Decode(serde.Typed{Type: record.Type, Data: record.Checkpoint})
	if err != nil {
		return nil, serde.WithContext(err, record.CheckpointID, "$checkpoint", record.Type)
	}
	checkpoint, ok := decoded.(dacheckpoint.Checkpoint)
	if !ok {
		return nil, fmt.Errorf("decode browser checkpoint %q: got %T", record.CheckpointID, decoded)
	}
	config := dacheckpoint.Config{
		ThreadID: record.ThreadID, Namespace: record.Namespace, CheckpointID: record.CheckpointID,
	}
	var records []browserWriteRecord
	if err := saver.execute(ctx, "get_writes", config, &records); err != nil {
		return nil, err
	}
	writes := make([]dacheckpoint.PendingWrite, 0, len(records))
	for _, record := range records {
		value, err := saver.codec.Decode(serde.Typed{Type: record.Type, Data: record.Value})
		if err != nil {
			return nil, serde.WithContext(err, record.CheckpointID, record.Channel, record.Type)
		}
		writes = append(writes, dacheckpoint.PendingWrite{
			TaskID: record.TaskID, TaskPath: record.TaskPath, Index: record.Index,
			Channel: record.Channel, Value: value,
		})
	}
	sort.SliceStable(writes, func(i, j int) bool {
		if writes[i].TaskPath != writes[j].TaskPath {
			return writes[i].TaskPath < writes[j].TaskPath
		}
		if writes[i].TaskID != writes[j].TaskID {
			return writes[i].TaskID < writes[j].TaskID
		}
		return writes[i].Index < writes[j].Index
	})
	tuple := &dacheckpoint.Tuple{Config: config, Checkpoint: checkpoint, Metadata: record.Metadata, PendingWrites: writes}
	if record.ParentCheckpointID != "" {
		parent := config
		parent.CheckpointID = record.ParentCheckpointID
		tuple.Parent = &parent
	}
	return tuple, nil
}

func (saver *IndexedDBSaver) List(
	ctx context.Context,
	config *dacheckpoint.Config,
	options dacheckpoint.ListOptions,
) ([]dacheckpoint.Tuple, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config != nil {
		if err := config.Validate(); err != nil {
			return nil, err
		}
	}
	var records []browserCheckpointRecord
	if err := saver.execute(ctx, "list_checkpoints", config, &records); err != nil {
		return nil, err
	}
	filtered := records[:0]
	for _, record := range records {
		if config != nil && config.CheckpointID != "" && record.CheckpointID != config.CheckpointID {
			continue
		}
		if options.Before != nil && options.Before.CheckpointID != "" && record.CheckpointID >= options.Before.CheckpointID {
			continue
		}
		if !browserMetadataMatches(record.Metadata, options.Metadata) {
			continue
		}
		filtered = append(filtered, record)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].ThreadID != filtered[j].ThreadID {
			return filtered[i].ThreadID < filtered[j].ThreadID
		}
		if filtered[i].Namespace != filtered[j].Namespace {
			return filtered[i].Namespace < filtered[j].Namespace
		}
		return filtered[i].CheckpointID > filtered[j].CheckpointID
	})
	if options.Limit > 0 && len(filtered) > options.Limit {
		filtered = filtered[:options.Limit]
	}
	result := make([]dacheckpoint.Tuple, 0, len(filtered))
	for _, record := range filtered {
		tuple, err := saver.decodeTuple(ctx, record)
		if err != nil {
			return nil, err
		}
		result = append(result, *tuple)
	}
	return result, nil
}

func browserMetadataMatches(metadata dacheckpoint.Metadata, filter map[string]any) bool {
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

func (saver *IndexedDBSaver) DeleteThread(ctx context.Context, threadID string) error {
	return saver.execute(ctx, "delete_thread", map[string]string{"thread_id": threadID}, nil)
}

func (saver *IndexedDBSaver) CopyThread(ctx context.Context, sourceThreadID, targetThreadID string) error {
	if sourceThreadID == "" || targetThreadID == "" || sourceThreadID == targetThreadID {
		return fmt.Errorf("copy browser checkpoint thread: distinct source and target ids are required")
	}
	return saver.execute(ctx, "copy_thread", map[string]string{
		"source_thread_id": sourceThreadID, "target_thread_id": targetThreadID,
	}, nil)
}

func (saver *IndexedDBSaver) Prune(ctx context.Context, threadIDs []string, strategy dacheckpoint.PruneStrategy) error {
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
	for _, threadID := range threadIDs {
		config := dacheckpoint.Config{ThreadID: threadID}
		var records []browserCheckpointRecord
		if err := saver.execute(ctx, "list_checkpoints", config, &records); err != nil {
			return err
		}
		byKey := make(map[string]browserCheckpointRecord, len(records))
		latest := map[string]string{}
		for _, record := range records {
			byKey[record.Namespace+"\x00"+record.CheckpointID] = record
			if record.CheckpointID > latest[record.Namespace] {
				latest[record.Namespace] = record.CheckpointID
			}
		}
		keep := map[string]struct{}{}
		for namespace, id := range latest {
			for id != "" {
				key := namespace + "\x00" + id
				record, ok := byKey[key]
				if !ok {
					break
				}
				keep[key] = struct{}{}
				id = record.ParentCheckpointID
			}
		}
		remove := make([]dacheckpoint.Config, 0)
		for _, record := range records {
			if _, ok := keep[record.Namespace+"\x00"+record.CheckpointID]; !ok {
				remove = append(remove, dacheckpoint.Config{
					ThreadID: threadID, Namespace: record.Namespace, CheckpointID: record.CheckpointID,
				})
			}
		}
		if len(remove) > 0 {
			if err := saver.execute(ctx, "delete_checkpoints", map[string]any{"configs": remove}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (saver *IndexedDBSaver) GetDeltaChannelHistory(
	ctx context.Context,
	config dacheckpoint.Config,
	channels []string,
) (map[string]dacheckpoint.DeltaHistory, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
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
	parent := target.Parent
	for parent != nil && len(remaining) > 0 {
		tuple, err := saver.GetTuple(ctx, *parent)
		if err != nil {
			return nil, err
		}
		if tuple == nil {
			break
		}
		terminated := make([]string, 0)
		for channel := range remaining {
			value, ok := tuple.Checkpoint.ChannelValues[channel]
			if ok {
				history := result[channel]
				history.Seed = value
				history.HasSeed = true
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
		parent = tuple.Parent
	}
	for channel, history := range result {
		for left, right := 0, len(history.Writes)-1; left < right; left, right = left+1, right-1 {
			history.Writes[left], history.Writes[right] = history.Writes[right], history.Writes[left]
		}
		result[channel] = history
	}
	return result, nil
}

func (saver *IndexedDBSaver) NextVersion(current string) (string, error) {
	return dacheckpoint.NextVersion(current)
}

var _ dacheckpoint.Saver = (*IndexedDBSaver)(nil)
