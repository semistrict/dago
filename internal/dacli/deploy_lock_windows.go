//go:build windows

package dacli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockDeployState(ctx context.Context, statePath string) (func() error, error) {
	file, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deploy state lock: %w", err)
	}
	overlapped := &windows.Overlapped{}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("lock deploy state: %w", err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			timer.Stop()
			_ = file.Close()
			return nil, errors.New("timed out locking deploy state")
		case <-timer.C:
		}
	}
	return func() error {
		unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
		return errors.Join(unlockErr, file.Close())
	}, nil
}
