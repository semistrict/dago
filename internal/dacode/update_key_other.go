//go:build !unix && !windows

package dacode

import "os"

func trustedUpdateKey(os.FileInfo) bool { return false }
