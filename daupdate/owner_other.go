//go:build !unix && !windows

package daupdate

import "os"

func ownedUpdateTarget(os.FileInfo) bool { return false }
