package graph

import (
	"fmt"
	"time"

	"github.com/semistrict/dago/state"
)

func checkpointTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func encodeTasks(tasks []task) []any {
	encoded := make([]any, 0, len(tasks))
	for _, task := range tasks {
		input := make(map[string]any, len(task.input))
		for key, value := range task.input {
			input[key] = value
		}
		encoded = append(encoded, map[string]any{
			"node": task.node, "input": input, "id": task.id, "path": task.path,
		})
	}
	return encoded
}

func decodeTasks(value any) ([]task, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("decode checkpoint tasks: got %T", value)
	}
	result := make([]task, 0, len(items))
	for index, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("decode checkpoint task %d: got %T", index, item)
		}
		node, ok := record["node"].(string)
		if !ok || node == "" {
			return nil, fmt.Errorf("decode checkpoint task %d: node is required", index)
		}
		task := task{node: node}
		task.id, _ = record["id"].(string)
		task.path, _ = record["path"].(string)
		if rawInput, exists := record["input"]; exists {
			input, ok := rawInput.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("decode checkpoint task %d input: got %T", index, rawInput)
			}
			task.input = state.Values(input)
		}
		result = append(result, task)
	}
	return result, nil
}

func encodeInterrupts(interrupts []Interrupt) []any {
	result := make([]any, 0, len(interrupts))
	for _, interrupt := range interrupts {
		result = append(result, map[string]any{"id": interrupt.ID, "value": interrupt.Value})
	}
	return result
}
