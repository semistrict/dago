// Package runloop adapts caller-supplied Runloop devbox transports to dago's
// sandbox backend. The package does not discover credentials and never creates
// remote resources unless the caller explicitly invokes Provider.GetOrCreate.
package runloop

import (
	"context"
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

// CommandResult is the transport-neutral result of one devbox command.
// Truncated reports that the transport already discarded output beyond its
// supplied bound.
type CommandResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// SandboxTransport is the narrow remote API needed by Backend. Implementations
// must honor ctx and must not return more than maxOutput or maxBytes. Backend
// checks those bounds again before exposing results.
//
// The sandbox ID, command/path, timeout, and bound are positional because each
// is required to perform the operation safely.
type SandboxTransport interface {
	Execute(context.Context, string, string, time.Duration, int) (CommandResult, error)
	Upload(context.Context, string, string, []byte, int64) error
	Download(context.Context, string, string, int64) ([]byte, error)
}

// Options controls finite command and file-transfer limits. Zero values select
// useful defaults. Negative limits are programmer errors and make New panic.
type Options struct {
	DefaultTimeout       time.Duration
	MaxOutput            int
	MaxFileSize          int64
	MaxResults           int
	MaxTransferFiles     int
	EnableCaptureOffload bool
}

// Backend is a complete dago sandbox backed by one existing Runloop devbox.
// Its ID is copied at construction and remains stable even if the transport's
// own state changes.
type Backend struct {
	*dabackend.BaseSandbox

	id               string
	transport        SandboxTransport
	defaultTimeout   time.Duration
	maxOutput        int
	maxFileSize      int64
	maxTransferFiles int
}

// New wraps an existing devbox. transport and sandboxID are mandatory static
// dependencies, so invalid values panic rather than producing a runtime error.
func New(transport SandboxTransport, sandboxID string, options Options) *Backend {
	if isNil(transport) {
		panic("runloop backend: transport is required")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		panic("runloop backend: sandbox id is required")
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
		panic("runloop backend: default timeout cannot be negative")
	}
	if options.MaxOutput < 0 {
		panic("runloop backend: max output cannot be negative")
	}
	if options.MaxFileSize < 0 {
		panic("runloop backend: max file size cannot be negative")
	}
	if uint64(options.MaxFileSize) > uint64(^uint(0)>>1) {
		panic("runloop backend: max file size exceeds platform integer range")
	}
	if options.MaxResults < 0 {
		panic("runloop backend: max results cannot be negative")
	}
	if options.MaxTransferFiles < 0 {
		panic("runloop backend: max transfer files cannot be negative")
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

// ID returns the stable devbox ID captured by New.
func (backend *Backend) ID() string { return backend.id }

// Execute runs a command. A zero timeout selects the 30-minute default, matching
// the partner backend; use ExecuteWithOptions with an explicit zero to disable
// the transport timeout when the transport supports that behavior.
func (backend *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("runloop backend: execute timeout cannot be negative")
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
			return dabackend.ExecuteResult{}, fmt.Errorf("runloop backend: execute timeout cannot be negative")
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
		return dabackend.ExecuteResult{}, fmt.Errorf("runloop backend: command is required")
	}
	result, err := backend.transport.Execute(ctx, backend.id, command, timeout, backend.maxOutput)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dabackend.ExecuteResult{}, ctxErr
	}
	if err != nil {
		return dabackend.ExecuteResult{}, wrapTransportError("runloop backend: execute", err)
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

// Upload transfers files sequentially so cancellation prevents later remote
// writes. Each item and the number of remote operations are independently
// bounded; one failure does not discard other per-file results.
func (backend *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "runloop backend: transfer batch limit exceeded"
		case int64(len(upload.Content)) > backend.maxFileSize:
			results[index].Error = fmt.Sprintf("runloop backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
		case ctx.Err() != nil:
			results[index].Error = ctx.Err().Error()
		default:
			err := backend.transport.Upload(ctx, backend.id, upload.Path, upload.Content, backend.maxFileSize)
			if ctxErr := ctx.Err(); ctxErr != nil {
				results[index].Error = ctxErr.Error()
			} else if err != nil {
				results[index].Error = "runloop backend: upload: " + boundedError(err)
			}
		}
	}
	return results
}

// Download uses the transport's native bounded file API rather than routing
// arbitrary bytes through command output.
func (backend *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	for index, path := range paths {
		results[index].Path = path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "runloop backend: transfer batch limit exceeded"
		case ctx.Err() != nil:
			results[index].Error = ctx.Err().Error()
		default:
			content, err := backend.transport.Download(ctx, backend.id, path, backend.maxFileSize)
			if ctxErr := ctx.Err(); ctxErr != nil {
				results[index].Error = ctxErr.Error()
				continue
			}
			if err != nil {
				results[index].Error = "runloop backend: download: " + boundedError(err)
				continue
			}
			if int64(len(content)) > backend.maxFileSize {
				results[index].Error = fmt.Sprintf("runloop backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
				continue
			}
			results[index].Content = append([]byte(nil), content...)
		}
	}
	return results
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
