//go:build !unix

package dabackend

import (
	"os/exec"
	"time"
)

func configureLocalProcess(command *exec.Cmd) {
	command.WaitDelay = time.Second
}
