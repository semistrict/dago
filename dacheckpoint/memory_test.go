package dacheckpoint

import (
	"context"
	"reflect"
	"testing"
)

func TestMemorySaverPutGetAndList(t *testing.T) {
	ctx := context.Background()
	saver := NewMemorySaver()
	rootConfig := Config{ThreadID: "thread", Namespace: ""}
	cp1 := testCheckpoint("cp1", map[string]any{"value": []string{"one"}}, map[string]string{"value": "v1"})
	config1, err := saver.Put(ctx, rootConfig, cp1, Metadata{Source: "input", Step: 0}, cp1.ChannelVersions)
	if err != nil {
		t.Fatalf("Put(cp1) error = %v", err)
	}
	cp2 := testCheckpoint("cp2", map[string]any{}, map[string]string{"value": "v2"})
	config2, err := saver.Put(ctx, config1, cp2, Metadata{Source: "loop", Step: 1}, cp2.ChannelVersions)
	if err != nil {
		t.Fatalf("Put(cp2) error = %v", err)
	}

	latest, err := saver.GetTuple(ctx, rootConfig)
	if err != nil {
		t.Fatalf("GetTuple(latest) error = %v", err)
	}
	if latest == nil || latest.Config != config2 {
		t.Fatalf("latest config = %+v, want %+v", latest, config2)
	}
	if _, exists := latest.Checkpoint.ChannelValues["value"]; exists {
		t.Fatal("empty channel blob was returned as a value")
	}
	first, err := saver.GetTuple(ctx, config1)
	if err != nil {
		t.Fatalf("GetTuple(cp1) error = %v", err)
	}
	if got := first.Checkpoint.ChannelValues["value"]; !reflect.DeepEqual(got, []string{"one"}) {
		t.Fatalf("cp1 value = %#v", got)
	}

	rows, err := saver.List(ctx, &rootConfig, ListOptions{Before: &config2})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Config.CheckpointID != "cp1" {
		t.Fatalf("List() = %+v", rows)
	}
}

func TestMemorySaverNamespacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	saver := NewMemorySaver()
	for _, namespace := range []string{"", "child"} {
		checkpoint := testCheckpoint("cp-"+namespace, nil, nil)
		if _, err := saver.Put(ctx, Config{ThreadID: "thread", Namespace: namespace}, checkpoint, Metadata{}, nil); err != nil {
			t.Fatalf("Put(%q) error = %v", namespace, err)
		}
	}
	rows, err := saver.List(ctx, &Config{ThreadID: "thread", Namespace: ""}, ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Config.Namespace != "" {
		t.Fatalf("root namespace rows = %+v", rows)
	}
}

func TestMemorySaverWritesAreIdempotentAndSpecialWritesReplace(t *testing.T) {
	ctx := context.Background()
	saver := NewMemorySaver()
	config := Config{ThreadID: "thread", CheckpointID: "cp"}
	writes := []ChannelWrite{{Channel: "a", Value: "first"}, {Channel: ChannelError, Value: "old error"}}
	if err := saver.PutWrites(ctx, config, "task", "path", writes); err != nil {
		t.Fatalf("PutWrites(first) error = %v", err)
	}
	if err := saver.PutWrites(ctx, config, "task", "path", []ChannelWrite{{Channel: "a", Value: "ignored"}, {Channel: ChannelError, Value: "new error"}}); err != nil {
		t.Fatalf("PutWrites(second) error = %v", err)
	}
	checkpoint := testCheckpoint("cp", nil, nil)
	if _, err := saver.Put(ctx, Config{ThreadID: "thread"}, checkpoint, Metadata{}, nil); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	tuple, err := saver.GetTuple(ctx, config)
	if err != nil {
		t.Fatalf("GetTuple() error = %v", err)
	}
	if len(tuple.PendingWrites) != 2 {
		t.Fatalf("pending writes = %+v", tuple.PendingWrites)
	}
	values := map[string]any{}
	for _, write := range tuple.PendingWrites {
		values[write.Channel] = write.Value
	}
	if values["a"] != "first" || values[ChannelError] != "new error" {
		t.Fatalf("pending values = %+v", values)
	}
}

func TestMemorySaverDeltaHistoryUsesNearestSeedAndExcludesTargetWrites(t *testing.T) {
	ctx := context.Background()
	saver := NewMemorySaver()
	root := Config{ThreadID: "thread"}
	cp0 := testCheckpoint("cp0", map[string]any{"items": []string{"older"}}, map[string]string{"items": "v0"})
	c0, err := saver.Put(ctx, root, cp0, Metadata{}, cp0.ChannelVersions)
	if err != nil {
		t.Fatalf("Put(cp0) error = %v", err)
	}
	if err := saver.PutWrites(ctx, c0, "task-0", "0", []ChannelWrite{{Channel: "items", Value: []string{"subsumed"}}}); err != nil {
		t.Fatalf("PutWrites(cp0) error = %v", err)
	}
	cp1 := testCheckpoint("cp1", map[string]any{"items": []string{"seed"}}, map[string]string{"items": "v1"})
	c1, err := saver.Put(ctx, c0, cp1, Metadata{}, cp1.ChannelVersions)
	if err != nil {
		t.Fatalf("Put(cp1) error = %v", err)
	}
	if err := saver.PutWrites(ctx, c1, "task-b", "b", []ChannelWrite{{Channel: "items", Value: []string{"second"}}}); err != nil {
		t.Fatalf("PutWrites(cp1 task-b) error = %v", err)
	}
	if err := saver.PutWrites(ctx, c1, "task-a", "a", []ChannelWrite{{Channel: "items", Value: []string{"first"}}}); err != nil {
		t.Fatalf("PutWrites(cp1 task-a) error = %v", err)
	}
	cp2 := testCheckpoint("cp2", map[string]any{}, map[string]string{"items": "v2"})
	c2, err := saver.Put(ctx, c1, cp2, Metadata{}, cp2.ChannelVersions)
	if err != nil {
		t.Fatalf("Put(cp2) error = %v", err)
	}
	if err := saver.PutWrites(ctx, c2, "target", "", []ChannelWrite{{Channel: "items", Value: []string{"pending"}}}); err != nil {
		t.Fatalf("PutWrites(cp2) error = %v", err)
	}

	history, err := saver.GetDeltaChannelHistory(ctx, c2, []string{"items"})
	if err != nil {
		t.Fatalf("GetDeltaChannelHistory() error = %v", err)
	}
	items := history["items"]
	if !items.HasSeed || !reflect.DeepEqual(items.Seed, []string{"seed"}) {
		t.Fatalf("seed = %#v, present %v", items.Seed, items.HasSeed)
	}
	gotWrites := make([]any, len(items.Writes))
	for index, write := range items.Writes {
		gotWrites[index] = write.Value
	}
	wantWrites := []any{[]string{"first"}, []string{"second"}}
	if !reflect.DeepEqual(gotWrites, wantWrites) {
		t.Fatalf("writes = %#v, want %#v", gotWrites, wantWrites)
	}
}

func TestMemorySaverCopiesAndDeletesThreads(t *testing.T) {
	ctx := context.Background()
	saver := NewMemorySaver()
	cp := testCheckpoint("cp", map[string]any{"value": []string{"source"}}, map[string]string{"value": "v1"})
	if _, err := saver.Put(ctx, Config{ThreadID: "source"}, cp, Metadata{}, cp.ChannelVersions); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := saver.CopyThread(ctx, "source", "target"); err != nil {
		t.Fatalf("CopyThread() error = %v", err)
	}
	if err := saver.DeleteThread(ctx, "source"); err != nil {
		t.Fatalf("DeleteThread() error = %v", err)
	}
	tuple, err := saver.GetTuple(ctx, Config{ThreadID: "target"})
	if err != nil || tuple == nil {
		t.Fatalf("target GetTuple() = %+v, %v", tuple, err)
	}
	if got := tuple.Checkpoint.ChannelValues["value"]; !reflect.DeepEqual(got, []string{"source"}) {
		t.Fatalf("target value = %#v", got)
	}
}

func testCheckpoint(id string, values map[string]any, versions map[string]string) Checkpoint {
	if values == nil {
		values = map[string]any{}
	}
	if versions == nil {
		versions = map[string]string{}
	}
	return Checkpoint{
		Version: LatestVersion, ID: id, Timestamp: id,
		ChannelValues: values, ChannelVersions: versions,
		VersionsSeen: map[string]map[string]string{},
	}
}
