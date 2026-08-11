//go:build unix

package dabackend

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalShellCancellationStopsChildProcessesPromptly(t *testing.T) {
	shell, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	_, err = shell.Execute(ctx, "sleep 10 & wait", 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %s; child process kept the command alive", elapsed)
	}
}
