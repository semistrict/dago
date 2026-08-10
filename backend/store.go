package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/store"
)

// Store persists virtual files in a namespaced runtime Store.
type Store struct {
	store            store.Store
	namespaceFactory NamespaceFactory
	session          storeSessionKey
	mu               sync.Mutex
}

// NamespaceFactory resolves the persistent namespace for one invocation. The
// runtime is nil when the backend is used directly outside an agent run.
type NamespaceFactory func(*Runtime) (store.Namespace, error)

type StoreOptions struct {
	// Store may be omitted when the agent runtime supplies one.
	Store     store.Store
	Namespace NamespaceFactory
}

type storeSessionKey struct{ owner *Store }
type storeSession struct {
	store     store.Store
	namespace store.Namespace
}

func NewStore(values store.Store, namespace store.Namespace) (*Store, error) {
	if values == nil {
		return nil, fmt.Errorf("backend store is required")
	}
	if err := namespace.Validate(); err != nil {
		return nil, err
	}
	fixed := append(store.Namespace(nil), namespace...)
	return NewStoreWithOptions(StoreOptions{Store: values, Namespace: func(*Runtime) (store.Namespace, error) {
		return append(store.Namespace(nil), fixed...), nil
	}})
}

// NewStoreWithOptions constructs a persistent backend whose namespace and,
// optionally, store are resolved from each agent invocation.
func NewStoreWithOptions(options StoreOptions) (*Store, error) {
	if options.Namespace == nil {
		return nil, fmt.Errorf("backend store namespace factory is required")
	}
	result := &Store{store: options.Store, namespaceFactory: options.Namespace}
	result.session.owner = result
	return result, nil
}

func (backend *Store) resolve(ctx context.Context) (store.Store, store.Namespace, error) {
	if session, ok := ctx.Value(backend.session).(*storeSession); ok && session != nil {
		return session.store, append(store.Namespace(nil), session.namespace...), nil
	}
	runtime := runtimeFromContext(ctx)
	values := backend.store
	if values == nil && runtime != nil {
		values = runtime.Store
	}
	if values == nil {
		return nil, nil, fmt.Errorf("store backend requires an explicit store or a bound agent runtime store")
	}
	namespace, err := backend.namespaceFactory(runtime)
	if err != nil {
		return nil, nil, err
	}
	if err := validateStoreNamespace(namespace); err != nil {
		return nil, nil, err
	}
	return values, append(store.Namespace(nil), namespace...), nil
}

func (backend *Store) BindRuntime(ctx context.Context, _ StateReader) (context.Context, error) {
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, backend.session, &storeSession{store: values, namespace: namespace}), nil
}

func (backend *Store) RuntimeUpdates(context.Context) map[string]any { return nil }
func (backend *Store) StateFields() []StateField                     { return nil }

func validateStoreNamespace(namespace store.Namespace) error {
	if err := namespace.Validate(); err != nil {
		return err
	}
	for index, component := range namespace {
		for _, character := range component {
			if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || strings.ContainsRune("-_.@+:~", character) {
				continue
			}
			return fmt.Errorf("invalid store namespace component %d %q: contains disallowed characters", index, component)
		}
	}
	return nil
}

func (backend *Store) snapshot(ctx context.Context) (*Memory, error) {
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		return nil, err
	}
	items, err := values.Search(ctx, store.SearchOptions{Prefix: namespace})
	if err != nil {
		return nil, err
	}
	files := map[string]FileData{}
	for _, item := range items {
		if len(item.Namespace) != len(namespace) {
			continue
		}
		data, err := fileDataFromMap(item.Value)
		if err != nil {
			return nil, fmt.Errorf("decode stored file %q: %w", item.Key, err)
		}
		files[item.Key] = data
	}
	return NewMemory(files)
}

func (backend *Store) persist(ctx context.Context, memory *Memory) error {
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		return err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	for name, data := range memory.files {
		if err := values.Put(ctx, namespace, name, fileDataMap(data)); err != nil {
			return err
		}
	}
	return nil
}

func (backend *Store) List(ctx context.Context, path string) (ListResult, error) {
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return ListResult{}, err
	}
	return memory.List(ctx, path)
}
func (backend *Store) Read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	return memory.Read(ctx, path, offset, limit)
}
func (backend *Store) Write(ctx context.Context, path, content string) (WriteResult, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	result, err := memory.Write(ctx, path, content)
	if err != nil {
		return WriteResult{}, err
	}
	return result, backend.persist(ctx, memory)
}
func (backend *Store) Edit(ctx context.Context, path, old, replacement string, all bool) (EditResult, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return EditResult{}, err
	}
	result, err := memory.Edit(ctx, path, old, replacement, all)
	if err != nil {
		return EditResult{}, err
	}
	return result, backend.persist(ctx, memory)
}
func (backend *Store) Delete(ctx context.Context, path string) (DeleteResult, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	before := map[string]struct{}{}
	memory.mu.RLock()
	for name := range memory.files {
		before[name] = struct{}{}
	}
	memory.mu.RUnlock()
	result, err := memory.Delete(ctx, path)
	if err != nil {
		return DeleteResult{}, err
	}
	memory.mu.RLock()
	for name := range memory.files {
		delete(before, name)
	}
	memory.mu.RUnlock()
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	operations := make([]store.Operation, 0, len(before))
	for name := range before {
		operations = append(operations, store.Operation{Namespace: namespace, Key: name, Delete: true})
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].Key < operations[j].Key })
	if len(operations) > 0 {
		if _, err := values.Batch(ctx, operations); err != nil {
			return DeleteResult{}, err
		}
	}
	return result, nil
}
func (backend *Store) Glob(ctx context.Context, pattern, path string) (GlobResult, error) {
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return GlobResult{}, err
	}
	return memory.Glob(ctx, pattern, path)
}
func (backend *Store) Grep(ctx context.Context, pattern string, options GrepOptions) (GrepResult, error) {
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return GrepResult{}, err
	}
	return memory.Grep(ctx, pattern, options)
}
func (backend *Store) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return failedUploads(uploads, err)
	}
	result := memory.Upload(ctx, uploads)
	if err := backend.persist(ctx, memory); err != nil {
		return failedUploads(uploads, err)
	}
	return result
}
func (backend *Store) Download(ctx context.Context, paths []string) []DownloadResult {
	memory, err := backend.snapshot(ctx)
	if err != nil {
		result := make([]DownloadResult, len(paths))
		for i, path := range paths {
			result[i] = DownloadResult{Path: path, Error: err.Error()}
		}
		return result
	}
	return memory.Download(ctx, paths)
}

func fileDataMap(data FileData) map[string]any {
	return map[string]any{"content": data.Content, "encoding": string(data.Encoding), "created_at": data.CreatedAt.Format(time.RFC3339Nano), "modified_at": data.ModifiedAt.Format(time.RFC3339Nano)}
}
func fileDataFromMap(value map[string]any) (FileData, error) {
	var content string
	switch raw := value["content"].(type) {
	case string:
		content = raw
	case []string:
		content = strings.Join(raw, "\n")
	case []any:
		lines := make([]string, len(raw))
		for index, item := range raw {
			line, ok := item.(string)
			if !ok {
				return FileData{}, fmt.Errorf("legacy content item %d has type %T", index, item)
			}
			lines[index] = line
		}
		content = strings.Join(lines, "\n")
	default:
		return FileData{}, fmt.Errorf("invalid content type %T", value["content"])
	}
	encoding := string(EncodingUTF8)
	if raw, exists := value["encoding"]; exists {
		var ok bool
		encoding, ok = raw.(string)
		if !ok {
			return FileData{}, fmt.Errorf("invalid encoding type %T", raw)
		}
	}
	if Encoding(encoding) != EncodingUTF8 && Encoding(encoding) != EncodingBase64 {
		return FileData{}, fmt.Errorf("invalid content or encoding")
	}
	result := FileData{Content: content, Encoding: Encoding(encoding)}
	if raw, ok := value["created_at"].(string); ok {
		result.CreatedAt, _ = time.Parse(time.RFC3339Nano, raw)
	}
	if raw, ok := value["modified_at"].(string); ok {
		result.ModifiedAt, _ = time.Parse(time.RFC3339Nano, raw)
	}
	return result, nil
}
func failedUploads(uploads []Upload, err error) []UploadResult {
	result := make([]UploadResult, len(uploads))
	for i, item := range uploads {
		result[i] = UploadResult{Path: item.Path, Error: err.Error()}
	}
	return result
}
