//go:build !unix

package daeventbus

import (
	"net"
	"os"
)

func listenUnix(string) (net.Listener, os.FileInfo, error) { return nil, nil, ErrUnsupported }

func cleanupUnix(string, os.FileInfo) error { return nil }

// Supported reports whether this build supports Unix-domain socket ingress.
func Supported() bool { return false }
