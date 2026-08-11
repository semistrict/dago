//go:build unix

package dabackend

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configureLocalProcess gives each command its own process group so context
// cancellation stops descendants that inherited the command's output pipes.
func configureLocalProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
}
