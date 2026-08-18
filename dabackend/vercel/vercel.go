// Package vercel adapts a caller-supplied Vercel Sandbox transport to dago's
// sandbox backend. It performs no credential discovery and creates no remote
// resources.
package vercel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
)

const (
	defaultTimeout       = 30 * time.Minute
	defaultMaxOutput     = 100_000
	defaultMaxFileSize   = 10 << 20
	defaultMaxResults    = 1000
	defaultMaxBatchFiles = 1000
	killTimeout          = 5 * time.Second
)

var (
	// ErrFileNotFound classifies a missing file.
	ErrFileNotFound = errors.New("vercel file not found")
	// ErrIsDirectory classifies a file read that targeted a directory.
	ErrIsDirectory = errors.New("vercel path is a directory")
	// ErrPermissionDenied classifies a remote permission failure.
	ErrPermissionDenied = errors.New("vercel permission denied")
	// ErrInvalidPath classifies a provider-side path validation failure.
	ErrInvalidPath = errors.New("vercel invalid path")
	// ErrSandboxNotFound distinguishes a deleted sandbox from one missing file.
	ErrSandboxNotFound = errors.New("vercel sandbox not found")
)

// Finished is the stable result returned after a detached command completes.
type Finished struct {
	ExitCode int
}

// Command is the narrow lifecycle required for a detached Vercel command.
// Wait may not support cancellation; Backend calls Kill when the caller or
// local timeout wins and requires transports to make Wait return afterward.
type Command interface {
	Wait() (Finished, error)
	Kill(context.Context) error
	Logs(context.Context, int) (stdout, stderr string, truncated bool, err error)
}

// Transport is the narrow API required from an existing, authenticated
// Vercel sandbox. Implementations must honor ctx and the supplied byte bounds.
// Upload is deliberately batched to preserve the provider's all-valid-files
// failure semantics.
type Transport interface {
	Start(context.Context, string, string, []string) (Command, error)
	Upload(context.Context, string, []dabackend.Upload, int64) error
	Download(context.Context, string, string, int64) ([]byte, error)
}

// Options controls finite execution and transfer limits. Zero values select
// useful defaults. Negative values are programmer errors and make New panic.
type Options struct {
	DefaultTimeout       time.Duration
	MaxOutput            int
	MaxFileSize          int64
	MaxResults           int
	MaxTransferFiles     int
	EnableCaptureOffload bool
}

// Backend wraps one existing Vercel sandbox with a stable identity.
type Backend struct {
	*dabackend.BaseSandbox

	id               string
	transport        Transport
	defaultTimeout   time.Duration
	maxOutput        int
	maxFileSize      int64
	maxTransferFiles int
}

// New wraps an existing Vercel sandbox. transport and sandboxID are mandatory
// static dependencies; invalid static configuration panics.
func New(transport Transport, sandboxID string, options Options) *Backend {
	if isNil(transport) {
		panic("vercel backend: transport is required")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		panic("vercel backend: sandbox id is required")
	}
	applyOptions(&options)
	backend := &Backend{
		id:               sandboxID,
		transport:        transport,
		defaultTimeout:   options.DefaultTimeout,
		maxOutput:        options.MaxOutput,
		maxFileSize:      options.MaxFileSize,
		maxTransferFiles: options.MaxTransferFiles,
	}
	base := dabackend.NewBaseSandbox(backend, dabackend.BaseSandboxOptions{
		EnableCaptureOffload: options.EnableCaptureOffload,
		MaxCaptureBytes:      int(options.MaxFileSize),
		MaxResults:           options.MaxResults,
	})
	backend.BaseSandbox = base
	return backend
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

func applyOptions(options *Options) {
	if options.DefaultTimeout < 0 {
		panic("vercel backend: default timeout cannot be negative")
	}
	if options.MaxOutput < 0 {
		panic("vercel backend: max output cannot be negative")
	}
	if options.MaxFileSize < 0 {
		panic("vercel backend: max file size cannot be negative")
	}
	if uint64(options.MaxFileSize) > uint64(^uint(0)>>1) {
		panic("vercel backend: max file size exceeds platform integer range")
	}
	if options.MaxResults < 0 {
		panic("vercel backend: max results cannot be negative")
	}
	if options.MaxTransferFiles < 0 {
		panic("vercel backend: max transfer files cannot be negative")
	}
	if options.DefaultTimeout == 0 {
		options.DefaultTimeout = defaultTimeout
	}
	if options.MaxOutput == 0 {
		options.MaxOutput = defaultMaxOutput
	}
	if options.MaxFileSize == 0 {
		options.MaxFileSize = defaultMaxFileSize
	}
	if options.MaxResults == 0 {
		options.MaxResults = defaultMaxResults
	}
	if options.MaxTransferFiles == 0 {
		options.MaxTransferFiles = defaultMaxBatchFiles
	}
}

// ID returns the stable Vercel sandbox ID captured by New.
func (backend *Backend) ID() string { return backend.id }

// Execute starts an upstream-compatible bash -lc command. A zero timeout uses
// the configured default; use ExecuteWithOptions for an explicit indefinite wait.
func (backend *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("vercel backend: execute timeout cannot be negative")
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
			return dabackend.ExecuteResult{}, fmt.Errorf("vercel backend: execute timeout cannot be negative")
		}
		timeout = *options.Timeout
	}
	return backend.execute(ctx, command, timeout)
}

type waitResult struct {
	finished Finished
	err      error
}

func (backend *Backend) execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if err := ctx.Err(); err != nil {
		return dabackend.ExecuteResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return dabackend.ExecuteResult{}, fmt.Errorf("vercel backend: command is required")
	}
	startedAt := time.Now()
	process, err := backend.transport.Start(ctx, backend.id, "bash", []string{"-lc", command})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return dabackend.ExecuteResult{}, ctxErr
		}
		return dabackend.ExecuteResult{}, wrapTransportError("vercel backend: start", err)
	}
	if isNil(process) {
		return dabackend.ExecuteResult{}, fmt.Errorf("vercel backend: transport returned a nil command")
	}

	wait := make(chan waitResult, 1)
	go func() {
		finished, waitErr := process.Wait()
		wait <- waitResult{finished: finished, err: waitErr}
	}()

	var timer <-chan time.Time
	var stop func() bool
	if timeout > 0 {
		remaining := timeout - time.Since(startedAt)
		if remaining < 0 {
			remaining = 0
		}
		clock := time.NewTimer(remaining)
		timer = clock.C
		stop = clock.Stop
	}
	if stop != nil {
		defer stop()
	}

	var finished Finished
	select {
	case outcome := <-wait:
		if outcome.err != nil {
			return dabackend.ExecuteResult{}, wrapTransportError("vercel backend: wait", outcome.err)
		}
		finished = outcome.finished
	case <-ctx.Done():
		backend.kill(process)
		return dabackend.ExecuteResult{}, ctx.Err()
	case <-timer:
		backend.kill(process)
		exitCode := 124
		return dabackend.ExecuteResult{
			Output:   fmt.Sprintf("Command timed out after %s", formatTimeout(timeout)),
			ExitCode: &exitCode,
		}, nil
	}

	stdout, stderr, transportTruncated, logErr := process.Logs(ctx, backend.maxOutput)
	if logErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return dabackend.ExecuteResult{}, ctxErr
		}
		output := "<output unavailable: failed to fetch command logs>"
		return dabackend.ExecuteResult{Output: output, ExitCode: &finished.ExitCode}, nil
	}
	output := stdout
	if strings.TrimSpace(stderr) != "" {
		output += "\n<stderr>" + strings.TrimSpace(stderr) + "</stderr>"
	}
	truncated := transportTruncated
	if len(output) > backend.maxOutput {
		output = validPrefix(output, backend.maxOutput)
		output += fmt.Sprintf("\n\n... Output truncated at %d bytes.", backend.maxOutput)
		truncated = true
	}
	return dabackend.ExecuteResult{Output: output, ExitCode: &finished.ExitCode, Truncated: truncated}, nil
}

func (backend *Backend) kill(process Command) {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	_ = process.Kill(ctx)
}

func formatTimeout(timeout time.Duration) string {
	if timeout%time.Second == 0 {
		return fmt.Sprintf("%d seconds", timeout/time.Second)
	}
	return timeout.String()
}

// Upload validates paths and sizes locally, then sends all valid files in one
// provider batch. A batch failure is reported for every valid path.
func (backend *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	valid := make([]dabackend.Upload, 0, len(uploads))
	validIndexes := make([]int, 0, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "vercel backend: transfer batch limit exceeded"
		case !strings.HasPrefix(upload.Path, "/"):
			results[index].Error = "invalid_path"
		case int64(len(upload.Content)) > backend.maxFileSize:
			results[index].Error = fmt.Sprintf("vercel backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
		default:
			valid = append(valid, dabackend.Upload{Path: upload.Path, Content: append([]byte(nil), upload.Content...)})
			validIndexes = append(validIndexes, index)
		}
	}
	if len(valid) == 0 {
		return results
	}
	if err := ctx.Err(); err != nil {
		for _, index := range validIndexes {
			results[index].Error = err.Error()
		}
		return results
	}
	err := backend.transport.Upload(ctx, backend.id, valid, backend.maxFileSize)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	if err != nil {
		mapped := mapFileError(err)
		for _, index := range validIndexes {
			results[index].Error = mapped
		}
	}
	return results
}

// Download preserves request order and distinguishes a missing sandbox from a
// missing file while bounding every payload.
func (backend *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	for index, path := range paths {
		results[index].Path = path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "vercel backend: transfer batch limit exceeded"
		case !strings.HasPrefix(path, "/"):
			results[index].Error = "invalid_path"
		case ctx.Err() != nil:
			results[index].Error = ctx.Err().Error()
		default:
			content, err := backend.transport.Download(ctx, backend.id, path, backend.maxFileSize)
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			}
			if err != nil {
				results[index].Error = mapFileError(err)
				continue
			}
			if int64(len(content)) > backend.maxFileSize {
				results[index].Error = fmt.Sprintf("vercel backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
				continue
			}
			results[index].Content = append([]byte(nil), content...)
		}
	}
	return results
}

func mapFileError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err.Error()
	case errors.Is(err, ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, ErrIsDirectory):
		return "is_directory"
	case errors.Is(err, ErrInvalidPath):
		return "invalid_path"
	case errors.Is(err, ErrFileNotFound):
		return "file_not_found"
	case errors.Is(err, ErrSandboxNotFound):
		return "sandbox not found"
	}
	message := strings.TrimSpace(err.Error())
	lower := strings.ToLower(message)
	for _, needle := range []string{"permission", "forbidden", "access denied"} {
		if strings.Contains(lower, needle) {
			return "permission_denied"
		}
	}
	if strings.Contains(lower, "is a directory") {
		return "is_directory"
	}
	if strings.Contains(lower, "invalid path") {
		return "invalid_path"
	}
	if strings.Contains(lower, "no such file") {
		return "file_not_found"
	}
	if message == "" {
		return "file_not_found"
	}
	return boundedError(err)
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

func boundedError(err error) string {
	const maxBytes = 300
	value := strings.TrimSpace(err.Error())
	if len(value) <= maxBytes {
		return value
	}
	return validPrefix(value, maxBytes) + "..."
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

var (
	_ dabackend.Sandbox             = (*Backend)(nil)
	_ dabackend.ConfigurableSandbox = (*Backend)(nil)
	_ dabackend.BoundedBinaryReader = (*Backend)(nil)
)
