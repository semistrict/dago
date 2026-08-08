package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sahilm/fuzzy"
)

const (
	// findFilesDefaultLimit caps how many matches we return by default.
	findFilesDefaultLimit = 100
	// findFilesMaxLimit is the hard ceiling on the requested limit.
	findFilesMaxLimit = 500
	// findFilesMaxCandidates bounds how many files we hold in memory /
	// fuzzy-match against, so a giant non-git tree can't blow up the server.
	findFilesMaxCandidates = 50000
	// findFilesWalkDepth bounds filesystem walk recursion for non-git dirs.
	findFilesWalkDepth = 12
	// fileListCacheTTL keeps a directory's file list warm across the burst
	// of requests that arrive while the user types a query.
	fileListCacheTTL = 5 * time.Second
	// fileListCacheMaxDirs caps how many distinct directories the cache
	// retains, so varied dir values can't grow the map without bound.
	fileListCacheMaxDirs = 64
	// findFilesWalkBudget bounds the time spent listing a directory.
	findFilesWalkBudget = 3 * time.Second
)

// fileListCache memoizes the (relatively expensive) directory file listing so
// that the stream of queries produced while a user types only lists the tree
// once every fileListCacheTTL.
type fileListCache struct {
	mu      sync.Mutex
	entries map[string]fileListCacheEntry
}

type fileListCacheEntry struct {
	files     []string
	truncated bool
	computed  time.Time
}

func newFileListCache() *fileListCache {
	return &fileListCache{entries: make(map[string]fileListCacheEntry)}
}

// get returns the cached file list for dir, computing it via load when the
// entry is missing or stale. load reports ok=false when the listing failed or
// was cut short; such results are returned to this caller but NOT cached, so a
// transient failure can't poison the entry for the full TTL.
func (c *fileListCache) get(dir string, load func() (files []string, truncated, ok bool)) ([]string, bool) {
	c.mu.Lock()
	if e, ok := c.entries[dir]; ok && time.Since(e.computed) < fileListCacheTTL {
		c.mu.Unlock()
		return e.files, e.truncated
	}
	c.mu.Unlock()

	files, truncated, ok := load()
	if !ok {
		return files, truncated
	}

	c.mu.Lock()
	c.evictLocked()
	c.entries[dir] = fileListCacheEntry{files: files, truncated: truncated, computed: time.Now()}
	c.mu.Unlock()
	return files, truncated
}

// evictLocked drops stale entries and, if still at capacity, the oldest one.
// Callers must hold c.mu.
func (c *fileListCache) evictLocked() {
	for k, e := range c.entries {
		if time.Since(e.computed) >= fileListCacheTTL {
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= fileListCacheMaxDirs {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.computed.Before(oldest) {
				oldestKey, oldest = k, e.computed
			}
		}
		delete(c.entries, oldestKey)
	}
}

// FindFilesMatch is a single ranked file match.
type FindFilesMatch struct {
	// Path is the file path relative to the requested directory.
	Path string `json:"path"`
	// MatchedIndexes are rune (code-point) offsets into Path that matched the
	// query, used by the UI to highlight the fuzzy match.
	MatchedIndexes []int `json:"matched_indexes,omitempty"`
}

// FindFilesResponse is the response from /api/find-files.
type FindFilesResponse struct {
	Dir       string           `json:"dir"`
	Query     string           `json:"query"`
	Matches   []FindFilesMatch `json:"matches"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
}

// handleFindFiles fuzzy-searches files under a working directory. The query
// is matched server-side (via github.com/sahilm/fuzzy) so the client never
// needs the full file list. Files are enumerated with `git ls-files`
// (tracked + untracked, honoring .gitignore) when dir is inside a repo, else
// via a bounded filesystem walk.
func (s *Server) handleFindFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dir := r.URL.Query().Get("dir")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home
		} else {
			dir = "/"
		}
	}
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		http.Error(w, "absolute dir required", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := findFilesDefaultLimit
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if limit > findFilesMaxLimit {
		limit = findFilesMaxLimit
	}

	files, listTruncated := s.fileListCache.get(dir, func() (files []string, truncated, ok bool) {
		return listWorkingDirFiles(dir)
	})

	resp := FindFilesResponse{
		Dir:       dir,
		Query:     query,
		Total:     len(files),
		Truncated: listTruncated,
		Matches:   []FindFilesMatch{},
	}

	if query == "" {
		// No query: return the first `limit` files in alphabetical order so
		// the picker has something to show immediately when it opens.
		sorted := append([]string(nil), files...)
		sort.Strings(sorted)
		if len(sorted) > limit {
			sorted = sorted[:limit]
			resp.Truncated = true
		}
		for _, p := range sorted {
			resp.Matches = append(resp.Matches, FindFilesMatch{Path: p})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	matches := fuzzy.Find(query, files)
	if len(matches) > limit {
		matches = matches[:limit]
		resp.Truncated = true
	}
	for _, m := range matches {
		resp.Matches = append(resp.Matches, FindFilesMatch{
			Path:           m.Str,
			MatchedIndexes: byteToRuneOffsets(m.Str, m.MatchedIndexes),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// byteToRuneOffsets converts sahilm/fuzzy's byte offsets into s to rune
// (code-point) offsets, which is what the UI needs to highlight matches via
// JS string slicing. For pure-ASCII paths this is an identity mapping. Byte
// offsets that don't fall on a rune boundary (shouldn't happen for real
// matches) are dropped.
func byteToRuneOffsets(s string, byteIdx []int) []int {
	if len(byteIdx) == 0 {
		return byteIdx
	}
	// Fast path: for pure-ASCII strings byte offset == rune offset, which is
	// the overwhelmingly common case for file paths.
	if len(s) == utf8.RuneCountInString(s) {
		return byteIdx
	}
	// Map byte offset -> rune offset by walking the string once.
	byteToRune := make(map[int]int, len(s))
	ri := 0
	for b := range s {
		byteToRune[b] = ri
		ri++
	}
	out := make([]int, 0, len(byteIdx))
	for _, b := range byteIdx {
		if r, ok := byteToRune[b]; ok {
			out = append(out, r)
		}
	}
	return out
}

// listWorkingDirFiles returns file paths (relative to dir) under dir. It
// prefers `git ls-files` so .gitignore is honored and the crawl is fast;
// otherwise it falls back to a bounded filesystem walk. truncated reports
// whether the list hit findFilesMaxCandidates; ok is false when the listing
// failed outright (so the caller shouldn't cache it).
func listWorkingDirFiles(dir string) (files []string, truncated, ok bool) {
	if gitFiles, isRepo := gitLsFiles(dir); isRepo {
		if len(gitFiles) > findFilesMaxCandidates {
			return gitFiles[:findFilesMaxCandidates], true, true
		}
		return gitFiles, false, true
	}
	return walkFiles(dir)
}

// gitLsFiles lists tracked + untracked (non-ignored) files under dir using
// git. The isRepo result is false when dir is not inside a git repository (or
// git is unavailable), so the caller can fall back to a plain walk.
func gitLsFiles(dir string) (files []string, isRepo bool) {
	ctx, cancel := context.WithTimeout(context.Background(), findFilesWalkBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-co", "--exclude-standard", "-z")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	raw := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	files = make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, true
}

// walkFiles enumerates files under dir when it isn't a git repo. It skips the
// same heavy directories as the git-repo crawler and stops at a depth, count,
// and time budget so a huge tree can't hang the request. ok is always true:
// the handler already verified dir exists and is a directory, and unreadable
// subdirectories are silently skipped rather than failing the whole listing.
func walkFiles(dir string) (files []string, truncated, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), findFilesWalkBudget)
	defer cancel()

	var walk func(abs, rel string, depth int)
	walk = func(abs, rel string, depth int) {
		if truncated || ctx.Err() != nil {
			return
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			name := entry.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if entry.IsDir() {
				if _, skip := crawlSkipNames[name]; skip {
					continue
				}
				if depth >= findFilesWalkDepth {
					continue
				}
				walk(filepath.Join(abs, name), childRel, depth+1)
				continue
			}
			if !entry.Type().IsRegular() {
				continue
			}
			files = append(files, childRel)
			if len(files) >= findFilesMaxCandidates {
				truncated = true
				return
			}
		}
	}
	// A readable top-level dir was already verified by the handler (os.Stat),
	// so treat the walk as successful even if some subdirs are unreadable.
	walk(dir, "", 0)
	return files, truncated, true
}
