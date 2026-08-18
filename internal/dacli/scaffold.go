// Package dacli contains shared command-line workflows for the dago binary.
package dacli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	starterInstructions = `# Agent Instructions

You are a helpful AI agent.

## Guidelines

- Follow the user's instructions carefully.
- Ask for clarification when the request is ambiguous.
`
	starterTools = `{
  "tools": [],
  "interrupt_config": {}
}
`
	starterSkill = `---
name: example-skill
description: Worked example of the skill format; replace with your own trigger.
---

# Example skill

Skills hold detailed, reusable instructions the agent pulls in only when the
description above matches the task.

## Steps

1. Describe the first step the agent should take.
2. Add any constraints, formats, or examples it should follow.
3. Delete or replace this skill once you have your own.
`
	starterSubagent = `# Researcher

You are a focused research subagent.

## Guidelines

- Gather the requested information and return a concise, well-sourced summary.
- State assumptions and call out anything you could not verify.
`
)

// ScaffoldOptions controls optional overwrite behavior. Its zero value refuses
// to modify an existing project directory.
type ScaffoldOptions struct {
	Force bool
}

// Scaffold creates the pinned managed-agent starter layout below parent and
// returns its absolute path. Parent and name are required inputs.
func Scaffold(parent, name string, options ScaffoldOptions) (string, error) {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return "", errors.New("scaffold parent is required")
	}
	if err := validateProjectName(name); err != nil {
		return "", err
	}
	absoluteParent, err := filepath.Abs(parent)
	if err != nil {
		return "", fmt.Errorf("resolve scaffold parent: %w", err)
	}
	parentInfo, err := os.Stat(absoluteParent)
	if err != nil {
		return "", fmt.Errorf("inspect scaffold parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return "", errors.New("scaffold parent is not a directory")
	}

	projectPath := filepath.Join(absoluteParent, name)
	projectInfo, err := os.Lstat(projectPath)
	switch {
	case err == nil:
		if projectInfo.Mode()&fs.ModeSymlink != 0 || !projectInfo.IsDir() {
			return "", errors.New("scaffold target must be a real directory")
		}
		if !options.Force {
			return "", fmt.Errorf("%s already exists; use --force to overwrite starter files", name)
		}
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(projectPath, 0o700); err != nil {
			return "", fmt.Errorf("create scaffold target: %w", err)
		}
	default:
		return "", fmt.Errorf("inspect scaffold target: %w", err)
	}

	root, err := os.OpenRoot(projectPath)
	if err != nil {
		return "", fmt.Errorf("open scaffold target: %w", err)
	}
	defer root.Close()
	for _, directory := range []string{"skills", "skills/example-skill", "subagents", "subagents/researcher"} {
		if err := ensureDirectory(root, directory); err != nil {
			return "", err
		}
	}
	deploymentID := ""
	if options.Force {
		if existing, readErr := root.ReadFile("agent.json"); readErr == nil {
			var current struct {
				Extras map[string]any `json:"extras"`
			}
			if json.Unmarshal(existing, &current) == nil {
				deploymentID, _ = current.Extras[deploymentIdentityField].(string)
			}
		}
	}
	if validateDeployValue("deployment identity", deploymentID, 256) != nil {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("create scaffold deployment identity: %w", err)
		}
		deploymentID = hex.EncodeToString(random[:])
	}

	agentJSON, err := json.MarshalIndent(map[string]any{
		"name": name, "description": "A managed deep agent.", "model": "openai:gpt-5.5",
		"backend": map[string]any{"type": "state"}, "extras": map[string]any{deploymentIdentityField: deploymentID},
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode agent configuration: %w", err)
	}
	subagentJSON, err := json.MarshalIndent(map[string]any{
		"description": "Researches a topic and returns a concise summary.",
		"model":       "openai:gpt-5.5",
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode subagent configuration: %w", err)
	}
	files := map[string][]byte{
		"agent.json":                      append(agentJSON, '\n'),
		"AGENTS.md":                       []byte(starterInstructions),
		".gitignore":                      []byte(".env\n"),
		"tools.json":                      []byte(starterTools),
		"skills/example-skill/SKILL.md":   []byte(starterSkill),
		"subagents/researcher/agent.json": append(subagentJSON, '\n'),
		"subagents/researcher/AGENTS.md":  []byte(starterSubagent),
	}
	for path := range files {
		if err := validateWritableFile(root, path); err != nil {
			return "", err
		}
	}
	for path, content := range files {
		if err := root.WriteFile(path, content, 0o600); err != nil {
			return "", fmt.Errorf("write scaffold file %s: %w", path, err)
		}
		if err := root.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("secure scaffold file %s: %w", path, err)
		}
	}
	return projectPath, nil
}

func validateProjectName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || len(name) > 128 || name == "." || name == ".." {
		return errors.New("project name is invalid")
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\\:`) {
		return errors.New("project name must be one path component")
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return errors.New("project name contains a control character")
		}
	}
	return nil
}

func ensureDirectory(root *os.Root, path string) error {
	info, err := root.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("scaffold path %s must be a real directory", path)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if err := root.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create scaffold directory %s: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("inspect scaffold directory %s: %w", path, err)
	}
}

func validateWritableFile(root *os.Root, path string) error {
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect scaffold file %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("scaffold file %s must be regular", path)
	}
	return nil
}
