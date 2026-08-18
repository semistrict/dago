//go:build !windows

package dacredential

import "os"

func replaceCredentialFile(source, target string) error { return os.Rename(source, target) }
