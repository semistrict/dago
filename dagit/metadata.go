// Package dagit reads lightweight Git repository metadata without requiring a
// Git subprocess for ordinary repository layouts.
package dagit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	gitDirPrefix  = "gitdir: "
	headRefPrefix = "ref: "
	gitTimeout    = 2 * time.Second
	maxGitOutput  = 1 << 20
)

// RepositoryMetadata is normalized attribution parsed from a Git remote URL.
type RepositoryMetadata struct {
	// URL is the credential-free https://host/path repository URL.
	URL string `json:"url"`
	// Provider is github, gitlab, bitbucket, or other.
	Provider string `json:"provider"`
	// Name is the repository path, such as owner/repository or a nested group path.
	Name string `json:"name"`
}

// Metadata is the current repository metadata resolved for a path.
type Metadata struct {
	Branch    string `json:"branch"`
	CommitSHA string `json:"commit_sha"`
	RemoteURL string `json:"remote_url"`
}

// Resolve reads current repository metadata using the filesystem first and a
// bounded Git subprocess only for fields whose on-disk representation cannot be
// interpreted confidently. Missing metadata is returned as an empty string.
func Resolve(ctx context.Context, path string) Metadata {
	return Metadata{
		Branch:    ResolveBranch(ctx, path),
		CommitSHA: ResolveCommitSHA(ctx, path),
		RemoteURL: ResolveRemoteURL(ctx, path),
	}
}

// FindDir locates the effective Git administration directory for path. Linked
// worktree .git pointers are resolved. The empty string means no repository was
// found.
func FindDir(path string) string {
	_, gitDir, _, ok := discover(path)
	if !ok {
		return ""
	}
	return gitDir
}

// FindRoot locates the worktree root containing path. The empty string means no
// repository was found.
func FindRoot(path string) string {
	root, _, _, ok := discover(path)
	if !ok {
		return ""
	}
	return root
}

// FindCommonDir returns a canonical, validated identity shared by a repository's
// linked worktrees. Path must name the exact worktree root or an already-known
// common directory; nested paths intentionally do not inherit an identity.
//
// A linked worktree is accepted only when its administration directory is a
// direct child of the common directory's worktrees directory and its gitdir
// backlink points lexically to the worktree's own .git file. These reciprocal
// checks prevent a forged worktree pointer from borrowing another repository's
// identity.
func FindCommonDir(path string) string {
	root, err := normalizeExistingDir(path)
	if err != nil {
		return ""
	}
	if validCommonDir(root) {
		return root
	}

	gitEntry := filepath.Join(root, ".git")
	entryInfo, err := os.Lstat(gitEntry)
	if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	if entryInfo.IsDir() {
		commonDir, err := filepath.EvalSymlinks(gitEntry)
		if err != nil || commonDir != gitEntry || !validCommonDir(commonDir) {
			return ""
		}
		return commonDir
	}
	if !entryInfo.Mode().IsRegular() {
		return ""
	}

	value, ok := readSingleLine(gitEntry, true)
	if !ok || !strings.HasPrefix(value, gitDirPrefix) {
		return ""
	}
	pointer := strings.TrimSpace(strings.TrimPrefix(value, gitDirPrefix))
	if pointer == "" {
		return ""
	}
	gitDir, err := resolveStoredPath(pointer, root, true)
	if err != nil || !isDir(gitDir) {
		return ""
	}

	commonValue, ok := readSingleLine(filepath.Join(gitDir, "commondir"), true)
	if !ok {
		return ""
	}
	commonDir, err := resolveStoredPath(commonValue, gitDir, true)
	if err != nil || filepath.Dir(gitDir) != filepath.Join(commonDir, "worktrees") {
		return ""
	}
	if !validCommonDir(commonDir) || !isRegular(filepath.Join(gitDir, "HEAD")) {
		return ""
	}

	backlink, ok := readSingleLine(filepath.Join(gitDir, "gitdir"), true)
	if !ok {
		return ""
	}
	backlinkPath, err := resolveStoredPath(backlink, gitDir, false)
	if err != nil || backlinkPath != gitEntry {
		return ""
	}
	return commonDir
}

// ResolveBranch returns the current branch, HEAD for a detached checkout, or an
// empty string when it cannot be determined.
func ResolveBranch(ctx context.Context, path string) string {
	value, definitive := readBranch(path)
	if definitive {
		return value
	}
	return runGit(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
}

// ResolveCommitSHA returns the full 40-character SHA-1 or 64-character SHA-256
// object ID for HEAD, or an empty string when it cannot be determined.
func ResolveCommitSHA(ctx context.Context, path string) string {
	value, definitive := readCommitSHA(path)
	if definitive {
		return value
	}
	value = runGit(ctx, path, "rev-parse", "HEAD")
	if !validObjectID(value) {
		return ""
	}
	return value
}

// ResolveRemoteURL returns remote.origin.url, or an empty string when it cannot
// be determined.
func ResolveRemoteURL(ctx context.Context, path string) string {
	value, definitive := readRemoteURL(path)
	if definitive {
		return value
	}
	return runGit(ctx, path, "config", "--get", "remote.origin.url")
}

// ParseRepositoryMetadata parses HTTPS, URL-style SSH, and scp-style SSH Git
// remotes into credential-free repository attribution.
func ParseRepositoryMetadata(remoteURL string) (RepositoryMetadata, bool) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return RepositoryMetadata{}, false
	}

	var host, repoPath string
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return RepositoryMetadata{}, false
		}
		host = strings.ToLower(parsed.Hostname())
		repoPath = strings.TrimPrefix(parsed.Path, "/")
	} else {
		userHost, path, ok := strings.Cut(raw, ":")
		if !ok {
			return RepositoryMetadata{}, false
		}
		host = strings.ToLower(userHost)
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		repoPath = strings.TrimPrefix(path, "/")
	}

	repoPath = strings.Trim(strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git"), "/")
	if host == "" || repoPath == "" || strings.ContainsAny(host, "/\\") {
		return RepositoryMetadata{}, false
	}
	provider := map[string]string{
		"github.com":    "github",
		"gitlab.com":    "gitlab",
		"bitbucket.org": "bitbucket",
	}[host]
	if provider == "" {
		provider = "other"
	}
	return RepositoryMetadata{
		URL:      "https://" + host + "/" + repoPath,
		Provider: provider,
		Name:     repoPath,
	}, true
}

func readBranch(path string) (string, bool) {
	_, gitDir, _, ok := discover(path)
	if !ok {
		return "", true
	}
	head, ok := readSingleLine(filepath.Join(gitDir, "HEAD"), false)
	if !ok {
		return "", false
	}
	if head == "" {
		return "", true
	}
	if !strings.HasPrefix(head, headRefPrefix) {
		return "HEAD", true
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, headRefPrefix))
	if !validRef(ref) {
		return "", false
	}
	for _, prefix := range []string{"refs/heads/", "refs/remotes/", "refs/tags/", "refs/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix), true
		}
	}
	return ref, true
}

func readCommitSHA(path string) (string, bool) {
	_, gitDir, commonDir, ok := discover(path)
	if !ok {
		return "", true
	}
	head, ok := readSingleLine(filepath.Join(gitDir, "HEAD"), false)
	if !ok || head == "" {
		return "", false
	}
	if !strings.HasPrefix(head, headRefPrefix) {
		if validObjectID(head) {
			return head, true
		}
		return "", false
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, headRefPrefix))
	if !validRef(ref) {
		return "", false
	}
	for _, dir := range uniqueStrings(gitDir, commonDir) {
		if value, ok := readSingleLine(filepath.Join(dir, filepath.FromSlash(ref)), false); ok && validObjectID(value) {
			return value, true
		}
	}
	for _, dir := range uniqueStrings(gitDir, commonDir) {
		if value := readPackedRef(filepath.Join(dir, "packed-refs"), ref); value != "" {
			return value, true
		}
	}
	return "", false
}

func readRemoteURL(path string) (string, bool) {
	_, gitDir, commonDir, ok := discover(path)
	if !ok {
		return "", true
	}
	for _, dir := range uniqueStrings(gitDir, commonDir) {
		raw, err := readSmallFile(filepath.Join(dir, "config"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false
		}
		inOrigin := false
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				inOrigin = strings.EqualFold(strings.ReplaceAll(trimmed, " ", ""), `[remote"origin"]`)
				continue
			}
			if inOrigin {
				key, value, found := strings.Cut(trimmed, "=")
				if found && strings.EqualFold(strings.TrimSpace(key), "url") {
					value = strings.TrimSpace(value)
					if value != "" {
						return value, true
					}
				}
			}
		}
	}
	return "", false
}

func discover(path string) (root, gitDir, commonDir string, ok bool) {
	current, err := normalizeLookup(path)
	if err != nil {
		return "", "", "", false
	}
	if info, statErr := os.Stat(current); statErr == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		gitEntry := filepath.Join(current, ".git")
		info, err := os.Stat(gitEntry)
		if err == nil {
			switch {
			case info.IsDir():
				gitDir = gitEntry
			case info.Mode().IsRegular():
				value, valid := readSingleLine(gitEntry, false)
				if !valid || !strings.HasPrefix(value, gitDirPrefix) {
					return "", "", "", false
				}
				pointer := strings.TrimSpace(strings.TrimPrefix(value, gitDirPrefix))
				if pointer == "" {
					return "", "", "", false
				}
				gitDir, err = resolveStoredPath(pointer, current, true)
				if err != nil || !isDir(gitDir) {
					return "", "", "", false
				}
			default:
				return "", "", "", false
			}
			commonDir = gitDir
			if value, valid := readSingleLine(filepath.Join(gitDir, "commondir"), false); valid {
				if resolved, resolveErr := resolveStoredPath(value, gitDir, true); resolveErr == nil && isDir(resolved) {
					commonDir = resolved
				}
			}
			return current, gitDir, commonDir, true
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", "", false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", "", "", false
		}
		current = parent
	}
}

func readPackedRef(path, ref string) string {
	raw, err := readSmallFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, found := strings.Cut(line, " ")
		if found && strings.TrimSpace(name) == ref && validObjectID(sha) {
			return sha
		}
	}
	return ""
}

func runGit(ctx context.Context, path string, args ...string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	dir, err := normalizeLookup(path)
	if err != nil {
		return ""
	}
	if info, statErr := os.Stat(dir); statErr == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = safeGitEnvironment(os.Environ())
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var output boundedBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return ""
	}
	value := strings.TrimSpace(output.String())
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return ""
	}
	return value
}

func safeGitEnvironment(env []string) []string {
	blocked := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true,
		"GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	}
	filtered := make([]string, 0, len(env)+2)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if !blocked[key] && key != "GIT_OPTIONAL_LOCKS" && key != "GIT_TERMINAL_PROMPT" {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}

type boundedBuffer struct {
	buf bytes.Buffer
	n   int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := maxGitOutput - b.n
	if remaining <= 0 || len(p) > remaining {
		return 0, errors.New("git output exceeds limit")
	}
	n, err := b.buf.Write(p)
	b.n += n
	return n, err
}

func (b *boundedBuffer) String() string { return b.buf.String() }

func normalizeLookup(path string) (string, error) {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		return filepath.Clean(resolved), nil
	}
	return filepath.Clean(abs), nil
}

func normalizeExistingDir(path string) (string, error) {
	normalized, err := normalizeLookup(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(normalized)
	if err != nil || !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return normalized, nil
}

func resolveStoredPath(value, relativeTo string, followLinks bool) (string, error) {
	candidate := filepath.FromSlash(value)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(relativeTo, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if !followLinks {
		return abs, nil
	}
	return filepath.EvalSymlinks(abs)
}

func validCommonDir(path string) bool {
	return isDir(path) && isRegular(filepath.Join(path, "HEAD")) &&
		isRegular(filepath.Join(path, "config")) && isDir(filepath.Join(path, "objects")) &&
		isDir(filepath.Join(path, "refs"))
}

func validRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.ContainsAny(ref, "\x00\r\n\\") {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(ref)))
	return cleaned == ref && !strings.Contains(ref, "/../") && !strings.HasSuffix(ref, "/..")
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func readSingleLine(path string, rejectSymlink bool) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if rejectSymlink {
			return "", false
		}
		info, err = os.Stat(path)
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGitOutput {
		return "", false
	}
	raw, err := readSmallFile(path)
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		return "", false
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		return "", false
	}
	return strings.TrimSpace(lines[0]), true
}

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxGitOutput {
		return nil, errors.New("git metadata exceeds limit")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxGitOutput+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxGitOutput {
		return nil, errors.New("git metadata exceeds limit")
	}
	return content, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func uniqueStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
