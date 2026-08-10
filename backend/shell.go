package backend

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// LocalShell is an explicitly constructed host process capability rooted at a
// Filesystem working directory. It is not a security sandbox.
type LocalShell struct {
	*Filesystem
	id             string
	shell          string
	defaultTimeout time.Duration
	maxOutput      int
}

type LocalShellOptions struct {
	Filesystem     FilesystemOptions
	ID             string
	Shell          string
	DefaultTimeout time.Duration
	MaxOutput      int
}

func NewLocalShell(options LocalShellOptions) (*LocalShell, error) {
	filesystem, err := NewFilesystem(options.Filesystem)
	if err != nil {
		return nil, err
	}
	if options.ID == "" {
		options.ID = "local"
	}
	if options.Shell == "" {
		options.Shell = "/bin/sh"
	}
	if options.DefaultTimeout <= 0 {
		options.DefaultTimeout = 30 * time.Second
	}
	if options.MaxOutput <= 0 {
		options.MaxOutput = 100_000
	}
	return &LocalShell{Filesystem: filesystem, id: options.ID, shell: options.Shell, defaultTimeout: options.DefaultTimeout, maxOutput: options.MaxOutput}, nil
}

func (shell *LocalShell) ID() string { return shell.id }

func (shell *LocalShell) Execute(ctx context.Context, command string, timeout time.Duration) (ExecuteResult, error) {
	if command == "" {
		return ExecuteResult{}, fmt.Errorf("execute command is required")
	}
	if timeout < 0 {
		return ExecuteResult{}, fmt.Errorf("execute timeout cannot be negative")
	}
	customTimeout := timeout > 0
	if !customTimeout {
		timeout = shell.defaultTimeout
	}
	executionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := exec.CommandContext(executionContext, shell.shell, "-c", command)
	process.Dir = shell.root
	output := cappedOutput{max: shell.maxOutput}
	process.Stdout, process.Stderr = &output, &output
	err := process.Run()
	text := output.String()
	result := ExecuteResult{Output: text, Truncated: output.Truncated()}
	if err == nil {
		code := 0
		result.ExitCode = &code
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if errors.Is(executionContext.Err(), context.DeadlineExceeded) {
		code := 124
		result.ExitCode = &code
		if customTimeout {
			result.Output += fmt.Sprintf("\nCommand timed out after %s using the custom timeout; it may be stuck. Inspect the command before retrying.", timeout)
		} else {
			result.Output += fmt.Sprintf("\nCommand timed out after %s. Retry with a larger timeout parameter if the command needs more time.", timeout)
		}
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		result.ExitCode = &code
		return result, nil
	}
	if executionContext.Err() != nil {
		return result, executionContext.Err()
	}
	return result, err
}

// cappedOutput drains process output after retaining max bytes. Returning the
// full input length avoids closing the pipe early and changing the process exit
// status through SIGPIPE.
type cappedOutput struct {
	mu        sync.Mutex
	data      []byte
	max       int
	truncated bool
}

func (output *cappedOutput) Write(value []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	remaining := output.max - len(output.data)
	if remaining > 0 {
		output.data = append(output.data, value[:min(remaining, len(value))]...)
	}
	if len(value) > remaining {
		output.truncated = true
	}
	return len(value), nil
}

func (output *cappedOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return string(output.data)
}

func (output *cappedOutput) Truncated() bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.truncated
}
