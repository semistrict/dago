package dacode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/dago/daskill"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	startupCommandTimeout = 60 * time.Second
	startupOutputLimit    = 100_000
	startupSkillLimit     = 10 << 20
)

// composeInitialSkillPrompt loads a project skill and wraps the initial request
// in the same progressive-disclosure envelope used by the reference CLI.
func composeInitialSkillPrompt(workingDir, skillName, request string) (string, error) {
	skillName = strings.ToLower(strings.TrimSpace(skillName))
	if skillName == "" {
		return request, nil
	}
	if err := daskill.ValidateName(skillName); err != nil {
		return "", fmt.Errorf("invalid startup skill %q: %w", skillName, err)
	}

	root, err := os.OpenRoot(workingDir)
	if err != nil {
		return "", fmt.Errorf("open workspace for startup skill: %w", err)
	}
	defer root.Close()

	type discoveredSkill struct {
		metadata daskill.Skill
		content  string
	}
	var selected *discoveredSkill
	for _, source := range []string{".agents/skills", ".claude/skills", ".deepagents/skills"} {
		filePath := filepath.Join(source, skillName, "SKILL.md")
		content, readErr := readRootFile(root, filePath, startupSkillLimit)
		if errors.Is(readErr, os.ErrNotExist) {
			filePath = filepath.Join(source, skillName, "skill.md")
			content, readErr = readRootFile(root, filePath, startupSkillLimit)
		}
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return "", fmt.Errorf("load startup skill %q: %w", skillName, readErr)
		}
		metadata, _, parseErr := daskill.ParseContent(content, filepath.ToSlash(filePath))
		if parseErr != nil {
			return "", fmt.Errorf("load startup skill %q: %w", skillName, parseErr)
		}
		if metadata.Name != skillName {
			return "", fmt.Errorf("load startup skill %q: name must match directory %q", skillName, skillName)
		}
		selected = &discoveredSkill{metadata: metadata, content: content}
	}
	if selected == nil {
		return "", fmt.Errorf("startup skill %q was not found in project skill directories", skillName)
	}
	if strings.TrimSpace(selected.content) == "" {
		return "", fmt.Errorf("startup skill %q is empty", skillName)
	}

	prompt := "I'm invoking the skill `" + selected.metadata.Name + "`. " +
		"Below are the full instructions from the skill's SKILL.md file. " +
		"Follow these instructions to complete the task.\n\n---\n" + selected.content + "\n---"
	if request != "" {
		prompt += "\n\n**User request:** " + request
	}
	return prompt, nil
}

func readRootFile(root *os.Root, name string, limit int64) (string, error) {
	file, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", filepath.ToSlash(name))
	}
	if info.Size() > limit {
		return "", fmt.Errorf("%s exceeds %d bytes", filepath.ToSlash(name), limit)
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > limit {
		return "", fmt.Errorf("%s exceeds %d bytes", filepath.ToSlash(name), limit)
	}
	return string(content), nil
}

func runStartupCommand(ctx context.Context, command, workingDir string, cleanStdout bool, stdout, stderr io.Writer) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}
	output := stdout
	if cleanStdout {
		output = stderr
	}
	if !cleanStdout {
		if _, err := fmt.Fprintf(output, "Running startup command: %s\n", unicodesecurity.RenderTerminalSafe(command)); err != nil {
			return err
		}
	}

	commandContext, cancel := context.WithTimeout(ctx, startupCommandTimeout)
	defer cancel()
	process := startupShellCommand(commandContext, command)
	process.Dir = workingDir
	configureStartupProcess(process)
	var processStdout, processStderr boundedStartupOutput
	processStdout.limit = startupOutputLimit
	processStderr.limit = startupOutputLimit
	process.Stdout = &processStdout
	process.Stderr = &processStderr
	err := process.Run()
	if text := processStdout.String(); text != "" {
		if _, writeErr := fmt.Fprintln(output, unicodesecurity.RenderTerminalSafe(strings.TrimRight(text, "\r\n"))); writeErr != nil {
			return writeErr
		}
	}
	if text := processStderr.String(); text != "" {
		if _, writeErr := fmt.Fprintln(output, unicodesecurity.RenderTerminalSafe(strings.TrimRight(text, "\r\n"))); writeErr != nil {
			return writeErr
		}
	}
	if processStdout.truncated || processStderr.truncated {
		if _, writeErr := fmt.Fprintf(output, "Warning: startup command output was truncated at %d bytes per stream\n", startupOutputLimit); writeErr != nil {
			return writeErr
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		_, writeErr := fmt.Fprintf(output, "Warning: startup command timed out (%s limit); continuing anyway\n", startupCommandTimeout)
		return writeErr
	}
	if err == nil {
		return nil
	}
	var exitError interface{ ExitCode() int }
	if errors.As(err, &exitError) {
		_, writeErr := fmt.Fprintf(output, "Warning: startup command exited with code %d; continuing anyway\n", exitError.ExitCode())
		return writeErr
	}
	_, writeErr := fmt.Fprintf(output, "Warning: startup command failed to launch: %s; continuing anyway\n", unicodesecurity.RenderTerminalSafe(err.Error()))
	return writeErr
}

type boundedStartupOutput struct {
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (output *boundedStartupOutput) Write(value []byte) (int, error) {
	remaining := output.limit - output.data.Len()
	if remaining > 0 {
		_, _ = output.data.Write(value[:min(remaining, len(value))])
	}
	if len(value) > remaining {
		output.truncated = true
	}
	return len(value), nil
}

func (output *boundedStartupOutput) String() string { return output.data.String() }
