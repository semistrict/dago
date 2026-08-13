package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/damessage"
)

type memoryBrowserCheckpointStore struct {
	checkpoints map[string]browserCheckpointRecord
	writes      map[string]browserWriteRecord
}

func newMemoryBrowserCheckpointStore() *memoryBrowserCheckpointStore {
	return &memoryBrowserCheckpointStore{
		checkpoints: map[string]browserCheckpointRecord{},
		writes:      map[string]browserWriteRecord{},
	}
}

func checkpointRecordKey(threadID, namespace, checkpointID string) string {
	return threadID + "\x00" + namespace + "\x00" + checkpointID
}

func writeRecordKey(record browserWriteRecord) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", record.ThreadID, record.Namespace, record.CheckpointID, record.TaskID, record.Index)
}

func decodeStorePayload[T any](payload []byte) (T, error) {
	var value T
	err := json.Unmarshal(payload, &value)
	return value, err
}

func encodeStoreResult(value any) ([]byte, error) {
	return json.Marshal(value)
}

func (store *memoryBrowserCheckpointStore) Execute(ctx context.Context, operation string, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch operation {
	case "put_checkpoint":
		record, err := decodeStorePayload[browserCheckpointRecord](payload)
		if err != nil {
			return nil, err
		}
		store.checkpoints[checkpointRecordKey(record.ThreadID, record.Namespace, record.CheckpointID)] = record
		return nil, nil
	case "put_writes":
		request, err := decodeStorePayload[struct {
			Writes []browserWriteRecord `json:"writes"`
		}](payload)
		if err != nil {
			return nil, err
		}
		for _, record := range request.Writes {
			key := writeRecordKey(record)
			if _, exists := store.writes[key]; exists && !record.Replace {
				continue
			}
			store.writes[key] = record
		}
		return nil, nil
	case "get_checkpoint":
		config, err := decodeStorePayload[dacheckpoint.Config](payload)
		if err != nil {
			return nil, err
		}
		var found *browserCheckpointRecord
		for _, record := range store.checkpoints {
			if record.ThreadID != config.ThreadID || record.Namespace != config.Namespace {
				continue
			}
			if config.CheckpointID != "" && record.CheckpointID != config.CheckpointID {
				continue
			}
			if found == nil || record.CheckpointID > found.CheckpointID {
				copy := record
				found = &copy
			}
		}
		return encodeStoreResult(found)
	case "get_writes":
		config, err := decodeStorePayload[dacheckpoint.Config](payload)
		if err != nil {
			return nil, err
		}
		result := make([]browserWriteRecord, 0)
		for _, record := range store.writes {
			if record.ThreadID == config.ThreadID && record.Namespace == config.Namespace && record.CheckpointID == config.CheckpointID {
				result = append(result, record)
			}
		}
		return encodeStoreResult(result)
	case "list_checkpoints":
		var config *dacheckpoint.Config
		if string(payload) != "null" {
			value, err := decodeStorePayload[dacheckpoint.Config](payload)
			if err != nil {
				return nil, err
			}
			config = &value
		}
		result := make([]browserCheckpointRecord, 0)
		for _, record := range store.checkpoints {
			if config != nil && (record.ThreadID != config.ThreadID || record.Namespace != config.Namespace) {
				continue
			}
			result = append(result, record)
		}
		return encodeStoreResult(result)
	case "delete_thread":
		request, err := decodeStorePayload[map[string]string](payload)
		if err != nil {
			return nil, err
		}
		for key, record := range store.checkpoints {
			if record.ThreadID == request["thread_id"] {
				delete(store.checkpoints, key)
			}
		}
		for key, record := range store.writes {
			if record.ThreadID == request["thread_id"] {
				delete(store.writes, key)
			}
		}
		return nil, nil
	case "copy_thread":
		request, err := decodeStorePayload[map[string]string](payload)
		if err != nil {
			return nil, err
		}
		source, target := request["source_thread_id"], request["target_thread_id"]
		for _, record := range store.checkpoints {
			if record.ThreadID == target {
				return nil, fmt.Errorf("target already exists")
			}
		}
		for _, record := range store.checkpoints {
			if record.ThreadID == source {
				record.ThreadID = target
				store.checkpoints[checkpointRecordKey(target, record.Namespace, record.CheckpointID)] = record
			}
		}
		for _, record := range store.writes {
			if record.ThreadID == source {
				record.ThreadID = target
				store.writes[writeRecordKey(record)] = record
			}
		}
		return nil, nil
	case "delete_checkpoints":
		request, err := decodeStorePayload[struct {
			Configs []dacheckpoint.Config `json:"configs"`
		}](payload)
		if err != nil {
			return nil, err
		}
		for _, config := range request.Configs {
			delete(store.checkpoints, checkpointRecordKey(config.ThreadID, config.Namespace, config.CheckpointID))
			for key, record := range store.writes {
				if record.ThreadID == config.ThreadID && record.Namespace == config.Namespace && record.CheckpointID == config.CheckpointID {
					delete(store.writes, key)
				}
			}
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported operation %q", operation)
	}
}

func TestIndexedDBSaverPersistsTypedCheckpointsAndWrites(t *testing.T) {
	ctx := context.Background()
	store := newMemoryBrowserCheckpointStore()
	saver := NewIndexedDBSaver(store)
	thread := dacheckpoint.Config{ThreadID: "browser-thread"}
	first, err := dacheckpoint.Empty(0)
	if err != nil {
		t.Fatal(err)
	}
	first.ChannelValues["messages"] = dacheckpoint.DeltaSnapshot{Value: []damessage.Message{damessage.Human("hello")}}
	first.ChannelVersions["messages"] = "0001"
	firstConfig, err := saver.Put(ctx, thread, first, dacheckpoint.Metadata{Source: "input", Step: -1}, map[string]string{"messages": "0001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := saver.PutWrites(ctx, firstConfig, "task", "root", []dacheckpoint.ChannelWrite{{Channel: "messages", Value: damessage.Assistant("first")}}); err != nil {
		t.Fatal(err)
	}
	second, err := dacheckpoint.Empty(1)
	if err != nil {
		t.Fatal(err)
	}
	second.ChannelValues["messages"] = []damessage.Message{damessage.Human("hello"), damessage.Assistant("first")}
	second.ChannelVersions["messages"] = "0002"
	secondConfig, err := saver.Put(ctx, firstConfig, second, dacheckpoint.Metadata{Source: "loop", Step: 0}, map[string]string{"messages": "0002"})
	if err != nil {
		t.Fatal(err)
	}

	latest, err := saver.GetTuple(ctx, thread)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.Config != secondConfig || latest.Parent == nil || *latest.Parent != firstConfig {
		t.Fatalf("latest tuple = %#v", latest)
	}
	messages, ok := latest.Checkpoint.ChannelValues["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("restored messages = %#v", latest.Checkpoint.ChannelValues["messages"])
	}
	assistant, assistantOK := messages[1].(damessage.Message)
	if !assistantOK || assistant.TextContent() != "first" {
		t.Fatalf("restored assistant = %#v", messages[1])
	}
	history, err := saver.GetDeltaChannelHistory(ctx, secondConfig, []string{"messages"})
	if err != nil {
		t.Fatal(err)
	}
	if !history["messages"].HasSeed || len(history["messages"].Writes) != 1 {
		t.Fatalf("delta history = %#v", history)
	}

	listed, err := saver.List(ctx, &thread, dacheckpoint.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed checkpoints = %d", len(listed))
	}
	ids := []string{listed[0].Config.CheckpointID, listed[1].Config.CheckpointID}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] > ids[j] }) {
		t.Fatalf("checkpoint order = %v", ids)
	}
	if err := saver.CopyThread(ctx, thread.ThreadID, "browser-copy"); err != nil {
		t.Fatal(err)
	}
	copy, err := saver.GetTuple(ctx, dacheckpoint.Config{ThreadID: "browser-copy"})
	if err != nil || copy == nil || copy.Checkpoint.ID != second.ID {
		t.Fatalf("copied tuple = %#v, %v", copy, err)
	}
	if err := saver.DeleteThread(ctx, thread.ThreadID); err != nil {
		t.Fatal(err)
	}
	deleted, err := saver.GetTuple(ctx, thread)
	if err != nil || deleted != nil {
		t.Fatalf("deleted tuple = %#v, %v", deleted, err)
	}
}
