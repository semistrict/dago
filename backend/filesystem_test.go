package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testFilesystem(t *testing.T) *Filesystem {
	t.Helper()
	backend, err := NewFilesystem(FilesystemOptions{Root: t.TempDir(), MaxResults: 100})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func TestFilesystemCRUDPaginationGlobAndGrep(t *testing.T) {
	backend := testFilesystem(t)
	ctx := context.Background()
	if result, err := backend.Write(ctx, "/docs/a.txt", "one\ntarget\nthree\ntarget\nfive\n"); err != nil || result.Path != "/docs/a.txt" {
		t.Fatalf("Write = %#v, %v", result, err)
	}
	if _, err := backend.Write(ctx, "/docs/b.md", "target markdown"); err != nil {
		t.Fatal(err)
	}
	read, err := backend.Read(ctx, "/docs/a.txt", 1, 2)
	if err != nil || read.Data.Content != "target\nthree" || *read.StartLine != 2 || *read.EndLine != 3 || *read.NextOffset != 3 {
		t.Fatalf("Read = %#v, %v", read, err)
	}
	zero, err := backend.Read(ctx, "/missing", 0, 0)
	if err != nil || !zero.NoLinesRequested {
		t.Fatalf("zero Read = %#v, %v", zero, err)
	}
	edit, err := backend.Edit(ctx, "/docs/a.txt", "target", "match", true)
	if err != nil || edit.Occurrences != 2 {
		t.Fatalf("Edit = %#v, %v", edit, err)
	}
	glob, err := backend.Glob(ctx, "**/*.txt", "/")
	if err != nil || len(glob.Matches) != 1 || glob.Matches[0].Path != "/docs/a.txt" {
		t.Fatalf("Glob = %#v, %v", glob, err)
	}
	grep, err := backend.Grep(ctx, "match", GrepOptions{Path: "/docs", Glob: "*.txt", MaxCount: 1, ContextLines: 1})
	if err != nil || len(grep.Matches) != 1 || !grep.Truncated || grep.Matches[0].Line != 2 {
		t.Fatalf("Grep = %#v, %v", grep, err)
	}
	listing, err := backend.List(ctx, "/docs")
	if err != nil || len(listing.Entries) != 2 {
		t.Fatalf("List = %#v, %v", listing, err)
	}
	if _, err := backend.Delete(ctx, "/docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.List(ctx, "/docs"); err == nil {
		t.Fatal("deleted directory still exists")
	}
}

func TestFilesystemBlocksTraversalAndSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	backend, err := NewFilesystem(FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(context.Background(), "/../escape", "bad"); err == nil {
		t.Fatal("traversal write succeeded")
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(context.Background(), "/link/escape", "bad"); err == nil {
		t.Fatal("symlink escape write succeeded")
	}
	if _, err := backend.Write(context.Background(), "/safe", "good"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(context.Background(), "/file-link", "bad"); err == nil {
		t.Fatal("file symlink escape write succeeded")
	}
}

func TestFilesystemBinaryAndBatchPartialResults(t *testing.T) {
	backend := testFilesystem(t)
	ctx := context.Background()
	uploads := backend.Upload(ctx, []Upload{{Path: "/binary", Content: []byte{0xff, 0x00}}, {Path: "/nested/text", Content: []byte("ok")}})
	if uploads[0].Error != "" || uploads[1].Error != "" {
		t.Fatalf("Upload = %#v", uploads)
	}
	read, err := backend.Read(ctx, "/binary", 0, 10)
	if err != nil || read.Data.Encoding != EncodingBase64 {
		t.Fatalf("binary Read = %#v, %v", read, err)
	}
	downloads := backend.Download(ctx, []string{"/binary", "/missing", "/nested"})
	if !reflect.DeepEqual(downloads[0].Content, []byte{0xff, 0x00}) || downloads[1].Error != "file_not_found" || downloads[2].Error != "is_directory" {
		t.Fatalf("Download = %#v", downloads)
	}
}

func TestFilesystemHonorsCancellation(t *testing.T) {
	backend := testFilesystem(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.Grep(ctx, "x", GrepOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Grep error = %v", err)
	}
}
