package daplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type MarketplaceRecord struct {
	Name            string            `json:"name"`
	Source          MarketplaceSource `json:"source"`
	InstallLocation string            `json:"install_location"`
}
type InstalledEntry struct {
	InstallPath string `json:"install_path"`
	Version     string `json:"version,omitempty"`
}
type State struct {
	Version      int                          `json:"version"`
	Marketplaces map[string]MarketplaceRecord `json:"marketplaces"`
	Installed    map[string]InstalledEntry    `json:"installed"`
	Enabled      map[string]bool              `json:"enabled"`
}

type StoreOptions struct {
	MaxCopyFiles int
	MaxCopyBytes int64
}
type Store struct {
	root, statePath string
	maxCopyFiles    int
	maxCopyBytes    int64
}

// NewStore constructs a local store without filesystem I/O.
func NewStore(root string, options StoreOptions) *Store {
	if strings.TrimSpace(root) == "" {
		panic("daplugin: store root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		panic("daplugin: invalid store root")
	}
	if options.MaxCopyFiles < 0 || options.MaxCopyBytes < 0 {
		panic("daplugin: copy limits cannot be negative")
	}
	if options.MaxCopyFiles == 0 {
		options.MaxCopyFiles = 4096
	}
	if options.MaxCopyBytes == 0 {
		options.MaxCopyBytes = 128 << 20
	}
	return &Store{root: abs, statePath: filepath.Join(abs, "state.json"), maxCopyFiles: options.MaxCopyFiles, maxCopyBytes: options.MaxCopyBytes}
}
func (store *Store) Root() string { return store.root }
func emptyState() State {
	return State{Version: 1, Marketplaces: map[string]MarketplaceRecord{}, Installed: map[string]InstalledEntry{}, Enabled: map[string]bool{}}
}

func (store *Store) Load(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if info, err := os.Lstat(store.root); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return State{}, errors.New("plugin store root must be a real directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}
	state := emptyState()
	info, err := os.Lstat(store.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return State{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > MaxManifestBytes {
		return State{}, errors.New("plugin state is not a bounded regular file")
	}
	if err := boundedJSON(store.statePath, &state); err != nil {
		return State{}, fmt.Errorf("read plugin state: %w", err)
	}
	if state.Version != 1 || state.Marketplaces == nil || state.Installed == nil || state.Enabled == nil {
		return State{}, errors.New("plugin state has unsupported shape or version")
	}
	if len(state.Marketplaces) > MaxPlugins || len(state.Installed) > MaxPlugins || len(state.Enabled) > MaxPlugins {
		return State{}, errors.New("plugin state exceeds entry limit")
	}
	return state, nil
}

func (store *Store) mutate(ctx context.Context, update func(*State) error) error {
	release, err := acquireStoreLock(ctx, filepath.Join(store.root, ".mutation.lock"))
	if err != nil {
		return err
	}
	defer release()
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if err := update(&state); err != nil {
		return err
	}
	return store.writeState(ctx, state)
}

func (store *Store) writeState(ctx context.Context, state State) error {
	if len(state.Marketplaces) > MaxPlugins || len(state.Installed) > MaxPlugins || len(state.Enabled) > MaxPlugins {
		return errors.New("plugin state exceeds entry limit")
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxManifestBytes {
		return errors.New("plugin state exceeds size limit")
	}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(store.root, ".state-*.tmp")
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
	if _, err := temporary.Write(raw); err != nil {
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
	return os.Rename(name, store.statePath)
}

func (store *Store) AddMarketplace(ctx context.Context, record MarketplaceRecord) error {
	if validateName(record.Name, false) != nil || record.InstallLocation == "" {
		return errors.New("invalid marketplace record")
	}
	return store.mutate(ctx, func(state *State) error {
		if existing, ok := state.Marketplaces[record.Name]; ok && existing != record {
			for id := range state.Installed {
				if strings.HasSuffix(id, "@"+record.Name) {
					return errors.New("cannot replace a marketplace while its plugins are installed")
				}
			}
		}
		state.Marketplaces[record.Name] = record
		return nil
	})
}
func (store *Store) RemoveMarketplace(ctx context.Context, name string) error {
	return store.mutate(ctx, func(state *State) error {
		delete(state.Marketplaces, name)
		prefix := "@" + name
		for id := range state.Installed {
			if strings.HasSuffix(id, prefix) {
				delete(state.Installed, id)
				delete(state.Enabled, id)
			}
		}
		return nil
	})
}

func (store *Store) RemoveMarketplaceCascade(ctx context.Context, name string) (MarketplaceRecord, bool, error) {
	release, err := acquireStoreLock(ctx, filepath.Join(store.root, ".mutation.lock"))
	if err != nil {
		return MarketplaceRecord{}, false, err
	}
	defer release()
	state, err := store.Load(ctx)
	if err != nil {
		return MarketplaceRecord{}, false, err
	}
	record, exists := state.Marketplaces[name]
	if !exists {
		return MarketplaceRecord{}, false, nil
	}
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return MarketplaceRecord{}, false, err
	}
	defer root.Close()
	renamed := map[string]string{}
	rollback := func() {
		for quarantine, original := range renamed {
			_ = root.Rename(quarantine, original)
		}
	}
	for id, entry := range state.Installed {
		if !strings.HasSuffix(id, "@"+name) {
			continue
		}
		rel, relErr := filepath.Rel(filepath.Join(store.root, "cache"), entry.InstallPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			rollback()
			return MarketplaceRecord{}, false, errors.New("installed plugin path escapes cache")
		}
		original, _ := filepath.Rel(store.root, entry.InstallPath)
		quarantine := original + ".removing-marketplace"
		_ = root.RemoveAll(quarantine)
		if renameErr := root.Rename(original, quarantine); renameErr != nil && !errors.Is(renameErr, os.ErrNotExist) {
			rollback()
			return MarketplaceRecord{}, false, renameErr
		}
		renamed[quarantine] = original
		delete(state.Installed, id)
		delete(state.Enabled, id)
	}
	delete(state.Marketplaces, name)
	if err := store.writeState(ctx, state); err != nil {
		rollback()
		return MarketplaceRecord{}, false, err
	}
	for quarantine := range renamed {
		_ = root.RemoveAll(quarantine)
	}
	return record, true, nil
}
func (store *Store) SetEnabled(ctx context.Context, pluginID string, enabled bool) error {
	if _, _, err := SplitID(pluginID); err != nil {
		return err
	}
	return store.mutate(ctx, func(state *State) error {
		if _, ok := state.Installed[pluginID]; !ok {
			return errors.New("plugin is not installed")
		}
		state.Enabled[pluginID] = enabled
		return nil
	})
}

func SplitID(id string) (string, string, error) {
	index := strings.LastIndex(id, "@")
	if index < 1 || index == len(id)-1 {
		return "", "", errors.New("plugin id must be name@marketplace")
	}
	name, market := id[:index], id[index+1:]
	if validateName(name, true) != nil || validateName(market, false) != nil {
		return "", "", errors.New("plugin id must be name@marketplace")
	}
	return name, market, nil
}

func (store *Store) Install(ctx context.Context, pluginID, source string, version string) (InstalledEntry, error) {
	return store.install(ctx, pluginID, source, version, nil)
}

func (store *Store) install(ctx context.Context, pluginID, source string, version string, expected *MarketplaceRecord) (InstalledEntry, error) {
	if _, _, err := SplitID(pluginID); err != nil {
		return InstalledEntry{}, err
	}
	canonical, err := canonicalRoot(source)
	if err != nil {
		return InstalledEntry{}, err
	}
	digest := sha256.Sum256([]byte(pluginID + "\x00" + version))
	target := filepath.Join(store.root, "cache", hex.EncodeToString(digest[:16]))
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return InstalledEntry{}, err
	}
	temp, err := os.MkdirTemp(parent, ".install-*")
	if err != nil {
		return InstalledEntry{}, err
	}
	defer os.RemoveAll(temp)
	if err := store.copyTree(ctx, canonical, temp); err != nil {
		return InstalledEntry{}, err
	}
	name, _, _ := SplitID(pluginID)
	manifest, _, err := LoadManifest(temp, name)
	if err != nil {
		return InstalledEntry{}, err
	}
	if manifest != nil && manifest.Name != name {
		return InstalledEntry{}, errors.New("plugin manifest name does not match marketplace entry")
	}
	if manifest != nil {
		version = manifest.Version
	}
	entry := InstalledEntry{InstallPath: target, Version: version}
	release, err := acquireStoreLock(ctx, filepath.Join(store.root, ".mutation.lock"))
	if err != nil {
		return InstalledEntry{}, err
	}
	defer release()
	state, err := store.Load(ctx)
	if err != nil {
		return InstalledEntry{}, err
	}
	if expected != nil {
		current, ok := state.Marketplaces[expected.Name]
		if !ok || current != *expected {
			return InstalledEntry{}, errors.New("marketplace changed during plugin installation")
		}
	}
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return InstalledEntry{}, err
	}
	defer root.Close()
	targetRel, _ := filepath.Rel(store.root, target)
	backupRel := targetRel + ".backup"
	_ = root.RemoveAll(backupRel)
	if version != "" {
		if info, statErr := root.Stat(targetRel); statErr == nil && info.IsDir() {
			state.Installed[pluginID] = entry
			state.Enabled[pluginID] = true
			return entry, store.writeState(ctx, state)
		}
	}
	if _, statErr := root.Stat(targetRel); statErr == nil {
		if err := root.Rename(targetRel, backupRel); err != nil {
			return InstalledEntry{}, err
		}
	}
	tempRel, _ := filepath.Rel(store.root, temp)
	if err := root.Rename(tempRel, targetRel); err != nil {
		_ = root.Rename(backupRel, targetRel)
		return InstalledEntry{}, err
	}
	state.Installed[pluginID] = entry
	state.Enabled[pluginID] = true
	if err := store.writeState(ctx, state); err != nil {
		_ = root.RemoveAll(targetRel)
		_ = root.Rename(backupRel, targetRel)
		return InstalledEntry{}, err
	}
	_ = root.RemoveAll(backupRel)
	return entry, nil
}

func (store *Store) copyTree(ctx context.Context, source, target string) error {
	files := 0
	var bytes int64
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, _ := filepath.Rel(source, path)
		if relative == "." {
			return nil
		}
		if strings.HasPrefix(relative, ".git"+string(filepath.Separator)) || relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return errors.New("plugin source contains linked or special entries")
		}
		destination := filepath.Join(target, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		files++
		bytes += info.Size()
		if files > store.maxCopyFiles || bytes > store.maxCopyBytes {
			return errors.New("plugin source exceeds copy limits")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o600) | info.Mode().Perm()&0o100
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, io.LimitReader(input, store.maxCopyBytes+1))
		inputErr := input.Close()
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputErr != nil {
			return inputErr
		}
		return closeErr
	})
}

func (store *Store) Uninstall(ctx context.Context, pluginID string) error {
	release, err := acquireStoreLock(ctx, filepath.Join(store.root, ".mutation.lock"))
	if err != nil {
		return err
	}
	defer release()
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	entry, ok := state.Installed[pluginID]
	if !ok {
		return nil
	}
	rel, err := filepath.Rel(filepath.Join(store.root, "cache"), entry.InstallPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("installed plugin path escapes cache")
	}
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return err
	}
	defer root.Close()
	relative, _ := filepath.Rel(store.root, entry.InstallPath)
	quarantine := relative + ".removing"
	_ = root.RemoveAll(quarantine)
	if err := root.Rename(relative, quarantine); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(state.Installed, pluginID)
	delete(state.Enabled, pluginID)
	if err := store.writeState(ctx, state); err != nil {
		_ = root.Rename(quarantine, relative)
		return err
	}
	_ = root.RemoveAll(quarantine)
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
