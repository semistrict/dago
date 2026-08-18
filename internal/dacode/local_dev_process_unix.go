//go:build unix

package dacode

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureLocalDevCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalLocalDevProcessTree(command *exec.Cmd, force bool) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(-command.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
