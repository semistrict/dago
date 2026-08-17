package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	engineclient "github.com/moby/moby/client"
	"github.com/semistrict/dago/dabackend"
)

type fakeEngine struct {
	mu sync.Mutex

	creates []engineclient.ContainerCreateOptions
	starts  []string
	kills   []string
	removes []string
	execs   []engineclient.ExecCreateOptions

	stdout      string
	stderr      string
	exitCode    int
	execID      string
	execRunning bool
	holdExec    bool
	createError error
	startError  error
	removeError error
	closeError  error
	closeCalls  int
}

func (fake *fakeEngine) ContainerCreate(_ context.Context, options engineclient.ContainerCreateOptions) (engineclient.ContainerCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.creates = append(fake.creates, options)
	if fake.createError != nil {
		return engineclient.ContainerCreateResult{}, fake.createError
	}
	return engineclient.ContainerCreateResult{ID: "container-id"}, nil
}

func (fake *fakeEngine) ContainerStart(_ context.Context, id string, _ engineclient.ContainerStartOptions) (engineclient.ContainerStartResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.starts = append(fake.starts, id)
	return engineclient.ContainerStartResult{}, fake.startError
}

func (fake *fakeEngine) ContainerKill(_ context.Context, id string, _ engineclient.ContainerKillOptions) (engineclient.ContainerKillResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.kills = append(fake.kills, id)
	return engineclient.ContainerKillResult{}, nil
}

func (fake *fakeEngine) ContainerRemove(_ context.Context, id string, _ engineclient.ContainerRemoveOptions) (engineclient.ContainerRemoveResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.removes = append(fake.removes, id)
	return engineclient.ContainerRemoveResult{}, fake.removeError
}

func (fake *fakeEngine) ExecCreate(_ context.Context, _ string, options engineclient.ExecCreateOptions) (engineclient.ExecCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.execs = append(fake.execs, options)
	execID := fake.execID
	if execID == "" {
		execID = "exec-id"
	}
	return engineclient.ExecCreateResult{ID: execID}, nil
}

func (fake *fakeEngine) ExecAttach(ctx context.Context, _ string, _ engineclient.ExecAttachOptions) (engineclient.ExecAttachResult, error) {
	clientConnection, serverConnection := net.Pipe()
	fake.mu.Lock()
	stdout, stderr, hold := fake.stdout, fake.stderr, fake.holdExec
	fake.mu.Unlock()
	go func() {
		defer serverConnection.Close()
		if hold {
			<-ctx.Done()
			return
		}
		if stdout != "" {
			writeDockerFrame(serverConnection, 1, stdout)
		}
		if stderr != "" {
			writeDockerFrame(serverConnection, 2, stderr)
		}
	}()
	return engineclient.ExecAttachResult{HijackedResponse: engineclient.NewHijackedResponse(clientConnection, "application/vnd.docker.multiplexed-stream")}, nil
}

func writeDockerFrame(connection net.Conn, stream byte, value string) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(value)))
	_, _ = connection.Write(append(header, value...))
}

func (fake *fakeEngine) ExecInspect(_ context.Context, _ string, _ engineclient.ExecInspectOptions) (engineclient.ExecInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return engineclient.ExecInspectResult{ID: "exec-id", ContainerID: "container-id", Running: fake.execRunning, ExitCode: fake.exitCode}, nil
}

func (fake *fakeEngine) Close() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.closeCalls++
	return fake.closeError
}

func (fake *fakeEngine) snapshot() (creates []engineclient.ContainerCreateOptions, starts, kills, removes []string, execs []engineclient.ExecCreateOptions, closes int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]engineclient.ContainerCreateOptions(nil), fake.creates...), append([]string(nil), fake.starts...), append([]string(nil), fake.kills...), append([]string(nil), fake.removes...), append([]engineclient.ExecCreateOptions(nil), fake.execs...), fake.closeCalls
}

func newTestBackend(t *testing.T, engine *fakeEngine, mutate func(*Options)) *Backend {
	t.Helper()
	options := Options{Workspace: t.TempDir(), User: "1000:1000"}
	if mutate != nil {
		mutate(&options)
	}
	backend, err := newBackend(context.Background(), "example:local", options, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func TestNewCreatesHardenedOwnedContainer(t *testing.T) {
	workspace := t.TempDir()
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeEngine{}
	backend, err := newBackend(context.Background(),
		"example:local", Options{
			Workspace: workspace, User: "123:456", Name: "sandbox-name",
			Env: map[string]string{"ZED": "last", "ALPHA": "first"},
		}, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if backend.ID() != "container-id" || backend.Workspace() != resolvedWorkspace {
		t.Fatalf("identity = %q, workspace = %q", backend.ID(), backend.Workspace())
	}
	var _ dabackend.Sandbox = backend
	var _ dabackend.ConfigurableSandbox = backend

	creates, starts, _, _, _, _ := engine.snapshot()
	if len(creates) != 1 || !reflect.DeepEqual(starts, []string{"container-id"}) {
		t.Fatalf("create/start = %#v, %#v", creates, starts)
	}
	created := creates[0]
	if created.Name != "sandbox-name" || created.Config == nil || created.HostConfig == nil {
		t.Fatalf("create options = %#v", created)
	}
	if created.Config.Image != "example:local" || created.Config.User != "123:456" || created.Config.WorkingDir != "/workspace" {
		t.Fatalf("container config = %#v", created.Config)
	}
	if !reflect.DeepEqual(created.Config.Entrypoint, []string{"/bin/sh"}) || !reflect.DeepEqual(created.Config.Cmd, []string{"-c", "while :; do sleep 3600; done"}) {
		t.Fatalf("container command = %#v %#v", created.Config.Entrypoint, created.Config.Cmd)
	}
	if !reflect.DeepEqual(created.Config.Env, []string{"ALPHA=first", "HOME=/workspace", "ZED=last"}) || !created.Config.NetworkDisabled {
		t.Fatalf("container env/network = %#v, %v", created.Config.Env, created.Config.NetworkDisabled)
	}
	host := created.HostConfig
	if host.NetworkMode != container.NetworkMode("none") || !host.ReadonlyRootfs || !reflect.DeepEqual(host.CapDrop, []string{"ALL"}) || !reflect.DeepEqual(host.SecurityOpt, []string{"no-new-privileges=true"}) {
		t.Fatalf("host isolation = %#v", host)
	}
	if host.Memory != 536870912 || host.MemorySwap != 536870912 || host.NanoCPUs != 1_000_000_000 || host.PidsLimit == nil || *host.PidsLimit != 256 {
		t.Fatalf("resource limits = %#v", host.Resources)
	}
	if host.Init == nil || !*host.Init || host.Tmpfs["/tmp"] == "" {
		t.Fatalf("init/tmpfs = %#v, %#v", host.Init, host.Tmpfs)
	}
	wantMount := mount.Mount{Type: mount.TypeBind, Source: resolvedWorkspace, Target: "/workspace"}
	if len(host.Mounts) != 1 || !reflect.DeepEqual(host.Mounts[0], wantMount) {
		t.Fatalf("mounts = %#v", host.Mounts)
	}
}

func TestWorkspaceFileOperationsAreSharedAndConfined(t *testing.T) {
	backend := newTestBackend(t, &fakeEngine{}, nil)
	if _, err := backend.Write(context.Background(), "/nested/file.txt", "hello\nworld\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(backend.Workspace(), "nested", "file.txt"))
	if err != nil || string(data) != "hello\nworld\n" {
		t.Fatalf("host file = %q, %v", data, err)
	}
	page, err := backend.Read(context.Background(), "/nested/file.txt", 1, 1)
	if err != nil || page.Data == nil || page.Data.Content != "world\n" {
		t.Fatalf("read = %#v, %v", page, err)
	}
	if _, err := backend.Read(context.Background(), "/../outside", 0, 1); err == nil {
		t.Fatal("workspace traversal succeeded")
	}
}

func TestExecuteUsesEngineExecAndBoundsMultiplexedOutput(t *testing.T) {
	engine := &fakeEngine{stdout: "12345", stderr: "error", exitCode: 7}
	backend := newTestBackend(t, engine, func(options *Options) {
		options.Workdir = "/work tree"
		options.MaxOutput = 8
	})
	result, err := backend.Execute(context.Background(), "false", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "12345\ner" || result.ExitCode == nil || *result.ExitCode != 7 || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	_, _, _, _, execs, _ := engine.snapshot()
	want := engineclient.ExecCreateOptions{AttachStdout: true, AttachStderr: true, WorkingDir: "/work tree", Cmd: []string{"/bin/sh", "-c", "false"}}
	if len(execs) != 1 || !reflect.DeepEqual(execs[0], want) {
		t.Fatalf("exec options = %#v, want %#v", execs, want)
	}
}

func TestExecuteRejectsInvalidOrStillRunningExec(t *testing.T) {
	t.Run("invalid exec id", func(t *testing.T) {
		backend := newTestBackend(t, &fakeEngine{execID: " bad-id"}, nil)
		if _, err := backend.Execute(context.Background(), "true", time.Second); err == nil || !strings.Contains(err.Error(), "invalid id") {
			t.Fatalf("execute error = %v", err)
		}
	})
	t.Run("output closed while exec is running", func(t *testing.T) {
		backend := newTestBackend(t, &fakeEngine{execRunning: true}, nil)
		if _, err := backend.Execute(context.Background(), "true", time.Second); err == nil || !strings.Contains(err.Error(), "still running") {
			t.Fatalf("execute error = %v", err)
		}
	})
}

func TestTimeoutRestartsContainerAndPreservesWorkspace(t *testing.T) {
	engine := &fakeEngine{holdExec: true}
	backend := newTestBackend(t, engine, nil)
	if _, err := backend.Write(context.Background(), "/keep.txt", "safe"); err != nil {
		t.Fatal(err)
	}
	timeout := 5 * time.Millisecond
	result, err := backend.ExecuteWithOptions(context.Background(), "sleep forever", dabackend.ExecuteOptions{Timeout: &timeout})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 124 || !strings.Contains(result.Output, "container was restarted") {
		t.Fatalf("timeout result = %#v", result)
	}
	_, starts, kills, _, _, _ := engine.snapshot()
	if !reflect.DeepEqual(kills, []string{"container-id"}) || !reflect.DeepEqual(starts, []string{"container-id", "container-id"}) {
		t.Fatalf("timeout lifecycle starts=%#v kills=%#v", starts, kills)
	}
	read, err := backend.Read(context.Background(), "/keep.txt", 0, 1)
	if err != nil || read.Data == nil || read.Data.Content != "safe" {
		t.Fatalf("workspace after restart = %#v, %v", read, err)
	}
}

func TestParentCancellationRestartsContainerAndReturnsCancellation(t *testing.T) {
	engine := &fakeEngine{holdExec: true}
	backend := newTestBackend(t, engine, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := backend.Execute(ctx, "sleep forever", time.Minute)
		done <- err
	}()
	for {
		_, _, _, _, execs, _ := engine.snapshot()
		if len(execs) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	_, starts, kills, _, _, _ := engine.snapshot()
	if !reflect.DeepEqual(kills, []string{"container-id"}) || len(starts) != 2 {
		t.Fatalf("cancellation lifecycle starts=%#v kills=%#v", starts, kills)
	}
}

func TestCloseRemovesOwnedContainerWorkspaceAndOwnedClient(t *testing.T) {
	engine := &fakeEngine{}
	backend, err := newBackend(context.Background(), "example:local", Options{User: "1000:1000"}, engine, engine.Close)
	if err != nil {
		t.Fatal(err)
	}
	workspace := backend.Workspace()
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace still exists: %v", err)
	}
	_, _, _, removes, _, closes := engine.snapshot()
	if !reflect.DeepEqual(removes, []string{"container-id"}) || closes != 1 {
		t.Fatalf("close lifecycle removes=%#v closes=%d", removes, closes)
	}
	if _, err := backend.Execute(context.Background(), "true", time.Second); err == nil {
		t.Fatal("closed sandbox executed a command")
	}
	if _, err := backend.Read(context.Background(), "/anything", 0, 1); err == nil {
		t.Fatal("closed sandbox read its workspace")
	}
}

func TestClosePreservesCallerWorkspaceAndClient(t *testing.T) {
	workspace := t.TempDir()
	engine := &fakeEngine{}
	backend, err := newBackend(context.Background(), "example:local", Options{Workspace: workspace, User: "1000:1000"}, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(context.Background(), "/keep.txt", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(workspace, "keep.txt")); err != nil || string(content) != "keep" {
		t.Fatalf("caller workspace content = %q, %v", content, err)
	}
	_, _, _, _, _, closes := engine.snapshot()
	if closes != 0 {
		t.Fatalf("caller client closed %d times", closes)
	}
}

func TestShutdownHonorsContextWhileAnotherOperationOwnsSandbox(t *testing.T) {
	backend := newTestBackend(t, &fakeEngine{}, nil)
	backend.operation <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := backend.Shutdown(ctx)
	<-backend.operation
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error = %v", err)
	}
}

func TestInvalidOptionsFailBeforeContainerCreation(t *testing.T) {
	tests := []Options{
		{Workdir: "relative"},
		{Workdir: "/"},
		{Workdir: "/tmp"},
		{Workdir: "/work/../escape"},
		{DefaultTimeout: -time.Second},
		{MemoryBytes: -1},
		{Env: map[string]string{"BAD=NAME": "value"}},
	}
	for _, options := range tests {
		options.Workspace = t.TempDir()
		options.User = "1000:1000"
		engine := &fakeEngine{}
		if _, err := newBackend(context.Background(), "image", options, engine, nil); err == nil {
			t.Fatalf("options %#v unexpectedly succeeded", options)
		}
		creates, _, _, _, _, _ := engine.snapshot()
		if len(creates) != 0 {
			t.Fatalf("invalid options invoked Engine: %#v", creates)
		}
	}
}

func TestEmptyImagePanicsBeforeContainerCreation(t *testing.T) {
	engine := &fakeEngine{}
	defer func() {
		if recover() == nil {
			t.Fatal("empty image did not panic")
		}
		creates, _, _, _, _, _ := engine.snapshot()
		if len(creates) != 0 {
			t.Fatalf("empty image invoked Engine: %#v", creates)
		}
	}()
	_, _ = newBackend(context.Background(), "", Options{}, engine, nil)
}

func TestCreateAndStartFailuresCleanUp(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		engine := &fakeEngine{createError: errors.New("bad image")}
		backend, err := newBackend(context.Background(), "example:local", Options{User: "1000:1000"}, engine, nil)
		if err == nil || backend != nil {
			t.Fatalf("backend = %#v, error = %v", backend, err)
		}
		_, _, _, removes, _, _ := engine.snapshot()
		if len(removes) != 0 {
			t.Fatalf("create cleanup removed unknown container: %#v", removes)
		}
	})
	t.Run("start", func(t *testing.T) {
		engine := &fakeEngine{startError: errors.New("cannot start")}
		backend, err := newBackend(context.Background(), "example:local", Options{User: "1000:1000"}, engine, nil)
		if err == nil || backend != nil {
			t.Fatalf("backend = %#v, error = %v", backend, err)
		}
		_, _, _, removes, _, _ := engine.snapshot()
		if !reflect.DeepEqual(removes, []string{"container-id"}) {
			t.Fatalf("start cleanup = %#v", removes)
		}
	})
}
