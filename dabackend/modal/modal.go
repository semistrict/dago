// Package modal adapts a caller-supplied Modal sandbox transport to dago's
// sandbox backend. It performs no credential discovery and creates no remote
// resources.
package modal

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
	defaultMaxOutput     = 1 << 20
	defaultMaxFileSize   = 10 << 20
	defaultMaxResults    = 1000
	defaultMaxBatchFiles = 1000
)

var (
	// ErrFileNotFound classifies a transport response for a missing file.
	ErrFileNotFound = errors.New("modal file not found")
	// ErrIsDirectory classifies a transport response when a file read targets a directory.
	ErrIsDirectory = errors.New("modal path is a directory")
	// ErrPermissionDenied classifies a transport response rejected by remote permissions.
	ErrPermissionDenied = errors.New("modal permission denied")
)

// CommandResult is the transport-neutral result of one Modal process.
// Truncated reports that the transport discarded output beyond maxOutput.
type CommandResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// Transport is the narrow API required from an existing, authenticated Modal
// sandbox. Implementations must honor ctx and the supplied byte bounds.
//
// The sandbox ID, argv/path, timeout, and bounds are positional because they
// are required for safe execution and transfer.
type Transport interface {
	Execute(context.Context, string, []string, time.Duration, int) (CommandResult, error)
	Upload(context.Context, string, string, []byte, int64) error
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

// Backend wraps one existing Modal sandbox. Its ID is copied at construction
// and remains stable even if the transport's own state changes.
type Backend struct {
	*dabackend.BaseSandbox

	id               string
	transport        Transport
	defaultTimeout   time.Duration
	maxOutput        int
	maxFileSize      int64
	maxTransferFiles int
}

// New wraps an existing Modal sandbox. transport and sandboxID are mandatory
// static dependencies; invalid static configuration panics.
func New(transport Transport, sandboxID string, options Options) *Backend {
	if isNil(transport) {
		panic("modal backend: transport is required")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		panic("modal backend: sandbox id is required")
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
		panic("modal backend: default timeout cannot be negative")
	}
	if options.MaxOutput < 0 {
		panic("modal backend: max output cannot be negative")
	}
	if options.MaxFileSize < 0 {
		panic("modal backend: max file size cannot be negative")
	}
	if uint64(options.MaxFileSize) > uint64(^uint(0)>>1) {
		panic("modal backend: max file size exceeds platform integer range")
	}
	if options.MaxResults < 0 {
		panic("modal backend: max results cannot be negative")
	}
	if options.MaxTransferFiles < 0 {
		panic("modal backend: max transfer files cannot be negative")
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

// ID returns the stable Modal sandbox ID captured by New.
func (backend *Backend) ID() string { return backend.id }

// Execute runs command through Modal's upstream-compatible bash -c boundary.
// A zero timeout selects the 30-minute default.
func (backend *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("modal backend: execute timeout cannot be negative")
	}
	if timeout == 0 {
		timeout = backend.defaultTimeout
	}
	return backend.execute(ctx, command, timeout)
}

// ExecuteWithOptions preserves omitted versus explicit-zero timeout semantics.
// Modal interprets an explicit zero as waiting indefinitely.
func (backend *Backend) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	timeout := backend.defaultTimeout
	if options.Timeout != nil {
		if *options.Timeout < 0 {
			return dabackend.ExecuteResult{}, fmt.Errorf("modal backend: execute timeout cannot be negative")
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
		return dabackend.ExecuteResult{}, fmt.Errorf("modal backend: command is required")
	}
	result, err := backend.transport.Execute(ctx, backend.id, []string{"bash", "-c", command}, timeout, backend.maxOutput)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return dabackend.ExecuteResult{}, ctxErr
		}
		return dabackend.ExecuteResult{}, wrapTransportError("modal backend: execute", err)
	}
	output := result.Stdout
	if result.Stderr != "" {
		if output != "" {
			output += "\n"
		}
		output += result.Stderr
	}
	truncated := result.Truncated
	if len(output) > backend.maxOutput {
		output = validPrefix(output, backend.maxOutput)
		truncated = true
	}
	exitCode := result.ExitCode
	return dabackend.ExecuteResult{Output: output, ExitCode: &exitCode, Truncated: truncated}, nil
}

// Upload writes files sequentially. Modal paths must be absolute, matching the
// upstream adapter. Cancellation prevents subsequent remote writes.
func (backend *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "modal backend: transfer batch limit exceeded"
		case !strings.HasPrefix(upload.Path, "/"):
			results[index].Error = "invalid_path"
		case int64(len(upload.Content)) > backend.maxFileSize:
			results[index].Error = fmt.Sprintf("modal backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
		case ctx.Err() != nil:
			results[index].Error = ctx.Err().Error()
		default:
			err := backend.transport.Upload(ctx, backend.id, upload.Path, upload.Content, backend.maxFileSize)
			if ctxErr := ctx.Err(); ctxErr != nil {
				results[index].Error = ctxErr.Error()
			} else {
				results[index].Error = fileError(err, false)
			}
		}
	}
	return results
}

// Download reads files sequentially through Modal's native file API. Paths and
// payloads are bounded before they are exposed to callers.
func (backend *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	for index, path := range paths {
		results[index].Path = path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "modal backend: transfer batch limit exceeded"
		case !strings.HasPrefix(path, "/"):
			results[index].Error = "invalid_path"
		case ctx.Err() != nil:
			results[index].Error = ctx.Err().Error()
		default:
			content, err := backend.transport.Download(ctx, backend.id, path, backend.maxFileSize)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					results[index].Error = ctxErr.Error()
				} else {
					results[index].Error = fileError(err, true)
				}
				continue
			}
			if int64(len(content)) > backend.maxFileSize {
				results[index].Error = fmt.Sprintf("modal backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
				continue
			}
			results[index].Content = append([]byte(nil), content...)
		}
	}
	return results
}

func fileError(err error, reading bool) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err.Error()
	case errors.Is(err, ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, ErrIsDirectory) && reading:
		return "is_directory"
	case errors.Is(err, ErrFileNotFound), errors.Is(err, ErrIsDirectory):
		return "file_not_found"
	default:
		return "modal backend: " + boundedError(err)
	}
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
