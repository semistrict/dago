//go:build !windows

package daupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func lockUpdate(ctx context.Context, path string, wait time.Duration) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ErrApplyFailed
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ErrApplyFailed
	}
	lockCtx, cancel := context.WithTimeout(ctx, wait)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return errors.Join(unlockErr, file.Close())
	}, nil
}
