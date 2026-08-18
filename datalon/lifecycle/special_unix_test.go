//go:build unix

package lifecycle

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSpecialFileFailsClosed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "assistant")
	media := filepath.Join(state, "media", "inbound")
	if err := os.MkdirAll(media, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(media, "old")
	writeAt(t, old, "old", time.Now().Add(-48*time.Hour), 0o600)
	if err := syscall.Mkfifo(filepath.Join(media, "pipe"), 0o600); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}
	if _, err := New(state, &fakeCronStore{}, Options{}).Clean(t.Context()); !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("special-file error = %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("regular file deleted before special-file failure: %v", err)
	}
}
