//go:build unix

package browse

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBrowserProcessGroupCleanup verifies that closing a BrowseTools instance
// kills not just headless-shell but its entire process group (zygote, GPU,
// renderer, utility processes). Without Setpgid + killpg, descendants
// reparent to PID 1 and live on past Shelley shutdown — the bug this guards
// against.
func TestBrowserProcessGroupCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tools := NewBrowseTools(ctx, 0)

	if _, err := tools.GetBrowserContext(); err != nil {
		if strings.Contains(err.Error(), "failed to start browser") {
			t.Skip("Browser automation not available in this environment")
		}
		t.Fatalf("Failed to get browser context: %v", err)
	}

	tools.mux.Lock()
	cmd := tools.browserCmd
	tools.mux.Unlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("browserCmd not captured; ModifyCmdFunc wiring is broken")
	}
	pid := cmd.Process.Pid

	// Sanity-check Setpgid: pgid should equal pid.
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", pid, err)
	}
	if pgid != pid {
		t.Fatalf("expected pgid==pid==%d, got pgid=%d (Setpgid not applied)", pid, pgid)
	}

	// Find at least one descendant beyond the direct child to make this test
	// meaningful: chromedp's default behavior would still kill the direct
	// child, and we want to prove we're cleaning up the whole tree.
	descendants := findDescendantsByPgid(t, pid)
	t.Logf("headless-shell pid=%d, %d processes in process group", pid, len(descendants))
	if len(descendants) < 2 {
		t.Logf("warning: only %d processes in headless-shell process group; test is weaker than expected", len(descendants))
	}

	tools.Close()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		alive := stillAlive(descendants)
		if len(alive) == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("processes still alive after Close: %v", alive)
		case <-poll.C:
		}
	}
}

// findDescendantsByPgid returns PIDs whose process group is pgid, by
// querying the host process table. Includes the leader itself.
func findDescendantsByPgid(t *testing.T, pgid int) []int {
	t.Helper()
	output, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil {
		t.Fatalf("inspect process table: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		got, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if got == pgid {
			pids = append(pids, pid)
		}
	}
	return pids
}

func stillAlive(pids []int) []int {
	var alive []int
	for _, pid := range pids {
		// Signal 0 probes existence without killing.
		if err := syscall.Kill(pid, 0); err == nil {
			alive = append(alive, pid)
		}
	}
	return alive
}
