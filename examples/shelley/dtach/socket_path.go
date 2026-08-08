package dtach

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

const portableUnixSocketPathLimit = 96

// socketPath maps an overlong logical path to a stable, user-private path.
// Unix socket limits vary by platform and are substantially lower than normal
// filesystem path limits. Both Serve and Attach apply the same mapping, so the
// public session identity remains the caller-provided path.
func socketPath(logical string) string {
	if len(logical) <= portableUnixSocketPathLimit {
		return logical
	}
	sum := sha256.Sum256([]byte(logical))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("sdt-%d", os.Getuid()))
	return filepath.Join(dir, fmt.Sprintf("%x.sock", sum[:8]))
}
