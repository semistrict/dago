//go:build !windows

package mcp

import "os"

func replaceFile(source, destination string) error { return os.Rename(source, destination) }
