//go:build windows

package dasubagent

import "os"

func openDefinition(root *os.Root, path string) (*os.File, error) {
	return root.Open(path)
}
