package dahook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type trustStore struct {
	Version  int                   `json:"version"`
	Projects map[string]trustEntry `json:"projects"`
}
type trustEntry struct {
	TrustedAt string `json:"trusted_at"`
}

// IsTrusted checks the canonical project key in a versioned trust store.
func IsTrusted(ctx context.Context, storePath, projectRoot string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	storePath, err := canonicalFilesystemPath(storePath)
	if err != nil {
		return false, err
	}
	key, err := canonicalProject(projectRoot)
	if err != nil {
		return false, err
	}
	if err := rejectLinkedAncestors(filepath.Dir(storePath)); err != nil {
		return false, fmt.Errorf("validate hook trust store path: %w", err)
	}
	raw, err := readBounded(storePath, maxConfigBytes)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read hook trust store: %w", err)
	}
	var store trustStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return false, fmt.Errorf("parse hook trust store: %w", err)
	}
	if store.Version != 1 {
		return false, fmt.Errorf("unsupported hook trust store version %d", store.Version)
	}
	_, ok := store.Projects[filepath.Clean(key)]
	return ok, nil
}

// TrustProject atomically persists a canonical workspace with owner-only modes.
func TrustProject(ctx context.Context, storePath, projectRoot string) error {
	if storePath == "" || projectRoot == "" {
		panic("dahook: trust store and project root are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	storePath, err = canonicalFilesystemPath(storePath)
	if err != nil {
		return err
	}
	key, err := canonicalProject(projectRoot)
	if err != nil {
		return err
	}
	if err := rejectLinkedAncestors(filepath.Dir(storePath)); err != nil {
		return fmt.Errorf("validate hook trust store path: %w", err)
	}
	release, err := acquireTrustLock(ctx, storePath+".lock")
	if err != nil {
		return err
	}
	defer release()
	store := trustStore{Version: 1, Projects: map[string]trustEntry{}}
	if raw, readErr := readBounded(storePath, maxConfigBytes); readErr == nil {
		if err := json.Unmarshal(raw, &store); err != nil {
			return fmt.Errorf("parse hook trust store: %w", err)
		}
		if store.Version != 1 || store.Projects == nil {
			return fmt.Errorf("invalid hook trust store")
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("read hook trust store: %w", readErr)
	}
	store.Projects[filepath.Clean(key)] = trustEntry{TrustedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, err := json.Marshal(store)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(storePath), ".hooks-trust-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if runtime.GOOS != "windows" {
		if err := temporary.Chmod(0o600); err != nil {
			temporary.Close()
			return err
		}
	}
	if _, err := temporary.Write(append(raw, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(name, storePath)
}
