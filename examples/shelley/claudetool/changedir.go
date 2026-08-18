package claudetool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/dago/datool"

	"github.com/semistrict/dago/examples/shelley/gitstate"
)

// tildeReplace replaces the home directory prefix with ~ for display.
func tildeReplace(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// changeDirTool changes the working directory for bash commands.
type changeDirTool struct {
	// WorkingDir is the shared mutable working directory.
	WorkingDir *mutableWorkingDir
	// OnChange is called after the working directory changes successfully.
	// This can be used to persist the change to a database.
	OnChange func(newDir string)
}

const (
	changeDirName        = "change_dir"
	changeDirDescription = `Change the working directory for subsequent bash commands.

This affects the working directory used by the bash tool. The directory must exist.
Relative paths are resolved against the current working directory.

Prefer this tool over 'cd <path> && ...' in bash: 'cd' inside a bash
invocation does not persist, so you'd have to repeat it every call. Call
change_dir once, then run subsequent commands directly.
`
)

type changeDirInput struct {
	Path string `json:"path" description:"The directory path to change to (absolute or relative)" jsonschema:"minLength=1"`
}

// nativeTool returns the production dago implementation.
func (c *changeDirTool) nativeTool() datool.Tool {
	if c == nil || c.WorkingDir == nil {
		panic("change-directory tool working directory is required")
	}
	return datool.MustNew(changeDirName, changeDirDescription, func(ctx context.Context, input changeDirInput) (string, error) {
		text, err := c.execute(ctx, input)
		if err != nil {
			return "", err
		}
		return text, nil
	})
}

func (c *changeDirTool) execute(_ context.Context, req changeDirInput) (string, error) {
	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Get current working directory
	currentWD := c.WorkingDir.Get()

	// Resolve the path
	targetPath := req.Path
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(currentWD, targetPath)
	}
	targetPath = filepath.Clean(targetPath)

	// Validate the directory exists
	info, err := os.Stat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("directory does not exist: %s", targetPath)
		}
		return "", fmt.Errorf("failed to stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", targetPath)
	}

	// Update the working directory
	c.WorkingDir.Set(targetPath)

	// Notify callback if set
	if c.OnChange != nil {
		c.OnChange(targetPath)
	}

	// Check git status for the new directory
	state := gitstate.GetGitState(targetPath)
	var resultText string
	if state.IsRepo {
		resultText = fmt.Sprintf("Changed working directory to: %s\n\nGit repository detected (root: %s, branch: %s)", targetPath, tildeReplace(state.Worktree), state.Branch)
		if state.Branch == "" {
			resultText = fmt.Sprintf("Changed working directory to: %s\n\nGit repository detected (root: %s, detached HEAD)", targetPath, tildeReplace(state.Worktree))
		}
	} else {
		resultText = fmt.Sprintf("Changed working directory to: %s\n\nNot in a git repository.", targetPath)
	}

	return resultText, nil
}
