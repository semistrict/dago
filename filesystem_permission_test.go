package dago

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/damessage"
	memorystore "github.com/semistrict/dago/dastore"
)

func TestPermissionGlobSemantics(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/**", "/", true},
		{"/**", "/a/b", true},
		{"/vault/**", "/vault/a/b", true},
		{"/work/*.log", "/work/a.log", true},
		{"/work/*.log", "/work/nested/a.log", false},
		{"/work/**/secret", "/work/secret", true},
		{"/work/**/secret", "/work/a/b/secret", true},
		{"/work/{keys,secrets}", "/work/secrets", true},
		{"/work/[ab].txt", "/work/c.txt", false},
		{"/data/a?", "/data/ab", true},
	}
	for _, test := range tests {
		if got := globMatch(test.pattern, test.path); got != test.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", test.pattern, test.path, got, test.want)
		}
	}
}

func TestRecursiveDeletePatternOverlap(t *testing.T) {
	tests := []struct {
		pattern string
		target  string
		want    bool
	}{
		{"/other/**", "/work", false},
		{"/workshop/**", "/work", false},
		{"/work/*.log", "/work/notes.txt", false},
		{"/work/*", "/work/app/child", true},
		{"/work/*.log", "/work/app.log/child", true},
		{"/work/*/secrets", "/work/app", true},
		{"/work/**/*.log", "/work/sub", true},
		{"/work/secrets/**", "/work", true},
		{"/work/**", "/work/logs", true},
		{"/work", "/work/sub/deep", true},
		{"/**/secrets", "/work", true},
	}
	for _, test := range tests {
		if got := deletePatternOverlaps(test.pattern, test.target); got != test.want {
			t.Errorf("deletePatternOverlaps(%q, %q) = %v, want %v", test.pattern, test.target, got, test.want)
		}
	}
}

func TestDeletePermissionsDistinguishLeafAndSubtree(t *testing.T) {
	rules := []FilesystemPermission{
		{Operations: []FilesystemOperation{FilesystemWrite}, Paths: []string{"/work/**"}, Mode: PermissionAllow},
		{Operations: []FilesystemOperation{FilesystemWrite}, Paths: []string{"/**"}, Mode: PermissionDeny},
	}
	if got := findDeletePatterns(rules, "/work/a.txt", false, PermissionDeny); len(got) != 0 {
		t.Fatalf("leaf denied by later rule: %v", got)
	}
	if got := findDeletePatterns(rules, "/work", true, PermissionDeny); !reflect.DeepEqual(got, []string{"/**"}) {
		t.Fatalf("subtree deny patterns = %v", got)
	}

	memory, err := dabackend.NewMemory(map[string]dabackend.FileData{
		"/work/a.txt":             {Content: "a", Encoding: dabackend.EncodingUTF8},
		"/work/secrets/token.txt": {Content: "secret", Encoding: dabackend.EncodingUTF8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleteTargetMayHaveDescendants(context.Background(), memory, "/work/a.txt", true) {
		t.Fatal("plain file reported descendants")
	}
	if !deleteTargetMayHaveDescendants(context.Background(), memory, "/work", true) {
		t.Fatal("directory reported as leaf")
	}
}

func TestFilesystemFiltersDeniedBulkResults(t *testing.T) {
	rules := []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/secret/**"}, Mode: PermissionDeny}}
	values := []dabackend.FileInfo{{Path: "/public/a.txt"}, {Path: "/secret/b.txt"}}
	filtered := filterFileInfo(rules, FilesystemRead, values)
	if len(filtered) != 1 || filtered[0].Path != "/public/a.txt" {
		t.Fatalf("filtered files = %#v", filtered)
	}
	grep := filterGrepMatches(rules, FilesystemRead, []dabackend.GrepMatch{{Path: "/public/a.txt"}, {Path: "/secret/b.txt"}})
	if len(grep) != 1 || grep[0].Path != "/public/a.txt" {
		t.Fatalf("filtered grep = %#v", grep)
	}
}

func TestFilesystemPermissionsRejectExecuteCapability(t *testing.T) {
	shell, err := dabackend.NewLocalShell(dabackend.LocalShellOptions{Filesystem: dabackend.FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	requirePanicContaining(t, "cannot constrain execute", func() {
		mustFilesystem(shell, Filesystem{
			Permissions: []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/**"}, Mode: PermissionAllow}},
		})
	})
}

func TestFilesystemPermissionsAllowOnlyShellInaccessibleRoutes(t *testing.T) {
	shell, err := dabackend.NewLocalShell(dabackend.LocalShellOptions{Filesystem: dabackend.FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := dabackend.NewStore(memorystore.NewMemory(), memorystore.Namespace{"files"})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := dabackend.NewComposite(shell, map[string]dabackend.Backend{"/memories/": persistent})
	if err != nil {
		t.Fatal(err)
	}
	rules := []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/memories/**"}, Mode: PermissionDeny}}
	middleware := mustFilesystem(composite, Filesystem{Permissions: rules})

	foundExecute := false
	for _, executable := range middleware.Tools {
		foundExecute = foundExecute || executable.Definition().Name == "execute"
	}
	if !foundExecute {
		t.Fatal("route-scoped permissions hid execute")
	}

	localRoute, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	accessible, err := dabackend.NewComposite(shell, map[string]dabackend.Backend{"/work/": localRoute})
	if err != nil {
		t.Fatal(err)
	}
	requirePanicContaining(t, "cannot constrain execute", func() {
		mustFilesystem(accessible, Filesystem{Permissions: []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/work/**"}, Mode: PermissionDeny}}})
	})
}

func TestFilesystemBulkInterruptCannotBeBypassed(t *testing.T) {
	rules := []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/secrets/**"}, Mode: PermissionInterrupt}}
	tests := []struct {
		name string
		tool string
		args string
		want bool
	}{
		{"pathless grep", "grep", `{"pattern":"key"}`, true},
		{"root alias", "ls", `{"path":"."}`, true},
		{"protected subtree", "grep", `{"pattern":"key","path":"/secrets"}`, true},
		{"unrelated subtree", "grep", `{"pattern":"key","path":"/workspace"}`, false},
		{"prefix lookalike", "ls", `{"path":"/secret"}`, false},
		{"invalid traversal", "ls", `{"path":"/secrets/../etc"}`, false},
		{"absolute protected glob", "glob", `{"pattern":"/secrets/**/*.pem","path":"/workspace"}`, true},
		{"root anchored glob", "glob", `{"pattern":"/**/key.pem","path":"/workspace"}`, true},
		{"absolute public glob", "glob", `{"pattern":"/workspace/**","path":"/workspace"}`, false},
		{"relative traversal glob", "glob", `{"pattern":"../secrets/*","path":"/workspace"}`, true},
		{"relative public glob", "glob", `{"pattern":"*.txt","path":"/workspace"}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := damessage.ToolCall{ID: test.name, Name: test.tool, Arguments: json.RawMessage(test.args)}
			if got := filesystemBulkInterrupt(rules, FilesystemRead, call); got != test.want {
				t.Fatalf("filesystemBulkInterrupt(%s, %s) = %v, want %v", test.tool, test.args, got, test.want)
			}
		})
	}
}

func FuzzPermissionGlobNeverPanics(f *testing.F) {
	for _, seed := range [][2]string{{"/**", "/a"}, {"/work/[ab]", "/work/a"}, {"/{a,b}/**", "/a/x"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pattern, value string) {
		_ = globMatch(pattern, value)
		_ = deletePatternOverlaps(pattern, value)
	})
}
