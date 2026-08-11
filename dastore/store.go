// Package dastore defines namespaced durable memory used by graph and agent runs.
package dastore

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound         = errors.New("store item not found")
	ErrInvalidNamespace = errors.New("invalid store namespace")
)

// Namespace is a hierarchical, case-sensitive address.
type Namespace []string

func (namespace Namespace) Validate() error {
	if len(namespace) == 0 {
		return fmt.Errorf("%w: at least one segment is required", ErrInvalidNamespace)
	}
	for _, segment := range namespace {
		if segment == "" || strings.ContainsRune(segment, '\x00') {
			return fmt.Errorf("%w: segments must be non-empty and contain no NUL bytes", ErrInvalidNamespace)
		}
	}
	return nil
}

func namespaceKey(namespace Namespace) string { return strings.Join(namespace, "\x00") }

// Item is one versioned store value.
type Item struct {
	Namespace Namespace      `json:"namespace"`
	Key       string         `json:"key"`
	Value     map[string]any `json:"value"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// SearchOptions selects a namespace prefix and optional literal text query.
type SearchOptions struct {
	Prefix Namespace
	Query  string
	Limit  int
	Offset int
}

// Operation is one atomic batch operation. Exactly one of PutValue or Delete must
// be selected. Reads are represented by neither.
type Operation struct {
	Namespace Namespace
	Key       string
	PutValue  map[string]any
	Delete    bool
}

// Result is the value observed or written by a batch operation. Deleted and missing
// reads have Item == nil.
type Result struct{ Item *Item }

// Store is the minimal namespaced memory contract used by the runtime.
type Store interface {
	Get(context.Context, Namespace, string) (*Item, error)
	Put(context.Context, Namespace, string, map[string]any) error
	Delete(context.Context, Namespace, string) error
	Search(context.Context, SearchOptions) ([]Item, error)
	ListNamespaces(context.Context, Namespace) ([]Namespace, error)
	Batch(context.Context, []Operation) ([]Result, error)
}

// Memory is a concurrency-safe in-memory Store.
type Memory struct {
	mu    sync.RWMutex
	items map[string]map[string]Item
	now   func() time.Time
}

func NewMemory() *Memory {
	return &Memory{items: map[string]map[string]Item{}, now: time.Now}
}

func (memory *Memory) Get(ctx context.Context, namespace Namespace, key string) (*Item, error) {
	if err := validate(ctx, namespace, key); err != nil {
		return nil, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	item, ok := memory.items[namespaceKey(namespace)][key]
	if !ok {
		return nil, nil
	}
	result := cloneItem(item)
	return &result, nil
}

func (memory *Memory) Put(ctx context.Context, namespace Namespace, key string, value map[string]any) error {
	if err := validate(ctx, namespace, key); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	memory.putLocked(namespace, key, value)
	return nil
}

func (memory *Memory) putLocked(namespace Namespace, key string, value map[string]any) Item {
	address := namespaceKey(namespace)
	if memory.items[address] == nil {
		memory.items[address] = map[string]Item{}
	}
	now := memory.now().UTC()
	created := now
	if current, ok := memory.items[address][key]; ok {
		created = current.CreatedAt
	}
	item := Item{Namespace: cloneNamespace(namespace), Key: key, Value: cloneMap(value), CreatedAt: created, UpdatedAt: now}
	memory.items[address][key] = item
	return item
}

func (memory *Memory) Delete(ctx context.Context, namespace Namespace, key string) error {
	if err := validate(ctx, namespace, key); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	address := namespaceKey(namespace)
	delete(memory.items[address], key)
	if len(memory.items[address]) == 0 {
		delete(memory.items, address)
	}
	return nil
}

func (memory *Memory) Search(ctx context.Context, options SearchOptions) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(options.Prefix) > 0 {
		if err := options.Prefix.Validate(); err != nil {
			return nil, err
		}
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	var result []Item
	query := strings.ToLower(options.Query)
	for _, items := range memory.items {
		for _, item := range items {
			if !hasPrefix(item.Namespace, options.Prefix) || (query != "" && !matchesQuery(item, query)) {
				continue
			}
			result = append(result, cloneItem(item))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := namespaceKey(result[i].Namespace), namespaceKey(result[j].Namespace)
		if left != right {
			return left < right
		}
		return result[i].Key < result[j].Key
	})
	if options.Offset >= len(result) {
		return []Item{}, nil
	}
	if options.Offset > 0 {
		result = result[options.Offset:]
	}
	if options.Limit > 0 && len(result) > options.Limit {
		result = result[:options.Limit]
	}
	return result, nil
}

func (memory *Memory) ListNamespaces(ctx context.Context, prefix Namespace) ([]Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(prefix) > 0 {
		if err := prefix.Validate(); err != nil {
			return nil, err
		}
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	var result []Namespace
	for _, items := range memory.items {
		for _, item := range items {
			if hasPrefix(item.Namespace, prefix) {
				result = append(result, cloneNamespace(item.Namespace))
			}
			break
		}
	}
	sort.Slice(result, func(i, j int) bool { return namespaceKey(result[i]) < namespaceKey(result[j]) })
	return result, nil
}

func (memory *Memory) Batch(ctx context.Context, operations []Operation) ([]Result, error) {
	for _, operation := range operations {
		if err := validate(ctx, operation.Namespace, operation.Key); err != nil {
			return nil, err
		}
		if operation.Delete && operation.PutValue != nil {
			return nil, fmt.Errorf("store batch: put and delete are mutually exclusive")
		}
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	result := make([]Result, len(operations))
	for index, operation := range operations {
		address := namespaceKey(operation.Namespace)
		switch {
		case operation.Delete:
			delete(memory.items[address], operation.Key)
			if len(memory.items[address]) == 0 {
				delete(memory.items, address)
			}
		case operation.PutValue != nil:
			item := cloneItem(memory.putLocked(operation.Namespace, operation.Key, operation.PutValue))
			result[index].Item = &item
		default:
			if item, ok := memory.items[address][operation.Key]; ok {
				copy := cloneItem(item)
				result[index].Item = &copy
			}
		}
	}
	return result, nil
}

func validate(ctx context.Context, namespace Namespace, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := namespace.Validate(); err != nil {
		return err
	}
	if key == "" || strings.ContainsRune(key, '\x00') {
		return fmt.Errorf("store key must be non-empty and contain no NUL bytes")
	}
	return nil
}

func hasPrefix(value, prefix Namespace) bool {
	if len(prefix) > len(value) {
		return false
	}
	for index := range prefix {
		if value[index] != prefix[index] {
			return false
		}
	}
	return true
}

func matchesQuery(item Item, query string) bool {
	if strings.Contains(strings.ToLower(item.Key), query) {
		return true
	}
	return strings.Contains(strings.ToLower(fmt.Sprint(item.Value)), query)
}

func cloneNamespace(value Namespace) Namespace { return append(Namespace(nil), value...) }

func cloneMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func cloneItem(value Item) Item {
	value.Namespace = cloneNamespace(value.Namespace)
	value.Value = cloneMap(value.Value)
	return value
}
