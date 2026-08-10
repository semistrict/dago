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
	ReadLimit         int
	GrepLimit         int
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

// FilesystemMiddleware constructs the standard file tools and permission boundary.
func FilesystemMiddleware(options FilesystemOptions) (agent.Middleware, error) {
	if options.Backend == nil {
		return agent.Middleware{}, fmt.Errorf("filesystem backend is required")
	}
	if options.ReadLimit <= 0 {
		options.ReadLimit = 100
	}
	if options.GrepLimit <= 0 {
		options.GrepLimit = 100
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
	return middleware, nil
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
	values["ls"] = tool.Func{Spec: tool.Definition{Name: "ls", Description: "List files and directories directly inside an absolute directory path.", InputSchema: schema(`{"path":{"type":"string"}}`, "path")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
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
	readDescription := "Read an absolute file path with 0-indexed line pagination. Returned text includes source line numbers."
	offsetDescription := "0-indexed starting line."
	limitDescription := "Maximum lines to return."
	if options.VideoExtractor != nil {
		readDescription += " For videos, offset and limit select a window in seconds and sampled JPEG frames are returned."
		offsetDescription += " For videos, seconds into the source."
		limitDescription += " For videos, duration in seconds and must be positive."
	}
	readSchema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"file_path": map[string]any{"type": "string"},
		"offset":    map[string]any{"type": "integer", "minimum": 0, "default": 0, "description": offsetDescription},
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
		if result.Data.Encoding == backend.EncodingBase64 {
			if video {
				return videoResult(ctx, options, input.FilePath, result.Data, input.Offset, limit)
			}
			return mediaResult(ctx, options.Backend, input.FilePath, result.Data)
		}
		if result.Data.Content == "" {
			return tool.TextResult("System reminder: the file is empty."), nil
		}
		start := input.Offset + 1
		if result.StartLine != nil {
			start = *result.StartLine
		}
		text := numberLines(result.Data.Content, start)
		if result.NextOffset != nil {
			text += fmt.Sprintf("\n\nMore lines are available; continue with offset=%d.", *result.NextOffset)
		}
		return tool.TextResult(text), nil
	}}
	values["write_file"] = tool.Func{Spec: tool.Definition{Name: "write_file", Description: "Create or completely replace a text file at an absolute path.", InputSchema: schema(`{"file_path":{"type":"string"},"content":{"type":"string"}}`, "file_path", "content")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
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
	values["edit_file"] = tool.Func{Spec: tool.Definition{Name: "edit_file", Description: "Replace exact text in an existing file. The old text must be unique unless replace_all is true.", InputSchema: schema(`{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean","default":false}}`, "file_path", "old_string", "new_string")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
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
	values["delete"] = tool.Func{Spec: tool.Definition{Name: "delete", Description: "Recursively delete an absolute file or directory path.", InputSchema: schema(`{"file_path":{"type":"string"}}`, "file_path")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
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
	values["glob"] = tool.Func{Spec: tool.Definition{Name: "glob", Description: "Find files using a glob pattern such as **/*.go. Returns absolute virtual paths.", InputSchema: schema(`{"pattern":{"type":"string"},"path":{"type":"string"}}`, "pattern")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
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
	values["grep"] = tool.Func{Spec: tool.Definition{Name: "grep", Description: "Search for a literal text pattern across files. This is not a regular-expression search.", InputSchema: schema(`{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"output_mode":{"type":"string","enum":["files_with_matches","content","count"],"default":"files_with_matches"},"max_count":{"type":"integer","minimum":1}}`, "pattern")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
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
		values["execute"] = tool.Func{Spec: tool.Definition{Name: "execute", Description: "Execute a shell command in the explicitly configured local or remote sandbox and return combined output and exit status.", InputSchema: schema(`{"command":{"type":"string"},"timeout":{"type":"integer","minimum":0}}`, "command")}, Run: func(ctx context.Context, raw json.RawMessage, _ tool.Runtime) (tool.Result, error) {
			var input struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := decodeArguments(raw, &input); err != nil {
				return tool.Result{}, err
			}
			result, err := sandbox.Execute(ctx, input.Command, time.Duration(input.Timeout)*time.Second)
			if err != nil {
				return tool.Result{}, err
			}
			artifact, _ := json.Marshal(map[string]any{"exit_code": result.ExitCode, "truncated": result.Truncated})
			return tool.Result{Content: []message.ContentBlock{{Type: message.BlockText, Text: result.Output}}, Artifact: artifact}, nil
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

func numberLines(content string, start int) string {
	lines := strings.Split(content, "\n")
	var output []string
	for index, line := range lines {
		number := start + index
		if len(line) <= 5000 {
			output = append(output, strconv.Itoa(number)+"  "+line)
			continue
		}
		part := 1
		for len(line) > 0 {
			size := min(5000, len(line))
			output = append(output, fmt.Sprintf("%d.%d  %s", number, part, line[:size]))
			line = line[size:]
			part++
		}
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
