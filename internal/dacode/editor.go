package dacode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

const (
	editorDisplayNameMaxLength = 20
	maxEditorFileBytes         = 10 << 20
)

var guiEditorWaitFlags = map[string]string{
	"atom": "--wait", "code": "--wait", "cursor": "--wait",
	"subl": "-w", "windsurf": "--wait", "zed": "--wait",
}

var vimEditors = map[string]bool{"vi": true, "vim": true, "nvim": true}

type editorFinishedMsg struct {
	text      string
	cancelled bool
	err       error
}

type editorInvocation struct {
	command   *exec.Cmd
	directory string
	file      string
}

func resolveEditorCommand() ([]string, error) {
	configured := os.Getenv("VISUAL")
	if configured == "" {
		configured = os.Getenv("EDITOR")
	}
	if configured == "" {
		if runtime.GOOS == "windows" {
			return []string{"notepad"}, nil
		}
		return []string{"vi"}, nil
	}
	arguments, err := splitEditorCommand(configured)
	if err != nil {
		return nil, fmt.Errorf("parse editor command: %w", err)
	}
	if len(arguments) == 0 || arguments[0] == "" {
		return nil, errors.New("editor command resolved to no arguments")
	}
	return arguments, nil
}

func editorDisplayName() string {
	configured := os.Getenv("VISUAL")
	if configured == "" {
		configured = os.Getenv("EDITOR")
	}
	if configured == "" {
		return ""
	}
	arguments, err := splitEditorCommand(configured)
	if err != nil || len(arguments) == 0 {
		return ""
	}
	name := editorExecutableName(arguments[0])
	if len(name) == 0 || len(name) > editorDisplayNameMaxLength {
		return ""
	}
	hasLetterOrDigit := false
	for _, character := range name {
		if character > 0x7f || !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._+-", character)) {
			return ""
		}
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			hasLetterOrDigit = true
		}
	}
	if !hasLetterOrDigit {
		return ""
	}
	return name
}

func prepareEditorCommand(arguments []string, filePath string) []string {
	prepared := append([]string(nil), arguments...)
	if len(prepared) == 0 {
		return []string{filePath}
	}
	executable := strings.ToLower(editorExecutableName(prepared[0]))
	if waitFlag := guiEditorWaitFlags[executable]; waitFlag != "" && !containsArgument(prepared, waitFlag) {
		prepared = append(prepared[:1], append([]string{waitFlag}, prepared[1:]...)...)
	}
	if vimEditors[executable] && !containsArgument(prepared, "-i") {
		prepared = append(prepared, "-i", "NONE")
	}
	return append(prepared, filePath)
}

func prepareEditorInvocation(ctx context.Context, currentText string) (*editorInvocation, error) {
	if len(currentText) > maxEditorFileBytes {
		return nil, fmt.Errorf("draft exceeds %d bytes", maxEditorFileBytes)
	}
	arguments, err := resolveEditorCommand()
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "dago-editor-")
	if err != nil {
		return nil, fmt.Errorf("create editor directory: %w", err)
	}
	filePath := filepath.Join(directory, "prompt.md")
	if err := os.WriteFile(filePath, []byte(currentText), 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("write editor draft: %w", err)
	}
	prepared := prepareEditorCommand(arguments, filePath)
	return &editorInvocation{
		command: exec.CommandContext(ctx, prepared[0], prepared[1:]...), directory: directory, file: filePath,
	}, nil
}

func (invocation *editorInvocation) teaCommand() tea.Cmd {
	return tea.ExecProcess(invocation.command, func(processErr error) tea.Msg {
		return invocation.finish(processErr)
	})
}

func prepareEditorTeaCommand(ctx context.Context, currentText string) (tea.Cmd, error) {
	invocation, err := prepareEditorInvocation(ctx, currentText)
	if err != nil {
		return nil, err
	}
	return invocation.teaCommand(), nil
}

func (invocation *editorInvocation) finish(processErr error) editorFinishedMsg {
	defer os.RemoveAll(invocation.directory)
	if processErr != nil {
		var exitError *exec.ExitError
		if errors.As(processErr, &exitError) {
			return editorFinishedMsg{cancelled: true}
		}
		return editorFinishedMsg{err: processErr}
	}
	root, err := os.OpenRoot(invocation.directory)
	if err != nil {
		return editorFinishedMsg{err: err}
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(invocation.file))
	if err != nil {
		return editorFinishedMsg{err: err}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("edited draft is not a regular file")
		}
		return editorFinishedMsg{err: err}
	}
	if info.Size() > maxEditorFileBytes {
		return editorFinishedMsg{err: fmt.Errorf("edited draft exceeds %d bytes", maxEditorFileBytes)}
	}
	content, err := io.ReadAll(io.LimitReader(file, maxEditorFileBytes+1))
	if err != nil || len(content) > maxEditorFileBytes || !utf8.Valid(content) {
		if err == nil {
			err = errors.New("edited draft is too large or is not UTF-8")
		}
		return editorFinishedMsg{err: err}
	}
	edited := strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n")
	edited = strings.TrimSuffix(edited, "\n")
	if strings.TrimSpace(edited) == "" {
		return editorFinishedMsg{cancelled: true}
	}
	return editorFinishedMsg{text: edited}
}

func editorExecutableName(value string) string {
	base := filepath.Base(value)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func containsArgument(arguments []string, value string) bool {
	for _, argument := range arguments {
		if argument == value {
			return true
		}
	}
	return false
}

func splitEditorCommand(value string) ([]string, error) {
	var arguments []string
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if started {
			arguments = append(arguments, current.String())
			current.Reset()
			started = false
		}
	}
	for _, character := range value {
		if escaped {
			current.WriteRune(character)
			started = true
			escaped = false
			continue
		}
		if quote == '\'' {
			if character == '\'' {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			started = true
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				current.WriteRune(character)
			}
			started = true
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
			started = true
		case '\\':
			escaped = true
			started = true
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			current.WriteRune(character)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated quote or escape")
	}
	flush()
	return arguments, nil
}
