//go:build windows

package dacode

import (
	"context"
	"os"
	"os/exec"
	"time"
)

func startupShellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", command)
}

func configureStartupProcess(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return command.Process.Kill()
	}
	command.WaitDelay = time.Second
}
