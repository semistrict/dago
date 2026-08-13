//go:build tinygo

package dabackend

import "fmt"

func openRoot(string) (rootedFilesystem, error) {
	return nil, fmt.Errorf("virtual filesystem confinement is unavailable in this build")
}
