package daserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel/modeltest"
)

func testServer(t *testing.T, delay time.Duration) (*Server, *http.Client, string) {
	t.Helper()
	server, err := New(Options{Graphs: []GraphRegistration{{
		ID: "agent", Description: "Test agent",
		Factory: func(_ context.Context, runtime Runtime) (Graph, error) {
			return dagent.New(dagent.Options{
				Model: modeltest.NewPredictable(modeltest.PredictableOptions{ResponseDelay: delay}),
				Saver: runtime.Saver, Store: runtime.Store,
			})
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: handlerTransport{handler: server.Handler()}}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})
	return server, client, "http://dago.test"
}

type handlerTransport struct{ handler http.Handler }

func (transport handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func requestJSON(t *testing.T, client *http.Client, method, target string, input, output any) *http.Response {
	t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("%s %s: HTTP %d: %s", method, target, response.StatusCode, data)
	}
	if output != nil {
		defer response.Body.Close()
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	} else if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body.Close()
	}
	return response
}

func TestStudioDiscoveryRunStateHistoryAndFork(t *testing.T) {
	_, client, endpoint := testServer(t, 0)
	var info map[string]any
	requestJSON(t, client, http.MethodGet, endpoint+"/info", nil, &info)
	if info["context"] != "go" {
		t.Fatalf("info = %#v", info)
	}
	var assistants []Assistant
	requestJSON(t, client, http.MethodPost, endpoint+"/assistants/search", map[string]any{}, &assistants)
	if len(assistants) != 1 || assistants[0].GraphID != "agent" || assistants[0].AssistantID != graphUUID("agent") {
		t.Fatalf("assistants = %#v", assistants)
	}
	var graph DrawableGraph
	requestJSON(t, client, http.MethodGet, endpoint+"/assistants/agent/graph", nil, &graph)
	if len(graph.Nodes) != 4 || len(graph.Edges) != 4 {
		t.Fatalf("graph = %#v", graph)
	}
	var schemas map[string]any
	requestJSON(t, client, http.MethodGet, endpoint+"/assistants/agent/schemas", nil, &schemas)
	stateSchema := schemas["state_schema"].(map[string]any)
	if stateSchema["type"] != "object" {
		t.Fatalf("schemas = %#v", schemas)
	}
	messages := stateSchema["properties"].(map[string]any)["messages"].(map[string]any)
	item := messages["items"].(map[string]any)
	if item["type"] != "array" || messages["langgraph_type"] != "messages" {
		t.Fatalf("messages schema is not Studio chat-compatible: %#v", messages)
	}

	var thread Thread
	requestJSON(t, client, http.MethodPost, endpoint+"/threads", map[string]any{"metadata": map[string]any{"purpose": "test"}}, &thread)
	streamRequest := map[string]any{
		"assistant_id": "agent",
		"input":        map[string]any{"messages": []any{map[string]any{"role": "user", "content": "echo: studio works"}}},
		"stream_mode":  []string{"messages", "updates", "values"},
	}
	response := requestJSON(t, client, http.MethodPost, endpoint+"/threads/"+thread.ThreadID+"/runs/stream", streamRequest, nil)
	events := readSSE(t, response.Body)
	response.Body.Close()
	if events[0].Event != "metadata" || !hasSSEText(events, "messages", "studio works") || !hasSSEText(events, "values", "studio works") {
		t.Fatalf("events = %#v", events)
	}

	var state ThreadState
	requestJSON(t, client, http.MethodGet, endpoint+"/threads/"+thread.ThreadID+"/state", nil, &state)
	if state.Checkpoint == nil || state.Checkpoint.CheckpointID == "" || !containsJSON(state.Values, "studio works") {
		t.Fatalf("state = %#v", state)
	}
	var updated map[string]any
	requestJSON(t, client, http.MethodPost, endpoint+"/threads/"+thread.ThreadID+"/state", map[string]any{
		"values": map[string]any{"messages": []any{map[string]any{"role": "user", "content": "state edit"}}},
	}, &updated)
	if !containsJSON(updated, "checkpoint_id") {
		t.Fatalf("updated state response = %#v", updated)
	}
	requestJSON(t, client, http.MethodGet, endpoint+"/threads/"+thread.ThreadID+"/state", nil, &state)
	if !containsJSON(state.Values, "state edit") || state.Next == nil {
		t.Fatalf("updated state = %#v", state)
	}
	var history []ThreadState
	requestJSON(t, client, http.MethodPost, endpoint+"/threads/"+thread.ThreadID+"/history", map[string]any{"limit": 20}, &history)
	if len(history) < 2 || !containsJSON(history[0].Values, "studio works") {
		t.Fatalf("history = %#v", history)
	}
	var copied Thread
	requestJSON(t, client, http.MethodPost, endpoint+"/threads/"+thread.ThreadID+"/copy", map[string]any{}, &copied)
	var copiedState ThreadState
	requestJSON(t, client, http.MethodGet, endpoint+"/threads/"+copied.ThreadID+"/state", nil, &copiedState)
	if !containsJSON(copiedState.Values, "studio works") {
		t.Fatalf("copied state = %#v", copiedState)
	}
}

func TestAssistantThreadRunAndStoreResources(t *testing.T) {
	_, client, endpoint := testServer(t, 0)
	var assistant Assistant
	requestJSON(t, client, http.MethodPost, endpoint+"/assistants", map[string]any{
		"graph_id": "agent", "name": "Configured", "config": map[string]any{"configurable": map[string]any{"model": "test"}},
	}, &assistant)
	requestJSON(t, client, http.MethodPatch, endpoint+"/assistants/"+assistant.AssistantID, map[string]any{"metadata": map[string]any{"team": "one"}}, &assistant)
	if assistant.Metadata["team"] != "one" || assistant.Version != 2 {
		t.Fatalf("assistant = %#v", assistant)
	}

	requestJSON(t, client, http.MethodPut, endpoint+"/store/items", map[string]any{"namespace": []string{"users", "one"}, "key": "preference", "value": map[string]any{"theme": "dark"}}, nil)
	var item map[string]any
	requestJSON(t, client, http.MethodGet, endpoint+"/store/items?namespace=users.one&key=preference", nil, &item)
	if item["value"].(map[string]any)["theme"] != "dark" {
		t.Fatalf("store item = %#v", item)
	}

	var thread Thread
	requestJSON(t, client, http.MethodPost, endpoint+"/threads", map[string]any{}, &thread)
	var run Run
	requestJSON(t, client, http.MethodPost, fmt.Sprintf("%s/threads/%s/runs", endpoint, thread.ThreadID), map[string]any{
		"assistant_id": assistant.AssistantID, "input": map[string]any{"messages": []any{map[string]any{"type": "human", "content": "hello"}}},
	}, &run)
	requestJSON(t, client, http.MethodGet, fmt.Sprintf("%s/threads/%s/runs/%s/join", endpoint, thread.ThreadID, run.RunID), nil, &item)
	requestJSON(t, client, http.MethodGet, fmt.Sprintf("%s/threads/%s/runs/%s", endpoint, thread.ThreadID, run.RunID), nil, &run)
	if run.Status != "success" {
		t.Fatalf("run = %#v", run)
	}
	replay, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/threads/%s/runs/%s/stream", endpoint, thread.ThreadID, run.RunID), nil)
	if err != nil {
		t.Fatal(err)
	}
	replay.Header.Set("Last-Event-ID", "999999")
	replayResponse, err := client.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d", replayResponse.StatusCode)
	}
	wrongThread, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/threads/wrong/runs/%s/stream", endpoint, run.RunID), nil)
	wrongResponse, err := client.Do(wrongThread)
	if err != nil {
		t.Fatal(err)
	}
	wrongResponse.Body.Close()
	if wrongResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong-thread stream status = %d", wrongResponse.StatusCode)
	}
}

func TestRunCancellationAndStudioCORS(t *testing.T) {
	_, client, endpoint := testServer(t, time.Second)
	var thread Thread
	requestJSON(t, client, http.MethodPost, endpoint+"/threads", map[string]any{}, &thread)
	var run Run
	requestJSON(t, client, http.MethodPost, endpoint+"/threads/"+thread.ThreadID+"/runs", map[string]any{
		"assistant_id": "agent", "input": map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}},
	}, &run)
	deadline := time.Now().Add(time.Second)
	for run.Status == "pending" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		requestJSON(t, client, http.MethodGet, endpoint+"/threads/"+thread.ThreadID+"/runs/"+run.RunID, nil, &run)
	}
	requestJSON(t, client, http.MethodPost, endpoint+"/threads/"+thread.ThreadID+"/runs/"+run.RunID+"/cancel?wait=1", map[string]any{}, nil)
	requestJSON(t, client, http.MethodGet, endpoint+"/threads/"+thread.ThreadID+"/runs/"+run.RunID, nil, &run)
	if run.Status != "interrupted" {
		t.Fatalf("canceled run = %#v", run)
	}

	request, _ := http.NewRequest(http.MethodOptions, endpoint+"/threads", nil)
	request.Header.Set("Origin", "https://smith.langchain.com")
	request.Header.Set("Access-Control-Request-Private-Network", "true")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("Access-Control-Allow-Origin") != "https://smith.langchain.com" || response.Header.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("CORS response = %d %#v", response.StatusCode, response.Header)
	}
}

type sseRecord struct{ Event, Data string }

func readSSE(t *testing.T, body io.Reader) []sseRecord {
	t.Helper()
	scanner := bufio.NewScanner(body)
	var result []sseRecord
	current := sseRecord{}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = strings.TrimPrefix(line, "data: ")
		case line == "" && current.Event != "":
			result = append(result, current)
			current = sseRecord{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func hasSSEText(events []sseRecord, event, text string) bool {
	for _, item := range events {
		if item.Event == event && strings.Contains(item.Data, text) {
			return true
		}
	}
	return false
}

func containsJSON(value any, text string) bool {
	data, _ := json.Marshal(value)
	return strings.Contains(string(data), text)
}
