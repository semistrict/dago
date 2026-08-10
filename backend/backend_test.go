package backend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/dago/state"
	memorystore "github.com/semistrict/dago/store"
)

func backendContract(t *testing.T, value Backend) {
	t.Helper()
	ctx := context.Background()
	if _, err := value.Write(ctx, "/dir/a.txt", "alpha\nneedle\nomega"); err != nil {
		t.Fatal(err)
	}
	read, err := value.Read(ctx, "/dir/a.txt", 0, 2)
	if err != nil || read.Data == nil || read.Data.Content != "alpha\nneedle" {
		t.Fatalf("Read = %#v, %v", read, err)
	}
	if _, err := value.Edit(ctx, "/dir/a.txt", "alpha", "first", false); err != nil {
		t.Fatal(err)
	}
	grep, err := value.Grep(ctx, "needle", GrepOptions{Path: "/dir"})
	if err != nil || len(grep.Matches) != 1 {
		t.Fatalf("Grep = %#v, %v", grep, err)
	}
	glob, err := value.Glob(ctx, "**/*.txt", "/")
	if err != nil || len(glob.Matches) != 1 {
		t.Fatalf("Glob = %#v, %v", glob, err)
	}
	if _, err := value.Delete(ctx, "/dir"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAndStoreBackendsShareContract(t *testing.T) {
	memory, err := NewMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	backendContract(t, memory)
	values := memorystore.NewMemory()
	persistent, err := NewStore(values, memorystore.Namespace{"files", "test"})
	if err != nil {
		t.Fatal(err)
	}
	backendContract(t, persistent)
	if _, err := persistent.Write(context.Background(), "/persist", "value"); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(values, memorystore.Namespace{"files", "test"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := reopened.Read(context.Background(), "/persist", 0, 10)
	if err != nil || read.Data.Content != "value" {
		t.Fatalf("reopened Read = %#v, %v", read, err)
	}
}

func TestStateBackendRequiresBindingAndEmitsPlainDataDelta(t *testing.T) {
	value, err := NewState("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.List(context.Background(), "/"); err == nil {
		t.Fatal("unbound State.List succeeded")
	}
	ctx, err := value.BindRuntime(context.Background(), state.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Write(ctx, "/note.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	updates := value.RuntimeUpdates(ctx)
	patch, ok := updates["files"].(map[string]any)
	if !ok {
		t.Fatalf("files update = %#v", updates["files"])
	}
	record, ok := patch["/note.txt"].(map[string]any)
	if !ok || record["content"] != "hello" || record["encoding"] != "utf-8" {
		t.Fatalf("file record = %#v", patch["/note.txt"])
	}

	merged, err := value.StateFields()[0].Reduce(map[string]any{}, []any{patch, map[string]any{"/note.txt": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.(map[string]any)) != 0 {
		t.Fatalf("merged files = %#v", merged)
	}
}

func TestCompositeUsesLongestRouteAndRemapsResults(t *testing.T) {
	root, _ := NewMemory(nil)
	memories, _ := NewMemory(nil)
	private, _ := NewMemory(nil)
	composite, err := NewComposite(root, map[string]Backend{"/memories/": memories, "/memories/private/": private})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = composite.Write(context.Background(), "/root.txt", "root")
	_, _ = composite.Write(context.Background(), "/memories/note.txt", "memory")
	_, _ = composite.Write(context.Background(), "/memories/private/secret.txt", "private")
	if read, err := private.Read(context.Background(), "/secret.txt", 0, 10); err != nil || read.Data.Content != "private" {
		t.Fatalf("private route Read = %#v, %v", read, err)
	}
	listing, err := composite.List(context.Background(), "/")
	if err != nil || len(listing.Entries) != 2 || listing.Entries[0].Path != "/memories/" {
		t.Fatalf("root List = %#v, %v", listing, err)
	}
	glob, err := composite.Glob(context.Background(), "**/*.txt", "/")
	if err != nil || len(glob.Matches) != 3 || glob.Matches[2].Path != "/root.txt" {
		t.Fatalf("composite Glob = %#v, %v", glob, err)
	}
}

func TestLocalShellIsExplicitBoundedAndCancelable(t *testing.T) {
	shell, err := NewLocalShell(LocalShellOptions{
		Filesystem: FilesystemOptions{Root: t.TempDir()}, MaxOutput: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !CapabilitiesOf(shell).Execute {
		t.Fatal("local shell does not advertise execute")
	}
	result, err := shell.Execute(context.Background(), "printf 123456", time.Second)
	if err != nil || result.Output != "1234" || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("Execute = %#v, %v", result, err)
	}
	result, err = shell.Execute(context.Background(), "printf failure; exit 7", time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 7 || !strings.HasPrefix(result.Output, "fail") {
		t.Fatalf("failed Execute = %#v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := shell.Execute(ctx, "sleep 1", time.Second); err == nil {
		t.Fatal("canceled Execute succeeded")
	}
}

func TestLocalShellDrainsRunawayOutputAndReportsTimeouts(t *testing.T) {
	shell, err := NewLocalShell(LocalShellOptions{
		Filesystem: FilesystemOptions{Root: t.TempDir()}, MaxOutput: 1_000,
		DefaultTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := shell.Execute(context.Background(), "yes x | head -n 50000", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 1_000 || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("runaway Execute = %#v", result)
	}

	result, err = shell.Execute(context.Background(), "sleep 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 124 || !strings.Contains(result.Output, "timeout parameter") {
		t.Fatalf("default timeout Execute = %#v", result)
	}
	result, err = shell.Execute(context.Background(), "sleep 1", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == nil || *result.ExitCode != 124 || !strings.Contains(result.Output, "custom timeout") || !strings.Contains(result.Output, "may be stuck") {
		t.Fatalf("custom timeout Execute = %#v", result)
	}
}
