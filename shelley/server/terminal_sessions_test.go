package server

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// procState returns the single-character process state from /proc/<pid>/stat
// (e.g. "R", "S", "Z"), or "" if the process no longer exists (fully reaped).
func procState(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	// Format: "pid (comm) state ...". comm may contain spaces/parens, so the
	// state field is the first token after the final ')'.
	s := string(data)
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ')' {
			if i+2 < len(s) {
				return string(s[i+2])
			}
			break
		}
	}
	return ""
}

// TestSpawnSubprocessReapsChild verifies that a spawned dtach child that exits
// is reaped rather than left as a zombie. Regression test for the
// Release()-without-Wait() bug that produced "[shelley] <defunct>" processes.
//
// The child here is /bin/true, which exits immediately (ignoring the dtach
// args). With the bug, the child becomes a zombie ("Z") and stays that way for
// the lifetime of the test process. With the fix, the background Wait() reaps
// it and its /proc entry disappears.
func TestSpawnSubprocessReapsChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reaping is observed via /proc/<pid>/stat, which is Linux-only")
	}
	dir := t.TempDir()
	ts, err := NewTerminalSessions(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewTerminalSessions: %v", err)
	}
	ts.exe = "/bin/true"

	socket := filepath.Join(dir, "sock")
	logFile := filepath.Join(dir, "log")
	pid, err := ts.spawnSubprocess(socket, logFile, dir, "echo hi", 80, 24, nil)
	if err != nil {
		t.Fatalf("spawnSubprocess: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("expected positive pid, got %d", pid)
	}

	// Poll until the child is fully reaped (no /proc entry). A child stuck in
	// the zombie state would remain readable forever, so a lingering "Z"
	// (or any other persistent state) fails the test.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if procState(pid) == "" {
			return // reaped — success
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child pid %d was not reaped; state=%q (expected gone)", pid, procState(pid))
}
