package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
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
	env            []string
}

type LocalShellOptions struct {
	Filesystem     FilesystemOptions
	ID             string
	Shell          string
	DefaultTimeout time.Duration
	MaxOutput      int
	// Env is the complete command environment unless InheritEnv is true, in
	// which case these values override a snapshot of the process environment.
	Env        map[string]string
	InheritEnv bool
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
		options.DefaultTimeout = 120 * time.Second
	}
	if options.MaxOutput <= 0 {
		options.MaxOutput = 100_000
	}
	environment, err := shellEnvironment(options.Env, options.InheritEnv)
	if err != nil {
		return nil, err
	}
	return &LocalShell{Filesystem: filesystem, id: options.ID, shell: options.Shell, defaultTimeout: options.DefaultTimeout, maxOutput: options.MaxOutput, env: environment}, nil
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
	process.Env = append([]string(nil), shell.env...)
	stdout := cappedOutput{max: shell.maxOutput + 1}
	stderr := cappedOutput{max: shell.maxOutput + 1}
	process.Stdout, process.Stderr = &stdout, &stderr
	err := process.Run()
	text, truncated := formatShellOutput(stdout.String(), stderr.String(), shell.maxOutput)
	result := ExecuteResult{Output: text, Truncated: truncated || stdout.Truncated() || stderr.Truncated()}
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
			result.Output = fmt.Sprintf("Error: Command timed out after %s (custom timeout). The command may be stuck or require more time.", timeout)
		} else {
			result.Output = fmt.Sprintf("Error: Command timed out after %s. For long-running commands, re-run using the timeout parameter.", timeout)
		}
		result.Truncated = false
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		result.ExitCode = &code
		result.Output = fmt.Sprintf("%s\n\nExit code: %d", strings.TrimRight(result.Output, " \t\r\n"), code)
		return result, nil
	}
	if executionContext.Err() != nil {
		return result, executionContext.Err()
	}
	return result, err
}

func shellEnvironment(overrides map[string]string, inherit bool) ([]string, error) {
	values := make(map[string]string, len(overrides))
	if inherit {
		for _, entry := range os.Environ() {
			name, value, ok := strings.Cut(entry, "=")
			if ok {
				values[name] = value
			}
		}
	}
	for name, value := range overrides {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return nil, fmt.Errorf("invalid environment variable name %q", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("environment variable %q contains a NUL byte", name)
		}
		values[name] = value
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+values[name])
	}
	return result, nil
}

func formatShellOutput(stdout, stderr string, max int) (string, bool) {
	parts := make([]string, 0, 2)
	if stdout != "" {
		parts = append(parts, stdout)
	}
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		lines := strings.Split(trimmed, "\n")
		for index := range lines {
			lines[index] = "[stderr] " + lines[index]
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	output := "<no output>"
	if len(parts) > 0 {
		output = strings.Join(parts, "\n")
	}
	if len(output) <= max {
		return output, false
	}
	return output[:max] + fmt.Sprintf("\n\n... Output truncated at %d bytes.", max), true
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
