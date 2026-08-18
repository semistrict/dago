//go:build unix && !js && !wasip1

package daplugin

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type gitProcess struct{}

func configureGitProcess(command *exec.Cmd) (*gitProcess, error) {
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
	return &gitProcess{}, nil
}
func (*gitProcess) started(*exec.Cmd) error { return nil }
func (*gitProcess) close()                  {}
