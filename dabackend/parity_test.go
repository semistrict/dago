package dabackend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/dago/dastore"
)

func TestMemoryGrepCappedResultsAreDeterministic(t *testing.T) {
	memory := NewMemory(map[string]FileData{
		"/z.txt": {Content: "needle", Encoding: EncodingUTF8},
		"/a.txt": {Content: "needle", Encoding: EncodingUTF8},
		"/m.txt": {Content: "needle", Encoding: EncodingUTF8},
	})
	for range 50 {
		result, err := memory.Grep(t.Context(), "needle", GrepOptions{Path: "/", MaxCount: 2})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncated || len(result.Matches) != 2 || result.Matches[0].Path != "/a.txt" || result.Matches[1].Path != "/m.txt" {
			t.Fatalf("capped Grep = %#v", result)
		}
	}
}

type recordingStore struct {
	dastore.Store
	puts    []string
	batches [][]dastore.Operation
}

type blockingGetStore struct {
	dastore.Store
	key         string
	started     chan struct{}
	unblock     chan struct{}
	startOnce   sync.Once
	mu          sync.Mutex
	blockedGets int
	searches    int
}

func (store *blockingGetStore) Get(ctx context.Context, namespace dastore.Namespace, key string) (*dastore.Item, error) {
	if key == store.key {
		store.mu.Lock()
		store.blockedGets++
		store.mu.Unlock()
		store.startOnce.Do(func() { close(store.started) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-store.unblock:
		}
	}
	return store.Store.Get(ctx, namespace, key)
}

func (store *blockingGetStore) Search(ctx context.Context, options dastore.SearchOptions) ([]dastore.Item, error) {
	store.mu.Lock()
	store.searches++
	store.mu.Unlock()
	return store.Store.Search(ctx, options)
}

func (store *blockingGetStore) counts() (int, int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.blockedGets, store.searches
}

func (store *recordingStore) Put(ctx context.Context, namespace dastore.Namespace, key string, value map[string]any) error {
	store.puts = append(store.puts, key)
	return store.Store.Put(ctx, namespace, key, value)
}

func (store *recordingStore) Batch(ctx context.Context, operations []dastore.Operation) ([]dastore.Result, error) {
	copy := append([]dastore.Operation(nil), operations...)
	store.batches = append(store.batches, copy)
	return store.Store.Batch(ctx, operations)
}

func TestStoreMutationsOnlyPersistTargetedKeys(t *testing.T) {
	ctx := t.Context()
	namespace := dastore.Namespace{"files"}
	values := dastore.NewMemory()
	seed := FileData{Content: "keep", Encoding: EncodingUTF8}
	if err := values.Put(ctx, namespace, "/keep.txt", fileDataMap(seed)); err != nil {
		t.Fatal(err)
	}
	recorded := &recordingStore{Store: values}
	backend := NewStore(recorded, namespace)
	if _, err := backend.Write(ctx, "/new.txt", "new"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorded.puts, []string{"/new.txt"}) {
		t.Fatalf("Write persisted keys %v", recorded.puts)
	}
	recorded.puts = nil
	if _, err := backend.Edit(ctx, "/keep.txt", "keep", "updated", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorded.puts, []string{"/keep.txt"}) {
		t.Fatalf("Edit persisted keys %v", recorded.puts)
	}
	uploads := backend.Upload(ctx, []Upload{{Path: "/one.bin", Content: []byte{0xff}}, {Path: "/two.txt", Content: []byte("two")}})
	if uploads[0].Error != "" || uploads[1].Error != "" {
		t.Fatalf("Upload = %#v", uploads)
	}
	if len(recorded.batches) != 1 || len(recorded.batches[0]) != 2 || recorded.batches[0][0].Key != "/one.bin" || recorded.batches[0][1].Key != "/two.txt" {
		t.Fatalf("Upload batches = %#v", recorded.batches)
	}
	item, err := values.Get(ctx, namespace, "/keep.txt")
	if err != nil || item == nil || item.Value["content"] != "updated" {
		t.Fatalf("unrelated item = %#v, %v", item, err)
	}
}

func TestStoreMutationLocksAreKeyScopedPrefixAwareAndCancelable(t *testing.T) {
	values := &blockingGetStore{
		Store:   dastore.NewMemory(),
		key:     "/dir/file.txt",
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	backend := NewStore(values, dastore.Namespace{"files"})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(values.unblock) }) }
	defer unblock()

	held := make(chan error, 1)
	go func() {
		_, err := backend.Write(context.Background(), "/dir/file.txt", "held")
		held <- err
	}()
	select {
	case <-values.started:
	case <-time.After(time.Second):
		t.Fatal("first keyed mutation did not reach the store")
	}

	otherCtx, cancelOther := context.WithTimeout(t.Context(), time.Second)
	defer cancelOther()
	if _, err := backend.Write(otherCtx, "/dir", "independent exact key"); err != nil {
		t.Fatalf("independent keyed Write = %v", err)
	}

	sameCtx, cancelSame := context.WithCancel(t.Context())
	sameDone := make(chan error, 1)
	go func() {
		_, err := backend.Write(sameCtx, "/dir/file.txt", "second")
		sameDone <- err
	}()
	cancelSame()
	select {
	case err := <-sameDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("same-key canceled Write error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("same-key canceled Write remained blocked")
	}

	deleteCtx, cancelDelete := context.WithCancel(t.Context())
	deleteDone := make(chan error, 1)
	go func() {
		_, err := backend.Delete(deleteCtx, "/dir")
		deleteDone <- err
	}()
	cancelDelete()
	select {
	case err := <-deleteDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("overlapping canceled Delete error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("overlapping canceled Delete remained blocked")
	}

	if gets, searches := values.counts(); gets != 1 || searches != 0 {
		t.Fatalf("blocked operations reached store: gets=%d searches=%d", gets, searches)
	}
	unblock()
	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("held Write = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("held Write did not finish after release")
	}
}

func TestFilesystemSeparatesReadMediaAndGrepLimits(t *testing.T) {
	root := t.TempDir()
	backend, err := NewFilesystem(FilesystemOptions{Root: root, MaxFileSize: 4, MaxVideoSize: 6, MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("needle\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := backend.Read(t.Context(), "/large.txt", 0, 1)
	if err != nil || read.Data == nil || read.Data.Content != "needle\n" || read.NextOffset == nil || *read.NextOffset != 1 {
		t.Fatalf("large text Read = %#v, %v", read, err)
	}
	grep, err := backend.Grep(t.Context(), "needle", GrepOptions{Path: "/"})
	if err != nil || len(grep.Matches) != 0 {
		t.Fatalf("oversize Grep = %#v, %v", grep, err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	capped, err := backend.Grep(t.Context(), "x", GrepOptions{Path: "/"})
	if err != nil || len(capped.Matches) != 1 || !capped.Truncated {
		t.Fatalf("default-capped Grep = %#v, %v", capped, err)
	}
	uncapped, err := backend.Grep(t.Context(), "x", GrepOptions{Path: "/", Uncapped: true})
	if err != nil || len(uncapped.Matches) != 2 || uncapped.Truncated {
		t.Fatalf("uncapped Grep = %#v, %v", uncapped, err)
	}
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("1234567"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Read(t.Context(), "/clip.mp4", 0, 1); err == nil || !errors.Is(err, ErrPayloadTooLarge) || !strings.Contains(err.Error(), "media file exceeds 6 bytes") {
		t.Fatalf("oversize media Read error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "large-image.png"), []byte("1234567"), 0o600); err != nil {
		t.Fatal(err)
	}
	largeMedia, err := backend.Read(t.Context(), "/large-image.png", 0, 1)
	if err != nil || largeMedia.Data == nil || largeMedia.Data.Content != base64.StdEncoding.EncodeToString([]byte("1234567")) {
		t.Fatalf("non-video binary Read = %#v, %v", largeMedia, err)
	}
	if err := os.WriteFile(filepath.Join(root, "image.png"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	media, err := backend.Read(t.Context(), "/image.png", 0, 1)
	if err != nil || media.Data == nil || media.Data.Content != base64.StdEncoding.EncodeToString([]byte("1234")) {
		t.Fatalf("media Read = %#v, %v", media, err)
	}
}

func TestFilesystemPaginatesTextLargerThanScanLimit(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "large.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("wanted\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(11 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err := NewFilesystem(FilesystemOptions{Root: root, MaxFileSize: 10 << 20})
	if err != nil {
		t.Fatal(err)
	}
	read, err := backend.Read(t.Context(), "/large.txt", 0, 1)
	if err != nil || read.Data == nil || read.Data.Content != "wanted\n" || read.TotalLines == nil || *read.TotalLines != 2 || read.NextOffset == nil || *read.NextOffset != 1 {
		t.Fatalf("large paginated read = %#v, %v", read, err)
	}
	grep, err := backend.Grep(t.Context(), "wanted", GrepOptions{Path: "/"})
	if err != nil || len(grep.Matches) != 0 {
		t.Fatalf("scan-limited grep = %#v, %v", grep, err)
	}
}

func TestFilesystemGrepPreservesMatchesAndReportsReadFailures(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "z.txt")); err != nil {
		t.Fatal(err)
	}
	backend, err := NewFilesystem(FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Grep(t.Context(), "hit", GrepOptions{Path: "/"})
	if err != nil || len(result.Matches) != 1 || result.Matches[0].Path != "/a.txt" || !strings.Contains(result.Error, "/z.txt") {
		t.Fatalf("partial grep = %#v, %v", result, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := backend.Grep(canceled, "hit", GrepOptions{Path: "/"}); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Grep error = %v", err)
	}
}

func TestBinaryPathsEncodeValidUTF8Bytes(t *testing.T) {
	ctx := t.Context()
	want := base64.StdEncoding.EncodeToString([]byte("ascii-image"))
	memory := NewMemory(map[string]FileData{"/image.png": {Content: "ascii-image", Encoding: EncodingUTF8}})
	read, err := memory.Read(ctx, "/image.png", 0, 1)
	if err != nil || read.Data == nil || read.Data.Encoding != EncodingBase64 || read.Data.Content != want {
		t.Fatalf("Memory.Read = %#v, %v", read, err)
	}
	values := dastore.NewMemory()
	namespace := dastore.Namespace{"files"}
	if err := values.Put(ctx, namespace, "/image.png", fileDataMap(FileData{Content: "ascii-image", Encoding: EncodingUTF8})); err != nil {
		t.Fatal(err)
	}
	store := NewStore(values, namespace)
	read, err = store.Read(ctx, "/image.png", 0, 1)
	if err != nil || read.Data == nil || read.Data.Encoding != EncodingBase64 || read.Data.Content != want {
		t.Fatalf("Store.Read = %#v, %v", read, err)
	}
}

type batchingBackend struct {
	*Memory
	uploads   [][]Upload
	downloads [][]string
}

type overReturningBackend struct{ *Memory }

type nonComparableBackend struct {
	*Memory
	marker []string
}

func (backend *overReturningBackend) Grep(context.Context, string, GrepOptions) (GrepResult, error) {
	return GrepResult{Matches: []GrepMatch{{Path: "/a", Line: 1}, {Path: "/b", Line: 1}}, Error: strings.Repeat("x", MaxGrepErrorBytes+100)}, nil
}

func TestCompositeEnforcesGlobalGrepCapAndErrorBound(t *testing.T) {
	memory := NewMemory(nil)
	composite := NewComposite(&overReturningBackend{Memory: memory}, nil)
	result, err := composite.Grep(t.Context(), "hit", GrepOptions{MaxCount: 1})
	if err != nil || len(result.Matches) != 1 || !result.Truncated || len(result.Error) > MaxGrepErrorBytes || !strings.Contains(result.Error, "truncated") {
		t.Fatalf("Composite.Grep = %#v, %v", result, err)
	}
}

func TestCompositeBoundsTargetedAndUncappedGrepErrors(t *testing.T) {
	memory := NewMemory(nil)
	bad := &overReturningBackend{Memory: memory}
	composite := NewComposite(bad, map[string]Backend{"/mounted": bad})
	for name, options := range map[string]GrepOptions{
		"targeted": {Path: "/mounted"},
		"uncapped": {Uncapped: true},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := composite.Grep(t.Context(), "hit", options)
			if err != nil || len(result.Error) > MaxGrepErrorBytes || !strings.Contains(result.Error, "truncated") {
				t.Fatalf("Composite.Grep = %#v, %v", result, err)
			}
		})
	}
}

func TestCompositeRejectsBackendWithoutStableBatchIdentity(t *testing.T) {
	memory := NewMemory(nil)
	backend := nonComparableBackend{Memory: memory, marker: []string{"not comparable"}}
	defer func() {
		if value := recover(); value == nil || !strings.Contains(fmt.Sprint(value), "stable identity") {
			t.Fatalf("NewComposite() panic = %v", value)
		}
	}()
	NewComposite(backend, nil)
}

func TestBuiltInGrepRejectsNegativeLimits(t *testing.T) {
	memory := NewMemory(nil)
	for name, options := range map[string]GrepOptions{
		"max_count":     {MaxCount: -1},
		"context_lines": {ContextLines: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := memory.Grep(t.Context(), "hit", options); err == nil {
				t.Fatalf("Grep(%#v) accepted negative limit", options)
			}
		})
	}
}

func newBatchingBackend(t *testing.T) *batchingBackend {
	t.Helper()
	memory := NewMemory(nil)
	return &batchingBackend{Memory: memory}
}

func (backend *batchingBackend) Upload(_ context.Context, uploads []Upload) []UploadResult {
	backend.uploads = append(backend.uploads, append([]Upload(nil), uploads...))
	results := make([]UploadResult, len(uploads))
	for index, upload := range uploads {
		results[index].Path = "/child" + upload.Path
	}
	return results
}

func (backend *batchingBackend) Download(_ context.Context, paths []string) []DownloadResult {
	backend.downloads = append(backend.downloads, append([]string(nil), paths...))
	results := make([]DownloadResult, len(paths))
	for index, path := range paths {
		results[index] = DownloadResult{Path: "/child" + path, Content: []byte(path)}
	}
	return results
}

func TestCompositeBatchesTransfersByRouteAndRestoresRequestOrder(t *testing.T) {
	root := newBatchingBackend(t)
	mounted := newBatchingBackend(t)
	composite := NewComposite(root, map[string]Backend{"/mounted/": mounted, "/also-mounted/": mounted})
	uploads := []Upload{{Path: "/root-a", Content: []byte("a")}, {Path: "/mounted/a", Content: []byte("b")}, {Path: "/root-b", Content: []byte("c")}, {Path: "/also-mounted/b", Content: []byte("d")}}
	uploadResults := composite.Upload(t.Context(), uploads)
	if len(root.uploads) != 1 || len(root.uploads[0]) != 2 || len(mounted.uploads) != 1 || len(mounted.uploads[0]) != 2 {
		t.Fatalf("upload batches root=%#v mounted=%#v", root.uploads, mounted.uploads)
	}
	for index, result := range uploadResults {
		if result.Path != uploads[index].Path {
			t.Fatalf("upload result %d = %#v", index, result)
		}
	}
	paths := []string{"/mounted/a", "/root-a", "/also-mounted/b", "/root-b"}
	downloadResults := composite.Download(t.Context(), paths)
	if len(root.downloads) != 1 || len(root.downloads[0]) != 2 || len(mounted.downloads) != 1 || len(mounted.downloads[0]) != 2 {
		t.Fatalf("download batches root=%#v mounted=%#v", root.downloads, mounted.downloads)
	}
	for index, result := range downloadResults {
		expectedContent := strings.TrimPrefix(strings.TrimPrefix(paths[index], "/mounted"), "/also-mounted")
		if result.Path != paths[index] || string(result.Content) != expectedContent {
			t.Fatalf("download result %d = %#v", index, result)
		}
	}
}

func TestGrepResultPartialErrorJSONIsStable(t *testing.T) {
	encoded, err := json.Marshal(GrepResult{Matches: []GrepMatch{{Path: "/a", Line: 1, Text: "hit"}}, Error: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"matches":[{"path":"/a","line":1,"text":"hit"}],"error":"partial"}` {
		t.Fatalf("GrepResult JSON = %s", encoded)
	}
}
