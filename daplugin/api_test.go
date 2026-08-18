package daplugin

import (
	"context"
	"testing"
)

type pointerMaterializer struct{}

func (*pointerMaterializer) Marketplace(context.Context, MarketplaceSource, string) (string, error) {
	return "", nil
}

func (*pointerMaterializer) Plugin(context.Context, Marketplace, MarketplaceEntry, string) (MaterializedPlugin, error) {
	return MaterializedPlugin{}, nil
}

func (*pointerMaterializer) Cleanup(context.Context, string) error { return nil }

func TestNewManagerTreatsTypedNilOptionalMaterializerAsAbsent(t *testing.T) {
	var materializer *pointerMaterializer
	manager := NewManager(NewStore(t.TempDir(), StoreOptions{}), materializer)
	if manager.materializer != nil {
		t.Fatal("typed nil optional materializer was retained")
	}
}
