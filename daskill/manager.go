package daskill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaximumSources = 32
	defaultMaximumSkills  = 1_000
	defaultMaximumFile    = 1 << 20
	defaultMaximumTrusted = 1_000
)

// Source is one skill directory. Sources are ordered from lowest to highest
// precedence; a later skill with the same name replaces an earlier one.
type Source struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// Entry is one discovered skill and its provenance. An untrusted external
// symlink is listed without reading its contents so callers can ask for trust.
type Entry struct {
	Skill         Skill  `json:"skill"`
	Source        string `json:"source"`
	TrustRequired bool   `json:"trust_required,omitempty"`
	TargetDir     string `json:"target_dir,omitempty"`
}

// ManagerOptions bounds local discovery and file reads. Zero values select
// conservative finite defaults.
type ManagerOptions struct {
	MaximumSources int
	MaximumSkills  int
	MaximumFile    int64
}

// Manager discovers and loads local skills without granting filesystem or
// network authority beyond its configured source directories.
type Manager struct {
	sources []Source
	trust   *TrustStore
	options ManagerOptions
}

// NewManager constructs a manager. The required sources and trust store are
// positional; invalid static inputs panic rather than creating a partly useful
// manager.
func NewManager(sources []Source, trust *TrustStore, options ManagerOptions) *Manager {
	if trust == nil {
		panic("daskill: trust store is nil")
	}
	if options.MaximumSources < 0 || options.MaximumSkills < 0 || options.MaximumFile < 0 {
		panic("daskill: manager limits cannot be negative")
	}
	if options.MaximumSources == 0 {
		options.MaximumSources = defaultMaximumSources
	}
	if options.MaximumSkills == 0 {
		options.MaximumSkills = defaultMaximumSkills
	}
	if options.MaximumFile == 0 {
		options.MaximumFile = defaultMaximumFile
	}
	if len(sources) > options.MaximumSources {
		panic("daskill: too many sources")
	}
	copySources := make([]Source, len(sources))
	for index, source := range sources {
		if strings.TrimSpace(source.Dir) == "" {
			panic("daskill: source directory is empty")
		}
		source.Dir = filepath.Clean(ExpandPath(source.Dir))
		copySources[index] = source
	}
	return &Manager{sources: copySources, trust: trust, options: options}
}

// List returns the effective catalog in deterministic name order.
func (manager *Manager) List(ctx context.Context) ([]Entry, error) {
	selected := make(map[string]Entry)
	count := 0
	for _, source := range manager.sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(source.Dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read skill source %q: %w", source.Name, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, child := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if ValidateName(child.Name()) != nil {
				continue
			}
			count++
			if count > manager.options.MaximumSkills {
				return nil, fmt.Errorf("skill discovery exceeds %d entries", manager.options.MaximumSkills)
			}
			entry, present, loadErr := manager.inspect(ctx, source, child.Name(), false)
			if loadErr != nil {
				return nil, loadErr
			}
			if present {
				selected[child.Name()] = entry
			}
		}
	}
	result := make([]Entry, 0, len(selected))
	for _, entry := range selected {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Skill.Name < result[j].Skill.Name })
	return result, nil
}

// Load returns the effective named skill. External symlinks require a current
// trust-store grant for their canonical target directory.
func (manager *Manager) Load(ctx context.Context, name string) (Entry, error) {
	if err := ValidateName(name); err != nil {
		return Entry{}, err
	}
	var selected *Entry
	for _, source := range manager.sources {
		entry, present, err := manager.inspect(ctx, source, name, true)
		if err != nil {
			var trustErr *TrustRequiredError
			if errors.As(err, &trustErr) {
				copyEntry := entry
				selected = &copyEntry
				continue
			}
			return Entry{}, err
		}
		if present {
			copyEntry := entry
			selected = &copyEntry
		}
	}
	if selected == nil {
		return Entry{}, fmt.Errorf("skill %q was not found", name)
	}
	if selected.TrustRequired {
		return *selected, &TrustRequiredError{Skill: name, TargetDir: selected.TargetDir}
	}
	return *selected, nil
}

func (manager *Manager) inspect(ctx context.Context, source Source, name string, body bool) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	root, err := filepath.Abs(source.Dir)
	if err != nil {
		return Entry{}, false, fmt.Errorf("resolve skill source %q: %w", source.Name, err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("resolve skill source %q: %w", source.Name, err)
	}
	dir := filepath.Join(root, name)
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.IsDir() {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("inspect skill %q: %w", name, err)
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Entry{}, false, fmt.Errorf("resolve skill %q: %w", name, err)
	}
	allowed := pathWithin(canonicalRoot, canonicalDir)
	if !allowed {
		allowed, err = manager.trust.Trusted(ctx, canonicalDir)
		if err != nil {
			return Entry{}, false, err
		}
	}
	if !allowed {
		entry := Entry{Skill: Skill{Name: name, Path: filepath.Join(dir, "SKILL.md")}, Source: source.Name, TrustRequired: true, TargetDir: canonicalDir}
		if body {
			return entry, true, &TrustRequiredError{Skill: name, TargetDir: canonicalDir}
		}
		return entry, true, nil
	}
	filePath := FindFile(dir)
	if filePath == "" {
		return Entry{}, false, nil
	}
	canonicalFile, err := filepath.EvalSymlinks(filePath)
	if err != nil || !pathWithin(canonicalDir, canonicalFile) {
		return Entry{}, false, fmt.Errorf("skill %q file escapes its skill directory", name)
	}
	content, err := readBoundedFile(ctx, canonicalFile, manager.options.MaximumFile)
	if err != nil {
		return Entry{}, false, fmt.Errorf("read skill %q: %w", name, err)
	}
	parsed, warnings, err := ParseContent(content, filepath.ToSlash(filePath))
	if err != nil || len(warnings) > 0 || parsed.Name != name {
		if err == nil {
			err = fmt.Errorf("skill metadata does not match directory %q", name)
		}
		return Entry{}, false, err
	}
	if !body {
		parsed.Body = ""
	}
	return Entry{Skill: parsed, Source: source.Name}, true, nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func readBoundedFile(ctx context.Context, path string, limit int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("not a regular file")
		}
		return "", err
	}
	if info.Size() > limit {
		return "", fmt.Errorf("file exceeds %d bytes", limit)
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > limit {
		return "", fmt.Errorf("file exceeds %d bytes", limit)
	}
	return string(content), ctx.Err()
}

// TrustRequiredError identifies the exact canonical directory that must be
// approved before an external symlink is read.
type TrustRequiredError struct {
	Skill     string
	TargetDir string
}

func (err *TrustRequiredError) Error() string {
	return fmt.Sprintf("skill %q resolves outside configured sources; trust %q to continue", err.Skill, err.TargetDir)
}

type trustDocument struct {
	Version int           `json:"version"`
	Paths   []TrustRecord `json:"paths"`
}

// TrustRecord is one exact canonical-directory approval.
type TrustRecord struct {
	Path      string    `json:"path"`
	TrustedAt time.Time `json:"trusted_at"`
}

// TrustStore persists exact canonical directories approved by a user.
type TrustStore struct {
	path string
	mu   sync.Mutex
}

// NewTrustStore creates a handle for path. It performs no I/O and therefore
// does not return an error; an empty required path is a programming error.
func NewTrustStore(path string) *TrustStore {
	if strings.TrimSpace(path) == "" {
		panic("daskill: trust store path is empty")
	}
	return &TrustStore{path: filepath.Clean(ExpandPath(path))}
}

// Trusted reports whether path is the same canonical directory that was
// approved. Repointed symlinks never inherit an old approval.
func (store *TrustStore) Trusted(ctx context.Context, path string) (bool, error) {
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return false, err
	}
	records, err := store.List(ctx)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.Path != canonical {
			continue
		}
		resolved, resolveErr := filepath.EvalSymlinks(record.Path)
		return resolveErr == nil && resolved == record.Path, nil
	}
	return false, nil
}

// Trust records one existing canonical directory.
func (store *TrustStore) Trust(ctx context.Context, path string) error {
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return err
	}
	return store.update(ctx, func(records []TrustRecord) ([]TrustRecord, error) {
		for _, record := range records {
			if record.Path == canonical {
				return records, nil
			}
		}
		if len(records) >= defaultMaximumTrusted {
			return nil, fmt.Errorf("trust store exceeds %d entries", defaultMaximumTrusted)
		}
		return append(records, TrustRecord{Path: canonical, TrustedAt: time.Now().UTC()}), nil
	})
}

// Revoke removes the canonical path if present.
func (store *TrustStore) Revoke(ctx context.Context, path string) error {
	canonical, err := filepath.Abs(ExpandPath(path))
	if err != nil {
		return err
	}
	canonical = filepath.Clean(canonical)
	if resolved, resolveErr := filepath.EvalSymlinks(canonical); resolveErr == nil {
		canonical = filepath.Clean(resolved)
	}
	return store.update(ctx, func(records []TrustRecord) ([]TrustRecord, error) {
		filtered := records[:0]
		for _, record := range records {
			if record.Path != canonical {
				filtered = append(filtered, record)
			}
		}
		return filtered, nil
	})
}

// Clear removes all grants while retaining a valid private store.
func (store *TrustStore) Clear(ctx context.Context) error {
	return store.update(ctx, func([]TrustRecord) ([]TrustRecord, error) { return nil, nil })
}

// List returns grants in deterministic path order.
func (store *TrustStore) List(ctx context.Context) ([]TrustRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	records, err := store.readLocked(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	return records, nil
}

func (store *TrustStore) update(ctx context.Context, change func([]TrustRecord) ([]TrustRecord, error)) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	records, err := store.readLocked(ctx)
	if err != nil {
		return err
	}
	records, err = change(records)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	document := trustDocument{Version: 1, Paths: records}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if err := validateTrustParent(parent); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".skill-trust-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
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
	if err := replaceTrustFile(temporaryPath, store.path); err != nil {
		return err
	}
	return syncTrustDirectory(parent)
}

func (store *TrustStore) readLocked(ctx context.Context) ([]TrustRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := validateTrustParent(filepath.Dir(store.path)); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("skill trust store is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("skill trust store permissions are too broad")
	}
	if info.Size() > defaultMaximumFile {
		return nil, errors.New("skill trust store is too large")
	}
	content, err := os.ReadFile(store.path)
	if err != nil {
		return nil, err
	}
	var document trustDocument
	if err := json.Unmarshal(content, &document); err != nil || document.Version != 1 {
		return nil, errors.New("skill trust store is corrupt or unsupported")
	}
	if len(document.Paths) > defaultMaximumTrusted {
		return nil, errors.New("skill trust store has too many entries")
	}
	seen := make(map[string]bool, len(document.Paths))
	for _, record := range document.Paths {
		if !filepath.IsAbs(record.Path) || filepath.Clean(record.Path) != record.Path || seen[record.Path] {
			return nil, errors.New("skill trust store contains an invalid path")
		}
		seen[record.Path] = true
	}
	return append([]TrustRecord(nil), document.Paths...), nil
}

func validateTrustParent(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("skill trust store parent is not a direct directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("skill trust store parent permissions are too broad")
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(ExpandPath(path))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return "", err
	}
	return filepath.Clean(canonical), nil
}
