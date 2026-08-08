package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
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
	if timeout == 0 {
		timeout = shell.defaultTimeout
	}
	executionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	process := exec.CommandContext(executionContext, shell.shell, "-c", command)
	process.Dir = shell.root
	var output bytes.Buffer
	process.Stdout, process.Stderr = &output, &output
	err := process.Run()
	text := output.String()
	truncated := false
	if len(text) > shell.maxOutput {
		text = text[:shell.maxOutput]
		truncated = true
	}
	result := ExecuteResult{Output: text, Truncated: truncated}
	if err == nil {
		code := 0
		result.ExitCode = &code
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
