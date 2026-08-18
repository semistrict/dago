package dacode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxRubricFileBytes int64 = 1 << 20

// rubricFileReference accepts both the literal path documented by /rubric file
// and the @-prefixed path produced by the shared file-completion surface.
func rubricFileReference(value string) string {
	return "@" + strings.TrimPrefix(strings.TrimSpace(value), "@")
}

func resolveRubricText(value, workingDir string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("rubric must not be empty")
	}
	if !strings.HasPrefix(value, "@") {
		return value, nil
	}
	name := strings.TrimSpace(strings.TrimPrefix(value, "@"))
	if name == "" {
		return "", errors.New("rubric file path must not be empty")
	}
	if strings.HasPrefix(name, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve rubric home directory: %w", err)
		}
		name = filepath.Join(home, strings.TrimPrefix(name, "~"+string(filepath.Separator)))
	} else if !filepath.IsAbs(name) {
		name = filepath.Join(workingDir, name)
	}
	file, err := os.Open(name)
	if err != nil {
		return "", fmt.Errorf("read rubric file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect rubric file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("rubric file must be a regular file")
	}
	if info.Size() > maxRubricFileBytes {
		return "", fmt.Errorf("rubric file exceeds %d bytes", maxRubricFileBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxRubricFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read rubric file: %w", err)
	}
	if int64(len(content)) > maxRubricFileBytes {
		return "", fmt.Errorf("rubric file exceeds %d bytes", maxRubricFileBytes)
	}
	if !utf8.Valid(content) {
		return "", errors.New("rubric file must contain UTF-8 text")
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		return "", errors.New("rubric file is empty")
	}
	return text, nil
}
