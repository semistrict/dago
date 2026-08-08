package dago

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/semistrict/dago/backend"
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

	memory, err := backend.NewMemory(map[string]backend.FileData{
		"/work/a.txt":             {Content: "a", Encoding: backend.EncodingUTF8},
		"/work/secrets/token.txt": {Content: "secret", Encoding: backend.EncodingUTF8},
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
	values := []backend.FileInfo{{Path: "/public/a.txt"}, {Path: "/secret/b.txt"}}
	filtered := filterFileInfo(rules, FilesystemRead, values)
	if len(filtered) != 1 || filtered[0].Path != "/public/a.txt" {
		t.Fatalf("filtered files = %#v", filtered)
	}
	grep := filterGrepMatches(rules, FilesystemRead, []backend.GrepMatch{{Path: "/public/a.txt"}, {Path: "/secret/b.txt"}})
	if len(grep) != 1 || grep[0].Path != "/public/a.txt" {
		t.Fatalf("filtered grep = %#v", grep)
	}
}

func TestFilesystemPermissionsRejectExecuteCapability(t *testing.T) {
	shell, err := backend.NewLocalShell(backend.LocalShellOptions{Filesystem: backend.FilesystemOptions{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = FilesystemMiddleware(FilesystemOptions{
		Backend:     shell,
		Permissions: []FilesystemPermission{{Operations: []FilesystemOperation{FilesystemRead}, Paths: []string{"/**"}, Mode: PermissionAllow}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot constrain execute") {
		t.Fatalf("error = %v", err)
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
