//go:build windows

package daupdate

import "os"

// Windows replacement authorization is enforced by the file ACL when the
// verified same-directory stage is renamed over the target.
func ownedUpdateTarget(os.FileInfo) bool { return true }
