//go:build windows

package dacode

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureLocalDevCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func signalLocalDevProcessTree(command *exec.Cmd, force bool) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	if !force {
		err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(command.Process.Pid))
		if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return os.ErrProcessDone
		}
		return err
	}
	// taskkill is invoked directly, never through a shell, and /T applies the
	// forced termination to the complete descendant tree.
	err := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F").Run()
	if errors.Is(err, windows.ERROR_NOT_FOUND) {
		return os.ErrProcessDone
	}
	return err
}
