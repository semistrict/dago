package daskill

import (
	"path/filepath"
	"testing"

	"github.com/semistrict/dago/dacheckpoint"
)

func TestConstructorsRejectNegativeLimitsAndTypedNilSaver(t *testing.T) {
	trust := NewTrustStore(filepath.Join(t.TempDir(), "trust.json"))
	var saver *dacheckpoint.MemorySaver
	for name, call := range map[string]func(){
		"manager":   func() { NewManager(nil, trust, ManagerOptions{MaximumSkills: -1}) },
		"inspector": func() { NewThreadInspector(dacheckpoint.NewMemorySaver(), ThreadInspectorOptions{MaximumMessages: -1}) },
		"typed nil": func() { NewThreadInspector(saver, ThreadInspectorOptions{}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid static constructor input did not panic")
				}
			}()
			call()
		})
	}
}
