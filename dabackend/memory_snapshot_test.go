package dabackend

import (
	"context"
	"testing"
)

func TestMemorySnapshotIsIndependent(t *testing.T) {
	memory, err := NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Write(context.Background(), "/workspace/a.txt", "one"); err != nil {
		t.Fatal(err)
	}
	snapshot := memory.Snapshot()
	snapshot["/workspace/a.txt"] = FileData{Content: "changed", Encoding: EncodingUTF8}
	snapshot["/workspace/new.txt"] = FileData{Content: "new", Encoding: EncodingUTF8}

	read, err := memory.Read(context.Background(), "/workspace/a.txt", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if read.Data == nil || read.Data.Content != "one" {
		t.Fatalf("memory changed through snapshot: %#v", read)
	}
	if _, err := memory.Read(context.Background(), "/workspace/new.txt", 0, 0); err == nil {
		t.Fatal("snapshot-only file appeared in memory")
	}
}
