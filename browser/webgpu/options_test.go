package webgpu

import (
	"testing"

	"github.com/semistrict/dago/damodel"
)

func TestZeroOptionsSelectStableBridgeDefaultsAndCloneProfile(t *testing.T) {
	levels := []string{"low", "high"}
	compiled := compileOptions(Options{Profile: damodel.Profile{ReasoningLevels: levels}})
	levels[0] = "mutated"
	if compiled.InvokeGlobal != DefaultInvokeGlobal || compiled.InterruptGlobal != DefaultInterruptGlobal {
		t.Fatalf("bridge defaults = %q, %q", compiled.InvokeGlobal, compiled.InterruptGlobal)
	}
	if compiled.Profile.ReasoningLevels[0] != "low" {
		t.Fatalf("compiled profile = %#v", compiled.Profile)
	}
}

func TestOptionsRejectPaddedBridgeNames(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("padded bridge name was accepted")
		}
	}()
	compileOptions(Options{InvokeGlobal: " padded "})
}
