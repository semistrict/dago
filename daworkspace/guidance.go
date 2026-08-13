// Package daworkspace discovers workspace instructions and conventional local
// configuration shared by dago applications.
package daworkspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultDiscoveryTimeout = 2 * time.Second

// GuidanceFile is one instruction file selected for immediate injection.
type GuidanceFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Guidance is the workspace instruction context shared by local applications.
// Root contains files whose content applies immediately. Subdirectories lists
// more narrowly scoped files that should be read before editing below them.
type Guidance struct {
	Root           []GuidanceFile `json:"root"`
	Subdirectories []string       `json:"subdirectories"`
}

// GuidanceOptions controls trusted instruction discovery.
type GuidanceOptions struct {
	Root             string   `json:"root,omitempty"`
	WorkingDirectory string   `json:"working_directory,omitempty"`
	UserFiles        []string `json:"user_files,omitempty"`
	// TrustWorkspace enables instruction discovery inside Root and
	// WorkingDirectory. Keep it false for unreviewed workspaces.
	TrustWorkspace bool          `json:"trust_workspace,omitempty"`
	Timeout        time.Duration `json:"timeout,omitempty"`
}

// DiscoverGuidance finds user and workspace instruction files with stable
// precedence and content/path deduplication. Unreadable files and directories
// are ignored so optional customization cannot prevent an application start.
func DiscoverGuidance(ctx context.Context, options GuidanceOptions) Guidance {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultDiscoveryTimeout
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var result Guidance
	seenPaths := make(map[string]bool)
	seenContents := make(map[string]bool)
	appendFile := func(filePath string) {
		if discoveryCtx.Err() != nil {
			return
		}
		canonical := canonicalPath(filePath)
		if seenPaths[canonical] {
			return
		}
		content, err := os.ReadFile(filePath)
		if err != nil || len(content) == 0 {
			return
		}
		text := string(content)
		if seenContents[text] {
			return
		}
		seenPaths[canonical] = true
		seenContents[text] = true
		result.Root = append(result.Root, GuidanceFile{Path: filePath, Content: text})
	}

	for _, filePath := range options.UserFiles {
		appendFile(filePath)
	}
	if !options.TrustWorkspace || discoveryCtx.Err() != nil {
		return result
	}

	root := filepath.Clean(options.Root)
	workingDirectory := filepath.Clean(options.WorkingDirectory)
	if options.Root == "" {
		root = workingDirectory
	}
	for _, filePath := range guidanceFilesInDirectory(root) {
		appendFile(filePath)
	}
	if workingDirectory != "" && workingDirectory != root {
		for _, filePath := range guidanceFilesInDirectory(workingDirectory) {
			appendFile(filePath)
		}
	}
	result.Subdirectories = subdirectoryGuidance(discoveryCtx, root)
	return result
}

// ExistingDirectories keeps only paths that currently name directories.
func ExistingDirectories(paths ...string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			result = append(result, path)
		}
	}
	return result
}

// FormatSubdirectoryGuidance renders the shared instruction reminder used by
// local agent applications.
func FormatSubdirectoryGuidance(paths []string, limit int) string {
	if len(paths) == 0 {
		return ""
	}
	if limit <= 0 {
		limit = len(paths)
	}
	show := paths
	if len(show) > limit {
		show = show[:limit]
	}
	var output strings.Builder
	output.WriteString("\nSubdirectory guidance files (read before editing files in these directories):\n")
	for _, filePath := range show {
		output.WriteString(filePath)
		output.WriteByte('\n')
	}
	if len(paths) > len(show) {
		fmt.Fprintf(&output, "...and %d more. Use `find` to discover others.\n", len(paths)-len(show))
	}
	return output.String()
}

func guidanceFilesInDirectory(directory string) []string {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var result []string
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if isGuidanceFile(name) && name != "readme.md" && !seen[name] {
			seen[name] = true
			result = append(result, filepath.Join(directory, entry.Name()))
		}
	}
	return result
}

func subdirectoryGuidance(ctx context.Context, root string) []string {
	var result []string
	seen := make(map[string]bool)
	_ = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if filePath != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Dir(filePath) == root || !isGuidanceFile(strings.ToLower(entry.Name())) {
			return nil
		}
		canonical := canonicalPath(filePath)
		if !seen[canonical] {
			seen[canonical] = true
			result = append(result, filePath)
		}
		return nil
	})
	return result
}

func isGuidanceFile(name string) bool {
	switch name {
	case "agents.md", "agent.md", "claude.md", "dear_llm.md", "readme.md":
		return true
	default:
		return false
	}
}

func canonicalPath(value string) string {
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		value = resolved
	}
	return strings.ToLower(filepath.Clean(value))
}
