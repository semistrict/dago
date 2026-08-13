// Package contexthub stores a dago virtual file tree in a persistent Context
// Hub agent repository.
package contexthub

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
)

// ErrRepositoryNotFound tells Backend to treat a pull as a new, empty repo.
var ErrRepositoryNotFound = errors.New("context hub repository not found")

type EntryKind string

const (
	EntryFile  EntryKind = "file"
	EntryAgent EntryKind = "agent"
	EntrySkill EntryKind = "skill"
)

// Entry is one flattened Context Hub directory entry. File entries use Content;
// linked agent and skill entries use RepoHandle.
type Entry struct {
	Kind       EntryKind
	Content    string
	RepoHandle string
}

// Tree is the resolved file tree at one repository commit.
type Tree struct {
	CommitHash string
	Entries    map[string]Entry
}

// PushResult identifies the commit created by Push.
type PushResult struct {
	CommitHash string
}

// Client is the provider-neutral transport required by Backend. LangSmithClient
// is the production implementation; small implementations are useful in tests.
type Client interface {
	Pull(context.Context, string) (Tree, error)
	Push(context.Context, string, map[string]*Entry, string) (PushResult, error)
}

// Backend lazily pulls a Context Hub tree and commits each mutation atomically.
// Its mutex deliberately spans remote commits so concurrent writes cannot use
// the same stale parent commit.
type Backend struct {
	mu         sync.Mutex
	identifier string
	client     Client
	loaded     bool
	files      map[string]string
	linked     map[string]string
	commitHash string
}

func New(identifier string, client Client) (*Backend, error) {
	if strings.TrimSpace(identifier) == "" {
		return nil, fmt.Errorf("context hub backend: identifier is required")
	}
	if client == nil {
		return nil, fmt.Errorf("context hub backend: client is required")
	}
	return &Backend{identifier: identifier, client: client}, nil
}

func (remote *Backend) loadLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if remote.loaded {
		return nil
	}
	tree, err := remote.client.Pull(ctx, remote.identifier)
	if errors.Is(err, ErrRepositoryNotFound) {
		remote.files = map[string]string{}
		remote.linked = map[string]string{}
		remote.commitHash = ""
		remote.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("context hub pull %q: %w", remote.identifier, err)
	}
	remote.files = make(map[string]string)
	remote.linked = make(map[string]string)
	for name, entry := range tree.Entries {
		switch entry.Kind {
		case EntryFile:
			remote.files[name] = entry.Content
		case EntryAgent, EntrySkill:
			remote.linked[name] = entry.RepoHandle
		}
	}
	remote.commitHash = tree.CommitHash
	remote.loaded = true
	return nil
}

func (remote *Backend) invalidateLocked() {
	remote.loaded = false
	remote.files = nil
	remote.linked = nil
	remote.commitHash = ""
}

func (remote *Backend) commitLocked(ctx context.Context, changes map[string]*Entry) error {
	if len(changes) == 0 {
		return nil
	}
	result, err := remote.client.Push(ctx, remote.identifier, changes, remote.commitHash)
	if err != nil {
		remote.invalidateLocked()
		return fmt.Errorf("context hub push %q: %w", remote.identifier, err)
	}
	if result.CommitHash != "" {
		remote.commitHash = result.CommitHash
	}
	for name, entry := range changes {
		if entry == nil {
			delete(remote.files, name)
			continue
		}
		if entry.Kind == EntryFile {
			remote.files[name] = entry.Content
		}
	}
	return nil
}

func cleanPath(value string) (string, string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "~") {
		return "", "", fmt.Errorf("invalid virtual path %q", value)
	}
	trimmed := strings.TrimLeft(value, "/")
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		if segment == ".." {
			return "", "", fmt.Errorf("path traversal is not allowed: %q", value)
		}
	}
	clean := strings.Join(nonEmptySegments(segments), "/")
	virtual := "/" + clean
	if clean == "" {
		virtual = "/"
	}
	return virtual, clean, nil
}

func nonEmptySegments(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && value != "." {
			result = append(result, value)
		}
	}
	return result
}

// LinkedEntries returns a copy of linked agent and skill paths mapped to repo handles.
func (remote *Backend) LinkedEntries(ctx context.Context) (map[string]string, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(remote.linked))
	for name, handle := range remote.linked {
		result[name] = handle
	}
	return result, nil
}

// HasPriorCommits reports whether the repository was pulled at a commit or has
// received a successful commit through this backend.
func (remote *Backend) HasPriorCommits(ctx context.Context) (bool, error) {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return false, err
	}
	return remote.commitHash != "", nil
}

func (remote *Backend) List(ctx context.Context, directory string) (dabackend.ListResult, error) {
	_, prefix, err := cleanPath(directory)
	if err != nil {
		return dabackend.ListResult{}, err
	}
	prefix = strings.TrimSuffix(prefix, "/")
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.ListResult{}, err
	}
	entries := map[string]dabackend.FileInfo{}
	for name, content := range remote.files {
		if prefix != "" && !strings.HasPrefix(name, prefix+"/") {
			continue
		}
		relative := name
		if prefix != "" {
			relative = strings.TrimPrefix(name, prefix+"/")
		}
		if relative == "" {
			continue
		}
		part, rest, _ := strings.Cut(relative, "/")
		entryName := part
		if prefix != "" {
			entryName = prefix + "/" + part
		}
		if rest != "" {
			entries[entryName] = dabackend.FileInfo{Path: "/" + entryName, IsDir: true}
		} else {
			entries[entryName] = dabackend.FileInfo{Path: "/" + entryName, Size: int64(len(content))}
		}
	}
	result := dabackend.ListResult{Entries: make([]dabackend.FileInfo, 0, len(entries))}
	for _, entry := range entries {
		result.Entries = append(result.Entries, entry)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
}

func (remote *Backend) Read(ctx context.Context, filePath string, offset, limit int) (dabackend.ReadResult, error) {
	virtual, name, err := cleanPath(filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.ReadResult{}, err
	}
	content, ok := remote.files[name]
	if !ok {
		return dabackend.ReadResult{}, fmt.Errorf("path %q: file not found", virtual)
	}
	return dabackend.SliceRead(dabackend.FileData{Content: content, Encoding: dabackend.EncodingUTF8}, offset, limit)
}

func (remote *Backend) Write(ctx context.Context, filePath, content string) (dabackend.WriteResult, error) {
	virtual, name, err := cleanPath(filePath)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	if name == "" {
		return dabackend.WriteResult{}, fmt.Errorf("write virtual root is not allowed")
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.WriteResult{}, err
	}
	if err := remote.commitLocked(ctx, map[string]*Entry{name: {Kind: EntryFile, Content: content}}); err != nil {
		return dabackend.WriteResult{}, err
	}
	return dabackend.WriteResult{Path: virtual}, nil
}

func (remote *Backend) Edit(ctx context.Context, filePath, old, replacement string, replaceAll bool) (dabackend.EditResult, error) {
	if old == "" || old == replacement {
		return dabackend.EditResult{}, fmt.Errorf("edit: old string must be non-empty and differ from replacement")
	}
	virtual, name, err := cleanPath(filePath)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.EditResult{}, err
	}
	content, ok := remote.files[name]
	if !ok {
		return dabackend.EditResult{}, fmt.Errorf("path %q: file not found", virtual)
	}
	updated, count, err := dabackend.ReplaceText(virtual, content, old, replacement, replaceAll)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	if err := remote.commitLocked(ctx, map[string]*Entry{name: {Kind: EntryFile, Content: updated}}); err != nil {
		return dabackend.EditResult{}, err
	}
	return dabackend.EditResult{Path: virtual, Occurrences: count}, nil
}

func (remote *Backend) Delete(ctx context.Context, filePath string) (dabackend.DeleteResult, error) {
	virtual, name, err := cleanPath(filePath)
	if err != nil {
		return dabackend.DeleteResult{}, err
	}
	if name == "" {
		return dabackend.DeleteResult{}, fmt.Errorf("delete virtual root is not allowed")
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.DeleteResult{}, err
	}
	changes := map[string]*Entry{}
	prefix := name + "/"
	for candidate := range remote.files {
		if candidate == name || strings.HasPrefix(candidate, prefix) {
			changes[candidate] = nil
		}
	}
	if len(changes) == 0 {
		return dabackend.DeleteResult{}, fmt.Errorf("path %q: file not found", virtual)
	}
	if err := remote.commitLocked(ctx, changes); err != nil {
		return dabackend.DeleteResult{}, err
	}
	return dabackend.DeleteResult{Path: virtual}, nil
}

func (remote *Backend) Glob(ctx context.Context, pattern, _ string) (dabackend.GlobResult, error) {
	matcher, err := compileFnmatch(pattern)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.GlobResult{}, err
	}
	result := dabackend.GlobResult{}
	for name, content := range remote.files {
		if matcher.MatchString(name) || matcher.MatchString("/"+name) {
			result.Matches = append(result.Matches, dabackend.FileInfo{Path: "/" + name, Size: int64(len(content))})
		}
	}
	sort.Slice(result.Matches, func(i, j int) bool { return result.Matches[i].Path < result.Matches[j].Path })
	return result, nil
}

func (remote *Backend) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	if err := dabackend.ValidateGrepOptions(options); err != nil {
		return dabackend.GrepResult{}, err
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return dabackend.GrepResult{}, fmt.Errorf("invalid regex pattern: %w", err)
	}
	var glob *regexp.Regexp
	if options.Glob != "" {
		glob, err = compileFnmatch(options.Glob)
		if err != nil {
			return dabackend.GrepResult{}, err
		}
	}
	_, prefix, err := cleanPath(defaultPath(options.Path))
	if err != nil {
		return dabackend.GrepResult{}, err
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if err := remote.loadLocked(ctx); err != nil {
		return dabackend.GrepResult{}, err
	}
	names := sortedNames(remote.files)
	result := dabackend.GrepResult{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return dabackend.GrepResult{}, err
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		if glob != nil && !glob.MatchString(name) {
			continue
		}
		for index, line := range splitContentLines(remote.files[name]) {
			if !matcher.MatchString(line) {
				continue
			}
			if !options.Uncapped && options.MaxCount > 0 && len(result.Matches) >= options.MaxCount {
				result.Truncated = true
				return result, nil
			}
			result.Matches = append(result.Matches, dabackend.GrepMatch{Path: "/" + name, Line: index + 1, Text: line})
		}
	}
	return result, nil
}

func defaultPath(value string) string {
	if value == "" {
		return "/"
	}
	return value
}

func splitContentLines(value string) []string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	if value == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(value, "\n"), "\n")
}

func sortedNames(files map[string]string) []string {
	result := make([]string, 0, len(files))
	for name := range files {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func compileFnmatch(pattern string) (*regexp.Regexp, error) {
	var expression strings.Builder
	expression.WriteString("(?s)^")
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			expression.WriteString(".*")
		case '?':
			expression.WriteByte('.')
		case '[':
			end := strings.IndexByte(pattern[index+1:], ']')
			if end < 0 {
				expression.WriteString("\\[")
				continue
			}
			end += index + 1
			class := pattern[index+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			expression.WriteByte('[')
			expression.WriteString(class)
			expression.WriteByte(']')
			index = end
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteByte('$')
	matcher, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}
	return matcher, nil
}

func (remote *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	result := make([]dabackend.UploadResult, len(uploads))
	changes := map[string]*Entry{}
	valid := make([]bool, len(uploads))
	for index, upload := range uploads {
		result[index].Path = upload.Path
		_, name, err := cleanPath(upload.Path)
		if err != nil || name == "" || !utf8.Valid(upload.Content) {
			result[index].Error = "invalid_path"
			continue
		}
		valid[index] = true
		changes[name] = &Entry{Kind: EntryFile, Content: string(upload.Content)}
	}
	if len(changes) == 0 {
		return result
	}
	remote.mu.Lock()
	err := remote.loadLocked(ctx)
	if err == nil {
		err = remote.commitLocked(ctx, changes)
	}
	remote.mu.Unlock()
	if err != nil {
		for index := range result {
			if valid[index] {
				result[index].Error = err.Error()
			}
		}
	}
	return result
}

func (remote *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	result := make([]dabackend.DownloadResult, len(paths))
	remote.mu.Lock()
	err := remote.loadLocked(ctx)
	for index, filePath := range paths {
		result[index].Path = filePath
		if err != nil {
			result[index].Error = err.Error()
			continue
		}
		_, name, cleanErr := cleanPath(filePath)
		if cleanErr != nil {
			result[index].Error = "file_not_found"
			continue
		}
		content, ok := remote.files[name]
		if !ok {
			result[index].Error = "file_not_found"
			continue
		}
		result[index].Content = []byte(content)
	}
	remote.mu.Unlock()
	return result
}

var _ dabackend.Backend = (*Backend)(nil)
