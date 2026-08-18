// Package docker provides an explicitly constructed sandbox backed by a local
// Docker Engine. It uses the official Engine Go client and never pulls images.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	engineclient "github.com/moby/moby/client"
	"github.com/semistrict/dago/dabackend"
)

const (
	defaultContainerWorkdir = "/workspace"
	defaultMemoryBytes      = 512 << 20
	defaultCPUs             = 1.0
	defaultPidsLimit        = 256
	defaultTimeout          = 120 * time.Second
	defaultMaxOutput        = 100_000
	defaultCleanupTimeout   = 30 * time.Second
)

// Options controls a Docker sandbox created and owned by New or NewWithClient.
type Options struct {
	// Name optionally assigns a container name. Docker generates one otherwise.
	Name string
	// Workspace optionally supplies the host directory mounted at Workdir. When
	// omitted, the backend creates a private temporary directory removed by Close.
	Workspace string
	// Workdir defaults to /workspace.
	Workdir string
	// User defaults to the current numeric uid:gid on Unix. Set it explicitly
	// when the image requires a different identity.
	User string
	// Env adds container environment variables. HOME defaults to Workdir unless
	// Env supplies it.
	Env map[string]string
	// Network defaults to none. Set it explicitly to grant network access.
	Network string
	// WritableRoot opts out of the read-only container root filesystem. The
	// workspace and /tmp remain writable either way.
	WritableRoot bool
	// Resource limits use secure non-zero defaults. Negative values are invalid.
	// A zero value selects the documented default rather than disabling a limit.
	MemoryBytes int64
	CPUs        float64
	PidsLimit   int64

	DefaultTimeout time.Duration
	MaxOutput      int
	MaxFileSize    int64
	MaxVideoSize   int64
	MaxResults     int
}

// Backend owns one Docker container and its mounted workspace. File operations
// are confined to the workspace; Execute runs inside the container.
type Backend struct {
	filesystem     *dabackend.Filesystem
	engine         engineAPI
	closeEngine    func() error
	containerID    string
	workspace      string
	ownsWorkspace  bool
	workdir        string
	defaultTimeout time.Duration
	maxOutput      int

	operation    chan struct{}
	closed       bool
	engineClosed bool
}

type engineAPI interface {
	ContainerCreate(context.Context, engineclient.ContainerCreateOptions) (engineclient.ContainerCreateResult, error)
	ContainerStart(context.Context, string, engineclient.ContainerStartOptions) (engineclient.ContainerStartResult, error)
	ContainerKill(context.Context, string, engineclient.ContainerKillOptions) (engineclient.ContainerKillResult, error)
	ContainerRemove(context.Context, string, engineclient.ContainerRemoveOptions) (engineclient.ContainerRemoveResult, error)
	ExecCreate(context.Context, string, engineclient.ExecCreateOptions) (engineclient.ExecCreateResult, error)
	ExecAttach(context.Context, string, engineclient.ExecAttachOptions) (engineclient.ExecAttachResult, error)
	ExecInspect(context.Context, string, engineclient.ExecInspectOptions) (engineclient.ExecInspectResult, error)
}

type commandResult struct {
	stdout    string
	stderr    string
	exitCode  int
	truncated bool
}

// New connects to Docker using the standard environment variables and API
// version negotiation, then creates and starts one owned container. Closing the
// backend also closes the Engine client created by this function.
func New(ctx context.Context, image string, options Options) (*Backend, error) {
	if nilDependency(ctx) {
		panic("docker sandbox: context is required")
	}
	validateStatic(image, options)
	engine, err := engineclient.New(engineclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: create Engine client: %w", err)
	}
	backend, err := newBackend(ctx, image, options, engine, engine.Close)
	if err != nil {
		_ = engine.Close()
		return nil, err
	}
	return backend, nil
}

// NewWithClient creates an owned container using a caller-managed Engine
// client. Closing the backend removes the container but does not close client.
func NewWithClient(ctx context.Context, client *engineclient.Client, image string, options Options) (*Backend, error) {
	if nilDependency(ctx) {
		panic("docker sandbox: context is required")
	}
	if client == nil {
		panic("docker sandbox: Engine client is required")
	}
	validateStatic(image, options)
	return newBackend(ctx, image, options, client, nil)
}

func nilDependency(value any) bool {
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

func newBackend(ctx context.Context, image string, options Options, engine engineAPI, closeEngine func() error) (*Backend, error) {
	if nilDependency(ctx) {
		panic("docker sandbox: context is required")
	}
	if engine == nil {
		panic("docker sandbox: Engine client is required")
	}
	validateStatic(image, options)
	image = strings.TrimSpace(image)
	if options.Workdir == "" {
		options.Workdir = defaultContainerWorkdir
	}
	if options.Network == "" {
		options.Network = "none"
	}
	if options.DefaultTimeout == 0 {
		options.DefaultTimeout = defaultTimeout
	}
	if options.MaxOutput == 0 {
		options.MaxOutput = defaultMaxOutput
	}
	if options.MemoryBytes == 0 {
		options.MemoryBytes = defaultMemoryBytes
	}
	if options.CPUs == 0 {
		options.CPUs = defaultCPUs
	}
	if options.PidsLimit == 0 {
		options.PidsLimit = defaultPidsLimit
	}

	workspace, ownsWorkspace, err := prepareWorkspace(options.Workspace)
	if err != nil {
		return nil, err
	}
	cleanupWorkspace := func() {
		if ownsWorkspace {
			_ = os.RemoveAll(workspace)
		}
	}
	filesystem, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{
		Root: workspace, MaxFileSize: options.MaxFileSize, MaxVideoSize: options.MaxVideoSize, MaxResults: options.MaxResults,
	})
	if err != nil {
		cleanupWorkspace()
		return nil, fmt.Errorf("docker sandbox: workspace backend: %w", err)
	}
	containerUser := options.User
	if containerUser == "" {
		containerUser, err = currentContainerUser()
		if err != nil {
			cleanupWorkspace()
			return nil, err
		}
	}
	created, err := engine.ContainerCreate(ctx, createOptions(image, options, workspace, containerUser))
	if err != nil {
		cleanupWorkspace()
		return nil, fmt.Errorf("docker sandbox: create container: %w", err)
	}
	containerID := strings.TrimSpace(created.ID)
	if containerID == "" || strings.ContainsAny(containerID, " \t\r\n\x00") {
		cleanupWorkspace()
		return nil, fmt.Errorf("docker sandbox: create returned an invalid container id %q", containerID)
	}
	if _, err := engine.ContainerStart(ctx, containerID, engineclient.ContainerStartOptions{}); err != nil {
		cleanupContext, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
		_, _ = engine.ContainerRemove(cleanupContext, containerID, engineclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		cancel()
		cleanupWorkspace()
		return nil, fmt.Errorf("docker sandbox: start container: %w", err)
	}
	return &Backend{
		filesystem: filesystem, engine: engine, closeEngine: closeEngine,
		containerID: containerID, workspace: workspace, ownsWorkspace: ownsWorkspace,
		workdir: options.Workdir, defaultTimeout: options.DefaultTimeout,
		maxOutput: options.MaxOutput, operation: make(chan struct{}, 1),
	}, nil
}

func validateStatic(image string, options Options) {
	if strings.TrimSpace(image) == "" {
		panic("docker sandbox: image is required")
	}
	workdir := options.Workdir
	if workdir == "" {
		workdir = defaultContainerWorkdir
	}
	if workdir == "/" || workdir == "/tmp" || !strings.HasPrefix(workdir, "/") || path.Clean(workdir) != workdir || strings.ContainsRune(workdir, '\x00') {
		panic(fmt.Sprintf("docker sandbox: invalid container workdir %q", workdir))
	}
	for name, value := range options.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
			panic(fmt.Sprintf("docker sandbox: invalid environment variable %q", name))
		}
	}
	if options.DefaultTimeout < 0 || options.MaxOutput < 0 || options.MaxFileSize < 0 || options.MaxVideoSize < 0 || options.MaxResults < 0 {
		panic("docker sandbox: durations and bounds cannot be negative")
	}
	if options.MemoryBytes < 0 || options.CPUs < 0 || math.IsNaN(options.CPUs) || math.IsInf(options.CPUs, 0) || options.CPUs > float64(math.MaxInt64)/1e9 || options.PidsLimit < 0 {
		panic("docker sandbox: resource limits are invalid")
	}
}

func createOptions(image string, options Options, workspace, containerUser string) engineclient.ContainerCreateOptions {
	environment := make(map[string]string, len(options.Env)+1)
	for name, value := range options.Env {
		environment[name] = value
	}
	if _, exists := environment["HOME"]; !exists {
		environment["HOME"] = options.Workdir
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	variables := make([]string, 0, len(names))
	for _, name := range names {
		variables = append(variables, name+"="+environment[name])
	}
	pidsLimit := options.PidsLimit
	initProcess := true
	return engineclient.ContainerCreateOptions{
		Name: options.Name,
		Config: &container.Config{
			Image: image, User: containerUser, Env: variables,
			WorkingDir: options.Workdir, Entrypoint: []string{"/bin/sh"},
			Cmd:             []string{"-c", "while :; do sleep 3600; done"},
			NetworkDisabled: options.Network == "none",
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(options.Network),
			ReadonlyRootfs: !options.WritableRoot,
			CapDrop:        []string{"ALL"}, SecurityOpt: []string{"no-new-privileges=true"},
			Tmpfs:  map[string]string{"/tmp": "rw,nosuid,nodev,exec,mode=1777,size=67108864"},
			Mounts: []mount.Mount{{Type: mount.TypeBind, Source: workspace, Target: options.Workdir}},
			Resources: container.Resources{
				Memory: options.MemoryBytes, MemorySwap: options.MemoryBytes,
				NanoCPUs: int64(options.CPUs * 1e9), PidsLimit: &pidsLimit,
			},
			Init: &initProcess,
		},
	}
}

func prepareWorkspace(configured string) (string, bool, error) {
	if configured == "" {
		workspace, err := os.MkdirTemp("", "dago-docker-")
		if err != nil {
			return "", false, fmt.Errorf("docker sandbox: create workspace: %w", err)
		}
		return workspace, true, nil
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", false, fmt.Errorf("docker sandbox: resolve workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", false, fmt.Errorf("docker sandbox: resolve workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, fmt.Errorf("docker sandbox: inspect workspace: %w", err)
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("docker sandbox: workspace %q is not a directory", configured)
	}
	return resolved, false, nil
}

func currentContainerUser() (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("docker sandbox: determine current user: %w", err)
	}
	if _, err := strconv.ParseUint(current.Uid, 10, 32); err != nil {
		return "", fmt.Errorf("docker sandbox: current uid %q is not numeric", current.Uid)
	}
	if _, err := strconv.ParseUint(current.Gid, 10, 32); err != nil {
		return "", fmt.Errorf("docker sandbox: current gid %q is not numeric", current.Gid)
	}
	return current.Uid + ":" + current.Gid, nil
}

func (backend *Backend) ID() string { return backend.containerID }

// Workspace returns the host directory mounted as the sandbox workspace.
func (backend *Backend) Workspace() string { return backend.workspace }

func (backend *Backend) List(ctx context.Context, directory string) (dabackend.ListResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.ListResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.ListResult{}, err
	}
	return backend.filesystem.List(ctx, directory)
}

func (backend *Backend) Read(ctx context.Context, name string, offset, limit int) (dabackend.ReadResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.ReadResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.ReadResult{}, err
	}
	return backend.filesystem.Read(ctx, name, offset, limit)
}

func (backend *Backend) ReadBinary(ctx context.Context, name string, maxBytes int64) (dabackend.ReadResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.ReadResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.ReadResult{}, err
	}
	return backend.filesystem.ReadBinary(ctx, name, maxBytes)
}

func (backend *Backend) Write(ctx context.Context, name, content string) (dabackend.WriteResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.WriteResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.WriteResult{}, err
	}
	return backend.filesystem.Write(ctx, name, content)
}

func (backend *Backend) Edit(ctx context.Context, name, old, replacement string, replaceAll bool) (dabackend.EditResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.EditResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.EditResult{}, err
	}
	return backend.filesystem.Edit(ctx, name, old, replacement, replaceAll)
}

func (backend *Backend) Delete(ctx context.Context, name string) (dabackend.DeleteResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.DeleteResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.DeleteResult{}, err
	}
	return backend.filesystem.Delete(ctx, name)
}

func (backend *Backend) Glob(ctx context.Context, pattern, base string) (dabackend.GlobResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.GlobResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.GlobResult{}, err
	}
	return backend.filesystem.Glob(ctx, pattern, base)
}

func (backend *Backend) Grep(ctx context.Context, pattern string, options dabackend.GrepOptions) (dabackend.GrepResult, error) {
	if err := backend.acquire(ctx); err != nil {
		return dabackend.GrepResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.GrepResult{}, err
	}
	return backend.filesystem.Grep(ctx, pattern, options)
}

func (backend *Backend) Upload(ctx context.Context, uploads []dabackend.Upload) []dabackend.UploadResult {
	if err := backend.acquire(ctx); err != nil {
		return uploadErrors(uploads, err)
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return uploadErrors(uploads, err)
	}
	return backend.filesystem.Upload(ctx, uploads)
}

func (backend *Backend) Download(ctx context.Context, paths []string) []dabackend.DownloadResult {
	if err := backend.acquire(ctx); err != nil {
		return downloadErrors(paths, err)
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return downloadErrors(paths, err)
	}
	return backend.filesystem.Download(ctx, paths)
}

func uploadErrors(uploads []dabackend.Upload, err error) []dabackend.UploadResult {
	results := make([]dabackend.UploadResult, len(uploads))
	for index := range uploads {
		results[index] = dabackend.UploadResult{Path: uploads[index].Path, Error: err.Error()}
	}
	return results
}

func downloadErrors(paths []string, err error) []dabackend.DownloadResult {
	results := make([]dabackend.DownloadResult, len(paths))
	for index := range paths {
		results[index] = dabackend.DownloadResult{Path: paths[index], Error: err.Error()}
	}
	return results
}

func (backend *Backend) acquire(ctx context.Context) error {
	select {
	case backend.operation <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (backend *Backend) release() { <-backend.operation }

func (backend *Backend) requireOpenLocked() error {
	if backend.closed {
		return fmt.Errorf("docker sandbox: container is closed")
	}
	return nil
}

func (backend *Backend) Execute(ctx context.Context, command string, timeout time.Duration) (dabackend.ExecuteResult, error) {
	if timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("docker sandbox: execute timeout cannot be negative")
	}
	if timeout == 0 {
		return backend.execute(ctx, command, backend.defaultTimeout, false)
	}
	return backend.execute(ctx, command, timeout, true)
}

func (backend *Backend) ExecuteWithOptions(ctx context.Context, command string, options dabackend.ExecuteOptions) (dabackend.ExecuteResult, error) {
	if options.Timeout == nil {
		return backend.execute(ctx, command, backend.defaultTimeout, false)
	}
	if *options.Timeout < 0 {
		return dabackend.ExecuteResult{}, fmt.Errorf("docker sandbox: execute timeout cannot be negative")
	}
	return backend.execute(ctx, command, *options.Timeout, true)
}

func (backend *Backend) execute(ctx context.Context, command string, timeout time.Duration, customTimeout bool) (dabackend.ExecuteResult, error) {
	if command == "" {
		code := 1
		return dabackend.ExecuteResult{Output: "Error: Command must be a non-empty string.", ExitCode: &code}, nil
	}
	if err := backend.acquire(ctx); err != nil {
		return dabackend.ExecuteResult{}, err
	}
	defer backend.release()
	if err := backend.requireOpenLocked(); err != nil {
		return dabackend.ExecuteResult{}, err
	}
	executionContext := ctx
	cancel := func() {}
	if timeout > 0 {
		executionContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	run, err := backend.runExec(executionContext, command)
	result := dockerExecuteResult(run, backend.maxOutput)
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		if resetErr := backend.resetLocked(); resetErr != nil {
			return result, errors.Join(ctx.Err(), resetErr)
		}
		return result, ctx.Err()
	}
	if errors.Is(executionContext.Err(), context.DeadlineExceeded) {
		if resetErr := backend.resetLocked(); resetErr != nil {
			return result, errors.Join(executionContext.Err(), resetErr)
		}
		code := 124
		result.ExitCode = &code
		if customTimeout {
			result.Output = fmt.Sprintf("Error: Command timed out after %s (custom timeout). The container was restarted to terminate it.", timeout)
		} else {
			result.Output = fmt.Sprintf("Error: Command timed out after %s. The container was restarted to terminate it; re-run with a custom timeout for long-running commands.", timeout)
		}
		result.Truncated = false
		return result, nil
	}
	return result, fmt.Errorf("docker sandbox: execute: %w", err)
}

func (backend *Backend) runExec(ctx context.Context, command string) (commandResult, error) {
	created, err := backend.engine.ExecCreate(ctx, backend.containerID, engineclient.ExecCreateOptions{
		AttachStdout: true, AttachStderr: true, WorkingDir: backend.workdir,
		Cmd: []string{"/bin/sh", "-c", command},
	})
	if err != nil {
		return commandResult{}, err
	}
	execID := strings.TrimSpace(created.ID)
	if execID == "" || execID != created.ID || strings.ContainsRune(execID, '\x00') {
		return commandResult{}, fmt.Errorf("create exec returned an invalid id %q", created.ID)
	}
	attached, err := backend.engine.ExecAttach(ctx, execID, engineclient.ExecAttachOptions{})
	if err != nil {
		return commandResult{}, err
	}
	defer attached.Close()
	stdout := &cappedBuffer{max: backend.maxOutput + 1}
	stderr := &cappedBuffer{max: backend.maxOutput + 1}
	readDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, attached.Reader)
		readDone <- copyErr
	}()
	select {
	case readErr := <-readDone:
		if ctx.Err() != nil {
			return commandResult{}, ctx.Err()
		}
		if readErr != nil {
			return commandResult{}, readErr
		}
	case <-ctx.Done():
		attached.Close()
		<-readDone
		return commandResult{}, ctx.Err()
	}
	inspected, err := backend.engine.ExecInspect(ctx, execID, engineclient.ExecInspectOptions{})
	if err != nil {
		return commandResult{}, err
	}
	if inspected.Running {
		return commandResult{}, fmt.Errorf("exec %q is still running after its output stream closed", execID)
	}
	return commandResult{
		stdout: stdout.String(), stderr: stderr.String(), exitCode: inspected.ExitCode,
		truncated: stdout.truncated || stderr.truncated,
	}, nil
}

func dockerExecuteResult(run commandResult, maxOutput int) dabackend.ExecuteResult {
	output, truncated := formatOutput(run.stdout, run.stderr, maxOutput)
	code := run.exitCode
	return dabackend.ExecuteResult{Output: output, ExitCode: &code, Truncated: truncated || run.truncated}
}

func formatOutput(stdout, stderr string, limit int) (string, bool) {
	output := stdout
	if stderr != "" {
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}
		output += stderr
	}
	if len(output) <= limit {
		return output, false
	}
	return output[:limit], true
}

func (backend *Backend) resetLocked() error {
	cleanupContext, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()
	if _, err := backend.engine.ContainerKill(cleanupContext, backend.containerID, engineclient.ContainerKillOptions{}); err != nil {
		return fmt.Errorf("docker sandbox: terminate canceled command: %w", err)
	}
	if _, err := backend.engine.ContainerStart(cleanupContext, backend.containerID, engineclient.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker sandbox: restart after canceled command: %w", err)
	}
	return nil
}

// Shutdown removes the owned container and, when the backend created it, its
// temporary workspace. A failed container removal leaves the workspace intact.
func (backend *Backend) Shutdown(ctx context.Context) error {
	if err := backend.acquire(ctx); err != nil {
		return err
	}
	defer backend.release()
	if !backend.closed {
		if _, err := backend.engine.ContainerRemove(ctx, backend.containerID, engineclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return fmt.Errorf("docker sandbox: remove container: %w", err)
		}
		backend.closed = true
	}
	if backend.ownsWorkspace {
		if err := os.RemoveAll(backend.workspace); err != nil {
			return fmt.Errorf("docker sandbox: remove workspace: %w", err)
		}
		backend.ownsWorkspace = false
	}
	if backend.closeEngine != nil && !backend.engineClosed {
		if err := backend.closeEngine(); err != nil {
			return fmt.Errorf("docker sandbox: close Engine client: %w", err)
		}
		backend.engineClosed = true
	}
	return nil
}

// Close removes the container using a bounded background context.
func (backend *Backend) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCleanupTimeout)
	defer cancel()
	return backend.Shutdown(ctx)
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	max       int
	truncated bool
}

func (buffer *cappedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := buffer.max - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.truncated = buffer.truncated || original > 0
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.buffer.Write(value)
	return original, nil
}

func (buffer *cappedBuffer) String() string { return buffer.buffer.String() }

var (
	_ dabackend.Sandbox             = (*Backend)(nil)
	_ dabackend.ConfigurableSandbox = (*Backend)(nil)
)
