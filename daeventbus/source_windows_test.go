//go:build windows

package daeventbus

import (
	"context"
	"errors"
	"testing"
)

func TestUnixSourceReportsUnsupportedOnWindows(t *testing.T) {
	if Supported() {
		t.Fatal("Unix source unexpectedly reported support")
	}
	source := NewUnixSource(SinkFunc(func(context.Context, Event) error { return nil }), `C:\events\events.sock`, Options{})
	if err := source.Run(t.Context()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Run error = %v", err)
	}
}
