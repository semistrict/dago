package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Model     string    `json:"model"`
	Backend   string    `json:"backend"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type conversationStore struct{ db *sql.DB }

func openConversationStore(databasePath string) (*conversationStore, error) {
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(`
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    backend TEXT NOT NULL DEFAULT 'local',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS conversations_updated_at_idx ON conversations(updated_at DESC);`); err != nil {
		database.Close()
		return nil, fmt.Errorf("setup conversation store: %w", err)
	}
	return &conversationStore{db: database}, nil
}

func (store *conversationStore) create(ctx context.Context, title, modelName, backendName string) (conversation, error) {
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return conversation{}, err
	}
	if strings.TrimSpace(title) == "" {
		title = "Untitled expedition"
	}
	now := time.Now().UTC()
	value := conversation{ID: hex.EncodeToString(idBytes), Title: cleanTitle(title), Model: modelName, Backend: backendName, CreatedAt: now, UpdatedAt: now}
	_, err := store.db.ExecContext(ctx, `INSERT INTO conversations (id, title, model, backend, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		value.ID, value.Title, value.Model, value.Backend, value.CreatedAt.Format(time.RFC3339Nano), value.UpdatedAt.Format(time.RFC3339Nano))
	return value, err
}

func (store *conversationStore) list(ctx context.Context, query string) ([]conversation, error) {
	statement := `SELECT id, title, model, backend, created_at, updated_at FROM conversations`
	var arguments []any
	if strings.TrimSpace(query) != "" {
		statement += ` WHERE lower(title) LIKE ?`
		arguments = append(arguments, "%"+strings.ToLower(strings.TrimSpace(query))+"%")
	}
	statement += ` ORDER BY updated_at DESC`
	rows, err := store.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []conversation{}
	for rows.Next() {
		var item conversation
		var created, updated string
		if err := rows.Scan(&item.ID, &item.Title, &item.Model, &item.Backend, &created, &updated); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (store *conversationStore) get(ctx context.Context, id string) (conversation, error) {
	var item conversation
	var created, updated string
	err := store.db.QueryRowContext(ctx, `SELECT id, title, model, backend, created_at, updated_at FROM conversations WHERE id = ?`, id).
		Scan(&item.ID, &item.Title, &item.Model, &item.Backend, &created, &updated)
	if err != nil {
		return conversation{}, err
	}
	item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return item, nil
}

func (store *conversationStore) touch(ctx context.Context, id, title, modelName, backendName string) error {
	if title != "" {
		_, err := store.db.ExecContext(ctx, `UPDATE conversations SET title = ?, model = ?, backend = ?, updated_at = ? WHERE id = ?`,
			cleanTitle(title), modelName, backendName, time.Now().UTC().Format(time.RFC3339Nano), id)
		return err
	}
	_, err := store.db.ExecContext(ctx, `UPDATE conversations SET model = ?, backend = ?, updated_at = ? WHERE id = ?`,
		modelName, backendName, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (store *conversationStore) delete(ctx context.Context, id string) error {
	_, err := store.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	return err
}

func cleanTitle(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 72 {
		value = string(runes[:72]) + "…"
	}
	return value
}
