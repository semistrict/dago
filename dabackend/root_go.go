//go:build !tinygo

package dabackend

import "os"

func openRoot(name string) (rootedFilesystem, error) {
	return os.OpenRoot(name)
}
