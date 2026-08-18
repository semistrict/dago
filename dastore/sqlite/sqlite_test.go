package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/semistrict/dago/dastore"
)

func TestNewRejectsNilDatabase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil database was accepted")
		}
	}()
	New(nil)
}

func TestSearchAppliesFiniteDefaultAndRejectsNegativeValues(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "bounded.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	operations := make([]dastore.Operation, dastore.DefaultSearchLimit+1)
	for index := range operations {
		operations[index] = dastore.Operation{
			Namespace: dastore.Namespace{"bounded"},
			Key:       fmt.Sprintf("item-%03d", index),
			PutValue:  map[string]any{"index": index},
		}
	}
	if _, err := storage.Batch(context.Background(), operations); err != nil {
		t.Fatal(err)
	}
	items, err := storage.Search(context.Background(), dastore.SearchOptions{Prefix: dastore.Namespace{"bounded"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != dastore.DefaultSearchLimit {
		t.Fatalf("default search length = %d, want %d", len(items), dastore.DefaultSearchLimit)
	}
	for _, options := range []dastore.SearchOptions{{Limit: -1}, {Offset: -1}} {
		if _, err := storage.Search(context.Background(), options); err == nil {
			t.Fatalf("Search(%+v) accepted a negative value", options)
		}
	}
}

func TestListNamespacesAppliesFiniteDefaultAndFiltersLargeCardinality(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "namespaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	operations := make([]dastore.Operation, dastore.DefaultListNamespacesLimit+25)
	for index := range operations {
		operations[index] = dastore.Operation{
			Namespace: dastore.Namespace{"tenant", fmt.Sprintf("%03d", index), "tail"},
			Key:       "item",
			PutValue:  map[string]any{"index": index},
		}
	}
	if _, err := storage.Batch(context.Background(), operations); err != nil {
		t.Fatal(err)
	}
	namespaces, err := storage.ListNamespaces(context.Background(), dastore.ListNamespacesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(namespaces) != dastore.DefaultListNamespacesLimit {
		t.Fatalf("default namespace length = %d, want %d", len(namespaces), dastore.DefaultListNamespacesLimit)
	}
	namespaces, err = storage.ListNamespaces(context.Background(), dastore.ListNamespacesOptions{
		Prefix: dastore.Namespace{"tenant", "*"}, Suffix: dastore.Namespace{"tail"},
		MaxDepth: 2, Limit: 3, Offset: 98,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []dastore.Namespace{{"tenant", "098"}, {"tenant", "099"}, {"tenant", "100"}}
	if !reflect.DeepEqual(namespaces, want) {
		t.Fatalf("filtered namespaces = %#v, want %#v", namespaces, want)
	}
	for _, options := range []dastore.ListNamespacesOptions{{Limit: -1}, {Offset: -1}, {MaxDepth: -1}} {
		if _, err := storage.ListNamespaces(context.Background(), options); err == nil {
			t.Fatalf("ListNamespaces(%+v) accepted a negative value", options)
		}
	}
}

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
	namespaces, err := second.ListNamespaces(context.Background(), dastore.ListNamespacesOptions{Prefix: dastore.Namespace{"users"}})
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
