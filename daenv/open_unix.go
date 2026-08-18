//go:build !windows

package daenv

import (
	"os"

	"golang.org/x/sys/unix"
)

func openDotenv(filePath string) (*os.File, error) {
	fd, err := unix.Open(filePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filePath), nil
}
