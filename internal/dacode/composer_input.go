package dacode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/semistrict/dago/damessage"
)

const (
	inputHistoryLimit       = 100
	largePasteCharacters    = 800
	largePasteNewlines      = 2
	legacyPasteBurstGap     = 30 * time.Millisecond
	legacyPasteFlushDelay   = 80 * time.Millisecond
	legacyPasteMinimumRunes = 3
	shellCommandTimeout     = 60 * time.Second
	shellOutputLimit        = 1 << 20
	mediaInputLimit         = 20 << 20
)

type inputMode uint8

const (
	inputNormal inputMode = iota
	inputShell
	inputIncognitoShell
	inputCommand
)

func (mode inputMode) prefix() string {
	switch mode {
	case inputShell:
		return "!"
	case inputIncognitoShell:
		return "!!"
	case inputCommand:
		return "/"
	default:
		return ""
	}
}

func (mode inputMode) prompt() string {
	switch mode {
	case inputShell, inputIncognitoShell:
		return "$ "
	case inputCommand:
		return "/ "
	default:
		return "> "
	}
}

type pasteBinding struct {
	placeholder string
	text        string
}

type completionKind uint8

const (
	completionNone completionKind = iota
	completionSlash
	completionFile
)

type completionState struct {
	kind     completionKind
	items    []string
	selected int
}

type queuedInput struct {
	mode        inputMode
	value       string
	display     string
	attachments []damessage.ContentBlock
}

type inputHistory struct {
	path    string
	entries []string
	records int
	matches []string
	index   int
	draft   string
}

type historyRecord struct {
	Text string `json:"text"`
}

func loadInputHistory(path string) (*inputHistory, error) {
	history := &inputHistory{path: path, index: -1}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return history, nil
	}
	if err != nil {
		return history, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		history.records++
		line := scanner.Text()
		var record historyRecord
		if json.Unmarshal([]byte(line), &record) == nil && record.Text != "" {
			line = record.Text
		}
		if strings.TrimSpace(line) != "" {
			history.entries = append(history.entries, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return history, err
	}
	if len(history.entries) > inputHistoryLimit {
		history.entries = history.entries[len(history.entries)-inputHistoryLimit:]
	}
	return history, nil
}

func (history *inputHistory) add(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || (strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "/skill:")) {
		return nil
	}
	if len(history.entries) > 0 && history.entries[len(history.entries)-1] == value {
		return nil
	}
	history.entries = append(history.entries, value)
	history.records++
	if len(history.entries) > inputHistoryLimit {
		history.entries = history.entries[len(history.entries)-inputHistoryLimit:]
	}
	if history.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(history.path), 0o700); err != nil {
		return err
	}
	encoded, err := json.Marshal(historyRecord{Text: value})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(history.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(file, string(encoded))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	if history.records > inputHistoryLimit*2 {
		return history.compact()
	}
	return nil
}

func (history *inputHistory) compact() error {
	temporary, err := os.CreateTemp(filepath.Dir(history.path), ".input-history-*.jsonl")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	for _, entry := range history.entries {
		if err := encoder.Encode(historyRecord{Text: entry}); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFileDurably(temporaryPath, history.path); err != nil {
		return err
	}
	history.records = len(history.entries)
	return nil
}

func (history *inputHistory) previous(current string) (string, bool) {
	if len(history.entries) == 0 {
		return "", false
	}
	if history.index < 0 {
		history.draft = current
		query := strings.ToLower(current)
		history.matches = history.matches[:0]
		for index := len(history.entries) - 1; index >= 0; index-- {
			if query == "" || strings.Contains(strings.ToLower(history.entries[index]), query) {
				history.matches = append(history.matches, history.entries[index])
			}
		}
	}
	if history.index+1 >= len(history.matches) {
		return "", false
	}
	history.index++
	return history.matches[history.index], true
}

func (history *inputHistory) next() (string, bool) {
	if history.index < 0 {
		return "", false
	}
	history.index--
	if history.index < 0 {
		return history.draft, true
	}
	return history.matches[history.index], true
}

func (history *inputHistory) resetNavigation() {
	history.index = -1
	history.matches = nil
	history.draft = ""
}

type shellDoneMsg struct {
	command    string
	incognito  bool
	output     string
	err        error
	generation uint64
}

func executeShellCommand(ctx context.Context, workingDir, command string, incognito bool) tea.Cmd {
	return executeShellCommandGeneration(ctx, workingDir, command, incognito, 0)
}

func executeShellCommandGeneration(ctx context.Context, workingDir, command string, incognito bool, generation uint64) tea.Cmd {
	return func() tea.Msg {
		commandContext, cancel := context.WithTimeout(ctx, shellCommandTimeout)
		defer cancel()
		var process *exec.Cmd
		if runtime.GOOS == "windows" {
			process = exec.CommandContext(commandContext, "cmd.exe", "/d", "/s", "/c", command)
		} else {
			shell := os.Getenv("SHELL")
			if shell == "" || !filepath.IsAbs(shell) {
				shell = "/bin/sh"
			}
			process = exec.CommandContext(commandContext, shell, "-c", command)
		}
		process.Dir = workingDir
		output, err := process.CombinedOutput()
		if len(output) > shellOutputLimit {
			output = append(output[:shellOutputLimit], []byte("\n... output truncated")...)
		}
		if commandContext.Err() != nil {
			err = commandContext.Err()
		}
		return shellDoneMsg{command: command, incognito: incognito, output: strings.TrimRight(string(output), "\r\n"), err: err, generation: generation}
	}
}

var slashCommandNames = buildSlashCompletionNames()

var legacySlashCommandDescriptions = map[string]string{"/skill:": "invoke a named skill"}

func buildSlashCompletionNames() []string {
	definitions := publicSlashCommandDefinitions()
	names := make([]string, 0, len(definitions)+1)
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return append(names, "/skill:")
}

func slashCompletionLabel(command string) string {
	return slashCompletionLabelWithGlyphs(command, unicodeUIGlyphs)
}

func slashCompletionLabelWithGlyphs(command string, glyphs uiGlyphs) string {
	description := legacySlashCommandDescriptions[command]
	if definition, exists := slashCommandDefinitionFor(command); exists {
		description = definition.Description
	}
	if description != "" {
		return command + " " + glyphs.Dash + " " + description
	}
	return command
}

func rankedCompletions(values []string, query string, maximum int) []string {
	query = strings.ToLower(query)
	type candidate struct {
		value string
		score int
	}
	candidates := make([]candidate, 0, len(values))
	for _, value := range values {
		lower := strings.ToLower(value)
		score := 3
		switch {
		case query == "":
			score = 2
		case strings.HasPrefix(lower, query):
			score = 0
		case strings.Contains(lower, query):
			score = 1
		case fuzzySubsequence(lower, query):
			score = 2
		default:
			continue
		}
		candidates = append(candidates, candidate{value: value, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].value < candidates[j].value
	})
	if len(candidates) > maximum {
		candidates = candidates[:maximum]
	}
	result := make([]string, len(candidates))
	for index := range candidates {
		result[index] = candidates[index].value
	}
	return result
}

func fuzzySubsequence(value, query string) bool {
	if query == "" {
		return true
	}
	queryRunes := []rune(query)
	index := 0
	for _, character := range value {
		if unicode.ToLower(character) == unicode.ToLower(queryRunes[index]) {
			index++
			if index == len(queryRunes) {
				return true
			}
		}
	}
	return false
}

func workspaceFiles(root string, maximum int) []string {
	values := make([]string, 0, min(maximum, 256))
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path != root && entry.IsDir() && (entry.Name() == ".git" || entry.Name() == ".dago_api" || entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr == nil {
			values = append(values, filepath.ToSlash(relative))
		}
		if len(values) >= maximum {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(values)
	return values
}

func parsePastedPaths(root, pasted string) []string {
	trimmed := strings.TrimSpace(pasted)
	if trimmed == "" || strings.ContainsRune(trimmed, '\x00') {
		return nil
	}
	parts := splitPathPaste(trimmed)
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(part, "file://") {
			parsed, err := url.Parse(part)
			if err != nil || parsed.Host != "" && parsed.Host != "localhost" {
				return nil
			}
			part, err = url.PathUnescape(parsed.Path)
			if err != nil {
				return nil
			}
		}
		if !filepath.IsAbs(part) {
			part = filepath.Join(root, part)
		}
		info, err := os.Stat(part)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, part)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			paths = append(paths, filepath.Clean(part))
		} else {
			paths = append(paths, filepath.ToSlash(relative))
		}
	}
	return paths
}

func splitPathPaste(value string) []string {
	var result []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() > 0 {
			result = append(result, current.String())
			current.Reset()
		}
	}
	for _, character := range value {
		switch {
		case quote != 0 && character == quote:
			quote = 0
		case quote == 0 && (character == '\'' || character == '"'):
			quote = character
		case quote == 0 && unicode.IsSpace(character):
			flush()
		default:
			current.WriteRune(character)
		}
	}
	if quote != 0 {
		return nil
	}
	flush()
	return result
}

func loadMediaInput(root, path string) (damessage.ContentBlock, bool) {
	extension := strings.ToLower(filepath.Ext(path))
	kind := damessage.BlockType("")
	switch extension {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic":
		kind = damessage.BlockImage
	case ".mp4", ".mov", ".webm", ".mkv":
		kind = damessage.BlockVideo
	}
	if kind == "" {
		return damessage.ContentBlock{}, false
	}
	fullPath := path
	if !filepath.IsAbs(fullPath) {
		fullPath = filepath.Join(root, filepath.FromSlash(fullPath))
	}
	file, err := os.Open(fullPath)
	if err != nil {
		return damessage.ContentBlock{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mediaInputLimit+1))
	if err != nil || len(data) == 0 || len(data) > mediaInputLimit || !validMediaSignature(kind, extension, data) {
		return damessage.ContentBlock{}, false
	}
	mimeType := mime.TypeByExtension(extension)
	if separator := strings.IndexByte(mimeType, ';'); separator >= 0 {
		mimeType = mimeType[:separator]
	}
	return damessage.ContentBlock{Type: kind, Data: data, MIMEType: mimeType, Name: filepath.Base(path)}, true
}

func validMediaSignature(kind damessage.BlockType, extension string, data []byte) bool {
	if kind == damessage.BlockImage {
		switch extension {
		case ".png":
			return len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n"
		case ".jpg", ".jpeg":
			return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
		case ".gif":
			return len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a")
		case ".webp":
			return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
		case ".heic":
			return len(data) >= 12 && string(data[4:8]) == "ftyp"
		}
	}
	if extension == ".webm" || extension == ".mkv" {
		return len(data) >= 4 && data[0] == 0x1a && data[1] == 0x45 && data[2] == 0xdf && data[3] == 0xa3
	}
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}
