package contexthub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/dago/backend"
)

type pushCall struct {
	changes map[string]*Entry
	parent  string
}

type fakeClient struct {
	mu        sync.Mutex
	tree      Tree
	pullErr   error
	pushErr   error
	pulls     int
	pushes    []pushCall
	nextIndex int
}

func (client *fakeClient) Pull(ctx context.Context, _ string) (Tree, error) {
	if err := ctx.Err(); err != nil {
		return Tree{}, err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.pulls++
	return client.tree, client.pullErr
}

func (client *fakeClient) Push(ctx context.Context, _ string, changes map[string]*Entry, parent string) (PushResult, error) {
	if err := ctx.Err(); err != nil {
		return PushResult{}, err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	copyChanges := make(map[string]*Entry, len(changes))
	for name, entry := range changes {
		if entry != nil {
			copy := *entry
			copyChanges[name] = &copy
		} else {
			copyChanges[name] = nil
		}
	}
	client.pushes = append(client.pushes, pushCall{changes: copyChanges, parent: parent})
	if client.pushErr != nil {
		return PushResult{}, client.pushErr
	}
	client.nextIndex++
	return PushResult{CommitHash: fmt.Sprintf("commit-%d", client.nextIndex)}, nil
}

func newTestBackend(t *testing.T, client *fakeClient) *Backend {
	t.Helper()
	result, err := New("-/agent", client)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLazyPullReadPaginationAndLinkedEntries(t *testing.T) {
	client := &fakeClient{tree: Tree{CommitHash: "base", Entries: map[string]Entry{
		"a.md":            {Kind: EntryFile, Content: "1\r\n2\n3\n4"},
		"skills/reviewer": {Kind: EntrySkill, RepoHandle: "reviewer"},
	}}}
	remote := newTestBackend(t, client)
	ctx := context.Background()

	result, err := remote.Read(ctx, "/a.md", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Data.Content; got != "2\n3\n" {
		t.Fatalf("content = %q", got)
	}
	if result.TotalLines == nil || *result.TotalLines != 4 || result.StartLine == nil || *result.StartLine != 2 || result.EndLine == nil || *result.EndLine != 3 || result.NextOffset == nil || *result.NextOffset != 3 {
		t.Fatalf("pagination = %#v", result)
	}
	linked, err := remote.LinkedEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	linked["mutated"] = "bad"
	linkedAgain, _ := remote.LinkedEntries(ctx)
	if linkedAgain["skills/reviewer"] != "reviewer" || linkedAgain["mutated"] != "" {
		t.Fatalf("linked entries = %#v", linkedAgain)
	}
	if client.pulls != 1 {
		t.Fatalf("pulls = %d", client.pulls)
	}
}

func TestReadAndEditUseSharedTextSemantics(t *testing.T) {
	client := &fakeClient{tree: Tree{Entries: map[string]Entry{
		"blank.md": {Kind: EntryFile, Content: " \r\n\t"},
		"eof.md":   {Kind: EntryFile, Content: "one\r\ntwo"},
	}}}
	remote := newTestBackend(t, client)
	ctx := context.Background()

	blank, err := remote.Read(ctx, "/blank.md", 100, 0)
	if err != nil || blank.Data.Content != " \r\n\t" || blank.NoLinesRequested {
		t.Fatalf("blank read = %#v, %v", blank, err)
	}
	if _, err := remote.Read(ctx, "/eof.md", 2, 1); err == nil {
		t.Fatal("out-of-range read succeeded")
	}
	if _, err := remote.Edit(ctx, "/eof.md", "two\n", "second\n", false); err == nil || !strings.Contains(err.Error(), "trailing newline removed") {
		t.Fatalf("EOF edit error = %v", err)
	}
	edited, err := remote.Edit(ctx, "/eof.md", "one\ntwo", "done", false)
	if err != nil || edited.Occurrences != 1 || client.pushes[len(client.pushes)-1].changes["eof.md"].Content != "done" {
		t.Fatalf("normalized edit = %#v, %v", edited, err)
	}
}

func TestMissingRepoStartsEmptyAndFirstWriteCreatesHistory(t *testing.T) {
	client := &fakeClient{pullErr: ErrRepositoryNotFound}
	remote := newTestBackend(t, client)
	ctx := context.Background()

	prior, err := remote.HasPriorCommits(ctx)
	if err != nil || prior {
		t.Fatalf("prior = %v, err = %v", prior, err)
	}
	if _, err := remote.Write(ctx, "/seed.md", "hello"); err != nil {
		t.Fatal(err)
	}
	prior, err = remote.HasPriorCommits(ctx)
	if err != nil || !prior {
		t.Fatalf("prior = %v, err = %v", prior, err)
	}
	if len(client.pushes) != 1 || client.pushes[0].parent != "" || client.pushes[0].changes["seed.md"].Content != "hello" {
		t.Fatalf("pushes = %#v", client.pushes)
	}
	result, err := remote.Read(ctx, "/seed.md", 0, 20)
	if err != nil || result.Data.Content != "hello" || client.pulls != 1 {
		t.Fatalf("read = %#v, err = %v, pulls = %d", result, err, client.pulls)
	}
}

func TestSequentialAndConcurrentWritesChainParents(t *testing.T) {
	client := &fakeClient{tree: Tree{CommitHash: "base", Entries: map[string]Entry{}}}
	remote := newTestBackend(t, client)
	ctx := context.Background()
	const count = 12
	var wait sync.WaitGroup
	for index := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := remote.Write(ctx, fmt.Sprintf("/%d", index), "x"); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wait.Wait()
	if len(client.pushes) != count {
		t.Fatalf("push count = %d", len(client.pushes))
	}
	for index, call := range client.pushes {
		want := "base"
		if index > 0 {
			want = fmt.Sprintf("commit-%d", index)
		}
		if call.parent != want {
			t.Fatalf("push %d parent = %q, want %q", index, call.parent, want)
		}
	}
}

func TestPushFailureInvalidatesCache(t *testing.T) {
	client := &fakeClient{tree: Tree{CommitHash: "base", Entries: map[string]Entry{"a": {Kind: EntryFile, Content: "a"}}}, pushErr: errors.New("unavailable")}
	remote := newTestBackend(t, client)
	ctx := context.Background()
	if _, err := remote.Write(ctx, "/b", "b"); err == nil {
		t.Fatal("write succeeded")
	}
	client.pushErr = nil
	if _, err := remote.Read(ctx, "/a", 0, 2); err != nil {
		t.Fatal(err)
	}
	if client.pulls != 2 {
		t.Fatalf("pulls = %d", client.pulls)
	}
}

func TestListEditAndRecursiveDelete(t *testing.T) {
	client := &fakeClient{tree: Tree{CommitHash: "base", Entries: map[string]Entry{
		"root.md":   {Kind: EntryFile, Content: "x"},
		"work/a.md": {Kind: EntryFile, Content: "x x x"},
		"work/b.md": {Kind: EntryFile, Content: "y"},
	}}}
	remote := newTestBackend(t, client)
	ctx := context.Background()
	listed, err := remote.List(ctx, "/")
	if err != nil || len(listed.Entries) != 2 || listed.Entries[0].Path != "/root.md" || listed.Entries[1].Path != "/work" || !listed.Entries[1].IsDir {
		t.Fatalf("list = %#v, err = %v", listed, err)
	}
	if _, err := remote.Edit(ctx, "/work/a.md", "x", "z", false); err == nil {
		t.Fatal("ambiguous edit succeeded")
	}
	edited, err := remote.Edit(ctx, "/work/a.md", "x", "z", true)
	if err != nil || edited.Occurrences != 3 {
		t.Fatalf("edit = %#v, err = %v", edited, err)
	}
	if _, err := remote.Delete(ctx, "/work"); err != nil {
		t.Fatal(err)
	}
	last := client.pushes[len(client.pushes)-1].changes
	if len(last) != 2 || last["work/a.md"] != nil || last["work/b.md"] != nil {
		t.Fatalf("delete changes = %#v", last)
	}
	if _, err := remote.Delete(ctx, "/"); err == nil {
		t.Fatal("root delete succeeded")
	}
}

func TestGlobAndRegexGrepMatchPythonFnmatchSemantics(t *testing.T) {
	client := &fakeClient{tree: Tree{Entries: map[string]Entry{
		"src/a.py":        {Kind: EntryFile, Content: "hit\nhit\n"},
		"src/b.md":        {Kind: EntryFile, Content: "hit\n"},
		"literal.{py,md}": {Kind: EntryFile, Content: "hit"},
	}}}
	remote := newTestBackend(t, client)
	ctx := context.Background()
	grep, err := remote.Grep(ctx, "^hit$", backend.GrepOptions{Glob: "*.py", MaxCount: 1})
	if err != nil || len(grep.Matches) != 1 || grep.Matches[0].Path != "/src/a.py" || !grep.Truncated {
		t.Fatalf("grep = %#v, err = %v", grep, err)
	}
	exact, err := remote.Grep(ctx, "hit", backend.GrepOptions{Path: "/src", MaxCount: 3})
	if err != nil || len(exact.Matches) != 3 || exact.Truncated {
		t.Fatalf("exact grep = %#v, err = %v", exact, err)
	}
	braces, err := remote.Grep(ctx, "hit", backend.GrepOptions{Glob: "*.{py,md}"})
	if err != nil || len(braces.Matches) != 1 || braces.Matches[0].Path != "/literal.{py,md}" {
		t.Fatalf("brace grep = %#v, err = %v", braces, err)
	}
	glob, err := remote.Glob(ctx, "src/*.py", "/ignored")
	if err != nil || len(glob.Matches) != 1 || glob.Matches[0].Path != "/src/a.py" {
		t.Fatalf("glob = %#v, err = %v", glob, err)
	}
}

func TestGrepDoesNotInventALineForEmptyFiles(t *testing.T) {
	client := &fakeClient{tree: Tree{Entries: map[string]Entry{"empty": {Kind: EntryFile, Content: ""}}}}
	remote := newTestBackend(t, client)
	result, err := remote.Grep(context.Background(), "^$", backend.GrepOptions{})
	if err != nil || len(result.Matches) != 0 {
		t.Fatalf("grep = %#v, err = %v", result, err)
	}
}

func TestUploadAndDownloadBatchContracts(t *testing.T) {
	client := &fakeClient{tree: Tree{Entries: map[string]Entry{}}}
	remote := newTestBackend(t, client)
	ctx := context.Background()
	results := remote.Upload(ctx, []backend.Upload{
		{Path: "/dup.md", Content: []byte("first")},
		{Path: "/bad.bin", Content: []byte{0xff}},
		{Path: "/dup.md", Content: []byte("second")},
		{Path: "/other.md", Content: []byte("other")},
	})
	if results[0].Error != "" || results[1].Error != "invalid_path" || results[2].Error != "" || results[3].Error != "" {
		t.Fatalf("upload = %#v", results)
	}
	if len(client.pushes) != 1 || len(client.pushes[0].changes) != 2 || client.pushes[0].changes["dup.md"].Content != "second" {
		t.Fatalf("push = %#v", client.pushes)
	}
	downloads := remote.Download(ctx, []string{"/dup.md", "/missing"})
	if string(downloads[0].Content) != "second" || downloads[0].Error != "" || downloads[1].Error != "file_not_found" {
		t.Fatalf("download = %#v", downloads)
	}
}

func TestUploadCommitFailureMarksOnlyValidInputs(t *testing.T) {
	client := &fakeClient{tree: Tree{Entries: map[string]Entry{}}, pushErr: errors.New("down")}
	remote := newTestBackend(t, client)
	results := remote.Upload(context.Background(), []backend.Upload{
		{Path: "/a", Content: []byte("a")},
		{Path: "/bad", Content: []byte{0xff}},
		{Path: "/b", Content: []byte("b")},
	})
	if results[0].Error == "" || results[1].Error != "invalid_path" || results[2].Error == "" {
		t.Fatalf("results = %#v", results)
	}
}
