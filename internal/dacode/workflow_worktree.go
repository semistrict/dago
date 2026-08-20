package dacode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const workflowGitTimeout = 30 * time.Second

type workflowWorktreeManager struct {
	workingDir string
	storageDir string

	mu     sync.Mutex
	active map[string]*workflowWorktree
}

type workflowWorktree struct {
	manager  *workflowWorktreeManager
	repoRoot string
	root     string
	path     string
	branch   string
	base     string

	closeMu  sync.Mutex
	closed   bool
	released bool
}

func newWorkflowWorktreeManager(workingDir, stateDir string) *workflowWorktreeManager {
	return &workflowWorktreeManager{
		workingDir: workingDir,
		storageDir: filepath.Join(stateDir, "workflows", "worktrees"),
		active:     map[string]*workflowWorktree{},
	}
}

func (manager *workflowWorktreeManager) Open(ctx context.Context) (*workflowWorktree, error) {
	if manager == nil {
		return nil, errors.New("workflow worktree manager is unavailable")
	}
	workingDir, err := filepath.EvalSymlinks(manager.workingDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow working directory: %w", err)
	}
	repoRoot, err := workflowGitOutput(ctx, workingDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("resolve workflow repository: %w", err)
	}
	base, err := workflowGitOutput(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve workflow base commit: %w", err)
	}
	relative, err := filepath.Rel(repoRoot, workingDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("workflow working directory %q is outside repository %q", workingDir, repoRoot)
	}
	stateDir := filepath.Dir(filepath.Dir(manager.storageDir))
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create workflow state directory: %w", err)
	}
	resolvedStateDir, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow state directory: %w", err)
	}
	storageDir := filepath.Join(resolvedStateDir, "workflows", "worktrees")
	if pathWithin(repoRoot, resolvedStateDir) {
		storageDir = filepath.Join(filepath.Dir(repoRoot), "."+filepath.Base(repoRoot)+"-workflow-worktrees")
	}
	if err := os.MkdirAll(storageDir, 0o700); err != nil {
		return nil, fmt.Errorf("create workflow worktree directory: %w", err)
	}

	for attempt := 0; attempt < 4; attempt++ {
		identifier, idErr := workflowWorktreeID()
		if idErr != nil {
			return nil, idErr
		}
		path := filepath.Join(storageDir, identifier)
		branch := "workflow/agent-" + identifier
		if _, addErr := workflowGit(ctx, repoRoot, "worktree", "add", "-b", branch, path, "HEAD"); addErr != nil {
			if attempt < 3 && strings.Contains(addErr.Error(), "already exists") {
				continue
			}
			return nil, fmt.Errorf("create workflow worktree: %w", addErr)
		}
		root := path
		if relative != "." {
			root = filepath.Join(path, relative)
			info, statErr := os.Stat(root)
			if statErr != nil || !info.IsDir() {
				_ = removeWorkflowWorktree(repoRoot, path, branch, true)
				if statErr != nil {
					return nil, fmt.Errorf("open workflow worktree subdirectory: %w", statErr)
				}
				return nil, fmt.Errorf("workflow worktree subdirectory %q is not a directory", root)
			}
		}
		worktree := &workflowWorktree{
			manager: manager, repoRoot: repoRoot, root: root, path: path, branch: branch, base: base,
		}
		manager.mu.Lock()
		manager.active[path] = worktree
		manager.mu.Unlock()
		return worktree, nil
	}
	return nil, errors.New("allocate unique workflow worktree")
}

func (worktree *workflowWorktree) Close() error {
	if worktree == nil {
		return nil
	}
	return worktree.cleanup(true)
}

func (worktree *workflowWorktree) cleanup(release bool) error {
	worktree.closeMu.Lock()
	defer worktree.closeMu.Unlock()
	if release {
		worktree.released = true
	}
	if !worktree.released {
		return nil
	}
	if worktree.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), workflowGitTimeout)
	defer cancel()
	status, err := workflowGitOutput(ctx, worktree.path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect workflow worktree %q: %w", worktree.path, err)
	}
	if status != "" {
		return fmt.Errorf("workflow worker left uncommitted changes in %q on branch %q; worktree retained", worktree.path, worktree.branch)
	}
	head, err := workflowGitOutput(ctx, worktree.path, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve workflow worktree commit: %w", err)
	}
	if _, err := workflowGit(ctx, worktree.repoRoot, "worktree", "remove", worktree.path); err != nil {
		return fmt.Errorf("remove workflow worktree %q: %w", worktree.path, err)
	}
	worktree.closed = true
	worktree.manager.mu.Lock()
	delete(worktree.manager.active, worktree.path)
	worktree.manager.mu.Unlock()
	if head == worktree.base {
		if _, err := workflowGit(ctx, worktree.repoRoot, "branch", "-d", worktree.branch); err != nil {
			return fmt.Errorf("remove unchanged workflow branch %q: %w", worktree.branch, err)
		}
	}
	return nil
}

func (manager *workflowWorktreeManager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	active := make([]*workflowWorktree, 0, len(manager.active))
	for _, worktree := range manager.active {
		active = append(active, worktree)
	}
	manager.mu.Unlock()
	var result error
	for _, worktree := range active {
		if err := worktree.cleanup(false); err != nil && result == nil {
			result = err
		}
	}
	return result
}

func workflowWorktreeID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate workflow worktree id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func removeWorkflowWorktree(repoRoot, path, branch string, deleteBranch bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), workflowGitTimeout)
	defer cancel()
	if _, err := workflowGit(ctx, repoRoot, "worktree", "remove", path); err != nil {
		return fmt.Errorf("remove workflow worktree %q: %w", path, err)
	}
	if deleteBranch {
		if _, err := workflowGit(ctx, repoRoot, "branch", "-d", branch); err != nil {
			return fmt.Errorf("remove unchanged workflow branch %q: %w", branch, err)
		}
	}
	return nil
}

func workflowGitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	output, err := workflowGit(ctx, directory, args...)
	return strings.TrimSpace(output), err
}

func workflowGit(ctx context.Context, directory string, args ...string) (string, error) {
	commandContext, cancel := context.WithTimeout(ctx, workflowGitTimeout)
	defer cancel()
	command := exec.CommandContext(commandContext, "git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), nil
	}
	if commandContext.Err() != nil {
		return "", commandContext.Err()
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		message = err.Error()
	}
	return "", errors.New(message)
}

var _ io.Closer = (*workflowWorktreeManager)(nil)
