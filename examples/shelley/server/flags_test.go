package server

import (
	"testing"

	"github.com/semistrict/dago/examples/shelley/featureflags"
)

func TestFlagToolPillsRegistered(t *testing.T) {
	f, ok := featureflags.Lookup("tool-pills")
	if !ok {
		t.Fatal("tool-pills not registered")
	}
	if f.Default != false {
		t.Fatalf("default = %v, want false", f.Default)
	}
}
