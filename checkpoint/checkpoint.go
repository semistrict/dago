// Package checkpoint defines durable graph execution records and saver contracts.
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

const LatestVersion = 4

const (
	ChannelError     = "__error__"
	ChannelScheduled = "__scheduled__"
	ChannelInterrupt = "__interrupt__"
	ChannelResume    = "__resume__"
	ChannelTasks     = "__pregel_tasks"
)

var SpecialWriteIndexes = map[string]int{
	ChannelError:     -1,
	ChannelScheduled: -2,
	ChannelInterrupt: -3,
	ChannelResume:    -4,
}

var (
	ErrInvalidConfig     = errors.New("invalid checkpoint config")
	ErrCheckpointMissing = errors.New("checkpoint not found")
	ErrUnsupportedPrune  = errors.New("unsupported checkpoint prune strategy")
)

// Config identifies a thread, checkpoint namespace, and optional checkpoint.
type Config struct {
	ThreadID     string `json:"thread_id"`
	Namespace    string `json:"checkpoint_ns,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
}

func (config Config) Validate() error {
	if config.ThreadID == "" {
		return fmt.Errorf("%w: thread id is required", ErrInvalidConfig)
	}
	return nil
}

// DeltaCounter records channel updates and total supersteps since its last snapshot.
type DeltaCounter struct {
	Updates    uint64 `json:"updates"`
	Supersteps uint64 `json:"supersteps"`
}

// Metadata describes the checkpoint's origin and execution position.
type Metadata struct {
	Source                     string                  `json:"source,omitempty"`
	Step                       int                     `json:"step"`
	Parents                    map[string]string       `json:"parents,omitempty"`
	RunID                      string                  `json:"run_id,omitempty"`
	CountersSinceDeltaSnapshot map[string]DeltaCounter `json:"counters_since_delta_snapshot,omitempty"`
	Extra                      map[string]any          `json:"extra,omitempty"`
}

// DeltaSnapshot is the language-neutral full-value seed encoded with MessagePack
// extension identifier 7 by compatible persistent savers.
type DeltaSnapshot struct {
	Value any `json:"value"`
}

// Checkpoint is the persisted graph state envelope.
type Checkpoint struct {
	Version         int                          `json:"v"`
	ID              string                       `json:"id"`
	Timestamp       string                       `json:"ts"`
	ChannelValues   map[string]any               `json:"channel_values"`
	ChannelVersions map[string]string            `json:"channel_versions"`
	VersionsSeen    map[string]map[string]string `json:"versions_seen"`
	UpdatedChannels []string                     `json:"updated_channels,omitempty"`
}

// Empty creates a versioned checkpoint with a sortable UUIDv6 identifier.
func Empty(step int) (Checkpoint, error) {
	id, err := NewID(step)
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{
		Version:         LatestVersion,
		ID:              id,
		Timestamp:       time.Now().UTC().Format(time.RFC3339Nano),
		ChannelValues:   map[string]any{},
		ChannelVersions: map[string]string{},
		VersionsSeen:    map[string]map[string]string{},
	}, nil
}

// ChannelWrite is a task-produced channel value before saver index assignment.
type ChannelWrite struct {
	Channel string
	Value   any
}

// PendingWrite is a saver-owned indexed task write.
type PendingWrite struct {
	TaskID   string
	TaskPath string
	Index    int
	Channel  string
	Value    any
}

// Tuple combines a checkpoint with its addressing, metadata, parent, and pending
// writes.
type Tuple struct {
	Config        Config
	Checkpoint    Checkpoint
	Metadata      Metadata
	Parent        *Config
	PendingWrites []PendingWrite
}

// DeltaHistory contains the nearest full seed, if any, and later writes ordered from
// oldest to newest. HasSeed distinguishes a present nil seed from no seed.
type DeltaHistory struct {
	Seed    any
	HasSeed bool
	Writes  []PendingWrite
}

// ListOptions filters checkpoint history.
type ListOptions struct {
	Metadata map[string]any
	Before   *Config
	Limit    int
}

// PruneStrategy controls thread-history pruning.
type PruneStrategy string

const (
	PruneKeepLatest PruneStrategy = "keep_latest"
	PruneDelete     PruneStrategy = "delete"
)

// Saver is the complete persistence contract used by the graph runtime.
type Saver interface {
	Put(ctx context.Context, config Config, value Checkpoint, metadata Metadata, newVersions map[string]string) (Config, error)
	PutWrites(ctx context.Context, config Config, taskID, taskPath string, writes []ChannelWrite) error
	GetTuple(ctx context.Context, config Config) (*Tuple, error)
	List(ctx context.Context, config *Config, options ListOptions) ([]Tuple, error)
	DeleteThread(ctx context.Context, threadID string) error
	CopyThread(ctx context.Context, sourceThreadID, targetThreadID string) error
	Prune(ctx context.Context, threadIDs []string, strategy PruneStrategy) error
	GetDeltaChannelHistory(ctx context.Context, config Config, channels []string) (map[string]DeltaHistory, error)
	NextVersion(current string) (string, error)
}

func metadataMatches(metadata Metadata, filter map[string]any) bool {
	if len(filter) == 0 {
		return true
	}
	values := map[string]any{
		"source": metadata.Source,
		"step":   metadata.Step,
		"run_id": metadata.RunID,
	}
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

func sortWrites(writes []PendingWrite) {
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
