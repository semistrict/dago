// Package dastore defines namespaced durable memory used by graph and agent runs.
package dastore

import (
	"container/heap"
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
	// Limit defaults to DefaultSearchLimit and cannot be negative.
	Limit int
	// Offset cannot be negative.
	Offset int
}

// DefaultSearchLimit bounds searches whose caller does not specify a limit.
const DefaultSearchLimit = 10

// Normalized returns a validated copy with the finite default limit applied.
func (options SearchOptions) Normalized() (SearchOptions, error) {
	if options.Limit < 0 {
		return SearchOptions{}, fmt.Errorf("store search limit cannot be negative")
	}
	if options.Offset < 0 {
		return SearchOptions{}, fmt.Errorf("store search offset cannot be negative")
	}
	if options.Limit == 0 {
		options.Limit = DefaultSearchLimit
	}
	return options, nil
}

// ListNamespacesOptions filters and paginates namespace listings.
type ListNamespacesOptions struct {
	Prefix Namespace
	Suffix Namespace
	// MaxDepth truncates namespaces to this depth before de-duplication. Zero
	// preserves their full depth and negative values are invalid.
	MaxDepth int
	// Limit defaults to DefaultListNamespacesLimit and cannot be negative.
	Limit int
	// Offset cannot be negative.
	Offset int
}

// DefaultListNamespacesLimit bounds namespace listings whose caller does not
// specify a limit.
const DefaultListNamespacesLimit = 100

// Normalized returns a validated copy with the finite default limit applied.
func (options ListNamespacesOptions) Normalized() (ListNamespacesOptions, error) {
	if options.MaxDepth < 0 {
		return ListNamespacesOptions{}, fmt.Errorf("store namespace maximum depth cannot be negative")
	}
	if options.Limit < 0 {
		return ListNamespacesOptions{}, fmt.Errorf("store namespace limit cannot be negative")
	}
	if options.Offset < 0 {
		return ListNamespacesOptions{}, fmt.Errorf("store namespace offset cannot be negative")
	}
	if options.Limit == 0 {
		options.Limit = DefaultListNamespacesLimit
	}
	return options, nil
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
	ListNamespaces(context.Context, ListNamespacesOptions) ([]Namespace, error)
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
	var err error
	options, err = options.Normalized()
	if err != nil {
		return nil, err
	}
	if len(options.Prefix) > 0 {
		if err := options.Prefix.Validate(); err != nil {
			return nil, err
		}
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	window := boundedWindow(options.Offset, options.Limit)
	selected := boundedItemSelection{limit: window}
	query := strings.ToLower(options.Query)
	for _, items := range memory.items {
		for _, item := range items {
			if !hasPrefix(item.Namespace, options.Prefix) || (query != "" && !matchesQuery(item, query)) {
				continue
			}
			selected.Add(item)
		}
	}
	items := selected.Sorted()
	if options.Offset >= len(items) {
		return []Item{}, nil
	}
	items = items[options.Offset:]
	result := make([]Item, len(items))
	for index, item := range items {
		result[index] = cloneItem(item)
	}
	return result, nil
}

func (memory *Memory) ListNamespaces(ctx context.Context, options ListNamespacesOptions) ([]Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	options, err = options.Normalized()
	if err != nil {
		return nil, err
	}
	for _, pattern := range []Namespace{options.Prefix, options.Suffix} {
		if len(pattern) > 0 {
			if err := pattern.Validate(); err != nil {
				return nil, err
			}
		}
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	window := boundedWindow(options.Offset, options.Limit)
	selected := boundedNamespaceSelection{limit: window}
	for _, items := range memory.items {
		for _, item := range items {
			if !namespaceMatches(item.Namespace, options) {
				break
			}
			candidate := item.Namespace
			if options.MaxDepth > 0 && len(candidate) > options.MaxDepth {
				candidate = candidate[:options.MaxDepth]
			}
			selected.Add(candidate)
			break
		}
	}
	namespaces := selected.Sorted()
	if options.Offset >= len(namespaces) {
		return []Namespace{}, nil
	}
	namespaces = namespaces[options.Offset:]
	result := make([]Namespace, len(namespaces))
	for index, candidate := range namespaces {
		result[index] = candidate.namespace
	}
	return result, nil
}

func boundedWindow(offset, limit int) int {
	maximum := int(^uint(0) >> 1)
	if offset > maximum-limit {
		return maximum
	}
	return offset + limit
}

func itemLess(left, right Item) bool {
	leftNamespace, rightNamespace := namespaceKey(left.Namespace), namespaceKey(right.Namespace)
	if leftNamespace != rightNamespace {
		return leftNamespace < rightNamespace
	}
	return left.Key < right.Key
}

type itemMaxHeap []Item

func (items itemMaxHeap) Len() int           { return len(items) }
func (items itemMaxHeap) Less(i, j int) bool { return itemLess(items[j], items[i]) }
func (items itemMaxHeap) Swap(i, j int)      { items[i], items[j] = items[j], items[i] }
func (items *itemMaxHeap) Push(value any)    { *items = append(*items, value.(Item)) }
func (items *itemMaxHeap) Pop() any {
	old := *items
	last := old[len(old)-1]
	*items = old[:len(old)-1]
	return last
}

type boundedItemSelection struct {
	limit int
	items itemMaxHeap
}

func (selection *boundedItemSelection) Add(item Item) {
	if len(selection.items) < selection.limit {
		heap.Push(&selection.items, item)
	} else if itemLess(item, selection.items[0]) {
		selection.items[0] = item
		heap.Fix(&selection.items, 0)
	}
}

func (selection *boundedItemSelection) Sorted() []Item {
	sort.Slice(selection.items, func(i, j int) bool {
		return itemLess(selection.items[i], selection.items[j])
	})
	return selection.items
}

type namespaceCandidate struct {
	namespace Namespace
	key       string
}

type namespaceMaxHeap []namespaceCandidate

func (namespaces namespaceMaxHeap) Len() int { return len(namespaces) }
func (namespaces namespaceMaxHeap) Less(i, j int) bool {
	return namespaces[i].key > namespaces[j].key
}
func (namespaces namespaceMaxHeap) Swap(i, j int) {
	namespaces[i], namespaces[j] = namespaces[j], namespaces[i]
}
func (namespaces *namespaceMaxHeap) Push(value any) {
	*namespaces = append(*namespaces, value.(namespaceCandidate))
}
func (namespaces *namespaceMaxHeap) Pop() any {
	old := *namespaces
	last := old[len(old)-1]
	*namespaces = old[:len(old)-1]
	return last
}

type boundedNamespaceSelection struct {
	limit    int
	items    namespaceMaxHeap
	retained map[string]struct{}
}

func (selection *boundedNamespaceSelection) Add(namespace Namespace) {
	key := namespaceKey(namespace)
	if _, exists := selection.retained[key]; exists {
		return
	}
	if selection.retained == nil {
		selection.retained = make(map[string]struct{})
	}
	candidate := namespaceCandidate{namespace: cloneNamespace(namespace), key: key}
	if len(selection.items) < selection.limit {
		heap.Push(&selection.items, candidate)
		selection.retained[key] = struct{}{}
	} else if key < selection.items[0].key {
		delete(selection.retained, selection.items[0].key)
		selection.items[0] = candidate
		heap.Fix(&selection.items, 0)
		selection.retained[key] = struct{}{}
	}
}

func (selection *boundedNamespaceSelection) Sorted() []namespaceCandidate {
	sort.Slice(selection.items, func(i, j int) bool {
		return selection.items[i].key < selection.items[j].key
	})
	return selection.items
}

func namespaceMatches(namespace Namespace, options ListNamespacesOptions) bool {
	return namespacePatternMatches(namespace, options.Prefix, false) &&
		namespacePatternMatches(namespace, options.Suffix, true)
}

func namespacePatternMatches(namespace, pattern Namespace, suffix bool) bool {
	if len(pattern) > len(namespace) {
		return false
	}
	start := 0
	if suffix {
		start = len(namespace) - len(pattern)
	}
	for index, segment := range pattern {
		if segment != "*" && segment != namespace[start+index] {
			return false
		}
	}
	return true
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
