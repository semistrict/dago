package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// findFiles issues a GET /api/find-files and decodes the response.
func findFiles(t *testing.T, h *TestHarness, dir, query string) FindFilesResponse {
	t.Helper()
	u := "/api/find-files?dir=" + dir
	if query != "" {
		u += "&q=" + query
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	h.server.handleFindFiles(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("find-files %q %q: expected 200, got %d: %s", dir, query, w.Code, w.Body.String())
	}
	var resp FindFilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func hasPath(matches []FindFilesMatch, p string) bool {
	for _, m := range matches {
		if m.Path == p {
			return true
		}
	}
	return false
}

// TestFindFilesGitRepo verifies fuzzy file finding inside a git repo honors
// .gitignore, ranks by the query, and returns highlight indexes.
func TestFindFilesGitRepo(t *testing.T) {
	t.Parallel()
	h := NewTestHarness(t)

	dir := t.TempDir()
	mustGitInit(t, dir)
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# hi\n")
	writeFile(t, filepath.Join(dir, "internal", "server", "handler.go"), "package server\n")
	writeFile(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(dir, "ignored.txt"), "secret\n")

	t.Run("no_query_lists_files", func(t *testing.T) {
		resp := findFiles(t, h, dir, "")
		if !hasPath(resp.Matches, "main.go") {
			t.Errorf("expected main.go in %+v", resp.Matches)
		}
		if hasPath(resp.Matches, "ignored.txt") {
			t.Errorf("gitignored file should not appear: %+v", resp.Matches)
		}
	})

	t.Run("fuzzy_query", func(t *testing.T) {
		resp := findFiles(t, h, dir, "handler")
		if len(resp.Matches) == 0 || resp.Matches[0].Path != "internal/server/handler.go" {
			t.Errorf("expected handler.go ranked first, got %+v", resp.Matches)
		}
		if len(resp.Matches[0].MatchedIndexes) == 0 {
			t.Errorf("expected match highlight indexes, got none")
		}
	})

	t.Run("subsequence_match", func(t *testing.T) {
		resp := findFiles(t, h, dir, "mgo")
		if !hasPath(resp.Matches, "main.go") {
			t.Errorf("expected fuzzy subsequence match main.go, got %+v", resp.Matches)
		}
	})
}

// TestFindFilesMultibyteHighlight verifies match indexes are rune (not byte)
// offsets so the UI highlights the right characters in non-ASCII paths.
func TestFindFilesMultibyteHighlight(t *testing.T) {
	t.Parallel()
	h := NewTestHarness(t)

	dir := t.TempDir()
	mustGitInit(t, dir)
	// "café" is 5 bytes (é = 2 bytes) but 4 runes, so a byte offset for the
	// "m" in main.go (byte 6) differs from its rune offset (5).
	writeFile(t, filepath.Join(dir, "café", "main.go"), "package main\n")

	resp := findFiles(t, h, dir, "main")
	if len(resp.Matches) == 0 {
		t.Fatalf("expected a match, got none")
	}
	m := resp.Matches[0]
	if m.Path != "café/main.go" {
		t.Fatalf("unexpected path %q", m.Path)
	}
	runes := []rune(m.Path)
	for _, idx := range m.MatchedIndexes {
		if idx < 0 || idx >= len(runes) {
			t.Fatalf("match index %d out of rune range [0,%d)", idx, len(runes))
		}
	}
	if len(m.MatchedIndexes) == 0 || runes[m.MatchedIndexes[0]] != 'm' {
		t.Errorf("expected first match index to point at 'm', got indexes %v in %q", m.MatchedIndexes, m.Path)
	}
}

// TestFindFilesCacheNoPoisonOnFailure verifies a failed listing isn't cached,
// so a subsequent successful request still sees the files.
func TestFindFilesCacheNoPoisonOnFailure(t *testing.T) {
	t.Parallel()
	c := newFileListCache()

	files, _ := c.get("/some/dir", func() ([]string, bool, bool) {
		return nil, false, false // ok=false: must not be cached
	})
	if len(files) != 0 {
		t.Fatalf("expected empty result from failed load, got %v", files)
	}
	if _, ok := c.entries["/some/dir"]; ok {
		t.Fatalf("failed load should not be cached")
	}

	files, _ = c.get("/some/dir", func() ([]string, bool, bool) {
		return []string{"a.go", "b.go"}, false, true
	})
	if len(files) != 2 {
		t.Fatalf("expected 2 files after successful load, got %v", files)
	}
	if _, ok := c.entries["/some/dir"]; !ok {
		t.Fatalf("successful load should be cached")
	}
}

// TestFileListCacheEviction verifies the cache stays under its dir cap.
func TestFileListCacheEviction(t *testing.T) {
	t.Parallel()
	c := newFileListCache()
	for i := 0; i < fileListCacheMaxDirs*2; i++ {
		dir := fmt.Sprintf("/dir/%d", i)
		c.get(dir, func() ([]string, bool, bool) { return []string{"x"}, false, true })
	}
	if len(c.entries) > fileListCacheMaxDirs {
		t.Errorf("cache exceeded cap: %d > %d", len(c.entries), fileListCacheMaxDirs)
	}
}

// TestFindFilesNonGit verifies the filesystem-walk fallback works outside a
// git repo and skips heavy directories.
func TestFindFilesNonGit(t *testing.T) {
	t.Parallel()
	h := NewTestHarness(t)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "alpha.txt"), "a\n")
	writeFile(t, filepath.Join(dir, "sub", "beta.txt"), "b\n")
	writeFile(t, filepath.Join(dir, "node_modules", "junk.js"), "junk\n")

	resp := findFiles(t, h, dir, "")
	if !hasPath(resp.Matches, "alpha.txt") || !hasPath(resp.Matches, "sub/beta.txt") {
		t.Errorf("expected alpha.txt and sub/beta.txt, got %+v", resp.Matches)
	}
	if hasPath(resp.Matches, "node_modules/junk.js") {
		t.Errorf("node_modules should be skipped: %+v", resp.Matches)
	}
}

// TestFindFilesBadRequests verifies input validation.
func TestFindFilesBadRequests(t *testing.T) {
	t.Parallel()
	h := NewTestHarness(t)

	t.Run("relative_dir", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/find-files?dir=relative/path", nil)
		w := httptest.NewRecorder()
		h.server.handleFindFiles(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("missing_dir", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/find-files?dir=/nonexistent/xyz/123", nil)
		w := httptest.NewRecorder()
		h.server.handleFindFiles(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("method_not_allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/find-files?dir=/tmp", nil)
		w := httptest.NewRecorder()
		h.server.handleFindFiles(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})
}

// TestHandleReadFile verifies the arbitrary-text-file read endpoint.
func TestHandleReadFile(t *testing.T) {
	t.Parallel()
	h := NewTestHarness(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	writeFile(t, path, "hello world\n")

	t.Run("reads_content", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/read-file?path="+path, nil)
		w := httptest.NewRecorder()
		h.server.handleReadFile(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Content != "hello world\n" {
			t.Errorf("content = %q", resp.Content)
		}
		if resp.Path != path {
			t.Errorf("path = %q, want %q", resp.Path, path)
		}
	})

	t.Run("relative_rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/read-file?path=relative.txt", nil)
		w := httptest.NewRecorder()
		h.server.handleReadFile(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})

	t.Run("missing_file", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/read-file?path="+filepath.Join(dir, "nope.txt"), nil)
		w := httptest.NewRecorder()
		h.server.handleReadFile(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("directory_rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/read-file?path="+dir, nil)
		w := httptest.NewRecorder()
		h.server.handleReadFile(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", w.Code)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}
