// Package agentcore adapts a caller-authenticated AgentCore Code Interpreter
// transport to dago's sandbox contracts. It bundles no vendor SDK, discovers no
// credentials, and does not provide registry or command-line wiring.
package agentcore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
)

const (
	// DefaultWorkingDir is the upstream factory's virtual AgentCore root.
	DefaultWorkingDir = "/tmp"
	defaultRegion     = "us-west-2"
	defaultSource     = "deepagents-code"
	defaultTimeout    = 30 * time.Minute
	defaultStartLimit = 3 * time.Minute
	defaultStopLimit  = 30 * time.Second
	defaultMaxOutput  = 1 << 20
	defaultMaxFile    = 10 << 20
	defaultMaxResults = 1000
	defaultMaxBatch   = 1000
	defaultMaxActive  = 32
)

var (
	// ErrSessionExpired classifies a transport response for an expired or
	// terminated Code Interpreter session.
	ErrSessionExpired = errors.New("agentcore session expired")
	// ErrReconnectUnsupported reports AgentCore's inability to reconnect to a
	// session after the owning interpreter process exits.
	ErrReconnectUnsupported = errors.New("agentcore session reconnection is unsupported")
	// ErrActiveSessionLimit reports that Provider reached its finite local
	// lifecycle cap.
	ErrActiveSessionLimit = errors.New("agentcore active session limit reached")
)

// OutputType identifies a streamed execute content item.
type OutputType string

const (
	OutputText  OutputType = "text"
	OutputError OutputType = "error"
)

// OutputItem is one streamed command content item. Text is a pointer because a
// missing error message selects "Unknown error" while an explicitly empty one
// remains empty.
type OutputItem struct {
	Type OutputType
	Text *string
}

// ExecuteEvent is one materialized AgentCore command result event. A nil result
// is represented by an event with no content and no exit code.
type ExecuteEvent struct {
	ExitCode *int
	Content  []OutputItem
}

// UploadFile is one writeFiles item. Exactly one of Text or Blob is populated.
type UploadFile struct {
	Path string
	Text *string
	Blob []byte
}

// FileResource is one readFiles resource event. Exactly one of Text or Blob is
// populated; pointers preserve an explicitly empty text or binary file.
type FileResource struct {
	URI  string
	Text *string
	Blob *[]byte
}

// Transport is the narrow authenticated API used by Backend and Provider.
// Implementations must honor ctx, eagerly consume remote streams, and enforce
// maxOutput/maxBytes before returning data. The adapter checks bounds again.
type Transport interface {
	Start(context.Context, string, string) (string, error)
	Stop(context.Context, string) error
	Execute(context.Context, string, string, int, int) ([]ExecuteEvent, error)
	WriteFiles(context.Context, string, []UploadFile, int64) error
	ReadFiles(context.Context, string, []string, int64) ([]FileResource, error)
}

// Options configures an existing session backend. Zero limits select useful
// bounded defaults. WorkingDir nil enables lazy pwd detection; a pointer to "/"
// explicitly selects the remote filesystem root.
type Options struct {
	WorkingDir           *string
	DefaultTimeout       time.Duration
	MaxOutput            int
	MaxFileSize          int64
	MaxResults           int
	MaxTransferFiles     int
	EnableCaptureOffload bool
}

// Backend wraps one active Code Interpreter session. Its ID is copied at
// construction and remains stable.
type Backend struct {
	*dabackend.BaseSandbox

	id               string
	transport        Transport
	defaultTimeout   time.Duration
	maxOutput        int
	maxFileSize      int64
	maxResults       int
	maxTransferFiles int

	cwdMu    sync.Mutex
	cwd      string
	cwdKnown bool
}

// New wraps an active session. transport and sessionID are mandatory static
// dependencies; invalid values panic rather than returning an error.
func New(transport Transport, sessionID string, options Options) *Backend {
	if isNil(transport) {
		panic("agentcore backend: transport is required")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		panic("agentcore backend: session id is required")
	}
	applyOptions(&options)
	backend := &Backend{
		id:               sessionID,
		transport:        transport,
		defaultTimeout:   options.DefaultTimeout,
		maxOutput:        options.MaxOutput,
		maxFileSize:      options.MaxFileSize,
		maxResults:       options.MaxResults,
		maxTransferFiles: options.MaxTransferFiles,
	}
	if options.WorkingDir != nil {
		backend.cwd = strings.TrimRight(*options.WorkingDir, "/")
		backend.cwdKnown = true
	}
	base := dabackend.NewBaseSandbox(backend, dabackend.BaseSandboxOptions{
		EnableCaptureOffload: options.EnableCaptureOffload,
		MaxCaptureBytes:      int(options.MaxFileSize),
		MaxResults:           options.MaxResults,
	})
	backend.BaseSandbox = base
	return backend
}

func applyOptions(options *Options) {
	if options.WorkingDir != nil {
		workingDir := strings.TrimSpace(*options.WorkingDir)
		if workingDir == "" || workingDir != *options.WorkingDir || !strings.HasPrefix(workingDir, "/") || strings.ContainsRune(workingDir, '\x00') {
			panic("agentcore backend: working directory must be a clean absolute path")
		}
		workingDir = path.Clean(workingDir)
		options.WorkingDir = new(workingDir)
	}
	if options.DefaultTimeout < 0 {
		panic("agentcore backend: default timeout cannot be negative")
	}
	if options.MaxOutput < 0 || options.MaxFileSize < 0 || options.MaxResults < 0 || options.MaxTransferFiles < 0 {
		panic("agentcore backend: limits cannot be negative")
	}
	if uint64(options.MaxFileSize) > uint64(^uint(0)>>1) {
		panic("agentcore backend: max file size exceeds platform integer range")
	}
	if options.DefaultTimeout == 0 {
		options.DefaultTimeout = defaultTimeout
	}
	if options.MaxOutput == 0 {
		options.MaxOutput = defaultMaxOutput
	}
	if options.MaxFileSize == 0 {
		options.MaxFileSize = defaultMaxFile
	}
	if options.MaxResults == 0 {
		options.MaxResults = defaultMaxResults
	}
	if options.MaxTransferFiles == 0 {
		options.MaxTransferFiles = defaultMaxBatch
	}
}

func isNil(value any) bool {
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

// ID returns the stable session ID captured by New.
func (backend *Backend) ID() string { return backend.id }

// Execute runs a shell command. A zero timeout selects the configured default;
// use ExecuteWithOptions with an explicit zero to rely only on caller context.
func (backend *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("agentcore backend: execute timeout cannot be negative")
	}
	if timeout == 0 {
		timeout = backend.defaultTimeout
	}
	return backend.execute(ctx, command, timeout)
}

// ExecuteWithOptions preserves omitted versus explicit-zero timeout semantics.
func (backend *Backend) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	timeout := backend.defaultTimeout
	if options.Timeout != nil {
		if *options.Timeout < 0 {
			return dabackend.ExecuteResult{}, fmt.Errorf("agentcore backend: execute timeout cannot be negative")
		}
		timeout = *options.Timeout
	}
	return backend.execute(ctx, command, timeout)
}

func (backend *Backend) execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if err := ctx.Err(); err != nil {
		return dabackend.ExecuteResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return dabackend.ExecuteResult{}, fmt.Errorf("agentcore backend: command is required")
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	events, err := backend.transport.Execute(ctx, backend.id, command, backend.maxOutput, backend.maxResults)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dabackend.ExecuteResult{}, ctxErr
	}
	if err != nil {
		exitCode := 1
		if errors.Is(err, ErrSessionExpired) {
			return dabackend.ExecuteResult{
				Output:   "Error: AgentCore session has expired. Start a new session to continue.",
				ExitCode: &exitCode,
			}, nil
		}
		return dabackend.ExecuteResult{
			Output:   "Error executing command: " + boundedError(err),
			ExitCode: &exitCode,
		}, nil
	}
	if len(events) > backend.maxResults {
		exitCode := 1
		return dabackend.ExecuteResult{
			Output:   "Error executing command: response event limit exceeded",
			ExitCode: &exitCode,
		}, nil
	}
	output, exitCode, truncated := extractOutput(events, backend.maxOutput)
	return dabackend.ExecuteResult{Output: output, ExitCode: &exitCode, Truncated: truncated}, nil
}

func extractOutput(events []ExecuteEvent, maxBytes int) (string, int, bool) {
	var output strings.Builder
	exitCode := 0
	hasExitCode := false
	hasPart := false
	truncated := false
	for _, event := range events {
		if event.ExitCode != nil {
			exitCode = *event.ExitCode
			hasExitCode = true
		}
		for _, item := range event.Content {
			var (
				part       string
				recognized bool
			)
			switch item.Type {
			case OutputText:
				recognized = true
				if item.Text != nil {
					part = *item.Text
				}
			case OutputError:
				recognized = true
				text := "Unknown error"
				if item.Text != nil {
					text = *item.Text
				}
				part = "Error: " + text
				if !hasExitCode {
					exitCode = 1
					hasExitCode = true
				}
			}
			if !recognized {
				continue
			}
			if hasPart {
				truncated = appendOutput(&output, "\n", maxBytes) || truncated
			}
			hasPart = true
			truncated = appendOutput(&output, part, maxBytes) || truncated
		}
	}
	return output.String(), exitCode, truncated
}

func appendOutput(output *strings.Builder, value string, maxBytes int) bool {
	remaining := maxBytes - output.Len()
	if remaining <= 0 {
		return value != ""
	}
	if len(value) <= remaining {
		output.WriteString(value)
		return false
	}
	output.WriteString(validPrefix(value, remaining))
	return true
}

func (backend *Backend) workingDir(ctx context.Context) (string, error) {
	backend.cwdMu.Lock()
	defer backend.cwdMu.Unlock()
	if backend.cwdKnown {
		return backend.cwd, nil
	}
	result, err := backend.Execute(ctx, "pwd", 0)
	if err != nil {
		return "", err
	}
	if result.ExitCode == nil || *result.ExitCode != 0 || strings.TrimSpace(result.Output) == "" {
		exitCode := "none"
		if result.ExitCode != nil {
			exitCode = fmt.Sprint(*result.ExitCode)
		}
		return "", fmt.Errorf("agentcore backend: detect working directory failed: exit_code=%s output=%q", exitCode, boundedString(result.Output))
	}
	backend.cwd = strings.TrimRight(strings.TrimSpace(result.Output), "/")
	backend.cwdKnown = true
	return backend.cwd, nil
}

func (backend *Backend) absolutePath(ctx context.Context, filePath string) (string, error) {
	cwd, err := backend.workingDir(ctx)
	if err != nil {
		return "", err
	}
	if cwd == "" || filePath == cwd || strings.HasPrefix(filePath, cwd+"/") {
		return filePath, nil
	}
	return cwd + "/" + strings.TrimLeft(filePath, "/"), nil
}

func (backend *Backend) relativePath(filePath string) string {
	backend.cwdMu.Lock()
	cwd, known := backend.cwd, backend.cwdKnown
	backend.cwdMu.Unlock()
	if known && cwd != "" && strings.HasPrefix(filePath, cwd+"/") {
		return filePath[len(cwd)+1:]
	}
	filePath = strings.TrimLeft(filePath, "/")
	for strings.HasPrefix(filePath, "./") {
		filePath = filePath[2:]
	}
	return filePath
}

// Upload maps valid UTF-8 to AgentCore text items and preserves other bytes as
// raw blobs. Remote failures map to the partner's permission_denied response.
func (backend *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	files := make([]UploadFile, 0, min(len(uploads), backend.maxTransferFiles))
	indexes := make([]int, 0, cap(files))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "agentcore backend: transfer batch limit exceeded"
		case int64(len(upload.Content)) > backend.maxFileSize:
			results[index].Error = fmt.Sprintf("agentcore backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
		default:
			file := UploadFile{Path: backend.relativePath(upload.Path)}
			if utf8.Valid(upload.Content) {
				text := string(upload.Content)
				file.Text = &text
			} else {
				file.Blob = append([]byte(nil), upload.Content...)
			}
			files = append(files, file)
			indexes = append(indexes, index)
		}
	}
	if len(files) == 0 {
		return results
	}
	if err := ctx.Err(); err != nil {
		setUploadErrors(results, indexes, err.Error())
		return results
	}
	err := backend.transport.WriteFiles(ctx, backend.id, files, backend.maxFileSize)
	if ctxErr := ctx.Err(); ctxErr != nil {
		setUploadErrors(results, indexes, ctxErr.Error())
	} else if err != nil {
		setUploadErrors(results, indexes, "permission_denied")
	}
	return results
}

func setUploadErrors(results []dabackend.UploadResult, indexes []int, detail string) {
	for _, index := range indexes {
		results[index].Error = detail
	}
}

// Download normalizes paths for readFiles and maps text or raw blob resources
// back to input order. Partial success is preserved.
func (backend *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	if len(paths) == 0 {
		return results
	}
	if _, err := backend.workingDir(ctx); err != nil {
		detail := "file_not_found"
		if ctxErr := ctx.Err(); ctxErr != nil {
			detail = ctxErr.Error()
		}
		for index, path := range paths {
			results[index] = dabackend.DownloadResult{Path: path, Error: detail}
		}
		return results
	}
	relative := make([]string, 0, min(len(paths), backend.maxTransferFiles))
	indexes := make(map[string][]int)
	for index, filePath := range paths {
		results[index].Path = filePath
		if index >= backend.maxTransferFiles {
			results[index].Error = "agentcore backend: transfer batch limit exceeded"
			continue
		}
		normalized := backend.relativePath(filePath)
		relative = append(relative, normalized)
		indexes[normalized] = append(indexes[normalized], index)
	}
	if err := ctx.Err(); err != nil {
		setDownloadErrors(results, indexes, err.Error())
		return results
	}
	resources, err := backend.transport.ReadFiles(ctx, backend.id, relative, backend.maxFileSize)
	if ctxErr := ctx.Err(); ctxErr != nil {
		setDownloadErrors(results, indexes, ctxErr.Error())
		return results
	}
	if err != nil {
		detail := "file_not_found"
		if errors.Is(err, ErrSessionExpired) {
			detail = "permission_denied"
		}
		setDownloadErrors(results, indexes, detail)
		return results
	}
	if len(resources) > len(indexes) {
		setDownloadErrors(results, indexes, "file_not_found")
		return results
	}
	seen := make(map[string]bool, len(resources))
	for _, resource := range resources {
		filePath := normalizeRelative(strings.TrimPrefix(resource.URI, "file://"))
		pathIndexes, expected := indexes[filePath]
		if !expected || seen[filePath] || (resource.Text == nil) == (resource.Blob == nil) {
			setDownloadErrors(results, indexes, "file_not_found")
			return results
		}
		seen[filePath] = true
		var content []byte
		if resource.Text != nil {
			content = []byte(*resource.Text)
		} else {
			content = append([]byte(nil), (*resource.Blob)...)
		}
		if int64(len(content)) > backend.maxFileSize {
			for _, index := range pathIndexes {
				results[index].Error = fmt.Sprintf("agentcore backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
			}
			continue
		}
		for _, index := range pathIndexes {
			results[index].Content = append([]byte(nil), content...)
		}
	}
	for filePath, pathIndexes := range indexes {
		if seen[filePath] {
			continue
		}
		for _, index := range pathIndexes {
			results[index].Error = "file_not_found"
		}
	}
	return results
}

func setDownloadErrors(results []dabackend.DownloadResult, indexes map[string][]int, detail string) {
	for _, pathIndexes := range indexes {
		for _, index := range pathIndexes {
			results[index].Content = nil
			results[index].Error = detail
		}
	}
}

func normalizeRelative(filePath string) string {
	filePath = strings.TrimLeft(filePath, "/")
	for strings.HasPrefix(filePath, "./") {
		filePath = filePath[2:]
	}
	return filePath
}

// Write resolves virtual and relative paths under the real AgentCore working
// directory and returns that executable absolute path.
func (backend *Backend) Write(ctx context.Context, filePath, content string) (dabackend.WriteResult, error) {
	resolved, err := backend.absolutePath(ctx, filePath)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	result, err := backend.BaseSandbox.Write(ctx, resolved, content)
	if err != nil {
		return dabackend.WriteResult{}, err
	}
	result.Path = resolved
	return result, nil
}

func (backend *Backend) List(ctx context.Context, directory string) (dabackend.ListResult, error) {
	resolved, err := backend.absolutePath(ctx, directory)
	if err != nil {
		return dabackend.ListResult{}, err
	}
	return backend.BaseSandbox.List(ctx, resolved)
}

func (backend *Backend) Read(ctx context.Context, filePath string, offset, limit int) (dabackend.ReadResult, error) {
	resolved, err := backend.absolutePath(ctx, filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	return backend.BaseSandbox.Read(ctx, resolved, offset, limit)
}

func (backend *Backend) ReadBinary(ctx context.Context, filePath string, maxBytes int64) (dabackend.ReadResult, error) {
	resolved, err := backend.absolutePath(ctx, filePath)
	if err != nil {
		return dabackend.ReadResult{}, err
	}
	return backend.BaseSandbox.ReadBinary(ctx, resolved, maxBytes)
}

func (backend *Backend) Edit(ctx context.Context, filePath, old, replacement string, replaceAll bool) (dabackend.EditResult, error) {
	resolved, err := backend.absolutePath(ctx, filePath)
	if err != nil {
		return dabackend.EditResult{}, err
	}
	return backend.BaseSandbox.Edit(ctx, resolved, old, replacement, replaceAll)
}

func (backend *Backend) Delete(ctx context.Context, filePath string) (dabackend.DeleteResult, error) {
	resolved, err := backend.absolutePath(ctx, filePath)
	if err != nil {
		return dabackend.DeleteResult{}, err
	}
	return backend.BaseSandbox.Delete(ctx, resolved)
}

func (backend *Backend) Glob(ctx context.Context, pattern, base string) (dabackend.GlobResult, error) {
	if base != "" {
		resolved, err := backend.absolutePath(ctx, base)
		if err != nil {
			return dabackend.GlobResult{}, err
		}
		base = resolved
	}
	return backend.BaseSandbox.Glob(ctx, pattern, base)
}

func (backend *Backend) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	if options.Path != "" {
		resolved, err := backend.absolutePath(ctx, options.Path)
		if err != nil {
			return dabackend.GrepResult{}, err
		}
		options.Path = resolved
	}
	return backend.BaseSandbox.Grep(ctx, pattern, options)
}

func validPrefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func boundedString(value string) string {
	const maxBytes = 300
	value = strings.TrimSpace(value)
	if len(value) <= maxBytes {
		return value
	}
	return validPrefix(value, maxBytes) + "..."
}

func boundedError(err error) string { return boundedString(err.Error()) }

var (
	_ dabackend.Sandbox             = (*Backend)(nil)
	_ dabackend.ConfigurableSandbox = (*Backend)(nil)
	_ dabackend.BoundedBinaryReader = (*Backend)(nil)
)

// EnvResolver returns one process configuration value and whether it exists.
type EnvResolver func(string) (string, bool)

// ProviderOptions configures explicit session lifecycle management. Zero
// values select the upstream region precedence and bounded local defaults.
type ProviderOptions struct {
	Region            string
	ResolveEnv        EnvResolver
	IntegrationSource string
	StartTimeout      time.Duration
	StopTimeout       time.Duration
	MaxActiveSessions int
	Backend           Options
}

// Provider creates fresh AgentCore sessions and tracks only sessions started by
// this process. Reconnection is deliberately unavailable.
type Provider struct {
	transport Transport
	options   ProviderOptions

	mu       sync.Mutex
	starting int
	active   map[string]struct{}
}

// NewProvider constructs lifecycle management around a caller-authenticated
// transport. It performs no credential discovery or network I/O.
func NewProvider(transport Transport, options ProviderOptions) *Provider {
	if isNil(transport) {
		panic("agentcore provider: transport is required")
	}
	if options.StartTimeout < 0 || options.StopTimeout < 0 || options.MaxActiveSessions < 0 {
		panic("agentcore provider: lifecycle limits cannot be negative")
	}
	applyOptions(&options.Backend)
	if options.ResolveEnv == nil {
		options.ResolveEnv = os.LookupEnv
	}
	options.Region = strings.TrimSpace(options.Region)
	if options.Region == "" {
		options.Region = firstEnvironment(options.ResolveEnv, "AWS_REGION", "AWS_DEFAULT_REGION")
		if options.Region == "" {
			options.Region = defaultRegion
		}
	}
	options.IntegrationSource = strings.TrimSpace(options.IntegrationSource)
	if options.IntegrationSource == "" {
		options.IntegrationSource = defaultSource
	}
	if options.StartTimeout == 0 {
		options.StartTimeout = defaultStartLimit
	}
	if options.StopTimeout == 0 {
		options.StopTimeout = defaultStopLimit
	}
	if options.MaxActiveSessions == 0 {
		options.MaxActiveSessions = defaultMaxActive
	}
	return &Provider{transport: transport, options: options, active: make(map[string]struct{})}
}

func firstEnvironment(resolve EnvResolver, names ...string) string {
	for _, name := range names {
		if value, ok := resolve(name); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Region returns the resolved AWS region passed to Transport.Start.
func (provider *Provider) Region() string { return provider.options.Region }

// Create starts and tracks one fresh session.
func (provider *Provider) Create(ctx context.Context) (*Backend, error) {
	provider.mu.Lock()
	if len(provider.active)+provider.starting >= provider.options.MaxActiveSessions {
		provider.mu.Unlock()
		return nil, ErrActiveSessionLimit
	}
	provider.starting++
	provider.mu.Unlock()
	defer func() {
		provider.mu.Lock()
		provider.starting--
		provider.mu.Unlock()
	}()

	startCtx, cancel := context.WithTimeout(ctx, provider.options.StartTimeout)
	defer cancel()
	sessionID, err := provider.transport.Start(startCtx, provider.options.Region, provider.options.IntegrationSource)
	if ctxErr := startCtx.Err(); ctxErr != nil {
		if strings.TrimSpace(sessionID) != "" {
			provider.cleanup(sessionID)
		}
		return nil, ctxErr
	}
	if err != nil {
		if strings.TrimSpace(sessionID) != "" {
			provider.cleanup(sessionID)
		}
		return nil, wrapTransportError("agentcore provider: start", err)
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("agentcore provider: start returned an empty session id")
	}
	provider.mu.Lock()
	if _, duplicate := provider.active[sessionID]; duplicate {
		provider.mu.Unlock()
		return nil, fmt.Errorf("agentcore provider: duplicate session id %q", sessionID)
	}
	provider.active[sessionID] = struct{}{}
	provider.mu.Unlock()
	return New(provider.transport, sessionID, provider.options.Backend), nil
}

// Attach always reports the upstream reconnection limitation.
func (provider *Provider) Attach(context.Context, string) (*Backend, error) {
	return nil, ErrReconnectUnsupported
}

// Delete stops a session created by this Provider. Unknown IDs are idempotent.
// Unlike the Python CLI wrapper, Go returns a bounded stop error so callers can
// verify that a billable session actually terminated.
func (provider *Provider) Delete(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("agentcore provider: session id is required")
	}
	provider.mu.Lock()
	_, tracked := provider.active[sessionID]
	if tracked {
		delete(provider.active, sessionID)
	}
	provider.mu.Unlock()
	if !tracked {
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, provider.options.StopTimeout)
	defer cancel()
	if err := provider.transport.Stop(stopCtx, sessionID); err != nil {
		if ctxErr := stopCtx.Err(); ctxErr != nil {
			return ctxErr
		}
		return wrapTransportError("agentcore provider: stop", err)
	}
	if ctxErr := stopCtx.Err(); ctxErr != nil {
		return ctxErr
	}
	return nil
}

func (provider *Provider) cleanup(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), provider.options.StopTimeout)
	defer cancel()
	_ = provider.transport.Stop(ctx, sessionID)
}

type transportError struct {
	prefix string
	cause  error
}

func (err transportError) Error() string { return err.prefix + ": " + boundedError(err.cause) }
func (err transportError) Unwrap() error { return err.cause }

func wrapTransportError(prefix string, err error) error {
	return transportError{prefix: prefix, cause: err}
}
