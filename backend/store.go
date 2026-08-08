package backend

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/semistrict/dago/store"
)

// Store persists virtual files in a namespaced runtime Store.
type Store struct {
	store     store.Store
	namespace store.Namespace
	mu        sync.Mutex
}

func NewStore(values store.Store, namespace store.Namespace) (*Store, error) {
	if values == nil {
		return nil, fmt.Errorf("backend store is required")
	}
	if err := namespace.Validate(); err != nil {
		return nil, err
	}
	return &Store{store: values, namespace: append(store.Namespace(nil), namespace...)}, nil
}

func (backend *Store) snapshot(ctx context.Context) (*Memory, error) {
	items, err := backend.store.Search(ctx, store.SearchOptions{Prefix: backend.namespace})
	if err != nil {
		return nil, err
	}
	files := map[string]FileData{}
	for _, item := range items {
		if len(item.Namespace) != len(backend.namespace) {
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
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	for name, data := range memory.files {
		if err := backend.store.Put(ctx, backend.namespace, name, fileDataMap(data)); err != nil {
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
	for name := range before {
		if err := backend.store.Delete(ctx, backend.namespace, name); err != nil {
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
	content, contentOK := value["content"].(string)
	encoding, encodingOK := value["encoding"].(string)
	if !contentOK || !encodingOK || (Encoding(encoding) != EncodingUTF8 && Encoding(encoding) != EncodingBase64) {
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
