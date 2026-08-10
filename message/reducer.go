package message

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const RemoveAllMessages = "__remove_all__"

var ErrRemoveUnknownMessage = errors.New("cannot remove unknown message")

var fallbackMessageID atomic.Uint64

// IDGenerator supplies missing message identifiers. It is injectable for deterministic
// tests and fixture generation.
type IDGenerator func() (string, error)

// Reducer implements append, replacement, removal, and reset semantics.
type Reducer struct {
	IDs IDGenerator
}

// Merge returns a new message list. Missing IDs are assigned, existing IDs are
// replaced in place, tombstones remove known IDs, and the last reset tombstone drops
// all preceding messages and changes.
func (reducer Reducer) Merge(left, right []Message) ([]Message, error) {
	ids := reducer.IDs
	if ids == nil {
		ids = randomUUID
	}

	base := cloneMessages(left)
	changes := cloneMessages(right)
	for index := range base {
		if base[index].ID == "" {
			id, err := ids()
			if err != nil {
				return nil, fmt.Errorf("assign existing message id: %w", err)
			}
			base[index].ID = id
		}
	}
	resetIndex := -1
	for index := range changes {
		if changes[index].ID == "" {
			id, err := ids()
			if err != nil {
				return nil, fmt.Errorf("assign new message id: %w", err)
			}
			changes[index].ID = id
		}
		if changes[index].Role == RoleRemove && changes[index].ID == RemoveAllMessages {
			resetIndex = index
		}
	}
	if resetIndex >= 0 {
		return cloneMessages(changes[resetIndex+1:]), nil
	}

	byID := make(map[string]int, len(base))
	for index, existing := range base {
		byID[existing.ID] = index
	}
	removed := make(map[string]struct{})
	for _, change := range changes {
		index, exists := byID[change.ID]
		if change.Role == RoleRemove {
			if !exists {
				return nil, fmt.Errorf("%w %q", ErrRemoveUnknownMessage, change.ID)
			}
			removed[change.ID] = struct{}{}
			continue
		}
		if exists {
			delete(removed, change.ID)
			base[index] = change
			continue
		}
		byID[change.ID] = len(base)
		base = append(base, change)
	}

	result := make([]Message, 0, len(base)-len(removed))
	for _, item := range base {
		if _, drop := removed[item.ID]; !drop {
			result = append(result, item)
		}
	}
	return result, nil
}

// DeltaReduce is the batching-invariant reducer used by a delta channel. Unlike
// Merge, it leaves missing IDs untouched and ignores tombstones for unknown IDs.
// A reset tombstone clears state and all changes that precede it.
func DeltaReduce(state []Message, writes [][]Message) ([]Message, error) {
	result := cloneMessages(state)
	return deltaReduce(result, writes)
}

// DeltaReduceOwned applies delta writes to an exclusively owned state slice.
// Existing message values are not mutated; new and replacement values are
// cloned before they are retained.
func DeltaReduceOwned(state []Message, writes [][]Message) ([]Message, error) {
	result := state
	for _, write := range writes {
		for _, raw := range write {
			if raw.Role == RoleRemove {
				return deltaReduce(state, writes)
			}
			if raw.ID != "" {
				for _, existing := range result {
					if existing.ID == raw.ID {
						return deltaReduce(state, writes)
					}
				}
			}
			result = append(result, raw.Clone())
		}
	}
	return result, nil
}

func deltaReduce(result []Message, writes [][]Message) ([]Message, error) {
	byID := make(map[string]int, len(result))
	for index, existing := range result {
		if existing.ID != "" {
			byID[existing.ID] = index
		}
	}

	for _, write := range writes {
		for _, raw := range write {
			change := raw.Clone()
			if change.Role == RoleRemove && change.ID == RemoveAllMessages {
				result = nil
				byID = map[string]int{}
				continue
			}
			if change.ID == "" {
				result = append(result, change)
				continue
			}
			index, exists := byID[change.ID]
			if change.Role == RoleRemove {
				if exists {
					result[index] = Message{Role: RoleRemove, ID: change.ID}
					delete(byID, change.ID)
				}
				continue
			}
			if exists {
				result[index] = change
				continue
			}
			byID[change.ID] = len(result)
			result = append(result, change)
		}
	}

	compacted := result[:0]
	for _, item := range result {
		if item.Role != RoleRemove {
			compacted = append(compacted, item)
		}
	}
	return compacted, nil
}

// EnsureIDs returns an isolated message slice with an identifier on every
// ordinary message. Agent runtimes call it before a delta write is serialized,
// so checkpoint replay observes the same IDs instead of generating new ones.
func EnsureIDs(messages []Message) []Message {
	result := cloneMessages(messages)
	for index := range result {
		if result[index].ID != "" || result[index].Role == RoleRemove {
			continue
		}
		id, err := randomUUID()
		if err != nil {
			id = fmt.Sprintf("message-%d-%d", time.Now().UnixNano(), fallbackMessageID.Add(1))
		}
		result[index].ID = id
	}
	return result
}

func cloneMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	result := make([]Message, len(messages))
	for index, item := range messages {
		result[index] = item.Clone()
	}
	return result
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4],
		value[4:6],
		value[6:8],
		value[8:10],
		value[10:16],
	), nil
}
