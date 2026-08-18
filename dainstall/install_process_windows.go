//go:build windows

package dainstall

import (
	"os"
	"os/exec"
	"time"
)

func configureInstallProcess(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return command.Process.Kill()
	}
	command.WaitDelay = time.Second
}
