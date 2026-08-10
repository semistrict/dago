package backend

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
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
	if err != nil || read.Data == nil || read.Data.Content != "alpha\nneedle\n" {
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

func TestBackendsPreserveCanonicalReadWindowsAndLegacyFiles(t *testing.T) {
	memory, _ := NewMemory(nil)
	root := t.TempDir()
	filesystem, err := NewFilesystem(FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]Backend{"memory": memory, "filesystem": filesystem} {
		t.Run(name, func(t *testing.T) {
			if _, err := value.Write(context.Background(), "/lines.txt", "one\r\ntwo\rthree\n"); err != nil {
				t.Fatal(err)
			}
			read, err := value.Read(context.Background(), "/lines.txt", 1, 1)
			if err != nil || read.Data == nil || read.Data.Content != "two\n" || read.TotalLines == nil || *read.TotalLines != 3 || read.NextOffset == nil || *read.NextOffset != 2 {
				t.Fatalf("window Read = %#v, %v", read, err)
			}
			if _, err := value.Read(context.Background(), "/lines.txt", 3, 1); err == nil || !strings.Contains(err.Error(), "exceeds file length") {
				t.Fatalf("past-EOF Read error = %v", err)
			}
			if _, err := value.Write(context.Background(), "/blank.txt", " \r\n"); err != nil {
				t.Fatal(err)
			}
			read, err = value.Read(context.Background(), "/blank.txt", 0, 0)
			if err != nil || read.Data == nil || read.Data.Content != " \r\n" || read.NoLinesRequested {
				t.Fatalf("blank zero-limit Read = %#v, %v", read, err)
			}
		})
	}

	if err := os.WriteFile(filepath.Join(root, "image.png"), []byte("ascii-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := filesystem.Read(context.Background(), "/image.png", 100, 0)
	if err != nil || read.Data == nil || read.Data.Encoding != EncodingBase64 || read.Data.Content != base64.StdEncoding.EncodeToString([]byte("ascii-image")) || read.NoLinesRequested {
		t.Fatalf("binary Read = %#v, %v", read, err)
	}

	values := memorystore.NewMemory()
	if err := values.Put(context.Background(), memorystore.Namespace{"files"}, "/legacy.txt", map[string]any{"content": []any{"one", "two"}}); err != nil {
		t.Fatal(err)
	}
	persistent, err := NewStore(values, memorystore.Namespace{"files"})
	if err != nil {
		t.Fatal(err)
	}
	read, err = persistent.Read(context.Background(), "/legacy.txt", 0, 10)
	if err != nil || read.Data == nil || read.Data.Content != "one\ntwo" || read.Data.Encoding != EncodingUTF8 {
		t.Fatalf("legacy Store.Read = %#v, %v", read, err)
	}
	if _, err := persistent.Write(context.Background(), "/legacy.txt", "modern"); err != nil {
		t.Fatal(err)
	}
	item, err := values.Get(context.Background(), memorystore.Namespace{"files"}, "/legacy.txt")
	if err != nil || item == nil || item.Value["content"] != "modern" {
		t.Fatalf("migrated store item = %#v, %v", item, err)
	}
}

func TestFilesystemDeleteDoesNotFollowSymlinksAndRejectsMissing(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	filesystem, err := NewFilesystem(FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Delete(context.Background(), "/link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "link")); !os.IsNotExist(err) {
		t.Fatalf("symlink still exists: %v", err)
	}
	if _, err := filesystem.Delete(context.Background(), "/missing"); err == nil {
		t.Fatal("missing delete succeeded")
	}
}

func TestMemoryGlobAndGrepFollowRecursiveIncludeSemantics(t *testing.T) {
	memory, _ := NewMemory(nil)
	for name := range map[string]string{"/root.go": "needle", "/nested/code.py": "needle", "/nested/skip.md": "needle"} {
		if _, err := memory.Write(context.Background(), name, "needle"); err != nil {
			t.Fatal(err)
		}
	}
	glob, err := memory.Glob(context.Background(), "{*.go,*.py}", "/")
	if err != nil || len(glob.Matches) != 2 || glob.Matches[0].Path != "/nested/code.py" || glob.Matches[1].Path != "/root.go" {
		t.Fatalf("recursive brace Glob = %#v, %v", glob, err)
	}
	grep, err := memory.Grep(context.Background(), "needle", GrepOptions{Path: "/", Glob: "*.py"})
	if err != nil || len(grep.Matches) != 1 || grep.Matches[0].Path != "/nested/code.py" {
		t.Fatalf("basename Grep include = %#v, %v", grep, err)
	}
}

func TestStoreBackendResolvesNamespaceAndStorePerRuntime(t *testing.T) {
	values := memorystore.NewMemory()
	persistent, err := NewStoreWithOptions(StoreOptions{Namespace: func(runtime *Runtime) (memorystore.Namespace, error) {
		if runtime == nil {
			return nil, fmt.Errorf("runtime is required")
		}
		user, ok := runtime.Context.(string)
		if !ok || user == "" {
			return nil, fmt.Errorf("runtime user is required")
		}
		return memorystore.Namespace{"files", user}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.Write(context.Background(), "/note.txt", "outside"); err == nil {
		t.Fatal("runtime-dependent Store.Write succeeded outside a bound run")
	}

	alice, err := BindRuntime(context.Background(), persistent, nil, Runtime{Context: "alice", Store: values})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := BindRuntime(context.Background(), persistent, nil, Runtime{Context: "bob", Store: values})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.Write(alice, "/note.txt", "alice note"); err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.Write(bob, "/note.txt", "bob note"); err != nil {
		t.Fatal(err)
	}
	for name, ctx := range map[string]context.Context{"alice": alice, "bob": bob} {
		result, err := persistent.Read(ctx, "/note.txt", 0, 10)
		if err != nil || result.Data == nil || result.Data.Content != name+" note" {
			t.Fatalf("%s Read = %#v, %v", name, result, err)
		}
	}
	if item, err := values.Get(context.Background(), memorystore.Namespace{"files", "alice"}, "/note.txt"); err != nil || item == nil || item.Value["content"] != "alice note" {
		t.Fatalf("alice stored item = %#v, %v", item, err)
	}
}

func TestStoreBackendRejectsUnsafeDynamicNamespace(t *testing.T) {
	persistent, err := NewStoreWithOptions(StoreOptions{
		Store: memorystore.NewMemory(),
		Namespace: func(*Runtime) (memorystore.Namespace, error) {
			return memorystore.Namespace{"files", "*"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := persistent.Write(context.Background(), "/note.txt", "unsafe"); err == nil || !strings.Contains(err.Error(), "disallowed") {
		t.Fatalf("unsafe namespace error = %v", err)
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

func TestCompositeResolvesDefaultExecutionAndShellRoutes(t *testing.T) {
	root := t.TempDir()
	shell, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: root}})
	if err != nil {
		t.Fatal(err)
	}
	mountedRoot := t.TempDir()
	mounted, err := NewFilesystem(FilesystemOptions{Root: mountedRoot})
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := NewStore(memorystore.NewMemory(), memorystore.Namespace{"files"})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := NewComposite(shell, map[string]Backend{"/common/": mounted, "/memories/": persistent})
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok := SandboxOf(composite)
	if !ok || resolved != shell || !CapabilitiesOf(composite).Execute {
		t.Fatalf("composite sandbox = %#v, %v", resolved, ok)
	}
	routes := ShellPathRoutes(composite)
	if len(routes) != 2 || routes[0].VirtualPrefix != "/memories/" || routes[0].Accessible || routes[1].VirtualPrefix != "/common/" || !routes[1].Accessible || routes[1].HostPrefix != mounted.localHostRoot() {
		t.Fatalf("shell routes = %#v", routes)
	}

	plain, _ := NewMemory(nil)
	withoutShell, err := NewComposite(plain, map[string]Backend{"/common/": mounted})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SandboxOf(withoutShell); ok || ShellPathRoutes(withoutShell)[0].Accessible {
		t.Fatalf("non-shell composite exposed execution: %#v", ShellPathRoutes(withoutShell))
	}
}

func TestCompositeArtifactsRootIsNormalized(t *testing.T) {
	memory, _ := NewMemory(nil)
	composite, err := NewCompositeWithOptions(CompositeOptions{Default: memory, ArtifactsRoot: "/workspace/"})
	if err != nil {
		t.Fatal(err)
	}
	if root := ArtifactsRootOf(composite); root != "/workspace" {
		t.Fatalf("artifacts root = %q", root)
	}
	if target := ArtifactPath(composite, "large_tool_results"); target != "/workspace/large_tool_results" {
		t.Fatalf("artifact path = %q", target)
	}
	plain, _ := NewComposite(memory, nil)
	if root := ArtifactsRootOf(plain); root != "/" || ArtifactPath(plain, "conversation_history") != "/conversation_history" {
		t.Fatalf("default artifacts root = %q", root)
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
	if err != nil || result.Output != "1234\n\n... Output truncated at 4 bytes." || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("Execute = %#v, %v", result, err)
	}
	result, err = shell.Execute(context.Background(), "printf failure; exit 7", time.Second)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 7 || result.Output != "fail\n\n... Output truncated at 4 bytes.\n\nExit code: 7" {
		t.Fatalf("failed Execute = %#v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := shell.Execute(ctx, "sleep 1", time.Second); err == nil {
		t.Fatal("canceled Execute succeeded")
	}
}

func TestLocalShellUsesCanonicalIdentityAndEmptyCommandResult(t *testing.T) {
	first, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ID()) != 14 || !strings.HasPrefix(first.ID(), "local-") || first.ID() == second.ID() {
		t.Fatalf("local shell ids = %q, %q", first.ID(), second.ID())
	}
	result, err := first.Execute(context.Background(), "", 0)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 1 || result.Output != "Error: Command must be a non-empty string." {
		t.Fatalf("empty command = %#v, %v", result, err)
	}
	if _, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: t.TempDir()}, DefaultTimeout: -time.Second}); err == nil {
		t.Fatal("negative default timeout succeeded")
	}
}

func TestLocalShellUsesExplicitEnvironmentAndLabelsStderr(t *testing.T) {
	t.Setenv("DAGO_SHELL_INHERITED", "parent")
	root := t.TempDir()
	shell, err := NewLocalShell(LocalShellOptions{
		Filesystem: FilesystemOptions{Root: root},
		Env:        map[string]string{"DAGO_SHELL_EXPLICIT": "child"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := shell.Execute(context.Background(), `printf '%s/%s' "$DAGO_SHELL_EXPLICIT" "${DAGO_SHELL_INHERITED-unset}"; printf 'problem\nsecond' >&2`, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "child/unset\n[stderr] problem\n[stderr] second" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("explicit environment Execute = %#v", result)
	}

	inherited, err := NewLocalShell(LocalShellOptions{
		Filesystem: FilesystemOptions{Root: root},
		Env:        map[string]string{"DAGO_SHELL_INHERITED": "override"},
		InheritEnv: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = inherited.Execute(context.Background(), `printf '%s/%s' "$DAGO_SHELL_INHERITED" "$PATH"`, time.Second)
	if err != nil || !strings.HasPrefix(result.Output, "override/") || result.Output == "override/" {
		t.Fatalf("inherited environment Execute = %#v, %v", result, err)
	}

	if _, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: root}, Env: map[string]string{"BAD=NAME": "value"}}); err == nil {
		t.Fatal("invalid environment variable name accepted")
	}
	if _, err := NewLocalShell(LocalShellOptions{Filesystem: FilesystemOptions{Root: root}, Env: map[string]string{"BAD": "value\x00"}}); err == nil {
		t.Fatal("NUL environment value accepted")
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
	if !strings.HasSuffix(result.Output, "... Output truncated at 1000 bytes.") || len(strings.SplitN(result.Output, "\n\n...", 2)[0]) != 1_000 || !result.Truncated || result.ExitCode == nil || *result.ExitCode != 0 {
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
