//go:build windows

package dacode

import "os"

// Windows trust-root writes are controlled by the file's ACL.
func trustedUpdateKey(os.FileInfo) bool { return true }
