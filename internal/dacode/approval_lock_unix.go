//go:build !windows

package dacode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return errors.Join(unlockErr, file.Close())
	}, nil
}
