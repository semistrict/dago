//go:build unix

package daupdate

import (
	"os"
	"syscall"
)

func ownedUpdateTarget(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
