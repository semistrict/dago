package dabackend

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type FilesystemOptions struct {
	Root string
	// GetRoot optionally supplies the current root for each operation. It is
	// useful for applications with an explicit change-directory tool.
	GetRoot func() string
	// AllowHostPaths disables virtual-root confinement. It is intended only for
	// trusted local development and must be opted into explicitly.
	AllowHostPaths bool
	// MaxFileSize limits files scanned by Grep. It does not limit paginated
	// text reads.
	MaxFileSize int64
	// MaxVideoSize limits video values returned by a direct Read. Middleware can
	// impose a lower per-invocation limit through ReadBinary.
	MaxVideoSize int64
	MaxResults   int
}

// Filesystem confines virtual paths to an explicit root by default.
type Filesystem struct {
	root         string
	getRoot      func() string
	virtual      bool
	maxFileSize  int64
	maxVideoSize int64
	maxResults   int
}

type filesystemPath struct {
	root *os.Root
	name string
	host string
}

func (value filesystemPath) Close() {
	if value.root != nil {
		_ = value.root.Close()
	}
}

func (backend *Filesystem) openPath(value string) (filesystemPath, error) {
	if strings.ContainsRune(value, '\x00') || value == "" {
		return filesystemPath{}, fmt.Errorf("invalid path %q", value)
	}
	rootName, err := backend.currentRoot()
	if err != nil {
		return filesystemPath{}, err
	}
	if !backend.virtual {
		host := value
		if !filepath.IsAbs(host) {
			host = filepath.Join(rootName, host)
		}
		return filesystemPath{host: filepath.Clean(host)}, nil
	}
	if strings.HasPrefix(value, "~") {
		return filesystemPath{}, fmt.Errorf("invalid virtual path %q", value)
	}
	name := strings.TrimLeft(filepath.ToSlash(value), "/")
	if name == "" {
		name = "."
	}
	root, err := os.OpenRoot(rootName)
	if err != nil {
		return filesystemPath{}, fmt.Errorf("filesystem root: %w", err)
	}
	return filesystemPath{root: root, name: filepath.FromSlash(name)}, nil
}

func (backend *Filesystem) displayPath(value filesystemPath, name string) string {
	if value.root == nil {
		return name
	}
	clean := path.Clean(filepath.ToSlash(name))
	if clean == "." {
		return "/"
	}
	return "/" + strings.TrimPrefix(clean, "/")
}

func (backend *Filesystem) localHostRoot() string {
	root, err := backend.currentRoot()
	if err != nil {
		return backend.root
	}
	return root
}

func (backend *Filesystem) currentRoot() (string, error) {
	root := backend.root
	if backend.getRoot != nil {
		root = backend.getRoot()
		if root == "" {
			return "", fmt.Errorf("filesystem root provider returned an empty path")
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("filesystem root: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func NewFilesystem(options FilesystemOptions) (*Filesystem, error) {
	root := options.Root
	if options.GetRoot != nil {
		root = options.GetRoot()
	}
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
	if options.MaxVideoSize <= 0 {
		options.MaxVideoSize = 1 << 30
	}
	return &Filesystem{root: filepath.Clean(resolved), getRoot: options.GetRoot, virtual: !options.AllowHostPaths, maxFileSize: options.MaxFileSize, maxVideoSize: options.MaxVideoSize, maxResults: options.MaxResults}, nil
}

func (backend *Filesystem) List(ctx context.Context, path string) (ListResult, error) {
	opened, err := backend.openPath(path)
	if err != nil {
		return ListResult{}, err
	}
	defer opened.Close()
	var entries []os.DirEntry
	if opened.root != nil {
		directory, openErr := opened.root.Open(opened.name)
		if openErr != nil {
			return ListResult{}, normalizeFileError(path, openErr)
		}
		entries, err = directory.ReadDir(-1)
		_ = directory.Close()
	} else {
		entries, err = os.ReadDir(opened.host)
	}
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
		name := filepath.Join(opened.name, entry.Name())
		if opened.root == nil {
			name = filepath.Join(opened.host, entry.Name())
		}
		display := backend.displayPath(opened, name)
		if info.IsDir() {
			display += "/"
		}
		result.Entries = append(result.Entries, FileInfo{Path: display, IsDir: info.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
}

func (backend *Filesystem) Read(ctx context.Context, path string, offset, limit int) (ReadResult, error) {
	opened, err := backend.openPath(path)
	if err != nil {
		return ReadResult{}, err
	}
	defer opened.Close()
	file, err := openFilesystemFile(opened)
	if err != nil {
		return ReadResult{}, normalizeFileError(path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ReadResult{}, normalizeFileError(path, err)
	}
	if info.IsDir() {
		return ReadResult{}, normalizeFileError(path, fmt.Errorf("is a directory"))
	}
	if IsBinaryReadPath(path) {
		maxBytes := int64(0)
		if IsVideoReadPath(path) {
			maxBytes = backend.maxVideoSize
		}
		return readFilesystemBinary(ctx, file, info, maxBytes)
	}
	result, binary, err := readFilesystemTextPage(ctx, file, info, offset, limit)
	if err != nil {
		return ReadResult{}, normalizeFileError(path, err)
	}
	if !binary {
		return result, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ReadResult{}, normalizeFileError(path, err)
	}
	return readFilesystemBinary(ctx, file, info, 0)
}

// ReadBinary reads one binary value with a caller-selected allocation bound.
func (backend *Filesystem) ReadBinary(ctx context.Context, name string, maxBytes int64) (ReadResult, error) {
	opened, err := backend.openPath(name)
	if err != nil {
		return ReadResult{}, err
	}
	defer opened.Close()
	file, err := openFilesystemFile(opened)
	if err != nil {
		return ReadResult{}, normalizeFileError(name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ReadResult{}, normalizeFileError(name, err)
	}
	if info.IsDir() {
		return ReadResult{}, normalizeFileError(name, fmt.Errorf("is a directory"))
	}
	return readFilesystemBinary(ctx, file, info, maxBytes)
}

func (backend *Filesystem) Write(ctx context.Context, path, content string) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	opened, err := backend.openPath(path)
	if err != nil {
		return WriteResult{}, err
	}
	defer opened.Close()
	if opened.root != nil {
		err = opened.root.MkdirAll(filepath.Dir(opened.name), 0o755)
		if err == nil {
			err = opened.root.WriteFile(opened.name, []byte(content), 0o644)
		}
	} else {
		err = os.MkdirAll(filepath.Dir(opened.host), 0o755)
		if err == nil {
			err = os.WriteFile(opened.host, []byte(content), 0o644)
		}
	}
	if err != nil {
		return WriteResult{}, normalizeFileError(path, err)
	}
	name := opened.name
	if opened.root == nil {
		name = opened.host
	}
	return WriteResult{Path: backend.displayPath(opened, name)}, nil
}

func (backend *Filesystem) Edit(ctx context.Context, path, old, replacement string, replaceAll bool) (EditResult, error) {
	if old == "" || old == replacement {
		return EditResult{}, fmt.Errorf("edit %q: old string must be non-empty and differ from replacement", path)
	}
	opened, err := backend.openPath(path)
	if err != nil {
		return EditResult{}, err
	}
	defer opened.Close()
	data, info, err := readFilesystemFile(ctx, opened, backend.maxFileSize)
	if err != nil {
		return EditResult{}, normalizeFileError(path, err)
	}
	if !utf8.Valid(data) {
		return EditResult{}, fmt.Errorf("edit %q: binary files are unsupported", path)
	}
	updated, count, err := ReplaceText(path, string(data), old, replacement, replaceAll)
	if err != nil {
		return EditResult{}, err
	}
	if opened.root != nil {
		err = opened.root.WriteFile(opened.name, []byte(updated), info.Mode().Perm())
	} else {
		err = os.WriteFile(opened.host, []byte(updated), info.Mode().Perm())
	}
	if err != nil {
		return EditResult{}, normalizeFileError(path, err)
	}
	name := opened.name
	if opened.root == nil {
		name = opened.host
	}
	return EditResult{Path: backend.displayPath(opened, name), Occurrences: count}, nil
}

func (backend *Filesystem) Delete(ctx context.Context, path string) (DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}
	opened, err := backend.openPath(path)
	if err != nil {
		return DeleteResult{}, err
	}
	defer opened.Close()
	if opened.root != nil && opened.name == "." {
		return DeleteResult{}, fmt.Errorf("delete virtual root is not allowed")
	}
	if opened.root != nil {
		_, err = opened.root.Lstat(opened.name)
	} else {
		_, err = os.Lstat(opened.host)
	}
	if err != nil {
		return DeleteResult{}, normalizeFileError(path, err)
	}
	if opened.root != nil {
		err = opened.root.RemoveAll(opened.name)
	} else {
		err = os.RemoveAll(opened.host)
	}
	if err != nil {
		return DeleteResult{}, normalizeFileError(path, err)
	}
	name := opened.name
	if opened.root == nil {
		name = opened.host
	}
	return DeleteResult{Path: backend.displayPath(opened, name)}, nil
}

func (backend *Filesystem) Glob(ctx context.Context, pattern, base string) (GlobResult, error) {
	if base == "" {
		base = "/"
	}
	opened, err := backend.openPath(base)
	if err != nil {
		return GlobResult{}, err
	}
	defer opened.Close()
	matcher, err := compileGlob(pattern)
	if err != nil {
		return GlobResult{}, err
	}
	result := GlobResult{}
	walkRoot := opened.host
	if opened.root != nil {
		walkRoot = filepath.ToSlash(opened.name)
	}
	err = walkFilesystem(opened, walkRoot, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filePath == walkRoot {
			return nil
		}
		relative, _ := filepath.Rel(walkRoot, filePath)
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
		display := backend.displayPath(opened, filePath)
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
	if err := ValidateGrepOptions(options); err != nil {
		return GrepResult{}, err
	}
	if pattern == "" {
		return GrepResult{}, fmt.Errorf("grep pattern is required")
	}
	base := options.Path
	if base == "" {
		base = "/"
	}
	opened, err := backend.openPath(base)
	if err != nil {
		return GrepResult{}, err
	}
	defer opened.Close()
	var matcher *regexp.Regexp
	if options.Glob != "" {
		matcher, err = compileIncludeGlob(options.Glob)
		if err != nil {
			return GrepResult{}, err
		}
	}
	limit := options.MaxCount
	if options.Uncapped {
		limit = int(^uint(0) >> 1)
	} else if limit <= 0 {
		limit = backend.maxResults
	}
	result := GrepResult{}
	walkRoot := opened.host
	if opened.root != nil {
		walkRoot = filepath.ToSlash(opened.name)
	}
	err = walkFilesystem(opened, walkRoot, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, _ := filepath.Rel(walkRoot, filePath)
		if matcher != nil && !matchIncludeGlob(matcher, options.Glob, filepath.ToSlash(relative)) {
			return nil
		}
		file := filesystemPath{root: opened.root, name: filepath.FromSlash(filePath), host: filePath}
		data, _, err := readFilesystemFile(ctx, file, backend.maxFileSize)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if !strings.Contains(err.Error(), "file exceeds") {
				result.Error = AppendGrepError(result.Error, fmt.Sprintf("read %s: filesystem_error", backend.displayPath(opened, filePath)))
			}
			return nil
		}
		if !utf8.Valid(data) {
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
			item := GrepMatch{Path: backend.displayPath(opened, filePath), Line: index + 1, Text: lines[index]}
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
		if errors.Is(err, ctx.Err()) {
			return GrepResult{}, err
		}
		result.Error = AppendGrepError(result.Error, grepWalkError(base, err))
		return result, nil
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Path != result.Matches[j].Path {
			return result.Matches[i].Path < result.Matches[j].Path
		}
		return result.Matches[i].Line < result.Matches[j].Line
	})
	return result, nil
}

func (backend *Filesystem) Upload(ctx context.Context, uploads []Upload) []UploadResult {
	result := make([]UploadResult, len(uploads))
	for index, upload := range uploads {
		result[index].Path = upload.Path
		if err := ctx.Err(); err != nil {
			result[index].Error = err.Error()
			continue
		}
		opened, err := backend.openPath(upload.Path)
		if err == nil {
			if opened.root != nil {
				err = opened.root.MkdirAll(filepath.Dir(opened.name), 0o755)
				if err == nil {
					err = opened.root.WriteFile(opened.name, upload.Content, 0o644)
				}
			} else {
				err = os.MkdirAll(filepath.Dir(opened.host), 0o755)
				if err == nil {
					err = os.WriteFile(opened.host, upload.Content, 0o644)
				}
			}
		}
		opened.Close()
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
		opened, err := backend.openPath(path)
		var info os.FileInfo
		if err == nil {
			if opened.root != nil {
				info, err = opened.root.Stat(opened.name)
			} else {
				info, err = os.Stat(opened.host)
			}
		}
		if err == nil && info.IsDir() {
			result[index].Error = "is_directory"
			opened.Close()
			continue
		}
		if err == nil {
			if opened.root != nil {
				result[index].Content, err = opened.root.ReadFile(opened.name)
			} else {
				result[index].Content, err = os.ReadFile(opened.host)
			}
		}
		opened.Close()
		if err != nil {
			result[index].Error = normalizedCode(err)
		}
	}
	return result
}

func readFilesystemFile(ctx context.Context, file filesystemPath, maxSize int64) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var info os.FileInfo
	var err error
	if file.root != nil {
		info, err = file.root.Stat(file.name)
	} else {
		info, err = os.Stat(file.host)
	}
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("is a directory")
	}
	if info.Size() > maxSize {
		return nil, nil, fmt.Errorf("file exceeds %d bytes", maxSize)
	}
	var data []byte
	if file.root != nil {
		opened, openErr := file.root.Open(file.name)
		if openErr != nil {
			return nil, nil, openErr
		}
		data, err = io.ReadAll(io.LimitReader(opened, maxSize+1))
		_ = opened.Close()
	} else {
		data, err = os.ReadFile(file.host)
	}
	if err == nil && int64(len(data)) > maxSize {
		return nil, nil, fmt.Errorf("file exceeds %d bytes", maxSize)
	}
	return data, info, err
}

func openFilesystemFile(file filesystemPath) (*os.File, error) {
	if file.root != nil {
		return file.root.Open(file.name)
	}
	return os.Open(file.host)
}

func walkFilesystem(file filesystemPath, root string, walk fs.WalkDirFunc) error {
	if file.root != nil {
		return fs.WalkDir(file.root.FS(), root, walk)
	}
	return filepath.WalkDir(root, walk)
}

func readFilesystemBinary(ctx context.Context, file *os.File, info os.FileInfo, maxBytes int64) (ReadResult, error) {
	if maxBytes > 0 && info.Size() > maxBytes {
		return ReadResult{}, fmt.Errorf("%w: media file exceeds %d bytes", ErrPayloadTooLarge, maxBytes)
	}
	reader := io.Reader(&contextReader{ctx: ctx, reader: file})
	if maxBytes > 0 {
		reader = io.LimitReader(reader, maxBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return ReadResult{}, err
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return ReadResult{}, fmt.Errorf("%w: media file exceeds %d bytes", ErrPayloadTooLarge, maxBytes)
	}
	fileData := FileData{
		Content: base64.StdEncoding.EncodeToString(data), Encoding: EncodingBase64,
		CreatedAt: info.ModTime().UTC(), ModifiedAt: info.ModTime().UTC(),
	}
	return ReadResult{Data: &fileData}, nil
}

func readFilesystemTextPage(ctx context.Context, file *os.File, info os.FileInfo, offset, limit int) (ReadResult, bool, error) {
	if offset < 0 {
		offset = 0
	}
	reader := bufio.NewReader(&contextReader{ctx: ctx, reader: file})
	var page, blank strings.Builder
	line, total := 0, 0
	lineOpen, pendingCR, blankOnly := false, false, true
	appendPage := func(value string) {
		if limit > 0 && line >= offset && line < offset+limit {
			page.WriteString(value)
		}
	}
	finishLine := func() {
		total++
		line++
		lineOpen = false
	}
	for {
		r, size, err := reader.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ReadResult{}, false, err
		}
		if r == utf8.RuneError && size == 1 {
			return ReadResult{}, true, nil
		}
		if blankOnly {
			blank.WriteRune(r)
			if !unicode.IsSpace(r) {
				blankOnly = false
				blank.Reset()
			}
		}
		if pendingCR {
			appendPage("\n")
			finishLine()
			pendingCR = false
			if r == '\n' {
				continue
			}
		}
		switch r {
		case '\r':
			lineOpen = true
			pendingCR = true
		case '\n':
			appendPage("\n")
			finishLine()
		default:
			lineOpen = true
			appendPage(string(r))
		}
	}
	if pendingCR {
		appendPage("\n")
		finishLine()
	} else if lineOpen {
		finishLine()
	}
	fileData := FileData{Encoding: EncodingUTF8, CreatedAt: info.ModTime().UTC(), ModifiedAt: info.ModTime().UTC()}
	if blankOnly {
		fileData.Content = blank.String()
		return ReadResult{Data: &fileData}, false, nil
	}
	if limit <= 0 {
		return ReadResult{Data: &fileData, NoLinesRequested: true}, false, nil
	}
	if offset >= total {
		return ReadResult{}, false, fmt.Errorf("line offset %d exceeds file length (%d lines)", offset, total)
	}
	end := min(total, offset+limit)
	fileData.Content = page.String()
	startLine, endLine := offset+1, end
	result := ReadResult{Data: &fileData, TotalLines: &total, StartLine: &startLine, EndLine: &endLine}
	if end < total {
		next := end
		result.NextOffset = &next
	}
	return result, false, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

func grepWalkError(base string, err error) string {
	code := "filesystem_error"
	switch {
	case errors.Is(err, os.ErrNotExist):
		code = "file_not_found"
	case errors.Is(err, os.ErrPermission):
		code = "permission_denied"
	}
	return fmt.Sprintf("grep %q stopped early: %s", base, code)
}

func splitLines(value string) []string {
	value = NormalizeNewlines(value)
	if value == "" {
		return nil
	}
	value = strings.TrimSuffix(value, "\n")
	if value == "" {
		return []string{""}
	}
	return strings.Split(value, "\n")
}

// SliceRead applies the canonical text pagination contract shared by every
// backend. Binary data must be returned by the caller without pagination.
func SliceRead(data FileData, offset, limit int) (ReadResult, error) {
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
	normalized := NormalizeNewlines(data.Content)
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

// NormalizeNewlines applies Python universal-newline semantics.
func NormalizeNewlines(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

// ReplaceText performs the backend edit contract consistently across storage
// implementations, including newline normalization and actionable EOF errors.
func ReplaceText(name, content, old, replacement string, replaceAll bool) (string, int, error) {
	if old == "" || old == replacement {
		return "", 0, fmt.Errorf("edit %q: old string must be non-empty and differ from replacement", name)
	}
	content = NormalizeNewlines(content)
	old = NormalizeNewlines(old)
	replacement = NormalizeNewlines(replacement)
	count := strings.Count(content, old)
	if count == 0 {
		return "", 0, editNotFoundError(name, content, old)
	}
	if !replaceAll && count != 1 {
		return "", 0, fmt.Errorf("edit %q: old string occurs %d times", name, count)
	}
	limit := 1
	if replaceAll {
		limit = -1
	} else {
		count = 1
	}
	return strings.Replace(content, old, replacement, limit), count, nil
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

// IsBinaryReadPath reports paths that are always routed as media or binary
// data, even when their bytes happen to be valid UTF-8.
func IsBinaryReadPath(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpeg", ".jpg", ".webp", ".gif", ".heic", ".heif",
		".mp4", ".mpeg", ".mov", ".avi", ".flv", ".mpg", ".webm", ".wmv", ".3gpp", ".mkv",
		".wav", ".mp3", ".aiff", ".aac", ".ogg", ".flac", ".pdf", ".ppt", ".pptx":
		return true
	default:
		return false
	}
}

// IsVideoReadPath reports paths whose payload must be size-checked before a
// video extractor receives it.
func IsVideoReadPath(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4", ".mpeg", ".mov", ".avi", ".flv", ".mpg", ".webm", ".wmv", ".3gpp", ".mkv":
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
