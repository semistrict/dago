//go:build windows

package dacli

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

func replaceStateFile(source, target string) error {
	info, err := os.Lstat(target)
	if err == nil {
		if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("deploy state target is invalid")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
