//go:build windows

package dacode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func lockApprovalPreferences(path string) (func() error, error) {
	if path == "" {
		return nil, errors.New("approval preferences path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create approval preferences lock directory: %w", err)
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open approval preferences lock: %w", err)
	}
	overlapped := &windows.Overlapped{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			file.Close()
			return nil, fmt.Errorf("lock approval preferences: %w", err)
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, errors.New("timed out locking approval preferences")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() error {
		unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		return errors.Join(unlockErr, file.Close())
	}, nil
}
