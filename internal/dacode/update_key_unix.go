//go:build unix

package dacode

import (
	"os"
	"syscall"
)

func trustedUpdateKey(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o022 == 0
}
