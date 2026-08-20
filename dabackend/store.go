package dabackend

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/dastore"
	"github.com/semistrict/dago/internal/optionvalue"
)

// Store persists virtual files in a namespaced runtime Store.
type Store struct {
	store            dastore.Store
	namespaceFactory NamespaceFactory
	session          storeSessionKey
	locks            *storePathLocks
}

type storePathLock struct {
	path string
	tree bool
}

type storePathLocks struct {
	mu      sync.Mutex
	next    uint64
	active  map[uint64][]storePathLock
	changed chan struct{}
}

func newStorePathLocks() *storePathLocks {
	return &storePathLocks{active: map[uint64][]storePathLock{}, changed: make(chan struct{})}
}

func (locks *storePathLocks) acquire(ctx context.Context, requested []storePathLock) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		locks.mu.Lock()
		if !locks.conflicts(requested) {
			locks.next++
			id := locks.next
			locks.active[id] = append([]storePathLock(nil), requested...)
			locks.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					locks.mu.Lock()
					delete(locks.active, id)
					close(locks.changed)
					locks.changed = make(chan struct{})
					locks.mu.Unlock()
				})
			}, nil
		}
		changed := locks.changed
		locks.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (locks *storePathLocks) conflicts(requested []storePathLock) bool {
	for _, held := range locks.active {
		for _, left := range requested {
			for _, right := range held {
				if storePathLocksConflict(left, right) {
					return true
				}
			}
		}
	}
	return false
}

func storePathLocksConflict(left, right storePathLock) bool {
	if left.path == right.path {
		return true
	}
	if left.tree && storePathWithin(right.path, left.path) {
		return true
	}
	return right.tree && storePathWithin(left.path, right.path)
}

func storePathWithin(candidate, root string) bool {
	return strings.HasPrefix(candidate, strings.TrimSuffix(root, "/")+"/")
}

// NamespaceFactory resolves the persistent namespace for one invocation. The
// runtime is nil when the backend is used directly outside an agent run.
type NamespaceFactory func(*Runtime) (dastore.Namespace, error)

// StoreOptions configures a persistent store backend.
type StoreOptions struct {
	// Store may be omitted when the agent runtime supplies one.
	Store dastore.Store
}

// FixedNamespace returns a namespace factory for a fixed namespace.
func FixedNamespace(namespace dastore.Namespace) NamespaceFactory {
	if err := namespace.Validate(); err != nil {
		panic(err)
	}
	fixed := append(dastore.Namespace(nil), namespace...)
	return func(*Runtime) (dastore.Namespace, error) {
		return append(dastore.Namespace(nil), fixed...), nil
	}
}

type storeSessionKey struct{ owner *Store }
type storeSession struct {
	store     dastore.Store
	namespace dastore.Namespace
}

// NewStore constructs a persistent backend whose namespace and, optionally,
// store are resolved from each agent invocation.
func NewStore(namespace NamespaceFactory, optionValues ...StoreOptions) *Store {
	if namespace == nil {
		panic("backend store namespace factory is required")
	}
	options := optionvalue.Resolve("backend store", optionValues)
	if nilInterface(options.Store) {
		options.Store = nil
	}
	result := &Store{store: options.Store, namespaceFactory: namespace, locks: newStorePathLocks()}
	result.session.owner = result
	return result
}

func (backend *Store) resolve(ctx context.Context) (dastore.Store, dastore.Namespace, error) {
	if session, ok := ctx.Value(backend.session).(*storeSession); ok && session != nil {
		return session.store, append(dastore.Namespace(nil), session.namespace...), nil
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
	return values, append(dastore.Namespace(nil), namespace...), nil
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

func validateStoreNamespace(namespace dastore.Namespace) error {
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
	items, err := searchAllStoreItems(ctx, values, namespace)
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
	return LoadMemory(files)
}

func searchAllStoreItems(ctx context.Context, values dastore.Store, namespace dastore.Namespace) ([]dastore.Item, error) {
	var items []dastore.Item
	for {
		page, err := values.Search(ctx, dastore.SearchOptions{
			Prefix: namespace,
			Limit:  dastore.DefaultSearchLimit,
			Offset: len(items),
		})
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
		if len(page) < dastore.DefaultSearchLimit {
			return items, nil
		}
	}
}

func (backend *Store) loadFile(ctx context.Context, name string) (dastore.Store, dastore.Namespace, FileData, bool, error) {
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		return nil, nil, FileData{}, false, err
	}
	item, err := values.Get(ctx, namespace, name)
	if err != nil {
		return nil, nil, FileData{}, false, err
	}
	if item == nil {
		return values, namespace, FileData{}, false, nil
	}
	data, err := fileDataFromMap(item.Value)
	if err != nil {
		return nil, nil, FileData{}, false, fmt.Errorf("decode stored file %q: %w", name, err)
	}
	return values, namespace, data, true, nil
}

func (backend *Store) List(ctx context.Context, path string) (ListResult, error) {
	memory, err := backend.snapshot(ctx)
	if err != nil {
		return ListResult{}, err
	}
	return memory.List(ctx, path)
}
func (backend *Store) Read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	name, err := normalizeVirtual(path)
	if err != nil {
		return ReadResult{}, err
	}
	_, _, data, ok, err := backend.loadFile(ctx, name)
	if err != nil {
		return ReadResult{}, err
	}
	if !ok {
		return ReadResult{}, fmt.Errorf("path %q: file not found", name)
	}
	if data.Encoding == EncodingBase64 || IsBinaryReadPath(name) {
		copy := data
		if copy.Encoding != EncodingBase64 {
			copy.Content = base64.StdEncoding.EncodeToString([]byte(copy.Content))
		}
		copy.Encoding = EncodingBase64
		return ReadResult{Data: &copy}, nil
	}
	return SliceRead(data, offset, limit)
}
func (backend *Store) Write(ctx context.Context, path, content string) (WriteResult, error) {
	name, err := normalizeVirtual(path)
	if err != nil {
		return WriteResult{}, err
	}
	release, err := backend.locks.acquire(ctx, []storePathLock{{path: name}})
	if err != nil {
		return WriteResult{}, err
	}
	defer release()
	values, namespace, previous, exists, err := backend.loadFile(ctx, name)
	if err != nil {
		return WriteResult{}, err
	}
	now := time.Now().UTC()
	created := now
	if exists {
		created = previous.CreatedAt
	}
	data := FileData{Content: content, Encoding: EncodingUTF8, CreatedAt: created, ModifiedAt: now}
	if err := values.Put(ctx, namespace, name, fileDataMap(data)); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Path: name}, nil
}
func (backend *Store) Edit(ctx context.Context, path, old, replacement string, all bool) (EditResult, error) {
	if old == "" || old == replacement {
		return EditResult{}, fmt.Errorf("edit: old string must be non-empty and differ from replacement")
	}
	name, err := normalizeVirtual(path)
	if err != nil {
		return EditResult{}, err
	}
	release, err := backend.locks.acquire(ctx, []storePathLock{{path: name}})
	if err != nil {
		return EditResult{}, err
	}
	defer release()
	values, namespace, data, exists, err := backend.loadFile(ctx, name)
	if err != nil {
		return EditResult{}, err
	}
	if !exists {
		return EditResult{}, fmt.Errorf("path %q: file not found", name)
	}
	if data.Encoding != EncodingUTF8 {
		return EditResult{}, fmt.Errorf("edit %q: binary files are unsupported", name)
	}
	updated, count, err := ReplaceText(name, data.Content, old, replacement, all)
	if err != nil {
		return EditResult{}, err
	}
	data.Content = updated
	data.ModifiedAt = time.Now().UTC()
	if err := values.Put(ctx, namespace, name, fileDataMap(data)); err != nil {
		return EditResult{}, err
	}
	return EditResult{Path: name, Occurrences: count}, nil
}
func (backend *Store) Delete(ctx context.Context, path string) (DeleteResult, error) {
	name, err := normalizeVirtual(path)
	if err != nil {
		return DeleteResult{}, err
	}
	if name == "/" {
		return DeleteResult{}, fmt.Errorf("delete virtual root is not allowed")
	}
	release, err := backend.locks.acquire(ctx, []storePathLock{{path: name, tree: true}})
	if err != nil {
		return DeleteResult{}, err
	}
	defer release()
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		return DeleteResult{}, err
	}
	items, err := searchAllStoreItems(ctx, values, namespace)
	if err != nil {
		return DeleteResult{}, err
	}
	prefix := strings.TrimSuffix(name, "/") + "/"
	var names []string
	for _, item := range items {
		if len(item.Namespace) == len(namespace) && (item.Key == name || strings.HasPrefix(item.Key, prefix)) {
			names = append(names, item.Key)
		}
	}
	if len(names) == 0 {
		return DeleteResult{}, fmt.Errorf("path %q: file not found", name)
	}
	sort.Strings(names)
	operations := make([]dastore.Operation, len(names))
	for index, candidate := range names {
		operations[index] = dastore.Operation{Namespace: namespace, Key: candidate, Delete: true}
	}
	if _, err := values.Batch(ctx, operations); err != nil {
		return DeleteResult{}, err
	}
	return DeleteResult{Path: name}, nil
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
	result := make([]UploadResult, len(uploads))
	names := make([]string, len(uploads))
	requested := make([]storePathLock, 0, len(uploads))
	seen := map[string]bool{}
	for index, upload := range uploads {
		result[index].Path = upload.Path
		name, err := normalizeVirtual(upload.Path)
		if err != nil {
			result[index].Error = err.Error()
			continue
		}
		names[index] = name
		if !seen[name] {
			seen[name] = true
			requested = append(requested, storePathLock{path: name})
		}
	}
	release, err := backend.locks.acquire(ctx, requested)
	if err != nil {
		for index := range result {
			if result[index].Error == "" {
				result[index].Error = err.Error()
			}
		}
		return result
	}
	defer release()
	values, namespace, err := backend.resolve(ctx)
	if err != nil {
		for index := range result {
			if result[index].Error == "" {
				result[index].Error = err.Error()
			}
		}
		return result
	}
	operations := make([]dastore.Operation, 0, len(uploads))
	operationIndexes := make([]int, 0, len(uploads))
	for index, upload := range uploads {
		if result[index].Error != "" {
			continue
		}
		name := names[index]
		itemErr := ctx.Err()
		var previous *dastore.Item
		if itemErr == nil {
			previous, itemErr = values.Get(ctx, namespace, name)
		}
		if itemErr != nil {
			result[index].Error = itemErr.Error()
			continue
		}
		now := time.Now().UTC()
		created := now
		if previous != nil {
			if data, decodeErr := fileDataFromMap(previous.Value); decodeErr != nil {
				result[index].Error = fmt.Sprintf("decode stored file %q: %v", name, decodeErr)
				continue
			} else {
				created = data.CreatedAt
			}
		}
		data := FileData{CreatedAt: created, ModifiedAt: now}
		if utf8.Valid(upload.Content) {
			data.Content, data.Encoding = string(upload.Content), EncodingUTF8
		} else {
			data.Content, data.Encoding = base64.StdEncoding.EncodeToString(upload.Content), EncodingBase64
		}
		operations = append(operations, dastore.Operation{Namespace: namespace, Key: name, PutValue: fileDataMap(data)})
		operationIndexes = append(operationIndexes, index)
	}
	if len(operations) > 0 {
		if _, err := values.Batch(ctx, operations); err != nil {
			for _, index := range operationIndexes {
				result[index].Error = err.Error()
			}
		}
	}
	return result
}
func (backend *Store) Download(ctx context.Context, paths []string) []DownloadResult {
	result := make([]DownloadResult, len(paths))
	for index, raw := range paths {
		result[index].Path = raw
		name, err := normalizeVirtual(raw)
		if err == nil {
			_, _, data, ok, loadErr := backend.loadFile(ctx, name)
			err = loadErr
			if err == nil && !ok {
				result[index].Error = "file_not_found"
				continue
			}
			if err == nil && data.Encoding == EncodingBase64 {
				result[index].Content, err = base64.StdEncoding.DecodeString(data.Content)
			} else if err == nil {
				result[index].Content = []byte(data.Content)
			}
		}
		if err != nil {
			result[index].Error = err.Error()
		}
	}
	return result
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
