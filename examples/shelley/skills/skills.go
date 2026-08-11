// Package skills implements the Agent Skills specification.
// See https://agentskills.io for the full specification.
package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	dskill "github.com/semistrict/dago/daskill"
)

const (
	MaxNameLength          = dskill.MaxNameLength
	MaxDescriptionLength   = dskill.MaxDescriptionLength
	MaxCompatibilityLength = dskill.MaxCompatibilityLength
)

// Skill preserves Shelley's package-level name without defining a second skill
// model. All skill values use dago's Agent Skills contract directly.
type Skill = dskill.Skill

// Discover finds all skills in the given directories.
// It scans each directory for subdirectories containing SKILL.md files.
func Discover(dirs []string) []Skill {
	return dskill.DiscoverDirectories(dirs)
}

// CreateTemplate creates a new skill directory with a template SKILL.md
// in ~/.config/shelley/<name>/SKILL.md. It returns the path to the created file.
func CreateTemplate(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}

	return dskill.CreateTemplate(filepath.Join(home, ".config", "shelley"), name)
}

// findSkillMD looks for SKILL.md or skill.md in a directory.
func findSkillMD(dir string) string {
	return dskill.FindFile(dir)
}

// Parse reads and parses a SKILL.md file.
func Parse(path string) (Skill, error) {
	return dskill.ParseFile(path)
}

type ValidationError = dskill.ValidationError

// validateName checks that a skill name follows the spec.
func validateName(name string) error {
	return dskill.ValidateName(name)
}

// RenderPromptXML is retained for the standalone prompt compatibility API.
// Production conversations use dago's SkillsMiddleware directly.
func RenderPromptXML(skills []Skill) string {
	return dskill.RenderXML(skills, func(item dskill.Skill) string { return "shelley skill cat " + item.Name })
}

// DefaultDirs returns the default skill directories to search.
// These are always returned if they exist, regardless of the current working directory.
func DefaultDirs() []string {
	var dirs []string

	home, err := os.UserHomeDir()
	if err != nil {
		return dirs
	}

	// Search these directories for skills:
	// 1. ~/.config/shelley/ (XDG convention for Shelley)
	// 2. ~/.config/agents/skills (shared agents skills directory)
	// 3. ~/.shelley/ (legacy location)
	candidateDirs := []string{
		filepath.Join(home, ".config", "shelley"),
		filepath.Join(home, ".config", "agents", "skills"),
		filepath.Join(home, ".shelley"),
	}

	for _, dir := range candidateDirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}

	return dirs
}

// expandPath expands ~ to the user's home directory.
func expandPath(path string) string {
	return dskill.ExpandPath(path)
}

// ProjectSkillsDirs returns all .skills directories found by walking up from
// the working directory to the git root (or filesystem root if no git root).
func ProjectSkillsDirs(workingDir, gitRoot string) []string {
	return dskill.ParentDirectories(workingDir, gitRoot, ".skills")
}

// DiscoverInTree finds all skills by walking the directory tree looking for SKILL.md files.
// If gitRoot is provided, it searches from gitRoot. Otherwise, it searches from workingDir downward.
// It returns both the parsed skills and the set of all SKILL.md parent directory names
// encountered during the walk (including unparseable/empty ones). This avoids needing
// a second walk just to collect names.
func DiscoverInTree(workingDir, gitRoot string) ([]Skill, map[string]bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	searchRoot := gitRoot
	if searchRoot == "" {
		searchRoot = workingDir
	}
	return dskill.DiscoverTree(ctx, searchRoot)
}

// ListAll returns all available skills (built-in + filesystem), deduplicated by name.
//
// Filesystem skills take priority over built-in skills with the same name.
// An empty SKILL.md on the filesystem suppresses the corresponding built-in
// skill entirely — this is the mechanism for users to disable built-in skills.
//
// If gitRoot is empty, it is computed from workingDir.
func ListAll(workingDir, gitRoot string) []Skill {
	if gitRoot == "" {
		gitRoot = findGitRoot(workingDir)
	}

	dirs := DefaultDirs()
	dirs = append(dirs, ProjectSkillsDirs(workingDir, gitRoot)...)

	all := Discover(dirs)

	// Add tree-discovered skills, deduplicated by name (first-seen wins).
	// DiscoverInTree also returns all SKILL.md names it encountered
	// (including unparseable/empty ones) so we don't need a second walk.
	seen := make(map[string]bool)
	for _, s := range all {
		seen[s.Name] = true
	}
	treeSkills, treeNames := DiscoverInTree(workingDir, gitRoot)
	for _, s := range treeSkills {
		if !seen[s.Name] {
			all = append(all, s)
			seen[s.Name] = true
		}
	}

	// Collect all skill names claimed on the filesystem (including empty
	// SKILL.md files that wouldn't survive Parse). A filesystem SKILL.md —
	// even an empty one — takes precedence over a built-in skill of the
	// same name. This lets users suppress a built-in skill by placing an
	// empty SKILL.md in the matching directory.
	fsNames := dirSkillNames(dirs)
	for name := range treeNames {
		fsNames[name] = true
	}
	for _, s := range all {
		fsNames[s.Name] = true
	}

	for _, s := range BuiltinSkills() {
		if !fsNames[s.Name] {
			all = append(all, s)
		}
	}

	return all
}

// FindByName looks up a skill by name and returns its raw SKILL.md content.
//
// Filesystem skills take priority: if a SKILL.md exists on the filesystem
// for the given name it is returned, even if a built-in skill with the
// same name exists. An empty filesystem SKILL.md suppresses the built-in
// skill — this lets users delete built-in skills they don't want.
func FindByName(name, workingDir string) (string, error) {
	gitRoot := findGitRoot(workingDir)
	dirs := DefaultDirs()
	dirs = append(dirs, ProjectSkillsDirs(workingDir, gitRoot)...)

	// Filesystem first: check directory-based discovery, then tree discovery.
	for _, s := range Discover(dirs) {
		if s.Name == name {
			content, err := os.ReadFile(s.Path)
			if err != nil {
				return "", fmt.Errorf("reading skill %q: %w", name, err)
			}
			return string(content), nil
		}
	}
	treeSkills, treeNames := DiscoverInTree(workingDir, gitRoot)
	for _, s := range treeSkills {
		if s.Name == name {
			content, err := os.ReadFile(s.Path)
			if err != nil {
				return "", fmt.Errorf("reading skill %q: %w", name, err)
			}
			return string(content), nil
		}
	}

	// If a SKILL.md exists on the filesystem for this name but didn't
	// parse (e.g. it's empty), the user is deliberately suppressing
	// the built-in skill. Don't fall through.
	fsNames := dirSkillNames(dirs)
	for n := range treeNames {
		fsNames[n] = true
	}
	if fsNames[name] {
		// Distinguish intentional suppression (empty file) from parse errors.
		for _, dir := range dirs {
			dir = expandPath(dir)
			if path := findSkillMD(filepath.Join(dir, name)); path != "" {
				if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
					if _, parseErr := Parse(path); parseErr != nil {
						return "", fmt.Errorf("skill %q (%s): %w", name, path, parseErr)
					}
				}
				break
			}
		}
		return "", fmt.Errorf("skill %q is disabled", name)
	}

	// Fall back to built-in skills.
	for _, s := range BuiltinSkills() {
		if s.Name == name {
			data, err := builtinFS.ReadFile("builtin/" + name + "/SKILL.md")
			if err != nil {
				return "", fmt.Errorf("reading built-in skill %q: %w", name, err)
			}
			return string(data), nil
		}
	}

	return "", fmt.Errorf("skill %q not found", name)
}

// dirSkillNames returns the set of skill names found in the given skill
// directories (not the project tree — tree names come from DiscoverInTree).
// An empty SKILL.md in a directory like ~/.config/shelley/schedule/ prevents
// the built-in "schedule" skill from appearing.
func dirSkillNames(dirs []string) map[string]bool {
	return dskill.ClaimedNames(dirs)
}

// findGitRoot returns the git root for the given directory, or "" if not in a repo.
func findGitRoot(dir string) string {
	return dskill.FindGitRoot(dir)
}
