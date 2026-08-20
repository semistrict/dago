package dacode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/damessage"
	"github.com/semistrict/dago/damodel"
	"github.com/semistrict/dago/damodel/modeltest"
	"github.com/semistrict/dago/daworkflow"
)

func TestWorkflowWorktreeRemovesCleanCheckoutAndUnchangedBranch(t *testing.T) {
	repository := newWorkflowTestRepository(t)
	manager := newWorkflowWorktreeManager(repository, filepath.Join(t.TempDir(), "state"))
	worktree, err := manager.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if worktree.root == repository || !strings.HasPrefix(worktree.branch, "workflow/agent-") {
		t.Fatalf("worktree = root %q branch %q", worktree.root, worktree.branch)
	}
	content, err := os.ReadFile(filepath.Join(worktree.root, "tracked.txt"))
	if err != nil || string(content) != "base\n" {
		t.Fatalf("isolated checkout content = %q, err = %v", content, err)
	}
	path, branch := worktree.path, worktree.branch
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("in-use worktree was removed: %v", err)
	}
	if err := worktree.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean worktree still exists: %v", err)
	}
	if _, err := workflowGitOutput(t.Context(), repository, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("unchanged branch %q still exists", branch)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowWorktreeRetainsCommittedBranch(t *testing.T) {
	repository := newWorkflowTestRepository(t)
	manager := newWorkflowWorktreeManager(repository, filepath.Join(t.TempDir(), "state"))
	worktree, err := manager.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree.root, "change.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkflowTestGit(t, worktree.root, "add", "change.txt")
	runWorkflowTestGit(t, worktree.root, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "isolated change")
	head := runWorkflowTestGit(t, worktree.root, "rev-parse", "HEAD")
	path, branch := worktree.path, worktree.branch
	if err := worktree.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed worktree still exists: %v", err)
	}
	if found := runWorkflowTestGit(t, repository, "rev-parse", "refs/heads/"+branch); found != head {
		t.Fatalf("retained branch head = %q, want %q", found, head)
	}
}

func TestWorkflowWorktreePreservesDirtyCheckoutUntilCommitted(t *testing.T) {
	repository := newWorkflowTestRepository(t)
	manager := newWorkflowWorktreeManager(repository, filepath.Join(t.TempDir(), "state"))
	worktree, err := manager.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree.root, "unfinished.txt"), []byte("recover me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := worktree.Close(); err == nil || !strings.Contains(err.Error(), "worktree retained") || !strings.Contains(err.Error(), worktree.path) {
		t.Fatalf("dirty close error = %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(worktree.root, "unfinished.txt")); err != nil || string(content) != "recover me\n" {
		t.Fatalf("preserved change = %q, err = %v", content, err)
	}
	runWorkflowTestGit(t, worktree.root, "add", "unfinished.txt")
	runWorkflowTestGit(t, worktree.root, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "finish change")
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worktree.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finished worktree still exists: %v", err)
	}
}

func TestWorkflowAgentUsesAndClosesIsolatedWorkspace(t *testing.T) {
	root := t.TempDir()
	backend, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	script := modeltest.New(damodel.Profile{NativeStreaming: true, SupportsSeparateSystemMessage: true}, modeltest.Step{
		Check: func(request damodel.Request) error {
			system := ""
			if request.SystemMessage != nil {
				system = request.SystemMessage.TextContent()
			}
			for _, message := range request.Messages {
				if message.Role == damessage.RoleSystem {
					system += message.TextContent()
				}
			}
			if !strings.Contains(system, root) || !strings.Contains(system, "workflow/agent-test") {
				return fmt.Errorf("isolated system message = %q", system)
			}
			return nil
		},
		Chunks: []damodel.Chunk{{MessageDelta: damessage.Assistant("done"), Done: true}},
	})
	runner := &dacodeWorkflowAgentRunner{
		authentication: modelAuthentication{apiKey: "test-key", decorateModel: func(damodel.Chat) damodel.Chat { return script }},
		model:          "openai:test-model",
		worktree: func(context.Context) (workflowAgentWorkspace, error) {
			return workflowAgentWorkspace{
				backend: backend, system: "isolated", workingDir: root, branch: "workflow/agent-test",
				closer: closeFunc(func() error { closed = true; return nil }),
			}, nil
		},
	}
	response, err := runner.RunWorkflowAgent(t.Context(), daworkflow.AgentRequest{Prompt: "Inspect the isolated workspace.", Isolation: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Value != "done" || !closed {
		t.Fatalf("response = %#v, closed = %t", response, closed)
	}
}

type closeFunc func() error

func (close closeFunc) Close() error { return close() }

func newWorkflowTestRepository(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	runWorkflowTestGit(t, repository, "init")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkflowTestGit(t, repository, "add", "tracked.txt")
	runWorkflowTestGit(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial")
	return repository
}

func runWorkflowTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, err := workflowGitOutput(t.Context(), directory, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}
