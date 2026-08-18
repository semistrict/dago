package daconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const fileVersion = 1

// StoreOptions bounds the versioned on-disk document.
type StoreOptions struct {
	MaxBytes  int64
	MaxValues int
}

// DefaultStoreOptions returns finite production bounds.
func DefaultStoreOptions() StoreOptions { return StoreOptions{MaxBytes: 1 << 20, MaxValues: 256} }

// Store manages one owner-private configuration file.
type Store struct {
	manifest *Manifest
	path     string
	options  StoreOptions
	lock     chan struct{}
}

// NewStore constructs an owner-private configuration file manager. The
// manifest and path are required positional inputs; no filesystem I/O occurs.
func NewStore(manifest *Manifest, path string, options StoreOptions) *Store {
	if manifest == nil {
		panic("daconfig: store manifest is nil")
	}
	if path == "" || strings.ContainsRune(path, 0) || len(path) > 4096 {
		panic("daconfig: store path is required and bounded")
	}
	defaults := DefaultStoreOptions()
	if options.MaxBytes == 0 {
		options.MaxBytes = defaults.MaxBytes
	}
	if options.MaxValues == 0 {
		options.MaxValues = defaults.MaxValues
	}
	if options.MaxBytes < 1 || options.MaxBytes > 16<<20 || options.MaxValues < 1 || options.MaxValues > 4096 {
		panic("daconfig: store bounds are outside their finite range")
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return &Store{manifest: manifest, path: filepath.Clean(path), options: options, lock: lock}
}

// Path returns the normalized configured path without filesystem access.
func (store *Store) Path() string { return store.path }

type fileEnvelope struct {
	Version int            `json:"version"`
	Values  map[string]any `json:"values"`
}

// Load returns a file layer. A missing file is an empty useful layer.
func (store *Store) Load(ctx context.Context) (Layer, error) {
	if err := ctx.Err(); err != nil {
		return Layer{}, err
	}
	if err := store.acquire(ctx); err != nil {
		return Layer{}, err
	}
	defer store.release()
	values, err := store.loadLocked()
	if err != nil {
		return Layer{}, err
	}
	return NewLayer("file:"+store.path, values), nil
}

// Set validates and atomically stores one persistable option.
func (store *Store) Set(ctx context.Context, key string, value any) error {
	option, ok := store.manifest.Option(key)
	if !ok {
		return fmt.Errorf("%w: unknown option %q", ErrInvalidConfig, key)
	}
	if !option.Persist {
		return fmt.Errorf("%w: option %q cannot be stored in the config file", ErrInvalidConfig, option.Key)
	}
	coerced, err := coerce(option, value)
	if err != nil {
		return fmt.Errorf("%w: option %q: %v", ErrInvalidConfig, option.Key, err)
	}
	return store.update(ctx, func(values map[string]any) { values[option.Key] = coerced })
}

// Unset atomically removes one option and reports whether it existed.
func (store *Store) Unset(ctx context.Context, key string) (bool, error) {
	option, ok := store.manifest.Option(key)
	if !ok {
		return false, fmt.Errorf("%w: unknown option %q", ErrInvalidConfig, key)
	}
	if !option.Persist {
		return false, fmt.Errorf("%w: option %q cannot be stored in the config file", ErrInvalidConfig, option.Key)
	}
	removed := false
	err := store.update(ctx, func(values map[string]any) {
		_, removed = values[option.Key]
		delete(values, option.Key)
	})
	return removed, err
}

func (store *Store) update(ctx context.Context, change func(map[string]any)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.acquire(ctx); err != nil {
		return err
	}
	defer store.release()
	values, err := store.loadLocked()
	if err != nil {
		return err
	}
	change(values)
	if len(values) > store.options.MaxValues {
		return fmt.Errorf("%w: config file exceeds %d values", ErrInvalidConfig, store.options.MaxValues)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.saveLocked(values)
}

func (store *Store) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-store.lock:
		return nil
	}
}

func (store *Store) release() { store.lock <- struct{}{} }

func (store *Store) loadLocked() (map[string]any, error) {
	info, err := os.Lstat(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > store.options.MaxBytes {
		return nil, fmt.Errorf("%w: config file must be a bounded regular file", ErrInvalidConfig)
	}
	file, err := os.Open(store.path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() > store.options.MaxBytes {
		return nil, fmt.Errorf("%w: opened config file must remain a bounded regular file", ErrInvalidConfig)
	}
	decoder := json.NewDecoder(io.LimitReader(file, store.options.MaxBytes+1))
	decoder.UseNumber()
	envelope, err := decodeFileEnvelope(decoder)
	if err != nil {
		return nil, fmt.Errorf("%w: decode config file", ErrInvalidConfig)
	}
	finalInfo, err := file.Stat()
	if err != nil || !finalInfo.Mode().IsRegular() || finalInfo.Size() > store.options.MaxBytes {
		return nil, fmt.Errorf("%w: config file changed while it was read", ErrInvalidConfig)
	}
	if envelope.Version != fileVersion || len(envelope.Values) > store.options.MaxValues {
		return nil, fmt.Errorf("%w: unsupported version or excessive values", ErrInvalidConfig)
	}
	for key, raw := range envelope.Values {
		option, ok := store.manifest.Option(key)
		if !ok || option.Key != key || !option.Persist {
			return nil, fmt.Errorf("%w: config file contains unsupported option %q", ErrInvalidConfig, key)
		}
		value, err := coerce(option, raw)
		if err != nil {
			return nil, fmt.Errorf("%w: config file option %q: %v", ErrInvalidConfig, key, err)
		}
		envelope.Values[key] = value
	}
	if envelope.Values == nil {
		envelope.Values = map[string]any{}
	}
	return envelope.Values, nil
}

func (store *Store) saveLocked(values map[string]any) error {
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if info, err := os.Lstat(store.path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: config target is not a regular file", ErrInvalidConfig)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect config file: %w", err)
	}
	payload, err := json.MarshalIndent(fileEnvelope{Version: fileVersion, Values: values}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config file: %w", err)
	}
	payload = append(payload, '\n')
	if int64(len(payload)) > store.options.MaxBytes {
		return fmt.Errorf("%w: encoded config file exceeds %d bytes", ErrInvalidConfig, store.options.MaxBytes)
	}
	file, err := os.CreateTemp(parent, ".config-*")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure config file: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write config file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync config file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config file: %w", err)
	}
	if err := os.Rename(name, store.path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func decodeFileEnvelope(decoder *json.Decoder) (fileEnvelope, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return fileEnvelope{}, errors.New("expected object")
	}
	var envelope fileEnvelope
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] {
			return fileEnvelope{}, errors.New("invalid or duplicate envelope key")
		}
		seen[key] = true
		switch key {
		case "version":
			if err := decoder.Decode(&envelope.Version); err != nil {
				return fileEnvelope{}, err
			}
		case "values":
			values, err := decodeValuesObject(decoder)
			if err != nil {
				return fileEnvelope{}, err
			}
			envelope.Values = values
		default:
			return fileEnvelope{}, errors.New("unknown envelope key")
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || !seen["version"] || !seen["values"] {
		return fileEnvelope{}, errors.New("incomplete envelope")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fileEnvelope{}, errors.New("trailing data")
	}
	return envelope, nil
}

func decodeValuesObject(decoder *json.Decoder) (map[string]any, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("values must be an object")
	}
	values := map[string]any{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return nil, errors.New("invalid value key")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, errors.New("duplicate value key")
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("incomplete values object")
	}
	return values, nil
}
