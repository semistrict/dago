// Package langsmith adapts a LangSmith remote sandbox to dago's backend
// contracts. The sandbox must be created explicitly by the caller; this package
// never creates billable remote resources implicitly.
package langsmith

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	ls "github.com/langchain-ai/langsmith-go"
	"github.com/langchain-ai/langsmith-go/option"
	"github.com/semistrict/dago/dabackend"
)

type sandboxAPI interface {
	ReadFile(context.Context, string, ...option.RequestOption) ([]byte, error)
	WriteFile(context.Context, string, []byte, ...option.RequestOption) error
	Run(context.Context, ls.SandboxBoxRunParams, ...option.RequestOption) (*ls.SandboxExecutionResult, error)
}

// Options bounds remote reads, result counts, and output retained in memory.
type Options struct {
	MaxFileSize int
	MaxResults  int
	MaxOutput   int
}

// Backend wraps one existing LangSmith sandbox.
type Backend struct {
	id          string
	sandbox     sandboxAPI
	maxFileSize int
	maxResults  int
	maxOutput   int
}

// New wraps an existing SDK sandbox.
func New(sandbox *ls.Sandbox, options Options) (*Backend, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("langsmith backend: sandbox is required")
	}
	name := sandbox.Name
	if name == "" {
		name = sandbox.ID
	}
	return newBackend(name, sandbox, options)
}

func newBackend(id string, sandbox sandboxAPI, options Options) (*Backend, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("langsmith backend: sandbox is required")
	}
	if options.MaxFileSize <= 0 {
		options.MaxFileSize = 10 << 20
	}
	if options.MaxResults <= 0 {
		options.MaxResults = 1000
	}
	if options.MaxOutput <= 0 {
		options.MaxOutput = 1 << 20
	}
	return &Backend{id: id, sandbox: sandbox, maxFileSize: options.MaxFileSize, maxResults: options.MaxResults, maxOutput: options.MaxOutput}, nil
}

func (remote *Backend) ID() string { return remote.id }

func (remote *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	var selected *time.Duration
	if timeout > 0 {
		selected = &timeout
	}
	return remote.execute(ctx, command, selected)
}

func (remote *Backend) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	if options.Timeout != nil && *options.Timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("langsmith backend: execute timeout cannot be negative")
	}
	return remote.execute(ctx, command, options.Timeout)
}

func (remote *Backend) execute(ctx context.Context, command string, timeout *time.Duration) (dabackend.ExecuteResult, error) {
	if strings.TrimSpace(command) == "" {
		return dabackend.ExecuteResult{}, fmt.Errorf("langsmith backend: command is required")
	}
	params := ls.SandboxBoxRunParams{Command: ls.F(command)}
	if timeout != nil {
		seconds := int64((*timeout + time.Second - 1) / time.Second)
		params.Timeout = ls.F(seconds)
	}
	result, err := remote.sandbox.Run(ctx, params)
	if err != nil {
		return dabackend.ExecuteResult{}, fmt.Errorf("langsmith backend: execute: %w", err)
	}
	output := result.Stdout
	if result.Stderr != "" {
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}
		output += result.Stderr
	}
	truncated := false
	if len(output) > remote.maxOutput {
		output = output[:remote.maxOutput]
		truncated = true
	}
	exitCode := int(result.ExitCode)
	return dabackend.ExecuteResult{Output: output, ExitCode: &exitCode, Truncated: truncated}, nil
}

func (remote *Backend) List(ctx context.Context, filePath string) (dabackend.ListResult, error) {
	filePath, err := cleanPath(filePath)
	if err != nil {
		return dabackend.ListResult{}, err
	}
	entries, err := remote.find(ctx, filePath, false)
	if err != nil {
		return dabackend.ListResult{}, err
	}
	return dabackend.ListResult{Entries: entries}, nil
}

func (remote *Backend) Read(ctx context.Context, filePath string, offset, limit int) (dabackend.ReadResult, error) {
	filePath, err := cleanPath(filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	data, err := remote.readRaw(ctx, filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	fileData := dabackend.FileData{}
	if dabackend.IsBinaryReadPath(filePath) || !utf8.Valid(data) {
		if len(data) > dabackend.MaxSandboxBinaryPreviewBytes {
			return dabackend.ReadResult{}, fmt.Errorf("file %q: binary file exceeds maximum preview size of %d bytes", filePath, dabackend.MaxSandboxBinaryPreviewBytes)
		}
		fileData.Content = base64.StdEncoding.EncodeToString(data)
		fileData.Encoding = dabackend.EncodingBase64
		return dabackend.ReadResult{Data: &fileData}, nil
	}
	fileData.Content = string(data)
	fileData.Encoding = dabackend.EncodingUTF8
	return sliceRemoteRead(fileData, offset, limit)
}

func sliceRemoteRead(data dabackend.FileData, offset, limit int) (dabackend.ReadResult, error) {
	if data.Content == "" {
		return dabackend.ReadResult{Data: &data}, nil
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		data.Content = ""
		return dabackend.ReadResult{Data: &data, NoLinesRequested: true}, nil
	}
	lines := strings.Split(dabackend.NormalizeNewlines(data.Content), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if offset >= len(lines) {
		return dabackend.ReadResult{}, fmt.Errorf("line offset %d exceeds file length (%d lines)", offset, len(lines))
	}
	page := strings.Join(lines[offset:min(len(lines), offset+limit)], "\n")
	returnedLines := min(limit, len(lines)-offset)
	effectiveLimit := dabackend.MaxSandboxReadOutputBytes - len(dabackend.SandboxReadTruncationMessage)
	if len(page) > effectiveLimit {
		page = validUTF8Prefix(page, effectiveLimit)
		returnedLines = strings.Count(page, "\n")
		if returnedLines == 0 {
			returnedLines = 1
		}
		page += dabackend.SandboxReadTruncationMessage
	}
	data.Content = page
	start, end := offset+1, offset+returnedLines
	result := dabackend.ReadResult{Data: &data, TotalLines: intPointer(len(lines)), StartLine: &start, EndLine: &end}
	if end < len(lines) {
		result.NextOffset = &end
	}
	return result, nil
}

func validUTF8Prefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func intPointer(value int) *int { return &value }

func (remote *Backend) Write(ctx context.Context, filePath, content string) (dabackend.WriteResult, error) {
	filePath, err := cleanPath(filePath)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	if _, err := remote.runChecked(ctx, "mkdir -p -- "+shellQuote(path.Dir(filePath)), 30*time.Second); err != nil {
		return dabackend.WriteResult{}, err
	}
	if err := remote.sandbox.WriteFile(ctx, filePath, []byte(content)); err != nil {
		return dabackend.WriteResult{}, fmt.Errorf("langsmith backend: write %q: %w", filePath, err)
	}
	return dabackend.WriteResult{Path: filePath}, nil
}

func (remote *Backend) Edit(ctx context.Context, filePath, old, replacement string, replaceAll bool) (dabackend.EditResult, error) {
	if old == "" || old == replacement {
		return dabackend.EditResult{}, fmt.Errorf("edit %q: old string must be non-empty and differ from replacement", filePath)
	}
	filePath, err := cleanPath(filePath)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	data, err := remote.readRaw(ctx, filePath)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	if !utf8.Valid(data) {
		return dabackend.EditResult{}, fmt.Errorf("edit %q: binary files are unsupported", filePath)
	}
	updated, count, err := dabackend.ReplaceText(filePath, string(data), old, replacement, replaceAll)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	if err := remote.sandbox.WriteFile(ctx, filePath, []byte(updated)); err != nil {
		return dabackend.EditResult{}, fmt.Errorf("langsmith backend: edit %q: %w", filePath, err)
	}
	return dabackend.EditResult{Path: filePath, Occurrences: count}, nil
}

func (remote *Backend) Delete(ctx context.Context, filePath string) (dabackend.DeleteResult, error) {
	filePath, err := cleanPath(filePath)
	if err != nil {
		return dabackend.DeleteResult{}, err
	}
	if filePath == "/" {
		return dabackend.DeleteResult{}, fmt.Errorf("delete sandbox root is not allowed")
	}
	if _, err := remote.runChecked(ctx, "rm -rf -- "+shellQuote(filePath), 30*time.Second); err != nil {
		return dabackend.DeleteResult{}, err
	}
	return dabackend.DeleteResult{Path: filePath}, nil
}

func (remote *Backend) Glob(ctx context.Context, pattern, base string) (dabackend.GlobResult, error) {
	if base == "" {
		base = "/"
	}
	base, err := cleanPath(base)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	matcher, err := compileGlob(pattern)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	entries, err := remote.find(ctx, base, true)
	if err != nil {
		return dabackend.GlobResult{}, err
	}
	result := dabackend.GlobResult{}
	for _, entry := range entries {
		relative := strings.TrimPrefix(strings.TrimPrefix(entry.Path, base), "/")
		if !matcher.MatchString(relative) {
			continue
		}
		if len(result.Matches) == remote.maxResults {
			result.Truncated = true
			break
		}
		result.Matches = append(result.Matches, entry)
	}
	return result, nil
}

func (remote *Backend) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	if pattern == "" {
		return dabackend.GrepResult{}, fmt.Errorf("grep pattern is required")
	}
	base := options.Path
	if base == "" {
		base = "/"
	}
	files, err := remote.Glob(ctx, "**", base)
	if err != nil {
		return dabackend.GrepResult{}, err
	}
	var matcher *regexp.Regexp
	if options.Glob != "" {
		matcher, err = compileGlob(options.Glob)
		if err != nil {
			return dabackend.GrepResult{}, err
		}
	}
	limit := options.MaxCount
	if limit <= 0 {
		limit = remote.maxResults
	}
	result := dabackend.GrepResult{}
	for _, info := range files.Matches {
		if err := ctx.Err(); err != nil {
			return dabackend.GrepResult{}, err
		}
		if info.IsDir {
			continue
		}
		relative := strings.TrimPrefix(info.Path, "/")
		if matcher != nil && !matcher.MatchString(relative) && !matcher.MatchString(path.Base(relative)) {
			continue
		}
		data, readErr := remote.readRaw(ctx, info.Path)
		if readErr != nil || !utf8.Valid(data) {
			continue
		}
		lines := splitLines(string(data))
		matched := make([]bool, len(lines))
		for index := range lines {
			matched[index] = strings.Contains(lines[index], pattern)
		}
		for index, yes := range matched {
			if !yes {
				continue
			}
			if len(result.Matches) == limit {
				result.Truncated = true
				return result, nil
			}
			item := dabackend.GrepMatch{Path: info.Path, Line: index + 1, Text: lines[index]}
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
	}
	return result, nil
}

func (remote *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		filePath, err := cleanPath(upload.Path)
		if err == nil {
			_, err = remote.runChecked(ctx, "mkdir -p -- "+shellQuote(path.Dir(filePath)), 30*time.Second)
		}
		if err == nil {
			err = remote.sandbox.WriteFile(ctx, filePath, upload.Content)
		}
		if err != nil {
			results[index].Error = err.Error()
		}
	}
	return results
}

func (remote *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	for index, value := range paths {
		results[index].Path = value
		filePath, err := cleanPath(value)
		if err == nil {
			results[index].Content, err = remote.readRaw(ctx, filePath)
		}
		if err != nil {
			results[index].Error = err.Error()
		}
	}
	return results
}

func (remote *Backend) readRaw(ctx context.Context, filePath string) ([]byte, error) {
	data, err := remote.sandbox.ReadFile(ctx, filePath)
	if err != nil {
		return nil, fmt.Errorf("langsmith backend: read %q: %w", filePath, err)
	}
	if len(data) > remote.maxFileSize {
		return nil, fmt.Errorf("langsmith backend: file %q exceeds %d bytes", filePath, remote.maxFileSize)
	}
	return data, nil
}

func (remote *Backend) find(ctx context.Context, base string, recursive bool) ([]dabackend.FileInfo, error) {
	depth := "-maxdepth 1"
	if recursive {
		depth = ""
	}
	command := "find " + shellQuote(base) + " -mindepth 1 " + depth + " -printf '%y\\t%s\\t%T@\\t%p\\0'"
	result, err := remote.runChecked(ctx, command, 60*time.Second)
	if err != nil {
		return nil, err
	}
	entries, err := parseFind(result.Output)
	if err != nil {
		return nil, err
	}
	if len(entries) > remote.maxResults {
		entries = entries[:remote.maxResults]
	}
	return entries, nil
}

func (remote *Backend) runChecked(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	result, err := remote.Execute(ctx, command, timeout)
	if err != nil {
		return dabackend.ExecuteResult{}, err
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("langsmith backend: command failed: %s", strings.TrimSpace(result.Output))
	}
	return result, nil
}

func parseFind(output string) ([]dabackend.FileInfo, error) {
	entries := []dabackend.FileInfo{}
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\t", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("langsmith backend: malformed find output")
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("langsmith backend: malformed file size: %w", err)
		}
		seconds, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return nil, fmt.Errorf("langsmith backend: malformed modification time: %w", err)
		}
		isDir := parts[0] == "d"
		filePath := parts[3]
		if isDir {
			filePath += "/"
		}
		entries = append(entries, dabackend.FileInfo{Path: filePath, IsDir: isDir, Size: size, ModifiedAt: time.Unix(0, int64(seconds*float64(time.Second))).UTC()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func cleanPath(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("langsmith backend: invalid absolute path %q", value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("langsmith backend: path traversal is not allowed: %q", value)
		}
	}
	return path.Clean(value), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func splitLines(value string) []string {
	if value == "" {
		return nil
	}
	value = strings.TrimSuffix(value, "\n")
	if value == "" {
		return []string{""}
	}
	return strings.Split(value, "\n")
}

func compileGlob(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob pattern is required")
	}
	pattern = strings.TrimPrefix(pattern, "/")
	var expression strings.Builder
	expression.WriteString("^")
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
	expression.WriteString("$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, fmt.Errorf("invalid glob %q: %w", pattern, err)
	}
	return compiled, nil
}
