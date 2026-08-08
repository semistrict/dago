// Package sqlite provides a durable namespaced store with versioned migrations.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/store"
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

func New(database *sql.DB) *Store { return &Store{db: database} }

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

func (storage *Store) Get(ctx context.Context, namespace store.Namespace, key string) (*store.Item, error) {
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

func (storage *Store) Put(ctx context.Context, namespace store.Namespace, key string, value map[string]any) error {
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

func (storage *Store) Delete(ctx context.Context, namespace store.Namespace, key string) error {
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

func (storage *Store) Search(ctx context.Context, options store.SearchOptions) ([]store.Item, error) {
	if err := validatePrefix(ctx, options.Prefix); err != nil {
		return nil, err
	}
	items, err := storage.all(ctx)
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(options.Query)
	result := make([]store.Item, 0)
	for _, item := range items {
		if !hasPrefix(item.Namespace, options.Prefix) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(item.Key+" "+fmt.Sprint(item.Value)), query) {
			continue
		}
		result = append(result, item)
	}
	if options.Offset >= len(result) {
		return []store.Item{}, nil
	}
	if options.Offset > 0 {
		result = result[options.Offset:]
	}
	if options.Limit > 0 && len(result) > options.Limit {
		result = result[:options.Limit]
	}
	return result, nil
}

func (storage *Store) ListNamespaces(ctx context.Context, prefix store.Namespace) ([]store.Namespace, error) {
	if err := validatePrefix(ctx, prefix); err != nil {
		return nil, err
	}
	items, err := storage.all(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]store.Namespace{}
	for _, item := range items {
		if hasPrefix(item.Namespace, prefix) {
			encoded, _ := json.Marshal(item.Namespace)
			seen[string(encoded)] = append(store.Namespace(nil), item.Namespace...)
		}
	}
	result := make([]store.Namespace, 0, len(seen))
	for _, namespace := range seen {
		result = append(result, namespace)
	}
	sort.Slice(result, func(i, j int) bool { return strings.Join(result[i], "\x00") < strings.Join(result[j], "\x00") })
	return result, nil
}

func (storage *Store) Batch(ctx context.Context, operations []store.Operation) ([]store.Result, error) {
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
	results := make([]store.Result, len(operations))
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

func scanItem(row scanner, namespace store.Namespace, key string) (*store.Item, error) {
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
	return &store.Item{Namespace: append(store.Namespace(nil), namespace...), Key: key, Value: value, CreatedAt: createdAt, UpdatedAt: updatedAt}, nil
}

func (storage *Store) all(ctx context.Context) ([]store.Item, error) {
	if err := storage.Setup(ctx); err != nil {
		return nil, err
	}
	rows, err := storage.db.QueryContext(ctx, `SELECT namespace, key, value, created_at, updated_at FROM dago_store_items ORDER BY namespace, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []store.Item
	for rows.Next() {
		var encoded, key, created, updated string
		var payload []byte
		if err := rows.Scan(&encoded, &key, &payload, &created, &updated); err != nil {
			return nil, err
		}
		var namespace store.Namespace
		var value map[string]any
		if err := json.Unmarshal([]byte(encoded), &namespace); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, err
		}
		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		result = append(result, store.Item{Namespace: namespace, Key: key, Value: value, CreatedAt: createdAt, UpdatedAt: updatedAt})
	}
	return result, rows.Err()
}

func validateAddress(ctx context.Context, namespace store.Namespace, key string) (string, error) {
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

func validatePrefix(ctx context.Context, prefix store.Namespace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(prefix) > 0 {
		return prefix.Validate()
	}
	return nil
}

func hasPrefix(value, prefix store.Namespace) bool {
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
