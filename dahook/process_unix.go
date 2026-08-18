//go:build !windows

package dahook

import (
	"os/exec"
	"syscall"
)

type hookProcess struct{}

func configureProcess(command *exec.Cmd) (*hookProcess, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	command.WaitDelay = processWaitDelay
	return &hookProcess{}, nil
}

func (*hookProcess) started(*exec.Cmd) error { return nil }
func (*hookProcess) close()                  {}
