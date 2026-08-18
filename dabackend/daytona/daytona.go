// Package daytona adapts caller-supplied Daytona transports to dago's sandbox
// backend. It bundles no vendor SDK and does not discover credentials or create
// remote sandboxes.
package daytona

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/semistrict/dago/dabackend"
)

const (
	defaultTimeout         = 30 * time.Minute
	defaultPollInterval    = 100 * time.Millisecond
	defaultMaxPollInterval = 30 * time.Second
	defaultCleanupTimeout  = 10 * time.Second
	defaultMaxOutput       = 1 << 20
	defaultMaxFileSize     = 10 << 20
	defaultMaxResults      = 1000
	defaultMaxBatchFiles   = 1000
	timeoutExitCode        = 124
)

// CommandStatus reports command completion. A nil ExitCode means the command
// is still running.
type CommandStatus struct {
	ExitCode *int
}

// CommandLogs is the bounded stdout/stderr result returned after completion.
type CommandLogs struct {
	Stdout    string
	Stderr    string
	Truncated bool
}

// DownloadedFile is one native batch-download response. Found false maps to a
// stable file_not_found result.
type DownloadedFile struct {
	Path    string
	Content []byte
	Found   bool
}

// Transport is the narrow authenticated Daytona API needed by Backend.
// Implementations must honor ctx and enforce maxOutput/maxBytes before
// allocating or returning data; Backend checks the bounds again.
type Transport interface {
	CreateSession(context.Context, string, string) error
	ExecuteSessionCommand(context.Context, string, string, string, time.Duration) (string, error)
	GetSessionCommand(context.Context, string, string, string) (CommandStatus, error)
	GetSessionCommandLogs(context.Context, string, string, string, int) (CommandLogs, error)
	DeleteSession(context.Context, string, string) error
	UploadFiles(context.Context, string, []dabackend.Upload, int64) error
	DownloadFiles(context.Context, string, []string, int64) ([]DownloadedFile, error)
}

// PollingStrategy returns the delay before the next command-status request.
// elapsed is measured since remote command submission.
type PollingStrategy func(elapsed time.Duration) time.Duration

// SessionIDFactory creates a unique remote session ID for one command.
type SessionIDFactory func() (string, error)

// Options configures finite command, polling, cleanup, and transfer bounds.
// Zero values select useful partner-compatible defaults. An explicit zero
// command timeout is supplied per call through ExecuteWithOptions.
type Options struct {
	DefaultTimeout       time.Duration
	PollingStrategy      PollingStrategy
	MaxPollInterval      time.Duration
	CleanupTimeout       time.Duration
	MaxOutput            int
	MaxFileSize          int64
	MaxResults           int
	MaxTransferFiles     int
	EnableCaptureOffload bool
	SessionID            SessionIDFactory
}

// Backend is a complete sandbox bound to one stable Daytona sandbox ID.
type Backend struct {
	*dabackend.BaseSandbox

	id               string
	transport        Transport
	defaultTimeout   time.Duration
	poll             PollingStrategy
	maxPollInterval  time.Duration
	cleanupTimeout   time.Duration
	maxOutput        int
	maxFileSize      int64
	maxTransferFiles int
	sessionID        SessionIDFactory
	now              func() time.Time
	wait             func(context.Context, time.Duration) error
}

// New wraps an existing sandbox. transport and sandboxID are mandatory static
// dependencies, so invalid values panic instead of returning an error.
func New(transport Transport, sandboxID string, options Options) *Backend {
	if isNil(transport) {
		panic("daytona backend: transport is required")
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		panic("daytona backend: sandbox id is required")
	}
	applyOptions(&options)
	backend := &Backend{
		id:               sandboxID,
		transport:        transport,
		defaultTimeout:   options.DefaultTimeout,
		poll:             options.PollingStrategy,
		maxPollInterval:  options.MaxPollInterval,
		cleanupTimeout:   options.CleanupTimeout,
		maxOutput:        options.MaxOutput,
		maxFileSize:      options.MaxFileSize,
		maxTransferFiles: options.MaxTransferFiles,
		sessionID:        options.SessionID,
		now:              time.Now,
		wait:             waitContext,
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
	if options.DefaultTimeout < 0 || options.MaxPollInterval < 0 || options.CleanupTimeout < 0 {
		panic("daytona backend: durations cannot be negative")
	}
	if options.MaxOutput < 0 || options.MaxFileSize < 0 || options.MaxResults < 0 || options.MaxTransferFiles < 0 {
		panic("daytona backend: limits cannot be negative")
	}
	if uint64(options.MaxFileSize) > uint64(^uint(0)>>1) {
		panic("daytona backend: max file size exceeds platform integer range")
	}
	if options.DefaultTimeout == 0 {
		options.DefaultTimeout = defaultTimeout
	}
	if options.PollingStrategy == nil {
		options.PollingStrategy = func(time.Duration) time.Duration { return defaultPollInterval }
	}
	if options.MaxPollInterval == 0 {
		options.MaxPollInterval = defaultMaxPollInterval
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = defaultCleanupTimeout
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
	if options.SessionID == nil {
		options.SessionID = randomSessionID
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

// ID returns the stable sandbox ID captured by New.
func (backend *Backend) ID() string { return backend.id }

// Execute runs command with the configured default when timeout is zero.
func (backend *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: execute timeout cannot be negative")
	}
	if timeout == 0 {
		timeout = backend.defaultTimeout
	}
	return backend.execute(ctx, command, timeout)
}

// ExecuteWithOptions preserves Daytona's explicit-zero meaning of waiting
// indefinitely while an omitted timeout selects the configured default.
func (backend *Backend) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	timeout := backend.defaultTimeout
	if options.Timeout != nil {
		if *options.Timeout < 0 {
			return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: execute timeout cannot be negative")
		}
		timeout = *options.Timeout
	}
	return backend.execute(ctx, command, timeout)
}

func (backend *Backend) execute(ctx context.Context, command string, timeout time.Duration) (response dabackend.ExecuteResult, err error) {
	if err := ctx.Err(); err != nil {
		return dabackend.ExecuteResult{}, err
	}
	if strings.TrimSpace(command) == "" {
		return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: command is required")
	}
	sessionID, err := backend.sessionID()
	if err != nil {
		return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: create session id: %w", err)
	}
	if strings.TrimSpace(sessionID) == "" {
		return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: session id factory returned an empty id")
	}
	if err = backend.transport.CreateSession(ctx, backend.id, sessionID); err != nil {
		return dabackend.ExecuteResult{}, backend.remoteError(ctx, "create session", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backend.cleanupTimeout)
		defer cancel()
		cleanupErr := backend.transport.DeleteSession(cleanupCtx, backend.id, sessionID)
		if cleanupErr != nil {
			cleanupErr = wrapTransportError("daytona backend: delete session", cleanupErr)
			if err == nil {
				err = cleanupErr
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dabackend.ExecuteResult{}, ctxErr
	}

	commandID, callErr := backend.transport.ExecuteSessionCommand(ctx, backend.id, sessionID, command, timeout)
	if callErr != nil {
		return dabackend.ExecuteResult{}, backend.remoteError(ctx, "execute session command", callErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return dabackend.ExecuteResult{}, ctxErr
	}
	if strings.TrimSpace(commandID) == "" {
		return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: transport returned an empty command id")
	}
	startedAt := backend.now()
	for {
		elapsed := backend.now().Sub(startedAt)
		if timeout != 0 && elapsed >= timeout {
			exitCode := timeoutExitCode
			return dabackend.ExecuteResult{
				Output:   timeoutMessage(timeout),
				ExitCode: &exitCode,
			}, nil
		}
		status, statusErr := backend.transport.GetSessionCommand(ctx, backend.id, sessionID, commandID)
		if statusErr != nil {
			return dabackend.ExecuteResult{}, backend.remoteError(ctx, "get session command", statusErr)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return dabackend.ExecuteResult{}, ctxErr
		}
		if status.ExitCode != nil {
			logs, logsErr := backend.transport.GetSessionCommandLogs(ctx, backend.id, sessionID, commandID, backend.maxOutput)
			if logsErr != nil {
				return dabackend.ExecuteResult{}, backend.remoteError(ctx, "get session command logs", logsErr)
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return dabackend.ExecuteResult{}, ctxErr
			}
			output := logs.Stdout
			if stderr := strings.TrimSpace(logs.Stderr); stderr != "" {
				output += "\n<stderr>" + stderr + "</stderr>"
			}
			truncated := logs.Truncated
			if len(output) > backend.maxOutput {
				output = validPrefix(output, backend.maxOutput)
				truncated = true
			}
			exitCode := *status.ExitCode
			return dabackend.ExecuteResult{Output: output, ExitCode: &exitCode, Truncated: truncated}, nil
		}
		delay := backend.poll(elapsed)
		if delay < 0 {
			return dabackend.ExecuteResult{}, fmt.Errorf("daytona backend: polling strategy returned a negative interval")
		}
		if delay > backend.maxPollInterval {
			delay = backend.maxPollInterval
		}
		if err := backend.wait(ctx, delay); err != nil {
			return dabackend.ExecuteResult{}, err
		}
	}
}

func timeoutMessage(timeout time.Duration) string {
	if timeout%time.Second == 0 {
		return fmt.Sprintf("Command timed out after %d seconds", timeout/time.Second)
	}
	return fmt.Sprintf("Command timed out after %s", timeout)
}

func (backend *Backend) remoteError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return wrapTransportError("daytona backend: "+operation, err)
}

// UploadFiles validates absolute Daytona paths, then sends valid files in one
// bounded native batch.
func (backend *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	valid := make([]dabackend.Upload, 0, min(len(uploads), backend.maxTransferFiles))
	validIndexes := make([]int, 0, cap(valid))
	for index, upload := range uploads {
		results[index].Path = upload.Path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "daytona backend: transfer batch limit exceeded"
		case !strings.HasPrefix(upload.Path, "/"):
			results[index].Error = "invalid_path"
		case int64(len(upload.Content)) > backend.maxFileSize:
			results[index].Error = fmt.Sprintf("daytona backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
		default:
			valid = append(valid, dabackend.Upload{Path: upload.Path, Content: append([]byte(nil), upload.Content...)})
			validIndexes = append(validIndexes, index)
		}
	}
	if len(valid) == 0 {
		return results
	}
	if err := ctx.Err(); err != nil {
		setUploadErrors(results, validIndexes, err.Error())
		return results
	}
	err := backend.transport.UploadFiles(ctx, backend.id, valid, backend.maxFileSize)
	if ctxErr := ctx.Err(); ctxErr != nil {
		setUploadErrors(results, validIndexes, ctxErr.Error())
	} else if err != nil {
		setUploadErrors(results, validIndexes, "daytona backend: upload: "+boundedError(err))
	}
	return results
}

func setUploadErrors(results []dabackend.UploadResult, indexes []int, detail string) {
	for _, index := range indexes {
		results[index].Error = detail
	}
}

// Download uses one native batch and rejects missing, duplicate, unexpected,
// or oversized transport responses without exposing ambiguous content.
func (backend *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	validPaths := make([]string, 0, min(len(paths), backend.maxTransferFiles))
	validIndexes := make(map[string][]int)
	for index, path := range paths {
		results[index].Path = path
		switch {
		case index >= backend.maxTransferFiles:
			results[index].Error = "daytona backend: transfer batch limit exceeded"
		case !strings.HasPrefix(path, "/"):
			results[index].Error = "invalid_path"
		default:
			if _, exists := validIndexes[path]; !exists {
				validPaths = append(validPaths, path)
			}
			validIndexes[path] = append(validIndexes[path], index)
		}
	}
	if len(validPaths) == 0 {
		return results
	}
	if err := ctx.Err(); err != nil {
		setDownloadErrors(results, validIndexes, err.Error())
		return results
	}
	downloaded, err := backend.transport.DownloadFiles(ctx, backend.id, validPaths, backend.maxFileSize)
	if ctxErr := ctx.Err(); ctxErr != nil {
		setDownloadErrors(results, validIndexes, ctxErr.Error())
		return results
	}
	if err != nil {
		setDownloadErrors(results, validIndexes, "daytona backend: download: "+boundedError(err))
		return results
	}
	seen := make(map[string]bool, len(downloaded))
	for _, file := range downloaded {
		indexes, expected := validIndexes[file.Path]
		if !expected || seen[file.Path] {
			setDownloadErrors(results, validIndexes, "daytona backend: invalid download response")
			return results
		}
		seen[file.Path] = true
		if !file.Found {
			for _, index := range indexes {
				results[index].Error = "file_not_found"
			}
			continue
		}
		if int64(len(file.Content)) > backend.maxFileSize {
			for _, index := range indexes {
				results[index].Error = fmt.Sprintf("daytona backend: %v (maximum %d bytes)", dabackend.ErrPayloadTooLarge, backend.maxFileSize)
			}
			continue
		}
		for _, index := range indexes {
			results[index].Content = append([]byte(nil), file.Content...)
		}
	}
	for path, indexes := range validIndexes {
		if seen[path] {
			continue
		}
		for _, index := range indexes {
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

func waitContext(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
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
