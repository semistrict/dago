//go:build windows

package daenv

import "os"

// A path beneath an ordinary Windows directory cannot name a filesystem FIFO.
// The descriptor is still validated as regular and identity-stable by applyFile.
func openDotenv(filePath string) (*os.File, error) {
	return os.Open(filePath)
}
