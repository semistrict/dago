package browserapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/dabackend"
)

const defaultBrowserShellTimeout = 120 * time.Second

// ShellRequest is the portable boundary between the Go agent and a browser
// shell implementation. Files contains the complete virtual workspace before
// execution; the response returns the complete workspace after execution.
type ShellRequest struct {
	Command             string                        `json:"command"`
	Cwd                 string                        `json:"cwd"`
	TimeoutMilliseconds int64                         `json:"timeout_milliseconds"`
	Files               map[string]dabackend.FileData `json:"files"`
}

// ShellResponse is returned by the browser shell implementation.
type ShellResponse struct {
	Stdout    string                        `json:"stdout"`
	Stderr    string                        `json:"stderr"`
	ExitCode  int                           `json:"exit_code"`
	Truncated bool                          `json:"truncated,omitempty"`
	Files     map[string]dabackend.FileData `json:"files"`
}

// ShellExecutor runs a command without granting access to the host process or
// host filesystem. The WASM entrypoint supplies an implementation backed by
// just-bash in the dedicated browser worker.
type ShellExecutor func(context.Context, ShellRequest) (ShellResponse, error)

// browserWorkspace serializes file operations with shell executions. This
// prevents a parallel file tool from being lost when a shell result replaces
// the just-bash workspace snapshot.
type browserWorkspace struct {
	mu     sync.Mutex
	memory *dabackend.Memory
}

func newBrowserWorkspace(files map[string]dabackend.FileData) (*browserWorkspace, error) {
	memory, err := dabackend.NewMemory(files)
	if err != nil {
		return nil, err
	}
	return &browserWorkspace{memory: memory}, nil
}

func (workspace *browserWorkspace) List(ctx context.Context, path string) (dabackend.ListResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.List(ctx, path)
}

func (workspace *browserWorkspace) Read(ctx context.Context, path string, offset, limit int) (dabackend.ReadResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Read(ctx, path, offset, limit)
}

func (workspace *browserWorkspace) Write(ctx context.Context, path, content string) (dabackend.WriteResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Write(ctx, path, content)
}

func (workspace *browserWorkspace) Edit(ctx context.Context, path, old, replacement string, replaceAll bool) (dabackend.EditResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Edit(ctx, path, old, replacement, replaceAll)
}

func (workspace *browserWorkspace) Delete(ctx context.Context, path string) (dabackend.DeleteResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Delete(ctx, path)
}

func (workspace *browserWorkspace) Glob(ctx context.Context, pattern, path string) (dabackend.GlobResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Glob(ctx, pattern, path)
}

func (workspace *browserWorkspace) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Grep(ctx, pattern, options)
}

func (workspace *browserWorkspace) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Upload(ctx, uploads)
}

func (workspace *browserWorkspace) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Download(ctx, paths)
}

func (workspace *browserWorkspace) Snapshot() map[string]dabackend.FileData {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.memory.Snapshot()
}

func (workspace *browserWorkspace) Replace(files map[string]dabackend.FileData) error {
	memory, err := dabackend.NewMemory(files)
	if err != nil {
		return err
	}
	workspace.mu.Lock()
	workspace.memory = memory
	workspace.mu.Unlock()
	return nil
}

type browserSandbox struct {
	*browserWorkspace
	shellMu sync.Mutex
	execute ShellExecutor
}

func (sandbox *browserSandbox) ID() string { return "browser-just-bash" }

func (sandbox *browserSandbox) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout == 0 {
		timeout = defaultBrowserShellTimeout
	}
	return sandbox.executeCommand(ctx, command, timeout)
}

func (sandbox *browserSandbox) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	timeout := defaultBrowserShellTimeout
	if options.Timeout != nil {
		timeout = *options.Timeout
	}
	return sandbox.executeCommand(ctx, command, timeout)
}

func (sandbox *browserSandbox) executeCommand(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if strings.TrimSpace(command) == "" {
		code := 1
		return dabackend.ExecuteResult{Output: "Error: Command must be a non-empty string.", ExitCode: &code}, nil
	}
	sandbox.shellMu.Lock()
	defer sandbox.shellMu.Unlock()
	before := sandbox.Snapshot()
	request := ShellRequest{Command: command, Cwd: workspaceRoot, Files: before}
	if timeout > 0 {
		request.TimeoutMilliseconds = timeout.Milliseconds()
	}
	response, err := sandbox.execute(ctx, request)
	if err != nil {
		return dabackend.ExecuteResult{}, err
	}
	if response.Files == nil {
		return dabackend.ExecuteResult{}, fmt.Errorf("browser shell returned no workspace snapshot")
	}
	if err := sandbox.mergeShellFiles(ctx, before, response.Files); err != nil {
		return dabackend.ExecuteResult{}, err
	}
	output := formatBrowserShellOutput(response.Stdout, response.Stderr)
	code := response.ExitCode
	return dabackend.ExecuteResult{Output: output, ExitCode: &code, Truncated: response.Truncated}, nil
}

func (sandbox *browserSandbox) mergeShellFiles(ctx context.Context, before, files map[string]dabackend.FileData) error {
	for path := range files {
		if !strings.HasPrefix(path, workspaceRoot+"/") {
			return fmt.Errorf("browser shell returned path outside %s: %q", workspaceRoot, path)
		}
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	for path, previous := range before {
		updated, exists := files[path]
		if exists && sameShellFile(previous, updated) {
			continue
		}
		if !exists {
			if _, err := sandbox.memory.Delete(ctx, path); err != nil {
				return err
			}
		}
	}
	for path, file := range files {
		if previous, exists := before[path]; exists && sameShellFile(previous, file) {
			continue
		}
		content := []byte(file.Content)
		if file.Encoding == dabackend.EncodingBase64 {
			decoded, err := base64.StdEncoding.DecodeString(file.Content)
			if err != nil {
				return fmt.Errorf("decode browser shell file %q: %w", path, err)
			}
			content = decoded
		} else if file.Encoding != "" && file.Encoding != dabackend.EncodingUTF8 {
			return fmt.Errorf("browser shell file %q has unsupported encoding %q", path, file.Encoding)
		}
		result := sandbox.memory.Upload(ctx, []dabackend.Upload{{Path: path, Content: content}})[0]
		if result.Error != "" {
			return fmt.Errorf("store browser shell file %q: %s", path, result.Error)
		}
	}
	return nil
}

func sameShellFile(left, right dabackend.FileData) bool {
	return left.Content == right.Content && left.Encoding == right.Encoding
}

func formatBrowserShellOutput(stdout, stderr string) string {
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
	if len(parts) == 0 {
		return "<no output>"
	}
	return strings.Join(parts, "\n")
}
