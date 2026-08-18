package dacode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/semistrict/dago/dabackend"
	"github.com/semistrict/dago/dagent"
	"github.com/semistrict/dago/internal/unicodesecurity"
)

const (
	approvalCommandPreviewCharacters = 120
	approvalCommandPreviewLines      = 5
	approvalContentPreviewCharacters = 4_000
	approvalContentPreviewLines      = 40
)

type approvalOption struct {
	label string
	kind  string
}

type approvalToolRenderer func(*approvalState, map[string]any, json.RawMessage) []string

var approvalToolRenderers = map[string]approvalToolRenderer{
	"task": func(_ *approvalState, _ map[string]any, _ json.RawMessage) []string { return nil },
	"write_file": func(_ *approvalState, arguments map[string]any, _ json.RawMessage) []string {
		path, _ := arguments["file_path"].(string)
		if sensitiveApprovalPath(path) {
			return []string{"Write file: " + path, "Contents hidden - file may contain credentials"}
		}
		content, _ := arguments["content"].(string)
		return []string{"Write file: " + path, approvalContentPreview(content)}
	},
	"edit_file": func(_ *approvalState, arguments map[string]any, _ json.RawMessage) []string {
		path, _ := arguments["file_path"].(string)
		if sensitiveApprovalPath(path) {
			return []string{"Edit file: " + path, "Contents hidden - file may contain credentials"}
		}
		oldText, _ := arguments["old_string"].(string)
		newText, _ := arguments["new_string"].(string)
		return []string{"Edit file: " + path, approvalFragmentDiff(oldText, newText)}
	},
	"delete": func(_ *approvalState, arguments map[string]any, _ json.RawMessage) []string {
		path, _ := arguments["file_path"].(string)
		if path == "" {
			path, _ = arguments["path"].(string)
		}
		if sensitiveApprovalPath(path) {
			return []string{"Delete: " + path, "Contents hidden - file may contain credentials"}
		}
		return []string{"Delete: " + path}
	},
}

var sensitiveApprovalNames = map[string]struct{}{
	".envrc": {}, ".netrc": {}, "_netrc": {}, ".pgpass": {}, ".npmrc": {}, ".pypirc": {},
	".htpasswd": {}, ".git-credentials": {}, "credentials": {}, "credentials.json": {}, "token.json": {},
	"auth.json": {}, "id_rsa": {}, "id_dsa": {}, "id_ecdsa": {}, "id_ed25519": {},
}

var sensitiveApprovalSuffixes = []string{".pem", ".key", ".pfx", ".p12", ".keystore", ".jks"}

func approvalOptions(state *approvalState) []approvalOption {
	many := len(state.requests) > 1
	approve, reject := "Approve (y)", "Reject (n)"
	if many {
		approve = fmt.Sprintf("Approve all %d (y)", len(state.requests))
		reject = fmt.Sprintf("Reject all %d (n)", len(state.requests))
	}
	middle := approvalOption{label: "Enable Auto for this thread (a)", kind: "auto"}
	if state.autoFallback {
		middle = approvalOption{label: "Switch to Manual (a)", kind: "manual"}
	}
	return []approvalOption{{label: approve, kind: "approve"}, middle, {label: reject, kind: "reject"}}
}

func approvalRequestDetails(state *approvalState, request dagent.ApprovalRequest) []string {
	name := request.Call.Name
	arguments := approvalArgumentMap(request.Call.Arguments)
	if preview := state.previews[request.Call.ID]; len(preview) > 0 {
		return preview
	}
	if name == "execute" {
		command, _ := arguments["command"].(string)
		if command == "" {
			return approvalGenericArguments(arguments, request.Call.Arguments)
		}
		return []string{"$ " + approvalCommandPreview(command, state.commandExpanded)}
	}
	if renderer := approvalToolRenderers[name]; renderer != nil {
		return renderer(state, arguments, request.Call.Arguments)
	}
	return approvalGenericArguments(arguments, request.Call.Arguments)
}

func approvalWorkspacePreviews(ctx context.Context, root string, requests []dagent.ApprovalRequest) map[string][]string {
	previews := make(map[string][]string)
	files, err := dabackend.NewFilesystem(dabackend.FilesystemOptions{Root: root})
	if err != nil {
		return previews
	}
	for _, request := range requests {
		if request.Call.ID == "" || (request.Call.Name != "edit_file" && request.Call.Name != "delete") {
			continue
		}
		if preview := approvalWorkspacePreview(ctx, files, root, request); len(preview) > 0 {
			previews[request.Call.ID] = preview
		}
	}
	return previews
}

func approvalWorkspacePreview(ctx context.Context, files *dabackend.Filesystem, root string, request dagent.ApprovalRequest) []string {
	arguments := approvalArgumentMap(request.Call.Arguments)
	filePath, _ := arguments["file_path"].(string)
	if filePath == "" {
		filePath, _ = arguments["path"].(string)
	}
	if filePath == "" || sensitiveApprovalPath(filePath) {
		return nil
	}
	previewPath := filePath
	if filepath.IsAbs(filePath) {
		relative, err := filepath.Rel(root, filePath)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil
		}
		previewPath = "/" + filepath.ToSlash(relative)
	}
	read, err := files.ReadBinary(ctx, previewPath, approvalContentPreviewCharacters)
	if err != nil || read.Data == nil || read.Data.Encoding != dabackend.EncodingBase64 {
		return nil
	}
	content, err := base64.StdEncoding.DecodeString(read.Data.Content)
	if err != nil || !utf8.Valid(content) {
		return nil
	}
	before := string(content)
	switch request.Call.Name {
	case "delete":
		return []string{"Delete: " + filePath, approvalDeletionDiff(before)}
	case "edit_file":
		oldText, oldOK := arguments["old_string"].(string)
		newText, newOK := arguments["new_string"].(string)
		if !oldOK || !newOK || oldText == "" || !strings.Contains(before, oldText) {
			return nil
		}
		count := 1
		if replaceAll, _ := arguments["replace_all"].(bool); replaceAll {
			count = -1
		}
		after := strings.Replace(before, oldText, newText, count)
		return []string{"Edit file: " + filePath, approvalFocusedDiff(before, after)}
	default:
		return nil
	}
}

func sensitiveApprovalPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsRune(value, '\x00') {
		return strings.ContainsRune(value, '\x00')
	}
	name := strings.ToLower(path.Base(filepath.ToSlash(value)))
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return true
	}
	if _, sensitive := sensitiveApprovalNames[name]; sensitive {
		return true
	}
	for _, suffix := range sensitiveApprovalSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func approvalArgumentMap(raw json.RawMessage) map[string]any {
	var arguments map[string]any
	if json.Unmarshal(raw, &arguments) != nil || arguments == nil {
		return map[string]any{}
	}
	return arguments
}

func approvalGenericArguments(arguments map[string]any, raw json.RawMessage) []string {
	if len(arguments) == 0 {
		if text := strings.TrimSpace(string(raw)); text != "" && text != "{}" {
			return []string{truncate(text, approvalContentPreviewCharacters)}
		}
		return nil
	}
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		encoded, err := json.Marshal(arguments[key])
		if err != nil {
			encoded = []byte(fmt.Sprint(arguments[key]))
		}
		lines = append(lines, key+": "+truncate(string(encoded), approvalContentPreviewCharacters))
	}
	return lines
}

func approvalCommandPreview(command string, expanded bool) string {
	command = unicodesecurity.RenderTerminalSafe(command)
	if expanded || !approvalCommandExpandable(command) {
		return command
	}
	lines := strings.Split(command, "\n")
	if len(lines) > approvalCommandPreviewLines {
		lines = lines[:approvalCommandPreviewLines]
	}
	preview := strings.Join(lines, "\n")
	if len([]rune(preview)) > approvalCommandPreviewCharacters {
		preview = string([]rune(preview)[:approvalCommandPreviewCharacters])
	}
	return preview + "... (press e to expand)"
}

func approvalCommandExpandable(command string) bool {
	return len([]rune(command)) > approvalCommandPreviewCharacters || strings.Count(command, "\n")+1 > approvalCommandPreviewLines
}

func approvalContentPreview(content string) string {
	content = unicodesecurity.RenderTerminalSafe(content)
	lines := strings.Split(content, "\n")
	truncated := false
	if len(lines) > approvalContentPreviewLines {
		lines = lines[:approvalContentPreviewLines]
		truncated = true
	}
	content = strings.Join(lines, "\n")
	if len([]rune(content)) > approvalContentPreviewCharacters {
		content = string([]rune(content)[:approvalContentPreviewCharacters])
		truncated = true
	}
	if truncated {
		content += "..."
	}
	return content
}

func approvalFragmentDiff(oldText, newText string) string {
	oldLines := strings.Split(approvalContentPreview(oldText), "\n")
	newLines := strings.Split(approvalContentPreview(newText), "\n")
	lines := []string{"--- before", "+++ after"}
	for _, line := range oldLines {
		lines = append(lines, "-"+line)
	}
	for _, line := range newLines {
		lines = append(lines, "+"+line)
	}
	return strings.Join(lines, "\n")
}

func approvalDeletionDiff(before string) string {
	lines := []string{"--- before", "+++ /dev/null"}
	for _, line := range strings.Split(approvalContentPreview(before), "\n") {
		lines = append(lines, "-"+line)
	}
	return strings.Join(lines, "\n")
}

func approvalFocusedDiff(before, after string) string {
	oldLines := strings.Split(approvalContentPreview(before), "\n")
	newLines := strings.Split(approvalContentPreview(after), "\n")
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	oldChanged := oldLines[prefix : len(oldLines)-suffix]
	newChanged := newLines[prefix : len(newLines)-suffix]
	lines := []string{
		"--- before",
		"+++ after",
		fmt.Sprintf("@@ -%d,%d +%d,%d @@", prefix+1, len(oldChanged), prefix+1, len(newChanged)),
	}
	for _, line := range oldChanged {
		lines = append(lines, "-"+line)
	}
	for _, line := range newChanged {
		lines = append(lines, "+"+line)
	}
	return strings.Join(lines, "\n")
}

func renderApprovalDetails(state *approvalState, width int) []string {
	lines := make([]string, 0, len(state.requests)*3)
	for index, request := range state.requests {
		if len(state.requests) > 1 {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorBody).Bold(true).Render(
				fmt.Sprintf("%d. %s", index+1, unicodesecurity.RenderTerminalSafe(request.Call.Name))))
		}
		if request.Description != "" {
			lines = append(lines, lipgloss.NewStyle().Foreground(colorMuted).Render(
				unicodesecurity.RenderTerminalSafe(request.Description)))
		}
		for _, detail := range approvalRequestDetails(state, request) {
			if strings.TrimSpace(detail) == "" {
				continue
			}
			lines = append(lines, renderApprovalDetail(detail, width, request.Call.Name == "edit_file" || request.Call.Name == "delete")...)
		}
	}
	return lines
}

func renderApprovalDetail(detail string, width int, diff bool) []string {
	detail = unicodesecurity.RenderTerminalSafe(detail)
	if !diff {
		return []string{lipgloss.NewStyle().Foreground(colorMuted).Width(max(width-4, 12)).Render(detail)}
	}
	lines := strings.Split(detail, "\n")
	rendered := make([]string, 0, len(lines))
	for _, line := range lines {
		color := colorMuted
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			color = colorSuccess
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			color = colorError
		case strings.HasPrefix(line, "@@"):
			color = colorWarning
		}
		rendered = append(rendered, lipgloss.NewStyle().Foreground(color).Width(max(width-4, 12)).Render(line))
	}
	return rendered
}
