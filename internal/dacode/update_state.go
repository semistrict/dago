package dacode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	updateStateVersion       = 1
	maxUpdateStateBytes      = 16 << 10
	maxUpdateStateValueBytes = 256
)

var updateStateWriteMu sync.Mutex

// updatePersistentState is the complete non-secret lifecycle record for
// update prompting and automatic-install loop prevention. Version and useful
// zero defaults are supplied by newUpdatePersistentState and Load.
type updatePersistentState struct {
	Version                     int       `json:"version"`
	AutoUpdateConsent           bool      `json:"auto_update_consent,omitempty"`
	ImplicitDefaultAcknowledged bool      `json:"implicit_default_acknowledged,omitempty"`
	NotifiedVersion             string    `json:"notified_version,omitempty"`
	NotifiedAt                  time.Time `json:"notified_at,omitempty"`
	SkipOnceVersion             string    `json:"skip_once_version,omitempty"`
	SkipVersion                 string    `json:"skip_version,omitempty"`
	CooldownVersion             string    `json:"cooldown_version,omitempty"`
	CooldownUntil               time.Time `json:"cooldown_until,omitempty"`
	RestartVersion              string    `json:"restart_version,omitempty"`
	RestartAttempts             int       `json:"restart_attempts,omitempty"`
	RestartedAt                 time.Time `json:"restarted_at,omitempty"`
}

func newUpdatePersistentState() updatePersistentState {
	return updatePersistentState{Version: updateStateVersion}
}

type updateStateStore struct{ path string }

// newUpdateStateStore constructs an owner-private state store without I/O.
func newUpdateStateStore(path string) *updateStateStore {
	if path == "" || strings.TrimSpace(path) != path || len(path) > 4096 || strings.ContainsRune(path, 0) {
		panic("dacode: bounded update state path is required")
	}
	return &updateStateStore{path: filepath.Clean(path)}
}

// Load returns a useful empty state for a missing file and rejects malformed,
// oversized, non-private, replaced, or symlink-backed records.
func (store *updateStateStore) Load(ctx context.Context) (updatePersistentState, error) {
	if ctx == nil {
		panic("dacode: update state context is required")
	}
	if store == nil || store.path == "" {
		return updatePersistentState{}, errors.New("update state store is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return updatePersistentState{}, err
	}
	return loadUpdatePersistentState(ctx, store.path)
}

// Update serializes a read-modify-write transaction and commits only a valid,
// bounded state. Mutator errors and cancellation leave the old file intact.
func (store *updateStateStore) Update(ctx context.Context, mutate func(*updatePersistentState) error) (updatePersistentState, error) {
	if ctx == nil || mutate == nil {
		panic("dacode: update state transaction dependencies are required")
	}
	if store == nil || store.path == "" {
		return updatePersistentState{}, errors.New("update state store is unavailable")
	}
	updateStateWriteMu.Lock()
	defer updateStateWriteMu.Unlock()
	state, err := loadUpdatePersistentState(ctx, store.path)
	if err != nil {
		return updatePersistentState{}, err
	}
	if err := mutate(&state); err != nil {
		return updatePersistentState{}, err
	}
	state.Version = updateStateVersion
	normalizeUpdateStateTimes(&state)
	if !validUpdatePersistentState(state) {
		return updatePersistentState{}, errors.New("update state is invalid")
	}
	if err := ctx.Err(); err != nil {
		return updatePersistentState{}, err
	}
	if err := saveUpdatePersistentState(ctx, store.path, state); err != nil {
		return updatePersistentState{}, err
	}
	return state, nil
}

func loadUpdatePersistentState(ctx context.Context, path string) (updatePersistentState, error) {
	state := newUpdatePersistentState()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return updatePersistentState{}, errors.New("inspect update state")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > maxUpdateStateBytes {
		return updatePersistentState{}, errors.New("update state is not an owner-private bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return updatePersistentState{}, errors.New("open update state")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 || opened.Size() <= 0 || opened.Size() > maxUpdateStateBytes || !os.SameFile(info, opened) {
		return updatePersistentState{}, errors.New("update state changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxUpdateStateBytes+1))
	if err != nil || len(payload) == 0 || len(payload) > maxUpdateStateBytes {
		return updatePersistentState{}, errors.New("read update state")
	}
	if err := ctx.Err(); err != nil {
		return updatePersistentState{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return updatePersistentState{}, errors.New("decode update state")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return updatePersistentState{}, errors.New("decode update state")
	}
	if !validUpdatePersistentState(state) {
		return updatePersistentState{}, errors.New("update state is invalid")
	}
	normalizeUpdateStateTimes(&state)
	return state, nil
}

func saveUpdatePersistentState(ctx context.Context, path string, state updatePersistentState) (err error) {
	if !validUpdatePersistentState(state) {
		return errors.New("update state is invalid")
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return errors.New("encode update state")
	}
	payload = append(payload, '\n')
	if len(payload) > maxUpdateStateBytes {
		return errors.New("update state exceeds its bound")
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateUpdateStateDirectory(directory); err != nil {
		return err
	}
	if info, inspectErr := os.Lstat(path); inspectErr == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("update state target is unsafe")
		}
	} else if !errors.Is(inspectErr, os.ErrNotExist) {
		return errors.New("inspect update state target")
	}
	temporary, err := os.CreateTemp(directory, ".update-state-*.tmp")
	if err != nil {
		return errors.New("create update state")
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("secure update state")
	}
	if _, err := temporary.Write(payload); err != nil {
		return errors.New("write update state")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("sync update state")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close update state")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceFileDurably(temporaryPath, path); err != nil {
		return errors.New("replace update state")
	}
	return nil
}

func ensurePrivateUpdateStateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return errors.New("create update state directory")
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("update state directory is not owner-private")
	}
	return nil
}

func validUpdatePersistentState(state updatePersistentState) bool {
	return state.Version == updateStateVersion && state.RestartAttempts >= 0 && state.RestartAttempts <= 8 &&
		validUpdateStateValue(state.NotifiedVersion) && validUpdateStateValue(state.SkipOnceVersion) &&
		validUpdateStateValue(state.SkipVersion) && validUpdateStateValue(state.CooldownVersion) &&
		validUpdateStateValue(state.RestartVersion) && validUpdateStateTime(state.NotifiedAt) &&
		validUpdateStateTime(state.CooldownUntil) && validUpdateStateTime(state.RestartedAt) &&
		(state.NotifiedVersion != "" || state.NotifiedAt.IsZero()) &&
		(state.CooldownVersion != "" || state.CooldownUntil.IsZero()) &&
		(state.RestartVersion != "" || state.RestartAttempts == 0 && state.RestartedAt.IsZero())
}

func validUpdateStateValue(value string) bool {
	if len(value) > maxUpdateStateValueBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, unicode.IsControl) < 0
}

func validUpdateStateTime(value time.Time) bool {
	return value.IsZero() || value.Year() >= 2000 && value.Year() <= 9999
}

func normalizeUpdateStateTimes(state *updatePersistentState) {
	state.NotifiedAt = normalizeUpdateStateTime(state.NotifiedAt)
	state.CooldownUntil = normalizeUpdateStateTime(state.CooldownUntil)
	state.RestartedAt = normalizeUpdateStateTime(state.RestartedAt)
}

func normalizeUpdateStateTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC().Truncate(time.Second)
}
