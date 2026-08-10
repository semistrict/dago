package backend

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Memory stores virtual files in process memory. It is also the behavioral core
// used by state-backed agent files.
type Memory struct {
	mu    sync.RWMutex
	files map[string]FileData
	now   func() time.Time
}

func NewMemory(initial map[string]FileData) (*Memory, error) {
	result := &Memory{files: map[string]FileData{}, now: time.Now}
	for name, data := range initial {
		normalized, err := normalizeVirtual(name)
		if err != nil {
			return nil, err
		}
		result.files[normalized] = data
	}
	return result, nil
}

func (memory *Memory) List(ctx context.Context, directory string) (ListResult, error) {
	directory, err := normalizeVirtual(directory)
	if err != nil {
		return ListResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}
	prefix := strings.TrimSuffix(directory, "/") + "/"
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	entries := map[string]FileInfo{}
	for name, data := range memory.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(name, prefix)
		part, rest, _ := strings.Cut(remainder, "/")
		entryPath := prefix + part
		if rest != "" {
			entries[entryPath] = FileInfo{Path: entryPath + "/", IsDir: true}
		} else {
			entries[entryPath] = FileInfo{Path: entryPath, Size: int64(len(data.Content)), ModifiedAt: data.ModifiedAt}
		}
	}
	result := ListResult{Entries: make([]FileInfo, 0, len(entries))}
	for _, entry := range entries {
		result.Entries = append(result.Entries, entry)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
}

func (memory *Memory) Read(ctx context.Context, name string, offset, limit int) (ReadResult, error) {
	name, err := normalizeVirtual(name)
	if err != nil {
		return ReadResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}
	memory.mu.RLock()
	data, ok := memory.files[name]
	memory.mu.RUnlock()
	if !ok {
		return ReadResult{}, fmt.Errorf("path %q: file not found", name)
	}
	if data.Encoding == EncodingBase64 || IsBinaryReadPath(name) {
		copy := data
		copy.Encoding = EncodingBase64
		return ReadResult{Data: &copy}, nil
	}
	return SliceRead(data, offset, limit)
}

func (memory *Memory) Write(ctx context.Context, name, content string) (WriteResult, error) {
	name, err := normalizeVirtual(name)
	if err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	now := memory.now().UTC()
	created := now
	if old, ok := memory.files[name]; ok {
		created = old.CreatedAt
	}
	memory.files[name] = FileData{Content: content, Encoding: EncodingUTF8, CreatedAt: created, ModifiedAt: now}
	return WriteResult{Path: name}, nil
}

func (memory *Memory) Edit(ctx context.Context, name, old, replacement string, replaceAll bool) (EditResult, error) {
	if old == "" || old == replacement {
		return EditResult{}, fmt.Errorf("edit: old string must be non-empty and differ from replacement")
	}
	name, err := normalizeVirtual(name)
	if err != nil {
		return EditResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return EditResult{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	data, ok := memory.files[name]
	if !ok {
		return EditResult{}, fmt.Errorf("path %q: file not found", name)
	}
	if data.Encoding != EncodingUTF8 {
		return EditResult{}, fmt.Errorf("edit %q: binary files are unsupported", name)
	}
	updated, count, err := ReplaceText(name, data.Content, old, replacement, replaceAll)
	if err != nil {
		return EditResult{}, err
	}
	data.Content = updated
	data.ModifiedAt = memory.now().UTC()
	memory.files[name] = data
	return EditResult{Path: name, Occurrences: count}, nil
}

func (memory *Memory) Delete(ctx context.Context, name string) (DeleteResult, error) {
	name, err := normalizeVirtual(name)
	if err != nil {
		return DeleteResult{}, err
	}
	if name == "/" {
		return DeleteResult{}, fmt.Errorf("delete virtual root is not allowed")
	}
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	deleted := false
	prefix := strings.TrimSuffix(name, "/") + "/"
	for candidate := range memory.files {
		if candidate == name || strings.HasPrefix(candidate, prefix) {
			delete(memory.files, candidate)
			deleted = true
		}
	}
	if !deleted {
		return DeleteResult{}, fmt.Errorf("path %q: file not found", name)
	}
	return DeleteResult{Path: name}, nil
}

func (memory *Memory) Glob(ctx context.Context, pattern, base string) (GlobResult, error) {
	if base == "" {
		base = "/"
	}
	base, err := normalizeVirtual(base)
	if err != nil {
		return GlobResult{}, err
	}
	matcher, err := compileGlob(pattern)
	if err != nil {
		return GlobResult{}, err
	}
	prefix := strings.TrimSuffix(base, "/") + "/"
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	result := GlobResult{}
	for name, data := range memory.files {
		if err := ctx.Err(); err != nil {
			return GlobResult{}, err
		}
		if !strings.HasPrefix(name, prefix) || !matcher.MatchString(strings.TrimPrefix(name, prefix)) {
			continue
		}
		result.Matches = append(result.Matches, FileInfo{Path: name, Size: int64(len(data.Content)), ModifiedAt: data.ModifiedAt})
	}
	sort.Slice(result.Matches, func(i, j int) bool { return result.Matches[i].Path < result.Matches[j].Path })
	return result, nil
}

func (memory *Memory) Grep(ctx context.Context, pattern string, options GrepOptions) (GrepResult, error) {
	if pattern == "" {
		return GrepResult{}, fmt.Errorf("grep pattern is required")
	}
	base := options.Path
	if base == "" {
		base = "/"
	}
	base, err := normalizeVirtual(base)
	if err != nil {
		return GrepResult{}, err
	}
	var matcher *regexp.Regexp
	if options.Glob != "" {
		matcher, err = compileIncludeGlob(options.Glob)
		if err != nil {
			return GrepResult{}, err
		}
	}
	prefix := strings.TrimSuffix(base, "/") + "/"
	limit := options.MaxCount
	if limit <= 0 {
		limit = int(^uint(0) >> 1)
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	result := GrepResult{}
	for name, data := range memory.files {
		if err := ctx.Err(); err != nil {
			return GrepResult{}, err
		}
		if !strings.HasPrefix(name, prefix) || data.Encoding != EncodingUTF8 {
			continue
		}
		relative := strings.TrimPrefix(name, prefix)
		if matcher != nil && !matchIncludeGlob(matcher, options.Glob, relative) {
			continue
		}
		lines := splitLines(data.Content)
		for index, line := range lines {
			if !strings.Contains(line, pattern) {
				continue
			}
			if len(result.Matches) >= limit {
				result.Truncated = true
				break
			}
			result.Matches = append(result.Matches, GrepMatch{Path: name, Line: index + 1, Text: line})
		}
		if result.Truncated {
			break
		}
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Path != result.Matches[j].Path {
			return result.Matches[i].Path < result.Matches[j].Path
		}
		return result.Matches[i].Line < result.Matches[j].Line
	})
	return result, nil
}

func (memory *Memory) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	result := make([]UploadResult, len(uploads))
	for index, upload := range uploads {
		result[index].Path = upload.Path
		name, err := normalizeVirtual(upload.Path)
		if err == nil {
			if err = ctx.Err(); err == nil {
				now := memory.now().UTC()
				data := FileData{CreatedAt: now, ModifiedAt: now}
				if utf8.Valid(upload.Content) {
					data.Content, data.Encoding = string(upload.Content), EncodingUTF8
				} else {
					data.Content, data.Encoding = base64.StdEncoding.EncodeToString(upload.Content), EncodingBase64
				}
				memory.mu.Lock()
				if previous, ok := memory.files[name]; ok {
					data.CreatedAt = previous.CreatedAt
				}
				memory.files[name] = data
				memory.mu.Unlock()
			}
		}
		if err != nil {
			result[index].Error = err.Error()
		}
	}
	return result
}

func (memory *Memory) Download(ctx context.Context, names []string) []DownloadResult {
	result := make([]DownloadResult, len(names))
	for index, raw := range names {
		result[index].Path = raw
		name, err := normalizeVirtual(raw)
		if err == nil {
			err = ctx.Err()
		}
		memory.mu.RLock()
		data, ok := memory.files[name]
		if err == nil && ok {
			if data.Encoding == EncodingBase64 {
				result[index].Content, err = base64.StdEncoding.DecodeString(data.Content)
			} else {
				result[index].Content = []byte(data.Content)
			}
		}
		memory.mu.RUnlock()
		if err != nil {
			result[index].Error = err.Error()
		} else if !ok {
			result[index].Error = "file_not_found"
		}
	}
	return result
}

func normalizeVirtual(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("invalid virtual path %q", value)
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == ".." {
			return "", fmt.Errorf("path traversal is not allowed: %q", value)
		}
	}
	return path.Clean("/" + strings.TrimPrefix(value, "/")), nil
}
