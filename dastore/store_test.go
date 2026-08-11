package dastore

import (
	"context"
	"testing"
)

func TestMemoryCRUDSearchAndIsolation(t *testing.T) {
	memory := NewMemory()
	ctx := context.Background()
	value := map[string]any{"text": "Alpha"}
	if err := memory.Put(ctx, Namespace{"users", "one"}, "memory", value); err != nil {
		t.Fatal(err)
	}
	value["text"] = "mutated"
	item, err := memory.Get(ctx, Namespace{"users", "one"}, "memory")
	if err != nil || item.Value["text"] != "Alpha" {
		t.Fatalf("get = %#v, %v", item, err)
	}
	items, err := memory.Search(ctx, SearchOptions{Prefix: Namespace{"users"}, Query: "alpha"})
	if err != nil || len(items) != 1 {
		t.Fatalf("search = %#v, %v", items, err)
	}
	if err := memory.Delete(ctx, Namespace{"users", "one"}, "memory"); err != nil {
		t.Fatal(err)
	}
	item, err = memory.Get(ctx, Namespace{"users", "one"}, "memory")
	if err != nil || item != nil {
		t.Fatalf("deleted get = %#v, %v", item, err)
	}
}

func TestMemoryBatchIsValidatedBeforeMutation(t *testing.T) {
	memory := NewMemory()
	ctx := context.Background()
	_, err := memory.Batch(ctx, []Operation{
		{Namespace: Namespace{"a"}, Key: "one", PutValue: map[string]any{"n": 1}},
		{Namespace: Namespace{"a"}, Key: "two", PutValue: map[string]any{}, Delete: true},
	})
	if err == nil {
		t.Fatal("expected invalid batch error")
	}
	item, getErr := memory.Get(ctx, Namespace{"a"}, "one")
	if getErr != nil || item != nil {
		t.Fatalf("invalid batch partially applied: %#v, %v", item, getErr)
	}
}

func TestMemoryBatchAndNamespaceOrdering(t *testing.T) {
	memory := NewMemory()
	ctx := context.Background()
	results, err := memory.Batch(ctx, []Operation{
		{Namespace: Namespace{"b"}, Key: "two", PutValue: map[string]any{"n": 2}},
		{Namespace: Namespace{"a"}, Key: "one", PutValue: map[string]any{"n": 1}},
		{Namespace: Namespace{"a"}, Key: "one"},
	})
	if err != nil || results[2].Item == nil {
		t.Fatalf("batch = %#v, %v", results, err)
	}
	namespaces, err := memory.ListNamespaces(ctx, nil)
	if err != nil || len(namespaces) != 2 || namespaces[0][0] != "a" || namespaces[1][0] != "b" {
		t.Fatalf("namespaces = %#v, %v", namespaces, err)
	}
}
