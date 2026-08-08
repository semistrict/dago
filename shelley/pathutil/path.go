// Package pathutil preserves user-facing logical paths when subprocesses
// return physical paths after resolving filesystem aliases.
package pathutil

import (
	"path/filepath"
	"strings"
)

// Logical rewrites path through the nearest ancestor of reference when both
// identify the same physical filesystem tree. This keeps paths such as /var
// stable on macOS even when git reports their /private/var equivalents.
func Logical(path, reference string) string {
	path = filepath.Clean(path)
	reference = filepath.Clean(reference)
	for ancestor := reference; ; ancestor = filepath.Dir(ancestor) {
		physical, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			rel, relErr := filepath.Rel(physical, path)
			if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return filepath.Clean(filepath.Join(ancestor, rel))
			}
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
	}
	return path
}
