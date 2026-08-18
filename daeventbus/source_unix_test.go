//go:build unix

package daeventbus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnixSourceForwardsLinesAndReturnsDeterministicReplies(t *testing.T) {
	var lock sync.Mutex
	var received []Event
	source, cancel, runErr := startTestSource(t, SinkFunc(func(_ context.Context, event Event) error {
		lock.Lock()
		received = append(received, event)
		lock.Unlock()
		return nil
	}), Options{})
	connection := dialTestSource(t, source)
	reader := bufio.NewReader(connection)
	lines := []string{
		`{"kind":"command","payload":"/clear"}`,
		`{"kind":"prompt","payload":"next task","correlation_id":"req-7"}`,
		`{"kind":"signal","payload":"interrupt","bypass":"always"}`,
	}
	for _, line := range lines {
		if _, err := connection.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
		var reply Reply
		if err := json.Unmarshal(readLine(t, reader), &reply); err != nil || !reply.OK {
			t.Fatalf("reply = %#v, %v", reply, err)
		}
	}
	connection.Close()
	cancel()
	if err := <-runErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	lock.Lock()
	defer lock.Unlock()
	if len(received) != 3 || received[1].CorrelationID != "req-7" || received[2].Bypass != BypassAlways {
		t.Fatalf("events = %#v", received)
	}
	if received[0].Source != "unix:"+source.Path() {
		t.Fatalf("source = %q", received[0].Source)
	}
	if _, err := os.Lstat(source.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after cancellation: %v", err)
	}
}

func TestUnixSourceNacksMalformedSinkFailuresAndLimits(t *testing.T) {
	secret := "sink-secret"
	source, cancel, runErr := startTestSource(t, SinkFunc(func(_ context.Context, event Event) error {
		switch event.Payload {
		case "error":
			return errors.New(secret)
		case "panic":
			panic(secret)
		default:
			return nil
		}
	}), Options{MaxLineBytes: 128, MaxPayloadBytes: 64, MaxEventsPerConnection: 5})
	defer func() {
		cancel()
		<-runErr
	}()
	connection := dialTestSource(t, source)
	reader := bufio.NewReader(connection)
	for _, test := range []struct {
		line, wantError, correlation string
	}{
		{`{"kind":"unknown","payload":"x","correlation_id":"bad-1"}`, "invalid event", "bad-1"},
		{`{"kind":"prompt","payload":"error"}`, "sink failed", ""},
		{`{"kind":"prompt","payload":"panic"}`, "sink failed", ""},
		{`{"kind":"prompt","payload":"ok"}`, "", ""},
	} {
		connection.Write([]byte(test.line + "\n"))
		var reply Reply
		if err := json.Unmarshal(readLine(t, reader), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Error != test.wantError || reply.CorrelationID != test.correlation || strings.Contains(reply.Error, secret) {
			t.Fatalf("reply = %#v", reply)
		}
	}
	connection.Write([]byte(`{"kind":"prompt","payload":"fifth"}` + "\n"))
	var fifth Reply
	if err := json.Unmarshal(readLine(t, reader), &fifth); err != nil || !fifth.OK {
		t.Fatalf("fifth reply = %#v, %v", fifth, err)
	}
	connection.Write([]byte(`{"kind":"prompt","payload":"sixth"}` + "\n"))
	var limited Reply
	if err := json.Unmarshal(readLine(t, reader), &limited); err != nil || limited.Error != "event count exceeds limit" {
		t.Fatalf("limited reply = %#v, %v", limited, err)
	}
	connection.Close()

	oversized := dialTestSource(t, source)
	oversized.Write([]byte(strings.Repeat("x", 129) + "\n"))
	var reply Reply
	if err := json.Unmarshal(readLine(t, bufio.NewReader(oversized)), &reply); err != nil || reply.Error != "line exceeds read limit" {
		t.Fatalf("oversized reply = %#v, %v", reply, err)
	}
	oversized.Close()
}

func TestUnixSourcePermissionsStaleRecoveryAndOccupiedPaths(t *testing.T) {
	private := filepath.Join(shortTempDir(t), "events")
	path := filepath.Join(private, "events.sock")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.Close()
	source := NewUnixSource(SinkFunc(func(context.Context, Event) error { return nil }), path, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- source.Run(ctx) }()
	waitForSocket(t, path)
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket info = %#v, %v", info, err)
	}
	if err := source.Run(t.Context()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent Run error = %v", err)
	}
	second := NewUnixSource(SinkFunc(func(context.Context, Event) error { return nil }), path, Options{})
	if err := second.Run(t.Context()); !errors.Is(err, ErrPathOccupied) {
		t.Fatalf("active path error = %v", err)
	}
	cancel()
	<-runErr

	if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.Run(t.Context()); !errors.Is(err, ErrPathOccupied) {
		t.Fatalf("regular path error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("regular entry changed: %q, %v", contents, err)
	}
}

func TestUnixSourceRejectsNonPrivateParent(t *testing.T) {
	parent := filepath.Join(shortTempDir(t), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	source := NewUnixSource(SinkFunc(func(context.Context, Event) error { return nil }), filepath.Join(parent, "events.sock"), Options{})
	if err := source.Run(t.Context()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestUnixSourceCloseCancelsSinkAndIsReusable(t *testing.T) {
	started := make(chan struct{}, 1)
	source, _, runErr := startTestSource(t, SinkFunc(func(ctx context.Context, _ Event) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}), Options{SinkTimeout: time.Second})
	connection := dialTestSource(t, source)
	connection.Write([]byte(`{"kind":"prompt","payload":"wait"}` + "\n"))
	<-started
	source.Close()
	source.Close()
	if err := <-runErr; err != nil {
		t.Fatalf("Close Run error = %v", err)
	}
	connection.Close()

	ctx, cancel := context.WithCancel(t.Context())
	again := make(chan error, 1)
	go func() { again <- source.Run(ctx) }()
	waitForSocket(t, source.Path())
	cancel()
	if err := <-again; !errors.Is(err, context.Canceled) {
		t.Fatalf("reused Run error = %v", err)
	}
}

func TestUnixSourceBoundsConcurrentConnections(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	source, cancel, runErr := startTestSource(t, SinkFunc(func(context.Context, Event) error {
		entered <- struct{}{}
		<-release
		return nil
	}), Options{MaxConcurrentConnections: 1})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		<-runErr
	}()

	first := dialTestSource(t, source)
	first.Write([]byte(`{"kind":"prompt","payload":"hold"}` + "\n"))
	<-entered
	second := dialTestSource(t, source)
	var busy Reply
	if err := json.Unmarshal(readLine(t, bufio.NewReader(second)), &busy); err != nil || busy.Error != "too many connections" {
		t.Fatalf("busy reply = %#v, %v", busy, err)
	}
	second.Close()
	releaseOnce.Do(func() { close(release) })
	var accepted Reply
	if err := json.Unmarshal(readLine(t, bufio.NewReader(first)), &accepted); err != nil || !accepted.OK {
		t.Fatalf("accepted reply = %#v, %v", accepted, err)
	}
	first.Close()

}

func TestUnixSourceClosesIdleClients(t *testing.T) {
	source, cancel, runErr := startTestSource(t, SinkFunc(func(context.Context, Event) error { return nil }), Options{ClientIdleTimeout: 50 * time.Millisecond})
	defer func() {
		cancel()
		<-runErr
	}()
	idle := dialTestSource(t, source)
	one := make([]byte, 1)
	if _, err := idle.Read(one); err == nil {
		t.Fatal("idle client remained open")
	}
	idle.Close()
}

func TestUnixSourceDoesNotRemoveReplacementEntry(t *testing.T) {
	source, _, runErr := startTestSource(t, SinkFunc(func(context.Context, Event) error { return nil }), Options{})
	if err := os.Remove(source.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.Path(), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	source.Close()
	if err := <-runErr; !errors.Is(err, ErrPathOccupied) {
		t.Fatalf("Run error = %v", err)
	}
	contents, err := os.ReadFile(source.Path())
	if err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement changed: %q, %v", contents, err)
	}
}

func startTestSource(t *testing.T, sink Sink, options Options) (*Source, context.CancelFunc, <-chan error) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "events", "events.sock")
	source := NewUnixSource(sink, path, options)
	ctx, cancel := context.WithCancel(t.Context())
	runErr := make(chan error, 1)
	go func() { runErr <- source.Run(ctx) }()
	waitForSocket(t, path)
	return source, cancel, runErr
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("socket %q did not appear", path)
}

func dialTestSource(t *testing.T, source *Source) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("unix", source.Path(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func readLine(t *testing.T, reader *bufio.Reader) []byte {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "dago-event-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestReplyJSONShape(t *testing.T) {
	encoded, err := json.Marshal(Reply{OK: true, CorrelationID: "r"})
	if err != nil || string(encoded) != `{"ok":true,"correlation_id":"r"}` {
		t.Fatalf("reply = %s, %v", encoded, err)
	}
}
