//go:build tinygo

package dago

import (
	"strings"
	"testing"
)

func TestTinyGoInterpreterIsUnavailable(t *testing.T) {
	_, err := newInterpreter(Interpreter{Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "unavailable in TinyGo builds") {
		t.Fatalf("newInterpreter error = %v", err)
	}
}
