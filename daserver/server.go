package daserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/dacheckpoint"
	"github.com/semistrict/dago/dastore"
)

// Server is an embeddable local Agent Server compatible with LangSmith Studio.
type Server struct {
	mu sync.RWMutex

	graphs      map[string]GraphRegistration
	assistants  map[string]*Assistant
	threads     map[string]*Thread
	runs        map[string]*Run
	events      map[string]*eventLog
	active      map[string]context.CancelFunc
	threadLocks map[string]*sync.Mutex

	saver     dacheckpoint.Saver
	store     dastore.Store
	context   any
	statePath string
	now       func() time.Time
	origins   map[string]bool

	queue   chan string
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	handler http.Handler
}

// New validates graph registrations, restores local metadata, and starts the
// in-process development run queue.
func New(graphs []GraphRegistration, options Options) (*Server, error) {
	if options.QueueWorkers < 0 {
		panic("Agent Server queue workers cannot be negative")
	}
	if len(graphs) == 0 {
		panic("Agent Server requires at least one graph")
	}
	compiledGraphs := make(map[string]GraphRegistration, len(graphs))
	for _, registration := range graphs {
		validateRegistration(&registration)
		if _, exists := compiledGraphs[registration.ID]; exists {
			panic(fmt.Sprintf("Agent Server graph ID %q is registered more than once", registration.ID))
		}
		compiledGraphs[registration.ID] = registration
	}
	if nilDependency(options.Saver) {
		options.Saver = dacheckpoint.NewMemorySaver()
	}
	if nilDependency(options.Store) {
		options.Store = dastore.NewMemory()
	}
	if options.QueueWorkers == 0 {
		options.QueueWorkers = 10
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		graphs: compiledGraphs, assistants: map[string]*Assistant{},
		threads: map[string]*Thread{}, runs: map[string]*Run{}, events: map[string]*eventLog{},
		active: map[string]context.CancelFunc{}, threadLocks: map[string]*sync.Mutex{},
		saver: options.Saver, store: options.Store, context: options.Deps,
		statePath: options.StatePath, now: options.Now, origins: map[string]bool{},
		queue: make(chan string, options.QueueWorkers*4), ctx: ctx, cancel: cancel,
	}
	for _, origin := range options.AllowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			server.origins[origin] = true
		}
	}
	if err := server.loadState(); err != nil {
		cancel()
		return nil, err
	}
	server.registerDefaultAssistants()
	if err := server.persistLocked(); err != nil {
		cancel()
		return nil, err
	}
	server.handler = server.routes()
	for worker := 0; worker < options.QueueWorkers; worker++ {
		server.wg.Add(1)
		go server.worker()
	}
	return server, nil
}

func validateRegistration(registration *GraphRegistration) {
	registration.ID = strings.TrimSpace(registration.ID)
	if registration.ID == "" || strings.ContainsAny(registration.ID, "\x00/\\") {
		panic(fmt.Sprintf("Agent Server graph ID %q is invalid", registration.ID))
	}
	if registration.Factory == nil {
		panic(fmt.Sprintf("Agent Server graph %q requires a factory", registration.ID))
	}
	if registration.Name == "" {
		registration.Name = registration.ID
	}
	if len(registration.InputSchema) == 0 {
		registration.InputSchema = defaultMessagesSchema()
	}
	if len(registration.OutputSchema) == 0 {
		registration.OutputSchema = defaultMessagesSchema()
	}
	if len(registration.StateSchema) == 0 {
		registration.StateSchema = defaultMessagesSchema()
	}
	if len(registration.ConfigSchema) == 0 {
		registration.ConfigSchema = defaultConfigSchema()
	}
	if len(registration.ContextSchema) == 0 {
		registration.ContextSchema = registration.ConfigSchema
	}
	for name, schema := range map[string]json.RawMessage{
		"input": registration.InputSchema, "output": registration.OutputSchema,
		"state": registration.StateSchema, "config": registration.ConfigSchema,
		"context": registration.ContextSchema,
	} {
		if !json.Valid(schema) {
			panic(fmt.Sprintf("Agent Server graph %q has invalid %s schema", registration.ID, name))
		}
	}
	if len(registration.Graph.Nodes) == 0 {
		registration.Graph = defaultDrawableGraph()
	}
}

func nilDependency(value any) bool {
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

func (server *Server) registerDefaultAssistants() {
	now := server.timestamp()
	for graphID, registration := range server.graphs {
		id := graphUUID(graphID)
		if existing := server.assistants[id]; existing != nil {
			existing.GraphID = graphID
			existing.Name = registration.Name
			existing.UpdatedAt = now
			continue
		}
		description := registration.Description
		server.assistants[id] = &Assistant{
			AssistantID: id, GraphID: graphID, Name: registration.Name,
			Description: &description, Config: map[string]any{}, Context: map[string]any{},
			Metadata: map[string]any{"created_by": "system"}, CreatedAt: now, UpdatedAt: now, Version: 1,
		}
	}
}

func (server *Server) timestamp() string {
	return server.now().UTC().Format(time.RFC3339Nano)
}

// Handler returns the concurrency-safe Agent Server HTTP handler.
func (server *Server) Handler() http.Handler { return server.handler }

// Close cancels active runs and waits for queue workers to stop.
func (server *Server) Close() error {
	if server == nil {
		return nil
	}
	server.cancel()
	server.mu.Lock()
	for _, cancel := range server.active {
		cancel()
	}
	server.mu.Unlock()
	server.wg.Wait()
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.persistLocked()
}

func (server *Server) graphForAssistant(ctx context.Context, assistant *Assistant) (Graph, error) {
	registration, exists := server.graphs[assistant.GraphID]
	if !exists {
		return nil, fmt.Errorf("graph %q is not registered", assistant.GraphID)
	}
	config := cloneJSON(assistant.Config)
	runtimeContext := server.context
	if assistant.Context != nil {
		runtimeContext = cloneJSON(assistant.Context)
	}
	graph, err := registration.Factory(ctx, Runtime{
		Saver: server.saver, Store: server.store, Config: config, Deps: runtimeContext,
	})
	if err != nil {
		return nil, err
	}
	if nilDependency(graph) {
		return nil, fmt.Errorf("graph factory returned nil")
	}
	return graph, nil
}

func (server *Server) threadLock(threadID string) *sync.Mutex {
	server.mu.Lock()
	defer server.mu.Unlock()
	lock := server.threadLocks[threadID]
	if lock == nil {
		lock = &sync.Mutex{}
		server.threadLocks[threadID] = lock
	}
	return lock
}

func (server *Server) worker() {
	defer server.wg.Done()
	for {
		select {
		case <-server.ctx.Done():
			return
		case runID := <-server.queue:
			server.executeRun(runID)
		}
	}
}

func (server *Server) cancelRun(threadID, runID string) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	run := server.runs[runID]
	if run == nil || run.ThreadID != threadID {
		return fmt.Errorf("run not found")
	}
	if cancel := server.active[runID]; cancel != nil {
		cancel()
		return nil
	}
	if run.Status == "pending" {
		run.Status = "interrupted"
		run.UpdatedAt = server.timestamp()
		if thread := server.threads[threadID]; thread != nil {
			thread.Status = "interrupted"
			thread.UpdatedAt = run.UpdatedAt
		}
		if log := server.events[runID]; log != nil {
			log.append("error", map[string]any{"error": "run canceled"})
			log.finish()
		}
		return server.persistLocked()
	}
	return nil
}

func (server *Server) enqueueRun(runID string) error {
	select {
	case server.queue <- runID:
		return nil
	case <-server.ctx.Done():
		return errors.New("Agent Server is closed")
	default:
		return errors.New("Agent Server run queue is full")
	}
}
