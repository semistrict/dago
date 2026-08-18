package dahousekeeping

import (
	"io/fs"
	"runtime"
)

func ownerPrivateDirectory(info fs.FileInfo) bool {
	if info == nil || !info.IsDir() {
		return false
	}
	// Go's Windows FileMode does not expose ACL entries. The explicit path's ACL
	// remains the caller/platform responsibility, as with other owner-private
	// state in this repository.
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o077 == 0
}
