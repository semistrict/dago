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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/dastate"
	"github.com/semistrict/dago/dastore"
)

type mapGraph map[string]any

func (mapGraph) Stream(context.Context, ...dagent.RunOption) *dagent.Stream { return nil }
func (mapGraph) State(context.Context, dacheckpoint.Config) (dagent.Snapshot, error) {
	return dagent.Snapshot{}, nil
}
func (mapGraph) UpdateState(context.Context, dacheckpoint.Config, dastate.Values) (dagent.Snapshot, error) {
	return dagent.Snapshot{}, nil
}
func (mapGraph) History(context.Context, dacheckpoint.Config, dacheckpoint.ListOptions) ([]dacheckpoint.Tuple, error) {
	return nil, nil
}
func (mapGraph) Fork(context.Context, string, string) error { return nil }
func (mapGraph) DeleteThread(context.Context, string) error { return nil }

func TestAdaptFactoryRejectsNilStaticFactoryAndTypedNilResult(t *testing.T) {
	var missing func(context.Context, Runtime) (Graph, error)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("AdaptFactory accepted a nil factory")
			}
		}()
		AdaptFactory(missing)
	}()

	adapted := AdaptFactory(func(context.Context, Runtime) (mapGraph, error) { return nil, nil })
	if _, err := adapted(context.Background(), Runtime{}); err == nil || !strings.Contains(err.Error(), "returned nil") {
		t.Fatalf("typed-nil graph error = %v", err)
	}
}

func TestGraphForAssistantRejectsTypedNilFromDirectFactory(t *testing.T) {
	server := &Server{graphs: map[string]GraphRegistration{
		"agent": {ID: "agent", Factory: func(context.Context, Runtime) (Graph, error) {
			var graph mapGraph
			return graph, nil
		}},
	}}
	if _, err := server.graphForAssistant(context.Background(), &Assistant{GraphID: "agent"}); err == nil || !strings.Contains(err.Error(), "returned nil") {
		t.Fatalf("typed-nil direct graph error = %v", err)
	}
}

func TestNilDependencyCoversEveryNilableKind(t *testing.T) {
	var pointer *int
	var mapping map[string]int
	var function func()
	var slice []int
	var channel chan int
	var interfaceValue any
	for name, value := range map[string]any{
		"pointer": pointer, "map": mapping, "function": function,
		"slice": slice, "channel": channel, "interface": interfaceValue,
	} {
		t.Run(name, func(t *testing.T) {
			if !nilDependency(value) {
				t.Fatal("typed nil was not detected")
			}
		})
	}
}

func TestNewPanicsForInvalidStaticGraphConfiguration(t *testing.T) {
	factory := func(context.Context, Runtime) (Graph, error) { return nil, nil }
	for name, graphs := range map[string][]GraphRegistration{
		"missing graph":   nil,
		"missing factory": {{ID: "agent"}},
		"invalid id":      {{ID: "bad/id", Factory: factory}},
		"invalid schema":  {{ID: "agent", Factory: factory, InputSchema: json.RawMessage(`{`)}},
		"duplicate id":    {{ID: "agent", Factory: factory}, {ID: " agent ", Factory: factory}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			_, _ = New(graphs, Options{})
		})
	}
}

func TestNewTreatsTypedNilOptionalStoresAsOmitted(t *testing.T) {
	var saver *dacheckpoint.MemorySaver
	var store *dastore.Memory
	server, err := New([]GraphRegistration{{
		ID: "agent", Factory: func(context.Context, Runtime) (Graph, error) { return nil, nil },
	}}, Options{Saver: saver, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	if server.saver == nil || server.store == nil {
		t.Fatal("typed-nil optional dependencies did not select defaults")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func testServer(t *testing.T, delay time.Duration) (*Server, *http.Client, string) {
	t.Helper()
	server, err := New([]GraphRegistration{{
		ID: "agent", Description: "Test agent",
		Factory: func(_ context.Context, runtime Runtime) (Graph, error) {
			return dagent.New(
				modeltest.NewPredictable(modeltest.PredictableOptions{ResponseDelay: delay}), dagent.Options{

					Saver: runtime.Saver, Store: runtime.Store,
				}), nil
		},
	}}, Options{})
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

func TestStoreNamespacesEndpointUsesBoundedOptions(t *testing.T) {
	server, client, endpoint := testServer(t, 0)
	operations := make([]dastore.Operation, dastore.DefaultListNamespacesLimit+5)
	for index := range operations {
		operations[index] = dastore.Operation{
			Namespace: dastore.Namespace{"tenant", fmt.Sprintf("%03d", index), "tail"},
			Key:       "item", PutValue: map[string]any{"index": index},
		}
	}
	if _, err := server.store.Batch(context.Background(), operations); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Namespaces []dastore.Namespace `json:"namespaces"`
	}
	requestJSON(t, client, http.MethodPost, endpoint+"/store/namespaces", map[string]any{}, &result)
	if len(result.Namespaces) != dastore.DefaultListNamespacesLimit {
		t.Fatalf("default namespace length = %d, want %d", len(result.Namespaces), dastore.DefaultListNamespacesLimit)
	}
	requestJSON(t, client, http.MethodPost, endpoint+"/store/namespaces", map[string]any{
		"prefix": []string{"tenant", "*"}, "suffix": []string{"tail"},
		"max_depth": 2, "limit": 2, "offset": 98,
	}, &result)
	want := []dastore.Namespace{{"tenant", "098"}, {"tenant", "099"}}
	if !reflect.DeepEqual(result.Namespaces, want) {
		t.Fatalf("filtered namespaces = %#v, want %#v", result.Namespaces, want)
	}

	body := bytes.NewBufferString(`{"limit":-1}`)
	request, err := http.NewRequest(http.MethodPost, endpoint+"/store/namespaces", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative namespace limit status = %d, want %d", response.StatusCode, http.StatusBadRequest)
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
