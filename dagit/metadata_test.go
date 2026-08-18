package dagit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testSHA = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"

func TestResolveReadsLooseRepositoryMetadata(t *testing.T) {
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".git")
	mustMkdirAll(t, filepath.Join(gitDir, "refs", "heads"))
	mustWrite(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/feature\n")
	mustWrite(t, filepath.Join(gitDir, "refs", "heads", "feature"), testSHA+"\n")
	mustWrite(t, filepath.Join(gitDir, "config"), `[remote "origin"]
	url = git@github.com:langchain-ai/deepagents.git
`)

	nested := filepath.Join(repo, "src", "nested")
	mustMkdirAll(t, nested)
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	gitDir = filepath.Join(repo, ".git")
	got := Resolve(context.Background(), nested)
	if got.Branch != "feature" || got.CommitSHA != testSHA || got.RemoteURL != "git@github.com:langchain-ai/deepagents.git" {
		t.Fatalf("Resolve() = %#v", got)
	}
	if FindRoot(nested) != repo {
		t.Fatalf("FindRoot() = %q, want %q", FindRoot(nested), repo)
	}
	if FindDir(nested) != gitDir {
		t.Fatalf("FindDir() = %q, want %q", FindDir(nested), gitDir)
	}
}

func TestResolveReadsDetachedAndPackedRefs(t *testing.T) {
	t.Run("detached", func(t *testing.T) {
		repo := t.TempDir()
		mustMkdirAll(t, filepath.Join(repo, ".git"))
		mustWrite(t, filepath.Join(repo, ".git", "HEAD"), testSHA+"\n")
		if got := ResolveBranch(context.Background(), repo); got != "HEAD" {
			t.Fatalf("ResolveBranch() = %q, want HEAD", got)
		}
		if got := ResolveCommitSHA(context.Background(), repo); got != testSHA {
			t.Fatalf("ResolveCommitSHA() = %q, want %q", got, testSHA)
		}
	})

	t.Run("packed", func(t *testing.T) {
		repo := t.TempDir()
		mustMkdirAll(t, filepath.Join(repo, ".git"))
		mustWrite(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/tags/v1.0\n")
		mustWrite(t, filepath.Join(repo, ".git", "packed-refs"), "# pack-refs\n1111111111111111111111111111111111111111 refs/tags/other\n^0000000000000000000000000000000000000000\n"+testSHA+" refs/tags/v1.0\n")
		if got := ResolveBranch(context.Background(), repo); got != "v1.0" {
			t.Fatalf("ResolveBranch() = %q, want v1.0", got)
		}
		if got := ResolveCommitSHA(context.Background(), repo); got != testSHA {
			t.Fatalf("ResolveCommitSHA() = %q, want %q", got, testSHA)
		}
	})
}

func TestLinkedWorktreeReadsCommonMetadataAndValidatesIdentity(t *testing.T) {
	requireGit(t)
	parent := t.TempDir()
	main := filepath.Join(parent, "main")
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	initRepository(t, main)
	runGitTest(t, main, "remote", "add", "origin", "https://github.com/example/project.git")
	runGitTest(t, main, "worktree", "add", "-b", "first", first, "HEAD")
	runGitTest(t, main, "worktree", "add", "-b", "second", second, "HEAD")

	wantCommon, err := filepath.EvalSymlinks(filepath.Join(main, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{main, first, second} {
		if got := FindCommonDir(root); got != wantCommon {
			t.Errorf("FindCommonDir(%q) = %q, want %q", root, got, wantCommon)
		}
		if got := ResolveCommitSHA(context.Background(), root); got == "" {
			t.Errorf("ResolveCommitSHA(%q) is empty", root)
		}
		if got := ResolveRemoteURL(context.Background(), root); got != "https://github.com/example/project.git" {
			t.Errorf("ResolveRemoteURL(%q) = %q", root, got)
		}
	}
	if got := ResolveBranch(context.Background(), first); got != "first" {
		t.Fatalf("ResolveBranch(first) = %q, want first", got)
	}
	if got := FindCommonDir(filepath.Join(first, "tracked.txt")); got != "" {
		t.Fatalf("nested FindCommonDir() = %q, want empty", got)
	}
}

func TestFindCommonDirRejectsForgedPointers(t *testing.T) {
	requireGit(t)
	parent := t.TempDir()
	main := filepath.Join(parent, "main")
	genuine := filepath.Join(parent, "genuine")
	initRepository(t, main)
	runGitTest(t, main, "worktree", "add", "-b", "genuine", genuine, "HEAD")
	wantCommon := filepath.Join(main, ".git")
	admin := strings.TrimSpace(strings.TrimPrefix(readFile(t, filepath.Join(genuine, ".git")), "gitdir:"))

	t.Run("pointer borrows genuine worktree", func(t *testing.T) {
		forged := filepath.Join(parent, "forged-borrow")
		mustMkdirAll(t, forged)
		mustWrite(t, filepath.Join(forged, ".git"), "gitdir: "+admin+"\n")
		if got := FindCommonDir(forged); got != "" {
			t.Fatalf("FindCommonDir() = %q, want rejection", got)
		}
	})

	t.Run("self consistent admin outside worktrees", func(t *testing.T) {
		forged := filepath.Join(parent, "forged-outside")
		forgedAdmin := filepath.Join(parent, "attacker", "admin")
		mustMkdirAll(t, forged)
		mustMkdirAll(t, forgedAdmin)
		mustWrite(t, filepath.Join(forged, ".git"), "gitdir: "+forgedAdmin+"\n")
		mustWrite(t, filepath.Join(forgedAdmin, "commondir"), wantCommon+"\n")
		mustWrite(t, filepath.Join(forgedAdmin, "HEAD"), "ref: refs/heads/forged\n")
		mustWrite(t, filepath.Join(forgedAdmin, "gitdir"), filepath.Join(forged, ".git")+"\n")
		if got := FindCommonDir(forged); got != "" {
			t.Fatalf("FindCommonDir() = %q, want rejection", got)
		}
	})

	t.Run("missing reciprocal backlink", func(t *testing.T) {
		backlink := filepath.Join(admin, "gitdir")
		original := readFile(t, backlink)
		if err := os.Remove(backlink); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.WriteFile(backlink, []byte(original), 0o600) })
		if got := FindCommonDir(genuine); got != "" {
			t.Fatalf("FindCommonDir() = %q, want rejection", got)
		}
	})
}

func TestFindCommonDirRejectsSymlinkedGitEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation generally needs extra privileges on Windows")
	}
	repo := t.TempDir()
	common := filepath.Join(repo, "common")
	forged := filepath.Join(repo, "forged")
	makeValidCommonDir(t, common)
	mustMkdirAll(t, forged)
	if err := os.Symlink(common, filepath.Join(forged, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := FindCommonDir(forged); got != "" {
		t.Fatalf("FindCommonDir() = %q, want rejection", got)
	}
}

func TestResolveFallsBackToGitForIncludedConfig(t *testing.T) {
	requireGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	initRepository(t, repo)
	gitDir := filepath.Join(repo, ".git")
	mustWrite(t, filepath.Join(gitDir, "remote-config"), "[remote \"origin\"]\n\turl = ssh://git@github.com/example/project.git\n")
	config := readFile(t, filepath.Join(gitDir, "config"))
	mustWrite(t, filepath.Join(gitDir, "config"), config+"\n[include]\n\tpath = remote-config\n")

	want := "ssh://git@github.com/example/project.git"
	if got := ResolveRemoteURL(context.Background(), repo); got != want {
		t.Fatalf("ResolveRemoteURL() = %q, want fallback result %q", got, want)
	}
}

func TestResolveSubprocessHonorsCancellationAndIgnoresRepositoryEnvironment(t *testing.T) {
	requireGit(t)
	repo := filepath.Join(t.TempDir(), "repo")
	initRepository(t, repo)
	// A missing origin forces the subprocess path. A canceled context must not
	// escape to an unbounded process.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := ResolveRemoteURL(ctx, repo); got != "" {
		t.Fatalf("ResolveRemoteURL(canceled) = %q, want empty", got)
	}

	other := filepath.Join(t.TempDir(), "other")
	initRepository(t, other)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	wantRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(context.Background(), repo, "rev-parse", "--show-toplevel"); got != wantRepo {
		t.Fatalf("runGit redirected by GIT_DIR: got %q, want %q", got, wantRepo)
	}
}

func TestParseRepositoryMetadata(t *testing.T) {
	tests := []struct {
		name     string
		remote   string
		want     RepositoryMetadata
		wantOkay bool
	}{
		{"https", "https://github.com/langchain-ai/deepagents.git", RepositoryMetadata{"https://github.com/langchain-ai/deepagents", "github", "langchain-ai/deepagents"}, true},
		{"scp", "git@github.com:langchain-ai/deepagents.git", RepositoryMetadata{"https://github.com/langchain-ai/deepagents", "github", "langchain-ai/deepagents"}, true},
		{"ssh with port", "ssh://git@github.com:22/langchain-ai/deepagents.git", RepositoryMetadata{"https://github.com/langchain-ai/deepagents", "github", "langchain-ai/deepagents"}, true},
		{"credentials", "https://user:" + "token@gitlab.com/group/subgroup/project.git", RepositoryMetadata{"https://gitlab.com/group/subgroup/project", "gitlab", "group/subgroup/project"}, true},
		{"bitbucket", "https://bitbucket.org/team/repo.git/", RepositoryMetadata{"https://bitbucket.org/team/repo", "bitbucket", "team/repo"}, true},
		{"unknown", "git@git.example.com:internal/tool.git", RepositoryMetadata{"https://git.example.com/internal/tool", "other", "internal/tool"}, true},
		{"empty", "   ", RepositoryMetadata{}, false},
		{"malformed", "not-a-url", RepositoryMetadata{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ParseRepositoryMetadata(test.remote)
			if ok != test.wantOkay || got != test.want {
				t.Fatalf("ParseRepositoryMetadata(%q) = (%#v, %v), want (%#v, %v)", test.remote, got, ok, test.want, test.wantOkay)
			}
		})
	}
}

func initRepository(t *testing.T, root string) {
	t.Helper()
	mustMkdirAll(t, root)
	runGitTest(t, root, "init")
	runGitTest(t, root, "config", "user.email", "test@example.com")
	runGitTest(t, root, "config", "user.name", "Test")
	runGitTest(t, root, "config", "commit.gpgsign", "false")
	mustWrite(t, filepath.Join(root, "tracked.txt"), "initial\n")
	runGitTest(t, root, "add", "tracked.txt")
	runGitTest(t, root, "commit", "-m", "initial")
}

func makeValidCommonDir(t *testing.T, root string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(root, "objects"))
	mustMkdirAll(t, filepath.Join(root, "refs"))
	mustWrite(t, filepath.Join(root, "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(root, "config"), "[core]\n")
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
