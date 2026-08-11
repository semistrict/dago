package dacheckpoint

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type memoryKey struct {
	threadID  string
	namespace string
	id        string
}

type blobKey struct {
	threadID  string
	namespace string
	channel   string
	version   string
}

type memoryRecord struct {
	checkpoint Checkpoint
	metadata   Metadata
	parentID   string
}

type memoryBlob struct {
	value   any
	present bool
}

type writeKey struct {
	taskID string
	index  int
}

// MemorySaver is a concurrency-safe saver for tests and local ephemeral runs.
type MemorySaver struct {
	mu      sync.RWMutex
	records map[memoryKey]memoryRecord
	blobs   map[blobKey]memoryBlob
	writes  map[memoryKey]map[writeKey]PendingWrite
}

func NewMemorySaver() *MemorySaver {
	return &MemorySaver{
		records: make(map[memoryKey]memoryRecord),
		blobs:   make(map[blobKey]memoryBlob),
		writes:  make(map[memoryKey]map[writeKey]PendingWrite),
	}
}

func (saver *MemorySaver) Put(
	ctx context.Context,
	config Config,
	value Checkpoint,
	metadata Metadata,
	newVersions map[string]string,
) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	if value.ID == "" {
		return Config{}, fmt.Errorf("put checkpoint: id is required")
	}
	if value.Version == 0 {
		value.Version = LatestVersion
	}
	namespace := config.Namespace
	stored := cloneCheckpoint(value)
	values := stored.ChannelValues
	stored.ChannelValues = map[string]any{}

	saver.mu.Lock()
	defer saver.mu.Unlock()
	for channel, version := range newVersions {
		blob := memoryBlob{}
		if item, ok := values[channel]; ok {
			blob.value = cloneAny(item)
			blob.present = true
		}
		saver.blobs[blobKey{config.ThreadID, namespace, channel, version}] = blob
	}
	saver.records[memoryKey{config.ThreadID, namespace, value.ID}] = memoryRecord{
		checkpoint: stored,
		metadata:   cloneMetadata(metadata),
		parentID:   config.CheckpointID,
	}
	return Config{ThreadID: config.ThreadID, Namespace: namespace, CheckpointID: value.ID}, nil
}

func (saver *MemorySaver) PutWrites(
	ctx context.Context,
	config Config,
	taskID string,
	taskPath string,
	writes []ChannelWrite,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.CheckpointID == "" || taskID == "" {
		return fmt.Errorf("put checkpoint writes: checkpoint id and task id are required")
	}
	key := memoryKey{config.ThreadID, config.Namespace, config.CheckpointID}
	saver.mu.Lock()
	defer saver.mu.Unlock()
	if saver.writes[key] == nil {
		saver.writes[key] = make(map[writeKey]PendingWrite)
	}
	for index, write := range writes {
		assigned := index
		if special, ok := SpecialWriteIndexes[write.Channel]; ok {
			assigned = special
		}
		writeKey := writeKey{taskID: taskID, index: assigned}
		if assigned >= 0 {
			if _, exists := saver.writes[key][writeKey]; exists {
				continue
			}
		}
		saver.writes[key][writeKey] = PendingWrite{
			TaskID: taskID, TaskPath: taskPath, Index: assigned,
			Channel: write.Channel, Value: cloneAny(write.Value),
		}
	}
	return nil
}

func (saver *MemorySaver) GetTuple(ctx context.Context, config Config) (*Tuple, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	saver.mu.RLock()
	defer saver.mu.RUnlock()
	return saver.getTupleLocked(config)
}

func (saver *MemorySaver) getTupleLocked(config Config) (*Tuple, error) {
	id := config.CheckpointID
	if id == "" {
		for key := range saver.records {
			if key.threadID == config.ThreadID && key.namespace == config.Namespace && key.id > id {
				id = key.id
			}
		}
	}
	if id == "" {
		return nil, nil
	}
	key := memoryKey{config.ThreadID, config.Namespace, id}
	record, ok := saver.records[key]
	if !ok {
		return nil, nil
	}
	checkpoint := cloneCheckpoint(record.checkpoint)
	checkpoint.ChannelValues = make(map[string]any)
	for channel, version := range checkpoint.ChannelVersions {
		blob, exists := saver.blobs[blobKey{config.ThreadID, config.Namespace, channel, version}]
		if exists && blob.present {
			checkpoint.ChannelValues[channel] = cloneAny(blob.value)
		}
	}
	pending := saver.pendingWritesLocked(key)
	actual := Config{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: id}
	tuple := &Tuple{
		Config: actual, Checkpoint: checkpoint, Metadata: cloneMetadata(record.metadata),
		PendingWrites: pending,
	}
	if record.parentID != "" {
		parent := Config{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: record.parentID}
		tuple.Parent = &parent
	}
	return tuple, nil
}

func (saver *MemorySaver) pendingWritesLocked(key memoryKey) []PendingWrite {
	items := make([]PendingWrite, 0, len(saver.writes[key]))
	for _, write := range saver.writes[key] {
		copy := write
		copy.Value = cloneAny(write.Value)
		items = append(items, copy)
	}
	sortWrites(items)
	return items
}

func (saver *MemorySaver) List(
	ctx context.Context,
	config *Config,
	options ListOptions,
) ([]Tuple, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config != nil {
		if err := config.Validate(); err != nil {
			return nil, err
		}
	}
	saver.mu.RLock()
	defer saver.mu.RUnlock()
	keys := make([]memoryKey, 0, len(saver.records))
	for key, record := range saver.records {
		if config != nil {
			if key.threadID != config.ThreadID {
				continue
			}
			if key.namespace != config.Namespace {
				continue
			}
			if config.CheckpointID != "" && key.id != config.CheckpointID {
				continue
			}
		}
		if options.Before != nil && options.Before.CheckpointID != "" && key.id >= options.Before.CheckpointID {
			continue
		}
		if !metadataMatches(record.metadata, options.Metadata) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].threadID != keys[j].threadID {
			return keys[i].threadID < keys[j].threadID
		}
		if keys[i].namespace != keys[j].namespace {
			return keys[i].namespace < keys[j].namespace
		}
		return keys[i].id > keys[j].id
	})
	if options.Limit > 0 && len(keys) > options.Limit {
		keys = keys[:options.Limit]
	}
	result := make([]Tuple, 0, len(keys))
	for _, key := range keys {
		tuple, err := saver.getTupleLocked(Config{key.threadID, key.namespace, key.id})
		if err != nil {
			return nil, err
		}
		result = append(result, *tuple)
	}
	return result, nil
}

func (saver *MemorySaver) DeleteThread(ctx context.Context, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	saver.mu.Lock()
	defer saver.mu.Unlock()
	for key := range saver.records {
		if key.threadID == threadID {
			delete(saver.records, key)
			delete(saver.writes, key)
		}
	}
	for key := range saver.blobs {
		if key.threadID == threadID {
			delete(saver.blobs, key)
		}
	}
	return nil
}

func (saver *MemorySaver) CopyThread(
	ctx context.Context,
	sourceThreadID string,
	targetThreadID string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sourceThreadID == "" || targetThreadID == "" || sourceThreadID == targetThreadID {
		return fmt.Errorf("copy checkpoint thread: distinct source and target ids are required")
	}
	saver.mu.Lock()
	defer saver.mu.Unlock()
	for key := range saver.records {
		if key.threadID == targetThreadID {
			return fmt.Errorf("copy checkpoint thread: target %q already exists", targetThreadID)
		}
	}
	for key, record := range saver.records {
		if key.threadID != sourceThreadID {
			continue
		}
		copyKey := key
		copyKey.threadID = targetThreadID
		saver.records[copyKey] = memoryRecord{
			checkpoint: cloneCheckpoint(record.checkpoint),
			metadata:   cloneMetadata(record.metadata), parentID: record.parentID,
		}
		if writes := saver.writes[key]; writes != nil {
			saver.writes[copyKey] = make(map[writeKey]PendingWrite, len(writes))
			for writeKey, write := range writes {
				write.Value = cloneAny(write.Value)
				saver.writes[copyKey][writeKey] = write
			}
		}
	}
	for key, blob := range saver.blobs {
		if key.threadID == sourceThreadID {
			copyKey := key
			copyKey.threadID = targetThreadID
			saver.blobs[copyKey] = memoryBlob{value: cloneAny(blob.value), present: blob.present}
		}
	}
	return nil
}

func (saver *MemorySaver) Prune(
	ctx context.Context,
	threadIDs []string,
	strategy PruneStrategy,
) error {
	if strategy == PruneDelete {
		for _, threadID := range threadIDs {
			if err := saver.DeleteThread(ctx, threadID); err != nil {
				return err
			}
		}
		return nil
	}
	if strategy != PruneKeepLatest {
		return fmt.Errorf("%w %q", ErrUnsupportedPrune, strategy)
	}
	// Keep-latest retains each latest checkpoint and its full parent chain. This is
	// deliberately conservative: it removes abandoned forks without severing delta
	// history before a future snapshot-rewrite compactor is available.
	saver.mu.Lock()
	defer saver.mu.Unlock()
	for _, threadID := range threadIDs {
		latestByNamespace := map[string]string{}
		for key := range saver.records {
			if key.threadID == threadID && key.id > latestByNamespace[key.namespace] {
				latestByNamespace[key.namespace] = key.id
			}
		}
		keep := map[memoryKey]struct{}{}
		for namespace, id := range latestByNamespace {
			for id != "" {
				key := memoryKey{threadID, namespace, id}
				record, ok := saver.records[key]
				if !ok {
					break
				}
				keep[key] = struct{}{}
				id = record.parentID
			}
		}
		for key := range saver.records {
			if key.threadID != threadID {
				continue
			}
			if _, ok := keep[key]; !ok {
				delete(saver.records, key)
				delete(saver.writes, key)
			}
		}
		referencedBlobs := map[blobKey]struct{}{}
		for key, record := range saver.records {
			if key.threadID != threadID {
				continue
			}
			for channel, version := range record.checkpoint.ChannelVersions {
				referencedBlobs[blobKey{threadID, key.namespace, channel, version}] = struct{}{}
			}
		}
		for key := range saver.blobs {
			if key.threadID == threadID {
				if _, ok := referencedBlobs[key]; !ok {
					delete(saver.blobs, key)
				}
			}
		}
	}
	return nil
}

func (saver *MemorySaver) GetDeltaChannelHistory(
	ctx context.Context,
	config Config,
	channels []string,
) (map[string]DeltaHistory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	result := make(map[string]DeltaHistory, len(channels))
	if len(channels) == 0 {
		return result, nil
	}
	saver.mu.RLock()
	defer saver.mu.RUnlock()
	targetID := config.CheckpointID
	if targetID == "" {
		for key := range saver.records {
			if key.threadID == config.ThreadID && key.namespace == config.Namespace && key.id > targetID {
				targetID = key.id
			}
		}
	}
	target, ok := saver.records[memoryKey{config.ThreadID, config.Namespace, targetID}]
	if !ok {
		return result, nil
	}
	remaining := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		remaining[channel] = struct{}{}
		result[channel] = DeltaHistory{}
	}

	currentID := target.parentID
	for currentID != "" && len(remaining) > 0 {
		key := memoryKey{config.ThreadID, config.Namespace, currentID}
		record, exists := saver.records[key]
		if !exists {
			break
		}
		terminated := make([]string, 0)
		for channel := range remaining {
			version, versioned := record.checkpoint.ChannelVersions[channel]
			if !versioned {
				continue
			}
			blob, exists := saver.blobs[blobKey{config.ThreadID, config.Namespace, channel, version}]
			if exists && blob.present {
				history := result[channel]
				history.Seed = cloneAny(blob.value)
				history.HasSeed = true
				result[channel] = history
				terminated = append(terminated, channel)
			}
		}
		writes := saver.pendingWritesLocked(key)
		for index := len(writes) - 1; index >= 0; index-- {
			write := writes[index]
			if _, include := remaining[write.Channel]; include {
				history := result[write.Channel]
				history.Writes = append(history.Writes, write)
				result[write.Channel] = history
			}
		}
		for _, channel := range terminated {
			delete(remaining, channel)
		}
		currentID = record.parentID
	}
	for channel, history := range result {
		for left, right := 0, len(history.Writes)-1; left < right; left, right = left+1, right-1 {
			history.Writes[left], history.Writes[right] = history.Writes[right], history.Writes[left]
		}
		result[channel] = history
	}
	return result, nil
}

func (saver *MemorySaver) NextVersion(current string) (string, error) {
	return NextVersion(current)
}
