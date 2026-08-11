package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/semistrict/dago/examples/shelley/db"
	"github.com/semistrict/dago/examples/shelley/dtach"
)

type databaseTestTB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
}

var (
	databaseTemplateOnce sync.Once
	databaseTemplateData []byte
	databaseTemplateErr  error
)

func newTestDatabase(tb databaseTestTB) (*db.DB, func()) {
	tb.Helper()
	databaseTemplateOnce.Do(func() { databaseTemplateData, databaseTemplateErr = buildDatabaseTemplate() })
	if databaseTemplateErr != nil {
		tb.Fatalf("Failed to prepare test database snapshot: %v", databaseTemplateErr)
	}
	dsn := filepath.Join(tb.TempDir(), "test.db")
	if err := os.WriteFile(dsn, databaseTemplateData, 0o600); err != nil {
		tb.Fatalf("Failed to write test database snapshot: %v", err)
	}
	database, err := db.New(db.Config{DSN: dsn})
	if err != nil {
		tb.Fatalf("Failed to create test database from snapshot: %v", err)
	}
	return database, func() { _ = database.Close() }
}

func buildDatabaseTemplate() (snapshot []byte, err error) {
	dir, err := os.MkdirTemp("", "shelley-db-template-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir template dir: %w", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "test.db")
	database, err := db.New(db.Config{DSN: path})
	if err != nil {
		return nil, fmt.Errorf("create template database: %w", err)
	}
	defer func() {
		if database != nil {
			if closeErr := database.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close template database: %w", closeErr)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate template database: %w", err)
	}
	if err := database.Pool().Exec(ctx, "PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return nil, fmt.Errorf("checkpoint template database: %w", err)
	}
	if err := database.Close(); err != nil {
		return nil, fmt.Errorf("close template database: %w", err)
	}
	database = nil
	return os.ReadFile(path)
}

func InProcessSpawner(socket, _ string, cwd, command string, cols, rows uint16, extraEnv []string) (int, error) {
	ready := make(chan struct{})
	var env []string
	if len(extraEnv) > 0 {
		env = append(os.Environ(), extraEnv...)
	}
	go func() {
		_ = dtach.Serve(dtach.ServerOptions{
			SocketPath: socket,
			Command:    "bash",
			Args:       []string{"--login", "-c", command},
			Dir:        cwd,
			Cols:       cols,
			Rows:       rows,
			Env:        env,
			Ready:      ready,
		})
	}()
	<-ready
	return os.Getpid(), nil
}

func RunNewConversationHook(input NewConversationHookInput) (NewConversationHookResult, error) {
	return RunNewConversationHookIn(defaultHooksDir(), input)
}
