package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/semistrict/dago/dastore"
)

func TestDurableStoreOperationsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	namespace := dastore.Namespace{"users", "one"}
	if err := first.Put(context.Background(), namespace, "memory", map[string]any{"text": "likes Go"}); err != nil {
		t.Fatal(err)
	}
	before, err := first.Get(context.Background(), namespace, "memory")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if err := first.Put(context.Background(), namespace, "memory", map[string]any{"text": "likes delta channels"}); err != nil {
		t.Fatal(err)
	}
	after, _ := first.Get(context.Background(), namespace, "memory")
	if !before.CreatedAt.Equal(after.CreatedAt) || !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("timestamps before=%#v after=%#v", before, after)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	items, err := second.Search(context.Background(), dastore.SearchOptions{Prefix: dastore.Namespace{"users"}, Query: "delta"})
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %#v, %v", items, err)
	}
	namespaces, err := second.ListNamespaces(context.Background(), dastore.Namespace{"users"})
	if err != nil || !reflect.DeepEqual(namespaces, []dastore.Namespace{namespace}) {
		t.Fatalf("namespaces = %#v, %v", namespaces, err)
	}
}

func TestBatchIsAtomicAndOrdered(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	namespace := dastore.Namespace{"batch"}
	results, err := storage.Batch(context.Background(), []dastore.Operation{
		{Namespace: namespace, Key: "a", PutValue: map[string]any{"value": float64(1)}},
		{Namespace: namespace, Key: "a"},
		{Namespace: namespace, Key: "a", Delete: true},
		{Namespace: namespace, Key: "a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 || results[0].Item == nil || results[1].Item == nil || results[2].Item != nil || results[3].Item != nil {
		t.Fatalf("results = %#v", results)
	}
}
