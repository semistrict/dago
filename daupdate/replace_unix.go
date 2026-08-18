//go:build !windows

package daupdate

import (
	"errors"
	"os"
)

func replaceUpdateFile(source, target string) error { return os.Rename(source, target) }

func syncUpdateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
