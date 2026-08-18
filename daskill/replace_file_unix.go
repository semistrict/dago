//go:build !windows

package daskill

import "os"

func replaceTrustFile(source, target string) error {
	return os.Rename(source, target)
}

func syncTrustDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
