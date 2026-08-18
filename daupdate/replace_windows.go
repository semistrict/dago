//go:build windows

package daupdate

import "os"

func replaceUpdateFile(source, target string) error { return os.Rename(source, target) }
func syncUpdateDirectory(string) error              { return nil }
