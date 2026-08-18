package dacode

import (
	"context"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalDevServerStartUsesSanitizedEnvironmentAndRetriesReadiness(t *testing.T) {
	arguments := []string{"serve", "--address", "127.0.0.1:4312"}
	explicit := map[string]string{"SERVER_MODE": "local"}
	ambient := map[string]string{
		"PATH": "/safe/bin", "SAFE_PARENT": "yes", "OPENAI_API_KEY": "never-inherit-this",
	}
	var launch localDevProcessLaunch
	var executable string
	var gotArguments []string
	process := newFakeLocalDevProcess()
	process.finishOnSignal = true
	probeCalls := 0
	server := newLocalDevServer("/usr/bin/example-server", arguments, localDevServerConfig{
		Endpoint: "http://localhost:4312", Environment: explicit, InheritEnvironment: []string{"SAFE_PARENT"},
		lookupEnvironment: func(key string) (string, bool) {
			value, exists := ambient[key]
			return value, exists
		},
		processFactory: func(path string, argv []string, options localDevProcessLaunch) (localDevProcess, error) {
			executable, gotArguments, launch = path, append([]string(nil), argv...), options
			_, _ = io.WriteString(options.Output, "booting\n")
			return process, nil
		},
		probe: func(_ context.Context, endpoint *url.URL) (bool, error) {
			probeCalls++
			if endpoint.String() != "http://127.0.0.1:4312/ok" {
				t.Fatalf("readiness endpoint = %s", endpoint)
			}
			return probeCalls == 3, errors.New("not ready")
		},
		sleep: func(context.Context, time.Duration) error { return nil },
	})
	arguments[0] = "mutated"
	explicit["SERVER_MODE"] = "mutated"
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if executable != "/usr/bin/example-server" || !reflect.DeepEqual(gotArguments, []string{"serve", "--address", "127.0.0.1:4312"}) {
		t.Fatalf("launch = %q %#v", executable, gotArguments)
	}
	if server.Endpoint() != "http://127.0.0.1:4312" || probeCalls != 3 {
		t.Fatalf("endpoint=%s probes=%d", server.Endpoint(), probeCalls)
	}
	if got := strings.Join(launch.Environment, "\n"); got != "PATH=/safe/bin\nSAFE_PARENT=yes\nSERVER_MODE=local" || strings.Contains(got, "never-inherit-this") {
		t.Fatalf("child environment = %q", got)
	}
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDevServerCrashDiagnosticIsBoundedTerminalSafeAndRedacted(t *testing.T) {
	process := newFakeLocalDevProcess()
	process.waitErr = errors.New("exit status 7: token=raw-cause-secret")
	server := newLocalDevServer("/usr/bin/example-server", nil, localDevServerConfig{
		LogBytes: 1024,
		processFactory: func(_ string, _ []string, launch localDevProcessLaunch) (localDevProcess, error) {
			_, _ = io.WriteString(launch.Output, strings.Repeat("padding ", 200)+"\nAuthorization: Bearer raw-log-secret\napi_key=sk-1234567890\n\x1b[31m")
			process.finish()
			return process, nil
		},
		probe: func(context.Context, *url.URL) (bool, error) { return false, nil },
	})
	err := server.Start(t.Context())
	var crash *localDevServerCrashError
	if !errors.As(err, &crash) {
		t.Fatalf("error = %T %v", err, err)
	}
	shown := err.Error()
	for _, secret := range []string{"raw-cause-secret", "raw-log-secret", "sk-1234567890"} {
		if strings.Contains(shown, secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, shown)
		}
	}
	if strings.ContainsRune(shown, '\x1b') || !strings.Contains(shown, "[REDACTED]") || len(crash.LogTail) > 1024 {
		t.Fatalf("unsafe crash diagnostic = %q", shown)
	}
}

func TestLocalDevServerCancellationReapsProcessBeforeReturning(t *testing.T) {
	process := newFakeLocalDevProcess()
	process.finishOnSignal = true
	probeCalled := make(chan struct{})
	var once sync.Once
	server := newLocalDevServer("/usr/bin/example-server", nil, localDevServerConfig{
		processFactory: func(string, []string, localDevProcessLaunch) (localDevProcess, error) { return process, nil },
		probe: func(context.Context, *url.URL) (bool, error) {
			once.Do(func() { close(probeCalled) })
			return false, nil
		},
		sleep: func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return context.Cause(ctx)
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- server.Start(ctx) }()
	<-probeCalled
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v", err)
	}
	if process.signalCalls.Load() != 1 || process.killCalls.Load() != 0 {
		t.Fatalf("signals=%d kills=%d", process.signalCalls.Load(), process.killCalls.Load())
	}
	select {
	case <-process.Done():
	default:
		t.Fatal("start returned before child was reaped")
	}
}

func TestLocalDevServerCloseIsConcurrentIdempotentAndEscalates(t *testing.T) {
	process := newFakeLocalDevProcess()
	process.finishOnKill = true
	server := newLocalDevServer("/usr/bin/example-server", nil, localDevServerConfig{
		ShutdownTimeout: time.Millisecond, KillTimeout: time.Second,
		processFactory: func(string, []string, localDevProcessLaunch) (localDevProcess, error) { return process, nil },
		probe:          func(context.Context, *url.URL) (bool, error) { return true, nil },
	})
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	const closers = 8
	results := make(chan error, closers)
	for range closers {
		go func() { results <- server.Close(t.Context()) }()
	}
	for range closers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if process.signalCalls.Load() != 1 || process.killCalls.Load() != 1 {
		t.Fatalf("signals=%d kills=%d", process.signalCalls.Load(), process.killCalls.Load())
	}
	if err := server.Start(t.Context()); !errors.Is(err, errLocalDevClosed) {
		t.Fatalf("restart after close = %v", err)
	}
}

func TestLocalDevRestartControllerReplacesProcessAndRemainsReusable(t *testing.T) {
	first := newFakeLocalDevProcess()
	first.finishOnSignal = true
	second := newFakeLocalDevProcess()
	second.finishOnSignal = true
	processes := []*fakeLocalDevProcess{first, second}
	starts := 0
	server := newLocalDevServer("/usr/bin/example-server", []string{"serve"}, localDevServerConfig{
		processFactory: func(string, []string, localDevProcessLaunch) (localDevProcess, error) {
			process := processes[starts]
			starts++
			return process, nil
		},
		probe: func(context.Context, *url.URL) (bool, error) { return true, nil },
	})
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	controller := newLocalDevRestartController(server)
	if err := controller.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if starts != 2 || first.signalCalls.Load() != 1 || second.signalCalls.Load() != 0 {
		t.Fatalf("starts=%d first signals=%d second signals=%d", starts, first.signalCalls.Load(), second.signalCalls.Load())
	}
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.signalCalls.Load() != 1 {
		t.Fatalf("replacement signals = %d", second.signalCalls.Load())
	}
	if err := controller.Restart(t.Context()); !errors.Is(err, errLocalDevClosed) {
		t.Fatalf("restart after close = %v", err)
	}
}

func TestLocalDevClosePreventsConcurrentRestartResurrection(t *testing.T) {
	process := newFakeLocalDevProcess()
	starts := atomic.Int32{}
	server := newLocalDevServer("/usr/bin/example-server", nil, localDevServerConfig{
		ShutdownTimeout: time.Second,
		processFactory: func(string, []string, localDevProcessLaunch) (localDevProcess, error) {
			starts.Add(1)
			return process, nil
		},
		probe: func(context.Context, *url.URL) (bool, error) { return true, nil },
	})
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	process.signalErr = nil
	restartResult := make(chan error, 1)
	go func() {
		restartResult <- server.Restart(t.Context())
	}()
	for process.signalCalls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- server.Close(t.Context()) }()
	for {
		server.stateMu.Lock()
		closed := server.closed
		server.stateMu.Unlock()
		if closed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	process.finish()
	if err := <-restartResult; !errors.Is(err, errLocalDevClosed) {
		t.Fatalf("restart result = %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("process was resurrected: starts=%d", starts.Load())
	}
}

func TestLocalDevLogKeepsOnlyNewestCompleteBound(t *testing.T) {
	log := newLocalDevLog(8)
	if written, err := log.Write([]byte("123456")); written != 6 || err != nil {
		t.Fatalf("first write = %d, %v", written, err)
	}
	_, _ = log.Write([]byte("789abc"))
	if got := log.String(); got != "56789abc" {
		t.Fatalf("tail = %q", got)
	}
	_, _ = log.Write([]byte("zzzzzzzzzz"))
	if got := log.String(); got != "zzzzzzzz" {
		t.Fatalf("oversized tail = %q", got)
	}
}

func TestLocalDevServerRejectsRemoteCredentialedAndUnboundedConfiguration(t *testing.T) {
	tests := []localDevServerConfig{
		{Endpoint: "http://example.com:2024"},
		{Endpoint: "http://127.0.0.1:2024?token=secret"},
		{Endpoint: "http://user:" + "secret@127.0.0.1:2024"},
		{Endpoint: "http://127.0.0.1:2024", HealthPath: "//evil.example/ok"},
		{Environment: map[string]string{"OPENAI_API_KEY": "secret"}},
		{InheritEnvironment: []string{"LD_PRELOAD"}},
		{LogBytes: maxLocalDevLogBytes + 1},
		{StartupTimeout: 3 * time.Minute},
	}
	for _, config := range tests {
		assertLocalDevPanic(t, func() { _ = newLocalDevServer("/usr/bin/server", nil, config) })
	}
	assertLocalDevPanic(t, func() { _ = newLocalDevServer("server-from-path", nil, localDevServerConfig{}) })
	assertLocalDevPanic(t, func() {
		_ = newLocalDevServer("/usr/bin/server", []string{strings.Repeat("x", maxLocalDevArgumentBytes+1)}, localDevServerConfig{})
	})
}

func TestSanitizeLocalDevDiagnosticRedactsCommonCredentialShapes(t *testing.T) {
	input := "password='hunter2' Bearer abc.def token:token-value https://user:" + "pass@127.0.0.1/ ghp_1234567890"
	result := sanitizeLocalDevDiagnostic(input, 4096)
	for _, secret := range []string{"hunter2", "abc.def", "token-value", "user:pass", "ghp_1234567890"} {
		if strings.Contains(result, secret) {
			t.Fatalf("result leaked %q: %s", secret, result)
		}
	}
}

func assertLocalDevPanic(t *testing.T, invoke func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("invalid static input did not panic")
		}
	}()
	invoke()
}

type fakeLocalDevProcess struct {
	done           chan struct{}
	finishOnce     sync.Once
	waitErr        error
	finishOnSignal bool
	finishOnKill   bool
	signalErr      error
	killErr        error
	signalCalls    atomic.Int32
	killCalls      atomic.Int32
}

func newFakeLocalDevProcess() *fakeLocalDevProcess {
	return &fakeLocalDevProcess{done: make(chan struct{})}
}

func (process *fakeLocalDevProcess) Done() <-chan struct{} { return process.done }
func (process *fakeLocalDevProcess) WaitError() error      { return process.waitErr }

func (process *fakeLocalDevProcess) SignalTree() error {
	process.signalCalls.Add(1)
	if process.finishOnSignal {
		process.finish()
	}
	return process.signalErr
}

func (process *fakeLocalDevProcess) KillTree() error {
	process.killCalls.Add(1)
	if process.finishOnKill {
		process.finish()
	}
	return process.killErr
}

func (process *fakeLocalDevProcess) finish() { process.finishOnce.Do(func() { close(process.done) }) }
