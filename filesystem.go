package dago

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/dago/agent"
	"github.com/semistrict/dago/backend"
	"github.com/semistrict/dago/message"
	"github.com/semistrict/dago/tool"
)

type FilesystemOperation string

const (
	FilesystemRead  FilesystemOperation = "read"
	FilesystemWrite FilesystemOperation = "write"
)

type PermissionMode string

const (
	PermissionAllow     PermissionMode = "allow"
	PermissionDeny      PermissionMode = "deny"
	PermissionInterrupt PermissionMode = "interrupt"
	// PermissionAsk is retained as a compatibility spelling for interrupt.
	PermissionAsk PermissionMode = "ask"
)

// FilesystemPermission is evaluated in declaration order; the first matching rule
// wins. Unmatched operations are allowed.
type FilesystemPermission struct {
	Operations []FilesystemOperation
	Paths      []string
	Mode       PermissionMode
}

type FilesystemOptions struct {
	Backend           backend.Backend
	Permissions       []FilesystemPermission
	Tools             []string
	ToolDescriptions  map[string]string
	ReadLimit         int
	GrepLimit         int
	MaxExecuteTimeout int
	LargeResultBytes  int
	ArtifactsRoot     string
	VideoExtractor    VideoExtractor
	MaxVideoBytes     int
	VideoSamplingRate float64
}

var filesystemToolOperations = map[string]FilesystemOperation{
	"ls": FilesystemRead, "read_file": FilesystemRead, "glob": FilesystemRead, "grep": FilesystemRead,
	"write_file": FilesystemWrite, "edit_file": FilesystemWrite, "delete": FilesystemWrite,
}

const FilesystemListDescription = `Lists all files in a directory.

This is useful for exploring the filesystem and finding the right file to read or edit.
You should almost always use this tool before using read_file or edit_file.`

const filesystemReadDescriptionTemplate = `Reads a file from the filesystem. Assume any path the user provides is valid; reading a missing file returns an error.

Usage:
- %s Use offset and limit to page through large files instead of reading them whole.
- Results include source line numbers, then two spaces, then the source line. Never include these prefixes when editing.
- Lines over 5,000 characters use continuation markers such as 5.1; limit counts source lines, not continuation rows.
- Batch independent read_file calls when several files may be useful.
- An empty file returns a system reminder in place of contents.
- Large tool results may be offloaded to a file; read the path from the tool result with pagination.
- Images, audio, video, and PDFs return multimodal content blocks.
%s
- Always read a file before editing it.`

var FilesystemReadDescription = fmt.Sprintf(filesystemReadDescriptionTemplate,
	"By default, it reads up to 100 lines starting from the beginning of the file.",
	"- For images and PDFs, omit offset and limit.",
)

var FilesystemReadVideoDescription = fmt.Sprintf(filesystemReadDescriptionTemplate,
	"For text files, by default it reads up to 100 lines starting from the beginning of the file.",
	"- For images and PDFs, omit offset and limit.\n- For videos, offset and limit are seconds; the default window is 100 seconds.",
)

const FilesystemWriteDescription = `Writes content to a file. Creates the file if it does not exist; replaces it entirely if it does.

Use this tool to create a new file or replace a whole file. Prefer edit_file for an existing file when possible.`

const FilesystemEditDescription = `Performs exact string replacements in files.

Read the file before editing. Preserve exact indentation and omit line-number prefixes. The old text must be unique unless replace_all is true.`

const FilesystemDeleteDescription = `Deletes a file or directory from the filesystem.

Directories are removed recursively. This cannot be undone, so only delete paths that are no longer needed.`

const FilesystemGlobDescription = `Find files matching a glob pattern and return absolute paths.

Supports * for characters, ** for directories, and ? for one character.`

func filesystemGrepDescription(includeExecute bool) string {
	description := `Search for a literal text pattern across files, not a regular expression.

Regular-expression metacharacters are ordinary characters. Run separate searches to match several strings.`
	if includeExecute {
		description += " If regular expressions are required, use execute with rg."
	}
	return description + "\n\nResults can be paths, matching content, or counts. Search the artifacts root for offloaded large tool results when the exact path is unknown."
}

func filesystemExecuteDescription(includeGrep, includeGlob bool) string {
	description := `Executes a shell command in an explicitly configured sandbox and returns combined output with the exit code.

Quote paths containing spaces, use absolute paths where practical, and use read_file instead of cat, head, or tail.`
	if includeGrep && includeGlob {
		description += " Use grep and glob instead of shell search commands."
	} else if includeGrep {
		description += " Use grep instead of shell text-search commands."
	} else if includeGlob {
		description += " Use glob instead of shell file-search commands."
	}
	return description
}

func describeFilesystemTools(values []tool.Tool, custom map[string]string) []tool.Tool {
	visible := map[string]bool{}
	for _, value := range values {
		visible[value.Definition().Name] = true
	}
	descriptions := map[string]string{}
	for name, description := range custom {
		if strings.TrimSpace(description) != "" {
			descriptions[name] = description
		}
	}
	if descriptions["grep"] == "" {
		descriptions["grep"] = filesystemGrepDescription(visible["execute"])
	}
	if descriptions["execute"] == "" {
		descriptions["execute"] = filesystemExecuteDescription(visible["grep"], visible["glob"])
	}
	return applyToolProfile(values, descriptions, nil)
}

// FilesystemMiddleware constructs the standard file tools and permission boundary.
func FilesystemMiddleware(options FilesystemOptions) (agent.Middleware, error) {
	if options.Backend == nil {
		return agent.Middleware{}, fmt.Errorf("filesystem backend is required")
	}
	if options.ReadLimit <= 0 {
		options.ReadLimit = 100
	}
	if options.GrepLimit <= 0 {
		options.GrepLimit = 1000
	}
	if options.MaxExecuteTimeout <= 0 {
		options.MaxExecuteTimeout = 3600
	}
	if options.LargeResultBytes <= 0 {
		options.LargeResultBytes = 20_000
	}
	if options.ArtifactsRoot == "" {
		options.ArtifactsRoot = "/large_tool_results"
	}
	if options.MaxVideoBytes <= 0 {
		options.MaxVideoBytes = DefaultMaxVideoInputBytes
	}
	if options.VideoSamplingRate <= 0 {
		options.VideoSamplingRate = DefaultVideoSamplingRate
	}
	if err := validatePermissions(options.Permissions); err != nil {
		return agent.Middleware{}, err
	}
	available := makeFilesystemTools(options)
	selected := []tool.Tool{}
	if options.Tools == nil {
		options.Tools = []string{"ls", "read_file", "write_file", "edit_file", "delete", "glob", "grep", "execute"}
	}
	seen := map[string]bool{}
	for _, name := range options.Tools {
		if seen[name] {
			return agent.Middleware{}, fmt.Errorf("duplicate filesystem tool %q", name)
		}
		seen[name] = true
		executable := available[name]
		if executable == nil {
			if name == "execute" {
				continue
			}
			return agent.Middleware{}, fmt.Errorf("unknown filesystem tool %q", name)
		}
		if name == "execute" && len(options.Permissions) > 0 {
			return agent.Middleware{}, fmt.Errorf("filesystem permissions cannot constrain execute; configure an isolated sandbox or omit execute")
		}
		selected = append(selected, executable)
	}
	selected = describeFilesystemTools(selected, options.ToolDescriptions)
	middleware := agent.Middleware{Name: "filesystem", Tools: selected}
	if fields := backend.RuntimeStateFields(options.Backend); len(fields) > 0 {
		middleware.Fields = make(map[string]agent.StateField, len(fields))
		for _, field := range fields {
			if _, duplicate := middleware.Fields[field.Key]; duplicate {
				return agent.Middleware{}, fmt.Errorf("duplicate backend state field %q", field.Key)
			}
			middleware.Fields[field.Key] = agent.StateField{
				Kind: agent.FieldDelta, Contract: field.Contract,
				SnapshotFrequency: field.SnapshotFrequency, Initial: field.Initial,
				Reduce: field.Reduce, Clone: field.Clone,
			}
		}
	}
	middleware.BeforeTools = filesystemApprovalHook(options.Backend, options.Permissions)
	middleware.WrapToolCall = filesystemPermissionWrapper(options)
	middleware.WrapModelCall = filesystemDescriptionWrapper()
	return middleware, nil
}

func filesystemDescriptionWrapper() agent.ModelWrapper {
	return func(ctx context.Context, request agent.ModelRequest, next agent.ModelHandler) (agent.ModelResponse, error) {
		visible := map[string]bool{}
		for _, executable := range request.Tools {
			visible[executable.Definition().Name] = true
		}
		descriptions := map[string]string{}
		for _, executable := range request.Tools {
			definition := executable.Definition()
			switch definition.Name {
			case "grep":
				if definition.Description == filesystemGrepDescription(true) || definition.Description == filesystemGrepDescription(false) {
					descriptions[definition.Name] = filesystemGrepDescription(visible["execute"])
				}
			case "execute":
				isDefault := false
				for _, grep := range []bool{false, true} {
					for _, glob := range []bool{false, true} {
						isDefault = isDefault || definition.Description == filesystemExecuteDescription(grep, glob)
					}
				}
				if isDefault {
					descriptions[definition.Name] = filesystemExecuteDescription(visible["grep"], visible["glob"])
				}
			}
		}
		request.Tools = applyToolProfile(request.Tools, descriptions, nil)
		return next(ctx, request)
	}
}

func validatePermissions(rules []FilesystemPermission) error {
	for ruleIndex, rule := range rules {
		if rule.Mode == "" {
			rule.Mode = PermissionAllow
		}
		if rule.Mode != PermissionAllow && rule.Mode != PermissionDeny && rule.Mode != PermissionInterrupt && rule.Mode != PermissionAsk {
			return fmt.Errorf("filesystem permission %d has invalid mode %q", ruleIndex, rule.Mode)
		}
		for _, operation := range rule.Operations {
			if operation != FilesystemRead && operation != FilesystemWrite {
				return fmt.Errorf("filesystem permission %d has invalid operation %q", ruleIndex, operation)
			}
		}
		for _, pattern := range rule.Paths {
			if !strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") || strings.Contains(pattern, "~") {
				return fmt.Errorf("filesystem permission path %q must be absolute and contain no traversal", pattern)
			}
			if _, err := path.Match(strings.ReplaceAll(pattern, "**", "*"), "/probe"); err != nil {
				return fmt.Errorf("filesystem permission path %q: %w", pattern, err)
			}
		}
	}
	return nil
}

func makeFilesystemTools(options FilesystemOptions) map[string]tool.Tool {
	values := map[string]tool.Tool{}
	values["ls"] = tool.Func{Spec: tool.Definition{Name: "ls", Description: FilesystemListDescription, InputSchema: schema(`{"path":{"type":"string","description":"Absolute path to the directory to list. Must be absolute, not relative."}}`, "path")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		result, err := options.Backend.List(ctx, input.Path)
		if err != nil {
			return tool.Result{}, err
		}
		result.Entries = filterFileInfo(options.Permissions, FilesystemRead, result.Entries)
		lines := make([]string, len(result.Entries))
		for i, item := range result.Entries {
			lines[i] = item.Path
		}
		return tool.TextResult(strings.Join(lines, "\n")), nil
	}}
	readDescription := FilesystemReadDescription
	offsetDescription := "Line number to start reading from (0-indexed). Use for pagination of large files."
	limitDescription := "Maximum number of lines to read. Use for pagination of large files."
	if options.VideoExtractor != nil {
		readDescription = FilesystemReadVideoDescription
		offsetDescription = "Line number to start reading from for text files (0-indexed). For videos, seconds into the source to start sampling."
		limitDescription = "Maximum number of lines to read for text files. For videos, seconds of source to sample."
	}
	readSchema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"file_path": map[string]any{"type": "string", "description": "Absolute path to the file to read. Must be absolute, not relative."},
		"offset":    map[string]any{"type": "integer", "default": 0, "description": offsetDescription},
		"limit":     map[string]any{"type": "integer", "default": 100, "description": limitDescription},
	}, "required": []string{"file_path"}})
	values["read_file"] = tool.Func{Spec: tool.Definition{Name: "read_file", Description: readDescription, InputSchema: readSchema}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			FilePath string `json:"file_path"`
			Offset   int    `json:"offset"`
			Limit    *int   `json:"limit"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		limit := options.ReadLimit
		if input.Limit != nil {
			limit = *input.Limit
		}
		video := options.VideoExtractor != nil && isVideoPath(input.FilePath)
		if video && limit <= 0 {
			return tool.Result{}, fmt.Errorf("error reading video %s: limit must be > 0, got %d", input.FilePath, limit)
		}
		result, err := options.Backend.Read(ctx, input.FilePath, input.Offset, limit)
		if err != nil {
			return tool.Result{}, err
		}
		if result.Data == nil {
			return tool.Result{}, fmt.Errorf("read %q returned no data", input.FilePath)
		}
		if result.NoLinesRequested {
			return tool.TextResult(fmt.Sprintf("System reminder: no lines were read because `limit` was %d. The file was not inspected and may have contents; retry with `limit` >= 1 to read it.", limit)), nil
		}
		if result.Data.Encoding == backend.EncodingBase64 {
			if video {
				return videoResult(ctx, options, input.FilePath, result.Data, input.Offset, limit)
			}
			return mediaResult(ctx, options.Backend, input.FilePath, result.Data)
		}
		if strings.TrimSpace(result.Data.Content) == "" {
			return tool.TextResult("System reminder: File exists but has empty contents"), nil
		}
		start := max(input.Offset, 0) + 1
		if result.StartLine != nil {
			start = *result.StartLine
		}
		text := numberLines(result.Data.Content, start)
		text += readPaginationNotice(result)
		if input.Offset < 0 {
			text += fmt.Sprintf("\n\n[Requested offset %d is before the start of the file; read from line 1 instead.]", input.Offset)
		}
		return tool.TextResult(text), nil
	}}
	values["write_file"] = tool.Func{Spec: tool.Definition{Name: "write_file", Description: FilesystemWriteDescription, InputSchema: schema(`{"file_path":{"type":"string","description":"Absolute path where the file should be written. Must be absolute, not relative."},"content":{"type":"string","description":"The text content to write to the file. This parameter is required."}}`, "file_path", "content")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		result, err := options.Backend.Write(ctx, input.FilePath, input.Content)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.TextResult("Wrote " + result.Path), nil
	}}
	values["edit_file"] = tool.Func{Spec: tool.Definition{Name: "edit_file", Description: FilesystemEditDescription, InputSchema: schema(`{"file_path":{"type":"string","description":"Absolute path to the file to edit. Must be absolute, not relative."},"old_string":{"type":"string","description":"The exact text to find and replace. Must be unique in the file unless replace_all is true."},"new_string":{"type":"string","description":"The text to replace old_string with. Must be different from old_string."},"replace_all":{"type":"boolean","default":false,"description":"If true, replace all occurrences of old_string. If false, old_string must be unique."}}`, "file_path", "old_string", "new_string")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			FilePath string `json:"file_path"`
			Old      string `json:"old_string"`
			New      string `json:"new_string"`
			All      bool   `json:"replace_all"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		result, err := options.Backend.Edit(ctx, input.FilePath, input.Old, input.New, input.All)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.TextResult(fmt.Sprintf("Edited %s (%d replacement(s)).", result.Path, result.Occurrences)), nil
	}}
	values["delete"] = tool.Func{Spec: tool.Definition{Name: "delete", Description: FilesystemDeleteDescription, InputSchema: schema(`{"file_path":{"type":"string","description":"Absolute path to the file or directory to delete. Must be absolute, not relative."}}`, "file_path")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			FilePath string `json:"file_path"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		result, err := options.Backend.Delete(ctx, input.FilePath)
		if err != nil {
			return tool.Result{}, err
		}
		return tool.TextResult("Deleted " + result.Path), nil
	}}
	values["glob"] = tool.Func{Spec: tool.Definition{Name: "glob", Description: FilesystemGlobDescription, InputSchema: schema(`{"pattern":{"type":"string","description":"Glob pattern to match files, such as **/*.go, *.txt, or /subdir/**/*.md."},"path":{"anyOf":[{"type":"string"},{"type":"null"}],"default":null,"description":"Base directory to search from. Defaults to the backend's default root."}}`, "pattern")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		result, err := options.Backend.Glob(ctx, input.Pattern, input.Path)
		if err != nil {
			return tool.Result{}, err
		}
		result.Matches = filterFileInfo(options.Permissions, FilesystemRead, result.Matches)
		lines := make([]string, len(result.Matches))
		for i, item := range result.Matches {
			lines[i] = item.Path
		}
		text := strings.Join(lines, "\n")
		if result.Truncated {
			text += "\nResults truncated; narrow the pattern or path."
		}
		return tool.TextResult(text), nil
	}}
	values["grep"] = tool.Func{Spec: tool.Definition{Name: "grep", Description: filesystemGrepDescription(true), InputSchema: schema(`{"pattern":{"type":"string","description":"Text pattern to search for (literal string, not regex)."},"path":{"anyOf":[{"type":"string"},{"type":"null"}],"default":null,"description":"Directory to search in. Defaults to the backend's default root."},"glob":{"anyOf":[{"type":"string"},{"type":"null"}],"default":null,"description":"Glob pattern (not regex) limiting which files are searched. A pattern without / matches file names at any depth; a pattern containing / matches paths relative to the search root."},"output_mode":{"type":"string","enum":["files_with_matches","content","count"],"default":"files_with_matches","description":"Shape of the result: matching paths, matching lines grouped by file, or match counts by file."},"max_count":{"anyOf":[{"type":"integer","minimum":1},{"type":"null"}],"default":null,"description":"Optional cap on matches across all files. Leave unset to use the configured default."}}`, "pattern")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
		var input struct {
			Pattern    string `json:"pattern"`
			Path       string `json:"path"`
			Glob       string `json:"glob"`
			OutputMode string `json:"output_mode"`
			MaxCount   int    `json:"max_count"`
		}
		if err := decodeArguments(raw, &input); err != nil {
			return tool.Result{}, err
		}
		if input.MaxCount == 0 {
			input.MaxCount = options.GrepLimit
		}
		result, err := options.Backend.Grep(ctx, input.Pattern, backend.GrepOptions{Path: input.Path, Glob: input.Glob, MaxCount: input.MaxCount})
		if err != nil {
			return tool.Result{}, err
		}
		result.Matches = filterGrepMatches(options.Permissions, FilesystemRead, result.Matches)
		text := formatGrep(result, input.OutputMode)
		return tool.TextResult(text), nil
	}}
	if sandbox, ok := options.Backend.(backend.Sandbox); ok {
		executeSchema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "Shell command to execute in the sandbox environment."},
			"timeout": map[string]any{"anyOf": []any{map[string]any{"type": "integer", "minimum": 0, "maximum": options.MaxExecuteTimeout}, map[string]any{"type": "null"}}, "default": nil, "description": fmt.Sprintf("Optional timeout in seconds, capped at %d. Zero uses the backend's default timeout.", options.MaxExecuteTimeout)},
		}, "required": []string{"command"}})
		values["execute"] = tool.Func{Spec: tool.Definition{Name: "execute", Description: filesystemExecuteDescription(true, true), InputSchema: executeSchema}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
			var input struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := decodeArguments(raw, &input); err != nil {
				return tool.Result{}, err
			}
			if input.Timeout > options.MaxExecuteTimeout {
				return tool.Result{}, fmt.Errorf("execute timeout %d exceeds maximum %d", input.Timeout, options.MaxExecuteTimeout)
			}
			result, err := sandbox.Execute(ctx, input.Command, time.Duration(input.Timeout)*time.Second)
			if err != nil {
				return tool.Result{}, err
			}
			artifact, _ := json.Marshal(map[string]any{"exit_code": result.ExitCode, "truncated": result.Truncated})
			text := result.Output
			if result.ExitCode != nil {
				status := "succeeded"
				if *result.ExitCode != 0 {
					status = "failed"
				}
				text += fmt.Sprintf("\n[Command %s with exit code %d]", status, *result.ExitCode)
			}
			if result.Truncated {
				text += "\n[Output was truncated because it exceeded the capture size limit]"
			}
			return tool.Result{Content: []message.ContentBlock{{Type: message.BlockText, Text: text}}, Artifact: artifact}, nil
		}}
	}
	return values
}

func filesystemPermissionWrapper(options FilesystemOptions) agent.ToolWrapper {
	return func(ctx context.Context, request agent.ToolCallRequest, next agent.ToolHandler) (agent.ToolCallResponse, error) {
		boundCtx, err := backend.BindRuntime(ctx, options.Backend, request.State)
		if err != nil {
			return agent.ToolCallResponse{}, err
		}
		ctx = boundCtx
		operation, known := filesystemToolOperations[request.Call.Name]
		if known {
			target := filesystemCallPath(request.Call)
			if request.Call.Name == "delete" {
				hasDescendants := deleteTargetMayHaveDescendants(ctx, options.Backend, target, len(options.Permissions) > 0)
				if patterns := findDeletePatterns(options.Permissions, target, hasDescendants, PermissionDeny); len(patterns) > 0 {
					return agent.ToolCallResponse{}, fmt.Errorf("permission denied for %s on %s by %s", operation, target, strings.Join(patterns, ", "))
				}
			} else if permissionDecision(options.Permissions, operation, target) == PermissionDeny {
				return agent.ToolCallResponse{}, fmt.Errorf("permission denied for %s on %s", operation, target)
			}
		}
		response, err := next(ctx, request)
		if err != nil {
			return response, err
		}
		if request.Call.Name != "ls" && request.Call.Name != "glob" && request.Call.Name != "grep" && request.Call.Name != "read_file" && len(response.Result.Content) > 0 {
			total := 0
			for _, block := range response.Result.Content {
				total += len(block.Text)
			}
			if total > options.LargeResultBytes {
				var combined strings.Builder
				for _, block := range response.Result.Content {
					combined.WriteString(block.Text)
				}
				artifactPath := path.Join(options.ArtifactsRoot, request.Call.ID+".txt")
				if _, writeErr := options.Backend.Write(ctx, artifactPath, combined.String()); writeErr == nil {
					preview := previewText(combined.String(), 2000)
					response.Result.Content = []message.ContentBlock{{Type: message.BlockText, Text: fmt.Sprintf("Result saved to %s. Preview:\n%s", artifactPath, preview)}}
				}
			}
		}
		updates := backend.RuntimeUpdates(ctx, options.Backend)
		if len(updates) > 0 && response.Result.Update == nil {
			response.Result.Update = map[string]any{}
		}
		for key, value := range updates {
			if _, exists := response.Result.Update[key]; exists {
				return agent.ToolCallResponse{}, fmt.Errorf("filesystem backend produced conflicting state update %q", key)
			}
			response.Result.Update[key] = value
		}
		return response, nil
	}
}

func filesystemApprovalHook(value backend.Backend, rules []FilesystemPermission) agent.ToolBatchHook {
	return func(ctx context.Context, request agent.ToolBatchRequest) (agent.ToolBatchResponse, error) {
		boundCtx, err := backend.BindRuntime(ctx, value, request.State)
		if err != nil {
			return agent.ToolBatchResponse{}, err
		}
		ctx = boundCtx
		var pending []agent.ApprovalRequest
		gated := map[string]bool{}
		for _, call := range request.Calls {
			operation, known := filesystemToolOperations[call.Name]
			if !known {
				continue
			}
			target := filesystemCallPath(call)
			decision := permissionDecision(rules, operation, target)
			if call.Name == "delete" {
				hasDescendants := deleteTargetMayHaveDescendants(ctx, value, target, len(rules) > 0)
				if len(findDeletePatterns(rules, target, hasDescendants, PermissionInterrupt)) > 0 {
					decision = PermissionInterrupt
				}
			}
			if decision == PermissionInterrupt {
				pending = append(pending, agent.ApprovalRequest{Call: call, Description: fmt.Sprintf("Allow %s access to %s?", operation, target)})
				gated[call.ID] = true
			}
		}
		if len(pending) == 0 {
			return agent.ToolBatchResponse{}, nil
		}
		if request.Runtime.Resume == nil {
			return agent.ToolBatchResponse{Interrupt: &agent.Interrupt{ID: "filesystem_approval", Value: pending}}, nil
		}
		resume, ok := request.Runtime.Resume.(agent.ApprovalResponse)
		if !ok {
			return agent.ToolBatchResponse{}, fmt.Errorf("filesystem approval resume has type %T", request.Runtime.Resume)
		}
		calls := make([]message.ToolCall, 0, len(request.Calls))
		var rejected []message.Message
		for _, call := range request.Calls {
			if !gated[call.ID] {
				calls = append(calls, call)
				continue
			}
			choice, exists := resume.Decisions[call.ID]
			if !exists {
				return agent.ToolBatchResponse{}, fmt.Errorf("filesystem approval missing call %q", call.ID)
			}
			switch choice.Decision {
			case agent.ApprovalApprove:
				calls = append(calls, call)
			case agent.ApprovalEdit:
				if choice.Call == nil || choice.Call.ID != call.ID || !json.Valid(choice.Call.Arguments) {
					return agent.ToolBatchResponse{}, fmt.Errorf("invalid filesystem approval edit for %q", call.ID)
				}
				calls = append(calls, *choice.Call)
			case agent.ApprovalReject:
				text := choice.Reason
				if text == "" {
					text = "Filesystem operation rejected."
				}
				item := message.Tool(call.ID, text)
				item.Name = call.Name
				item.ToolStatus = message.ToolStatusError
				rejected = append(rejected, item)
			default:
				return agent.ToolBatchResponse{}, fmt.Errorf("invalid filesystem approval decision %q", choice.Decision)
			}
		}
		return agent.ToolBatchResponse{Calls: calls, Messages: rejected, ResumeConsumed: true}, nil
	}
}

func permissionDecision(rules []FilesystemPermission, operation FilesystemOperation, target string) PermissionMode {
	for _, rule := range rules {
		applies := false
		for _, item := range rule.Operations {
			if item == operation {
				applies = true
				break
			}
		}
		if !applies {
			continue
		}
		for _, pattern := range rule.Paths {
			if globMatch(pattern, target) {
				if rule.Mode == "" {
					return PermissionAllow
				}
				return normalizedMode(rule.Mode)
			}
		}
	}
	return PermissionAllow
}

func filesystemCallPath(call message.ToolCall) string {
	var values map[string]any
	if json.Unmarshal(call.Arguments, &values) != nil {
		return "/"
	}
	for _, key := range []string{"file_path", "path"} {
		if value, ok := values[key].(string); ok && value != "" {
			if !strings.HasPrefix(value, "/") {
				return "/" + value
			}
			return path.Clean(value)
		}
	}
	return "/"
}

func globMatch(pattern, value string) bool {
	pattern = normalizeMatchPath(pattern)
	value = normalizeMatchPath(value)
	for _, expanded := range expandBraces(pattern) {
		if matchGlobSegments(splitMatchPath(expanded), splitMatchPath(value)) {
			return true
		}
	}
	return false
}

func normalizeMatchPath(value string) string {
	if value == "" {
		return "/"
	}
	return path.Clean("/" + strings.TrimPrefix(strings.ReplaceAll(value, "\\", "/"), "/"))
}

func splitMatchPath(value string) []string {
	trimmed := strings.Trim(value, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func matchGlobSegments(pattern, value []string) bool {
	if len(pattern) == 0 {
		return len(value) == 0
	}
	if pattern[0] == "**" {
		return matchGlobSegments(pattern[1:], value) || len(value) > 0 && matchGlobSegments(pattern, value[1:])
	}
	if len(value) == 0 {
		return false
	}
	matched, err := path.Match(strings.ReplaceAll(pattern[0], "**", "*"), value[0])
	return err == nil && matched && matchGlobSegments(pattern[1:], value[1:])
}

func expandBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	endOffset := strings.IndexByte(pattern[start+1:], '}')
	if endOffset < 0 {
		return []string{pattern}
	}
	end := start + 1 + endOffset
	choices := strings.Split(pattern[start+1:end], ",")
	if len(choices) < 2 {
		return []string{pattern}
	}
	var result []string
	for _, choice := range choices {
		result = append(result, expandBraces(pattern[:start]+choice+pattern[end+1:])...)
	}
	return result
}

func deleteTargetMayHaveDescendants(ctx context.Context, value backend.Backend, target string, configured bool) bool {
	if !configured || value == nil {
		return false
	}
	listing, err := value.List(ctx, target)
	if err == nil && len(listing.Entries) > 0 {
		return true
	}
	parent := path.Dir(target)
	parentListing, parentErr := value.List(ctx, parent)
	if parentErr != nil {
		return true
	}
	cleanTarget := strings.TrimSuffix(normalizeMatchPath(target), "/")
	for _, entry := range parentListing.Entries {
		if strings.TrimSuffix(normalizeMatchPath(entry.Path), "/") == cleanTarget {
			return entry.IsDir
		}
	}
	return err == nil
}

func findDeletePatterns(rules []FilesystemPermission, target string, hasDescendants bool, mode PermissionMode) []string {
	target = normalizeMatchPath(target)
	if !hasDescendants {
		for _, rule := range rules {
			if !hasOperation(rule, FilesystemWrite) {
				continue
			}
			var matched []string
			for _, pattern := range rule.Paths {
				if globMatch(pattern, target) {
					matched = append(matched, pattern)
				}
			}
			if len(matched) > 0 {
				if normalizedMode(rule.Mode) == mode {
					return matched
				}
				return nil
			}
		}
		return nil
	}
	seen := map[string]bool{}
	var result []string
	for _, rule := range rules {
		if normalizedMode(rule.Mode) != mode || !hasOperation(rule, FilesystemWrite) {
			continue
		}
		for _, pattern := range rule.Paths {
			if seen[pattern] || !deletePatternOverlaps(pattern, target) {
				continue
			}
			seen[pattern] = true
			result = append(result, pattern)
		}
	}
	return result
}

func deletePatternOverlaps(pattern, target string) bool {
	pattern = normalizeMatchPath(pattern)
	anchor, wildcard := globAnchor(pattern)
	if !wildcard {
		return pathsOverlap(anchor, target)
	}
	if anchor == "/" || globMatch(pattern, target) || isPathWithin(anchor, target) {
		return true
	}
	if !isPathWithin(target, anchor) {
		return false
	}
	anchorSegments := splitMatchPath(anchor)
	patternSegments := splitMatchPath(pattern)
	suffix := patternSegments[len(anchorSegments):]
	if len(suffix) != 1 || strings.Contains(suffix[0], "**") {
		return true
	}
	targetSegments := splitMatchPath(target)
	for depth := len(anchorSegments); depth < len(targetSegments); depth++ {
		candidate := "/" + strings.Join(targetSegments[:depth], "/")
		if globMatch(pattern, candidate) {
			return true
		}
	}
	return false
}

func globAnchor(pattern string) (string, bool) {
	segments := splitMatchPath(pattern)
	anchor := make([]string, 0, len(segments))
	for _, segment := range segments {
		if strings.ContainsAny(segment, "*?[{") {
			return "/" + strings.Join(anchor, "/"), true
		}
		anchor = append(anchor, segment)
	}
	return "/" + strings.Join(anchor, "/"), false
}

func pathsOverlap(left, right string) bool {
	return isPathWithin(left, right) || isPathWithin(right, left)
}

func isPathWithin(value, ancestor string) bool {
	value, ancestor = normalizeMatchPath(value), normalizeMatchPath(ancestor)
	return ancestor == "/" || value == ancestor || strings.HasPrefix(value, strings.TrimSuffix(ancestor, "/")+"/")
}

func hasOperation(rule FilesystemPermission, operation FilesystemOperation) bool {
	for _, candidate := range rule.Operations {
		if candidate == operation {
			return true
		}
	}
	return false
}

func normalizedMode(mode PermissionMode) PermissionMode {
	if mode == "" {
		return PermissionAllow
	}
	if mode == PermissionAsk {
		return PermissionInterrupt
	}
	return mode
}

func filterFileInfo(rules []FilesystemPermission, operation FilesystemOperation, values []backend.FileInfo) []backend.FileInfo {
	result := make([]backend.FileInfo, 0, len(values))
	for _, value := range values {
		if permissionDecision(rules, operation, normalizeMatchPath(value.Path)) != PermissionDeny {
			result = append(result, value)
		}
	}
	return result
}

func filterGrepMatches(rules []FilesystemPermission, operation FilesystemOperation, values []backend.GrepMatch) []backend.GrepMatch {
	result := make([]backend.GrepMatch, 0, len(values))
	for _, value := range values {
		if permissionDecision(rules, operation, normalizeMatchPath(value.Path)) != PermissionDeny {
			result = append(result, value)
		}
	}
	return result
}

func schema(properties string, required ...string) json.RawMessage {
	var props map[string]json.RawMessage
	_ = json.Unmarshal([]byte(properties), &props)
	value := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		value["required"] = required
	}
	data, _ := json.Marshal(value)
	return data
}
func decodeArguments(raw json.RawMessage, target any) error {
	if !json.Valid(raw) {
		return tool.ErrInvalidArguments
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("%w: %v", tool.ErrInvalidArguments, err)
	}
	return nil
}

func readPaginationNotice(result backend.ReadResult) string {
	if result.StartLine == nil || result.EndLine == nil || result.NextOffset == nil {
		return ""
	}
	count := *result.EndLine - *result.StartLine + 1
	unit := "lines"
	if count == 1 {
		unit = "line"
	}
	if result.TotalLines == nil {
		return fmt.Sprintf("\n\n[Read %d %s (lines %d-%d). More lines remain from offset %d.]", count, unit, *result.StartLine, *result.EndLine, *result.NextOffset)
	}
	if *result.EndLine >= *result.TotalLines {
		return ""
	}
	remaining := *result.TotalLines - *result.EndLine
	remainingUnit := "lines"
	if remaining == 1 {
		remainingUnit = "line"
	}
	return fmt.Sprintf("\n\n[Read %d %s (lines %d-%d of %d total). %d %s remaining from offset %d.]", count, unit, *result.StartLine, *result.EndLine, *result.TotalLines, remaining, remainingUnit, *result.NextOffset)
}

func numberLines(content string, start int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type row struct {
		marker string
		text   string
	}
	rows := make([]row, 0, len(lines))
	width := 0
	for index, line := range lines {
		number := start + index
		if line == "" {
			marker := strconv.Itoa(number)
			rows = append(rows, row{marker: marker})
			width = max(width, len(marker))
			continue
		}
		for part := 0; len(line) > 0; part++ {
			size := min(5000, len(line))
			marker := strconv.Itoa(number)
			if part > 0 {
				marker += "." + strconv.Itoa(part)
			}
			rows = append(rows, row{marker: marker, text: line[:size]})
			width = max(width, len(marker))
			line = line[size:]
		}
	}
	output := make([]string, len(rows))
	for index, item := range rows {
		output[index] = fmt.Sprintf("%*s  %s", width, item.marker, item.text)
	}
	return strings.Join(output, "\n")
}

func mediaResult(ctx context.Context, value backend.Backend, filePath string, data *backend.FileData) (tool.Result, error) {
	raw, err := binaryFileBytes(ctx, value, filePath, data)
	if err != nil {
		return tool.Result{}, err
	}
	mimeType := mime.TypeByExtension(path.Ext(filePath))
	blockType := message.BlockFile
	if strings.HasPrefix(mimeType, "image/") {
		blockType = message.BlockImage
	} else if strings.HasPrefix(mimeType, "audio/") {
		blockType = message.BlockAudio
	} else if strings.HasPrefix(mimeType, "video/") && strings.ToLower(path.Ext(filePath)) != ".mkv" {
		blockType = message.BlockVideo
	}
	return tool.Result{Content: []message.ContentBlock{{Type: message.BlockText, Text: "Read binary file " + filePath}, {Type: blockType, Data: raw, MIMEType: mimeType, Name: path.Base(filePath)}}}, nil
}

func binaryFileBytes(ctx context.Context, value backend.Backend, filePath string, data *backend.FileData) ([]byte, error) {
	downloads := value.Download(ctx, []string{filePath})
	if len(downloads) == 1 && downloads[0].Error == "" {
		return append([]byte(nil), downloads[0].Content...), nil
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(data.Content)
	if err != nil {
		return nil, fmt.Errorf("binary content for %s is not valid base64: %w", filePath, err)
	}
	return raw, nil
}

func videoResult(ctx context.Context, options FilesystemOptions, filePath string, data *backend.FileData, offset, limit int) (tool.Result, error) {
	raw, err := binaryFileBytes(ctx, options.Backend, filePath, data)
	if err != nil {
		return tool.Result{}, err
	}
	if len(raw) > options.MaxVideoBytes {
		return tool.Result{}, fmt.Errorf("error reading video %s: video payload exceeds maximum input size of %d bytes", filePath, options.MaxVideoBytes)
	}
	window := VideoWindow{OffsetSeconds: float64(max(0, offset)), DurationSeconds: float64(limit), SamplingRate: options.VideoSamplingRate}
	blocks, err := options.VideoExtractor.Extract(ctx, raw, window)
	if err != nil {
		return tool.Result{}, fmt.Errorf("error reading video %s: %w\n%s", filePath, err, videoWindowHeader(filePath, window))
	}
	frameCount := 0
	for _, block := range blocks {
		if block.Type == message.BlockImage {
			frameCount++
		}
	}
	frameLabel := "frames"
	if frameCount == 1 {
		frameLabel = "frame"
	}
	content := make([]message.ContentBlock, 0, len(blocks)+2)
	content = append(content,
		message.ContentBlock{Type: message.BlockText, Text: fmt.Sprintf("Read video %s: sampled %d %s.", filePath, frameCount, frameLabel)},
		message.ContentBlock{Type: message.BlockText, Text: videoWindowHeader(filePath, window)},
	)
	content = append(content, blocks...)
	return tool.Result{Content: content}, nil
}

func videoWindowHeader(filePath string, window VideoWindow) string {
	end := window.OffsetSeconds + window.DurationSeconds
	if window.OffsetSeconds <= 0 {
		return fmt.Sprintf("Reading first %gs of %s at %g fps.", window.DurationSeconds, filePath, window.SamplingRate)
	}
	return fmt.Sprintf("Reading [%.3fs, %.3fs) of %s at %g fps.", window.OffsetSeconds, end, filePath, window.SamplingRate)
}

func isVideoPath(filePath string) bool {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".mp4", ".mpeg", ".mov", ".avi", ".flv", ".mpg", ".webm", ".wmv", ".3gpp", ".mkv":
		return true
	default:
		return false
	}
}

func formatGrep(result backend.GrepResult, mode string) string {
	if mode == "" {
		mode = "files_with_matches"
	}
	switch mode {
	case "content":
		lines := make([]string, len(result.Matches))
		for i, item := range result.Matches {
			lines[i] = fmt.Sprintf("%s:%d: %s", item.Path, item.Line, item.Text)
		}
		if result.Truncated {
			lines = append(lines, "Results truncated; narrow the pattern or path.")
		}
		return strings.Join(lines, "\n")
	case "count":
		counts := map[string]int{}
		for _, item := range result.Matches {
			counts[item.Path]++
		}
		paths := make([]string, 0, len(counts))
		for p := range counts {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		lines := make([]string, len(paths))
		for i, p := range paths {
			lines[i] = fmt.Sprintf("%s: %d", p, counts[p])
		}
		return strings.Join(lines, "\n")
	default:
		seen := map[string]bool{}
		var paths []string
		for _, item := range result.Matches {
			if !seen[item.Path] {
				paths = append(paths, item.Path)
				seen[item.Path] = true
			}
		}
		sort.Strings(paths)
		if result.Truncated {
			paths = append(paths, "Results truncated; narrow the pattern or path.")
		}
		return strings.Join(paths, "\n")
	}
}

func previewText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	half := limit / 2
	return value[:half] + "\n... truncated ...\n" + value[len(value)-half:]
}
