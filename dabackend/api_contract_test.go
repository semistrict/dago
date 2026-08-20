package dabackend_test

import (
	"context"
	"testing"
	"time"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dastore"
)

type staticSandboxTransport struct{}

func (staticSandboxTransport) ID() string { return "static" }
func (staticSandboxTransport) Execute(context.Context, string, time.Duration) (dabackend.ExecuteResult, error) {
	return dabackend.ExecuteResult{}, nil
}
func (staticSandboxTransport) Upload(context.Context, []dabackend.Upload) []dabackend.UploadResult {
	return nil
}

func TestStaticBackendConstructorsReturnReadyValues(t *testing.T) {
	memory := dabackend.NewMemory(nil)
	state := dabackend.NewState("", nil)
	base := dabackend.NewBaseSandbox(staticSandboxTransport{}, dabackend.BaseSandboxOptions{})
	composite := dabackend.NewComposite(memory, nil)
	store := dabackend.NewStore(dabackend.FixedNamespace(dastore.Namespace{"files"}), dabackend.StoreOptions{Store: dastore.NewMemory()})
	dynamic := dabackend.NewStore(func(*dabackend.Runtime) (dastore.Namespace, error) {
		return dastore.Namespace{"files"}, nil
	}, dabackend.StoreOptions{Store: dastore.NewMemory()})

	for name, value := range map[string]dabackend.Backend{
		"memory": memory, "state": state, "base": base, "composite": composite,
		"store": store, "dynamic store": dynamic,
	} {
		if value == nil {
			t.Fatalf("%s constructor returned nil", name)
		}
	}
}

func TestExternalMemorySnapshotReturnsValidationError(t *testing.T) {
	if _, err := dabackend.LoadMemory(map[string]dabackend.FileData{"../escape": {}}); err == nil {
		t.Fatal("invalid external snapshot succeeded")
	}
}

func TestExternalStateSnapshotReturnsValidationError(t *testing.T) {
	if _, err := dabackend.LoadState("", map[string]any{"../escape": map[string]any{"content": "x", "encoding": "utf-8"}}); err == nil {
		t.Fatal("invalid external state snapshot succeeded")
	}
}

func TestStaticMemorySnapshotPanicsOnInvalidPath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("invalid static snapshot did not panic")
		}
	}()
	dabackend.NewMemory(map[string]dabackend.FileData{"../escape": {}})
}
