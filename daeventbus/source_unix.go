//go:build unix

package daeventbus

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func listenUnix(path string) (net.Listener, os.FileInfo, error) {
	parent := filepath.Dir(path)
	parentInfo, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, nil, errors.Join(ErrTransport, err)
		}
		if err := os.Chmod(parent, 0o700); err != nil {
			return nil, nil, errors.Join(ErrTransport, err)
		}
		parentInfo, err = os.Lstat(parent)
	}
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o077 != 0 {
		return nil, nil, ErrUnsafePath
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, errors.Join(ErrTransport, err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		listener.Close()
		return nil, nil, ErrTransport
	}
	// Go otherwise unlinks the pathname from Close without checking whether
	// another local process replaced it. Cleanup below verifies file identity.
	unixListener.SetUnlinkOnClose(false)
	identity, err := os.Lstat(path)
	if err != nil || identity.Mode()&os.ModeSocket == 0 {
		listener.Close()
		return nil, nil, ErrUnsafePath
	}
	currentParent, parentErr := os.Lstat(parent)
	if parentErr != nil || !currentParent.IsDir() || currentParent.Mode()&os.ModeSymlink != 0 ||
		currentParent.Mode().Perm()&0o077 != 0 || !os.SameFile(parentInfo, currentParent) {
		listener.Close()
		_ = cleanupUnix(path, identity)
		return nil, nil, ErrUnsafePath
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		_ = cleanupUnix(path, identity)
		return nil, nil, errors.Join(ErrTransport, err)
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSocket == 0 || current.Mode().Perm() != 0o600 || !os.SameFile(identity, current) {
		listener.Close()
		_ = cleanupUnix(path, identity)
		return nil, nil, ErrUnsafePath
	}
	return listener, current, nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrTransport, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return ErrPathOccupied
	}
	connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		connection.Close()
		return ErrPathOccupied
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return ErrPathOccupied
	}
	if err := os.Remove(path); err != nil {
		return errors.Join(ErrTransport, fmt.Errorf("remove stale socket: %w", err))
	}
	return nil
}

func cleanupUnix(path string, identity os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrTransport, err)
	}
	if current.Mode()&os.ModeSocket == 0 || identity == nil || !os.SameFile(identity, current) {
		return ErrPathOccupied
	}
	if err := os.Remove(path); err != nil {
		return errors.Join(ErrTransport, err)
	}
	return nil
}

// Supported reports whether this build supports Unix-domain socket ingress.
func Supported() bool { return true }
