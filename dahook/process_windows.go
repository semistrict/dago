//go:build windows

package dahook

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type hookProcess struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func configureProcess(command *exec.Cmd) (*hookProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	process := &hookProcess{job: job}
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	command.Cancel = process.terminate
	command.WaitDelay = processWaitDelay
	return process, nil
}

func (process *hookProcess) started(command *exec.Cmd) error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.closed {
		return windows.ERROR_INVALID_HANDLE
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(process.job, handle); err != nil {
		return err
	}
	return resumeProcessThreads(uint32(command.Process.Pid))
}

func resumeProcessThreads(processID uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	resumed := false
	for {
		if entry.OwnerProcessID == processID {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return openErr
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			resumed = true
		}
		err = windows.Thread32Next(snapshot, &entry)
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			return err
		}
	}
	if !resumed {
		return windows.ERROR_NOT_FOUND
	}
	return nil
}

func (process *hookProcess) terminate() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.closed {
		return nil
	}
	process.closed = true
	return windows.CloseHandle(process.job)
}

func (process *hookProcess) close() { _ = process.terminate() }
