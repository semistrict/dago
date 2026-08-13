package modeltest

import (
	"testing"
	"testing/synctest"
)

// TestWithFakeTime runs a concurrent test in a synctest bubble. Timers and
// sleeps advance deterministically without wall-clock delays.
func TestWithFakeTime(t *testing.T, run func(*testing.T)) {
	t.Helper()
	synctest.Test(t, run)
}
