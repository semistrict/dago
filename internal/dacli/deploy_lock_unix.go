//go:build !windows

package dacli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func lockDeployState(ctx context.Context, statePath string) (func() error, error) {
	file, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deploy state lock: %w", err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return errors.Join(unlockErr, file.Close())
	}, nil
}
