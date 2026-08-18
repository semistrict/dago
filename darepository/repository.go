// Package darepository provides bounded, read-only repository tools for
// untrusted nested agents such as acceptance-criteria drafters and graders.
package darepository

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/datool"
)

const (
	defaultToolCalls        = 25
	defaultReadLines        = 120
	defaultReadBytes        = 256_000
	defaultDirectoryEntries = 200
	defaultGlobMatches      = 200
	defaultGrepMatches      = 100
	defaultResultBytes      = 12_000
)

// Options bounds one repository-inspection operation. Zero values select the
// conservative defaults used by the reference implementation.
type Options struct {
	Root                string
	MaxCalls            int
	ReadLineLimit       int
	ReadByteLimit       int64
	DirectoryEntryLimit int
	GlobMatchLimit      int
	GrepMatchLimit      int
	ResultByteLimit     int
}

// Inspector owns a reusable set of read-only repository tools. Each nested
// invocation must use Operation so calls share one fail-closed budget.
type Inspector struct {
	backend dabackend.Backend
	options Options
}

type operationKey struct{ inspector *Inspector }
type operationBudget struct {
	mu    sync.Mutex
	calls int
}

// New constructs a bounded repository inspector. Static invalid options panic.
func New(backend dabackend.Backend, options Options) *Inspector {
	if nilInterface(backend) {
		panic("repository inspector backend is nil")
	}
	if options.Root == "" {
		options.Root = "/"
	}
	if _, err := cleanPath(options.Root, "/"); err != nil {
		panic(fmt.Sprintf("repository inspector root: %v", err))
	}
	options.Root = path.Clean(options.Root)
	setDefault := func(value *int, fallback int, name string) {
		if *value < 0 {
			panic("repository inspector " + name + " cannot be negative")
		}
		if *value == 0 {
			*value = fallback
		}
	}
	setDefault(&options.MaxCalls, defaultToolCalls, "max calls")
	setDefault(&options.ReadLineLimit, defaultReadLines, "read line limit")
	if options.ReadByteLimit < 0 {
		panic("repository inspector read byte limit cannot be negative")
	}
	if options.ReadByteLimit == 0 {
		options.ReadByteLimit = defaultReadBytes
	}
	setDefault(&options.DirectoryEntryLimit, defaultDirectoryEntries, "directory entry limit")
	setDefault(&options.GlobMatchLimit, defaultGlobMatches, "glob match limit")
	setDefault(&options.GrepMatchLimit, defaultGrepMatches, "grep match limit")
	setDefault(&options.ResultByteLimit, defaultResultBytes, "result byte limit")
	return &Inspector{backend: backend, options: options}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Operation installs a fresh shared call budget for one nested-agent run.
func (inspector *Inspector) Operation(ctx context.Context) context.Context {
	return context.WithValue(ctx, operationKey{inspector}, &operationBudget{})
}

func (inspector *Inspector) reserve(ctx context.Context) error {
	budget, ok := ctx.Value(operationKey{inspector}).(*operationBudget)
	if !ok {
		return fmt.Errorf("repository inspection operation is unavailable")
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.calls >= inspector.options.MaxCalls {
		return fmt.Errorf("repository inspection call limit of %d reached", inspector.options.MaxCalls)
	}
	budget.calls++
	return nil
}

type listInput struct {
	Path string `json:"path,omitempty" jsonschema:"description=Absolute repository path to list; defaults to the repository root."`
}
type readInput struct {
	Path   string `json:"path" jsonschema:"description=Absolute repository file path."`
	Offset int    `json:"offset,omitempty" jsonschema:"description=Zero-based line offset."`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Maximum lines; capped by the repository policy."`
}
type globInput struct {
	Pattern string `json:"pattern" jsonschema:"description=Glob pattern relative to path."`
	Path    string `json:"path,omitempty" jsonschema:"description=Absolute repository path to search; defaults to the repository root."`
}
type grepInput struct {
	Pattern  string `json:"pattern" jsonschema:"description=Literal text or regular expression accepted by the backend."`
	Path     string `json:"path,omitempty" jsonschema:"description=Absolute repository path to search; defaults to the repository root."`
	Glob     string `json:"glob,omitempty" jsonschema:"description=Optional file glob."`
	MaxCount int    `json:"max_count,omitempty" jsonschema:"description=Maximum matches; capped by the repository policy."`
}

// Tools returns ls, read_file, glob, and grep. No mutation or shell tool is
// exposed by this package.
func (inspector *Inspector) Tools() []datool.Tool {
	return []datool.Tool{
		datool.MustNew("ls", "List a bounded repository directory. Paths must be absolute and remain inside the configured repository root.", inspector.list),
		datool.MustNew("read_file", "Read a bounded page from a UTF-8 repository file. Paths must be absolute and remain inside the configured repository root.", inspector.read),
		datool.MustNew("glob", "Find a bounded set of repository paths matching a glob.", inspector.glob),
		datool.MustNew("grep", "Search repository text and return a bounded set of matches.", inspector.grep),
	}
}

func (inspector *Inspector) prepare(ctx context.Context, raw string) (string, *datool.Result) {
	if err := inspector.reserve(ctx); err != nil {
		result := errorResult(err)
		return "", &result
	}
	if raw == "" {
		raw = inspector.options.Root
	}
	clean, err := cleanPath(raw, inspector.options.Root)
	if err != nil {
		result := errorResult(err)
		return "", &result
	}
	return clean, nil
}

func cleanPath(raw, root string) (string, error) {
	if strings.ContainsRune(raw, 0) || strings.HasPrefix(raw, "~") || !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("repository paths must be absolute")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", fmt.Errorf("repository path traversal is not allowed")
		}
	}
	clean := path.Clean(raw)
	root = path.Clean(root)
	if root != "/" && clean != root && !strings.HasPrefix(clean, root+"/") {
		return "", fmt.Errorf("repository path is outside root %q", root)
	}
	return clean, nil
}

func cleanPattern(value string) error {
	if value == "" {
		return fmt.Errorf("search pattern is required")
	}
	if strings.ContainsRune(value, 0) || strings.HasPrefix(value, "~") {
		return fmt.Errorf("unsafe search pattern")
	}
	for _, part := range strings.Split(strings.ReplaceAll(value, "\\", "/"), "/") {
		if part == ".." {
			return fmt.Errorf("search pattern traversal is not allowed")
		}
	}
	return nil
}

func (inspector *Inspector) list(ctx context.Context, input listInput) (datool.Result, error) {
	p, rejected := inspector.prepare(ctx, input.Path)
	if rejected != nil {
		return *rejected, nil
	}
	value, err := inspector.backend.List(ctx, p)
	if err != nil {
		return errorResult(err), nil
	}
	if len(value.Entries) > inspector.options.DirectoryEntryLimit {
		return errorResult(fmt.Errorf("directory has %d entries; limit is %d", len(value.Entries), inspector.options.DirectoryEntryLimit)), nil
	}
	return inspector.jsonResult(value)
}

func (inspector *Inspector) read(ctx context.Context, input readInput) (datool.Result, error) {
	p, rejected := inspector.prepare(ctx, input.Path)
	if rejected != nil {
		return *rejected, nil
	}
	if input.Offset < 0 {
		return errorResult(fmt.Errorf("read offset cannot be negative")), nil
	}
	parent := path.Dir(p)
	listing, err := inspector.backend.List(ctx, parent)
	if err != nil {
		return errorResult(err), nil
	}
	if len(listing.Entries) > inspector.options.DirectoryEntryLimit {
		return errorResult(fmt.Errorf("parent directory has %d entries; limit is %d", len(listing.Entries), inspector.options.DirectoryEntryLimit)), nil
	}
	found := false
	for _, entry := range listing.Entries {
		if path.Clean(entry.Path) == p {
			found = true
			if entry.IsDir {
				return errorResult(fmt.Errorf("repository path is a directory")), nil
			}
			if entry.Size > inspector.options.ReadByteLimit {
				return errorResult(fmt.Errorf("file size %d exceeds limit %d", entry.Size, inspector.options.ReadByteLimit)), nil
			}
			break
		}
	}
	if !found {
		return errorResult(fmt.Errorf("repository file metadata is unavailable")), nil
	}
	limit := input.Limit
	if limit <= 0 || limit > inspector.options.ReadLineLimit {
		limit = inspector.options.ReadLineLimit
	}
	value, err := inspector.backend.Read(ctx, p, input.Offset, limit)
	if err != nil {
		return errorResult(err), nil
	}
	if value.Data == nil || value.Data.Encoding != dabackend.EncodingUTF8 {
		return errorResult(fmt.Errorf("repository file is not UTF-8 text")), nil
	}
	if int64(len(value.Data.Content)) > inspector.options.ReadByteLimit {
		return errorResult(fmt.Errorf("read payload exceeds byte limit")), nil
	}
	return inspector.jsonResult(value)
}

func (inspector *Inspector) glob(ctx context.Context, input globInput) (datool.Result, error) {
	p, rejected := inspector.prepare(ctx, input.Path)
	if rejected != nil {
		return *rejected, nil
	}
	if err := cleanPattern(input.Pattern); err != nil {
		return errorResult(err), nil
	}
	value, err := inspector.backend.Glob(ctx, input.Pattern, p)
	if err != nil {
		return errorResult(err), nil
	}
	if len(value.Matches) > inspector.options.GlobMatchLimit {
		value.Matches = value.Matches[:inspector.options.GlobMatchLimit]
		value.Truncated = true
	}
	return inspector.jsonResult(value)
}

func (inspector *Inspector) grep(ctx context.Context, input grepInput) (datool.Result, error) {
	p, rejected := inspector.prepare(ctx, input.Path)
	if rejected != nil {
		return *rejected, nil
	}
	if err := cleanPattern(input.Pattern); err != nil {
		return errorResult(err), nil
	}
	if input.Glob != "" {
		if err := cleanPattern(input.Glob); err != nil {
			return errorResult(err), nil
		}
	}
	limit := input.MaxCount
	if limit <= 0 || limit > inspector.options.GrepMatchLimit {
		limit = inspector.options.GrepMatchLimit
	}
	value, err := inspector.backend.Grep(ctx, input.Pattern, dabackend.GrepOptions{Path: p, Glob: input.Glob, MaxCount: limit})
	if err != nil {
		return errorResult(err), nil
	}
	if len(value.Matches) > limit {
		value.Matches = value.Matches[:limit]
		value.Truncated = true
	}
	return inspector.jsonResult(value)
}

func (inspector *Inspector) jsonResult(value any) (datool.Result, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return datool.Result{}, err
	}
	encoded = boundUTF8(encoded, inspector.options.ResultByteLimit)
	return datool.TextResult(string(encoded)), nil
}

func boundUTF8(value []byte, limit int) []byte {
	if len(value) <= limit {
		return value
	}
	marker := []byte("\n...[repository result truncated]")
	value = value[:max(0, limit-len(marker))]
	for len(value) > 0 && !utf8.Valid(value) {
		value = value[:len(value)-1]
	}
	return append(value, marker...)
}

func errorResult(err error) datool.Result {
	result := datool.TextResult("repository inspection rejected: " + err.Error())
	result.Status = damessage.ToolStatusError
	return result
}
