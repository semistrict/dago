// Package sqlite provides a durable namespaced store with versioned migrations.
package sqlite

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/dastore"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS dago_store_migrations (
    v INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS dago_store_items (
    namespace TEXT NOT NULL,
    key TEXT NOT NULL,
    value BLOB NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (namespace, key)
);
CREATE INDEX IF NOT EXISTS dago_store_items_namespace_idx
ON dago_store_items(namespace);
INSERT OR IGNORE INTO dago_store_migrations(v) VALUES (0);`

type Store struct {
	db    *sql.DB
	owned bool
	once  sync.Once
	err   error
}

func Open(path string) (*Store, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	result := New(database)
	result.owned = true
	if err := result.Setup(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return result, nil
}

func New(database *sql.DB) *Store {
	if database == nil {
		panic("SQLite store database is required")
	}
	return &Store{db: database}
}

func (storage *Store) Setup(ctx context.Context) error {
	storage.once.Do(func() {
		if storage.db == nil {
			storage.err = fmt.Errorf("setup SQLite store: database is nil")
			return
		}
		_, storage.err = storage.db.ExecContext(ctx, schema)
		if storage.err != nil {
			storage.err = fmt.Errorf("setup SQLite store: %w", storage.err)
		}
	})
	return storage.err
}

func (storage *Store) Close() error {
	if storage.owned && storage.db != nil {
		return storage.db.Close()
	}
	return nil
}

func (storage *Store) Get(ctx context.Context, namespace dastore.Namespace, key string) (*dastore.Item, error) {
	encoded, err := validateAddress(ctx, namespace, key)
	if err != nil {
		return nil, err
	}
	if err := storage.Setup(ctx); err != nil {
		return nil, err
	}
	row := storage.db.QueryRowContext(ctx, `SELECT value, created_at, updated_at FROM dago_store_items WHERE namespace = ? AND key = ?`, encoded, key)
	item, err := scanItem(row, namespace, key)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

func (storage *Store) Put(ctx context.Context, namespace dastore.Namespace, key string, value map[string]any) error {
	encoded, err := validateAddress(ctx, namespace, key)
	if err != nil {
		return err
	}
	if err := storage.Setup(ctx); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode store value: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = storage.db.ExecContext(ctx, `
INSERT INTO dago_store_items(namespace, key, value, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, encoded, key, payload, now, now)
	return err
}

func (storage *Store) Delete(ctx context.Context, namespace dastore.Namespace, key string) error {
	encoded, err := validateAddress(ctx, namespace, key)
	if err != nil {
		return err
	}
	if err := storage.Setup(ctx); err != nil {
		return err
	}
	_, err = storage.db.ExecContext(ctx, `DELETE FROM dago_store_items WHERE namespace = ? AND key = ?`, encoded, key)
	return err
}

func (storage *Store) Search(ctx context.Context, options dastore.SearchOptions) ([]dastore.Item, error) {
	var err error
	options, err = options.Normalized()
	if err != nil {
		return nil, err
	}
	if err := validatePrefix(ctx, options.Prefix); err != nil {
		return nil, err
	}
	if err := storage.Setup(ctx); err != nil {
		return nil, err
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT namespace, key, value, created_at, updated_at FROM dago_store_items ORDER BY namespace, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	query := strings.ToLower(options.Query)
	result := make([]dastore.Item, 0, options.Limit)
	matched := 0
	for rows.Next() {
		item, err := scanStoredItem(rows)
		if err != nil {
			return nil, err
		}
		if !hasPrefix(item.Namespace, options.Prefix) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Key+" "+fmt.Sprint(item.Value)), query) {
			continue
		}
		if matched < options.Offset {
			matched++
			continue
		}
		result = append(result, item)
		if len(result) >= options.Limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (storage *Store) ListNamespaces(ctx context.Context, options dastore.ListNamespacesOptions) ([]dastore.Namespace, error) {
	var err error
	options, err = options.Normalized()
	if err != nil {
		return nil, err
	}
	if err := validatePrefix(ctx, options.Prefix); err != nil {
		return nil, err
	}
	if err := validatePrefix(ctx, options.Suffix); err != nil {
		return nil, err
	}
	if err := storage.Setup(ctx); err != nil {
		return nil, err
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT DISTINCT namespace FROM dago_store_items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	window := namespaceWindow(options.Offset, options.Limit)
	selected := make(namespaceMaxHeap, 0)
	retained := make(map[string]struct{})
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var namespace dastore.Namespace
		if err := json.Unmarshal([]byte(encoded), &namespace); err != nil {
			return nil, fmt.Errorf("decode store namespace: %w", err)
		}
		if !namespaceMatches(namespace, options) {
			continue
		}
		if options.MaxDepth > 0 && len(namespace) > options.MaxDepth {
			namespace = namespace[:options.MaxDepth]
		}
		key := strings.Join(namespace, "\x00")
		if _, exists := retained[key]; exists {
			continue
		}
		candidate := namespaceCandidate{namespace: append(dastore.Namespace(nil), namespace...), key: key}
		if len(selected) < window {
			heap.Push(&selected, candidate)
			retained[key] = struct{}{}
		} else if key < selected[0].key {
			delete(retained, selected[0].key)
			selected[0] = candidate
			heap.Fix(&selected, 0)
			retained[key] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].key < selected[j].key })
	if options.Offset >= len(selected) {
		return []dastore.Namespace{}, nil
	}
	selected = selected[options.Offset:]
	result := make([]dastore.Namespace, len(selected))
	for index, candidate := range selected {
		result[index] = candidate.namespace
	}
	return result, nil
}

func namespaceWindow(offset, limit int) int {
	maximum := int(^uint(0) >> 1)
	if offset > maximum-limit {
		return maximum
	}
	return offset + limit
}

type namespaceCandidate struct {
	namespace dastore.Namespace
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

func namespaceMatches(namespace dastore.Namespace, options dastore.ListNamespacesOptions) bool {
	return namespacePatternMatches(namespace, options.Prefix, false) &&
		namespacePatternMatches(namespace, options.Suffix, true)
}

func namespacePatternMatches(namespace, pattern dastore.Namespace, suffix bool) bool {
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

func (storage *Store) Batch(ctx context.Context, operations []dastore.Operation) ([]dastore.Result, error) {
	for _, operation := range operations {
		if _, err := validateAddress(ctx, operation.Namespace, operation.Key); err != nil {
			return nil, err
		}
		if operation.Delete && operation.PutValue != nil {
			return nil, fmt.Errorf("store batch: put and delete are mutually exclusive")
		}
	}
	if err := storage.Setup(ctx); err != nil {
		return nil, err
	}
	transaction, err := storage.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.Rollback()
	results := make([]dastore.Result, len(operations))
	for index, operation := range operations {
		encoded, _ := json.Marshal(operation.Namespace)
		switch {
		case operation.Delete:
			_, err = transaction.ExecContext(ctx, `DELETE FROM dago_store_items WHERE namespace = ? AND key = ?`, string(encoded), operation.Key)
		case operation.PutValue != nil:
			payload, marshalErr := json.Marshal(operation.PutValue)
			if marshalErr != nil {
				return nil, marshalErr
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_, err = transaction.ExecContext(ctx, `INSERT INTO dago_store_items(namespace, key, value, created_at, updated_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, string(encoded), operation.Key, payload, now, now)
			if err == nil {
				results[index].Item, err = scanItem(transaction.QueryRowContext(ctx, `SELECT value, created_at, updated_at FROM dago_store_items WHERE namespace = ? AND key = ?`, string(encoded), operation.Key), operation.Namespace, operation.Key)
			}
		default:
			results[index].Item, err = scanItem(transaction.QueryRowContext(ctx, `SELECT value, created_at, updated_at FROM dago_store_items WHERE namespace = ? AND key = ?`, string(encoded), operation.Key), operation.Namespace, operation.Key)
			if err == sql.ErrNoRows {
				err = nil
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

type scanner interface{ Scan(...any) error }

func scanItem(row scanner, namespace dastore.Namespace, key string) (*dastore.Item, error) {
	var payload []byte
	var created, updated string
	if err := row.Scan(&payload, &created, &updated); err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, fmt.Errorf("decode store value: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, err
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return nil, err
	}
	return &dastore.Item{Namespace: append(dastore.Namespace(nil), namespace...), Key: key, Value: value, CreatedAt: createdAt, UpdatedAt: updatedAt}, nil
}

func (storage *Store) all(ctx context.Context) ([]dastore.Item, error) {
	if err := storage.Setup(ctx); err != nil {
		return nil, err
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT namespace, key, value, created_at, updated_at FROM dago_store_items ORDER BY namespace, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []dastore.Item
	for rows.Next() {
		item, err := scanStoredItem(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func scanStoredItem(row scanner) (dastore.Item, error) {
	var encoded, key, created, updated string
	var payload []byte
	if err := row.Scan(&encoded, &key, &payload, &created, &updated); err != nil {
		return dastore.Item{}, err
	}
	var namespace dastore.Namespace
	var value map[string]any
	if err := json.Unmarshal([]byte(encoded), &namespace); err != nil {
		return dastore.Item{}, err
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return dastore.Item{}, err
	}
	createdAt, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return dastore.Item{}, err
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return dastore.Item{}, err
	}
	return dastore.Item{Namespace: namespace, Key: key, Value: value, CreatedAt: createdAt, UpdatedAt: updatedAt}, nil
}

func validateAddress(ctx context.Context, namespace dastore.Namespace, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := namespace.Validate(); err != nil {
		return "", err
	}
	if key == "" || strings.ContainsRune(key, '\x00') {
		return "", fmt.Errorf("store key must be non-empty and contain no NUL bytes")
	}
	encoded, err := json.Marshal(namespace)
	return string(encoded), err
}

func validatePrefix(ctx context.Context, prefix dastore.Namespace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(prefix) > 0 {
		return prefix.Validate()
	}
	return nil
}

func hasPrefix(value, prefix dastore.Namespace) bool {
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
