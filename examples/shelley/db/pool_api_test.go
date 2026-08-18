package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNewPoolZeroReaderCountSelectsUsefulDefault(t *testing.T) {
	pool, err := newPool(filepath.Join(t.TempDir(), "pool.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := pool.Rx(t.Context(), func(context.Context, *Rx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNewPoolPanicsForNegativeReaderCount(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewPool did not panic")
		}
	}()
	_, _ = newPool(filepath.Join(t.TempDir(), "pool.db"), -1)
}
