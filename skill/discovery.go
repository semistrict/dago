package skill

import (
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ExpandPath expands a leading ~/ using the current user's home directory.
func ExpandPath(value string) string {
	if strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, value[2:])
		}
	}
	return value
}

// FindFile returns the canonical skill file in dir. A lowercase filename is
// accepted for compatibility with existing skill collections.
func FindFile(dir string) string {
	for _, name := range []string{"SKILL.md", "skill.md"} {
		filePath := filepath.Join(dir, name)
		if _, err := os.Stat(filePath); err == nil {
			return filePath
		}
	}
	return ""
}

// DiscoverDirectories scans priority-ordered source directories for immediate
// child skill directories. The first occurrence of a physical file wins.
func DiscoverDirectories(dirs []string) []Skill {
	var result []Skill
	seen := make(map[string]bool)
	for _, dir := range dirs {
		dir = ExpandPath(dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			skillDir := filepath.Join(dir, entry.Name())
			info, err := os.Stat(skillDir)
			if err != nil || !info.IsDir() {
				continue
			}
			filePath := FindFile(skillDir)
			if filePath == "" {
				continue
			}
			absolute, err := filepath.Abs(filePath)
			if err != nil || seen[absolute] {
				continue
			}
			seen[absolute] = true
			parsed, err := ParseFile(filePath)
			if err != nil || parsed.Name != entry.Name() {
				continue
			}
			result = append(result, parsed)
		}
	}
	return result
}

// DiscoverTree recursively discovers skills below root. Hidden directories,
// dependency trees, and vendored trees are excluded. Claimed names include
// invalid or empty skill files so applications can implement suppression.
func DiscoverTree(ctx context.Context, root string) ([]Skill, map[string]bool) {
	var result []Skill
	seen := make(map[string]bool)
	claimed := make(map[string]bool)
	_ = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
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
		if strings.ToLower(entry.Name()) != "skill.md" {
			return nil
		}
		name := filepath.Base(filepath.Dir(filePath))
		claimed[name] = true
		absolute, err := filepath.Abs(filePath)
		if err != nil || seen[absolute] {
			return nil
		}
		seen[absolute] = true
		parsed, err := ParseFile(filePath)
		if err != nil || parsed.Name != name {
			return nil
		}
		result = append(result, parsed)
		return nil
	})
	return result, claimed
}

// ParentDirectories returns existing directories named leaf while walking from
// workingDir through stopAt. The closest directory is returned first.
func ParentDirectories(workingDir, stopAt, leaf string) []string {
	if stopAt == "" {
		stopAt = filepath.VolumeName(workingDir) + string(filepath.Separator)
	}
	var result []string
	seen := make(map[string]bool)
	for current := workingDir; current != ""; current = filepath.Dir(current) {
		candidate := filepath.Join(current, leaf)
		if !seen[candidate] {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				result = append(result, candidate)
				seen[candidate] = true
			}
		}
		if current == stopAt {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return result
}

// ClaimedNames returns immediate child directories containing a skill file,
// including files whose metadata is invalid or intentionally empty.
func ClaimedNames(dirs []string) map[string]bool {
	result := make(map[string]bool)
	for _, dir := range dirs {
		entries, err := os.ReadDir(ExpandPath(dir))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			skillDir := filepath.Join(ExpandPath(dir), entry.Name())
			info, err := os.Stat(skillDir)
			if err == nil && info.IsDir() && FindFile(skillDir) != "" {
				result[entry.Name()] = true
			}
		}
	}
	return result
}

// FindGitRoot walks upward to the closest directory containing .git.
func FindGitRoot(dir string) string {
	for current := dir; current != ""; current = filepath.Dir(current) {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return ""
}

// CreateTemplate creates a valid skill skeleton below root.
func CreateTemplate(root, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	filePath := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(filePath); err == nil {
		return "", fmt.Errorf("%s already exists", filePath)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating directory: %w", err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: Use when %s.\n---\n\nWhen %s, act accordingly.\n", name, name, name)
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing SKILL.md: %w", err)
	}
	return filePath, nil
}

// RenderXML renders stable skill metadata. activate supplies the product- or
// backend-specific instruction for loading each full skill on demand.
func RenderXML(skills []Skill, activate func(Skill) string) string {
	if len(skills) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("<available_skills>\n")
	for _, item := range skills {
		output.WriteString("<skill>\n<name>")
		output.WriteString(html.EscapeString(item.Name))
		output.WriteString("</name>\n<description>")
		output.WriteString(html.EscapeString(item.Description))
		output.WriteString("</description>\n<activate>")
		if activate != nil {
			output.WriteString(html.EscapeString(activate(item)))
		}
		output.WriteString("</activate>\n</skill>\n")
	}
	output.WriteString("</available_skills>")
	return output.String()
}

// SortByName provides deterministic metadata ordering for callers that merge
// multiple discovery mechanisms.
func SortByName(skills []Skill) {
	sort.SliceStable(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
}
