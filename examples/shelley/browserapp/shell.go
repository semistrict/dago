package browserapp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/dago/browser/justbash"
	"github.com/semistrict/dago/dabackend"
)

const defaultBrowserShellTimeout = 120 * time.Second

// ShellRequest, ShellResponse, and ShellExecutor retain the application API
// while sharing the reusable WASM shell boundary.
type ShellRequest = justbash.Request
type ShellResponse = justbash.Response
type ShellExecutor = justbash.Executor

// Workspace is the browser application's filesystem contract. Browser WASM
// supplies a bridge backed by the File System Access API and IndexedDB; native
// tests use the bounded in-memory implementation below.
type Workspace interface {
	dabackend.Backend
	CreateDirectory(context.Context, string) error
}

type browserWorkspace struct {
	mu          sync.Mutex
	memory      *dabackend.Memory
	directories map[string]bool
}

func newBrowserWorkspace(files map[string]dabackend.FileData) (*browserWorkspace, error) {
	memory, err := dabackend.LoadMemory(files)
	if err != nil {
		return nil, err
	}
	return &browserWorkspace{memory: memory, directories: browserDirectories(files)}, nil
}

func (workspace *browserWorkspace) List(ctx context.Context, path string) (dabackend.ListResult, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	result, err := workspace.memory.List(ctx, path)
	if err != nil {
		return result, err
	}
	prefix := strings.TrimSuffix(path, "/") + "/"
	seen := make(map[string]bool, len(result.Entries))
	for _, entry := range result.Entries {
		seen[strings.TrimSuffix(entry.Path, "/")] = true
	}
	for directory := range workspace.directories {
		relative := strings.TrimPrefix(directory, prefix)
		if relative == directory || relative == "" || strings.Contains(relative, "/") || seen[directory] {
			continue
		}
		result.Entries = append(result.Entries, dabackend.FileInfo{Path: directory + "/", IsDir: true})
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Path < result.Entries[j].Path })
	return result, nil
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
	memory, err := dabackend.LoadMemory(files)
	if err != nil {
		return err
	}
	workspace.mu.Lock()
	workspace.memory = memory
	workspace.directories = browserDirectories(files)
	workspace.mu.Unlock()
	return nil
}

func (workspace *browserWorkspace) CreateDirectory(ctx context.Context, path string) error {
	path = strings.TrimSuffix(strings.TrimSpace(path), "/")
	if path == "" || (path != workspaceRoot && !strings.HasPrefix(path, workspaceRoot+"/")) {
		return fmt.Errorf("browser directory must be inside %s", workspaceRoot)
	}
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for current := path; current != "" && current != workspaceRoot; {
		workspace.directories[current] = true
		index := strings.LastIndex(current, "/")
		if index <= 0 {
			break
		}
		current = current[:index]
	}
	return nil
}

func (workspace *browserWorkspace) Directories() []string {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	result := make([]string, 0, len(workspace.directories))
	for path := range workspace.directories {
		result = append(result, path)
	}
	return result
}

func browserDirectories(files map[string]dabackend.FileData) map[string]bool {
	result := map[string]bool{workspaceRoot: true}
	for path := range files {
		current := strings.TrimSuffix(path, "/")
		for {
			index := strings.LastIndex(current, "/")
			if index <= 0 {
				break
			}
			current = current[:index]
			if current == workspaceRoot || strings.HasPrefix(current, workspaceRoot+"/") {
				result[current] = true
			}
			if current == workspaceRoot {
				break
			}
		}
	}
	return result
}

type browserSandbox struct {
	Workspace
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
	request := ShellRequest{Command: command, Cwd: workspaceRoot}
	if timeout > 0 {
		request.TimeoutMilliseconds = timeout.Milliseconds()
	}
	response, err := sandbox.execute(ctx, request)
	if err != nil {
		return dabackend.ExecuteResult{}, err
	}
	output := formatBrowserShellOutput(response.Stdout, response.Stderr)
	code := response.ExitCode
	return dabackend.ExecuteResult{Output: output, ExitCode: &code, Truncated: response.Truncated}, nil
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
