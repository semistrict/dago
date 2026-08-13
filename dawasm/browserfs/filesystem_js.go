//go:build js && wasm

// Package browserfs provides a bounded browser filesystem backed by the File
// System Access API when a directory is selected and by record-oriented
// browser storage otherwise.
package browserfs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall/js"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dawasm/jsbridge"
)

const (
	browserWorkspaceRoot    = "/workspace"
	browserMaxEntries       = 20_000
	browserMaxTraversal     = 20_000
	browserMaxFileBytes     = 8 << 20
	browserMaxVirtualBytes  = 128 << 20
	browserMaxSearchResults = 1_000
)

// Store persists virtual workspace records outside the Go heap. Implementations
// must keep records independently addressable; Metadata must not read file
// bodies.
type Store interface {
	Execute(context.Context, string, []byte) ([]byte, error)
}

// Filesystem is a browser-owned workspace shared by agent file tools and a
// browser shell. Selected directory contents are loaded only when read.
type Filesystem struct {
	mu           sync.RWMutex
	store        Store
	root         js.Value
	connected    bool
	external     map[string]browserEntry
	virtual      map[string]browserEntry
	virtualBytes int64
	nextIdentity uint64
}

type browserEntry struct {
	Path       string
	IsDir      bool
	Size       int64
	Mode       uint32
	ModifiedAt time.Time
	Identity   string
	Handle     js.Value
}

type storedBrowserRecord struct {
	Path      string              `json:"path"`
	Value     *dabackend.FileData `json:"value,omitempty"`
	Directory bool                `json:"directory,omitempty"`
	Mode      uint32              `json:"mode,omitempty"`
	MTime     string              `json:"mtime,omitempty"`
	Size      int64               `json:"size,omitempty"`
}

// DirectoryInfo describes the bounded path index built for a selected browser
// directory. File bodies are not read while producing this value.
type DirectoryInfo struct {
	Name         string `json:"name"`
	FileCount    int    `json:"fileCount"`
	SkippedCount int    `json:"skippedCount"`
}

// New constructs a browser filesystem and restores only virtual-file metadata.
func New(ctx context.Context, store Store) (*Filesystem, error) {
	filesystem := &Filesystem{
		store: store, external: map[string]browserEntry{}, virtual: map[string]browserEntry{},
	}
	filesystem.virtual[browserWorkspaceRoot] = filesystem.newEntry(browserWorkspaceRoot, true, 0, js.Undefined())
	raw, err := store.Execute(ctx, "metadata", nil)
	if err != nil {
		return nil, fmt.Errorf("load browser filesystem metadata: %w", err)
	}
	var records []storedBrowserRecord
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &records); err != nil {
			return nil, fmt.Errorf("decode browser filesystem metadata: %w", err)
		}
	}
	for _, record := range records {
		normalized, normalizeErr := normalizeBrowserPath(record.Path)
		if normalizeErr != nil || normalized == browserWorkspaceRoot {
			continue
		}
		entry := filesystem.newEntry(normalized, record.Directory, record.Size, js.Undefined())
		entry.Mode = record.Mode
		if entry.Mode == 0 {
			if entry.IsDir {
				entry.Mode = 0o755
			} else {
				entry.Mode = 0o644
			}
		}
		if parsed, parseErr := time.Parse(time.RFC3339Nano, record.MTime); parseErr == nil {
			entry.ModifiedAt = parsed
		}
		filesystem.virtual[normalized] = entry
		if !entry.IsDir {
			filesystem.virtualBytes += entry.Size
		}
	}
	if len(filesystem.virtual) == 1 {
		if _, err := filesystem.Write(ctx, browserWorkspaceRoot+"/README.md", "# Browser workspace\n\nFiles created by the agent are stored in this browser.\n"); err != nil {
			return nil, err
		}
	}
	return filesystem, nil
}

func (filesystem *Filesystem) newEntry(filePath string, directory bool, size int64, handle js.Value) browserEntry {
	filesystem.nextIdentity++
	mode := uint32(0o644)
	if directory {
		mode = 0o755
	}
	return browserEntry{
		Path: filePath, IsDir: directory, Size: size, Mode: mode,
		ModifiedAt: time.Now().UTC(), Identity: fmt.Sprintf("browser-entry-%d", filesystem.nextIdentity), Handle: handle,
	}
}

func normalizeBrowserPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("invalid browser path %q", value)
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == ".." {
			return "", fmt.Errorf("path traversal is not allowed: %q", value)
		}
	}
	normalized := path.Clean("/" + strings.TrimPrefix(value, "/"))
	if normalized != browserWorkspaceRoot && !strings.HasPrefix(normalized, browserWorkspaceRoot+"/") {
		return "", fmt.Errorf("browser path must be inside %s: %q", browserWorkspaceRoot, value)
	}
	return normalized, nil
}

func (filesystem *Filesystem) activeEntries() map[string]browserEntry {
	if filesystem.connected {
		return filesystem.external
	}
	return filesystem.virtual
}

func (filesystem *Filesystem) Connect(ctx context.Context, handle js.Value) (DirectoryInfo, error) {
	if handle.Type() != js.TypeObject || handle.Get("kind").String() != "directory" {
		return DirectoryInfo{}, fmt.Errorf("browser directory handle is required")
	}
	entries := map[string]browserEntry{
		browserWorkspaceRoot: filesystem.newEntry(browserWorkspaceRoot, true, 0, handle),
	}
	fileCount, skipped := 0, 0
	var walk func(js.Value, string, int) error
	walk = func(directory js.Value, prefix string, depth int) error {
		if depth > 64 {
			skipped++
			return nil
		}
		iterator := directory.Call("entries")
		for {
			next, err := awaitJSValue(ctx, iterator.Call("next"), "enumerate browser directory")
			if err != nil {
				return err
			}
			if next.Get("done").Bool() {
				break
			}
			pair := next.Get("value")
			name, child := pair.Index(0).String(), pair.Index(1)
			if len(entries) >= browserMaxEntries {
				skipped++
				continue
			}
			childPath := path.Join(prefix, name)
			directoryChild := child.Get("kind").String() == "directory"
			entries[childPath] = filesystem.newEntry(childPath, directoryChild, 0, child)
			if directoryChild {
				if err := walk(child, childPath, depth+1); err != nil {
					return err
				}
			} else {
				fileCount++
			}
		}
		return nil
	}
	if err := walk(handle, browserWorkspaceRoot, 0); err != nil {
		return DirectoryInfo{}, err
	}
	filesystem.mu.Lock()
	filesystem.root = handle
	filesystem.connected = true
	filesystem.external = entries
	filesystem.mu.Unlock()
	return DirectoryInfo{Name: handle.Get("name").String(), FileCount: fileCount, SkippedCount: skipped}, nil
}

func (filesystem *Filesystem) Disconnect() {
	filesystem.mu.Lock()
	filesystem.root = js.Undefined()
	filesystem.connected = false
	filesystem.external = map[string]browserEntry{}
	filesystem.mu.Unlock()
}

func (filesystem *Filesystem) List(ctx context.Context, directory string) (dabackend.ListResult, error) {
	directory, err := normalizeBrowserPath(directory)
	if err != nil {
		return dabackend.ListResult{}, err
	}
	if filesystem.isConnected() {
		if err := filesystem.refreshExternalDirectory(ctx, directory); err != nil {
			return dabackend.ListResult{}, err
		}
	}
	filesystem.mu.RLock()
	entries := filesystem.activeEntries()
	directoryEntry, exists := entries[directory]
	if !exists || !directoryEntry.IsDir {
		filesystem.mu.RUnlock()
		return dabackend.ListResult{}, fmt.Errorf("path %q: directory not found", directory)
	}
	prefix := strings.TrimSuffix(directory, "/") + "/"
	result := dabackend.ListResult{}
	for candidate, entry := range entries {
		if !strings.HasPrefix(candidate, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(candidate, prefix)
		if remainder == "" || strings.Contains(remainder, "/") {
			continue
		}
		display := candidate
		if entry.IsDir {
			display += "/"
		}
		result.Entries = append(result.Entries, dabackend.FileInfo{
			Path: display, IsDir: entry.IsDir, Size: entry.Size, ModifiedAt: entry.ModifiedAt,
		})
	}
	filesystem.mu.RUnlock()
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
}

func (filesystem *Filesystem) Read(ctx context.Context, filePath string, offset, limit int) (dabackend.ReadResult, error) {
	filePath, err := normalizeBrowserPath(filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	data, modified, err := filesystem.readBytes(ctx, filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	file := dabackend.FileData{CreatedAt: modified, ModifiedAt: modified}
	if dabackend.IsBinaryReadPath(filePath) || !utf8.Valid(data) {
		file.Content = base64.StdEncoding.EncodeToString(data)
		file.Encoding = dabackend.EncodingBase64
		return dabackend.ReadResult{Data: &file}, nil
	}
	file.Content = string(data)
	file.Encoding = dabackend.EncodingUTF8
	return dabackend.SliceRead(file, offset, limit)
}

func (filesystem *Filesystem) Write(ctx context.Context, filePath, content string) (dabackend.WriteResult, error) {
	filePath, err := normalizeBrowserPath(filePath)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	if err := filesystem.writeBytes(ctx, filePath, []byte(content)); err != nil {
		return dabackend.WriteResult{}, err
	}
	return dabackend.WriteResult{Path: filePath}, nil
}

func (filesystem *Filesystem) Edit(ctx context.Context, filePath, old, replacement string, replaceAll bool) (dabackend.EditResult, error) {
	read, err := filesystem.Read(ctx, filePath, 0, int(^uint(0)>>1))
	if err != nil {
		return dabackend.EditResult{}, err
	}
	if read.Data == nil || read.Data.Encoding != dabackend.EncodingUTF8 {
		return dabackend.EditResult{}, fmt.Errorf("edit %q: binary files are unsupported", filePath)
	}
	updated, count, err := dabackend.ReplaceText(filePath, read.Data.Content, old, replacement, replaceAll)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	written, err := filesystem.Write(ctx, filePath, updated)
	return dabackend.EditResult{Path: written.Path, Occurrences: count}, err
}

func (filesystem *Filesystem) Delete(ctx context.Context, filePath string) (dabackend.DeleteResult, error) {
	filePath, err := normalizeBrowserPath(filePath)
	if err != nil {
		return dabackend.DeleteResult{}, err
	}
	if filePath == browserWorkspaceRoot {
		return dabackend.DeleteResult{}, fmt.Errorf("delete browser workspace root is not allowed")
	}
	if err := filesystem.remove(ctx, filePath, true); err != nil {
		return dabackend.DeleteResult{}, err
	}
	return dabackend.DeleteResult{Path: filePath}, nil
}

func (filesystem *Filesystem) Glob(ctx context.Context, pattern, base string) (dabackend.GlobResult, error) {
	if base == "" {
		base = browserWorkspaceRoot
	}
	base, err := normalizeBrowserPath(base)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	matcher, err := compileBrowserGlob(pattern, true)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	entries, truncated, err := filesystem.walk(ctx, base)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	prefix := strings.TrimSuffix(base, "/") + "/"
	result := dabackend.GlobResult{Truncated: truncated}
	for _, entry := range entries {
		relative := strings.TrimPrefix(entry.Path, prefix)
		if relative == entry.Path || !matcher.MatchString(relative) {
			continue
		}
		if len(result.Matches) >= browserMaxSearchResults {
			result.Truncated = true
			break
		}
		display := entry.Path
		if entry.IsDir {
			display += "/"
		}
		result.Matches = append(result.Matches, dabackend.FileInfo{
			Path: display, IsDir: entry.IsDir, Size: entry.Size, ModifiedAt: entry.ModifiedAt,
		})
	}
	sort.Slice(result.Matches, func(i, j int) bool { return result.Matches[i].Path < result.Matches[j].Path })
	return result, nil
}

func (filesystem *Filesystem) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	if pattern == "" {
		return dabackend.GrepResult{}, fmt.Errorf("grep pattern is required")
	}
	base := options.Path
	if base == "" {
		base = browserWorkspaceRoot
	}
	base, err := normalizeBrowserPath(base)
	if err != nil {
		return dabackend.GrepResult{}, err
	}
	var include *regexp.Regexp
	if options.Glob != "" {
		include, err = compileBrowserGlob(options.Glob, false)
		if err != nil {
			return dabackend.GrepResult{}, err
		}
	}
	entries, traversalTruncated, err := filesystem.walk(ctx, base)
	if err != nil {
		return dabackend.GrepResult{}, err
	}
	limit := options.MaxCount
	if limit <= 0 || limit > browserMaxSearchResults {
		limit = browserMaxSearchResults
	}
	prefix := strings.TrimSuffix(base, "/") + "/"
	result := dabackend.GrepResult{Truncated: traversalTruncated}
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		relative := strings.TrimPrefix(entry.Path, prefix)
		if include != nil && !include.MatchString(relative) {
			continue
		}
		data, _, readErr := filesystem.readBytes(ctx, entry.Path)
		if readErr != nil || !utf8.Valid(data) {
			continue
		}
		lines := splitBrowserLines(string(data))
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
				break
			}
			item := dabackend.GrepMatch{Path: entry.Path, Line: index + 1, Text: lines[index]}
			for before := max(0, index-options.ContextLines); before < index; before++ {
				if !matched[before] {
					item.ContextBefore = append(item.ContextBefore, dabackend.ContextLine{Line: before + 1, Text: lines[before]})
				}
			}
			for after := index + 1; after < min(len(lines), index+options.ContextLines+1); after++ {
				if !matched[after] {
					item.ContextAfter = append(item.ContextAfter, dabackend.ContextLine{Line: after + 1, Text: lines[after]})
				}
			}
			result.Matches = append(result.Matches, item)
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

func (filesystem *Filesystem) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	result := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		result[index].Path = upload.Path
		filePath, err := normalizeBrowserPath(upload.Path)
		if err == nil {
			err = filesystem.writeBytes(ctx, filePath, upload.Content)
		}
		if err != nil {
			result[index].Error = err.Error()
		}
	}
	return result
}

func (filesystem *Filesystem) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	result := make([]dabackend.DownloadResult, len(paths))
	for index, raw := range paths {
		result[index].Path = raw
		filePath, err := normalizeBrowserPath(raw)
		if err == nil {
			result[index].Content, _, err = filesystem.readBytes(ctx, filePath)
		}
		if err != nil {
			result[index].Error = err.Error()
		}
	}
	return result
}

func (filesystem *Filesystem) CreateDirectory(ctx context.Context, directory string) error {
	directory, err := normalizeBrowserPath(directory)
	if err != nil {
		return err
	}
	return filesystem.mkdir(ctx, directory, true)
}

func (filesystem *Filesystem) isConnected() bool {
	filesystem.mu.RLock()
	defer filesystem.mu.RUnlock()
	return filesystem.connected
}

func (filesystem *Filesystem) refreshExternalDirectory(ctx context.Context, directory string) error {
	filesystem.mu.RLock()
	entry, ok := filesystem.external[directory]
	filesystem.mu.RUnlock()
	if !ok {
		var err error
		entry, err = filesystem.resolveExternal(ctx, directory)
		if err != nil {
			return err
		}
	}
	if !entry.IsDir {
		return fmt.Errorf("path %q: not a directory", directory)
	}
	iterator := entry.Handle.Call("entries")
	seen := map[string]bool{}
	for {
		next, err := awaitJSValue(ctx, iterator.Call("next"), "list browser directory")
		if err != nil {
			return err
		}
		if next.Get("done").Bool() {
			break
		}
		pair := next.Get("value")
		name, handle := pair.Index(0).String(), pair.Index(1)
		childPath := path.Join(directory, name)
		seen[childPath] = true
		child := filesystem.newEntry(childPath, handle.Get("kind").String() == "directory", 0, handle)
		if !child.IsDir {
			file, fileErr := awaitJSValue(ctx, handle.Call("getFile"), "stat browser file")
			if fileErr != nil {
				return fileErr
			}
			child.Size = int64(file.Get("size").Float())
			child.ModifiedAt = time.UnixMilli(int64(file.Get("lastModified").Float())).UTC()
		}
		filesystem.mu.Lock()
		filesystem.external[childPath] = child
		filesystem.mu.Unlock()
	}
	filesystem.mu.Lock()
	prefix := strings.TrimSuffix(directory, "/") + "/"
	for candidate := range filesystem.external {
		remainder := strings.TrimPrefix(candidate, prefix)
		if remainder != candidate && remainder != "" && !strings.Contains(remainder, "/") && !seen[candidate] {
			delete(filesystem.external, candidate)
			removedPrefix := candidate + "/"
			for descendant := range filesystem.external {
				if strings.HasPrefix(descendant, removedPrefix) {
					delete(filesystem.external, descendant)
				}
			}
		}
	}
	filesystem.mu.Unlock()
	return nil
}

func (filesystem *Filesystem) resolveExternal(ctx context.Context, filePath string) (browserEntry, error) {
	filesystem.mu.RLock()
	if entry, ok := filesystem.external[filePath]; ok {
		filesystem.mu.RUnlock()
		return entry, nil
	}
	root := filesystem.root
	filesystem.mu.RUnlock()
	if root.Type() != js.TypeObject {
		return browserEntry{}, fmt.Errorf("browser directory is disconnected")
	}
	current, currentPath := root, browserWorkspaceRoot
	parts := strings.Split(strings.TrimPrefix(filePath, browserWorkspaceRoot+"/"), "/")
	for index, part := range parts {
		last := index == len(parts)-1
		if last {
			fileHandle, fileErr := awaitJSValue(ctx, current.Call("getFileHandle", part), "resolve browser file")
			if fileErr == nil {
				currentPath = path.Join(currentPath, part)
				entry := filesystem.newEntry(currentPath, false, 0, fileHandle)
				filesystem.mu.Lock()
				filesystem.external[currentPath] = entry
				filesystem.mu.Unlock()
				return entry, nil
			}
		}
		directory, err := awaitJSValue(ctx, current.Call("getDirectoryHandle", part), "resolve browser directory")
		if err != nil {
			return browserEntry{}, fmt.Errorf("path %q: file not found", filePath)
		}
		current = directory
		currentPath = path.Join(currentPath, part)
		entry := filesystem.newEntry(currentPath, true, 0, directory)
		filesystem.mu.Lock()
		filesystem.external[currentPath] = entry
		filesystem.mu.Unlock()
		if last {
			return entry, nil
		}
	}
	return browserEntry{}, fmt.Errorf("path %q: file not found", filePath)
}

func (filesystem *Filesystem) readBytes(ctx context.Context, filePath string) ([]byte, time.Time, error) {
	if filesystem.isConnected() {
		entry, err := filesystem.resolveExternal(ctx, filePath)
		if err != nil {
			return nil, time.Time{}, err
		}
		if entry.IsDir {
			return nil, time.Time{}, fmt.Errorf("path %q: is a directory", filePath)
		}
		file, err := awaitJSValue(ctx, entry.Handle.Call("getFile"), "read browser file")
		if err != nil {
			return nil, time.Time{}, err
		}
		size := int64(file.Get("size").Float())
		if size > browserMaxFileBytes {
			return nil, time.Time{}, fmt.Errorf("file %q exceeds %d bytes", filePath, browserMaxFileBytes)
		}
		buffer, err := awaitJSValue(ctx, file.Call("arrayBuffer"), "read browser file bytes")
		if err != nil {
			return nil, time.Time{}, err
		}
		data := make([]byte, int(size))
		js.CopyBytesToGo(data, js.Global().Get("Uint8Array").New(buffer))
		modified := time.UnixMilli(int64(file.Get("lastModified").Float())).UTC()
		return data, modified, nil
	}
	raw, err := filesystem.store.Execute(ctx, "get", []byte(fmt.Sprintf(`{"path":%q}`, filePath)))
	if err != nil {
		return nil, time.Time{}, err
	}
	var record storedBrowserRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.Value == nil {
		return nil, time.Time{}, fmt.Errorf("path %q: file not found", filePath)
	}
	var data []byte
	if record.Value.Encoding == dabackend.EncodingBase64 {
		data, err = base64.StdEncoding.DecodeString(record.Value.Content)
	} else {
		data = []byte(record.Value.Content)
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("decode browser file %q: %w", filePath, err)
	}
	if len(data) > browserMaxFileBytes {
		return nil, time.Time{}, fmt.Errorf("file %q exceeds %d bytes", filePath, browserMaxFileBytes)
	}
	modified, _ := time.Parse(time.RFC3339Nano, record.MTime)
	return data, modified, nil
}

func (filesystem *Filesystem) writeBytes(ctx context.Context, filePath string, data []byte) error {
	if len(data) > browserMaxFileBytes {
		return fmt.Errorf("file %q exceeds %d bytes", filePath, browserMaxFileBytes)
	}
	if filesystem.isConnected() {
		parent, err := filesystem.ensureExternalDirectory(ctx, path.Dir(filePath))
		if err != nil {
			return err
		}
		handle, err := awaitJSValue(ctx, parent.Call("getFileHandle", path.Base(filePath), mapToJS(map[string]any{"create": true})), "create browser file")
		if err != nil {
			return err
		}
		writable, err := awaitJSValue(ctx, handle.Call("createWritable"), "open browser file for writing")
		if err != nil {
			return err
		}
		bytes := js.Global().Get("Uint8Array").New(len(data))
		js.CopyBytesToJS(bytes, data)
		if _, err := awaitJSValue(ctx, writable.Call("write", bytes), "write browser file"); err != nil {
			writable.Call("abort")
			return err
		}
		if _, err := awaitJSValue(ctx, writable.Call("close"), "close browser file"); err != nil {
			return err
		}
		entry := filesystem.newEntry(filePath, false, int64(len(data)), handle)
		filesystem.mu.Lock()
		filesystem.external[filePath] = entry
		filesystem.mu.Unlock()
		return nil
	}
	filesystem.mu.RLock()
	previous := filesystem.virtual[filePath].Size
	newTotal := filesystem.virtualBytes - previous + int64(len(data))
	filesystem.mu.RUnlock()
	if newTotal > browserMaxVirtualBytes {
		return fmt.Errorf("browser workspace exceeds %d bytes", browserMaxVirtualBytes)
	}
	if err := filesystem.mkdir(ctx, path.Dir(filePath), true); err != nil {
		return err
	}
	now := time.Now().UTC()
	file := dabackend.FileData{CreatedAt: now, ModifiedAt: now}
	if utf8.Valid(data) {
		file.Content, file.Encoding = string(data), dabackend.EncodingUTF8
	} else {
		file.Content, file.Encoding = base64.StdEncoding.EncodeToString(data), dabackend.EncodingBase64
	}
	record := storedBrowserRecord{Path: filePath, Value: &file, Mode: 0o644, MTime: now.Format(time.RFC3339Nano), Size: int64(len(data))}
	payload, _ := json.Marshal(record)
	if _, err := filesystem.store.Execute(ctx, "put", payload); err != nil {
		return err
	}
	entry := filesystem.newEntry(filePath, false, int64(len(data)), js.Undefined())
	entry.ModifiedAt = now
	filesystem.mu.Lock()
	filesystem.virtualBytes = newTotal
	filesystem.virtual[filePath] = entry
	filesystem.mu.Unlock()
	return nil
}

func (filesystem *Filesystem) ensureExternalDirectory(ctx context.Context, directory string) (js.Value, error) {
	filesystem.mu.RLock()
	root := filesystem.root
	filesystem.mu.RUnlock()
	current, currentPath := root, browserWorkspaceRoot
	if directory == browserWorkspaceRoot {
		return current, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(directory, browserWorkspaceRoot+"/"), "/") {
		next, err := awaitJSValue(ctx, current.Call("getDirectoryHandle", part, mapToJS(map[string]any{"create": true})), "create browser directory")
		if err != nil {
			return js.Undefined(), err
		}
		current = next
		currentPath = path.Join(currentPath, part)
		filesystem.mu.Lock()
		filesystem.external[currentPath] = filesystem.newEntry(currentPath, true, 0, current)
		filesystem.mu.Unlock()
	}
	return current, nil
}

func (filesystem *Filesystem) mkdir(ctx context.Context, directory string, recursive bool) error {
	if directory == browserWorkspaceRoot {
		return nil
	}
	if filesystem.isConnected() {
		if !recursive {
			filesystem.mu.RLock()
			_, exists := filesystem.external[directory]
			filesystem.mu.RUnlock()
			if exists {
				return fmt.Errorf("path %q already exists", directory)
			}
		}
		_, err := filesystem.ensureExternalDirectory(ctx, directory)
		return err
	}
	parts := []string{}
	for current := directory; current != browserWorkspaceRoot; current = path.Dir(current) {
		parts = append(parts, current)
	}
	for index := len(parts) - 1; index >= 0; index-- {
		candidate := parts[index]
		filesystem.mu.RLock()
		_, exists := filesystem.virtual[candidate]
		filesystem.mu.RUnlock()
		if exists {
			continue
		}
		if !recursive && candidate != directory {
			return fmt.Errorf("parent directory %q does not exist", path.Dir(directory))
		}
		now := time.Now().UTC()
		record := storedBrowserRecord{Path: candidate, Directory: true, Mode: 0o755, MTime: now.Format(time.RFC3339Nano)}
		payload, _ := json.Marshal(record)
		if _, err := filesystem.store.Execute(ctx, "put", payload); err != nil {
			return err
		}
		entry := filesystem.newEntry(candidate, true, 0, js.Undefined())
		entry.ModifiedAt = now
		filesystem.mu.Lock()
		filesystem.virtual[candidate] = entry
		filesystem.mu.Unlock()
	}
	return nil
}

func (filesystem *Filesystem) remove(ctx context.Context, filePath string, recursive bool) error {
	filesystem.mu.RLock()
	entries := filesystem.activeEntries()
	entry, exists := entries[filePath]
	filesystem.mu.RUnlock()
	if !exists && filesystem.isConnected() {
		var err error
		entry, err = filesystem.resolveExternal(ctx, filePath)
		if err == nil {
			exists = true
		}
	}
	if !exists {
		return fmt.Errorf("path %q: file not found", filePath)
	}
	prefix := strings.TrimSuffix(filePath, "/") + "/"
	filesystem.mu.RLock()
	children := []string{}
	for candidate := range filesystem.activeEntries() {
		if candidate == filePath || strings.HasPrefix(candidate, prefix) {
			children = append(children, candidate)
		}
	}
	filesystem.mu.RUnlock()
	if entry.IsDir && len(children) > 1 && !recursive {
		return fmt.Errorf("directory %q is not empty", filePath)
	}
	if filesystem.isConnected() {
		parent, err := filesystem.resolveExternal(ctx, path.Dir(filePath))
		if err != nil {
			return err
		}
		_, err = awaitJSValue(ctx, parent.Handle.Call("removeEntry", path.Base(filePath), mapToJS(map[string]any{"recursive": recursive})), "remove browser path")
		if err != nil {
			return err
		}
		filesystem.mu.Lock()
		for _, candidate := range children {
			delete(filesystem.external, candidate)
		}
		filesystem.mu.Unlock()
		return nil
	}
	for _, candidate := range children {
		payload, _ := json.Marshal(map[string]string{"path": candidate})
		if _, err := filesystem.store.Execute(ctx, "delete", payload); err != nil {
			return err
		}
	}
	filesystem.mu.Lock()
	for _, candidate := range children {
		if item := filesystem.virtual[candidate]; !item.IsDir {
			filesystem.virtualBytes -= item.Size
		}
		delete(filesystem.virtual, candidate)
	}
	filesystem.mu.Unlock()
	return nil
}

func (filesystem *Filesystem) walk(ctx context.Context, root string) ([]browserEntry, bool, error) {
	queue := []string{root}
	result := []browserEntry{}
	visited := 0
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if visited >= browserMaxTraversal {
			return result, true, nil
		}
		current := queue[0]
		queue = queue[1:]
		listed, err := filesystem.List(ctx, current)
		if err != nil {
			return nil, false, err
		}
		for _, info := range listed.Entries {
			candidate := strings.TrimSuffix(info.Path, "/")
			entry := browserEntry{Path: candidate, IsDir: info.IsDir, Size: info.Size, ModifiedAt: info.ModifiedAt}
			result = append(result, entry)
			visited++
			if info.IsDir {
				queue = append(queue, candidate)
			}
		}
	}
	return result, false, nil
}

func (filesystem *Filesystem) copy(ctx context.Context, source, destination string, recursive bool) error {
	source, err := normalizeBrowserPath(source)
	if err != nil {
		return err
	}
	destination, err = normalizeBrowserPath(destination)
	if err != nil {
		return err
	}
	filesystem.mu.RLock()
	entry, exists := filesystem.activeEntries()[source]
	filesystem.mu.RUnlock()
	if !exists && filesystem.isConnected() {
		entry, err = filesystem.resolveExternal(ctx, source)
		exists = err == nil
	}
	if !exists {
		return fmt.Errorf("path %q: file not found", source)
	}
	if !entry.IsDir {
		data, _, err := filesystem.readBytes(ctx, source)
		if err != nil {
			return err
		}
		return filesystem.writeBytes(ctx, destination, data)
	}
	if !recursive {
		return fmt.Errorf("copy directory %q requires recursive mode", source)
	}
	if destination == source || strings.HasPrefix(destination, source+"/") {
		return fmt.Errorf("cannot copy directory %q into itself", source)
	}
	if err := filesystem.mkdir(ctx, destination, true); err != nil {
		return err
	}
	listed, err := filesystem.List(ctx, source)
	if err != nil {
		return err
	}
	for _, child := range listed.Entries {
		name := path.Base(strings.TrimSuffix(child.Path, "/"))
		if err := filesystem.copy(ctx, strings.TrimSuffix(child.Path, "/"), path.Join(destination, name), recursive); err != nil {
			return err
		}
	}
	return nil
}

func (filesystem *Filesystem) Paths() []string {
	filesystem.mu.RLock()
	defer filesystem.mu.RUnlock()
	result := make([]string, 0, len(filesystem.activeEntries()))
	for filePath := range filesystem.activeEntries() {
		result = append(result, filePath)
	}
	sort.Strings(result)
	return result
}

func (filesystem *Filesystem) ExecuteJS(ctx context.Context, operation string, payload []byte) ([]byte, error) {
	var input struct {
		Path        string `json:"path"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Content     string `json:"content"`
		Recursive   bool   `json:"recursive"`
		Force       bool   `json:"force"`
		Mode        uint32 `json:"mode"`
		MTime       string `json:"mtime"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &input); err != nil {
			return nil, err
		}
	}
	switch operation {
	case "read_file":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		data, _, err := filesystem.readBytes(ctx, filePath)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"content": base64.StdEncoding.EncodeToString(data)})
	case "write_file":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(input.Content)
		if err != nil {
			return nil, err
		}
		return nil, filesystem.writeBytes(ctx, filePath, data)
	case "append_file":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		addition, err := base64.StdEncoding.DecodeString(input.Content)
		if err != nil {
			return nil, err
		}
		current, _, readErr := filesystem.readBytes(ctx, filePath)
		if readErr != nil {
			current = nil
		}
		return nil, filesystem.writeBytes(ctx, filePath, append(current, addition...))
	case "exists":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		filesystem.mu.RLock()
		_, exists := filesystem.activeEntries()[filePath]
		filesystem.mu.RUnlock()
		if !exists && filesystem.isConnected() {
			_, err = filesystem.resolveExternal(ctx, filePath)
			exists = err == nil
		}
		return json.Marshal(exists)
	case "stat", "lstat":
		return filesystem.jsStat(ctx, input.Path)
	case "mkdir":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		return nil, filesystem.mkdir(ctx, filePath, input.Recursive)
	case "readdir":
		listed, err := filesystem.List(ctx, input.Path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(listed.Entries)
	case "rm":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		err = filesystem.remove(ctx, filePath, input.Recursive)
		if err != nil && input.Force && strings.Contains(err.Error(), "file not found") {
			return nil, nil
		}
		return nil, err
	case "cp":
		return nil, filesystem.copy(ctx, input.Source, input.Destination, input.Recursive)
	case "mv":
		if err := filesystem.copy(ctx, input.Source, input.Destination, true); err != nil {
			return nil, err
		}
		return nil, filesystem.remove(ctx, input.Source, true)
	case "chmod":
		return nil, filesystem.setMode(input.Path, input.Mode)
	case "realpath":
		filePath, err := normalizeBrowserPath(input.Path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(filePath)
	case "utimes":
		return nil, filesystem.setMTime(input.Path, input.MTime)
	case "symlink", "link":
		return nil, fmt.Errorf("%s is unavailable in browser directories", operation)
	case "readlink":
		return nil, fmt.Errorf("path %q is not a symbolic link", input.Path)
	default:
		return nil, fmt.Errorf("unsupported browser filesystem operation %q", operation)
	}
}

func (filesystem *Filesystem) jsStat(ctx context.Context, raw string) ([]byte, error) {
	filePath, err := normalizeBrowserPath(raw)
	if err != nil {
		return nil, err
	}
	filesystem.mu.RLock()
	entry, exists := filesystem.activeEntries()[filePath]
	filesystem.mu.RUnlock()
	if !exists && filesystem.isConnected() {
		entry, err = filesystem.resolveExternal(ctx, filePath)
		exists = err == nil
	}
	if !exists {
		return nil, fmt.Errorf("path %q: file not found", filePath)
	}
	if filesystem.isConnected() && !entry.IsDir {
		file, fileErr := awaitJSValue(ctx, entry.Handle.Call("getFile"), "stat browser file")
		if fileErr != nil {
			return nil, fileErr
		}
		entry.Size = int64(file.Get("size").Float())
		entry.ModifiedAt = time.UnixMilli(int64(file.Get("lastModified").Float())).UTC()
	}
	return json.Marshal(map[string]any{
		"is_file": entry.IsDir == false, "is_directory": entry.IsDir, "is_symbolic_link": false,
		"mode": entry.Mode, "size": entry.Size, "mtime": entry.ModifiedAt.Format(time.RFC3339Nano), "identity": entry.Identity,
	})
}

func (filesystem *Filesystem) setMode(raw string, mode uint32) error {
	filePath, err := normalizeBrowserPath(raw)
	if err != nil {
		return err
	}
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	entries := filesystem.activeEntries()
	entry, exists := entries[filePath]
	if !exists {
		return fmt.Errorf("path %q: file not found", filePath)
	}
	entry.Mode = mode
	entries[filePath] = entry
	return nil
}

func (filesystem *Filesystem) setMTime(raw, value string) error {
	filePath, err := normalizeBrowserPath(raw)
	if err != nil {
		return err
	}
	modified, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return err
	}
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	entries := filesystem.activeEntries()
	entry, exists := entries[filePath]
	if !exists {
		return fmt.Errorf("path %q: file not found", filePath)
	}
	entry.ModifiedAt = modified
	entries[filePath] = entry
	return nil
}

func compileBrowserGlob(pattern string, anywhere bool) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob pattern is required")
	}
	pattern = strings.TrimPrefix(strings.ReplaceAll(pattern, "\\", "/"), "/")
	if anywhere && !strings.HasPrefix(pattern, "**/") {
		pattern = "**/" + pattern
	}
	var expression strings.Builder
	expression.WriteByte('^')
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
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteByte('$')
	return regexp.Compile(expression.String())
}

func splitBrowserLines(value string) []string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	value = strings.TrimSuffix(value, "\n")
	if value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

func awaitJSValue(ctx context.Context, promise js.Value, fallback string) (js.Value, error) {
	type outcome struct {
		value js.Value
		err   error
	}
	completed := make(chan outcome, 1)
	resolve := js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		value := js.Undefined()
		if len(arguments) > 0 {
			value = arguments[0]
		}
		completed <- outcome{value: value}
		return nil
	})
	reject := js.FuncOf(func(_ js.Value, arguments []js.Value) any {
		completed <- outcome{err: fmt.Errorf("%s", jsbridge.RejectionMessage(arguments, fallback))}
		return nil
	})
	promise.Call("then", resolve).Call("catch", reject)
	release := func() {
		resolve.Release()
		reject.Release()
	}
	select {
	case result := <-completed:
		release()
		return result.value, result.err
	case <-ctx.Done():
		go func() {
			<-completed
			release()
		}()
		return js.Undefined(), ctx.Err()
	}
}

func mapToJS(value map[string]any) js.Value {
	object := js.Global().Get("Object").New()
	for key, item := range value {
		object.Set(key, item)
	}
	return object
}
