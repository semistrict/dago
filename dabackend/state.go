package dabackend

import (
	"context"
	"fmt"
	"sync"

	"github.com/semistrict/dago/dastore"
)

// StateReader is the minimal graph-state view required by a runtime-bound
// backend. It intentionally matches the state view exposed to tools.
type StateReader interface {
	Get(string) (any, bool)
}

// StateField describes a delta-backed virtual filesystem field. Consumers use
// these declarations to register the field with their graph runtime.
type StateField struct {
	Key               string
	Contract          string
	SnapshotFrequency uint64
	Initial           func() any
	Reduce            func(any, []any) (any, error)
	Clone             func(any) any
}

// Runtime contains the invocation values a backend may use to select durable
// resources. Deps is application-defined and is the usual source for a
// per-user or per-assistant namespace.
type Runtime struct {
	Deps         any
	ThreadID     string
	Namespace    string
	CheckpointID string
	TaskID       string
	Store        dastore.Store
}

// DepsAs returns typed application dependencies.
func DepsAs[T any](runtime *Runtime) (T, bool) {
	var zero T
	if runtime == nil {
		return zero, false
	}
	typed, ok := runtime.Deps.(T)
	return typed, ok
}

type backendRuntimeKey struct{}

func runtimeFromContext(ctx context.Context) *Runtime {
	runtime, _ := ctx.Value(backendRuntimeKey{}).(*Runtime)
	return runtime
}

// RuntimeBackend is a backend whose storage is supplied by the current graph
// state. BindRuntime creates an invocation-local view; RuntimeUpdates returns
// the partial field writes produced through that view.
type RuntimeBackend interface {
	Backend
	BindRuntime(context.Context, StateReader) (context.Context, error)
	RuntimeUpdates(context.Context) map[string]any
	StateFields() []StateField
}

type stateSessionKey struct{ owner *State }

type stateSession struct {
	mu      sync.Mutex
	memory  *Memory
	updates map[string]any
}

// State stores files in a required delta channel. It can only be used while
// bound to graph state by filesystem middleware; direct calls fail clearly.
type State struct {
	key     string
	initial map[string]any
	session stateSessionKey
}

// NewState constructs a thread-scoped state backend. An empty key selects the
// standard "files" field. Initial values use the same plain-data records that
// are persisted in checkpoints.
func NewState(key string, initial map[string]any) *State {
	if key == "" {
		key = "files"
	}
	value := &State{key: key}
	value.session.owner = value
	cloned, err := cloneStateFiles(initial)
	if err != nil {
		panic(fmt.Errorf("state backend initial files: %w", err))
	}
	value.initial = cloned
	return value
}

func (backend *State) BindRuntime(ctx context.Context, reader StateReader) (context.Context, error) {
	files := backend.initial
	if reader != nil {
		if value, exists := reader.Get(backend.key); exists {
			var err error
			files, err = cloneStateFilesValue(value)
			if err != nil {
				return nil, fmt.Errorf("state backend field %q: %w", backend.key, err)
			}
		}
	}
	decoded, err := decodeStateFiles(files)
	if err != nil {
		return nil, err
	}
	memory, err := restoreMemory(decoded)
	if err != nil {
		return nil, err
	}
	session := &stateSession{memory: memory, updates: map[string]any{}}
	return context.WithValue(ctx, backend.session, session), nil
}

func (backend *State) RuntimeUpdates(ctx context.Context) map[string]any {
	session, err := backend.bound(ctx)
	if err != nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.updates) == 0 {
		return nil
	}
	return map[string]any{backend.key: cloneStatePatch(session.updates)}
}

func (backend *State) StateFields() []StateField {
	return []StateField{{
		Key: backend.key, Contract: "dago.files.delta.v1", SnapshotFrequency: 50,
		Initial: func() any { return cloneStatePatch(backend.initial) },
		Reduce:  reduceStateFiles, Clone: func(value any) any {
			cloned, err := cloneStateFilesValue(value)
			if err != nil {
				return value
			}
			return cloned
		},
	}}
}

func (backend *State) bound(ctx context.Context) (*stateSession, error) {
	session, ok := ctx.Value(backend.session).(*stateSession)
	if !ok || session == nil {
		return nil, fmt.Errorf("state backend must be used inside a bound graph execution")
	}
	return session, nil
}

func (backend *State) List(ctx context.Context, path string) (ListResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return ListResult{}, err
	}
	return session.memory.List(ctx, path)
}

func (backend *State) Read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	return session.memory.Read(ctx, path, offset, limit)
}

func (backend *State) Write(ctx context.Context, path, content string) (WriteResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	result, err := session.memory.Write(ctx, path, content)
	if err == nil {
		backend.recordFiles(session, []string{result.Path})
	}
	return result, err
}

func (backend *State) Edit(ctx context.Context, path, old, replacement string, all bool) (EditResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return EditResult{}, err
	}
	result, err := session.memory.Edit(ctx, path, old, replacement, all)
	if err == nil {
		backend.recordFiles(session, []string{result.Path})
	}
	return result, err
}

func (backend *State) Delete(ctx context.Context, path string) (DeleteResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	session.memory.mu.RLock()
	before := make([]string, 0, len(session.memory.files))
	for name := range session.memory.files {
		before = append(before, name)
	}
	session.memory.mu.RUnlock()
	result, err := session.memory.Delete(ctx, path)
	if err != nil {
		return result, err
	}
	session.memory.mu.RLock()
	session.mu.Lock()
	for _, name := range before {
		if _, exists := session.memory.files[name]; !exists {
			session.updates[name] = nil
		}
	}
	session.mu.Unlock()
	session.memory.mu.RUnlock()
	return result, nil
}

func (backend *State) Glob(ctx context.Context, pattern, path string) (GlobResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return GlobResult{}, err
	}
	return session.memory.Glob(ctx, pattern, path)
}

func (backend *State) Grep(ctx context.Context, pattern string, options GrepOptions) (GrepResult, error) {
	session, err := backend.bound(ctx)
	if err != nil {
		return GrepResult{}, err
	}
	return session.memory.Grep(ctx, pattern, options)
}

func (backend *State) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	session, err := backend.bound(ctx)
	if err != nil {
		return failedUploads(uploads, err)
	}
	result := session.memory.Upload(ctx, uploads)
	paths := make([]string, 0, len(result))
	for _, item := range result {
		if item.Error == "" {
			paths = append(paths, item.Path)
		}
	}
	backend.recordFiles(session, paths)
	return result
}

func (backend *State) Download(ctx context.Context, paths []string) []DownloadResult {
	session, err := backend.bound(ctx)
	if err != nil {
		result := make([]DownloadResult, len(paths))
		for index, path := range paths {
			result[index] = DownloadResult{Path: path, Error: err.Error()}
		}
		return result
	}
	return session.memory.Download(ctx, paths)
}

func (backend *State) recordFiles(session *stateSession, paths []string) {
	session.memory.mu.RLock()
	defer session.memory.mu.RUnlock()
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, name := range paths {
		if data, exists := session.memory.files[name]; exists {
			session.updates[name] = fileDataMap(data)
		}
	}
}

func reduceStateFiles(current any, writes []any) (any, error) {
	result, err := cloneStateFilesValue(current)
	if err != nil {
		return nil, err
	}
	for _, write := range writes {
		patch, ok := write.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("state files update has type %T", write)
		}
		for path, value := range patch {
			if value == nil {
				delete(result, path)
				continue
			}
			data, err := fileDataFromValue(value)
			if err != nil {
				return nil, fmt.Errorf("state file %q: %w", path, err)
			}
			result[path] = fileDataMap(data)
		}
	}
	return result, nil
}

func decodeStateFiles(values map[string]any) (map[string]FileData, error) {
	result := make(map[string]FileData, len(values))
	for path, value := range values {
		data, err := fileDataFromValue(value)
		if err != nil {
			return nil, fmt.Errorf("state file %q: %w", path, err)
		}
		result[path] = data
	}
	return result, nil
}

func fileDataFromValue(value any) (FileData, error) {
	switch typed := value.(type) {
	case FileData:
		return typed, nil
	case map[string]any:
		return fileDataFromMap(typed)
	default:
		return FileData{}, fmt.Errorf("file data has type %T", value)
	}
}

func cloneStateFilesValue(value any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	switch typed := value.(type) {
	case map[string]any:
		return cloneStateFiles(typed)
	case map[string]FileData:
		result := make(map[string]any, len(typed))
		for path, data := range typed {
			result[path] = fileDataMap(data)
		}
		return result, nil
	default:
		return nil, fmt.Errorf("files value has type %T", value)
	}
}

func cloneStateFiles(values map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(values))
	for path, value := range values {
		if value == nil {
			result[path] = nil
			continue
		}
		data, err := fileDataFromValue(value)
		if err != nil {
			return nil, fmt.Errorf("state file %q: %w", path, err)
		}
		result[path] = fileDataMap(data)
	}
	return result, nil
}

func cloneStatePatch(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		if nested, ok := value.(map[string]any); ok {
			copy := make(map[string]any, len(nested))
			for nestedKey, nestedValue := range nested {
				copy[nestedKey] = nestedValue
			}
			result[key] = copy
		} else {
			result[key] = value
		}
	}
	return result
}

// BindRuntime binds every runtime-backed component reachable through value.
func BindRuntime(ctx context.Context, value Backend, reader StateReader, runtime ...Runtime) (context.Context, error) {
	if len(runtime) > 0 {
		copy := runtime[0]
		ctx = context.WithValue(ctx, backendRuntimeKey{}, &copy)
	}
	if bound, ok := value.(RuntimeBackend); ok {
		return bound.BindRuntime(ctx, reader)
	}
	return ctx, nil
}

// RuntimeUpdates collects state writes emitted by a runtime-backed backend.
func RuntimeUpdates(ctx context.Context, value Backend) map[string]any {
	if bound, ok := value.(RuntimeBackend); ok {
		return bound.RuntimeUpdates(ctx)
	}
	return nil
}

// RuntimeStateFields returns all graph fields required by a backend.
func RuntimeStateFields(value Backend) []StateField {
	if bound, ok := value.(RuntimeBackend); ok {
		return bound.StateFields()
	}
	return nil
}
