package daserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
)

func (server *Server) assistantForThread(threadID string) (*Assistant, error) {
	server.mu.RLock()
	defer server.mu.RUnlock()
	if server.threads[threadID] == nil {
		return nil, fmt.Errorf("%w: thread %q", errNotFound, threadID)
	}
	var selected *Run
	for _, run := range server.runs {
		if run.ThreadID == threadID && (selected == nil || run.CreatedAt > selected.CreatedAt) {
			selected = run
		}
	}
	if selected != nil {
		if assistant := server.assistants[selected.AssistantID]; assistant != nil {
			copy := cloneJSON(*assistant)
			if config, ok := selected.Kwargs["config"].(map[string]any); ok {
				copy.Config = mergeMaps(copy.Config, config)
			}
			return &copy, nil
		}
	}
	if thread := server.threads[threadID]; thread != nil {
		if assistantID, _ := thread.Metadata["assistant_id"].(string); assistantID != "" {
			if assistant := server.assistants[assistantID]; assistant != nil {
				copy := cloneJSON(*assistant)
				return &copy, nil
			}
		}
	}
	ids := make([]string, 0, len(server.assistants))
	for id := range server.assistants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("no assistant is available")
	}
	copy := cloneJSON(*server.assistants[ids[0]])
	return &copy, nil
}

func (server *Server) threadState(ctx context.Context, threadID, checkpointID string) (ThreadState, error) {
	assistant, err := server.assistantForThread(threadID)
	if err != nil {
		return ThreadState{}, err
	}
	graph, err := server.graphForAssistant(ctx, assistant)
	if err != nil {
		return ThreadState{}, err
	}
	config := dacheckpoint.Config{ThreadID: threadID, CheckpointID: checkpointID}
	tuple, err := server.saver.GetTuple(ctx, config)
	if err != nil {
		return ThreadState{}, err
	}
	if tuple == nil {
		server.mu.RLock()
		thread := server.threads[threadID]
		values := cloneJSON(thread.Values)
		server.mu.RUnlock()
		if values == nil {
			values = map[string]any{}
		}
		return ThreadState{Values: values, Next: []string{}, Tasks: []ThreadTask{}, Metadata: map[string]any{}}, nil
	}
	snapshot, err := graph.State(ctx, tuple.Config)
	if err != nil {
		return ThreadState{}, err
	}
	return server.snapshotState(snapshot, tuple), nil
}

func (server *Server) snapshotState(snapshot dagent.Snapshot, tuple *dacheckpoint.Tuple) ThreadState {
	state := ThreadState{
		Values: stateToProtocol(snapshot.State), Next: append([]string{}, snapshot.Next...),
		Checkpoint: checkpointFromConfig(snapshot.Config), Metadata: metadataMap(snapshot.Metadata),
		Tasks: make([]ThreadTask, 0, len(snapshot.Next)),
	}
	if tuple != nil {
		created := tuple.Checkpoint.Timestamp
		state.CreatedAt = &created
		if tuple.Parent != nil {
			state.ParentCheckpoint = checkpointFromConfig(*tuple.Parent)
		}
	}
	server.mu.RLock()
	interrupts := map[string]any(nil)
	if thread := server.threads[snapshot.Config.ThreadID]; thread != nil {
		interrupts = cloneJSON(thread.Interrupts)
	}
	server.mu.RUnlock()
	for index, name := range snapshot.Next {
		task := ThreadTask{ID: fmt.Sprintf("%s:%d", snapshot.Config.CheckpointID, index), Name: name, Interrupts: []any{}}
		for id, value := range interrupts {
			task.Interrupts = append(task.Interrupts, map[string]any{"id": id, "value": value})
		}
		state.Tasks = append(state.Tasks, task)
	}
	return state
}

func (server *Server) threadHistory(ctx context.Context, threadID string, limit int, before string, metadata map[string]any) ([]ThreadState, error) {
	assistant, err := server.assistantForThread(threadID)
	if err != nil {
		return nil, err
	}
	graph, err := server.graphForAssistant(ctx, assistant)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	options := dacheckpoint.ListOptions{Limit: limit, Metadata: metadata}
	if before != "" {
		options.Before = &dacheckpoint.Config{ThreadID: threadID, CheckpointID: before}
	}
	tuples, err := graph.History(ctx, dacheckpoint.Config{ThreadID: threadID}, options)
	if err != nil {
		return nil, err
	}
	result := make([]ThreadState, 0, len(tuples))
	for index := range tuples {
		snapshot, err := graph.State(ctx, tuples[index].Config)
		if err != nil {
			return nil, err
		}
		result = append(result, server.snapshotState(snapshot, &tuples[index]))
	}
	return result, nil
}

func checkpointFromConfig(config dacheckpoint.Config) *Checkpoint {
	if config.ThreadID == "" || config.CheckpointID == "" {
		return nil
	}
	return &Checkpoint{ThreadID: config.ThreadID, Namespace: config.Namespace, CheckpointID: config.CheckpointID}
}

func metadataMap(metadata dacheckpoint.Metadata) map[string]any {
	data, _ := json.Marshal(metadata)
	result := map[string]any{}
	_ = json.Unmarshal(data, &result)
	return result
}
