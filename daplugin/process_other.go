//go:build (!unix || js || wasip1) && !windows

package daplugin

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

type gitProcess struct{}

func configureGitProcess(command *exec.Cmd) (*gitProcess, error) {
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := command.Process.Kill()
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	return &gitProcess{}, nil
}

func (*gitProcess) started(*exec.Cmd) error { return nil }
func (*gitProcess) close()                  {}
