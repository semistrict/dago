package daserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dastore"
)

const maxRequestBody = 8 << 20

type searchRequest struct {
	Metadata map[string]any `json:"metadata"`
	GraphID  string         `json:"graph_id"`
	Name     string         `json:"name"`
	Status   string         `json:"status"`
	IDs      []string       `json:"ids"`
	Limit    int            `json:"limit"`
	Offset   int            `json:"offset"`
}

func (server *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /info", server.handleInfo)

	mux.HandleFunc("POST /assistants", server.handleCreateAssistant)
	mux.HandleFunc("POST /assistants/search", server.handleSearchAssistants)
	mux.HandleFunc("POST /assistants/count", server.handleCountAssistants)
	mux.HandleFunc("GET /assistants/{assistant_id}", server.handleGetAssistant)
	mux.HandleFunc("PATCH /assistants/{assistant_id}", server.handlePatchAssistant)
	mux.HandleFunc("DELETE /assistants/{assistant_id}", server.handleDeleteAssistant)
	mux.HandleFunc("GET /assistants/{assistant_id}/graph", server.handleAssistantGraph)
	mux.HandleFunc("GET /assistants/{assistant_id}/schemas", server.handleAssistantSchemas)
	mux.HandleFunc("GET /assistants/{assistant_id}/subgraphs", server.handleAssistantSubgraphs)
	mux.HandleFunc("GET /assistants/{assistant_id}/subgraphs/{namespace}", server.handleAssistantSubgraphs)

	mux.HandleFunc("POST /threads", server.handleCreateThread)
	mux.HandleFunc("POST /threads/search", server.handleSearchThreads)
	mux.HandleFunc("POST /threads/count", server.handleCountThreads)
	mux.HandleFunc("GET /threads/{thread_id}", server.handleGetThread)
	mux.HandleFunc("PATCH /threads/{thread_id}", server.handlePatchThread)
	mux.HandleFunc("DELETE /threads/{thread_id}", server.handleDeleteThread)
	mux.HandleFunc("POST /threads/{thread_id}/copy", server.handleCopyThread)
	mux.HandleFunc("GET /threads/{thread_id}/state", server.handleThreadState)
	mux.HandleFunc("POST /threads/{thread_id}/state", server.handleUpdateThreadState)
	mux.HandleFunc("GET /threads/{thread_id}/state/{checkpoint_id}", server.handleThreadState)
	mux.HandleFunc("POST /threads/{thread_id}/state/checkpoint", server.handleThreadStatePost)
	mux.HandleFunc("GET /threads/{thread_id}/history", server.handleThreadHistory)
	mux.HandleFunc("POST /threads/{thread_id}/history", server.handleThreadHistoryPost)

	mux.HandleFunc("GET /threads/{thread_id}/runs", server.handleListRuns)
	mux.HandleFunc("POST /threads/{thread_id}/runs", server.handleCreateRun)
	mux.HandleFunc("POST /threads/{thread_id}/runs/stream", server.handleCreateRunStream)
	mux.HandleFunc("POST /threads/{thread_id}/runs/wait", server.handleCreateRunWait)
	mux.HandleFunc("GET /threads/{thread_id}/runs/{run_id}", server.handleGetRun)
	mux.HandleFunc("DELETE /threads/{thread_id}/runs/{run_id}", server.handleDeleteRun)
	mux.HandleFunc("GET /threads/{thread_id}/runs/{run_id}/join", server.handleJoinRun)
	mux.HandleFunc("GET /threads/{thread_id}/runs/{run_id}/stream", server.handleRunStream)
	mux.HandleFunc("POST /threads/{thread_id}/runs/{run_id}/cancel", server.handleCancelRun)

	mux.HandleFunc("GET /store/items", server.handleStoreGet)
	mux.HandleFunc("PUT /store/items", server.handleStorePut)
	mux.HandleFunc("DELETE /store/items", server.handleStoreDelete)
	mux.HandleFunc("POST /store/items/search", server.handleStoreSearch)
	mux.HandleFunc("POST /store/namespaces", server.handleStoreNamespaces)

	return server.middleware(mux)
}

func (server *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		origin := request.Header.Get("Origin")
		allowed := origin == "" || server.originAllowed(origin)
		if origin != "" && allowed {
			writer.Header().Set("Access-Control-Allow-Origin", origin)
			writer.Header().Set("Vary", "Origin")
			writer.Header().Set("Access-Control-Allow-Credentials", "true")
			writer.Header().Set("Access-Control-Expose-Headers", "Content-Location, Link, X-Pagination-Total, X-Pagination-Next")
		}
		if request.Header.Get("Access-Control-Request-Private-Network") == "true" && allowed {
			writer.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if request.Method == http.MethodOptions {
			if !allowed {
				writeError(writer, http.StatusForbidden, "origin is not allowed")
				return
			}
			writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			requestHeaders := request.Header.Get("Access-Control-Request-Headers")
			if requestHeaders == "" {
				requestHeaders = "Content-Type, Authorization, X-Api-Key, X-Auth-Scheme, X-Tenant-Id, Last-Event-ID, Traceparent, Tracestate"
			}
			writer.Header().Set("Access-Control-Allow-Headers", requestHeaders)
			writer.Header().Set("Access-Control-Max-Age", "600")
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if origin != "" && !allowed {
			writeError(writer, http.StatusForbidden, "origin is not allowed")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) originAllowed(origin string) bool {
	if origin == "https://smith.langchain.com" {
		return true
	}
	if server.origins[origin] || server.origins["*"] {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func (server *Server) handleInfo(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"version": "0.1.0", "langgraph_version": nil, "langgraph_js_version": nil,
		"context": "go", "flags": map[string]any{
			"assistants": true, "crons": false, "langsmith": false,
			"langsmith_tracing_replicas": false,
		},
	})
}

func (server *Server) handleCreateAssistant(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		AssistantID string         `json:"assistant_id"`
		GraphID     string         `json:"graph_id"`
		Config      map[string]any `json:"config"`
		Context     any            `json:"context"`
		Metadata    map[string]any `json:"metadata"`
		IfExists    string         `json:"if_exists"`
		Name        string         `json:"name"`
		Description *string        `json:"description"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if _, exists := server.graphs[payload.GraphID]; !exists {
		writeError(writer, http.StatusNotFound, "graph not found")
		return
	}
	id := payload.AssistantID
	if id == "" {
		var err error
		id, err = newUUID()
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if existing := server.assistants[id]; existing != nil {
		if payload.IfExists == "do_nothing" {
			writeJSON(writer, http.StatusOK, cloneJSON(*existing))
			return
		}
		writeError(writer, http.StatusConflict, "assistant already exists")
		return
	}
	if payload.Name == "" {
		payload.Name = "Untitled"
	}
	now := server.timestamp()
	assistant := &Assistant{
		AssistantID: id, GraphID: payload.GraphID, Name: payload.Name,
		Description: payload.Description, Config: nonNilMap(payload.Config), Context: payload.Context,
		Metadata: nonNilMap(payload.Metadata), CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	server.assistants[id] = assistant
	if err := server.persistLocked(); err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, cloneJSON(*assistant))
}

func (server *Server) handleSearchAssistants(writer http.ResponseWriter, request *http.Request) {
	var search searchRequest
	if !decodeJSON(writer, request, &search) {
		return
	}
	server.mu.RLock()
	items := sortedAssistants(server.assistants)
	server.mu.RUnlock()
	filtered := items[:0]
	for _, item := range items {
		if search.GraphID != "" && item.GraphID != search.GraphID || search.Name != "" && !containsFold(item.Name, search.Name) || !mapMatches(item.Metadata, search.Metadata) {
			continue
		}
		filtered = append(filtered, item)
	}
	setPagination(writer, len(filtered), search.Offset, search.Limit)
	writeJSON(writer, http.StatusOK, paginate(filtered, search.Offset, search.Limit))
}

func (server *Server) handleCountAssistants(writer http.ResponseWriter, request *http.Request) {
	var search searchRequest
	if !decodeJSON(writer, request, &search) {
		return
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	count := 0
	for _, item := range server.assistants {
		if (search.GraphID == "" || item.GraphID == search.GraphID) && (search.Name == "" || containsFold(item.Name, search.Name)) && mapMatches(item.Metadata, search.Metadata) {
			count++
		}
	}
	writeJSON(writer, http.StatusOK, count)
}

func (server *Server) resolveAssistantLocked(id string) *Assistant {
	if registration := server.graphs[id]; registration.Factory != nil {
		id = graphUUID(id)
	}
	return server.assistants[id]
}

func (server *Server) handleGetAssistant(writer http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	assistant := server.resolveAssistantLocked(request.PathValue("assistant_id"))
	if assistant != nil {
		copy := cloneJSON(*assistant)
		server.mu.RUnlock()
		writeJSON(writer, http.StatusOK, copy)
		return
	}
	server.mu.RUnlock()
	writeError(writer, http.StatusNotFound, "assistant not found")
}

func (server *Server) handlePatchAssistant(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		GraphID     string         `json:"graph_id"`
		Config      map[string]any `json:"config"`
		Context     any            `json:"context"`
		Name        *string        `json:"name"`
		Description *string        `json:"description"`
		Metadata    map[string]any `json:"metadata"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	assistant := server.resolveAssistantLocked(request.PathValue("assistant_id"))
	if assistant == nil {
		writeError(writer, http.StatusNotFound, "assistant not found")
		return
	}
	if payload.GraphID != "" {
		if _, exists := server.graphs[payload.GraphID]; !exists {
			writeError(writer, http.StatusNotFound, "graph not found")
			return
		}
		assistant.GraphID = payload.GraphID
	}
	if payload.Config != nil {
		assistant.Config = payload.Config
	}
	if payload.Context != nil {
		assistant.Context = payload.Context
	}
	if payload.Name != nil {
		assistant.Name = *payload.Name
	}
	if payload.Description != nil {
		assistant.Description = payload.Description
	}
	assistant.Metadata = mergeMaps(assistant.Metadata, payload.Metadata)
	assistant.UpdatedAt = server.timestamp()
	assistant.Version++
	if err := server.persistLocked(); err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, cloneJSON(*assistant))
}

func (server *Server) handleDeleteAssistant(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	defer server.mu.Unlock()
	assistant := server.resolveAssistantLocked(request.PathValue("assistant_id"))
	if assistant == nil {
		writeError(writer, 404, "assistant not found")
		return
	}
	delete(server.assistants, assistant.AssistantID)
	if request.URL.Query().Get("delete_threads") == "true" {
		for id, thread := range server.threads {
			if thread.Metadata["assistant_id"] == assistant.AssistantID {
				delete(server.threads, id)
				_ = server.saver.DeleteThread(request.Context(), id)
			}
		}
	}
	if err := server.persistLocked(); err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, nil)
}

func (server *Server) handleAssistantGraph(writer http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	assistant := server.resolveAssistantLocked(request.PathValue("assistant_id"))
	if assistant == nil {
		server.mu.RUnlock()
		writeError(writer, 404, "assistant not found")
		return
	}
	graph := cloneJSON(server.graphs[assistant.GraphID].Graph)
	server.mu.RUnlock()
	writeJSON(writer, http.StatusOK, graph)
}

func (server *Server) handleAssistantSchemas(writer http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	assistant := server.resolveAssistantLocked(request.PathValue("assistant_id"))
	if assistant == nil {
		server.mu.RUnlock()
		writeError(writer, 404, "assistant not found")
		return
	}
	registration := server.graphs[assistant.GraphID]
	server.mu.RUnlock()
	writeJSON(writer, http.StatusOK, map[string]any{
		"graph_id": assistant.GraphID, "input_schema": rawJSON(registration.InputSchema),
		"output_schema": rawJSON(registration.OutputSchema), "state_schema": rawJSON(registration.StateSchema),
		"config_schema": rawJSON(registration.ConfigSchema), "context_schema": rawJSON(registration.ContextSchema),
	})
}

func (server *Server) handleAssistantSubgraphs(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{})
}

func (server *Server) handleCreateThread(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		ThreadID string         `json:"thread_id"`
		Metadata map[string]any `json:"metadata"`
		IfExists string         `json:"if_exists"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	if payload.ThreadID == "" {
		var err error
		payload.ThreadID, err = newUUID()
		if err != nil {
			writeError(writer, http.StatusInternalServerError, err.Error())
			return
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if existing := server.threads[payload.ThreadID]; existing != nil {
		if payload.IfExists == "do_nothing" {
			writeJSON(writer, 200, cloneJSON(*existing))
			return
		}
		writeError(writer, 409, "thread already exists")
		return
	}
	now := server.timestamp()
	thread := &Thread{ThreadID: payload.ThreadID, CreatedAt: now, UpdatedAt: now, Metadata: nonNilMap(payload.Metadata), Status: "idle"}
	server.threads[payload.ThreadID] = thread
	if err := server.persistLocked(); err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, 200, cloneJSON(*thread))
}

func (server *Server) handleSearchThreads(writer http.ResponseWriter, request *http.Request) {
	var search searchRequest
	if !decodeJSON(writer, request, &search) {
		return
	}
	server.mu.RLock()
	items := sortedThreads(server.threads)
	server.mu.RUnlock()
	filtered := items[:0]
	idSet := map[string]bool{}
	for _, id := range search.IDs {
		idSet[id] = true
	}
	for _, item := range items {
		if search.Status != "" && item.Status != search.Status || len(idSet) > 0 && !idSet[item.ThreadID] || !mapMatches(item.Metadata, search.Metadata) {
			continue
		}
		filtered = append(filtered, item)
	}
	setPagination(writer, len(filtered), search.Offset, search.Limit)
	writeJSON(writer, 200, paginate(filtered, search.Offset, search.Limit))
}

func (server *Server) handleCountThreads(writer http.ResponseWriter, request *http.Request) {
	var search searchRequest
	if !decodeJSON(writer, request, &search) {
		return
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	count := 0
	for _, item := range server.threads {
		if (search.Status == "" || item.Status == search.Status) && mapMatches(item.Metadata, search.Metadata) {
			count++
		}
	}
	writeJSON(writer, 200, count)
}

func (server *Server) handleGetThread(writer http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	thread := server.threads[request.PathValue("thread_id")]
	if thread == nil {
		server.mu.RUnlock()
		writeError(writer, 404, "thread not found")
		return
	}
	copy := cloneJSON(*thread)
	server.mu.RUnlock()
	writeJSON(writer, 200, copy)
}

func (server *Server) handlePatchThread(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Metadata map[string]any `json:"metadata"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	thread := server.threads[request.PathValue("thread_id")]
	if thread == nil {
		writeError(writer, 404, "thread not found")
		return
	}
	thread.Metadata = mergeMaps(thread.Metadata, payload.Metadata)
	thread.UpdatedAt = server.timestamp()
	if err := server.persistLocked(); err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, 200, cloneJSON(*thread))
}

func (server *Server) handleDeleteThread(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("thread_id")
	server.mu.Lock()
	if server.threads[id] == nil {
		server.mu.Unlock()
		writeError(writer, 404, "thread not found")
		return
	}
	delete(server.threads, id)
	for runID, run := range server.runs {
		if run.ThreadID == id {
			if cancel := server.active[runID]; cancel != nil {
				cancel()
			}
			if log := server.events[runID]; log != nil {
				log.append("error", map[string]any{"error": "thread deleted"})
				log.finish()
			}
			delete(server.runs, runID)
			delete(server.events, runID)
		}
	}
	err := server.persistLocked()
	server.mu.Unlock()
	if err == nil {
		err = server.saver.DeleteThread(request.Context(), id)
	}
	if err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) handleCopyThread(writer http.ResponseWriter, request *http.Request) {
	sourceID := request.PathValue("thread_id")
	targetID, err := newUUID()
	if err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	server.mu.RLock()
	source := server.threads[sourceID]
	if source == nil {
		server.mu.RUnlock()
		writeError(writer, 404, "thread not found")
		return
	}
	copied := cloneJSON(*source)
	server.mu.RUnlock()
	if err := server.saver.CopyThread(request.Context(), sourceID, targetID); err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	now := server.timestamp()
	copied.ThreadID = targetID
	copied.CreatedAt = now
	copied.UpdatedAt = now
	copied.Status = "idle"
	server.mu.Lock()
	server.threads[targetID] = &copied
	err = server.persistLocked()
	server.mu.Unlock()
	if err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, 200, copied)
}

func (server *Server) handleThreadState(writer http.ResponseWriter, request *http.Request) {
	server.writeThreadState(writer, request, request.PathValue("checkpoint_id"))
}

func (server *Server) handleThreadStatePost(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Checkpoint *Checkpoint `json:"checkpoint"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	id := ""
	if payload.Checkpoint != nil {
		id = payload.Checkpoint.CheckpointID
	}
	server.writeThreadState(writer, request, id)
}

func (server *Server) handleUpdateThreadState(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Values       any         `json:"values"`
		AsNode       string      `json:"as_node"`
		CheckpointID string      `json:"checkpoint_id"`
		Checkpoint   *Checkpoint `json:"checkpoint"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	values, err := protocolStateValues(payload.Values)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	threadID := request.PathValue("thread_id")
	assistant, err := server.assistantForThread(threadID)
	if err != nil {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	graph, err := server.graphForAssistant(request.Context(), assistant)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	config := dacheckpoint.Config{ThreadID: threadID, CheckpointID: payload.CheckpointID}
	if payload.Checkpoint != nil {
		config.Namespace = payload.Checkpoint.Namespace
		if payload.Checkpoint.CheckpointID != "" {
			config.CheckpointID = payload.Checkpoint.CheckpointID
		}
	}
	snapshot, err := graph.UpdateState(request.Context(), config, values)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	server.mu.Lock()
	if thread := server.threads[threadID]; thread != nil {
		thread.Values = stateToProtocol(snapshot.State)
		thread.UpdatedAt = server.timestamp()
	}
	err = server.persistLocked()
	server.mu.Unlock()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"checkpoint": checkpointFromConfig(snapshot.Config)})
}

func (server *Server) writeThreadState(writer http.ResponseWriter, request *http.Request, checkpointID string) {
	state, err := server.threadState(request.Context(), request.PathValue("thread_id"), checkpointID)
	if err != nil {
		if errors.Is(err, errNotFound) {
			writeError(writer, 404, err.Error())
		} else {
			writeError(writer, 500, err.Error())
		}
		return
	}
	writeJSON(writer, 200, state)
}

func (server *Server) handleThreadHistory(writer http.ResponseWriter, request *http.Request) {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	before := request.URL.Query().Get("before")
	server.writeThreadHistory(writer, request, limit, before, nil)
}

func (server *Server) handleThreadHistoryPost(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Limit      int            `json:"limit"`
		Before     string         `json:"before"`
		Metadata   map[string]any `json:"metadata"`
		Checkpoint *Checkpoint    `json:"checkpoint"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	server.writeThreadHistory(writer, request, payload.Limit, payload.Before, payload.Metadata)
}

func (server *Server) writeThreadHistory(writer http.ResponseWriter, request *http.Request, limit int, before string, metadata map[string]any) {
	states, err := server.threadHistory(request.Context(), request.PathValue("thread_id"), limit, before, metadata)
	if err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, 200, states)
}

func (server *Server) handleListRuns(writer http.ResponseWriter, request *http.Request) {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
	status := request.URL.Query().Get("status")
	server.mu.RLock()
	items := sortedRuns(server.runs, request.PathValue("thread_id"))
	server.mu.RUnlock()
	filtered := items[:0]
	for _, run := range items {
		if status == "" || run.Status == status {
			filtered = append(filtered, run)
		}
	}
	writeJSON(writer, 200, paginate(filtered, offset, limit))
}

func (server *Server) handleCreateRun(writer http.ResponseWriter, request *http.Request) {
	run, ok := server.createRun(writer, request)
	if ok {
		writer.Header().Set("Content-Location", "/threads/"+run.ThreadID+"/runs/"+run.RunID)
		writeJSON(writer, 200, run)
	}
}

func (server *Server) handleCreateRunStream(writer http.ResponseWriter, request *http.Request) {
	run, ok := server.createRun(writer, request)
	if ok {
		writer.Header().Set("Content-Location", "/threads/"+run.ThreadID+"/runs/"+run.RunID)
		server.streamRun(writer, request, run.RunID, 0)
	}
}

func (server *Server) handleCreateRunWait(writer http.ResponseWriter, request *http.Request) {
	run, ok := server.createRun(writer, request)
	if !ok {
		return
	}
	writer.Header().Set("Content-Location", "/threads/"+run.ThreadID+"/runs/"+run.RunID)
	if !server.waitRun(request.Context(), run.RunID) {
		return
	}
	server.mu.RLock()
	storedThread := server.threads[run.ThreadID]
	if storedThread == nil {
		server.mu.RUnlock()
		writeError(writer, http.StatusNotFound, "thread not found")
		return
	}
	thread := cloneJSON(*storedThread)
	server.mu.RUnlock()
	writeJSON(writer, 200, thread.Values)
}

func (server *Server) createRun(writer http.ResponseWriter, request *http.Request) (Run, bool) {
	var raw map[string]any
	if !decodeJSON(writer, request, &raw) {
		return Run{}, false
	}
	data, _ := json.Marshal(raw)
	var payload createRunRequest
	if err := json.Unmarshal(data, &payload); err != nil {
		writeError(writer, 400, err.Error())
		return Run{}, false
	}
	threadID := request.PathValue("thread_id")
	server.mu.Lock()
	assistant := server.resolveAssistantLocked(payload.AssistantID)
	if assistant == nil {
		server.mu.Unlock()
		writeError(writer, 404, "assistant not found")
		return Run{}, false
	}
	thread := server.threads[threadID]
	if thread == nil && payload.IfNotExists == "create" {
		now := server.timestamp()
		thread = &Thread{ThreadID: threadID, CreatedAt: now, UpdatedAt: now, Metadata: map[string]any{}, Status: "idle"}
		server.threads[threadID] = thread
	}
	if thread == nil {
		server.mu.Unlock()
		writeError(writer, 404, "thread not found")
		return Run{}, false
	}
	if thread.Metadata == nil {
		thread.Metadata = map[string]any{}
	}
	thread.Metadata["assistant_id"] = assistant.AssistantID
	strategy := payload.MultitaskStrategy
	if strategy == "" {
		strategy = "reject"
	}
	for _, existing := range server.runs {
		if existing.ThreadID != threadID || existing.Status != "pending" && existing.Status != "running" {
			continue
		}
		if strategy == "reject" {
			server.mu.Unlock()
			writeError(writer, 422, "thread is already running a task")
			return Run{}, false
		}
		if strategy == "interrupt" || strategy == "rollback" {
			if cancel := server.active[existing.RunID]; cancel != nil {
				cancel()
			} else {
				existing.Status = "interrupted"
				if log := server.events[existing.RunID]; log != nil {
					log.finish()
				}
			}
		}
	}
	runID, err := newUUID()
	if err != nil {
		server.mu.Unlock()
		writeError(writer, 500, err.Error())
		return Run{}, false
	}
	now := server.timestamp()
	run := &Run{RunID: runID, ThreadID: threadID, AssistantID: assistant.AssistantID, CreatedAt: now, UpdatedAt: now, Status: "pending", Metadata: nonNilMap(payload.Metadata), Kwargs: raw, MultitaskStrategy: strategy}
	server.runs[runID] = run
	server.events[runID] = newEventLog()
	err = server.persistLocked()
	copy := cloneJSON(*run)
	server.mu.Unlock()
	if err != nil {
		writeError(writer, 500, err.Error())
		return Run{}, false
	}
	if err := server.enqueueRun(runID); err != nil {
		server.mu.Lock()
		if current := server.runs[runID]; current != nil {
			server.failRunLocked(current, err)
		}
		server.mu.Unlock()
		writeError(writer, 503, err.Error())
		return Run{}, false
	}
	return copy, true
}

func (server *Server) handleGetRun(writer http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	run := server.runs[request.PathValue("run_id")]
	if run == nil || run.ThreadID != request.PathValue("thread_id") {
		server.mu.RUnlock()
		writeError(writer, 404, "run not found")
		return
	}
	copy := cloneJSON(*run)
	server.mu.RUnlock()
	writeJSON(writer, 200, copy)
}

func (server *Server) handleDeleteRun(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	id := request.PathValue("run_id")
	run := server.runs[id]
	if run == nil || run.ThreadID != request.PathValue("thread_id") {
		server.mu.Unlock()
		writeError(writer, 404, "run not found")
		return
	}
	if cancel := server.active[id]; cancel != nil {
		cancel()
	}
	if log := server.events[id]; log != nil {
		log.append("error", map[string]any{"error": "run deleted"})
		log.finish()
	}
	delete(server.runs, id)
	delete(server.events, id)
	err := server.persistLocked()
	server.mu.Unlock()
	if err != nil {
		writeError(writer, 500, err.Error())
		return
	}
	writer.WriteHeader(204)
}

func (server *Server) handleJoinRun(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("run_id")
	if !server.waitRun(request.Context(), id) {
		return
	}
	server.mu.RLock()
	run := server.runs[id]
	if run == nil || run.ThreadID != request.PathValue("thread_id") {
		server.mu.RUnlock()
		writeError(writer, 404, "run not found")
		return
	}
	thread := server.threads[run.ThreadID]
	if thread == nil {
		server.mu.RUnlock()
		writeError(writer, 404, "thread not found")
		return
	}
	values := cloneJSON(thread.Values)
	server.mu.RUnlock()
	writeJSON(writer, 200, values)
}

func (server *Server) handleRunStream(writer http.ResponseWriter, request *http.Request) {
	start := 0
	if value := request.Header.Get("Last-Event-ID"); value != "" {
		if id, err := strconv.Atoi(value); err == nil {
			start = id + 1
		}
	}
	runID := request.PathValue("run_id")
	server.mu.RLock()
	run := server.runs[runID]
	valid := run != nil && run.ThreadID == request.PathValue("thread_id")
	server.mu.RUnlock()
	if !valid {
		writeError(writer, http.StatusNotFound, "run not found")
		return
	}
	server.streamRun(writer, request, runID, start)
}

func (server *Server) handleCancelRun(writer http.ResponseWriter, request *http.Request) {
	err := server.cancelRun(request.PathValue("thread_id"), request.PathValue("run_id"))
	if err != nil {
		writeError(writer, 404, err.Error())
		return
	}
	if request.URL.Query().Get("wait") == "1" || request.URL.Query().Get("wait") == "true" {
		server.waitRun(request.Context(), request.PathValue("run_id"))
		writer.WriteHeader(204)
		return
	}
	writer.WriteHeader(202)
}

func (server *Server) streamRun(writer http.ResponseWriter, request *http.Request, runID string, start int) {
	server.mu.RLock()
	log := server.events[runID]
	server.mu.RUnlock()
	if log == nil {
		writeError(writer, 404, "run not found")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, 500, "streaming is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(200)
	flusher.Flush()
	index := start
	for {
		items, done, notify := log.snapshot(index)
		for _, event := range items {
			data, _ := json.Marshal(event.Data)
			_, _ = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Event, data)
			index++
			flusher.Flush()
		}
		if done {
			return
		}
		select {
		case <-request.Context().Done():
			return
		case <-notify:
		}
	}
}

func (server *Server) waitRun(ctx context.Context, runID string) bool {
	server.mu.RLock()
	log := server.events[runID]
	server.mu.RUnlock()
	if log == nil {
		return false
	}
	for {
		_, done, notify := log.snapshot(0)
		if done {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-notify:
		}
	}
}

func (server *Server) handleStoreGet(writer http.ResponseWriter, request *http.Request) {
	namespace := request.URL.Query()["namespace"]
	if len(namespace) == 1 {
		namespace = strings.Split(namespace[0], ".")
	}
	key := request.URL.Query().Get("key")
	item, err := server.store.Get(request.Context(), dastore.Namespace(namespace), key)
	if err != nil {
		writeError(writer, 400, err.Error())
		return
	}
	writeJSON(writer, 200, storeItem(item))
}

func (server *Server) handleStorePut(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Namespace []string       `json:"namespace"`
		Key       string         `json:"key"`
		Value     map[string]any `json:"value"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	if err := server.store.Put(request.Context(), payload.Namespace, payload.Key, payload.Value); err != nil {
		writeError(writer, 400, err.Error())
		return
	}
	writer.WriteHeader(204)
}

func (server *Server) handleStoreDelete(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Namespace []string `json:"namespace"`
		Key       string   `json:"key"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	if err := server.store.Delete(request.Context(), payload.Namespace, payload.Key); err != nil {
		writeError(writer, 400, err.Error())
		return
	}
	writer.WriteHeader(204)
}

func (server *Server) handleStoreSearch(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Prefix []string `json:"namespace_prefix"`
		Query  string   `json:"query"`
		Limit  int      `json:"limit"`
		Offset int      `json:"offset"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	items, err := server.store.Search(request.Context(), dastore.SearchOptions{Prefix: payload.Prefix, Query: payload.Query, Limit: payload.Limit, Offset: payload.Offset})
	if err != nil {
		writeError(writer, 400, err.Error())
		return
	}
	result := make([]any, len(items))
	for i := range items {
		item := items[i]
		result[i] = storeItem(&item)
	}
	writeJSON(writer, 200, map[string]any{"items": result})
}

func (server *Server) handleStoreNamespaces(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Prefix []string `json:"prefix"`
	}
	if !decodeJSON(writer, request, &payload) {
		return
	}
	values, err := server.store.ListNamespaces(request.Context(), payload.Prefix)
	if err != nil {
		writeError(writer, 400, err.Error())
		return
	}
	writeJSON(writer, 200, map[string]any{"namespaces": values})
}

var errNotFound = errors.New("not found")

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	defer request.Body.Close()
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		writeError(writer, 400, "invalid JSON: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		writeError(writer, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(writer).Encode(value)
	}
}
func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"detail": message})
}
func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}
func rawJSON(raw json.RawMessage) any { var value any; _ = json.Unmarshal(raw, &value); return value }
func setPagination(writer http.ResponseWriter, total, offset, limit int) {
	writer.Header().Set("X-Pagination-Total", strconv.Itoa(total))
	if limit > 0 && offset+limit < total {
		writer.Header().Set("X-Pagination-Next", strconv.Itoa(offset+limit))
	}
}
func storeItem(item *dastore.Item) any {
	if item == nil {
		return nil
	}
	return map[string]any{"namespace": item.Namespace, "key": item.Key, "value": item.Value, "created_at": item.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": item.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}
