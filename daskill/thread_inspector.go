package daskill

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damessage"
)

// ThreadInspectorOptions bounds read-only local checkpoint inspection. Zero
// values select finite defaults.
type ThreadInspectorOptions struct {
	MaximumCheckpoints int
	MaximumThreads     int
	MaximumMessages    int
	MaximumTextBytes   int
}

// ThreadInspector reads checkpoint state without executing or mutating it.
type ThreadInspector struct {
	saver   dacheckpoint.Saver
	options ThreadInspectorOptions
}

// NewThreadInspector constructs a bounded inspector around the required saver.
func NewThreadInspector(saver dacheckpoint.Saver, options ThreadInspectorOptions) *ThreadInspector {
	if nilInterface(saver) {
		panic("daskill: thread inspector saver is nil")
	}
	if options.MaximumCheckpoints < 0 || options.MaximumThreads < 0 || options.MaximumMessages < 0 || options.MaximumTextBytes < 0 {
		panic("daskill: thread inspector limits cannot be negative")
	}
	if options.MaximumCheckpoints == 0 {
		options.MaximumCheckpoints = 5_000
	}
	if options.MaximumThreads == 0 {
		options.MaximumThreads = 100
	}
	if options.MaximumMessages == 0 {
		options.MaximumMessages = 1_000
	}
	if options.MaximumTextBytes == 0 {
		options.MaximumTextBytes = 1 << 20
	}
	return &ThreadInspector{saver: saver, options: options}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// ThreadSummary is stable metadata for one local thread.
type ThreadSummary struct {
	ThreadID     string `json:"thread_id"`
	CheckpointID string `json:"checkpoint_id"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// List returns at most the configured number of latest distinct threads.
func (inspector *ThreadInspector) List(ctx context.Context) ([]ThreadSummary, error) {
	tuples, err := inspector.saver.List(ctx, nil, dacheckpoint.ListOptions{Limit: inspector.options.MaximumCheckpoints})
	if err != nil {
		return nil, err
	}
	if len(tuples) > inspector.options.MaximumCheckpoints {
		return nil, fmt.Errorf("checkpoint saver returned more than the %d requested entries", inspector.options.MaximumCheckpoints)
	}
	seen := make(map[string]bool)
	result := make([]ThreadSummary, 0, min(len(tuples), inspector.options.MaximumThreads))
	for _, tuple := range tuples {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tuple.Config.Namespace != "" || seen[tuple.Config.ThreadID] {
			continue
		}
		seen[tuple.Config.ThreadID] = true
		result = append(result, ThreadSummary{ThreadID: tuple.Config.ThreadID, CheckpointID: tuple.Config.CheckpointID, UpdatedAt: tuple.Checkpoint.Timestamp})
		if len(result) >= inspector.options.MaximumThreads {
			break
		}
	}
	return result, nil
}

// Inspect reconstructs the message channel for a thread, applying message
// tombstones and enforcing output bounds.
func (inspector *ThreadInspector) Inspect(ctx context.Context, threadID string) ([]damessage.Message, error) {
	if strings.TrimSpace(threadID) == "" || len(threadID) > 512 {
		return nil, errors.New("thread id must be 1-512 characters")
	}
	tuple, err := inspector.saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: threadID})
	if err != nil {
		return nil, err
	}
	if tuple == nil {
		return nil, fmt.Errorf("thread %q was not found", threadID)
	}
	var updates []damessage.Message
	if current, exists := tuple.Checkpoint.ChannelValues[dagent.MessagesKey]; exists {
		updates, err = appendMessages(updates, current)
		if err != nil {
			return nil, err
		}
	} else {
		histories, historyErr := inspector.saver.GetDeltaChannelHistory(ctx, tuple.Config, []string{dagent.MessagesKey})
		if historyErr != nil {
			return nil, historyErr
		}
		history := histories[dagent.MessagesKey]
		if history.HasSeed {
			updates, err = appendMessages(updates, history.Seed)
			if err != nil {
				return nil, err
			}
		}
		for _, write := range history.Writes {
			updates, err = appendMessages(updates, write.Value)
			if err != nil {
				return nil, err
			}
			if len(updates) > inspector.options.MaximumMessages*2 {
				return nil, fmt.Errorf("thread exceeds %d message updates", inspector.options.MaximumMessages*2)
			}
		}
	}
	if len(updates) > inspector.options.MaximumMessages*2 {
		return nil, fmt.Errorf("thread exceeds %d message updates", inspector.options.MaximumMessages*2)
	}
	textBytes := 0
	for _, message := range updates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if message.Role != damessage.RoleRemove {
			textBytes += len(message.TextContent())
		}
		if textBytes > inspector.options.MaximumTextBytes {
			return nil, fmt.Errorf("thread text exceeds %d bytes", inspector.options.MaximumTextBytes)
		}
	}
	result, err := damessage.DeltaReduce(nil, [][]damessage.Message{updates})
	if err != nil {
		return nil, err
	}
	if len(result) > inspector.options.MaximumMessages {
		result = result[len(result)-inspector.options.MaximumMessages:]
	}
	return result, nil
}

func appendMessages(target []damessage.Message, value any) ([]damessage.Message, error) {
	if snapshot, ok := value.(dacheckpoint.DeltaSnapshot); ok {
		value = snapshot.Value
	}
	switch typed := value.(type) {
	case nil:
		return target, nil
	case damessage.Message:
		return append(target, typed), nil
	case []damessage.Message:
		return append(target, typed...), nil
	case []any:
		for _, item := range typed {
			message, ok := item.(damessage.Message)
			if !ok {
				return nil, fmt.Errorf("message update has type %T", item)
			}
			target = append(target, message)
		}
		return target, nil
	default:
		return nil, fmt.Errorf("message channel has type %T", value)
	}
}

// SortThreadSummaries is provided for deterministic custom reports.
func SortThreadSummaries(values []ThreadSummary) {
	sort.SliceStable(values, func(i, j int) bool { return values[i].ThreadID < values[j].ThreadID })
}
