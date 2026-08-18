package dastore

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestSearchOptionsNormalizeFiniteDefaultsAndRejectNegatives(t *testing.T) {
	options, err := (SearchOptions{}).Normalized()
	if err != nil {
		t.Fatal(err)
	}
	if options.Limit != DefaultSearchLimit || options.Offset != 0 {
		t.Fatalf("normalized options = %+v", options)
	}
	for _, options := range []SearchOptions{{Limit: -1}, {Offset: -1}} {
		if _, err := options.Normalized(); err == nil {
			t.Fatalf("Normalized(%+v) accepted a negative value", options)
		}
	}
}

func TestListNamespacesOptionsNormalizeFiniteDefaultsAndRejectNegatives(t *testing.T) {
	options, err := (ListNamespacesOptions{}).Normalized()
	if err != nil {
		t.Fatal(err)
	}
	if options.Limit != DefaultListNamespacesLimit || options.Offset != 0 || options.MaxDepth != 0 {
		t.Fatalf("normalized options = %+v", options)
	}
	for _, options := range []ListNamespacesOptions{{Limit: -1}, {Offset: -1}, {MaxDepth: -1}} {
		if _, err := options.Normalized(); err == nil {
			t.Fatalf("Normalized(%+v) accepted a negative value", options)
		}
	}
}

func TestMemorySearchAppliesFiniteDefaultLimit(t *testing.T) {
	memory := NewMemory()
	operations := make([]Operation, DefaultSearchLimit+1)
	for index := range operations {
		operations[index] = Operation{
			Namespace: Namespace{"bounded"},
			Key:       fmt.Sprintf("item-%03d", index),
			PutValue:  map[string]any{"index": index},
		}
	}
	if _, err := memory.Batch(context.Background(), operations); err != nil {
		t.Fatal(err)
	}
	items, err := memory.Search(context.Background(), SearchOptions{Prefix: Namespace{"bounded"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != DefaultSearchLimit {
		t.Fatalf("default search length = %d, want %d", len(items), DefaultSearchLimit)
	}
}

func TestMemorySearchSelectionRetainsOnlyRequestedWindow(t *testing.T) {
	selection := boundedItemSelection{limit: 3}
	for index := 9999; index >= 0; index-- {
		selection.Add(Item{Namespace: Namespace{"large"}, Key: fmt.Sprintf("item-%05d", index)})
		if len(selection.items) > selection.limit {
			t.Fatalf("retained %d items for limit %d", len(selection.items), selection.limit)
		}
	}
	items := selection.Sorted()
	if len(items) != 3 || items[0].Key != "item-00000" || items[2].Key != "item-00002" {
		t.Fatalf("selected items = %#v", items)
	}
}

func TestMemoryListNamespacesIsBoundedAndSupportsFilters(t *testing.T) {
	memory := NewMemory()
	operations := make([]Operation, DefaultListNamespacesLimit+25)
	for index := range operations {
		operations[index] = Operation{
			Namespace: Namespace{"tenant", fmt.Sprintf("%03d", index), "tail"},
			Key:       "item",
			PutValue:  map[string]any{"index": index},
		}
	}
	if _, err := memory.Batch(context.Background(), operations); err != nil {
		t.Fatal(err)
	}
	namespaces, err := memory.ListNamespaces(context.Background(), ListNamespacesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(namespaces) != DefaultListNamespacesLimit {
		t.Fatalf("default namespace length = %d, want %d", len(namespaces), DefaultListNamespacesLimit)
	}
	namespaces, err = memory.ListNamespaces(context.Background(), ListNamespacesOptions{
		Prefix: Namespace{"tenant", "*"}, Suffix: Namespace{"tail"}, MaxDepth: 2, Limit: 3, Offset: 98,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Namespace{{"tenant", "098"}, {"tenant", "099"}, {"tenant", "100"}}
	if !reflect.DeepEqual(namespaces, want) {
		t.Fatalf("filtered namespaces = %#v, want %#v", namespaces, want)
	}
	for _, options := range []ListNamespacesOptions{{Limit: -1}, {Offset: -1}, {MaxDepth: -1}} {
		if _, err := memory.ListNamespaces(context.Background(), options); err == nil {
			t.Fatalf("ListNamespaces(%+v) accepted a negative value", options)
		}
	}
}

func TestMemoryNamespaceSelectionRetainsOnlyRequestedWindow(t *testing.T) {
	selection := boundedNamespaceSelection{limit: 4}
	for index := 9999; index >= 0; index-- {
		selection.Add(Namespace{"large", fmt.Sprintf("%05d", index)})
		if len(selection.items) > selection.limit || len(selection.retained) > selection.limit {
			t.Fatalf("retained %d items and %d keys for limit %d", len(selection.items), len(selection.retained), selection.limit)
		}
	}
	items := selection.Sorted()
	if len(items) != 4 || items[0].key != "large\x0000000" || items[3].key != "large\x0000003" {
		t.Fatalf("selected namespaces = %#v", items)
	}
}

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
	namespaces, err := memory.ListNamespaces(ctx, ListNamespacesOptions{})
	if err != nil || len(namespaces) != 2 || namespaces[0][0] != "a" || namespaces[1][0] != "b" {
		t.Fatalf("namespaces = %#v, %v", namespaces, err)
	}
}
