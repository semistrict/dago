package daserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/semistrict/dago/dagent"
)

func (server *Server) executeRun(runID string) {
	server.mu.RLock()
	run := server.runs[runID]
	if run == nil || run.Status != "pending" {
		server.mu.RUnlock()
		return
	}
	threadID := run.ThreadID
	server.mu.RUnlock()

	lock := server.threadLock(threadID)
	lock.Lock()
	defer lock.Unlock()

	server.mu.Lock()
	run = server.runs[runID]
	if run == nil || run.Status != "pending" {
		server.mu.Unlock()
		return
	}
	assistant := server.assistants[run.AssistantID]
	if assistant == nil {
		server.failRunLocked(run, fmt.Errorf("assistant %q not found", run.AssistantID))
		server.mu.Unlock()
		return
	}
	request, err := requestFromKwargs(run.Kwargs)
	if err != nil {
		server.failRunLocked(run, err)
		server.mu.Unlock()
		return
	}
	run.Status = "running"
	run.UpdatedAt = server.timestamp()
	thread := server.threads[threadID]
	if thread == nil {
		server.failRunLocked(run, fmt.Errorf("thread %q not found", threadID))
		server.mu.Unlock()
		return
	}
	thread.Status = "busy"
	thread.UpdatedAt = run.UpdatedAt
	runContext, cancel := context.WithCancel(server.ctx)
	server.active[runID] = cancel
	_ = server.persistLocked()
	server.mu.Unlock()

	defer func() {
		cancel()
		server.mu.Lock()
		delete(server.active, runID)
		server.mu.Unlock()
	}()

	configured := cloneJSON(*assistant)
	configured.Config = mergeMaps(configured.Config, request.Config)
	if request.Context != nil {
		configured.Context = cloneJSON(request.Context)
	}
	graph, err := server.graphForAssistant(runContext, &configured)
	if err != nil {
		server.completeRun(runID, nil, err)
		return
	}
	input, err := inputToAgent(request, threadID)
	if err != nil {
		server.completeRun(runID, nil, err)
		return
	}
	log := server.eventLog(runID)
	if log == nil {
		server.completeRun(runID, nil, fmt.Errorf("run event stream is unavailable"))
		return
	}
	log.append("metadata", map[string]any{"run_id": runID, "attempt": 1})
	modes := requestedModes(request.StreamMode)
	valuesPublished := false
	stream := graph.Stream(runContext, input...)
	defer stream.Close()
	for {
		event, nextErr := stream.Next(runContext)
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				break
			}
			server.completeRun(runID, nil, nextErr)
			return
		}
		valuesPublished = server.publishAgentEvent(log, modes, event) || valuesPublished
	}
	result, err := stream.Result(runContext)
	if err != nil {
		server.completeRun(runID, nil, err)
		return
	}
	if modes["values"] && !valuesPublished {
		log.append("values", resultValues(result))
	}
	server.completeRun(runID, &result, nil)
}

func requestFromKwargs(kwargs map[string]any) (createRunRequest, error) {
	data, err := json.Marshal(kwargs)
	if err != nil {
		return createRunRequest{}, err
	}
	var request createRunRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return createRunRequest{}, err
	}
	return request, nil
}

func (server *Server) eventLog(runID string) *eventLog {
	server.mu.RLock()
	log := server.events[runID]
	server.mu.RUnlock()
	return log
}

func (server *Server) publishAgentEvent(log *eventLog, modes map[string]bool, event dagent.Event) bool {
	switch event.Mode {
	case dagent.EventUpdate:
		if modes["updates"] {
			log.append("updates", map[string]any{event.Node: stateToProtocol(event.Update)})
		}
	case dagent.EventValues:
		if modes["values"] {
			log.append("values", stateToProtocol(event.Values))
			return true
		}
	case dagent.EventCustom:
		if modes["custom"] {
			var data any
			if json.Unmarshal(event.Custom, &data) != nil {
				data = string(event.Custom)
			}
			log.append("custom", data)
		}
	case dagent.EventToken:
		if event.Chunk == nil {
			return false
		}
		chunk := messageToProtocol(event.Chunk.MessageDelta)
		node := event.Node
		if node == "" {
			node = "model"
		}
		metadata := map[string]any{"langgraph_node": node, "langgraph_step": event.Step}
		if modes["messages"] || modes["messages-tuple"] {
			log.append("messages", []any{chunk, metadata})
		}
	case dagent.EventTask:
		if modes["tasks"] {
			log.append("tasks", map[string]any{"id": event.TaskID, "name": event.Node, "input": nil, "interrupts": []any{}})
		}
	case dagent.EventInterrupt:
		if event.Interrupt != nil {
			log.append("updates", map[string]any{"__interrupt__": []any{map[string]any{
				"id": event.Interrupt.ID, "value": event.Interrupt.Value,
			}}})
		}
	case dagent.EventChild, dagent.EventToolProgress:
		if modes["custom"] {
			log.append("custom", event)
		}
	}
	return false
}

func (server *Server) completeRun(runID string, result *dagent.Result, runErr error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	run := server.runs[runID]
	if run == nil {
		return
	}
	thread := server.threads[run.ThreadID]
	log := server.events[runID]
	now := server.timestamp()
	run.UpdatedAt = now
	if thread != nil {
		thread.UpdatedAt = now
	}
	switch {
	case runErr != nil && errors.Is(runErr, context.Canceled):
		run.Status = "interrupted"
		if thread != nil {
			thread.Status = "interrupted"
		}
		if log != nil {
			log.append("error", map[string]any{"error": "run canceled"})
		}
	case runErr != nil:
		run.Status = "error"
		if thread != nil {
			thread.Status = "error"
		}
		if log != nil {
			log.append("error", map[string]any{"error": runErr.Error()})
		}
	case result != nil && len(result.Interrupts) > 0:
		run.Status = "interrupted"
		if thread != nil {
			thread.Status = "interrupted"
			thread.Values = resultValues(*result)
			thread.Interrupts = interruptMap(result.Interrupts)
		}
	case result != nil:
		run.Status = "success"
		if thread != nil {
			thread.Status = "idle"
			thread.Values = resultValues(*result)
			thread.Interrupts = nil
		}
	default:
		run.Status = "error"
		if thread != nil {
			thread.Status = "error"
		}
		if log != nil {
			log.append("error", map[string]any{"error": "run ended without a result"})
		}
	}
	_ = server.persistLocked()
	if log != nil {
		log.finish()
	}
}

func (server *Server) failRunLocked(run *Run, err error) {
	run.Status = "error"
	run.UpdatedAt = server.timestamp()
	if thread := server.threads[run.ThreadID]; thread != nil {
		thread.Status = "error"
		thread.UpdatedAt = run.UpdatedAt
	}
	if log := server.events[run.RunID]; log != nil {
		log.append("error", map[string]any{"error": err.Error()})
		log.finish()
	}
	_ = server.persistLocked()
}

func resultValues(result dagent.Result) map[string]any {
	values := stateToProtocol(result.State)
	if len(result.Messages) > 0 {
		messages := make([]any, len(result.Messages))
		for index, message := range result.Messages {
			messages[index] = messageToProtocol(message)
		}
		values[dagent.MessagesKey] = messages
	}
	if len(result.Structured) > 0 {
		var structured any
		if json.Unmarshal(result.Structured, &structured) == nil {
			values[dagent.StructuredResponseKey] = structured
		}
	}
	return values
}

func interruptMap(interrupts []dagent.Interrupt) map[string]any {
	result := map[string]any{}
	for _, interrupt := range interrupts {
		result[interrupt.ID] = interrupt.Value
	}
	return result
}

func requestedModes(raw any) map[string]bool {
	result := map[string]bool{"values": true}
	switch value := raw.(type) {
	case string:
		result = map[string]bool{value: true}
	case []any:
		result = map[string]bool{}
		for _, item := range value {
			if mode, ok := item.(string); ok {
				result[mode] = true
			}
		}
	case []string:
		result = map[string]bool{}
		for _, mode := range value {
			result[mode] = true
		}
	}
	return result
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	result := cloneJSON(base)
	if result == nil {
		result = map[string]any{}
	}
	for key, value := range overlay {
		if key == "configurable" {
			left, _ := result[key].(map[string]any)
			right, _ := value.(map[string]any)
			result[key] = mergeMaps(left, right)
			continue
		}
		result[key] = value
	}
	return result
}
