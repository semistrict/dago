//go:build windows

package daupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

func lockUpdate(ctx context.Context, path string, wait time.Duration) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ErrApplyFailed
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ErrApplyFailed
	}
	overlapped := &windows.Overlapped{}
	lockCtx, cancel := context.WithTimeout(ctx, wait)
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			cancel()
			file.Close()
			return nil, ErrApplyFailed
		}
		select {
		case <-lockCtx.Done():
			cancel()
			file.Close()
			return nil, lockCtx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	return func() error {
		unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		return errors.Join(unlockErr, file.Close())
	}, nil
}
