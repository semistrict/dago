package backend

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type FilesystemOptions struct {
	Root string
	// AllowHostPaths disables virtual-root confinement. It is intended only for
	// trusted local development and must be opted into explicitly.
	AllowHostPaths bool
	MaxFileSize    int64
	MaxResults     int
}

// Filesystem confines virtual paths to an explicit root by default.
type Filesystem struct {
	root        string
	virtual     bool
	maxFileSize int64
	maxResults  int
}

func (backend *Filesystem) localHostRoot() string { return backend.root }

func NewFilesystem(options FilesystemOptions) (*Filesystem, error) {
	root := options.Root
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("filesystem root: %w", err)
	}
	if options.MaxFileSize <= 0 {
		options.MaxFileSize = 10 << 20
	}
	if options.MaxResults <= 0 {
		options.MaxResults = 1000
	}
	return &Filesystem{root: filepath.Clean(resolved), virtual: !options.AllowHostPaths, maxFileSize: options.MaxFileSize, maxResults: options.MaxResults}, nil
}

func (backend *Filesystem) List(ctx context.Context, path string) (ListResult, error) {
	resolved, err := backend.resolve(path, false)
	if err != nil {
		return ListResult{}, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return ListResult{}, normalizeFileError(path, err)
	}
	result := ListResult{Entries: make([]FileInfo, 0, len(entries))}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		info, err := entry.Info()
		if err != nil {
			return ListResult{}, err
		}
		name := filepath.Join(resolved, entry.Name())
		display := backend.display(name)
		if info.IsDir() {
			display += "/"
		}
		result.Entries = append(result.Entries, FileInfo{Path: display, IsDir: info.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
}

func (backend *Filesystem) Read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	resolved, err := backend.resolve(path, false)
	if err != nil {
		return ReadResult{}, err
	}
	data, info, err := readFile(ctx, resolved, backend.maxFileSize)
	if err != nil {
		return ReadResult{}, normalizeFileError(path, err)
	}
	fileData := FileData{CreatedAt: info.ModTime().UTC(), ModifiedAt: info.ModTime().UTC()}
	if isBinaryReadPath(path) || !utf8.Valid(data) {
		fileData.Content = base64.StdEncoding.EncodeToString(data)
		fileData.Encoding = EncodingBase64
		return ReadResult{Data: &fileData}, nil
	}
	fileData.Content = string(data)
	fileData.Encoding = EncodingUTF8
	return sliceFileData(fileData, offset, limit)
}

func (backend *Filesystem) Write(ctx context.Context, path, content string) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	resolved, err := backend.resolve(path, true)
	if err != nil {
		return WriteResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return WriteResult{}, normalizeFileError(path, err)
	}
	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		return WriteResult{}, normalizeFileError(path, err)
	}
	return WriteResult{Path: backend.display(resolved)}, nil
}

func (backend *Filesystem) Edit(ctx context.Context, path, old, replacement string, replaceAll bool) (EditResult, error) {
	if old == "" || old == replacement {
		return EditResult{}, fmt.Errorf("edit %q: old string must be non-empty and differ from replacement", path)
	}
	resolved, err := backend.resolve(path, false)
	if err != nil {
		return EditResult{}, err
	}
	data, info, err := readFile(ctx, resolved, backend.maxFileSize)
	if err != nil {
		return EditResult{}, normalizeFileError(path, err)
	}
	if !utf8.Valid(data) {
		return EditResult{}, fmt.Errorf("edit %q: binary files are unsupported", path)
	}
	content := normalizeNewlines(string(data))
	old = normalizeNewlines(old)
	replacement = normalizeNewlines(replacement)
	count := strings.Count(content, old)
	if count == 0 {
		return EditResult{}, editNotFoundError(path, content, old)
	}
	if !replaceAll && count != 1 {
		return EditResult{}, fmt.Errorf("edit %q: old string occurs %d times", path, count)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	updated := strings.Replace(content, old, replacement, limit)
	if err := os.WriteFile(resolved, []byte(updated), info.Mode().Perm()); err != nil {
		return EditResult{}, normalizeFileError(path, err)
	}
	if !replaceAll {
		count = 1
	}
	return EditResult{Path: backend.display(resolved), Occurrences: count}, nil
}

func (backend *Filesystem) Delete(ctx context.Context, path string) (DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	resolved, err := backend.resolveDelete(path)
	if err != nil {
		return DeleteResult{}, err
	}
	if backend.virtual && resolved == backend.root {
		return DeleteResult{}, fmt.Errorf("delete virtual root is not allowed")
	}
	if _, err := os.Lstat(resolved); err != nil {
		return DeleteResult{}, normalizeFileError(path, err)
	}
	if err := os.RemoveAll(resolved); err != nil {
		return DeleteResult{}, normalizeFileError(path, err)
	}
	return DeleteResult{Path: backend.display(resolved)}, nil
}

func (backend *Filesystem) Glob(ctx context.Context, pattern, base string) (GlobResult, error) {
	if base == "" {
		base = "/"
	}
	root, err := backend.resolve(base, false)
	if err != nil {
		return GlobResult{}, err
	}
	matcher, err := compileGlob(pattern)
	if err != nil {
		return GlobResult{}, err
	}
	result := GlobResult{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		if !matcher.MatchString(filepath.ToSlash(relative)) {
			return nil
		}
		if len(result.Matches) >= backend.maxResults {
			result.Truncated = true
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		display := backend.display(path)
		if info.IsDir() {
			display += "/"
		}
		result.Matches = append(result.Matches, FileInfo{Path: display, IsDir: info.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return GlobResult{}, err
	}
	sort.Slice(result.Matches, func(i, j int) bool { return result.Matches[i].Path < result.Matches[j].Path })
	return result, nil
}

func (backend *Filesystem) Grep(ctx context.Context, pattern string, options GrepOptions) (GrepResult, error) {
	if pattern == "" {
		return GrepResult{}, fmt.Errorf("grep pattern is required")
	}
	base := options.Path
	if base == "" {
		base = "/"
	}
	root, err := backend.resolve(base, false)
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
	limit := options.MaxCount
	if limit <= 0 {
		limit = backend.maxResults
	}
	result := GrepResult{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		if matcher != nil && !matchIncludeGlob(matcher, options.Glob, filepath.ToSlash(relative)) {
			return nil
		}
		data, _, err := readFile(ctx, path, backend.maxFileSize)
		if err != nil || !utf8.Valid(data) {
			return nil
		}
		lines := splitLines(string(data))
		matched := make([]bool, len(lines))
		for index, line := range lines {
			matched[index] = strings.Contains(line, pattern)
		}
		for index, yes := range matched {
			if !yes {
				continue
			}
			if len(result.Matches) >= limit {
				result.Truncated = true
				return fs.SkipAll
			}
			item := GrepMatch{Path: backend.display(path), Line: index + 1, Text: lines[index]}
			if options.ContextLines > 0 {
				for before := max(0, index-options.ContextLines); before < index; before++ {
					if !matched[before] {
						item.ContextBefore = append(item.ContextBefore, ContextLine{Line: before + 1, Text: lines[before]})
					}
				}
				for after := index + 1; after < min(len(lines), index+options.ContextLines+1); after++ {
					if !matched[after] {
						item.ContextAfter = append(item.ContextAfter, ContextLine{Line: after + 1, Text: lines[after]})
					}
				}
			}
			result.Matches = append(result.Matches, item)
		}
		return nil
	})
	if err != nil {
		return GrepResult{}, err
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Path != result.Matches[j].Path {
			return result.Matches[i].Path < result.Matches[j].Path
		}
		return result.Matches[i].Line < result.Matches[j].Line
	})
	return result, nil
}

func (backend *Filesystem) resolveDelete(value string) (string, error) {
	if strings.ContainsRune(value, '\x00') || value == "" {
		return "", fmt.Errorf("invalid path %q", value)
	}
	if !backend.virtual {
		if filepath.IsAbs(value) {
			return filepath.Clean(value), nil
		}
		return filepath.Join(backend.root, value), nil
	}
	if strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("invalid virtual path %q", value)
	}
	for _, segment := range strings.FieldsFunc(filepath.ToSlash(value), func(r rune) bool { return r == '/' }) {
		if segment == ".." {
			return "", fmt.Errorf("path traversal is not allowed: %q", value)
		}
	}
	clean := filepath.Clean("/" + filepath.ToSlash(value))
	candidate := filepath.Join(backend.root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if candidate == backend.root {
		return candidate, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", normalizeFileError(value, err)
	}
	relative, err := filepath.Rel(backend.root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes virtual root", value)
	}
	return filepath.Join(parent, filepath.Base(candidate)), nil
}

func (backend *Filesystem) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	result := make([]UploadResult, len(uploads))
	for index, upload := range uploads {
		result[index].Path = upload.Path
		if err := ctx.Err(); err != nil {
			result[index].Error = err.Error()
			continue
		}
		resolved, err := backend.resolve(upload.Path, true)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(resolved), 0o755)
		}
		if err == nil {
			err = os.WriteFile(resolved, upload.Content, 0o644)
		}
		if err != nil {
			result[index].Error = normalizedCode(err)
		}
	}
	return result
}

func (backend *Filesystem) Download(ctx context.Context, paths []string) []DownloadResult {
	result := make([]DownloadResult, len(paths))
	for index, path := range paths {
		result[index].Path = path
		if err := ctx.Err(); err != nil {
			result[index].Error = err.Error()
			continue
		}
		resolved, err := backend.resolve(path, false)
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(resolved)
		}
		if err == nil && info.IsDir() {
			result[index].Error = "is_directory"
			continue
		}
		if err == nil {
			result[index].Content, err = os.ReadFile(resolved)
		}
		if err != nil {
			result[index].Error = normalizedCode(err)
		}
	}
	return result
}

func (backend *Filesystem) resolve(value string, allowMissing bool) (string, error) {
	if strings.ContainsRune(value, '\x00') || value == "" {
		return "", fmt.Errorf("invalid path %q", value)
	}
	if !backend.virtual {
		if filepath.IsAbs(value) {
			return filepath.Clean(value), nil
		}
		return filepath.Join(backend.root, value), nil
	}
	if strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("invalid virtual path %q", value)
	}
	for _, segment := range strings.FieldsFunc(filepath.ToSlash(value), func(r rune) bool { return r == '/' }) {
		if segment == ".." {
			return "", fmt.Errorf("path traversal is not allowed: %q", value)
		}
	}
	clean := filepath.Clean("/" + filepath.ToSlash(value))
	candidate := filepath.Join(backend.root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if allowMissing {
		if info, statErr := os.Lstat(candidate); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", normalizeFileError(value, err)
			}
			candidate = resolved
			relative, relErr := filepath.Rel(backend.root, candidate)
			if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("path %q escapes virtual root", value)
			}
			return candidate, nil
		}
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			candidate = resolved
			relative, relErr := filepath.Rel(backend.root, candidate)
			if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("path %q escapes virtual root", value)
			}
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", normalizeFileError(value, err)
		}
	}
	verified := candidate
	if allowMissing {
		verified = filepath.Dir(candidate)
	}
	for {
		resolved, err := filepath.EvalSymlinks(verified)
		if err == nil {
			if allowMissing {
				relative, _ := filepath.Rel(verified, candidate)
				candidate = filepath.Join(resolved, relative)
			} else {
				candidate = resolved
			}
			break
		}
		if !allowMissing || !errors.Is(err, os.ErrNotExist) {
			return "", normalizeFileError(value, err)
		}
		parent := filepath.Dir(verified)
		if parent == verified {
			return "", err
		}
		verified = parent
	}
	relative, err := filepath.Rel(backend.root, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes virtual root", value)
	}
	return candidate, nil
}

func (backend *Filesystem) display(path string) string {
	if !backend.virtual {
		return path
	}
	relative, err := filepath.Rel(backend.root, path)
	if err != nil || relative == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(relative)
}

func readFile(ctx context.Context, path string, maxSize int64) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("is a directory")
	}
	if info.Size() > maxSize {
		return nil, nil, fmt.Errorf("file exceeds %d bytes", maxSize)
	}
	data, err := os.ReadFile(path)
	return data, info, err
}

func splitLines(value string) []string {
	value = normalizeNewlines(value)
	if value == "" {
		return nil
	}
	value = strings.TrimSuffix(value, "\n")
	if value == "" {
		return []string{""}
	}
	return strings.Split(value, "\n")
}

func sliceFileData(data FileData, offset, limit int) (ReadResult, error) {
	if strings.TrimSpace(data.Content) == "" {
		copy := data
		return ReadResult{Data: &copy}, nil
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		copy := data
		copy.Content = ""
		return ReadResult{Data: &copy, NoLinesRequested: true}, nil
	}
	normalized := normalizeNewlines(data.Content)
	lines := strings.SplitAfter(normalized, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if offset >= total {
		return ReadResult{}, fmt.Errorf("line offset %d exceeds file length (%d lines)", offset, total)
	}
	end := min(total, offset+limit)
	copy := data
	copy.Content = strings.Join(lines[offset:end], "")
	startLine, endLine := offset+1, end
	result := ReadResult{Data: &copy, TotalLines: &total, StartLine: &startLine, EndLine: &endLine}
	if end < total {
		next := end
		result.NextOffset = &next
	}
	return result, nil
}

func normalizeNewlines(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

func editNotFoundError(name, content, old string) error {
	if len(old) > 1 && strings.HasSuffix(old, "\n") && strings.HasSuffix(content, strings.TrimSuffix(old, "\n")) {
		withoutNewline := strings.TrimSuffix(old, "\n")
		count := strings.Count(content, withoutNewline)
		if count == 1 {
			return fmt.Errorf("edit %q: old string ends with a newline, but the file does not; retry with the trailing newline removed from old and replacement strings", name)
		}
		return fmt.Errorf("edit %q: old string ends with a newline, but the file does not; without it the string occurs %d times, so add surrounding context", name, count)
	}
	return fmt.Errorf("edit %q: old string not found", name)
}

func isBinaryReadPath(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpeg", ".jpg", ".webp", ".gif", ".heic", ".heif",
		".mp4", ".mpeg", ".mov", ".avi", ".flv", ".mpg", ".webm", ".wmv", ".3gpp", ".mkv",
		".wav", ".mp3", ".aiff", ".aac", ".ogg", ".flac", ".pdf", ".ppt", ".pptx":
		return true
	default:
		return false
	}
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob pattern is required")
	}
	pattern = filepath.ToSlash(strings.TrimPrefix(pattern, "/"))
	if !strings.HasPrefix(pattern, "**/") {
		pattern = "**/" + pattern
	}
	return compileGlobExpression(pattern)
}

func compileIncludeGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob pattern is required")
	}
	return compileGlobExpression(filepath.ToSlash(strings.TrimPrefix(pattern, "/")))
}

func compileGlobExpression(pattern string) (*regexp.Regexp, error) {
	var expression strings.Builder
	expression.WriteString("^(?:")
	for patternIndex, expanded := range expandGlobBraces(pattern) {
		if patternIndex > 0 {
			expression.WriteByte('|')
		}
		writeGlobPattern(&expression, expanded)
	}
	expression.WriteString(")$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	return compiled, nil
}

func writeGlobPattern(expression *strings.Builder, pattern string) {
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					expression.WriteString("(?:.*/)?")
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
			}
		case '?':
			expression.WriteString("[^/]")
		case '[', ']':
			expression.WriteByte(pattern[index])
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
}

func expandGlobBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	endOffset := strings.IndexByte(pattern[start+1:], '}')
	if endOffset < 0 {
		return []string{pattern}
	}
	end := start + 1 + endOffset
	choices := strings.Split(pattern[start+1:end], ",")
	if len(choices) < 2 {
		return []string{pattern}
	}
	var result []string
	for _, choice := range choices {
		for _, expanded := range expandGlobBraces(pattern[:start] + choice + pattern[end+1:]) {
			result = append(result, expanded)
		}
	}
	return result
}

func matchIncludeGlob(matcher *regexp.Regexp, pattern, relative string) bool {
	if strings.Contains(filepath.ToSlash(pattern), "/") {
		return matcher.MatchString(relative)
	}
	return matcher.MatchString(path.Base(relative))
}

func normalizeFileError(path string, err error) error {
	return fmt.Errorf("path %q: %w", path, err)
}

func normalizedCode(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "file_not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	default:
		return err.Error()
	}
}
